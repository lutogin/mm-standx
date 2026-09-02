package engine

import (
	"context"
	"fmt"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/shopspring/decimal"

	"github.com/nd/mm-standx/internal/config"
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

	// Глубина замеряется здесь, где стакан заведомо двусторонний, и до всякого
	// раннего выхода: норма обязана продолжать накапливаться и тогда, когда
	// актор не котирует, иначе после каждой паузы окно оказывалось бы пустым
	// ровно в тот момент, когда судить о стакане нужнее всего.
	bidUSD, askUSD := e.bookDepth()
	e.liq.Observe(market.Buy, bidUSD, now)
	e.liq.Observe(market.Sell, askUSD, now)

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
		// quoteOffHours is the operator accepting the reduced accrual, so the
		// gate prices the hour at full rate and does not refuse it a second
		// time on the same grounds. The tier multiplier stays as measured:
		// that one says how much of the hour is still reachable, which the
		// flag has no opinion about.
		accrual := hour.Multiplier
		if e.cfg.Program.QuoteOffHours {
			accrual = 1
		}
		decision := e.economics.Evaluate(hour.TierMultiplier, accrual)
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
	quote := e.stickyTarget(now, anchored)
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
	// Глубина судится один раз за такт: вердикт нужен и съёму ниже, и запрету
	// на размещение в самом конце, а печатать переход дважды нельзя.
	thin := e.thinSides(now)
	// Съём по обвалу стакана стоит здесь по той же причине, что и опасность:
	// кулдаун бережёт бюджет запросов, а этот сигнал опережает движение
	// на доли секунды, и четверть секунды задержки съедает весь запас.
	if e.pullThin(ctx, thin, now) {
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
	// Урезание выражено долей капитала, а не суммой: настроенной суммы больше
	// нет, бюджет считается от баланса счёта (sizing.Budget).
	utilization := e.risk.UtilizationPct
	if news.defensive {
		utilization *= e.newsGuard.DefensiveSizeMultiple
	}
	marginRate, marginSource := e.marginRate()
	e.marginSource = marginSource

	plan, err := sizing.Compute(sizing.Inputs{
		Budget: sizing.Budget{
			EquityUSD:        e.equity,
			SpendableUSD:     e.spendable(),
			InitialMarginPct: marginRate,
			UtilizationPct:   utilization,
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
		e.execute(ctx, side, actions, allowPlace && !thin[side], plan.Rungs)
	}
}

// thinSides судит обе стороны по глубине стакана и печатает переходы.
//
// Считается один раз за такт: вердикт нужен и съёму, и запрету на размещение,
// а строка о переходе обязана появиться ровно однажды.
func (e *Engine) thinSides(now time.Time) map[market.Side]bool {
	out := map[market.Side]bool{}
	for _, side := range []market.Side{market.Buy, market.Sell} {
		v := e.liq.Verdict(side, now)
		out[side] = v.Thin
		if v.Thin == e.thinSide[side] {
			continue
		}
		e.thinSide[side] = v.Thin
		if v.Thin {
			e.deps.Log.Warn("book depth collapsed on this side",
				"symbol", e.symbol, "side", side,
				"depth_usd", int64(v.DepthUSD), "median_usd", int64(v.MedianUSD),
				"threshold_usd", int64(v.ThresholdUSD))
		} else {
			e.deps.Log.Info("book depth recovered, quoting the side again",
				"symbol", e.symbol, "side", side,
				"depth_usd", int64(v.DepthUSD), "median_usd", int64(v.MedianUSD))
		}
	}
	return out
}

// pullThin снимает сторону, из-под которой ушла ликвидность.
//
// ПОЧЕМУ СЪЁМ, А НЕ ТОЛЬКО ЗАПРЕТ НА РАЗМЕЩЕНИЕ. Первая версия ворот держала
// только новые ордера, и замер 2 сентября 2026 показал, чего это стоит:
//
//	09:05:19.441  выставлен sell 4326.3
//	09:05:59.668  ворота: глубина аска 2.84M против порога 4.90M
//	09:06:00.073  биржа свела этот самый ордер
//	09:06:11      jump вырос с 0.98 до 6.74
//
// Вердикт опередил исполнение на 405 миллисекунд, а обвал глубины опередил
// само движение на десяток секунд. Круг запроса на снятие — десятки
// миллисекунд, то есть запаса хватало с избытком. Не хватило права снимать:
// ордер стоял с 09:05:19, под запрет на размещение не подпадал и дождался
// своей сделки.
//
// Уходит вся сторона сразу, как и на пути опасности: глубина — свойство
// стороны целиком, и оставлять на ней часть ступеней значит оставлять их
// под тем же ударом.
//
// Возврат отдельного запрета не требует: пока сторона тонка, тот же вердикт
// не даёт её заполнить.
func (e *Engine) pullThin(ctx context.Context, thin map[market.Side]bool, now time.Time) bool {
	if !e.cfg.Liquidity.PullOnThin {
		return false
	}

	var ids []trading.ClOrdID
	var sides []market.Side
	for _, side := range []market.Side{market.Buy, market.Sell} {
		if !thin[side] {
			continue
		}
		ladder := e.ladderFor(side)
		actions := ladder.PullAll(now, 0, "book depth collapsed")
		for _, action := range actions {
			ids = append(ids, action.ClOrdID)
			ladder.OnCancelSent(action.Slot, now)
		}
		if len(actions) > 0 {
			sides = append(sides, side)
		}
	}
	if len(ids) == 0 {
		return false
	}

	e.lastAction = now
	if !e.sendWithdrawal(ctx, ids) {
		return true
	}
	for _, side := range sides {
		e.deps.Metrics.OrderSent(e.symbol, side, "cancel")
	}
	e.deps.Log.Warn("side pulled, the book under it went thin",
		"symbol", e.symbol, "sides", sides, "orders", len(ids))
	return true
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
		actions := ladder.Danger(now, e.classifier(side, th), e.rules.RefillLockout)
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
// Новая цель принимается немедленно, когда она требует отойти дальше, либо
// когда расхождение уже не спишешь на дрожание. Возврат к рынку допускает
// третье основание — время: требование, продержавшееся ниже стоящей цели
// целый TargetRelaxAfter, дрожанием не является.
//
// Третье основание существует потому, что без него порог расхождения работал
// не гистерезисом, а храповиком. Свежая цель снизу зажата полом (minBps плюс
// половина ширины лесенки), значит расхождение с ней ограничено сверху тем,
// насколько цель отодвинулась от пола. Всякий всплеск, отодвинувший её меньше
// чем на MinDeadBandBps, разрешить возврат уже не мог ничем: рынок успокаивался,
// требование падало до пола, а лесенка так и стояла снаружи. На боевом счёте
// это держало центр на 8.4 bps более двадцати часов подряд при требовании 3.1
// и поле 7.4 — базисный пункт дистанции, который никто не выбирал, ценой
// пятой части веса котировки.
func (e *Engine) stickyTarget(now time.Time, fresh policy.Quote) policy.Quote {
	if !fresh.Feasible || !e.target.Feasible {
		e.target = fresh
		e.targetNarrowSince = time.Time{}
		return fresh
	}

	// Осторожность применяется немедленно и обнуляет отсчёт: требование
	// вернулось к стоящей цели, отступать больше не от чего.
	if fresh.Distance >= e.target.Distance {
		e.targetNarrowSince = time.Time{}
		if fresh.Distance > e.target.Distance {
			e.target = fresh
		}
		return e.target
	}

	if e.target.Distance-fresh.Distance >= e.cfg.Appetite.MinDeadBandBps {
		e.target = fresh
		e.targetNarrowSince = time.Time{}
		return e.target
	}

	if e.cfg.Appetite.TargetRelaxAfter <= 0 {
		return e.target
	}
	if e.targetNarrowSince.IsZero() {
		e.targetNarrowSince = now
		return e.target
	}
	if now.Sub(e.targetNarrowSince) >= e.cfg.Appetite.TargetRelaxAfter {
		e.target = fresh
		e.targetNarrowSince = time.Time{}
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

	price, ok := e.placementPrice(side, action.Distance)
	if !ok {
		return
	}
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

// placementPrice считает цену новой ступени так, чтобы она стояла не ближе
// заданной дистанции НИ ОТ ОФИЦИАЛЬНОГО MARK, НИ ОТ БЫСТРОЙ ОЦЕНКИ.
//
// СНЯЛ И ПОСТАВИЛ ТУДА ЖЕ. Опасность судится по быстрому сигналу (Engine.
// classifier), а цена ступени считалась только от официального mark. Когда
// ориентир уже ушёл, а mark ещё стоит, эти два взгляда расходятся, и бот
// делал ровно то, что видно в логе 2 сентября 2026:
//
//	11:53:29.194  cancelled sell slot 0 — closer than target range
//	11:53:29.811  placed    sell slot 0 @ 4334.9      <- та же цена
//	11:53:30.229  cancelled sell slot 0 — closer than target range
//	11:53:30.570  placed    sell slot 0 @ 4334.9      <- та же цена
//	11:53:31.151  cancelled sell slot 0 — closer than target range
//
// Классификатор по оценке говорил «слишком близко», планировщик по mark
// отвечал «вот правильное место» — и это было то же самое место. За день
// таких пар 32 из 240, каждая — два запроса и ноль перестановки. В 11:54:22
// сторона была снята тем же способом за 106 мс ПОСЛЕ сведения сделки.
//
// Правило простое: якорем служит тот из двух ориентиров, что ближе к ордеру.
// Для продажи — больший, для покупки — меньший. Оценка может только отодвинуть
// ступень, но никогда не придвинуть: если ориентир ушёл в другую сторону,
// цена считается от mark, как и раньше.
//
// Полоса награды при этом по-прежнему судится по официальному mark — так её
// считает биржа. Если отодвинутая ступень выходит за край полосы, она не
// ставится вовсе: ордер, который не стоит ничего, нечем оправдывать.
func (e *Engine) placementPrice(side market.Side, distance money.Bps) (money.Price, bool) {
	anchor := e.mark
	if est := e.markEstimate(); est.IsPositive() {
		ahead := est.Cmp(e.mark) > 0
		if (side == market.Sell && ahead) || (side == market.Buy && !ahead) {
			anchor = est
		}
	}
	price := e.spec.SnapPrice(market.PriceAtDistance(anchor, side, distance), side)
	if anchor.Cmp(e.mark) == 0 {
		return price, true
	}

	official, err := market.DistanceBps(e.mark, side, price)
	if err != nil || official > e.bounds.MaxBps {
		e.deps.Log.Info("skipping a quote the fast estimate pushed out of the band",
			"symbol", e.symbol, "side", side, "mark", e.mark, "estimate", anchor,
			"price", price, "distance_from_mark", official, "band", e.bounds.MaxBps)
		return price, false
	}
	return price, true
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
		actions := ladder.PullAll(now, e.rules.RefillLockout, string(cause))
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
func (e *Engine) onFill(ctx context.Context, order trading.Order, rung int) {
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
		// Ступень отвечает на вопрос, которого больше не задать задним числом:
		// снимает ли рынок сторону целиком или останавливается на ближней.
		// От этого зависит, покупает ли лесенка что-нибудь за свой средний вес.
		"rung", rung,
		"qty", delta, "price", order.AvgFillPrice, "new_episode", startsEpisode,
		// Отметка биржи и наше отставание от неё. Без них по логу нельзя
		// отличить опоздавшую отмену от ордера, исполненного до решения о ней
		// (Engine.venueLag).
		"venue_ts", order.UpdatedAt, "venue_lag", e.venueLag(order))

	e.publishFill(ctx, order, delta, startsEpisode, rung, now)
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
	rung int,
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
		Rung:             rung,
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
		// Позиции нет — значит и отсчёт попыток к следующему эпизоду
		// отношения не имеет.
		e.flattenAttempts = 0
		if e.pendingFill.IsPositive() && now.Before(e.fillDeadline.Add(flattenGrace)) {
			// Исполнение уже зафиксировано, но PositionUpdated ещё не пришёл.
			// Следующий тик или событие позиции повторит попытку.
			return
		}
		e.clearEpisode(now)
		return
	}

	// Мейкерское закрытие ещё в силе: ему дан срок, и второй ордер поверх
	// него стоил бы комиссии. По истечении срока оно снимается, и остаток
	// позиции уходит маркетом ниже.
	if mc := e.makerClose; mc != nil {
		if now.Before(mc.deadline) {
			return
		}
		e.deps.Log.Warn("maker close timed out, falling back to market",
			"symbol", e.symbol, "cl_ord_id", mc.id, "qty", mc.qty,
			"waited", now.Sub(mc.deadline.Add(-e.risk.MakerUnwindTimeout)).Truncate(time.Millisecond).String())
		if err := e.deps.Gateway.Cancel(ctx, mc.id, ports.PrioritySafety); err != nil {
			e.deps.Log.Warn("cancel of the maker close failed",
				"symbol", e.symbol, "cl_ord_id", mc.id, "error", err)
		}
		e.makerClose = nil
	}

	if now.Before(e.flattenRetryAt) {
		return
	}

	side := market.Sell
	if position.IsNegative() {
		side = market.Buy
	}

	if !e.submitMarketClose(ctx, side, position.Abs(), now, "position reconciliation") {
		return
	}
	// Повтор замедляется, но не прекращается.
	//
	// ПОЧЕМУ НЕ ПОДАВЛЕНИЕ. Напрашивается не повторять, пока предыдущее
	// закрытие в пути: замер 25-28 августа 2026 дал 4 эпизода дублей из 15,
	// худший — шесть ордеров на один залив 0.03 XAU, пять из них отвергнуты.
	// Но размен здесь несимметричен. Дубль стоит запроса и строки в логе,
	// а двойной экспозиции не создаёт вовсе — от неё защищает reduce-only
	// на самих ордерах. Пропущенное закрытие стоит открытой позиции. Поэтому
	// повтор обязан оставаться безусловным: молчание биржи неотличимо
	// от потерянного ордера, и трактовать его надо как потерю.
	//
	// Замедление же обходится даром. Первая попытка по-прежнему через
	// AckTimeout, то есть скорость восстановления в обычном случае не меняется;
	// удваивается только шаг, если биржа продолжает молчать. Эпизод 15:56
	// с ответом через 11.49 с дал бы 4 ордера вместо 6 при той же реакции
	// на первой секунде.
	e.flattenAttempts++
	e.flattenRetryAt = now.Add(e.flattenBackoff())

	e.pendingFill = money.MustParseQty("0")
	e.fillDeadline = time.Time{}
	e.cooldownUntil = now.Add(e.risk.PostFillCooldown)
}

// outstandingFlatten — сколько закрывающего объёма отправлено и ещё не получило
// терминального ответа биржи.
//
// Величина наблюдательная: она печатается рядом с повтором, чтобы по логу было
// видно, повторяем ли мы в пустоту или поверх уже работающего закрытия.
// Решения на ней не строятся — см. Engine.flatten о том, почему повтор
// безусловен.
//
// Записи старше flattenInFlight не считаются: молчание биржи не имеет права
// навсегда объявить объём закрывающимся.
func (e *Engine) outstandingFlatten(now time.Time) money.Qty {
	total := money.MustParseQty("0")
	for _, o := range e.flattenOrders {
		if now.Sub(o.at) <= flattenInFlight {
			total = total.Add(o.qty)
		}
	}
	return total
}

// flattenBackoff — пауза перед следующей попыткой закрыть позицию.
//
// Удвоение от AckTimeout с потолком: восстановление обязано оставаться
// быстрым на первой попытке и переставать быть штормом на десятой.
func (e *Engine) flattenBackoff() time.Duration {
	step := e.health.AckTimeout
	if step <= 0 {
		step = time.Second
	}
	for i := 1; i < e.flattenAttempts && step < flattenBackoffCap; i++ {
		step *= 2
	}
	return min(step, flattenBackoffCap)
}

// submitFlatten закрывает исполнение по настроенной политике.
//
// ЧТО СТОИТ ЗАЛИВ. Разбор 2 сентября 2026, 12 заливов за утро: комиссии
// 31.86 USD, проскальзывание 1.50. Из комиссий 24.8 — тейкерские 4 bps
// на ОДНОМ маркет-закрытии 14.363 XAU (62 000 USD нотионала). Закрыли за
// 350 мс с отклонением 0.1 USD — и заплатили за скорость, которая была
// не нужна: цена после залива в этом эпизоде никуда не ушла.
//
// Политика maker_then_market ставит вместо маркета post-only лимит на вершину
// стакана с нашей стороны и даёт ему срок. Исполнился — комиссия 1 bps
// вместо 4 (а при MM1/MM2 — рибейт). Не исполнился — снимается, остаток
// уходит маркетом. Так же закрывается сторонний бот (makerThenMarketClose).
//
// Позиция при этом открыта дольше — на срок ожидания. Это осознанный размен:
// 3 bps нотионала за каждые несколько секунд экспозиции.
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
	if e.risk.FlattenPolicy == config.FlattenMakerThenMarket && e.makerClose == nil {
		if e.submitMakerClose(ctx, side, qty, now, reason) {
			return true
		}
		// Мейкером не вышло (стакан не читается, отправка не удалась) —
		// закрываем как раньше.
	}
	return e.submitMarketClose(ctx, side, qty, now, reason)
}

// submitMakerClose ставит reduce-only post-only лимит на вершину стакана.
//
// Цена: лучшая, при которой ордер остаётся мейкером. Для покупки это
// лучший аск минус буфер post-only, для продажи — лучший бид плюс буфер.
// MakerUnwindBps > 0 вместо этого считает цену от mark на заданной дистанции,
// как у стороннего бота (MAKER_UNWIND_BPS), и всё равно зажимает её post-only.
func (e *Engine) submitMakerClose(
	ctx context.Context,
	side market.Side,
	qty money.Qty,
	now time.Time,
	reason string,
) bool {
	var want money.Price
	if e.risk.MakerUnwindBps > 0 {
		want = market.PriceAtDistance(e.mark, side, money.Bps(e.risk.MakerUnwindBps))
	} else {
		var ok bool
		if side == market.Buy {
			want, ok = e.book.BestAsk()
		} else {
			want, ok = e.book.BestBid()
		}
		if !ok {
			return false
		}
	}
	price := e.spec.SnapPrice(
		market.ClampPostOnly(want, side, e.book, e.spec, e.cfg.Quote.PostOnlyBufferTicks), side)
	if !price.IsPositive() || e.book.WouldCross(side, price) {
		return false
	}

	id := trading.ClOrdID(ulid.Make().String())
	e.flattenOrders[id] = flattenOrder{at: now, qty: qty}
	err := e.deps.Gateway.Place(ctx, ports.PlaceRequest{
		ClOrdID:     id,
		Symbol:      e.symbol,
		Side:        side,
		Type:        trading.Limit,
		TimeInForce: trading.ALO,
		Price:       price,
		Qty:         qty,
		ReduceOnly:  true,
		MarginMode:  e.risk.MarginMode,
		Leverage:    e.cfg.SymbolRisk.Leverage,
		Priority:    ports.PrioritySafety,
	})
	if err != nil {
		delete(e.flattenOrders, id)
		e.deps.Log.Warn("maker close failed to send, closing at market",
			"symbol", e.symbol, "side", side, "qty", qty, "error", err)
		return false
	}

	deadline := now.Add(e.risk.MakerUnwindTimeout)
	e.makerClose = &makerClose{id: id, side: side, qty: qty, deadline: deadline}
	// Восстановление по позиции ждёт весь срок мейкерского ордера.
	e.flattenRetryAt = deadline
	if until := now.Add(e.risk.PostFillCooldown); until.After(e.cooldownUntil) {
		e.cooldownUntil = until
	}
	e.deps.Log.Info("closing the position as maker",
		"symbol", e.symbol, "side", side, "qty", qty, "price", price,
		"until", deadline.UTC().Format(time.RFC3339Nano), "reason", reason, "cl_ord_id", id)
	return true
}

// submitMarketClose sends one reduce-only IOC close and remembers its identity.
//
// The identifier is stored before the request: its fill report must be
// recognized as a close rather than as a new quote fill. flattenRetryAt is set
// even on a transport error because placement errors can be ambiguous; the
// position-based path waits for reconciliation before risking a duplicate.
func (e *Engine) submitMarketClose(
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
	e.flattenOrders[id] = flattenOrder{at: now, qty: qty}

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
		"thin_book", e.thinSummary(),
		"depth_ask_usd", fmt.Sprintf("%.0f", askDepth),
		"jump", estimate.JumpBps,
		"jump_effective", conditions.JumpBps,
		// Мера риска — большее из быстрого рекорда и ограниченной памяти, и по
		// одному итогу нельзя сказать, какое из двух держит бота вне рынка.
		// Разница решает, что чинить: полураспад рекорда или ограничение памяти.
		// Поля для этого в Estimate заведены с самого начала, а в сводку
		// не попадали — 10 августа сорокаминутный простой пришлось раскладывать
		// по скорости распада значения между строками лога.
		//
		// jump_window с 19 августа — сырой максимум окна, уже не участвующий
		// в решении. Оставлен как база сравнения: по нему видно, сколько именно
		// осторожности отпустил полураспад против прежней плоской полки.
		"jump_fast", estimate.FastBps,
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

// thinSummary — какие стороны сейчас закрыты по глубине стакана.
//
// В сводке нужно потому, что запрет наблюдаем только по отсутствию: сторона,
// которая не котируется из-за тонкого стакана, выглядит в логе ровно так же,
// как сторона, которой просто нечего делать.
func (e *Engine) thinSummary() string {
	switch {
	case e.thinSide[market.Buy] && e.thinSide[market.Sell]:
		return "both"
	case e.thinSide[market.Buy]:
		return "bid"
	case e.thinSide[market.Sell]:
		return "ask"
	default:
		return "no"
	}
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
		// Прочерк только пока не измеряли ни разу. Прежде он стоял всякий раз,
		// когда стакан пуст, и тогда по сводке нельзя было отличить «не знаем»
		// от «знаем, но сейчас нечего мерить» — а размер в этих случаях разный.
		if e.marginRateSeen > 0 {
			return fmt.Sprintf("%.3f%% (last)", e.marginRateSeen)
		}
		return "-"
	}
	return fmt.Sprintf("%.3f%%", rate)
}

// measuredMarginRate — какую долю выставленного нотионала биржа держит
// заблокированной прямо сейчас, в процентах. false, пока мерить нечего.
//
// Второй результат отвечает «есть ли что показать», а не «можно ли этому
// верить». Пригодность к запоминанию решает cleanMeasurement.
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
	rate, _ := e.locked.Div(notional).Float64()
	return rate * 100, true
}

// cleanMeasurement сообщает, что отношение locked к нотионалу сейчас означает
// именно долю блокировки, а не переходное состояние.
//
// Числитель и знаменатель приходят из разных мест: locked — от биржи, нотионал —
// из нашей лесенки. Совпадают они не всегда, и каждое расхождение искажает
// отношение:
//
//   - ступень в полёте (Placing, Cancelling) — биржа маржу уже держит или ещё
//     держит, а мы её нотионал не считаем: доля завышается;
//   - открытая позиция — под неё тоже держится маржа, и она попадает в locked
//     мимо всякой лесенки: доля завышается;
//   - неотправленные снятия — ордера считаются нашими, но уже обречены.
//
// Мгновенное значение от этого страдает несильно и всегда в безопасную сторону.
// Опасно другое: запомнить такое значение и считать по нему размер потом, когда
// картина уже чистая. 15 августа так и вышло — уход на выходные снял котировки,
// последнее измерение снялось в середине съёма и дало 9.572% вместо 2.667%,
// и с ним бот встретил бы понедельник, выставив втрое меньше нужного.
func (e *Engine) cleanMeasurement() bool {
	if !e.position.IsZero() || len(e.pendingWithdraw) > 0 {
		return false
	}
	for _, ladder := range []*quotes.Ladder{e.bids, e.asks} {
		live := false
		for _, slot := range ladder.Slots {
			if slot.Phase.InFlight() {
				return false
			}
			if slot.Phase == quotes.Live {
				live = true
			}
		}
		// Обе стороны обязаны стоять: односторонняя лесенка означает, что мы
		// в середине перестановки, и знаменатель неполон.
		if !live {
			return false
		}
	}
	return true
}

// spendable — сколько биржа готова заблокировать под висящими ордерами.
//
// Свободный остаток плюс уже заблокированное нашими же ордерами. Складывать
// обязательно: Available сам по себе — это остаток ПОСЛЕ того, как мы
// выставились, и бюджет от него сходится примерно на 40% капитала при любой
// утилизации (sizing.Budget.EquityUSD). Locked возвращает в сумму ровно то,
// что Available потерял, поэтому величина не зависит от того, стоим мы сейчас
// в стакане или нет.
//
// Маржа под открытой позицией (Balance.Margin) сюда намеренно не входит: она
// занята, и занята до закрытия позиции.
//
// Ноль означает «снимка баланса ещё не было» и отключает потолок в sizing.
func (e *Engine) spendable() decimal.Decimal {
	if !e.available.IsPositive() && !e.locked.IsPositive() {
		return decimal.Zero
	}
	return e.available.Add(e.locked)
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
// Последнее измерение переживает опустевший стакан.
//
// Мерить можно только по живым ордерам, а стакан пустеет постоянно: 14 августа
// съём по устаревшему mark случался каждые 67 секунд. Откат к настроенному
// значению на это время означал, что размер скачет вчетверо — 6.27 против 24.9
// XAU на сторону — в зависимости от того, стояли ли ордера в момент пересчёта.
// И скачет в худшую сторону: заново лесенка собирается именно тогда, когда
// мерить нечем, то есть по заниженному размеру.
//
// Доля блокировки — свойство счёта и плеча. Она не меняется оттого, что мы сняли
// котировки, и забывать её на каждый съём значит путать «величина не измерима
// прямо сейчас» с «величина стала другой».
//
// Настроенное значение остаётся только на старте, до первого в жизни процесса
// измерения. Там выбора нет, и консервативный перекос там уместен.
func (e *Engine) marginRate() (float64, string) {
	configured := e.cfg.SymbolRisk.InitialMarginPct

	floor := configured
	if lev := e.cfg.SymbolRisk.Leverage; lev >= 1 {
		floor = 100 / float64(lev)
	}

	measured, ok := e.measuredMarginRate()
	if ok && e.cleanMeasurement() {
		e.marginRateSeen, e.marginClamped = max(measured, floor), measured < floor
	}

	// Зажим называется всегда, даже когда значение уже запомнено: он означает,
	// что биржа сообщила долю ниже физически возможной, и молча продолжать
	// считать по ней размер — ровно тот молчаливый отказ, который эта серия
	// правок и разбирает.
	clamped := ""
	if e.marginClamped {
		clamped = ", clamped to the leverage floor"
	}

	switch {
	case e.marginRateSeen > 0 && ok:
		return e.marginRateSeen, "measured" + clamped
	case e.marginRateSeen > 0:
		return e.marginRateSeen, "last measured" + clamped
	case ok && measured < floor:
		return floor, "measured, clamped to the leverage floor"
	case ok:
		// Есть что показать, но картина переходная: считаем по этому значению,
		// запоминать не станем. Ошибка здесь всегда в сторону меньшего размера.
		return measured, "measured, not yet clean"
	default:
		return configured, "configured"
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
