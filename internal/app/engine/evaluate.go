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
	if e.halted || !e.mark.IsPositive() {
		// Молчание метрики неоднозначно: остановленный актор выглядел бы так же,
		// как не запущенный. Ноль — утверждение, отсутствие ряда — нет.
		e.deps.Metrics.Quoting(e.symbol, false)
		return
	}
	now := e.deps.Clock.Now()

	// Незакрытая позиция важнее награды: котировать, имея экспозицию, значит
	// удвоить её при следующем заливе.
	if now.Before(e.flattenRetryAt) || e.pendingFill.IsPositive() || e.overPositionLimit() {
		e.withdraw(ctx, e.positionCause(), "no quoting while exposure is open")
		return
	}
	if now.Before(e.cooldownUntil) {
		return
	}

	halfSpread, ok := e.halfSpreadBps()
	if !ok {
		e.withdraw(ctx, ports.CauseBookUnreadable, "no two-sided book")
		return
	}

	estimate := e.vol.Estimate(now)
	conditions := policy.Conditions{
		HalfSpreadBps: halfSpread,
		JumpBps:       estimate.EffectiveJump(),
		Ready:         estimate.Ready,
	}

	hour := e.hour.Assess(now)

	// The reference market is shut: the exchange scales the hour's accrual
	// while the fill risk stays exactly the same. For gold that is a tenth,
	// so the break-even price of a Maker Hour rises tenfold.
	if hour.Multiplier < 1 && !e.cfg.Program.QuoteOffHours {
		reason := fmt.Sprintf(
			"the reference market is closed, the hour accrues at %.0f%%", hour.Multiplier*100)
		e.reportBlocked(now, conditions, estimate, reason)
		e.withdraw(ctx, ports.CauseOffHours, reason)
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
		e.reportBlocked(now, conditions, estimate, hour.Reason)
		e.withdraw(ctx, ports.CauseHourLost, hour.Reason)
		return
	case uptime.Banked:
		// Цель взята, дополнительный аптайм ничего не добавляет — отходим.
		appetite.JumpMultiple *= bankedCaution
	}

	bounds := e.quotingBounds()
	quote := e.stickyTarget(e.ladder.Anchor(appetite.Target(conditions, bounds), bounds))
	e.reportStatus(now, conditions, estimate, quote)
	e.reportMetrics(now, conditions, estimate, quote)

	if !quote.Feasible {
		e.withdraw(ctx, ports.CauseNoSafeDistance, quote.Reason)
		return
	}

	thresholds := appetite.Thresholds(quote, conditions, e.bounds)
	distances := e.ladder.Distances(quote.Distance)

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
	plan, err := sizing.Compute(sizing.Inputs{
		Budget: sizing.Budget{
			AllocationUSD:  decimal.NewFromFloat(e.cfg.AllocationUSD),
			AvailableUSD:   e.available,
			Leverage:       e.cfg.SymbolRisk.Leverage,
			UtilizationPct: utilizationPct,
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
	pulled := false

	for _, side := range []market.Side{market.Buy, market.Sell} {
		ladder := e.ladderFor(side)
		actions := ladder.Danger(e.classifier(side, th))
		if len(actions) == 0 {
			continue
		}
		pulled = true
		e.lastAction = now
		// allowPlace = false: съём никогда не сопровождается размещением,
		// а выставлять по цене, от которой мы только что убежали, — тем более.
		e.execute(ctx, side, actions, false, nil)
	}
	return pulled
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
	// Биржа отклонит пересекающий ALO-ордер сама, но проверка на нашей стороне
	// экономит запрос и, что важнее, ловит ошибку в расчёте дистанции.
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
	e.deps.Metrics.OrderSent(e.symbol, side, "place")
	e.deps.Metrics.QuoteDistance(e.symbol, side, float64(action.Distance))
	// Логируется и успех: в боевом режиме заглушка молчит, и без этой записи
	// по логу невозможно отличить работающего бота от бездействующего.
	e.deps.Log.Info("quote placed",
		"symbol", e.symbol, "side", side, "slot", action.Slot,
		"price", price, "qty", qty, "distance", action.Distance, "reason", action.Reason)

	e.ladderFor(side).OnPlaceSent(action.Slot, id, price, qty, now)
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
func (e *Engine) sendWithdrawal(ctx context.Context, ids []trading.ClOrdID) {
	if err := e.deps.Gateway.CancelBatch(ctx, ids, ports.PrioritySafety); err != nil {
		e.deps.Log.Error("withdrawal failed, retrying",
			"symbol", e.symbol, "count", len(ids), "attempt", e.withdrawAttempts+1, "error", err)
		e.deps.Metrics.OrderFailed(e.symbol, "cancel_batch")
		e.queueWithdrawal(ids)
		return
	}
	e.forgetWithdrawal(ids)
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

// onFill накапливает исполнение и назначает срок закрытия.
//
// Дебаунс существует потому, что свип даёт десятки частичных исполнений подряд.
// Закрывать каждое отдельным маркет-ордером значило бы платить тейкерскую
// комиссию столько же раз.
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

	e.pendingFill = e.pendingFill.Add(delta)
	if e.fillDeadline.IsZero() || e.fillDeadline.Before(now) {
		e.fillDeadline = now.Add(e.risk.FlattenDebounce)
	}

	e.deps.Log.Warn("order filled",
		"symbol", e.symbol, "side", order.Side,
		"qty", delta, "price", order.AvgFillPrice, "new_episode", startsEpisode)

	e.publishFill(ctx, order, delta, startsEpisode, now)
	e.withdraw(ctx, ports.CauseFilled, "closing the position")
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

// flatten закрывает позицию рыночным ордером.
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
	e.flattenRetryAt = now.Add(e.health.AckTimeout)

	side := market.Sell
	if position.IsNegative() {
		side = market.Buy
	}

	// Идентификатор запоминается до отправки: сообщение об исполнении этого
	// ордера обязано быть опознано как закрытие, а не как новый залив.
	id := trading.ClOrdID(ulid.Make().String())
	e.flattenOrders[id] = now

	err := e.deps.Gateway.Place(ctx, ports.PlaceRequest{
		ClOrdID:     id,
		Symbol:      e.symbol,
		Side:        side,
		Type:        trading.Market,
		TimeInForce: trading.IOC,
		Qty:         position.Abs(),
		// reduce_only исключает переворот позиции, если количество устарело
		// к моменту, когда ордер дойдёт до матчинга.
		ReduceOnly: true,
		MarginMode: e.risk.MarginMode,
		Leverage:   e.cfg.SymbolRisk.Leverage,
		Priority:   ports.PrioritySafety,
	})
	if err != nil {
		delete(e.flattenOrders, id)
		e.deps.Log.Error("flatten failed", "symbol", e.symbol, "qty", position, "error", err)
		return
	}

	e.pendingFill = money.MustParseQty("0")
	e.fillDeadline = time.Time{}
	e.cooldownUntil = now.Add(e.risk.PostFillCooldown)
	// Ордер отправлен, но не исполнен: подтверждение придёт отдельным сообщением,
	// и до него позиция остаётся открытой.
	e.deps.Log.Info("closing the position at market",
		"symbol", e.symbol, "side", side, "qty", position, "cl_ord_id", id)
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
) {
	e.blocked = reason
	e.reportStatus(now, conditions, estimate, policy.Quote{})
	e.reportMetrics(now, conditions, estimate, policy.Quote{})
	e.blocked = ""
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
		"samples", estimate.Samples,
		"tick", estimate.Tick,
		"confidence", fmt.Sprintf("%.0f%%", estimate.Confidence*100),
		// As a string, not the value: the slog JSON handler does not call
		// String() on numeric types, and the mode printed as "0" in the live
		// log.
		"hour_mode", hour.Mode.String(),
		"uptime_pct", fmt.Sprintf("%.0f", hour.QualifiedPct),
		"maker_hours", fmt.Sprintf("%.4f", hour.MakerHours),
		"accrual_multiplier", fmt.Sprintf("%.2f", hour.Multiplier),
		"bids_live", e.bids.LiveCount(),
		"asks_live", e.asks.LiveCount(),
		// Свободная маржа и связывающее размер ограничение: без них нельзя
		// понять, сколько депозита допустимо выделить, а с ними это видно
		// за сутки наблюдения.
		"available_usd", fmt.Sprintf("%.0f", available),
		"per_side", e.perSide,
		"size_limit", e.sizeLimit,
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
	if quoting {
		m.TargetDistance(e.symbol, float64(quote.Distance))
	}
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
