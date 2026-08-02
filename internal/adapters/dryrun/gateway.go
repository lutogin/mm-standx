// Package dryrun содержит декоратор шлюза ордеров, не отправляющий команды на биржу.
package dryrun

import (
	"context"
	"log/slog"

	"github.com/nd/mm-standx/internal/domain/trading"
	"github.com/nd/mm-standx/internal/ports"
)

// Gateway перехватывает торговые команды и только логирует их.
//
// Реализован декоратором, а не проверкой режима внутри клиента: так путь
// отправки настоящего ордера физически отсутствует в графе зависимостей,
// пока режим не переключён в live. Забыть проверку невозможно — её просто нет.
type Gateway struct {
	log *slog.Logger
}

// New создаёт заглушку шлюза.
func New(log *slog.Logger) *Gateway {
	return &Gateway{log: log.With("component", "dryrun")}
}

func (g *Gateway) Place(_ context.Context, req ports.PlaceRequest) error {
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
	return nil
}

func (g *Gateway) Cancel(_ context.Context, id trading.ClOrdID, p ports.Priority) error {
	g.log.Info("cancel not sent (dry_run)", "cl_ord_id", id, "priority", p)
	return nil
}

func (g *Gateway) CancelBatch(_ context.Context, ids []trading.ClOrdID, p ports.Priority) error {
	g.log.Info("batch cancel not sent (dry_run)", "count", len(ids), "priority", p)
	return nil
}

var _ ports.OrderGateway = (*Gateway)(nil)
