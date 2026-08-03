// Package dryrun содержит декоратор шлюза ордеров, не отправляющий команды на биржу.
package dryrun

import (
	"context"
	"log/slog"

	"github.com/nd/mm-standx/internal/domain/trading"
	"github.com/nd/mm-standx/internal/events"
	"github.com/nd/mm-standx/internal/ports"
)

// Gateway перехватывает торговые команды и подтверждает их локально.
//
// Реализован декоратором, а не проверкой режима внутри клиента: так путь
// отправки настоящего ордера физически отсутствует в графе зависимостей,
// пока режим не переключён в live. Забыть проверку невозможно — её просто нет.
//
// Заглушка воспроизводит жизненный цикл собственных ордеров: без подтверждений
// ступени лесенки зависали бы до таймаута, и теневой прогон мерил бы не аптайм
// стратегии, а частоту таймаутов. Это моделирование нашей стороны, а не рынка:
// исполнения не выдумываются, потому что достоверно смоделировать их нельзя.
type Gateway struct {
	log   *slog.Logger
	bus   *events.Bus
	clock ports.Clock

	// live хранит выставленные ордера, чтобы отмена относилась к чему-то
	// существующему, как и на бирже.
	live map[trading.ClOrdID]trading.Order
}

// New создаёт заглушку шлюза.
func New(log *slog.Logger, bus *events.Bus, clock ports.Clock) *Gateway {
	return &Gateway{
		log:   log.With("component", "dryrun"),
		bus:   bus,
		clock: clock,
		live:  make(map[trading.ClOrdID]trading.Order),
	}
}

func (g *Gateway) Place(ctx context.Context, req ports.PlaceRequest) error {
	g.log.Info("order not sent (dry_run)",
		"cl_ord_id", req.ClOrdID,
		"symbol", req.Symbol,
		"side", req.Side,
		"type", req.Type,
		"tif", req.TimeInForce,
		"price", req.Price,
		"qty", req.Qty,
		"reduce_only", req.ReduceOnly,
	)

	// Рыночный ордер на бирже не отдыхает в стакане, поэтому и здесь он
	// сразу считается завершённым, но без выдуманного исполнения.
	if req.Type == trading.Market {
		return nil
	}

	now := g.clock.Now()
	order := trading.Order{
		ClOrdID:     req.ClOrdID,
		Symbol:      req.Symbol,
		Side:        req.Side,
		Type:        req.Type,
		TimeInForce: req.TimeInForce,
		Status:      trading.StatusOpen,
		Price:       req.Price,
		Qty:         req.Qty,
		ReduceOnly:  req.ReduceOnly,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	g.live[req.ClOrdID] = order
	events.Publish(g.bus, ctx, events.OrderUpdated{Order: order})
	return nil
}

func (g *Gateway) Cancel(ctx context.Context, id trading.ClOrdID, p ports.Priority) error {
	g.log.Info("cancel not sent (dry_run)", "cl_ord_id", id, "priority", p)
	g.confirmCancel(ctx, id)
	return nil
}

func (g *Gateway) CancelBatch(ctx context.Context, ids []trading.ClOrdID, p ports.Priority) error {
	g.log.Info("batch cancel not sent (dry_run)", "count", len(ids), "priority", p)
	for _, id := range ids {
		g.confirmCancel(ctx, id)
	}
	return nil
}

func (g *Gateway) confirmCancel(ctx context.Context, id trading.ClOrdID) {
	order, ok := g.live[id]
	if !ok {
		return
	}
	delete(g.live, id)

	order.Status = trading.StatusCanceled
	order.UpdatedAt = g.clock.Now()
	events.Publish(g.bus, ctx, events.OrderUpdated{Order: order})
}

var _ ports.OrderGateway = (*Gateway)(nil)
