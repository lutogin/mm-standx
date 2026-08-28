package events

import (
	"time"

	"github.com/shopspring/decimal"

	"github.com/nd/mm-standx/internal/domain/calendar"
	"github.com/nd/mm-standx/internal/domain/market"
	"github.com/nd/mm-standx/internal/domain/money"
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

// ReferencePriceUpdated — обновилась цена на внешней площадке, опережающей mark.
//
// Награда по этой цене не считается никогда: она приходит с чужой площадки
// и к полосе биржи отношения не имеет. Её единственная роль — снять слепоту
// между тактами mark, то есть питать пороги опасности (PLAN.md §4.7).
type ReferencePriceUpdated struct {
	Reference market.ReferencePrice
}

func (ReferencePriceUpdated) EventName() string { return "market.reference_price_updated" }

// NewsCalendarUpdated — обновлено расписание экономических публикаций.
//
// Передаётся расписание целиком, а не команда «уйти из стакана». Актор
// вычисляет позу сам, по своим часам: событие о переходе могло бы не прийти,
// и тогда бот стоял бы в стакане на публикации либо, наоборот, не возвращался
// после неё. Расписание же остаётся верным независимо от того, пришло ли
// следующее сообщение.
type NewsCalendarUpdated struct {
	Calendar calendar.Calendar
	// Fetched — когда расписание последний раз удалось прочитать. Пустое
	// значение означает, что не удавалось ни разу.
	Fetched time.Time
	At      time.Time
}

func (NewsCalendarUpdated) EventName() string { return "market.news_calendar_updated" }

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

// QuoteFilled — рынок достал нашу котировку.
//
// Отличается от OrderUpdated тем, что несёт истолкование факта, а не сам факт:
// актор уже знает, начинает ли это исполнение новую серию, и во что оно
// обойдётся при закрытии по рынку. Восстанавливать это в защитном контуре
// заново значило бы держать вторую копию состояния лесенки.
type QuoteFilled struct {
	Symbol market.Symbol
	Side   market.Side
	Qty    money.Qty
	Price  money.Price
	// NotionalUSD — объём исполнения в долларах.
	NotionalUSD decimal.Decimal
	// EstimatedCostUSD — оценка стоимости: тейкерская комиссия за закрытие,
	// мейкерская за само исполнение и половина спреда как проскальзывание.
	EstimatedCostUSD decimal.Decimal
	// StartsEpisode отличает первое исполнение серии от продолжения.
	StartsEpisode bool
	// Rung is the ladder index the filled order was standing on, or
	// RungUnknown when the order is no longer matched to a slot.
	//
	// It decides the ladder geometry and nothing else. A ladder limits the
	// size taken at a touch only if fills arrive rung by rung; if the market
	// sweeps the side, the extra rungs buy no protection and cost weight,
	// because the mean rung weight is what the programme scores. Both readings
	// exist in the record — three rungs went in 51 ms on 6 August, six fills in
	// 1.8 s on 18 August — and neither says how the ordinary fill behaves.
	Rung int
	At   time.Time
}

// RungUnknown marks a fill that could not be attributed to a ladder rung: the
// slot had already been released when the update arrived. Counting these as
// rung zero would quietly bias the distribution the geometry is chosen from.
const RungUnknown = -1

func (QuoteFilled) EventName() string { return "account.quote_filled" }

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
	// Optional отличает поток, улучшающий решение, от потока, без которого
	// решение принимать нельзя.
	//
	// Разрыв обязательного канала означает торговлю вслепую и приводит
	// к немедленному съёму котировок. Разрыв необязательного означает лишь,
	// что мы вернулись к той картине, которую видели и раньше, — снимать
	// котировки в этом случае значило бы платить присутствием за чужой сбой.
	Optional bool
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

// KillSwitchCleared — аварийная остановка снята, котирование разрешено снова.
//
// Существует потому, что остановка у актора была защёлкой в одну сторону:
// поднять её было нечем, и переживший смену суток UTC процесс продолжал молчать
// с уже пустым запретом. Событие публикуется, когда вердикт защитного контура
// перестаёт быть остановкой, — на смене суток или после того, как человек
// вычистил снимок состояния.
type KillSwitchCleared struct {
	At time.Time
}

func (KillSwitchCleared) EventName() string { return "system.kill_switch_cleared" }

// QuotingPaused — котирование приостановлено на срок.
//
// Отличается от KillSwitchTripped ровно тем, ради чего и заведено: пауза имеет
// конец, известный в момент её объявления, и снимается без участия человека.
// Причина — транзиентная (частота заливов в скользящем часе), поэтому и реакция
// обязана быть транзиентной.
type QuotingPaused struct {
	Until  time.Time
	Reason string
	At     time.Time
}

func (QuotingPaused) EventName() string { return "system.quoting_paused" }

// QuotingResumed — срок паузы истёк.
type QuotingResumed struct {
	At time.Time
}

func (QuotingResumed) EventName() string { return "system.quoting_resumed" }
