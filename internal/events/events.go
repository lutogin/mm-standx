package events

import (
	"time"

	"github.com/nd/mm-standx/internal/domain/market"
	"github.com/nd/mm-standx/internal/domain/trading"
)

// Словарь событий системы. Каждое событие — факт в прошедшем времени, не команда.
//
// Имена топиков стабильны и используются в метриках, поэтому менять их без нужды
// не стоит.

// --- Рыночные данные ---

// MarkPriceUpdated — пришла новая справочная цена. Основной вход стратегии:
// вся программа наград считается относительно mark price.
type MarkPriceUpdated struct {
	Mark market.MarkPrice
}

func (MarkPriceUpdated) EventName() string { return "market.mark_price_updated" }

// BookUpdated — обновился стакан. Используется для проверки на пересечение
// до отправки ордера.
type BookUpdated struct {
	Book market.Book
}

func (BookUpdated) EventName() string { return "market.book_updated" }

// --- Состояние аккаунта ---

// OrderUpdated — биржа сообщила о переходе ордера в новое состояние.
//
// Единственный достоверный источник информации о судьбе ордера: ответ на запрос
// размещения означает лишь приём заявки, но не постановку в стакан.
type OrderUpdated struct {
	Order trading.Order
}

func (OrderUpdated) EventName() string { return "account.order_updated" }

// OrderRejected — биржа отклонила заявку на этапе матчинга.
//
// Основной случай — отклонение ALO: наша котировка оказалась бы тейкером,
// то есть дистанция подобрана слишком агрессивно относительно стакана.
// Событие приходит быстрее, чем обновление в канале order, и служит сигналом
// расширить дистанцию.
type OrderRejected struct {
	ClOrdID trading.ClOrdID
	Code    int
	Reason  string
	At      time.Time
}

func (OrderRejected) EventName() string { return "account.order_rejected" }

// PositionUpdated — изменилась позиция.
type PositionUpdated struct {
	Position trading.Position
}

func (PositionUpdated) EventName() string { return "account.position_updated" }

// BalanceUpdated — изменился баланс.
type BalanceUpdated struct {
	Balance trading.Balance
}

func (BalanceUpdated) EventName() string { return "account.balance_updated" }

// OpenOrdersSnapshot — полный список висящих ордеров по инструменту,
// прочитанный запросом.
//
// Единственный способ обнаружить расхождение локального состояния
// с действительностью. Потоковые события можно пропустить, ответ на запрос —
// нет, поэтому снимок и служит опорой сверки.
type OpenOrdersSnapshot struct {
	Symbol market.Symbol
	Orders []trading.Order
	At     time.Time
}

func (OpenOrdersSnapshot) EventName() string { return "account.open_orders_snapshot" }

// TradeExecuted — наше исполнение с известной комиссией.
type TradeExecuted struct {
	Trade trading.Trade
}

func (TradeExecuted) EventName() string { return "account.trade_executed" }

// --- Состояние соединений ---

// StreamConnected — поток подключился и подписки восстановлены.
type StreamConnected struct {
	Stream string
	At     time.Time
}

func (StreamConnected) EventName() string { return "conn.stream_connected" }

// StreamDisconnected — поток разорван. Reason содержит причину, если известна.
type StreamDisconnected struct {
	Stream string
	Reason error
	At     time.Time
}

func (StreamDisconnected) EventName() string { return "conn.stream_disconnected" }

// MarkPriceStale — справочная цена не обновлялась дольше допустимого.
//
// Торговать вслепую нельзя: подписчик обязан снять котировки.
type MarkPriceStale struct {
	Symbol market.Symbol
	Since  time.Time
	Age    time.Duration
}

func (MarkPriceStale) EventName() string { return "conn.mark_price_stale" }

// --- Системные ---

// KillSwitchTripped — сработала аварийная остановка. Все котировки снимаются,
// новые ордера не отправляются до перезапуска.
type KillSwitchTripped struct {
	Reason string
	At     time.Time
}

func (KillSwitchTripped) EventName() string { return "system.kill_switch_tripped" }
