// Package trading описывает торговые сущности: ордера, позиции, баланс.
//
// Это внутреннее представление, не зависящее от формата биржи. Маппинг из DTO
// StandX живёт в adapters/standx/mapper.
package trading

import (
	"time"

	"github.com/shopspring/decimal"

	"github.com/nd/mm-standx/internal/domain/market"
	"github.com/nd/mm-standx/internal/domain/money"
)

// OrderType — тип ордера.
type OrderType string

const (
	Limit  OrderType = "limit"
	Market OrderType = "market"
)

// TimeInForce — срок жизни и режим исполнения ордера.
type TimeInForce string

const (
	// GTC — обычный лимитный ордер, живёт до отмены.
	GTC TimeInForce = "gtc"
	// IOC — исполнить сколько можно немедленно, остаток отменить. Используется
	// для закрытия позиции после залива.
	IOC TimeInForce = "ioc"
	// ALO — post-only: биржа отклонит ордер, если он пересечёт стакан.
	// Главная защита стратегии от превращения в тейкера.
	ALO TimeInForce = "alo"
)

// OrderStatus — состояние ордера на бирже.
type OrderStatus string

const (
	StatusOpen        OrderStatus = "open"
	StatusCanceled    OrderStatus = "canceled"
	StatusFilled      OrderStatus = "filled"
	StatusRejected    OrderStatus = "rejected"
	StatusUntriggered OrderStatus = "untriggered"
)

// IsTerminal сообщает, что ордер больше не может измениться.
func (s OrderStatus) IsTerminal() bool {
	switch s {
	case StatusCanceled, StatusFilled, StatusRejected:
		return true
	default:
		return false
	}
}

// MarginMode — режим маржи позиции.
type MarginMode string

const (
	Cross    MarginMode = "cross"
	Isolated MarginMode = "isolated"
)

// ClOrdID — клиентский идентификатор ордера (ULID), генерируется нами.
//
// Это основной ключ во всей системе: он известен до отправки запроса, переживает
// ретраи и позволяет однозначно сверить локальное состояние с биржевым.
type ClOrdID string

func (c ClOrdID) String() string { return string(c) }

// OrderID — биржевой идентификатор, известен только после подтверждения.
type OrderID int64

// Order — снимок состояния ордера.
type Order struct {
	ID           OrderID
	ClOrdID      ClOrdID
	Symbol       market.Symbol
	Side         market.Side
	Type         OrderType
	TimeInForce  TimeInForce
	Status       OrderStatus
	Price        money.Price
	Qty          money.Qty
	FilledQty    money.Qty
	AvgFillPrice money.Price
	ReduceOnly   bool
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// RemainingQty возвращает неисполненный остаток.
func (o Order) RemainingQty() money.Qty { return o.Qty.Sub(o.FilledQty) }

// IsLive сообщает, что ордер стоит в стакане и может быть исполнен.
func (o Order) IsLive() bool { return o.Status == StatusOpen }

// Position — открытая позиция по инструменту.
type Position struct {
	Symbol     market.Symbol
	Qty        money.Qty // знаковое: положительное — лонг, отрицательное — шорт
	EntryPrice money.Price
	MarkPrice  money.Price
	LiqPrice   money.Price
	Leverage   int
	MarginMode MarginMode
	Unrealized decimal.Decimal
	Realized   decimal.Decimal
	UpdatedAt  time.Time
}

// IsFlat сообщает об отсутствии экспозиции.
func (p Position) IsFlat() bool { return p.Qty.IsZero() }

// Side возвращает сторону позиции. Для плоской позиции результат не определён,
// поэтому вызывать следует только после проверки IsFlat.
func (p Position) Side() market.Side {
	if p.Qty.IsNegative() {
		return market.Sell
	}
	return market.Buy
}

// ClosingSide возвращает сторону ордера, закрывающего позицию.
func (p Position) ClosingSide() market.Side { return p.Side().Opposite() }

// Balance — сводка средств аккаунта.
type Balance struct {
	Equity    decimal.Decimal // balance + upnl
	Available decimal.Decimal // cross_available
	Locked    decimal.Decimal // блокировка под висящими ордерами
	Margin    decimal.Decimal // маржа под исполненными позициями
	UpdatedAt time.Time
}

// Trade — наше исполнение.
type Trade struct {
	ID       int64
	OrderID  OrderID
	Symbol   market.Symbol
	Side     market.Side
	Price    money.Price
	Qty      money.Qty
	Fee      decimal.Decimal
	FeeAsset string
	PnL      decimal.Decimal
	Time     time.Time
}
