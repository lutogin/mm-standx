package engine

import (
	"context"
	"fmt"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/shopspring/decimal"

	"github.com/nd/mm-standx/internal/domain/market"
	"github.com/nd/mm-standx/internal/domain/money"
	"github.com/nd/mm-standx/internal/domain/policy"
	"github.com/nd/mm-standx/internal/domain/quotes"
	"github.com/nd/mm-standx/internal/domain/sizing"
	"github.com/nd/mm-standx/internal/domain/trading"
	"github.com/nd/mm-standx/internal/domain/uptime"
	"github.com/nd/mm-standx/internal/domain/volatility"
	"github.com/nd/mm-standx/internal/events"
	"github.com/nd/mm-standx/internal/ports"
)

// evaluate — единственное место, где принимается решение о котировках.
//
// allowPlace различает два источника вызова: такт mark price несёт официальную
// цену и разрешает выставление, обновление стакана — только снятие.
func (e *Engine) evaluate(ctx context.Context, allowPlace bool) {
	now := e.deps.Clock.Now()

	// Ни один выход отсюда не имеет права быть молчаливым.
	//
	// Прежде их было четыре: остановка, пауза после залива, и оба съёма при
	// пустом стакане, где withdraw выходит на len(ids) == 0, не написав ничего.
	// Актор при этом продолжал тикать, метрика показывала ноль — а ноль
	// одинаково означает и «воздерживаюсь по делу», и «сломан». Разбор каждого
	// зависания начинался с восстановления этой разницы по косвенным уликам.
	if !e.mark.IsPositive() {
		e.deps.Metrics.Quoting(e.symbol, false)
		e.reportBlocked(now, policy.Conditions{}, volatility.Estimate{},
			"waiting for the first mark price", policy.Quote{})
		return
	}

	estimate := e.vol.Estimate(now)
	halfSpread, bookOK := e.halfSpreadBps()
	conditions := policy.Conditions{
		HalfSpreadBps: halfSpread,
		JumpBps:       estimate.EffectiveJump(),
		Ready:         estimate.Ready,
	}
	// blocked печатает сводку с причиной и выкладывает её в метрики.
	blocked := func(reason string) {
		e.reportBlocked(now, conditions, estimate, reason, e.geometry(e.appetite, conditions))
	}

	if e.halted {
		blocked("halted: " + e.haltReason)
		e.withdraw(ctx, ports.CauseKillSwitch, e.haltReason)
		return
	}

	// Очередь переполнялась: часть событий об ордерах и позиции до актора
	// не дошла, и его представление о собственном стакане неполно. Котировать
	// поверх такой картины — выставлять вторую котировку при живой первой.
	//
	// Проверка стоит выше паузы защитного контура намеренно: та говорит, что
	// котировать невыгодно, эта — что нечем.
	if now.Before(e.resyncUntil) {
		blocked(e.resyncReason)
		e.withdraw(ctx, ports.CauseInboxOverflow, e.resyncReason)
		return
	}

	// Пауза защитного контура держится сроком, а не флагом: сообщение о снятии
	// может не прийти, и тогда актор молчал бы до конца запуска.
	if now.Before(e.riskPauseUntil) {
		blocked(e.riskPauseReason)
		e.withdraw(ctx, ports.CauseRiskPause, e.riskPauseReason)
		return
	}

	// Незакрытая позиция важнее награды: котировать, имея экспозицию, значит
	// удвоить её при следующем заливе.
	if now.Before(e.flattenRetryAt) || e.pendingFill.IsPositive() || e.overPositionLimit() {
		blocked("no quoting while exposure is open")
		e.withdraw(ctx, e.positionCause(), "no quoting while exposure is open")
		return
	}
	if now.Before(e.cooldownUntil) {
		blocked(fmt.Sprintf("post-fill cooldown is active until %s",
			e.cooldownUntil.UTC().Format(time.RFC3339)))
		return
	}

	if !bookOK {
		blocked("no two-sided book")
		e.withdraw(ctx, ports.CauseBookUnreadable, "no two-sided book")
		return
	}

	if now.Before(e.fillBurstUntil) {
		reason := fmt.Sprintf("fill burst cooldown is active until %s", e.fillBurstUntil.UTC().Format(time.RFC3339))
		blocked(reason)
		e.withdraw(ctx, ports.CauseFillBurst, reason)
		return
	}

	hour := e.hour.Assess(now)

	// The reference market is shut: the exchange scales the hour's accrual
	// while the fill risk stays exactly the same. For gold that is a tenth,
	// so the break-even price of a Maker Hour rises tenfold.
	if hour.Multiplier < 1 && !e.cfg.Program.QuoteOffHours {
		reason := fmt.Sprintf(
			"the reference market is closed, the hour accrues at %.0f%%", hour.Multiplier*100)
		e.reportBlocked(now, conditions, estimate, reason, e.geometry(e.appetite, conditions))
		e.withdraw(ctx, ports.CauseOffHours, reason)
		return
	}

	// Единственная защита контура, работающая на упреждение. Всё остальное
	// отвечает на движение, которое уже началось; против публикации, время
	// которой известно заранее, это не работает вовсе.
	news := e.assessNews(now, hour)
	if news.blocked != "" {
		e.reportBlocked(now, conditions, estimate, news.blocked, e.geometry(e.appetite, conditions))
		e.withdraw(ctx, news.cause, news.blocked)
		return
	}

	appetite := e.appetite
	switch hour.Mode {
	case uptime.Lost:
		// Час недостижим: дальнейшее присутствие оплачивается исполнениями,
		// а зачёта не приносит.
		//
		// The summary is printed before returning: without it the actor goes
		// silent for the rest of the hour, and a deliberate refusal reads in
		// the log exactly like a crash — the ambiguity the summary exists to
		// remove.
		e.reportBlocked(now, conditions, estimate, hour.Reason, e.geometry(appetite, conditions))
		e.withdraw(ctx, ports.CauseHourLost, hour.Reason)
		return
	case uptime.Banked:
		// Цель взята, дополнительный аптайм ничего не добавляет — отходим.
		appetite.JumpMultiple *= bankedCaution
	}

	if e.live && e.economics.BreakEvenUSDPerMakerHour > 0 {
		decision := e.economics.Evaluate(e.cfg.AllocationUSD, hour.TierMultiplier, hour.Multiplier)
		if !decision.Allowed {
			e.reportBlocked(now, conditions, estimate, decision.Reason, e.geometry(appetite, conditions))
			e.withdraw(ctx, ports.CauseEconomics, decision.Reason)
			return
		}
	}

	bounds := e.quotingBounds()
	if news.defensive {
		// Защитная поза выражена границей, а не отдельной геометрией: правило
		// дистанции остаётся тем же, ему лишь запрещено подходить ближе края
		// полосы. Вердикт риска при этом не отменяется — если рынок требует
		// большего, чем полоса вмещает, котировки не будет и здесь.
		bounds = bounds.Outer(e.newsGuard.DefensiveInsetBps)
	}
	// Лесенка сужается до той, что помещается, вместо отказа котировать:
	// её ширина берётся из того же бюджета дистанции, что и сама котировка,
	// и перестаёт помещаться раньше неё. См. policy.Ladder.Fit.
	ladder, anchored := e.ladder.Fit(appetite.Target(conditions, bounds), bounds)
	quote := e.stickyTarget(anchored)
	e.rungs = ladder.Layers
	e.reportStatus(now, conditions, estimate, quote)
	e.reportMetrics(now, conditions, estimate, quote)

	if !quote.Feasible {
		e.withdraw(ctx, ports.CauseNoSafeDistance, quote.Reason)
		return
	}

	thresholds := appetite.Thresholds(quote, conditions, e.bounds)
	distances := ladder.Distances(quote.Distance)

	// Опасность проверяется до ограничения темпа. Кулдаун существует, чтобы
	// не жечь бюджет запросов на перестановках, но, стоя выше этой проверки,
	// он делал актор слепым на четверть секунды после каждого действия —
	// при обновлении стакана раз в 300 мс это ровно тот такт, ради которого
	// быстрый сигнал и заведён.
	if e.pullDanger(ctx, thresholds, now) {
		return
	}
	// Темп перестановки ограничен независимо от частоты событий: стакан
	// приходит несколько раз в секунду, и реагировать на каждое обновление
	// значило бы исчерпать бюджет запросов за минуту.
	if now.Sub(e.lastAction) < e.cfg.Quote.RequoteCooldown {
		return
	}

	e.blocked = ""
	// Размер режется вместе с отходом, и это бесплатно по зачёту. Тир 0.5x
	// берётся с медианы замеров, а её держат обычные полчаса часа; урезанные
	// замеры попадают только в верхний перцентиль, который защитная поза
	// и так отдала, уйдя к краю полосы. Потери же от исполнения размеру прямо
	// пропорциональны.
	allocation := e.cfg.AllocationUSD
	if news.defensive {
		allocation *= e.newsGuard.DefensiveSizeMultiple
	}
	marginRate, marginSource := e.marginRate()
	e.marginSource = marginSource

	plan, err := sizing.Compute(sizing.Inputs{
		Budget: sizing.Budget{
			AllocationUSD:    decimal.NewFromFloat(allocation),
			EquityUSD:        e.equity,
			InitialMarginPct: marginRate,
			UtilizationPct:   utilizationPct,
		},
		Spec:      e.spec,
		Program:   e.program,
		Mark:      e.mark,
		Distances: distances,
	})
	if err != nil {
		// Расчёт размера падает штатно, пока не пришёл первый снимок баланса.
		// Причина обязана быть видна: молчащий актор иначе выглядит одинаково
		// и когда он корректно воздерживается, и когда он сломан.
		e.blocked = "sizing: " + err.Error()
		e.sizeLimit, e.perSide = "", money.Qty{}
		e.withdraw(ctx, ports.CauseSizing, err.Error())
		return
	}
	// Which constraint actually bound the size, kept for the status line.
	//
	// The answer decides how much of the deposit may be allocated at all.
	// Resting orders lock margin, so the exchange's available balance falls as
	// soon as quotes go up; if that balance is what binds, raising the
	// allocation makes the size oscillate instead of growing, and an
	// oscillating size lowers the reward — the exchange scores a percentile of
	// the samples, which lands on the low ones. Whether it binds cannot be
	// derived, only observed (PLAN.md §12.5).
	e.sizeLimit, e.perSide = plan.Reason, plan.PerSide

	for _, side := range []market.Side{market.Buy, market.Sell} {
		ladder := e.ladderFor(side)
		actions := ladder.Plan(now, distances, e.classifier(side, thresholds), e.rules)
		if len(actions) > 0 {
			e.lastAction = now
		}
		e.execute(ctx, side, actions, allowPlace, plan.Rungs)
	}
}

// quotingBounds сужает полосу, пока актор вне рынка.
//
// Порог осуществимости — единственное решение в этом контуре без мёртвой зоны,
// и рынок стоит ровно на нём. Требуемая дистанция складывается из половины
// спреда, а тот гуляет широко: 6 августа в двенадцатом часу медиана была
// 1.99 bps при максимуме 3.40. Каждый заход хвоста за край полосы снимал всю
// лесенку, каждый возврат выставлял её заново — 564 съёма за час при аптайме
// 99%. Присутствие сохранялось, но ордера жили в среднем 6.4 секунды при
// программном требовании в три, а на дрожание уходило 13% бюджета запросов.
//
// Асимметрия здесь та же, что и во всём остальном контуре: уходим по обычному
// порогу и немедленно, возвращаемся только когда до края есть запас.
func (e *Engine) quotingBounds() policy.Bounds {
	bounds := e.bounds
	if e.target.Feasible {
		return bounds
	}
	if inset := bounds.MaxBps - e.appetite.ReentryDeadBandBps; inset > bounds.MinBps {
		bounds.MaxBps = inset
	}
	return bounds
}

// pullDanger снимает стороны, к ордерам которых рынок подошёл вплотную,
// и сообщает, что съём состоялся.
//
// Путь отделён от общего планирования намеренно: он обязан оставаться
// доступным независимо от ограничения темпа перестановок и от того, есть ли
// на стороне команда в полёте.
func (e *Engine) pullDanger(ctx context.Context, th policy.Thresholds, now time.Time) bool {
	type cancelIntent struct {
		side   market.Side
		action quotes.Action
	}

	var intents []cancelIntent
	var ids []trading.ClOrdID

	for _, side := range []market.Side{market.Buy, market.Sell} {
		ladder := e.ladderFor(side)
		actions := ladder.Danger(e.classifier(side, th))
		for _, action := range actions {
			intents = append(intents, cancelIntent{side: side, action: action})
			ids = append(ids, action.ClOrdID)
			ladder.OnCancelSent(action.Slot, now)
		}
	}
	if len(ids) == 0 {
		return false
	}

	e.lastAction = now
	// One exchange batch is the safety invariant here. Waiting for one ACK per
	// rung leaves the later rungs live for N network round trips during the same
	// sweep that triggered the panic pull.
	if !e.sendWithdrawal(ctx, ids) {
		return true
	}
	for _, intent := range intents {
		e.deps.Metrics.OrderSent(e.symbol, intent.side, "cancel")
		e.deps.Log.Info("quote cancelled",
			"symbol", e.symbol, "side", intent.side, "slot", intent.action.Slot,
			"reason", intent.action.Reason)
	}
	return true
}

// stickyTarget удерживает целевую дистанцию, пока расчётная не отойдёт заметно.
//
// Без этого гистерезис не работает вовсе. Мёртвая зона строится вокруг цели,
// и если цель пересчитывается на каждом обновлении стакана, зона едет вместе
// с ней — ордера оказываются снаружи не потому, что сдвинулись сами, а потому
// что под ними сдвинули границы. Защита от дрожания должна начинаться с того,
// чтобы не дрожала сама точка отсчёта.
//
// Новая цель принимается только при существенном расхождении либо когда она
// требует отойти дальше: осторожность применяется немедленно, жадность — нет.
func (e *Engine) stickyTarget(fresh policy.Quote) policy.Quote {
	if !fresh.Feasible || !e.target.Feasible {
		e.target = fresh
		return fresh
	}

	widening := fresh.Distance > e.target.Distance
	drift := fresh.Distance - e.target.Distance
	if drift < 0 {
		drift = -drift
	}

	if widening || drift >= e.cfg.Appetite.MinDeadBandBps {
		e.target = fresh
	}
	return e.target
}

// overPositionLimit сообщает, что экспозиция вышла за разрешённую инструменту.
//
// При значении по умолчанию (ноль) под предел попадает любая ненулевая
// позиция: стратегия зарабатывает на присутствии в стакане, и направленная
// экспозиция для неё — авария, а не инструмент.
func (e *Engine) overPositionLimit() bool {
	return e.position.Abs().Gt(e.maxNetPosition)
}

// positionCause различает штатное ожидание закрытия и превышение предела.
//
// Разница видна только в метриках, но именно там она и нужна: регулярный
// «position_open» — это заливы, а «position_limit» — это позиция, выросшая
// сверх разрешённой, то есть отказ закрывающего пути.
func (e *Engine) positionCause() ports.WithdrawCause {
	if !e.maxNetPosition.IsZero() && e.overPositionLimit() {
		return ports.CausePositionLimit
	}
	return ports.CausePositionOpen
}

// utilizationPct — доля свободных средств, которую разрешено занимать.
//
// Остаток нужен не про запас: висящие ордера блокируют маржу, и при полной
// утилизации закрывающий ордер после залива может не пройти.
const utilizationPct = 70

// classifier строит правило оценки ступени для стороны.
//
// Опасность судится по быстрому сигналу, попадание в полосу — по официальной
// цене. Смешивать их в одно число нельзя: оценка сместила бы границу полосы,
// а официальная цена опоздала бы с тревогой на три секунды.
func (e *Engine) classifier(side market.Side, th policy.Thresholds) quotes.Classifier {
	estimated := e.markEstimate()

	return func(slot quotes.Slot) policy.Decision {
		official, err := market.DistanceBps(e.mark, side, slot.Price)
		if err != nil {
			return policy.Decision{Action: policy.Pull, Urgency: policy.UrgencySafety, Reason: "distance is not computable"}
		}

		if fast, ferr := market.DistanceBps(estimated, side, slot.Price); ferr == nil {
			if danger := th.Classify(fast); danger.Urgency == policy.UrgencySafety {
				return danger
			}
		}
		return th.Classify(official)
	}
}

// execute превращает намерения лесенки в команды биржи.
func (e *Engine) execute(
	ctx context.Context,
	side market.Side,
	actions []quotes.Action,
	allowPlace bool,
	rungs []money.Qty,
) {
	ladder := e.ladderFor(side)
	now := e.deps.Clock.Now()

	for _, action := range actions {
		switch action.Kind {
		case quotes.Cancel:
			if err := e.deps.Gateway.Cancel(ctx, action.ClOrdID, priorityOf(action.Urgency)); err != nil {
				e.deps.Log.Warn("cancel failed", "symbol", e.symbol, "cl_ord_id", action.ClOrdID, "error", err)
				e.deps.Metrics.OrderFailed(e.symbol, "cancel")
				continue
			}
			e.deps.Metrics.OrderSent(e.symbol, side, "cancel")
			e.deps.Log.Info("quote cancelled",
				"symbol", e.symbol, "side", side, "slot", action.Slot, "reason", action.Reason)
			ladder.OnCancelSent(action.Slot, now)

		case quotes.Place:
			if !allowPlace {
				continue
			}
			e.place(ctx, side, action, rungs, now)
		}
	}
}

func (e *Engine) place(
	ctx context.Context,
	side market.Side,
	action quotes.Action,
	rungs []money.Qty,
	now time.Time,
) {
	if len(rungs) == 0 || action.Slot >= len(rungs) {
		return
	}
	qty := rungs[action.Slot]

	price := e.spec.SnapPrice(market.PriceAtDistance(e.mark, side, action.Distance), side)
	if err := e.spec.ValidateQty(qty); err != nil {
		e.deps.Log.Warn("size rejected", "symbol", e.symbol, "error", err)
		return
	}
	if clamped, ok := e.clampPostOnly(side, price); ok {
		price = clamped
	} else {
		return
	}
	// Биржа отклонит пересекающий ALO-ордер сама, но проверка на нашей стороне
	// экономит запрос и, что важнее, ловит ошибку в расчёте дистанции.
	//
	// После зажима она уже не должна срабатывать, и именно поэтому остаётся:
	// сработавшая означает, что зажим выключен либо стакан односторонний.
	if e.book.WouldCross(side, price) {
		e.deps.Log.Warn("quote would cross the book, skipping",
			"symbol", e.symbol, "side", side, "price", price)
		return
	}

	id := trading.ClOrdID(ulid.Make().String())
	req := ports.PlaceRequest{
		ClOrdID: id,
		Symbol:  e.symbol,
		Side:    side,
		Type:    trading.Limit,
		// ALO — единственная гарантия, что котировка не станет тейкером.
		TimeInForce: trading.ALO,
		Price:       price,
		Qty:         qty,
		MarginMode:  e.risk.MarginMode,
		Leverage:    e.cfg.SymbolRisk.Leverage,
		Priority:    priorityOf(action.Urgency),
	}

	if err := e.deps.Gateway.Place(ctx, req); err != nil {
		e.deps.Log.Warn("place failed", "symbol", e.symbol, "cl_ord_id", id, "error", err)
		e.deps.Metrics.OrderFailed(e.symbol, "place")
		return
	}
	// Register ownership before the asynchronous order update is handled.
	// Besides cumulative-fill accounting, the presence of the key prevents a
	// foreign or replayed terminal order from opening a fill episode.
	e.filled[id] = money.MustParseQty("0")
	e.deps.Metrics.OrderSent(e.symbol, side, "place")
	e.deps.Metrics.QuoteDistance(e.symbol, side, float64(action.Distance))
	// Логируется и успех: в боевом режиме заглушка молчит, и без этой записи
	// по логу невозможно отличить работающего бота от бездействующего.
	e.deps.Log.Info("quote placed",
		"symbol", e.symbol, "side", side, "slot", action.Slot,
		"price", price, "qty", qty, "distance", action.Distance, "reason", action.Reason)

	e.ladderFor(side).OnPlaceSent(action.Slot, id, price, qty, now)
}

// clampPostOnly отодвигает цену от вершины стакана и решает, стоит ли отправлять
// то, что получилось.
//
// Второй результат false означает отказ от размещения. Отказ здесь дешевле
// отправки: зажим срабатывает лишь тогда, когда стакан ушёл дальше, чем успела
// обновиться справочная цена, и вся геометрия котировки в этот момент построена
// на устаревшем mark. Прежде такой ордер уходил на биржу и возвращался отказом
// post-only — круг запроса и пустая ступень до следующего такта, — либо, что
// хуже, вставал на вершину и забирался ближайшим движением.
//
// Проверка односторонняя, потому что односторонний и зажим: он уводит цену
// только от mark. Ближе разрешённого котировка после него оказаться не может,
// а вот за краем полосы — вполне, и там она не стоит ничего.
func (e *Engine) clampPostOnly(side market.Side, price money.Price) (money.Price, bool) {
	clamped := market.ClampPostOnly(price, side, e.book, e.spec, e.cfg.Quote.PostOnlyBufferTicks)
	if clamped.Cmp(price) == 0 {
		return price, true
	}

	distance, err := market.DistanceBps(e.mark, side, clamped)
	if err != nil || !clamped.IsPositive() || distance > e.bounds.MaxBps {
		e.deps.Log.Info("skipping a quote the post-only buffer pushed out of the band",
			"symbol", e.symbol, "side", side,
			"price", price, "clamped", clamped, "distance", distance, "band", e.bounds.MaxBps)
		return price, false
	}

	e.deps.Log.Info("post-only buffer moved the quote off the touch",
		"symbol", e.symbol, "side", side,
		"price", price, "clamped", clamped, "distance", distance)
	return clamped, true
}

// withdraw снимает обе стороны.
func (e *Engine) withdraw(ctx context.Context, cause ports.WithdrawCause, detail string) {
	var ids []trading.ClOrdID
	now := e.deps.Clock.Now()

	e.deps.Metrics.Quoting(e.symbol, false)

	for _, ladder := range []*quotes.Ladder{e.bids, e.asks} {
		actions := ladder.PullAll(string(cause))
		for _, a := range actions {
			ids = append(ids, a.ClOrdID)
		}
		for _, a := range actions {
			ladder.OnCancelSent(a.Slot, now)
		}
	}
	if len(ids) == 0 {
		return
	}

	e.deps.Log.Info("withdrawing quotes",
		"symbol", e.symbol, "count", len(ids), "cause", cause, "detail", detail)
	e.deps.Metrics.Withdrawal(e.symbol, cause)
	// Пакетная отмена стоит одного кредита вместо N — это важно именно
	// в момент, когда бюджет запросов нужнее всего.
	e.sendWithdrawal(ctx, ids)
}

// withdrawRetryBase и withdrawRetryMax задают отступ между повторами.
//
// Первый повтор уходит быстрее, чем биржа успевает подтвердить исходную
// команду: повторная отмена безвредна, а промедление — нет.
const (
	withdrawRetryBase = 250 * time.Millisecond
	withdrawRetryMax  = 5 * time.Second
)

// sendWithdrawal отправляет пакетную отмену и берёт неудачу на повтор.
//
// Молчаливая потеря команды на этом пути недопустима. Ступени уже переведены
// в снимаемые, то есть локально сторона считается пустой, тогда как на бирже
// она стоит под тем самым движением, ради которого съём и затевался. Сверка
// обнаружила бы расхождение, но лишь через несколько секунд — а повтор
// безопасен, потому что отмена идемпотентна (PLAN.md §2.2).
func (e *Engine) sendWithdrawal(ctx context.Context, ids []trading.ClOrdID) bool {
	if err := e.deps.Gateway.CancelBatch(ctx, ids, ports.PrioritySafety); err != nil {
		e.deps.Log.Error("withdrawal failed, retrying",
			"symbol", e.symbol, "count", len(ids), "attempt", e.withdrawAttempts+1, "error", err)
		e.deps.Metrics.OrderFailed(e.symbol, "cancel_batch")
		e.queueWithdrawal(ids)
		return false
	}
	e.forgetWithdrawal(ids)
	return true
}

// queueWithdrawal добавляет идентификаторы в очередь повтора и назначает срок.
func (e *Engine) queueWithdrawal(ids []trading.ClOrdID) {
	known := make(map[trading.ClOrdID]bool, len(e.pendingWithdraw))
	for _, id := range e.pendingWithdraw {
		known[id] = true
	}
	for _, id := range ids {
		if !known[id] {
			e.pendingWithdraw = append(e.pendingWithdraw, id)
		}
	}

	backoff := withdrawRetryBase << min(e.withdrawAttempts, 8)
	e.withdrawAttempts++
	e.withdrawRetryAt = e.deps.Clock.Now().Add(min(backoff, withdrawRetryMax))
}

// forgetWithdrawal убирает идентификаторы из очереди повтора.
func (e *Engine) forgetWithdrawal(ids []trading.ClOrdID) {
	if len(e.pendingWithdraw) == 0 {
		return
	}
	gone := make(map[trading.ClOrdID]bool, len(ids))
	for _, id := range ids {
		gone[id] = true
	}

	kept := e.pendingWithdraw[:0]
	for _, id := range e.pendingWithdraw {
		if !gone[id] {
			kept = append(kept, id)
		}
	}
	e.pendingWithdraw = kept
	if len(e.pendingWithdraw) == 0 {
		e.withdrawAttempts = 0
	}
}

// retryWithdrawal повторяет неудавшийся съём.
//
// Верхнего предела на число попыток нет намеренно. Ордер, оставшийся в стакане,
// продолжает исполняться независимо от того, сколько раз мы уже пытались его
// снять; сдаться означало бы оставить в рынке позицию, за которой никто
// не следит. Отступ растёт, поэтому недоступная биржа не съедает бюджет
// запросов, а список очищается по мере того, как биржа подтверждает отмены.
func (e *Engine) retryWithdrawal(ctx context.Context, now time.Time) {
	if len(e.pendingWithdraw) == 0 || now.Before(e.withdrawRetryAt) {
		return
	}
	ids := append([]trading.ClOrdID(nil), e.pendingWithdraw...)
	e.deps.Log.Warn("retrying withdrawal", "symbol", e.symbol, "count", len(ids))
	e.sendWithdrawal(ctx, ids)
}

// onFill accounts for a newly observed fill delta and closes it immediately.
//
// FilledQty is cumulative, so every delta is sent once as a reduce-only IOC.
// Waiting to aggregate partial fills leaves directional exposure open during
// the exact move that reached the quote. Taker fees are proportional to
// notional, while that exposure can move again before the position stream or
// service tick arrives.
func (e *Engine) onFill(ctx context.Context, order trading.Order) {
	// Биржа присылает накопленный объём исполнения, а не приращение: каждое
	// следующее сообщение по тому же ордеру повторяет всё, что уже было.
	// Складывать эти числа значило бы удваивать залив на каждом обновлении.
	delta := order.FilledQty.Sub(e.filled[order.ClOrdID])
	if !delta.IsPositive() {
		return
	}
	e.filled[order.ClOrdID] = order.FilledQty

	now := e.deps.Clock.Now()
	startsEpisode := !e.pendingFill.IsPositive()
	if startsEpisode {
		e.recordFillEpisode(now)
	}

	e.pendingFill = e.pendingFill.Add(delta)
	if e.fillDeadline.IsZero() || e.fillDeadline.Before(now) {
		e.fillDeadline = now.Add(e.risk.FlattenDebounce)
	}

	e.deps.Log.Warn("order filled",
		"symbol", e.symbol, "side", order.Side,
		"qty", delta, "price", order.AvgFillPrice, "new_episode", startsEpisode)

	e.publishFill(ctx, order, delta, startsEpisode, now)
	// The matching engine has applied the quote fill before publishing this
	// update, so a reduce-only close can be submitted without waiting for the
	// slower position channel. New fill deltas deliberately bypass
	// flattenRetryAt; that deadline only suppresses position-based duplicates.
	e.submitFlatten(ctx, order.Side.Opposite(), delta, now, "fill delta")
	e.withdraw(ctx, ports.CauseFilled, "closing the position")
}

// recordFillEpisode applies a transient, self-resetting circuit breaker. It is
// intentionally separate from the persistent hourly kill switch: a short
// volatility regime should pause quoting and then be re-evaluated, while a
// sustained model failure still requires operator intervention.
func (e *Engine) recordFillEpisode(now time.Time) {
	if e.risk.FillBurstLimit < 2 || e.risk.FillBurstWindow <= 0 || e.risk.FillBurstCooldown <= 0 {
		return
	}
	cutoff := now.Add(-e.risk.FillBurstWindow)
	kept := e.fillEpisodes[:0]
	for _, at := range e.fillEpisodes {
		if at.After(cutoff) {
			kept = append(kept, at)
		}
	}
	e.fillEpisodes = append(kept, now)
	if len(e.fillEpisodes) < e.risk.FillBurstLimit {
		return
	}

	e.fillBurstUntil = now.Add(e.burstCooldown(now))
	e.deps.Log.Warn("fill burst detected, entering a transient cooldown",
		"symbol", e.symbol, "episodes", len(e.fillEpisodes),
		"window", e.risk.FillBurstWindow, "until", e.fillBurstUntil.UTC())
}

// minBurstCooldown — сколько длится пауза, даже когда часу уже нечего терять.
const minBurstCooldown = 30 * time.Second

// burstCooldown ограничивает паузу тем, что час способен впитать.
//
// Зачёт ступенчатый: пока присутствие держится выше порога тира, простой стоит
// ноль, а шаг за порог обнуляет и уже накопленное. Пауза длиннее оставшегося
// бюджета простоя поэтому не «немного дороже» — она превращает мелкий инцидент
// в потерянный час целиком, после чего uptime.Lost снимает котировки до конца
// часа и без неё. Двадцатиминутная пауза при бюджете в 28% часа именно так
// и работала: два залива стоили всей смены.
//
// Ограничение никогда не опускает паузу ниже minBurstCooldown: когда бюджет
// уже исчерпан, отойти всё равно нужно — просто ненадолго, потому что платить
// присутствием больше нечем.
func (e *Engine) burstCooldown(now time.Time) time.Duration {
	cooldown := e.risk.FillBurstCooldown
	budget := e.hour.Assess(now).DowntimeBudget
	if cooldown <= budget {
		return cooldown
	}
	return max(budget, min(cooldown, minBurstCooldown))
}

// publishFill сообщает защитному контуру о заливе вместе с оценкой стоимости.
//
// Оценка считается здесь, а не в контуре, потому что здесь есть измеренный
// спред: цена закрытия по рынку отличается от цены исполнения на тейкерскую
// комиссию и на проскальзывание порядка половины спреда. Собственный расчёт
// нужен потому, что канал сделок биржи не проверен длительной эксплуатацией,
// а защита, зависящая от одного источника, отказывает молча.
func (e *Engine) publishFill(
	ctx context.Context,
	order trading.Order,
	qty money.Qty,
	startsEpisode bool,
	now time.Time,
) {
	price := order.AvgFillPrice
	if !price.IsPositive() {
		price = e.mark
	}
	notional := qty.Decimal().Mul(price.Decimal()).Abs()

	costBps := e.risk.Fees.MakerBps + e.risk.Fees.TakerBps
	if half, ok := e.halfSpreadBps(); ok {
		costBps += half
	}
	cost := notional.Mul(costBps.Ratio())

	events.Publish(e.deps.Bus, ctx, events.QuoteFilled{
		Symbol:           e.symbol,
		Side:             order.Side,
		Qty:              qty,
		Price:            price,
		NotionalUSD:      notional,
		EstimatedCostUSD: cost,
		StartsEpisode:    startsEpisode,
		At:               now,
	})
}

// flattenGrace — сколько ждать PositionUpdated после залива.
//
// Между исполнением и обновлением позиции проходит отдельное сообщение, поэтому
// нулевая позиция сразу после залива ещё ничего не значит. Но ждать её вечно
// нельзя: залив мог и не открыть позиции — например, встречное исполнение
// на другой стороне свело её в ноль. Незакрытый эпизод в этом случае навсегда
// перекрывает котирование, потому что снять его больше некому.
const flattenGrace = 2 * time.Second

// flatten reconciles any residual position with a market order.
//
// The immediate close is submitted directly from onFill. This slower path is
// retained for a rejected or partially filled close, an unexpected external
// position, or a missed order update.
func (e *Engine) flatten(ctx context.Context) {
	now := e.deps.Clock.Now()

	position := e.position
	if position.IsZero() {
		if e.pendingFill.IsPositive() && now.Before(e.fillDeadline.Add(flattenGrace)) {
			// Исполнение уже зафиксировано, но PositionUpdated ещё не пришёл.
			// Следующий тик или событие позиции повторит попытку.
			return
		}
		e.clearEpisode(now)
		return
	}

	if now.Before(e.flattenRetryAt) {
		return
	}

	side := market.Sell
	if position.IsNegative() {
		side = market.Buy
	}

	if !e.submitFlatten(ctx, side, position.Abs(), now, "position reconciliation") {
		return
	}

	e.pendingFill = money.MustParseQty("0")
	e.fillDeadline = time.Time{}
	e.cooldownUntil = now.Add(e.risk.PostFillCooldown)
}

// submitFlatten sends one reduce-only IOC close and remembers its identity.
//
// The identifier is stored before the request: its fill report must be
// recognized as a close rather than as a new quote fill. flattenRetryAt is set
// even on a transport error because placement errors can be ambiguous; the
// position-based path waits for reconciliation before risking a duplicate.
func (e *Engine) submitFlatten(
	ctx context.Context,
	side market.Side,
	qty money.Qty,
	now time.Time,
	reason string,
) bool {
	if !side.Valid() || !qty.IsPositive() {
		return false
	}

	e.flattenRetryAt = now.Add(e.health.AckTimeout)
	id := trading.ClOrdID(ulid.Make().String())
	e.flattenOrders[id] = now

	err := e.deps.Gateway.Place(ctx, ports.PlaceRequest{
		ClOrdID:     id,
		Symbol:      e.symbol,
		Side:        side,
		Type:        trading.Market,
		TimeInForce: trading.IOC,
		Qty:         qty,
		ReduceOnly:  true,
		MarginMode:  e.risk.MarginMode,
		Leverage:    e.cfg.SymbolRisk.Leverage,
		Priority:    ports.PrioritySafety,
	})
	if err != nil {
		delete(e.flattenOrders, id)
		e.deps.Log.Error("flatten failed",
			"symbol", e.symbol, "side", side, "qty", qty, "reason", reason, "error", err)
		e.deps.Metrics.OrderFailed(e.symbol, "flatten")
		return false
	}

	if until := now.Add(e.risk.PostFillCooldown); until.After(e.cooldownUntil) {
		e.cooldownUntil = until
	}
	e.deps.Log.Info("closing the position at market",
		"symbol", e.symbol, "side", side, "qty", qty, "reason", reason, "cl_ord_id", id)
	return true
}

// clearEpisode закрывает эпизод залива, не породивший позиции.
//
// Проверка на положительный залив обязательна: без неё каждый тик плоского
// актора продлевал бы паузу после залива, и котирование не возобновилось бы
// никогда.
func (e *Engine) clearEpisode(now time.Time) {
	if !e.pendingFill.IsPositive() {
		return
	}
	e.deps.Log.Warn("fill did not open a position, closing the episode",
		"symbol", e.symbol, "qty", e.pendingFill)

	e.pendingFill = money.MustParseQty("0")
	e.fillDeadline = time.Time{}
	e.cooldownUntil = now.Add(e.risk.PostFillCooldown)
}

// statusInterval — период сводки о состоянии актора.
const statusInterval = 15 * time.Second

// reportBlocked prints the summary for paths that abandon the evaluation
// before any quote geometry exists.
//
// Those paths never reach the geometry, and without an explicit call the actor
// falls completely silent — for the rest of the hour when the hour is lost, and
// for a whole weekend when the reference market is shut. A silent actor is
// indistinguishable from a broken one.
func (e *Engine) reportBlocked(
	now time.Time,
	conditions policy.Conditions,
	estimate volatility.Estimate,
	reason string,
	quote policy.Quote,
) {
	e.blocked = reason
	e.reportStatus(now, conditions, estimate, quote)
	e.reportMetrics(now, conditions, estimate, quote)
	e.blocked = ""
}

// geometry считает, где стояла бы котировка, не трогая состояние.
//
// Нужна воротам часа. Они обрывают расчёт раньше геометрии, и сводка тогда
// сообщает только про час, умалчивая, пустил бы вообще рынок. При разборе
// 7 августа это стоило дня догадок: в логе стояло «hour is unreachable»,
// а блокировала на самом деле дистанция.
//
// Липкая цель здесь намеренно не применяется: она хранит защёлку гистерезиса,
// и трогать её ради наблюдения значило бы менять решение отчётом о нём.
func (e *Engine) geometry(a policy.Appetite, c policy.Conditions) policy.Quote {
	return e.ladder.Anchor(a.Target(c, e.bounds), e.bounds)
}

// reportStatus печатает, что актор думает о рынке прямо сейчас.
//
// Без этого теневой прогон невозможно интерпретировать: молчащий актор
// выглядит одинаково и когда он ждёт прогрева, и когда сломан.
func (e *Engine) reportStatus(
	now time.Time,
	conditions policy.Conditions,
	estimate volatility.Estimate,
	quote policy.Quote,
) {
	if now.Sub(e.lastStatus) < statusInterval {
		return
	}
	e.lastStatus = now

	hour := e.hour.Assess(now)
	bidDepth, askDepth := e.bookDepth()
	available, _ := e.available.Float64()
	attrs := []any{
		"symbol", e.symbol,
		"mark", e.mark,
		"half_spread", conditions.HalfSpreadBps,
		"depth_bid_usd", fmt.Sprintf("%.0f", bidDepth),
		"depth_ask_usd", fmt.Sprintf("%.0f", askDepth),
		"jump", estimate.JumpBps,
		"jump_effective", conditions.JumpBps,
		// Мера риска — большее из живого окна и затухающего рекорда, и по одному
		// итогу нельзя сказать, какое из двух держит бота вне рынка. Разница
		// решает, что чинить: длину окна или ограничение памяти. Поля для этого
		// в Estimate заведены с самого начала, а в сводку не попадали — 10 августа
		// сорокаминутный простой пришлось раскладывать по скорости распада
		// значения между строками лога.
		"jump_window", estimate.WindowMaxBps,
		"jump_memory", estimate.MemoryBps,
		"samples", estimate.Samples,
		"tick", estimate.Tick,
		"confidence", fmt.Sprintf("%.0f%%", estimate.Confidence*100),
		// As a string, not the value: the slog JSON handler does not call
		// String() on numeric types, and the mode printed as "0" in the live
		// log.
		"hour_mode", hour.Mode.String(),
		"hour_tier", fmt.Sprintf("%.2fx", hour.TierMultiplier),
		"hour_target_pct", fmt.Sprintf("%.0f", hour.TargetPct),
		"uptime_pct", fmt.Sprintf("%.0f", hour.QualifiedPct),
		"maker_hours", fmt.Sprintf("%.4f", hour.MakerHours),
		"accrual_multiplier", fmt.Sprintf("%.2f", hour.Multiplier),
		"bids_live", e.bids.LiveCount(),
		"asks_live", e.asks.LiveCount(),
		// Какой сигнал сейчас правит порогами опасности. Без этого разбор
		// пропущенного залива начинается с догадки о том, работал ли ориентир.
		"fast_signal", e.fastSignal(),
		// Поза по календарю: обычная, отход или отсутствие. Печатается всегда,
		// а не только при отходе — «обычная» здесь такое же утверждение,
		// как остальные, и именно его хочется видеть после публикации.
		"news", e.newsSummary(now),
		// Свободная маржа и связывающее размер ограничение: без них нельзя
		// понять, сколько депозита допустимо выделить, а с ними это видно
		// за сутки наблюдения.
		"available_usd", fmt.Sprintf("%.0f", available),
		// Действующая доля блокировки: единственный способ увидеть, применилось
		// ли плечо счёта. Ответ биржи на change_leverage означает приём запроса,
		// а не применение настройки, и расхождение иначе проявилось бы только
		// недобранным начислением.
		"margin_rate", e.observedMarginRate(),
		"margin_source", e.marginSource,
		"per_side", e.perSide,
		"size_limit", e.sizeLimit,
		"rungs", e.rungs,
	}
	if !e.live {
		attrs = append(attrs, "shadow_fills", e.shadowFills)
	}
	switch {
	case e.blocked != "":
		attrs = append(attrs, "quoting", "no", "reason", e.blocked)
	case quote.Feasible:
		attrs = append(attrs, "distance", quote.Distance, "depth", quote.Depth, "margin", quote.Margin)
	default:
		attrs = append(attrs, "quoting", "no", "reason", quote.Reason)
	}
	e.deps.Log.Info("engine status", attrs...)
}

const (
	// stallThreshold — сколько актор вправе не котировать, прежде чем это
	// перестаёт быть штатным воздержанием.
	//
	// Пять минут выбраны по программе, а не по вкусу: на простой в час есть 28%
	// при годном только тире 1.0x, то есть меньше семнадцати минут. Отсутствие
	// котировок пять минут подряд расходует треть этого запаса — событие
	// достаточно редкое, чтобы им не засорять лог, и достаточно дорогое, чтобы
	// узнать о нём до конца часа, а не после.
	stallThreshold = 5 * time.Minute
	// stallLogInterval прореживает повтор тревоги.
	stallLogInterval = 15 * time.Minute
)

// noteStall поднимает затянувшееся молчание из сводки в тревогу.
//
// Сводка отвечает на вопрос «почему сейчас нет котировок», и этого мало:
// причина, названная один раз в пятнадцать секунд ровным тоном INFO, теряется
// в потоке. Нужен отдельный уровень для случая, когда воздержание перестало
// быть эпизодом, и полный снимок управляющего состояния вместе с ним — иначе
// следующий разбор снова начнётся с восстановления невидимых полей по
// косвенным уликам.
//
// Печатается всё, что способно удержать актор вне рынка, включая заведомо
// пустые поля: отсутствие значения здесь такое же свидетельство, как значение.
func (e *Engine) noteStall(now time.Time, quoting bool, reason string) {
	if quoting {
		e.blockedSince, e.nextStallLog = time.Time{}, time.Time{}
		return
	}
	if e.blockedSince.IsZero() {
		e.blockedSince = now
		return
	}
	if now.Sub(e.blockedSince) < stallThreshold || now.Before(e.nextStallLog) {
		return
	}
	e.nextStallLog = now.Add(stallLogInterval)

	estimate := e.vol.Estimate(now)
	e.deps.Log.Error("no quoting for a long stretch",
		"symbol", e.symbol,
		"for", now.Sub(e.blockedSince).Truncate(time.Second).String(),
		"reason", reason,
		"halted", e.halted,
		"halt_reason", e.haltReason,
		"risk_pause_until", stamp(e.riskPauseUntil),
		"resync_until", stamp(e.resyncUntil),
		"cooldown_until", stamp(e.cooldownUntil),
		"fill_burst_until", stamp(e.fillBurstUntil),
		"flatten_retry_at", stamp(e.flattenRetryAt),
		"pending_fill", e.pendingFill,
		"fill_deadline", stamp(e.fillDeadline),
		"position", e.position,
		"max_net_position", e.maxNetPosition,
		"flatten_orders", len(e.flattenOrders),
		"pending_withdraw", len(e.pendingWithdraw),
		"mark", e.mark,
		"mark_age", now.Sub(e.markAt).Truncate(time.Millisecond).String(),
		"book_readable", e.bookReadable(),
		"estimate_ready", estimate.Ready,
		"estimate_samples", estimate.Samples,
		"available_usd", e.available,
		"bids_live", e.bids.LiveCount(),
		"asks_live", e.asks.LiveCount())
}

// blockedReason возвращает причину, по которой котировок нет.
func (e *Engine) blockedReason(quote policy.Quote) string {
	if e.blocked != "" {
		return e.blocked
	}
	return quote.Reason
}

// bookReadable сообщает, читается ли стакан обеими сторонами.
func (e *Engine) bookReadable() bool {
	_, ok := e.halfSpreadBps()
	return ok
}

// stamp печатает момент так, чтобы «не задано» отличалось от «задано давно».
func stamp(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.UTC().Format(time.RFC3339)
}

// reportMetrics выкладывает состояние актора в метрики.
//
// Вызывается на каждом пересчёте, а не по расписанию сводки в логе: метрики
// снимаются извне со своей частотой, и разрежать их вручную значило бы терять
// разрешение как раз на быстрых эпизодах, ради которых наблюдаемость и нужна.
func (e *Engine) reportMetrics(
	now time.Time,
	conditions policy.Conditions,
	estimate volatility.Estimate,
	quote policy.Quote,
) {
	m := e.deps.Metrics
	hour := e.hour.Assess(now)

	e.noteFastSignal(now)

	m.QuoteUptime(e.symbol, hour.QualifiedPct/100)
	m.EstimatedMakerHours(e.symbol, hour.MakerHours)
	m.HalfSpread(e.symbol, float64(conditions.HalfSpreadBps))
	bidDepth, askDepth := e.bookDepth()
	m.BookDepth(e.symbol, market.Buy, bidDepth)
	m.BookDepth(e.symbol, market.Sell, askDepth)
	m.Jump(e.symbol, float64(estimate.EffectiveJump()))
	m.LiveQuotes(e.symbol, market.Buy, e.bids.LiveCount())
	m.LiveQuotes(e.symbol, market.Sell, e.asks.LiveCount())

	quoting := quote.Feasible && e.blocked == ""
	m.Quoting(e.symbol, quoting)
	e.noteStall(now, quoting, e.blockedReason(quote))
	if quoting {
		m.TargetDistance(e.symbol, float64(quote.Distance))
	}
}

// fastSignal называет источник, питающий пороги опасности прямо сейчас.
func (e *Engine) fastSignal() string {
	switch {
	case e.refStaleAfter <= 0:
		return "book"
	case e.referenceLeading:
		return "reference"
	default:
		return "book (reference stale)"
	}
}

// noteFastSignal публикует возраст ориентира и печатает смену источника
// быстрого сигнала.
//
// Переход именно печатается, а не выводится из метрики: замолчавший ориентир
// не проявляется ничем — котирование продолжается, дистанции те же, исчезает
// лишь предупреждение, а отсутствие предупреждения ненаблюдаемо по определению.
// Строка на переходе, а не на каждом такте: смена источника — событие, и в логе
// она должна выглядеть событием.
func (e *Engine) noteFastSignal(now time.Time) {
	if e.refStaleAfter <= 0 {
		return // ориентир для инструмента не настроен
	}

	// Возраст обязан быть конечным и растущим.
	//
	// Бесконечность на «ещё не приходил» напрашивалась, но почасовая свёртка
	// кладёт квантили в JSON, а json.Encode на ±Inf возвращает ошибку — то есть
	// одна ненастроенная площадка стоила бы всей истории показателей за час.
	// Отсчёт от момента, с которого актор ждёт, даёт конечное число и делает
	// «не подключился ни разу» неотличимым от «умер»: для быстрого сигнала это
	// и есть одно и то же.
	if e.referenceWaitSince.IsZero() {
		e.referenceWaitSince = now
	}
	since := e.referenceAt
	if since.IsZero() {
		since = e.referenceWaitSince
	}
	e.deps.Metrics.ReferenceStaleness(e.symbol, now.Sub(since).Seconds())

	_, leading := e.referenceDrift()
	if leading == e.referenceLeading {
		return
	}
	e.referenceLeading = leading

	if leading {
		e.deps.Log.Info("fast signal now leads on the external reference",
			"symbol", e.symbol, "reference", e.reference)
		return
	}
	e.deps.Log.Warn("external reference is stale, falling back to the book mid",
		"symbol", e.symbol,
		"age", now.Sub(since).Truncate(time.Millisecond).String(),
		"stale_after", e.refStaleAfter)
}

// observedMarginRate — какую долю выставленного нотионала биржа держит
// заблокированной прямо сейчас.
//
// Считается по факту, а не берётся из конфигурации: настроенное значение
// говорит, во что мы верим, а это — что происходит. Расхождение означает, что
// плечо счёта не то, которое мы просили, и размер посчитан не от той величины.
//
// Прочерк, пока в стакане пусто: делить на ноль нечего, а «0%» читалось бы
// как «маржа не берётся».
func (e *Engine) observedMarginRate() string {
	rate, ok := e.measuredMarginRate()
	if !ok {
		return "-"
	}
	return fmt.Sprintf("%.3f%%", rate)
}

// measuredMarginRate — какую долю выставленного нотионала биржа держит
// заблокированной прямо сейчас, в процентах. false, пока мерить нечего.
func (e *Engine) measuredMarginRate() (float64, bool) {
	notional := decimal.Zero
	for _, ladder := range []*quotes.Ladder{e.bids, e.asks} {
		for _, slot := range ladder.Slots {
			if slot.Phase == quotes.Live {
				notional = notional.Add(money.Notional(slot.Price, slot.Qty))
			}
		}
	}
	if !notional.IsPositive() || !e.locked.IsPositive() {
		return 0, false
	}
	// Позиция тоже держит маржу, и во время залива отношение уезжает вверх.
	// Это безопасная сторона: завышенная доля даёт меньший размер.
	rate, _ := e.locked.Div(notional).Float64()
	return rate * 100, true
}

// marginRate — действующая доля блокировки, которой считается размер.
//
// Измеряется, а не задаётся. Настроенное число говорит, во что мы верим,
// и стареет молча: 10.542% сняты 12 августа при плече 9.49, а после того как
// плечо стало 40, биржа держит втрое с лишним меньше. Размер, посчитанный
// по старому числу, вчетверо ниже того, что позволяет капитал, и увидеть это
// можно было только по расхождению в сводке.
//
// Пол — физический: initial margin не может быть ниже 1/плечо, поэтому
// испорченное измерение не способно раздуть размер сильнее, чем разрешает
// само плечо. Это и делает измерение безопасным: ошибиться вниз оно может
// ровно до этой границы и ни на процент дальше.
//
// Пока измерять нечего — при старте, до первых живых ордеров — берётся
// настроенное значение. Оно консервативнее измеренного, и это верный перекос
// для величины, от которой зависит размер.
func (e *Engine) marginRate() (float64, string) {
	configured := e.cfg.SymbolRisk.InitialMarginPct

	floor := configured
	if lev := e.cfg.SymbolRisk.Leverage; lev >= 1 {
		floor = 100 / float64(lev)
	}

	measured, ok := e.measuredMarginRate()
	if !ok {
		return configured, "configured"
	}
	if measured < floor {
		return floor, "measured, clamped to the leverage floor"
	}
	return measured, "measured"
}

// bookDepth возвращает нотионал в пределах полосы награды по каждой стороне.
//
// Опорой служит официальный mark, а не середина стакана: полоса награды
// отсчитывается именно от него, и глубину имеет смысл мерить в тех же
// координатах, в которых стоит котировка.
func (e *Engine) bookDepth() (bid, ask float64) {
	b, _ := e.book.DepthNotional(e.mark, market.Buy, e.bounds.MaxBps).Float64()
	a, _ := e.book.DepthNotional(e.mark, market.Sell, e.bounds.MaxBps).Float64()
	return b, a
}

// priorityOf отображает мотив решения в приоритет шейпера запросов.
func priorityOf(u policy.Urgency) ports.Priority {
	if u == policy.UrgencySafety {
		return ports.PrioritySafety
	}
	return ports.PriorityQuote
}
