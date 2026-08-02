package rest

import (
	"context"
	"fmt"
	"net/http"

	"github.com/nd/mm-standx/internal/adapters/standx/dto"
	"github.com/nd/mm-standx/internal/domain/trading"
	"github.com/nd/mm-standx/internal/ports"
)

// Client реализует ports.OrderGateway.
//
// REST-путь используется как запасной: горячий путь размещения и отмены идёт
// через WebSocket (adapters/standx/wsapi), где нет накладных расходов на
// установку соединения.
var _ ports.OrderGateway = (*Client)(nil)

// Place отправляет заявку на размещение ордера.
//
// Возврат nil означает, что биржа приняла заявку, но не что ордер оказался
// в стакане: ALO может быть отклонён на этапе матчинга. Итог придёт событием
// OrderUpdated.
func (c *Client) Place(ctx context.Context, req ports.PlaceRequest) error {
	body := dto.NewOrderRequest{
		Symbol:      req.Symbol.String(),
		Side:        req.Side.String(),
		OrderType:   string(req.Type),
		Qty:         req.Qty.String(),
		TimeInForce: string(req.TimeInForce),
		ReduceOnly:  req.ReduceOnly,
		ClOrdID:     req.ClOrdID.String(),
		MarginMode:  string(req.MarginMode),
		Leverage:    req.Leverage,
	}
	if req.Type == trading.Limit {
		body.Price = req.Price.String()
	}

	var envelope dto.Envelope
	err := c.do(ctx, request{
		method:   http.MethodPost,
		path:     "/api/new_order",
		body:     body,
		auth:     true,
		signed:   true,
		priority: req.Priority,
	}, &envelope)
	if err != nil {
		return fmt.Errorf("place order %s: %w", req.ClOrdID, err)
	}
	if envelope.Code != 0 {
		return fmt.Errorf("place order %s: биржа отклонила заявку: code %d: %s",
			req.ClOrdID, envelope.Code, envelope.Message)
	}
	return nil
}

// Cancel отменяет один ордер по клиентскому идентификатору.
func (c *Client) Cancel(ctx context.Context, id trading.ClOrdID, p ports.Priority) error {
	var envelope dto.Envelope
	err := c.do(ctx, request{
		method:   http.MethodPost,
		path:     "/api/cancel_order",
		body:     dto.CancelOrderRequest{ClOrdID: id.String()},
		auth:     true,
		signed:   true,
		priority: p,
	}, &envelope)
	if err != nil {
		return fmt.Errorf("cancel order %s: %w", id, err)
	}
	if envelope.Code != 0 {
		return fmt.Errorf("cancel order %s: code %d: %s", id, envelope.Code, envelope.Message)
	}
	return nil
}

// CancelBatch отменяет пачку ордеров одним запросом.
//
// Это основной инструмент аварийного съёма стороны: один запрос вместо N штук
// экономит бюджет шейпера в момент, когда он нужнее всего.
func (c *Client) CancelBatch(ctx context.Context, ids []trading.ClOrdID, p ports.Priority) error {
	if len(ids) == 0 {
		return nil
	}

	list := make([]string, 0, len(ids))
	for _, id := range ids {
		list = append(list, id.String())
	}

	var envelope dto.Envelope
	err := c.do(ctx, request{
		method:   http.MethodPost,
		path:     "/api/cancel_orders",
		body:     dto.CancelOrdersRequest{ClOrdIDList: list},
		auth:     true,
		signed:   true,
		priority: p,
	}, &envelope)
	if err != nil {
		return fmt.Errorf("cancel %d orders: %w", len(ids), err)
	}
	if envelope.Code != 0 {
		return fmt.Errorf("cancel %d orders: code %d: %s", len(ids), envelope.Code, envelope.Message)
	}
	return nil
}
