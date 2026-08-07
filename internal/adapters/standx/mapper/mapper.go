// Package mapper переводит проводные структуры StandX в доменные типы.
//
// Это единственное место, где допустим разбор строк в числа и дат: выше по стеку
// работают уже типизированные значения.
package mapper

import (
	"fmt"
	"strconv"
	"time"

	"github.com/shopspring/decimal"

	"github.com/nd/mm-standx/internal/adapters/standx/dto"
	"github.com/nd/mm-standx/internal/domain/market"
	"github.com/nd/mm-standx/internal/domain/money"
	"github.com/nd/mm-standx/internal/domain/trading"
)

// Order переводит ордер в доменное представление.
func Order(d dto.Order) (trading.Order, error) {
	price, err := optPrice(d.Price)
	if err != nil {
		return trading.Order{}, fmt.Errorf("order %d: price: %w", d.ID, err)
	}
	qty, err := optQty(d.Qty)
	if err != nil {
		return trading.Order{}, fmt.Errorf("order %d: qty: %w", d.ID, err)
	}
	fillQty, err := optQty(d.FillQty)
	if err != nil {
		return trading.Order{}, fmt.Errorf("order %d: fill_qty: %w", d.ID, err)
	}
	fillPrice, err := optPrice(d.FillAvgPrice)
	if err != nil {
		return trading.Order{}, fmt.Errorf("order %d: fill_avg_price: %w", d.ID, err)
	}

	return trading.Order{
		ID:           trading.OrderID(d.ID),
		ClOrdID:      trading.ClOrdID(d.ClOrdID),
		Symbol:       market.Symbol(d.Symbol),
		Side:         market.Side(d.Side),
		Type:         trading.OrderType(d.OrderType),
		TimeInForce:  trading.TimeInForce(d.TimeInForce),
		Status:       trading.OrderStatus(d.Status),
		Price:        price,
		Qty:          qty,
		FilledQty:    fillQty,
		AvgFillPrice: fillPrice,
		ReduceOnly:   d.ReduceOnly,
		CreatedAt:    optTime(d.CreatedAt),
		UpdatedAt:    optTime(d.UpdatedAt),
	}, nil
}

// Position переводит позицию в доменное представление.
//
// Знак qty считается индикатором направления: положительное — лонг, отрицательное —
// шорт. Отдельного поля side биржа не отдаёт. Это предположение обязано быть
// подтверждено на реальном шорте в фазе живого дымового теста, поскольку от него
// зависит сторона закрывающего ордера.
func Position(d dto.Position) (trading.Position, error) {
	qty, err := optQty(d.Qty)
	if err != nil {
		return trading.Position{}, fmt.Errorf("position %s: qty: %w", d.Symbol, err)
	}
	entry, err := optPrice(d.EntryPrice)
	if err != nil {
		return trading.Position{}, fmt.Errorf("position %s: entry_price: %w", d.Symbol, err)
	}
	mark, err := optPrice(d.MarkPrice)
	if err != nil {
		return trading.Position{}, fmt.Errorf("position %s: mark_price: %w", d.Symbol, err)
	}
	liq, err := optPrice(d.LiqPrice)
	if err != nil {
		return trading.Position{}, fmt.Errorf("position %s: liq_price: %w", d.Symbol, err)
	}

	return trading.Position{
		Symbol:     market.Symbol(d.Symbol),
		Qty:        qty,
		EntryPrice: entry,
		MarkPrice:  mark,
		LiqPrice:   liq,
		Leverage:   optInt(d.Leverage),
		MarginMode: trading.MarginMode(d.MarginMode),
		Unrealized: optDecimal(d.Unrealized),
		Realized:   optDecimal(d.Realized),
		UpdatedAt:  optTime(d.UpdatedAt),
	}, nil
}

// Balance переводит сводку средств.
func Balance(d dto.Balance) trading.Balance {
	return trading.Balance{
		Equity:    optDecimal(d.Equity),
		Available: optDecimal(d.CrossAvailable),
		Locked:    optDecimal(d.Locked),
		Margin:    optDecimal(d.CrossMargin),
	}
}

// WalletBalance переводит сообщение канала balance.
//
// Канал отдаёт учёт кошелька, а не сводку счёта: equity и unrealized PnL в нём
// отсутствуют. Total берётся как приближение equity, точное значение приходит
// из query_balance при сверке.
func WalletBalance(d dto.WalletBalance) trading.Balance {
	return trading.Balance{
		Equity:    optDecimal(d.Total),
		Available: optDecimal(d.Free),
		Locked:    optDecimal(d.Locked),
		Margin:    optDecimal(d.Occupied),
	}
}

// Trade переводит исполнение.
func Trade(d dto.Trade) (trading.Trade, error) {
	price, err := optPrice(d.Price)
	if err != nil {
		return trading.Trade{}, fmt.Errorf("trade %d: price: %w", d.ID, err)
	}
	qty, err := optQty(d.Qty)
	if err != nil {
		return trading.Trade{}, fmt.Errorf("trade %d: qty: %w", d.ID, err)
	}
	return trading.Trade{
		ID:       d.ID,
		OrderID:  trading.OrderID(d.OrderID),
		Symbol:   market.Symbol(d.Symbol),
		Side:     market.Side(d.Side),
		Price:    price,
		Qty:      qty,
		Fee:      optDecimal(d.FeeValue()),
		FeeAsset: d.FeeAsset,
		PnL:      optDecimal(d.PnLValue()),
		Time:     optTime(d.Time),
	}, nil
}

// SymbolSpec переводит торговую спецификацию инструмента.
func SymbolSpec(d dto.SymbolInfo) (market.SymbolSpec, error) {
	minQty, err := optQty(d.MinOrderQty)
	if err != nil {
		return market.SymbolSpec{}, fmt.Errorf("symbol %s: min_order_qty: %w", d.Symbol, err)
	}
	maxQty, err := optQty(d.MaxOrderQty)
	if err != nil {
		return market.SymbolSpec{}, fmt.Errorf("symbol %s: max_order_qty: %w", d.Symbol, err)
	}
	maxPos, err := optQty(d.MaxPositionSize)
	if err != nil {
		return market.SymbolSpec{}, fmt.Errorf("symbol %s: max_position_size: %w", d.Symbol, err)
	}

	return market.SymbolSpec{
		Symbol:            market.Symbol(d.Symbol),
		PriceTickDecimals: d.PriceTickDecimals,
		QtyTickDecimals:   d.QtyTickDecimals,
		MinOrderQty:       minQty,
		MaxOrderQty:       maxQty,
		MaxPositionSize:   maxPos,
		MaxOpenOrders:     optInt(d.MaxOpenOrders),
		MaxLeverage:       optInt(d.MaxLeverage),
		MakerFee:          optDecimal(d.MakerFee),
		TakerFee:          optDecimal(d.TakerFee),
		Enabled:           d.Enabled,
	}, nil
}

// MarkPrice извлекает справочную цену из котировочного снимка.
func MarkPrice(d dto.SymbolPrice, received time.Time) (market.MarkPrice, error) {
	price, err := optPrice(d.MarkPrice)
	if err != nil {
		return market.MarkPrice{}, fmt.Errorf("symbol %s: mark_price: %w", d.Symbol, err)
	}
	if !price.IsPositive() {
		return market.MarkPrice{}, fmt.Errorf("symbol %s: non-positive mark_price %q", d.Symbol, d.MarkPrice)
	}
	return market.MarkPrice{
		Symbol:   market.Symbol(d.Symbol),
		Price:    price,
		Received: received,
	}, nil
}

// Book переводит снимок стакана, попутно сортируя уровни: их порядок биржей
// не гарантирован.
func Book(d dto.DepthBook, received time.Time) (market.Book, error) {
	bids, err := levels(d.Bids)
	if err != nil {
		return market.Book{}, fmt.Errorf("symbol %s: bids: %w", d.Symbol, err)
	}
	asks, err := levels(d.Asks)
	if err != nil {
		return market.Book{}, fmt.Errorf("symbol %s: asks: %w", d.Symbol, err)
	}
	return market.NewBook(market.Symbol(d.Symbol), bids, asks, received), nil
}

func levels(raw [][2]string) ([]market.Level, error) {
	out := make([]market.Level, 0, len(raw))
	for _, l := range raw {
		price, err := money.ParsePrice(l[0])
		if err != nil {
			return nil, err
		}
		qty, err := money.ParseQty(l[1])
		if err != nil {
			return nil, err
		}
		out = append(out, market.Level{Price: price, Qty: qty})
	}
	return out, nil
}

// --- Разбор необязательных полей ---
//
// Биржа отдаёт отсутствующие значения пустой строкой, поэтому пустое значение
// трактуется как ноль, а не как ошибка.

func optPrice(s string) (money.Price, error) {
	if s == "" {
		return money.NewPrice(decimal.Zero), nil
	}
	return money.ParsePrice(s)
}

func optQty(s string) (money.Qty, error) {
	if s == "" {
		return money.NewQty(decimal.Zero), nil
	}
	return money.ParseQty(s)
}

func optDecimal(s string) decimal.Decimal {
	if s == "" {
		return decimal.Zero
	}
	d, err := decimal.NewFromString(s)
	if err != nil {
		return decimal.Zero
	}
	return d
}

func optInt(s string) int {
	if s == "" {
		return 0
	}
	// Биржа отдаёт плечо и лимиты то целым, то с дробной частью ("10" или "10.0").
	d, err := decimal.NewFromString(s)
	if err != nil {
		v, convErr := strconv.Atoi(s)
		if convErr != nil {
			return 0
		}
		return v
	}
	return int(d.IntPart())
}

func optTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t
}
