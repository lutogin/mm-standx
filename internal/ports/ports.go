// Package ports объявляет контракты между приложением и внешним миром.
//
// Здесь только интерфейсы и передаваемые структуры — никакой логики. Слой app
// зависит исключительно от этого пакета, реализации живут в adapters.
package ports

import (
	"context"
	"errors"
	"time"

	"github.com/nd/mm-standx/internal/domain/calendar"
	"github.com/nd/mm-standx/internal/domain/market"
	"github.com/nd/mm-standx/internal/domain/money"
	"github.com/nd/mm-standx/internal/domain/trading"
)

// Runnable — компонент с жизненным циклом: поток данных, сервер метрик, актор.
//
// Композиционный корень регистрирует такие компоненты в fx.Lifecycle. Start
// не должен блокироваться: фоновая работа запускается в отдельной горутине,
// которую останавливает Stop.
type Runnable interface {
	Name() string
	Start(ctx context.Context) error
	Stop(ctx context.Context) error
}

// Priority определяет очерёдность запроса при нехватке лимита.
//
// Команды безопасности обязаны иметь возможность вытеснить команды заработка:
// иначе бот, исчерпавший бюджет запросов на перекотировках, не сможет снять
// ордер перед заливом.
type Priority int

const (
	// PrioritySafety — снятие котировок, закрытие позиции. Расходует резерв.
	PrioritySafety Priority = iota
	// PriorityQuote — размещение и перенос котировок ради награды.
	PriorityQuote
	// PriorityBackground — сверка, справочники, всё что может подождать.
	PriorityBackground
)

func (p Priority) String() string {
	switch p {
	case PrioritySafety:
		return "safety"
	case PriorityQuote:
		return "quote"
	default:
		return "background"
	}
}

// RateLimiter шейпит исходящие запросы ниже биржевого лимита.
type RateLimiter interface {
	// Acquire блокирует до появления кредита либо до отмены контекста.
	Acquire(ctx context.Context, p Priority) error
	// Available возвращает текущее число доступных запросов — для метрик.
	Available() float64
}

// PlaceRequest — заявка на размещение ордера.
type PlaceRequest struct {
	ClOrdID     trading.ClOrdID
	Symbol      market.Symbol
	Side        market.Side
	Type        trading.OrderType
	TimeInForce trading.TimeInForce
	Price       money.Price // игнорируется для рыночных ордеров
	Qty         money.Qty
	ReduceOnly  bool
	MarginMode  trading.MarginMode
	Leverage    int
	Priority    Priority
}

// OrderGateway отправляет торговые команды на биржу.
//
// Методы возвращают ошибку только на этапе передачи заявки. Судьба ордера
// приходит асинхронно событием OrderUpdated: у StandX успешный ответ означает
// лишь приём заявки, но не постановку в стакан.
type OrderGateway interface {
	Place(ctx context.Context, req PlaceRequest) error
	Cancel(ctx context.Context, id trading.ClOrdID, p Priority) error
	CancelBatch(ctx context.Context, ids []trading.ClOrdID, p Priority) error
}

// ExchangeReader читает состояние биржи запросами. Используется для сверки
// и загрузки справочников, но не в горячем пути — там работают события.
type ExchangeReader interface {
	Symbols(ctx context.Context) ([]market.Symbol, error)
	SymbolSpec(ctx context.Context, s market.Symbol) (market.SymbolSpec, error)
	OpenOrders(ctx context.Context, s market.Symbol) ([]trading.Order, error)
	Positions(ctx context.Context, s market.Symbol) ([]trading.Position, error)
	Balance(ctx context.Context) (trading.Balance, error)
}

// ErrNoCredentials означает отсутствие учётных данных.
//
// В отличие от сетевого сбоя это состояние повтором запроса не исправляется,
// поэтому клиенты обязаны прекращать попытки, а не расходовать бюджет
// на заведомо безнадёжные обращения.
var ErrNoCredentials = errors.New("credentials are not configured")

// TokenProvider выдаёт действующий JWT, продлевая его по мере необходимости.
//
// Отсутствие ключа возвращается как ErrNoCredentials; прочие ошибки считаются
// временными и допускают повтор.
type TokenProvider interface {
	Token(ctx context.Context) (string, error)
}

// BodySigner подписывает тело торгового запроса эфемерным ключом сессии.
type BodySigner interface {
	// Sign возвращает заголовки подписи для переданного тела. Подписывается
	// ровно тот байтовый срез, который уйдёт в сеть.
	Sign(body []byte) (SignatureHeaders, error)
}

// SignatureHeaders — заголовки подписи тела запроса StandX.
type SignatureHeaders struct {
	Version   string
	RequestID string
	Timestamp int64
	Signature string
}

// Clock абстрагирует время, чтобы тесты не зависели от реальных задержек.
//
// Прямые вызовы time.Now, time.Sleep и time.After вне адаптеров запрещены.
type Clock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
	NewTicker(d time.Duration) Ticker
	Sleep(ctx context.Context, d time.Duration) error
}

// Ticker — периодический источник тиков.
type Ticker interface {
	C() <-chan time.Time
	Stop()
}

// NewsSource отдаёт расписание экономических публикаций на ближайшее время.
//
// Отдельный порт, а не поле конфигурации: календарь приходит от третьей
// стороны, у которой перед нами обязательств нет, и подменить его в тестах
// нужно так же, как биржу.
type NewsSource interface {
	Events(ctx context.Context) ([]calendar.Event, error)
}

// Recorder сохраняет сырые сообщения потока для последующего воспроизведения
// в интеграционных тестах.
type Recorder interface {
	Record(stream string, received time.Time, payload []byte) error
	Close() error
}

// PositionConfigurer читает и меняет настройки счёта по инструменту.
//
// Вынесено из ExchangeReader намеренно: чтение здесь идёт в паре с записью,
// и разделять их значило бы разрешить менять плечо, не проверив результат.
// Ответ биржи означает приём запроса, а не применение настройки.
type PositionConfigurer interface {
	PositionConfig(ctx context.Context, s market.Symbol) (trading.PositionConfig, error)
	SetLeverage(ctx context.Context, s market.Symbol, leverage int) error
}

// Liveness — отметка о том, что актор инструмента прошёл очередной такт.
//
// Порт существует потому, что признак жизни процесса и признак жизни торговли —
// разные вещи, а до сих пор различались только на словах. Файл биения писала
// отдельная горутина по своему таймеру, ничего не знавшая об акторах, поэтому
// заблокировавшийся актор при живом процессе выглядел здоровым и для сторожа,
// и для /healthz. 13 августа так и вышло: контейнер отвечал healthy тридцать
// пять минут, пока актор стоял мёртвым (PLAN.md §0, дефект 3).
//
// Реализация обязана быть неблокирующей и безопасной для конкурентного вызова:
// отметка ставится из горячего пути актора, а читают её другие горутины.
type Liveness interface {
	Beat(s market.Symbol, at time.Time)
}

// LivenessSource отвечает, какой из ожидаемых акторов молчит дольше всех.
//
// Возвращает false, когда молчащих нет. Отсчёт для инструмента, не тикнувшего
// ещё ни разу, идёт с момента запуска: «не ожил» и «умер» для наблюдателя —
// одно и то же, и различать их значило бы дать процессу бессрочную фору
// на старте.
type LivenessSource interface {
	Stalest(now time.Time) (symbol market.Symbol, silent time.Duration, stale bool)
}

// NopLiveness — отметка, которую никто не читает. Для тестов и утилит.
type NopLiveness struct{}

func (NopLiveness) Beat(market.Symbol, time.Time) {}

var _ Liveness = NopLiveness{}
