package ports

import (
	"github.com/nd/mm-standx/internal/domain/market"
)

// Metrics публикует показатели работы бота.
//
// Интерфейс намеренно состоит из именованных методов, а не из универсальной
// пары «имя счётчика — значение». Набор показателей у стратегии фиксирован
// и продуман (PLAN.md §8), а строковые имена превратили бы опечатку в молчаливо
// исчезнувшую метрику — то есть в слепое пятно ровно там, где наблюдаемость
// и нужна.
//
// Реализация обязана быть неблокирующей и безопасной для конкурентного вызова:
// метрики пишутся из горячего пути актора.
type Metrics interface {
	// --- Награда: то, ради чего всё работает ---

	// QuoteUptime — доля наблюдений текущего часа с квалифицирующими
	// котировками. Главный SLI: именно он определяет начисление.
	QuoteUptime(s market.Symbol, ratio float64)
	// EstimatedMakerHours — наша оценка начисления за текущий час.
	// Сверяется с отчётом StandX; расхождение означает ошибку в модели.
	EstimatedMakerHours(s market.Symbol, mh float64)
	// QuoteDistance — фактическая дистанция выставленного ордера.
	QuoteDistance(s market.Symbol, side market.Side, bps float64)
	// LiveQuotes — сколько ступеней стороны стоит в стакане.
	LiveQuotes(s market.Symbol, side market.Side, n int)
	// Quoting — котирует ли актор прямо сейчас.
	Quoting(s market.Symbol, yes bool)

	// --- Модель риска ---

	// TargetDistance — расчётная целевая дистанция.
	TargetDistance(s market.Symbol, bps float64)
	// HalfSpread — половина спреда, измеренная по стакану.
	HalfSpread(s market.Symbol, bps float64)
	// BookDepth — нотионал, стоящий на стороне внутри полосы награды.
	//
	// Спред описывает только вершину. Стакан, узкий сверху и пустой за ней,
	// пропускает небольшой рыночный ордер на несколько базисных пунктов вглубь
	// и достаёт котировку, которая по спреду выглядела защищённой.
	BookDepth(s market.Symbol, side market.Side, usd float64)
	// Jump — принятая мера риска: скачок mark price за такт с поправками.
	Jump(s market.Symbol, bps float64)
	// MarkStaleness — возраст последней справочной цены в секундах.
	MarkStaleness(s market.Symbol, seconds float64)

	// --- Цена стратегии ---

	// Fill — исполнение котировки, то есть событие, которого стратегия
	// стремится избежать.
	Fill(s market.Symbol, side market.Side)
	// ShadowFill — рынок дошёл до нашей котировки в теневом прогоне.
	//
	// Отдельный ряд, а не Fill: смешивать измеренное с гипотетическим в одной
	// метрике значит готовить себе неверный вывод через месяц, когда происхождение
	// чисел забудется. Это верхняя оценка — рынок мог коснуться уровня,
	// не выбрав его целиком.
	ShadowFill(s market.Symbol, side market.Side)
	// FlattenCost — оценка стоимости закрытия позиции после залива.
	FlattenCost(s market.Symbol, usd float64)
	// RealizedPnL — результат за сутки по данным биржи, знаковый.
	RealizedPnL(usd float64)
	// EstimatedCost — независимая оценка стоимости заливов за сутки.
	EstimatedCost(usd float64)
	// FillsLastHour — число заливов в скользящем часе.
	FillsLastHour(n int)
	// KillSwitch — взведена ли аварийная остановка.
	KillSwitch(tripped bool)

	// --- Нагрузка и здоровье ---

	// OrderSent — отправленная торговая команда.
	OrderSent(s market.Symbol, side market.Side, kind string)
	// OrderFailed — команда, не покинувшая процесс.
	OrderFailed(s market.Symbol, kind string)
	// ALOReject — отклонение post-only: дистанция подобрана слишком агрессивно.
	ALOReject(s market.Symbol)
	// RateLimitTokens — остаток кредитов шейпера.
	RateLimitTokens(v float64)
	// StreamReconnect — переподключение потока.
	StreamReconnect(stream string)
	// Withdrawal — аварийный съём котировок.
	Withdrawal(s market.Symbol, cause WithdrawCause)
}

// WithdrawCause — причина съёма котировок, приведённая к замкнутому множеству.
//
// Человекочитаемое объяснение уходит в лог и содержит измеренные величины;
// сюда попадает только вид причины. Разница принципиальна: метка Prometheus
// с текстом вроде «risk demands 12.3 bps» породила бы новый временной ряд
// на каждое значение, и через сутки хранилище метрик заняла бы не статистика
// съёмов, а перечисление всех наблюдавшихся дистанций.
type WithdrawCause string

const (
	// CauseStaleMark — справочная цена перестала обновляться.
	CauseStaleMark WithdrawCause = "stale_mark"
	// CauseDisconnected — поток данных разорван.
	CauseDisconnected WithdrawCause = "disconnected"
	// CausePositionOpen — есть незакрытая экспозиция.
	CausePositionOpen WithdrawCause = "position_open"
	// CauseFilled — котировку залили.
	CauseFilled WithdrawCause = "filled"
	// CauseNoSafeDistance — требуемая риском дистанция не помещается в полосу.
	CauseNoSafeDistance WithdrawCause = "no_safe_distance"
	// CauseHourLost — час недостижим, дальнейший риск не окупается.
	CauseHourLost WithdrawCause = "hour_lost"
	// CauseSizing — размер ордера не вычислим.
	CauseSizing WithdrawCause = "sizing"
	// CauseBookUnreadable — стакан не читается.
	CauseBookUnreadable WithdrawCause = "book_unreadable"
	// CausePositionLimit — позиция превысила разрешённую инструменту.
	CausePositionLimit WithdrawCause = "position_limit"
	// CauseKillSwitch — сработала аварийная остановка.
	CauseKillSwitch WithdrawCause = "kill_switch"
	// CauseShutdown — штатное завершение процесса.
	CauseShutdown WithdrawCause = "shutdown"
)

// NopMetrics — реализация, не делающая ничего.
//
// Нужна тестам и утилитам: обязывать каждый вызывающий код проверять nil
// значило бы засорять горячий путь проверками ради удобства сборки.
type NopMetrics struct{}

func (NopMetrics) QuoteUptime(market.Symbol, float64)                {}
func (NopMetrics) EstimatedMakerHours(market.Symbol, float64)        {}
func (NopMetrics) QuoteDistance(market.Symbol, market.Side, float64) {}
func (NopMetrics) LiveQuotes(market.Symbol, market.Side, int)        {}
func (NopMetrics) Quoting(market.Symbol, bool)                       {}
func (NopMetrics) TargetDistance(market.Symbol, float64)             {}
func (NopMetrics) HalfSpread(market.Symbol, float64)                 {}
func (NopMetrics) BookDepth(market.Symbol, market.Side, float64)     {}
func (NopMetrics) Jump(market.Symbol, float64)                       {}
func (NopMetrics) MarkStaleness(market.Symbol, float64)              {}
func (NopMetrics) Fill(market.Symbol, market.Side)                   {}
func (NopMetrics) ShadowFill(market.Symbol, market.Side)             {}
func (NopMetrics) FlattenCost(market.Symbol, float64)                {}
func (NopMetrics) RealizedPnL(float64)                               {}
func (NopMetrics) EstimatedCost(float64)                             {}
func (NopMetrics) FillsLastHour(int)                                 {}
func (NopMetrics) KillSwitch(bool)                                   {}
func (NopMetrics) OrderSent(market.Symbol, market.Side, string)      {}
func (NopMetrics) OrderFailed(market.Symbol, string)                 {}
func (NopMetrics) ALOReject(market.Symbol)                           {}
func (NopMetrics) RateLimitTokens(float64)                           {}
func (NopMetrics) StreamReconnect(string)                            {}
func (NopMetrics) Withdrawal(market.Symbol, WithdrawCause)           {}

var _ Metrics = NopMetrics{}

// MetricKind различает накопительный счётчик и мгновенное значение.
type MetricKind uint8

const (
	// KindGauge — величина, осмысленная в момент замера: спред, скачок,
	// глубина стакана. Для отчёта за час нужны её квантили.
	KindGauge MetricKind = iota
	// KindCounter — монотонный счётчик: заливы, съёмы, отправленные команды.
	// Для отчёта за час нужна разность между началом и концом.
	KindCounter
)

// MetricSample — одно значение показателя со своими метками.
type MetricSample struct {
	Name   string
	Labels map[string]string
	Kind   MetricKind
	Value  float64
}

// MetricsSource отдаёт текущие значения всех показателей.
//
// Существует ради выгрузки истории на диск. Реестр показателей живёт в памяти
// и отвечает по HTTP, но отвечает он одним мгновенным снимком: без внешнего
// сборщика за неделю работы останется одно число «сейчас» вместо ряда.
// Периодическая свёртка на диск закрывает это, не требуя ни отдельного
// сервиса, ни базы — файл забирается с сервера как обычный файл.
type MetricsSource interface {
	Snapshot() []MetricSample
}
