// Package dryrun содержит декоратор шлюза ордеров, не отправляющий команды на биржу.
package dryrun

import (
	"context"
	"log/slog"
	"sync"

	"github.com/nd/mm-standx/internal/domain/market"
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

	// mu защищает live: акторов столько же, сколько инструментов, и сверка
	// читает список из своей горутины. На бирже эту роль играет сама биржа.
	mu sync.Mutex
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
	g.mu.Lock()
	g.live[req.ClOrdID] = order
	g.mu.Unlock()

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
	g.mu.Lock()
	order, ok := g.live[id]
	delete(g.live, id)
	g.mu.Unlock()

	if !ok {
		return
	}
	order.Status = trading.StatusCanceled
	order.UpdatedAt = g.clock.Now()
	events.Publish(g.bus, ctx, events.OrderUpdated{Order: order})
}

// OpenOrders возвращает ордера, которые заглушка считает стоящими в стакане.
//
// Без этого метода сверка сравнивала бы теневую лесенку с настоящей биржей,
// где никаких наших ордеров нет по определению. Каждая ступень оказывалась бы
// призрачной, освобождалась и выставлялась заново — теневой прогон мерил бы
// частоту собственной ошибки вместо поведения стратегии.
func (g *Gateway) OpenOrders(symbol market.Symbol) []trading.Order {
	g.mu.Lock()
	defer g.mu.Unlock()

	out := make([]trading.Order, 0, len(g.live))
	for _, order := range g.live {
		if order.Symbol == symbol {
			out = append(out, order)
		}
	}
	return out
}

var _ ports.OrderGateway = (*Gateway)(nil)
