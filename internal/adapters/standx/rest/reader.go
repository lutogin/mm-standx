package rest

import (
	"context"
	"fmt"
	"net/http"
	"net/url"

	"github.com/nd/mm-standx/internal/adapters/standx/dto"
	"github.com/nd/mm-standx/internal/adapters/standx/mapper"
	"github.com/nd/mm-standx/internal/domain/market"
	"github.com/nd/mm-standx/internal/domain/trading"
	"github.com/nd/mm-standx/internal/ports"
)

// Client реализует ports.ExchangeReader.
var _ ports.ExchangeReader = (*Client)(nil)

// Symbols возвращает список торгуемых инструментов.
//
// Источник — сводка рынков, а не перечисление из справочника API: последнее
// устарело и содержит меньше половины реально доступных пар.
func (c *Client) Symbols(ctx context.Context) ([]market.Symbol, error) {
	overview, err := c.MarketOverview(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]market.Symbol, 0, len(overview.Symbols))
	for _, s := range overview.Symbols {
		out = append(out, market.Symbol(s.Symbol))
	}
	return out, nil
}

// MarketOverview возвращает сводку по всем рынкам.
func (c *Client) MarketOverview(ctx context.Context) (dto.MarketOverview, error) {
	var out dto.MarketOverview
	err := c.do(ctx, request{
		method:   http.MethodGet,
		path:     "/api/query_market_overview",
		priority: ports.PriorityBackground,
	}, &out)
	if err != nil {
		return dto.MarketOverview{}, fmt.Errorf("market overview: %w", err)
	}
	return out, nil
}

// SymbolSpec возвращает торговую спецификацию инструмента.
func (c *Client) SymbolSpec(ctx context.Context, s market.Symbol) (market.SymbolSpec, error) {
	var out []dto.SymbolInfo
	err := c.do(ctx, request{
		method:   http.MethodGet,
		path:     "/api/query_symbol_info",
		query:    url.Values{"symbol": {s.String()}},
		priority: ports.PriorityBackground,
	}, &out)
	if err != nil {
		return market.SymbolSpec{}, fmt.Errorf("symbol info %s: %w", s, err)
	}
	if len(out) == 0 {
		return market.SymbolSpec{}, fmt.Errorf("symbol info %s: exchange returned an empty response", s)
	}
	return mapper.SymbolSpec(out[0])
}

// SymbolPrice возвращает котировочный снимок инструмента.
func (c *Client) SymbolPrice(ctx context.Context, s market.Symbol) (market.MarkPrice, error) {
	var out dto.SymbolPrice
	err := c.do(ctx, request{
		method:   http.MethodGet,
		path:     "/api/query_symbol_price",
		query:    url.Values{"symbol": {s.String()}},
		priority: ports.PriorityBackground,
	}, &out)
	if err != nil {
		return market.MarkPrice{}, fmt.Errorf("symbol price %s: %w", s, err)
	}
	return mapper.MarkPrice(out, c.clock.Now())
}

// DepthBook возвращает снимок стакана.
func (c *Client) DepthBook(ctx context.Context, s market.Symbol) (market.Book, error) {
	var out dto.DepthBook
	err := c.do(ctx, request{
		method:   http.MethodGet,
		path:     "/api/query_depth_book",
		query:    url.Values{"symbol": {s.String()}},
		priority: ports.PriorityBackground,
	}, &out)
	if err != nil {
		return market.Book{}, fmt.Errorf("depth book %s: %w", s, err)
	}
	return mapper.Book(out, c.clock.Now())
}

// OpenOrders возвращает все висящие ордера по инструменту. Основа сверки.
func (c *Client) OpenOrders(ctx context.Context, s market.Symbol) ([]trading.Order, error) {
	var page dto.Page[dto.Order]
	err := c.do(ctx, request{
		method:   http.MethodGet,
		path:     "/api/query_open_orders",
		query:    url.Values{"symbol": {s.String()}, "limit": {"1200"}},
		auth:     true,
		priority: ports.PriorityBackground,
	}, &page)
	if err != nil {
		return nil, fmt.Errorf("open orders %s: %w", s, err)
	}

	out := make([]trading.Order, 0, len(page.Result))
	for _, d := range page.Result {
		o, err := mapper.Order(d)
		if err != nil {
			return nil, fmt.Errorf("open orders %s: %w", s, err)
		}
		out = append(out, o)
	}
	return out, nil
}

// Positions возвращает открытые позиции по инструменту.
func (c *Client) Positions(ctx context.Context, s market.Symbol) ([]trading.Position, error) {
	var out []dto.Position
	err := c.do(ctx, request{
		method:   http.MethodGet,
		path:     "/api/query_positions",
		query:    url.Values{"symbol": {s.String()}},
		auth:     true,
		priority: ports.PriorityBackground,
	}, &out)
	if err != nil {
		return nil, fmt.Errorf("positions %s: %w", s, err)
	}

	positions := make([]trading.Position, 0, len(out))
	for _, d := range out {
		p, err := mapper.Position(d)
		if err != nil {
			return nil, fmt.Errorf("positions %s: %w", s, err)
		}
		positions = append(positions, p)
	}
	return positions, nil
}

// Balance возвращает сводку средств аккаунта.
func (c *Client) Balance(ctx context.Context) (trading.Balance, error) {
	var out dto.Balance
	err := c.do(ctx, request{
		method:   http.MethodGet,
		path:     "/api/query_balance",
		auth:     true,
		priority: ports.PriorityBackground,
	}, &out)
	if err != nil {
		return trading.Balance{}, fmt.Errorf("balance: %w", err)
	}
	return mapper.Balance(out), nil
}
