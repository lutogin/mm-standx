package engine

import (
	"context"

	"github.com/nd/mm-standx/internal/domain/quotes"
	"github.com/nd/mm-standx/internal/domain/trading"
	"github.com/nd/mm-standx/internal/events"
	"github.com/nd/mm-standx/internal/ports"
)

// reconcile сверяет локальное представление о лесенке со снимком биржи.
//
// Расхождение неизбежно и не является признаком поломки: команда могла
// потеряться, событие — не дойти, а процесс — перезапуститься, оставив ордера
// в стакане. Опасно не расхождение само по себе, а то, что оно накапливается:
// ордер, о котором мы забыли, продолжает исполняться.
//
// Различаются два случая, и они требуют противоположных действий:
//
//   - осиротевший ордер есть на бирже, но не в нашей лесенке — его нужно снять,
//     потому что управлять им мы уже не можем;
//   - призрачный числится живым у нас, но на бирже отсутствует — нужно
//     освободить ступень, иначе она останется занятой навсегда.
func (e *Engine) reconcile(ctx context.Context, snapshot events.OpenOrdersSnapshot) {
	if snapshot.Symbol != e.symbol {
		return
	}
	now := e.deps.Clock.Now()

	onExchange := make(map[trading.ClOrdID]trading.Order, len(snapshot.Orders))
	for _, order := range snapshot.Orders {
		onExchange[order.ClOrdID] = order
	}

	// Очередь повторного съёма чистится по ответу биржи, а не по нашим
	// ожиданиям: команда могла дойти, а подтверждение — потеряться. Без этого
	// повторы уходили бы в пустоту, расходуя бюджет запросов.
	e.pruneWithdrawals(onExchange)

	known := make(map[trading.ClOrdID]bool)
	for _, ladder := range []*quotes.Ladder{e.bids, e.asks} {
		for i := range ladder.Slots {
			slot := ladder.Slots[i]
			if slot.ClOrdID == "" {
				continue
			}
			known[slot.ClOrdID] = true

			// Ступень, отправленная в полёт, ещё не обязана быть видна бирже:
			// подтверждение занимает около 400 мс, и торопить его нельзя.
			if slot.Phase != quotes.Live {
				continue
			}
			// Ордер, появившийся после снятия снимка, не мог в него попасть.
			// Без этой проверки сверка объявляет его призраком и освобождает
			// ступень — а ордер продолжает стоять в стакане, и следующее
			// размещение даёт вторую котировку вместо одной. Снимок читается
			// не мгновенно: между чтением у биржи и разбором в акторе проходит
			// и сеть, и очередь событий.
			if slot.Since.After(snapshot.At) {
				continue
			}
			if _, present := onExchange[slot.ClOrdID]; !present {
				e.deps.Log.Warn("ghost order: live locally but absent on the exchange",
					"symbol", e.symbol, "cl_ord_id", slot.ClOrdID)
				ladder.OnOrderUpdate(trading.Order{
					ClOrdID: slot.ClOrdID,
					Status:  trading.StatusCanceled,
				}, now)
			}
		}
	}

	// Сирота объявляется только со второго снимка подряд.
	//
	// У призрака выше защита от гонки есть — ордер моложе снимка пропускается.
	// У сироты симметричной проверки не было, и она нужна по той же причине,
	// только с другой стороны: снимок берётся у биржи, а разбирается в акторе
	// спустя сеть и очередь событий. Ордер, стоявший в лесенке в момент снимка
	// и успевший исполниться или сняться до разбора, из лесенки уже ушёл —
	// и объявлялся сиротой. Отмена уходила на биржу и получала «order is not
	// open»: 209 таких эпизодов и 366 ошибок за 14 августа.
	//
	// Спросить «мог ли ордер уйти из лесенки после снимка» нечем: момента ухода
	// мы не храним. Зато следующий снимок отвечает на это сам — терминальный
	// ордер в нём просто не появится. Поэтому условие не по времени, а по
	// повторению: неизвестен дважды подряд — значит и правда ничей.
	//
	// Настоящая сирота остаётся от прошлого запуска и никуда не денется, ей
	// лишний интервал сверки ничего не стоит. Отмена идёт с приоритетом
	// безопасности, то есть из резерва бакета, отложенного под аварийный съём:
	// тратить его на призраков — прямо противоположно смыслу резерва.
	unknown := make(map[trading.ClOrdID]bool, len(onExchange))
	var orphans []trading.ClOrdID
	for id := range onExchange {
		if known[id] {
			continue
		}
		unknown[id] = true
		if e.orphanSeen[id] {
			orphans = append(orphans, id)
		}
	}
	e.orphanSeen = unknown

	if len(orphans) == 0 {
		return
	}

	// Осиротевшие ордера чаще всего остались от предыдущего запуска. Оставить
	// их значило бы держать в стакане котировки, за которыми никто не следит.
	e.deps.Log.Warn("orphan orders on the exchange, cancelling",
		"symbol", e.symbol, "count", len(orphans))

	if err := e.deps.Gateway.CancelBatch(ctx, orphans, ports.PrioritySafety); err != nil {
		e.deps.Log.Error("failed to cancel orphan orders", "symbol", e.symbol, "error", err)
		e.deps.Metrics.OrderFailed(e.symbol, "cancel_batch")
	}
}

// pruneWithdrawals оставляет в очереди повтора только то, что биржа
// действительно всё ещё держит в стакане.
func (e *Engine) pruneWithdrawals(onExchange map[trading.ClOrdID]trading.Order) {
	if len(e.pendingWithdraw) == 0 {
		return
	}
	var settled []trading.ClOrdID
	for _, id := range e.pendingWithdraw {
		if _, present := onExchange[id]; !present {
			settled = append(settled, id)
		}
	}
	e.forgetWithdrawal(settled)
}
