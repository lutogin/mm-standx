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
	"github.com/nd/mm-standx/internal/ports"
)

// evaluate — единственное место, где принимается решение о котировках.
//
// allowPlace различает два источника вызова: такт mark price несёт официальную
// цену и разрешает выставление, обновление стакана — только снятие.
func (e *Engine) evaluate(ctx context.Context, allowPlace bool) {
	if e.halted || !e.mark.IsPositive() {
		return
	}
	now := e.deps.Clock.Now()

	// Незакрытая позиция важнее награды: котировать, имея экспозицию, значит
	// удвоить её при следующем заливе.
	if e.flattenInFlight || !e.position.IsZero() || e.pendingFill.IsPositive() {
		e.withdraw(ctx, "position is open")
		return
	}
	if now.Before(e.cooldownUntil) {
		return
	}

	halfSpread, ok := e.halfSpreadBps()
	if !ok {
		e.withdraw(ctx, "order book is not readable")
		return
	}

	estimate := e.vol.Estimate(now)
	conditions := policy.Conditions{
		HalfSpreadBps: halfSpread,
		JumpBps:       estimate.EffectiveJump(),
		Ready:         estimate.Ready,
	}

	appetite := e.appetite
	switch hour := e.hour.Assess(now); hour.Mode {
	case uptime.Lost:
		// Час недостижим: дальнейшее присутствие оплачивается исполнениями,
		// а зачёта не приносит.
		e.withdraw(ctx, "hour unreachable: "+hour.Reason)
		return
	case uptime.Banked:
		// Цель взята, дополнительный аптайм ничего не добавляет — отходим.
		appetite.JumpMultiple *= bankedCaution
	}

	quote := e.stickyTarget(appetite.Target(conditions, e.bounds))
	e.reportStatus(now, conditions, estimate, quote)

	if !quote.Feasible {
		e.withdraw(ctx, quote.Reason)
		return
	}
	// Темп перестановки ограничен независимо от частоты событий: стакан
	// приходит несколько раз в секунду, и реагировать на каждое обновление
	// значило бы исчерпать бюджет запросов за минуту.
	if now.Sub(e.lastAction) < e.cfg.Quote.RequoteCooldown {
		return
	}

	thresholds := appetite.Thresholds(quote, conditions, e.bounds)
	distances := e.ladder.Distances(quote.Distance)

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
		e.withdraw(ctx, e.blocked)
		return
	}

	for _, side := range []market.Side{market.Buy, market.Sell} {
		ladder := e.ladderFor(side)
		actions := ladder.Plan(now, distances, e.classifier(side, thresholds), e.rules)
		if len(actions) > 0 {
			e.lastAction = now
		}
		e.execute(ctx, side, actions, allowPlace, plan.Rungs)
	}
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
				continue
			}
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
		return
	}
	// Логируется и успех: в боевом режиме заглушка молчит, и без этой записи
	// по логу невозможно отличить работающего бота от бездействующего.
	e.deps.Log.Info("quote placed",
		"symbol", e.symbol, "side", side, "slot", action.Slot,
		"price", price, "qty", qty, "distance", action.Distance, "reason", action.Reason)

	e.ladderFor(side).OnPlaceSent(action.Slot, id, price, qty, now)
}

// withdraw снимает обе стороны.
func (e *Engine) withdraw(ctx context.Context, reason string) {
	var ids []trading.ClOrdID
	now := e.deps.Clock.Now()

	for _, ladder := range []*quotes.Ladder{e.bids, e.asks} {
		actions := ladder.PullAll(reason)
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

	e.deps.Log.Info("withdrawing quotes", "symbol", e.symbol, "count", len(ids), "reason", reason)
	// Пакетная отмена стоит одного кредита вместо N — это важно именно
	// в момент, когда бюджет запросов нужнее всего.
	if err := e.deps.Gateway.CancelBatch(ctx, ids, ports.PrioritySafety); err != nil {
		e.deps.Log.Error("withdrawal failed", "symbol", e.symbol, "error", err)
	}
}

// onFill накапливает исполнение и назначает срок закрытия.
//
// Дебаунс существует потому, что свип даёт десятки частичных исполнений подряд.
// Закрывать каждое отдельным маркет-ордером значило бы платить тейкерскую
// комиссию столько же раз.
func (e *Engine) onFill(ctx context.Context, order trading.Order) {
	now := e.deps.Clock.Now()

	e.pendingFill = e.pendingFill.Add(order.FilledQty)
	if e.fillDeadline.IsZero() || e.fillDeadline.Before(now) {
		e.fillDeadline = now.Add(e.risk.FlattenDebounce)
	}

	e.deps.Log.Warn("order filled",
		"symbol", e.symbol, "side", order.Side,
		"qty", order.FilledQty, "price", order.AvgFillPrice)

	e.withdraw(ctx, "filled, closing position")
}

// flatten закрывает позицию рыночным ордером.
func (e *Engine) flatten(ctx context.Context) {
	if e.flattenInFlight {
		return
	}
	position := e.position
	if position.IsZero() {
		if e.pendingFill.IsPositive() {
			// Исполнение уже зафиксировано, но PositionUpdated ещё не пришёл.
			// Следующий тик или событие позиции повторит попытку.
			return
		}
		return
	}

	e.flattenInFlight = true
	defer func() { e.flattenInFlight = false }()

	side := market.Sell
	if position.IsNegative() {
		side = market.Buy
	}

	err := e.deps.Gateway.Place(ctx, ports.PlaceRequest{
		ClOrdID:     trading.ClOrdID(ulid.Make().String()),
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
		e.deps.Log.Error("flatten failed", "symbol", e.symbol, "qty", position, "error", err)
		return
	}

	e.pendingFill = money.MustParseQty("0")
	e.fillDeadline = time.Time{}
	e.cooldownUntil = e.deps.Clock.Now().Add(e.risk.PostFillCooldown)
	e.deps.Log.Info("position closed", "symbol", e.symbol, "qty", position)
}

// statusInterval — период сводки о состоянии актора.
const statusInterval = 15 * time.Second

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
	attrs := []any{
		"symbol", e.symbol,
		"mark", e.mark,
		"half_spread", conditions.HalfSpreadBps,
		"jump", estimate.JumpBps,
		"jump_effective", conditions.JumpBps,
		"samples", estimate.Samples,
		"confidence", fmt.Sprintf("%.0f%%", estimate.Confidence*100),
		"hour_mode", hour.Mode,
		"uptime_pct", fmt.Sprintf("%.0f", hour.QualifiedPct),
		"maker_hours", fmt.Sprintf("%.4f", hour.MakerHours),
		"bids_live", e.bids.LiveCount(),
		"asks_live", e.asks.LiveCount(),
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

// priorityOf отображает мотив решения в приоритет шейпера запросов.
func priorityOf(u policy.Urgency) ports.Priority {
	if u == policy.UrgencySafety {
		return ports.PrioritySafety
	}
	return ports.PriorityQuote
}
