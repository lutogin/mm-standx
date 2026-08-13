package rest

import (
	"context"
	"fmt"
	"net/http"
	"net/url"

	"github.com/nd/mm-standx/internal/adapters/standx/dto"
	"github.com/nd/mm-standx/internal/domain/market"
	"github.com/nd/mm-standx/internal/domain/trading"
	"github.com/nd/mm-standx/internal/ports"
)

// Плечо и режим маржи — настройка счёта, а не свойство ордера.
//
// Поле leverage в new_order биржа принимает и на блокировку маржи под висящей
// заявкой не смотрит: проба 12 августа заблокировала одинаковые 10.542%
// нотионала при запрошенных 4, 8 и 20 (PLAN.md §12.5). Действует именно
// настройка счёта, а мы её никогда не выставляли — отсюда эффективное плечо
// 9.49, значение биржи по умолчанию, и втрое меньший размер на те же деньги.

// PositionConfig возвращает действующие плечо и режим маржи инструмента.
func (c *Client) PositionConfig(ctx context.Context, s market.Symbol) (trading.PositionConfig, error) {
	var out []dto.PositionConfig
	err := c.do(ctx, request{
		method:   http.MethodGet,
		path:     "/api/query_position_config",
		query:    url.Values{"symbol": {s.String()}},
		auth:     true,
		priority: ports.PriorityBackground,
	}, &out)
	if err != nil {
		return trading.PositionConfig{}, fmt.Errorf("position config %s: %w", s, err)
	}
	for _, cfg := range out {
		if cfg.Symbol == s.String() {
			return trading.PositionConfig{
				Symbol:     s,
				Leverage:   cfg.Leverage,
				MarginMode: trading.MarginMode(cfg.MarginMode),
			}, nil
		}
	}
	return trading.PositionConfig{}, fmt.Errorf("position config %s: not in the response", s)
}

// SetLeverage меняет плечо счёта по инструменту.
//
// Успешный ответ означает приём запроса, а не применение настройки, поэтому
// вызывающий обязан перечитать PositionConfig. Разница здесь не теоретическая:
// именно доверие ответу оставило бы нас с прежним плечом и втрое меньшим
// размером, не показав ничего в логе.
func (c *Client) SetLeverage(ctx context.Context, s market.Symbol, leverage int) error {
	if leverage < 1 {
		return fmt.Errorf("set leverage %s: must be at least 1, got %d", s, leverage)
	}

	var envelope dto.Envelope
	err := c.do(ctx, request{
		method:   http.MethodPost,
		path:     "/api/change_leverage",
		body:     dto.ChangeLeverageRequest{Symbol: s.String(), Leverage: leverage},
		auth:     true,
		signed:   true,
		priority: ports.PriorityBackground,
	}, &envelope)
	if err != nil {
		return fmt.Errorf("set leverage %s to %d: %w", s, leverage, err)
	}
	if envelope.Code != 0 {
		return fmt.Errorf("set leverage %s to %d: exchange rejected the request: code %d: %s",
			s, leverage, envelope.Code, envelope.Message)
	}
	return nil
}

var _ ports.PositionConfigurer = (*Client)(nil)
