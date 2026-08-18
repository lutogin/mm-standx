// Package config загружает и валидирует конфигурацию бота.
//
// Источник настроек ровно один — YAML-файл поверх значений по умолчанию.
// Окружение не переопределяет ничего: из него читается единственная вещь,
// которой в файле не место, — приватный ключ кошелька.
//
// Так было не всегда. Прежде любой параметр переопределялся переменной
// MMSTANDX_*, и это стоило дважды. 11 августа устаревший ключ `reference`,
// оставшийся в .env от закрытой ветки, уронил процесс в цикл падения; деплой
// на упавший старт откатывается на предыдущий образ, то есть одна забытая
// строка тихо заморозила прод на старой сборке. Второй раз — тише и дороже:
// действующие пределы риска разъехались с записанными в файле, и по числам
// в репозитории нельзя было сказать, чем именно торгует бот.
//
// Отсюда правило: действующее значение читается в одном месте, и это файл.
//
// Валидация строгая и происходит на старте: неверно упорядоченные пороги дистанций
// или отсутствующая аллокация — это ошибки, которые в бою стоят денег, поэтому
// процесс не должен подниматься с некорректным конфигом.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/go-viper/mapstructure/v2"
	"github.com/joho/godotenv"
	"github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/providers/structs"
	"github.com/knadh/koanf/v2"

	"github.com/nd/mm-standx/internal/domain/market"
	"github.com/nd/mm-standx/internal/domain/money"
	"github.com/nd/mm-standx/internal/domain/reward"
	"github.com/nd/mm-standx/internal/domain/trading"
	"github.com/nd/mm-standx/internal/secret"
)

// Mode — режим работы процесса.
type Mode string

const (
	// ModeDryRun — полный цикл работы, но торговые команды не уходят на биржу.
	// Значение по умолчанию: включить торговлю можно только осознанно.
	ModeDryRun Mode = "dry_run"
	// ModeLive — реальные ордера.
	ModeLive Mode = "live"
)

// Config — корневая конфигурация.
type Config struct {
	Runtime   Runtime           `koanf:"runtime"`
	Exchange  Exchange          `koanf:"exchange"`
	RateLimit RateLimit         `koanf:"rateLimit"`
	Reward    Reward            `koanf:"reward"`
	Risk      Risk              `koanf:"risk"`
	Health    Health            `koanf:"health"`
	News      News              `koanf:"news"`
	Defaults  Symbol            `koanf:"defaults"`
	Symbols   map[string]Symbol `koanf:"symbols"`
}

// Runtime — параметры процесса.
type Runtime struct {
	// Mode приходит из переменной MM_MODE и в файле не принимается.
	//
	// Это единственный параметр, вынесенный из конфигурации, и вынесен он
	// по цене ошибки, а не по удобству. Файл версионируется и одинаков везде,
	// где его запускают; строка `mode: live` в нём означала бы, что любой
	// локальный прогон на машине с рабочим ключом отправляет настоящие ордера.
	//
	// Незаданная переменная означает dry_run: торговля включается осознанным
	// действием, а не тем, что кто-то забыл что-то выставить.
	Mode      Mode   `koanf:"-"`
	LogLevel  string `koanf:"logLevel"`
	LogFormat string `koanf:"logFormat"` // json | text
	// LogFile — path to a bounded copy of the log on disk. Empty leaves stdout
	// as the only sink.
	//
	// Stdout alone is what the container's json-file driver captures, and those
	// files sit under /var/lib/docker owned by root, named after the container
	// id. That is a poor place to be looking things up during an incident, and
	// a `docker compose down` takes them with it. A path here puts the same
	// stream next to the compose file, where scp reaches it.
	LogFile string `koanf:"logFile"`
	// LogMaxSizeMB bounds one log file, LogMaxFiles is how many rotated copies
	// are kept besides the live one. The disk footprint is the product plus one.
	//
	// A trading box that fills its disk kills the process that is holding
	// orders in the market, so the bound is not optional once the file exists.
	LogMaxSizeMB int `koanf:"logMaxSizeMb"`
	LogMaxFiles  int `koanf:"logMaxFiles"`

	MetricsAddr   string        `koanf:"metricsAddr"`
	HeartbeatPath string        `koanf:"heartbeatPath"`
	RecordingDir  string        `koanf:"recordingDir"` // пусто — запись выключена
	StatsInterval time.Duration `koanf:"statsInterval"`
	// SessionPath — файл, в котором сохраняется выданный токен вместе с ключом
	// сессии. Позволяет пережить перезапуск, не подключая ключ кошелька.
	// Пусто — сессия не сохраняется.
	SessionPath string `koanf:"sessionPath"`
	// StateDir — каталог снимков состояния защитного контура.
	//
	// Пустое значение отключает сохранение, и это допустимо только в теневом
	// режиме: в бою счётчик убытка, обнуляемый перезапуском, не защищает
	// ни от чего.
	StateDir string `koanf:"stateDir"`
	// StatsDir — каталог почасовой свёртки показателей.
	//
	// Реестр показателей отвечает по HTTP мгновенным снимком; без выгрузки
	// на диск история не сохраняется вовсе. Пусто — свёртка выключена.
	StatsDir string `koanf:"statsDir"`
	// StatsSample — период съёма показаний внутри часа.
	StatsSample time.Duration `koanf:"statsSample"`
}

// Exchange — координаты и учётные данные биржи.
type Exchange struct {
	AuthBaseURL      string        `koanf:"authBaseUrl"`
	RESTBaseURL      string        `koanf:"restBaseUrl"`
	WSStreamURL      string        `koanf:"wsStreamUrl"`
	WSAPIURL         string        `koanf:"wsApiUrl"`
	Chain            string        `koanf:"chain"`
	WalletPrivateKey secret.Secret `koanf:"-"` // только из окружения
	TokenTTL         time.Duration `koanf:"tokenTtl"`
	TokenRefreshLead time.Duration `koanf:"tokenRefreshLead"`
	WSRotateAfter    time.Duration `koanf:"wsRotateAfter"`
	WSReconnectMin   time.Duration `koanf:"wsReconnectMin"`
	WSReconnectMax   time.Duration `koanf:"wsReconnectMax"`
	// WSIdleTimeout — сколько молчания считается разрывом. Активный ping
	// использовать нельзя: сервер StandX на него не отвечает.
	WSIdleTimeout time.Duration `koanf:"wsIdleTimeout"`
	// WSHeartbeatInterval — период прикладного запроса на молчащих каналах.
	WSHeartbeatInterval time.Duration `koanf:"wsHeartbeatInterval"`
	RequestTimeout      time.Duration `koanf:"requestTimeout"`
}

// RateLimit — параметры шейпера исходящих запросов.
type RateLimit struct {
	SustainedRPS     float64 `koanf:"sustainedRps"`
	Burst            int     `koanf:"burst"`
	SafetyReservePct float64 `koanf:"safetyReservePct"`
}

// Reward — параметры программы наград StandX.
//
// Вынесены в конфиг, потому что биржа меняет их без предупреждения, а таблица
// весов в документации противоречит формуле на той же странице (PLAN.md §12.1).
type Reward struct {
	BandBps      money.Bps    `koanf:"bandBps"`
	MaxWeightPct float64      `koanf:"maxWeightPct"`
	MinWeightPct float64      `koanf:"minWeightPct"`
	UptimeTiers  []UptimeTier `koanf:"uptimeTiers"`
}

// UptimeTier — порог аптайма и соответствующий множитель начисления.
type UptimeTier struct {
	ThresholdPct float64 `koanf:"thresholdPct"`
	Multiplier   float64 `koanf:"multiplier"`
}

// Risk — общие для всех инструментов ограничения риска.
type Risk struct {
	MaxDailyLossUSD float64 `koanf:"maxDailyLossUsd"`
	// UtilizationPct — доля equity, которую разрешено держать заблокированной
	// под котировками.
	//
	// Остаток нужен не про запас: висящие ордера уже блокируют маржу, и при
	// полной утилизации закрывающий ордер после залива может не пройти
	// по нехватке средств. Прежде жило константой в коде актора — предел риска,
	// который нельзя было ни увидеть в работающем процессе, ни поправить
	// без пересборки образа, в отличие от каждого соседнего предела здесь.
	UtilizationPct       float64            `koanf:"utilizationPct"`
	MaxFillsPerHour      int                `koanf:"maxFillsPerHour"`
	MarginMode           trading.MarginMode `koanf:"marginMode"`
	FlattenPolicy        string             `koanf:"flattenPolicy"`
	FlattenDebounce      time.Duration      `koanf:"flattenDebounce"`
	PostFillCooldown     time.Duration      `koanf:"postFillCooldown"`
	FillBurstLimit       int                `koanf:"fillBurstLimit"`
	FillBurstWindow      time.Duration      `koanf:"fillBurstWindow"`
	FillBurstCooldown    time.Duration      `koanf:"fillBurstCooldown"`
	KillSwitchOnDrawdown bool               `koanf:"killSwitchOnDrawdown"`
	Fees                 Fees               `koanf:"fees"`
}

// Fees — ставки комиссий биржи.
//
// Нужны не для расчёта котировок, а для независимой оценки стоимости залива.
// Биржа присылает и комиссию, и реализованный результат, но защитный контур,
// целиком зависящий от одного непроверенного канала, отказывает молча:
// отсутствие сообщений выглядит в точности как отсутствие убытка.
type Fees struct {
	// MakerBps — комиссия за исполнение нашей котировки.
	MakerBps money.Bps `koanf:"makerBps"`
	// TakerBps — комиссия за закрытие позиции по рынку.
	TakerBps money.Bps `koanf:"takerBps"`
}

// Health — пороги контроля живости.
//
// Отсрочки перед съёмом котировок при разрыве потока здесь нет намеренно.
// Разрыв приводит к немедленному съёму, а право котировать зарабатывается
// заново прогревом оценщика. Замер теневого прогона: десять переподключений
// за 22.6 часа стоили 11 минут присутствия, то есть 0.8%. Отсрочка выкупала бы
// эту долю ценой секунд слепоты с живыми ордерами — обмен невыгодный.
type Health struct {
	MarkStale time.Duration `koanf:"markStale"`
	// MarkStaleHoldBps — насколько ориентиру позволено разойтись с последним
	// тактом mark, чтобы уже стоящие котировки не снимались.
	//
	// Молчащий канал mark и движущийся рынок — разные события, и общего ответа
	// у них быть не должно. Пока внешняя площадка свежа и показывает цену там
	// же, где она была на последнем такте, геометрия висящих ордеров остаётся
	// той самой, что для них считалась, и снимать их значит платить
	// присутствием за чужой график публикаций. Замер 18 августа 2026:
	// 1514 съёмов за сутки, час 06:00 UTC закрылся нулём Maker Hours при
	// открытом и спокойном рынке.
	//
	// Ноль возвращает прежнее поведение — безусловный съём. Порог не отменяет
	// запрет выставлять по устаревшей цене: удерживаются только те ордера,
	// что уже стоят. См. Engine.onStaleMark.
	MarkStaleHoldBps  money.Bps     `koanf:"markStaleHoldBps"`
	ReconcileInterval time.Duration `koanf:"reconcileInterval"`
	AckTimeout        time.Duration `koanf:"ackTimeout"`
	HeartbeatInterval time.Duration `koanf:"heartbeatInterval"`
}

// News — блэкаут вокруг запланированных экономических публикаций.
//
// Единственная защита в контуре, работающая на упреждение, а не на реакцию.
// Всё остальное отвечает на движение, которое уже началось; против публикации,
// время которой известно заранее, это не работает вовсе — круг отмены занимает
// около 400 мс (PLAN.md §2.4), а золото на CPI проходит десятки базисных
// пунктов за первую секунду.
type News struct {
	Enabled bool `koanf:"enabled"`
	// URL недельного календаря. Пусто — адрес по умолчанию.
	URL string `koanf:"url"`
	// Currencies и Impacts отбирают публикации, которые нас касаются.
	// Пустой список не пропускает ничего: соглашение «пусто значит всё»
	// обнуляло бы стакан на каждой мелкой цифре недели.
	Currencies []string `koanf:"currencies"`
	Impacts    []string `koanf:"impacts"`

	// DefensiveBefore и DefensiveAfter — окно, в котором бот отходит к внешнему
	// краю полосы и уменьшает размер, но остаётся в стакане.
	DefensiveBefore time.Duration `koanf:"defensiveBefore"`
	DefensiveAfter  time.Duration `koanf:"defensiveAfter"`
	// HardBefore и HardAfter — окно, в котором бот уходит из стакана совсем.
	HardBefore time.Duration `koanf:"hardBefore"`
	HardAfter  time.Duration `koanf:"hardAfter"`

	// DefensiveInsetBps — насколько внутрь от края полосы разрешено стоять
	// в защитной позе. Ноль прижимает котировку вплотную к краю, где вес
	// минимален, но зачёт ещё идёт.
	DefensiveInsetBps money.Bps `koanf:"defensiveInsetBps"`
	// DefensiveSizeMultiple — во сколько раз уменьшается размер в защитной позе.
	//
	// Урезание бесплатно по зачёту и прямо пропорционально снижает потери
	// от исполнения: тир 0.5x берётся с медианы замеров, а её держат обычные
	// полчаса часа, тогда как урезанные замеры попадают лишь в верхний
	// перцентиль, который защитная поза и так отдала, уйдя к краю полосы.
	DefensiveSizeMultiple float64 `koanf:"defensiveSizeMultiple"`

	// Refresh — период повторного чтения календаря.
	Refresh time.Duration `koanf:"refresh"`
	// MaxAge — возраст, после которого расписанию перестают доверять.
	MaxAge time.Duration `koanf:"maxAge"`
	// Timeout ограничивает одно обращение к издателю.
	Timeout time.Duration `koanf:"timeout"`

	// FailOpen решает, что означает недоступный или устаревший календарь.
	//
	// True — котировать дальше по уже известному расписанию; недельный файл
	// покрывает неделю вперёд, и суточная недоступность издателя сама по себе
	// ничего не меняет. False — не котировать, пока расписание не подтверждено.
	//
	// По умолчанию true: остановка на сбое третьей стороны стоит присутствия
	// каждый раз, а слепота — только в те редкие часы, когда публикацию
	// перенесли ровно в окно недоступности.
	FailOpen bool `koanf:"failOpen"`
}

// Symbol — настройки котирования одного инструмента.
type Symbol struct {
	Enabled    bool       `koanf:"enabled"`
	Economics  Economics  `koanf:"economics"`
	Quote      Quote      `koanf:"quote"`
	Appetite   Appetite   `koanf:"appetite"`
	Volatility Volatility `koanf:"volatility"`
	Program    Program    `koanf:"program"`
	Uptime     Uptime     `koanf:"uptime"`
	SymbolRisk SymbolRisk `koanf:"risk"`
	Reference  Reference  `koanf:"reference"`
}

// Reference identifies the external venue whose price leads this instrument.
//
// The exchange publishes its mark about once every three seconds. What reaches
// a resting quote inside that window is arbitrage against a move that already
// happened on a faster venue, so watching that venue is watching the cause
// rather than the effect.
//
// The feed is advisory by construction. Losing it degrades the fast signal and
// must never stop quoting: the bot falls back to what it can see on the
// exchange itself. That is the whole reason there is no "required" flag here.
type Reference struct {
	Enabled bool `koanf:"enabled"`
	// Provider names the venue and market type, see the Provider* constants.
	Provider string `koanf:"provider"`
	// Symbol is the instrument as that venue names it, for example BTCUSDT.
	// It must track the same underlying: a mismatch is worse than no feed,
	// because a wrong signal is still acted upon.
	Symbol string `koanf:"symbol"`
	// StaleAfter is how much silence makes the reference unusable as a signal.
	//
	// The threshold is a property of the venue, not of the exchange: a liquid
	// book ticks tens of times a second, so silence measured in seconds means
	// the connection is broken in a way the socket has not noticed yet. Past
	// it the actor falls back to the exchange's own book mid rather than
	// steering by a price that stopped moving.
	StaleAfter time.Duration `koanf:"staleAfter"`
}

// Supported reference providers.
//
// The vocabulary lives here rather than in the adapter because the adapter
// depends on this package for connection settings, and the reverse dependency
// would close a cycle.
const (
	ProviderBinanceFutures = "binance_futures"
	ProviderBinanceSpot    = "binance_spot"
	ProviderBybitLinear    = "bybit_linear"
	ProviderBybitSpot      = "bybit_spot"
)

// ReferenceProviders lists every supported venue in a stable order.
func ReferenceProviders() []string {
	return []string{
		ProviderBinanceFutures,
		ProviderBinanceSpot,
		ProviderBybitLinear,
		ProviderBybitSpot,
	}
}

// Economics contains the measured unit economics for one market.
//
// RewardRateLowerBoundUSDPerMakerHour is intentionally a lower confidence
// bound, not a point estimate. Scaling on one unusually good day would convert
// uncertainty directly into fill losses.
type Economics struct {
	RewardRateLowerBoundUSDPerMakerHour float64 `koanf:"rewardRateLowerBoundUsdPerMakerHour"`
	BreakEvenUSDPerMakerHour            float64 `koanf:"breakEvenUsdPerMakerHour"`
}

// Quote — геометрия лесенки и темп перестановки.
//
// Дистанций здесь нет намеренно: они вычисляются по рыночным условиям,
// см. PLAN.md §4.5.
type Quote struct {
	LadderLayers    int           `koanf:"ladderLayers"`
	LadderSpreadBps money.Bps     `koanf:"ladderSpreadBps"`
	MinRest         time.Duration `koanf:"minRest"`
	RequoteCooldown time.Duration `koanf:"requoteCooldown"`

	// RefillLockout — сколько ступень остаётся пустой после того, как её
	// освободила защита. Ноль возвращает безусловное заполнение.
	// См. quotes.Rules.RefillLockout и quotes.Slot.RefillNotBefore.
	RefillLockout time.Duration `koanf:"refillLockout"`

	// PostOnlyBufferTicks — на сколько тиков цена отодвигается от противоположной
	// вершины стакана перед отправкой.
	//
	// Проверка на пересечение отвечает только на вопрос «отвергнет ли биржа».
	// Ордер на самой вершине она примет, а рынок заберёт первым же движением.
	// Ноль возвращает прежнее поведение.
	PostOnlyBufferTicks int `koanf:"postOnlyBufferTicks"`
}

// Appetite — готовность рисковать ради награды.
//
// Задаёт не сами дистанции, а правило их вычисления: сколько характерных
// скачков должно умещаться в глубину котировки.
type Appetite struct {
	// JumpMultiple — сколько скачков должно покрывать глубину.
	JumpMultiple float64 `koanf:"jumpMultiple"`
	// FloorMarginBps — абсолютный запас поверх расчётного.
	FloorMarginBps money.Bps `koanf:"floorMarginBps"`
	// DeadBandJumps — ширина мёртвой зоны в скачках.
	DeadBandJumps float64 `koanf:"deadBandJumps"`
	// MinDeadBandBps — нижняя граница мёртвой зоны.
	MinDeadBandBps money.Bps `koanf:"minDeadBandBps"`

	// PanicFraction — доля целевой дистанции, ближе которой сторона снимается
	// целиком аварийным путём. Больше значение — раньше отход и реже заливы,
	// ценой присутствия. Ноль оставляет только физический порог.
	PanicFraction float64 `koanf:"panicFraction"`

	// ReentryDeadBandBps — насколько требуемая дистанция обязана не дотягивать
	// до края полосы, чтобы актор вернулся в рынок после отказа.
	//
	// Мёртвая зона для решения «котировать или нет». Без неё хвост спреда
	// перебрасывает порог по нескольку раз в минуту, и лесенка снимается
	// и выставляется впустую. Ноль отключает гистерезис.
	ReentryDeadBandBps money.Bps `koanf:"reentryDeadBandBps"`
	// MinBps — ближе к mark не подходим ни при каких условиях.
	// Страхует случай, когда измеренный риск обманчиво мал.
	MinBps money.Bps `koanf:"minBps"`
	// MaxBps — дальше награда обнуляется.
	MaxBps money.Bps `koanf:"maxBps"`
}

// Volatility — параметры оценщика скачка.
type Volatility struct {
	// Window — глубина скользящего окна.
	Window time.Duration `koanf:"window"`
	// MinSamples — сколько замеров нужно, чтобы начать котировать.
	MinSamples int `koanf:"minSamples"`
	// FullConfidenceAt — при каком числе замеров оценка считается зрелой.
	FullConfidenceAt int `koanf:"fullConfidenceAt"`
	// MemoryHalfLife — полураспад памяти о крупных скачках.
	//
	// Нижняя граница меры риска, живущая дольше окна. Без неё замерший рынок
	// даёт зрелую оценку нулевого скачка, и бот подходит к цене вплотную
	// ровно перед тем, как рынок оживёт.
	MemoryHalfLife time.Duration `koanf:"memoryHalfLife"`

	// MemoryWindowMultiple bounds the decaying record by the live window: it may
	// never claim more than this multiple of the window maximum. Zero disables
	// the bound. See volatility.Config.MemoryWindowMultiple.
	MemoryWindowMultiple float64 `koanf:"memoryWindowMultiple"`
}

// Program — параметры начисления инструмента, публикуемые StandX.
type Program struct {
	// UnitSize — размер, дающий один Maker Hour.
	UnitSize string `koanf:"unitSize"`
	// PerSideCap — потолок засчитываемого размера на сторону.
	PerSideCap string `koanf:"perSideCap"`
	// MaxMakerHoursPerHour — потолок начисления за час.
	MaxMakerHoursPerHour float64 `koanf:"maxMakerHoursPerHour"`

	// OffHoursMultiplier scales the hour's accrual while the reference market
	// is shut. One, the default, describes a 24/7 market.
	OffHoursMultiplier float64 `koanf:"offHoursMultiplier"`
	// OffHours is the recurring shutdown window of the reference market.
	OffHours OffHours `koanf:"offHours"`
	// QuoteOffHours allows quoting while the reduced multiplier is in force.
	//
	// Off by default. Presence during those hours carries the full fill risk
	// and earns a fraction, so it has to be asked for explicitly.
	QuoteOffHours bool `koanf:"quoteOffHours"`
}

// Schedule converts the configured off-hours into the domain form.
//
// Lives here rather than in the composition root so that validation and
// construction read the same value: a window that parses at startup but not
// during validation would be a defect nobody sees until a Saturday.
func (p Program) Schedule() (reward.Schedule, error) {
	window, err := reward.ParseWindow(p.OffHours.From, p.OffHours.To)
	if err != nil {
		return reward.Schedule{}, err
	}

	holidays := make([]time.Time, 0, len(p.OffHours.Holidays))
	for _, raw := range p.OffHours.Holidays {
		day, err := time.Parse(time.DateOnly, strings.TrimSpace(raw))
		if err != nil {
			return reward.Schedule{}, fmt.Errorf("holiday %q: expected a date like 2026-12-25", raw)
		}
		holidays = append(holidays, day)
	}
	return reward.Schedule{Weekly: window, Holidays: holidays}, nil
}

// OffHours describes when the reference market behind an instrument is shut.
//
// Bounds are written as "Fri 21:00" in UTC, and the window is allowed to wrap
// through Sunday. An empty pair means the market never closes.
type OffHours struct {
	From string `koanf:"from"`
	To   string `koanf:"to"`
	// Holidays are ISO dates ("2026-12-25") on which the reference market is
	// shut for the whole UTC day. The exchange publishes no machine-readable
	// calendar, so the list is maintained by hand.
	Holidays []string `koanf:"holidays"`
}

// Uptime — цели по присутствию в стакане и бюджет простоя.
type Uptime struct {
	TargetPct    float64 `koanf:"targetPct"`
	HardFloorPct float64 `koanf:"hardFloorPct"`
	ReservePct   float64 `koanf:"reservePct"`
}

// SymbolRisk — ограничения риска на инструмент.
type SymbolRisk struct {
	// Leverage — плечо счёта по инструменту. Выставляется на бирже при старте
	// (account.ApplyLeverage); в самом ордере это поле биржа игнорирует.
	Leverage int `koanf:"leverage"`
	// InitialMarginPct — доля нотионала, блокируемая под висящим ордером.
	//
	// Свойство биржи, а не наша политика, и потому измеряется, а не выбирается:
	// `test/live/margin_test.go`, PLAN.md §12.5. Значение зависит от плеча
	// счёта, поэтому меняется вместе с ним и требует нового замера.
	//
	// Заниженное опаснее завышенного: оно раздувает размер, и после залива
	// закрывающему ордеру может не хватить свободных средств.
	InitialMarginPct   float64 `koanf:"initialMarginPct"`
	MaxNetPositionBase string  `koanf:"maxNetPositionBase"`
}

const (
	// dotenvPath — файл, подхватываемый при запуске.
	dotenvPath = ".env"
	// walletKeyEnv — переменная с приватным ключом кошелька.
	walletKeyEnv = "STANDX_WALLET_PK"
	// modeEnv — переменная с режимом работы.
	modeEnv = "MM_MODE"
	// retiredEnvPrefix — префикс снятого слоя переопределений.
	//
	// Значение больше не читается ниоткуда, но имя нужно, чтобы узнать
	// оставшуюся строку на сервере и сказать о ней вслух.
	retiredEnvPrefix = "MMSTANDX_"
)

// StaleEnvOverrides перечисляет переменные снятого слоя MMSTANDX_*, ещё
// присутствующие в окружении.
//
// Пустой список — норма и ожидаемый исход. Непустой означает, что окружение
// на сервере пережило смену версии: строка стоит, оператор считает параметр
// заданным, а действует значение из файла. Разница между «параметр задан»
// и «параметр действует» — ровно тот отказ, который здесь ловится.
//
// Список отсортирован: одинаковое окружение обязано давать одинаковую строку
// в логе, иначе её нельзя сравнить с предыдущим запуском.
func StaleEnvOverrides() []string {
	var out []string
	for _, entry := range os.Environ() {
		name, _, ok := strings.Cut(entry, "=")
		if ok && strings.HasPrefix(name, retiredEnvPrefix) {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}

// LoadDotEnv подхватывает переменные из файла .env, если он существует.
//
// Файл ищется от конфигурации, а не только в текущем каталоге: процесс могут
// запустить откуда угодно, а тесты — вообще из каталога пакета. Привязка
// к расположению конфигурации делает поиск предсказуемым.
//
// Каталогов три, в порядке убывания точности: рядом с самим файлом, на уровень
// выше и в текущем каталоге. Средний — не запас прочности, а рабочая раскладка:
// конфигурации лежат в config/, а .env с ключом — рядом с docker-compose.yml,
// то есть ровно на уровень выше.
//
// Уже установленные переменные окружения не перезаписываются: то, что задано
// явно при запуске, приоритетнее файла.
func LoadDotEnv(nearPath string) error {
	candidates := []string{dotenvPath}
	if nearPath != "" {
		dir := filepath.Dir(nearPath)
		candidates = []string{
			filepath.Join(dir, dotenvPath),
			filepath.Join(filepath.Dir(dir), dotenvPath),
			dotenvPath,
		}
	}

	for _, path := range candidates {
		if _, err := os.Stat(path); err != nil {
			continue // файла нет — штатная ситуация
		}
		if err := godotenv.Load(path); err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}
		return nil
	}
	return nil
}

// Load читает конфигурацию из значений по умолчанию и файла, применяет блок
// defaults к каждому символу и валидирует результат.
//
// Из окружения приходят ровно две вещи, и обе — не настройки стратегии:
// приватный ключ кошелька (STANDX_WALLET_PK) и режим работы (MM_MODE).
// Всё остальное описано в файле, и это единственное место, где его искать.
//
// Опечатка в файле — отказ загрузки, а не откат к значению по умолчанию:
// порог, который никто не выбирал, в бою стоит денег.
func Load(path string) (*Config, error) {
	if err := LoadDotEnv(path); err != nil {
		return nil, err
	}

	cfg, err := assemble(path)
	if err != nil {
		return nil, err
	}

	cfg.Exchange.WalletPrivateKey = secret.New(os.Getenv(walletKeyEnv))
	if mode := strings.TrimSpace(os.Getenv(modeEnv)); mode != "" {
		cfg.Runtime.Mode = Mode(mode)
	}

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}
	return cfg, nil
}

// baseKoanf собирает умолчания и файл поверх них.
func baseKoanf(path string) (*koanf.Koanf, error) {
	k := koanf.New(".")

	// Значения по умолчанию задают канонический набор ключей: то, чего файл
	// не упоминает, остаётся на умолчании, а не обнуляется.
	if err := k.Load(structs.Provider(*defaults(), "koanf"), nil); err != nil {
		return nil, fmt.Errorf("load defaults: %w", err)
	}
	if err := k.Load(file.Provider(path), yaml.Parser()); err != nil {
		return nil, fmt.Errorf("load config %s: %w", path, err)
	}
	return k, nil
}

// assemble декодирует конфигурацию из файла поверх значений по умолчанию.
func assemble(path string) (*Config, error) {
	k, err := baseKoanf(path)
	if err != nil {
		return nil, err
	}

	cfg := defaults()
	if err := unmarshal(k, "", cfg); err != nil {
		return nil, fmt.Errorf("decode config: %w", err)
	}

	// Каждый символ наследует блок defaults, затем накрывается своими полями.
	// Merge делается кодом, а не YAML-якорями: якорь копирует поддерево целиком
	// в момент разбора, и отличие в одном поле пришлось бы выписывать заново.
	resolved := make(map[string]Symbol, len(cfg.Symbols))
	for name := range cfg.Symbols {
		sym := cfg.Defaults
		if err := unmarshal(k, "symbols."+name, &sym); err != nil {
			return nil, fmt.Errorf("decode symbol %s: %w", name, err)
		}
		resolved[name] = sym
	}
	cfg.Symbols = resolved
	return cfg, nil
}

// unmarshal декодирует поддерево конфигурации в структуру, уже заполненную
// значениями по умолчанию: отсутствующие ключи оставляют текущее значение.
//
// ErrorUnused включён намеренно. Опечатка в имени параметра иначе молча вернула бы
// значение по умолчанию, и бот торговал бы не по тем порогам. Побочный полезный
// эффект: конфиг с приватным ключом в файле не пройдёт загрузку, потому что
// это поле помечено koanf:"-".
func unmarshal(k *koanf.Koanf, path string, out any) error {
	return k.UnmarshalWithConf(path, out, koanf.UnmarshalConf{
		Tag: "koanf",
		DecoderConfig: &mapstructure.DecoderConfig{
			DecodeHook:       mapstructure.StringToTimeDurationHookFunc(),
			WeaklyTypedInput: true,
			ErrorUnused:      true,
			Result:           out,
		},
	})
}

// EnabledSymbols возвращает только включённые инструменты.
func (c *Config) EnabledSymbols() map[market.Symbol]Symbol {
	out := make(map[market.Symbol]Symbol)
	for name, sym := range c.Symbols {
		if sym.Enabled {
			out[market.Symbol(name)] = sym
		}
	}
	return out
}

// IsLive сообщает, что торговые команды действительно уходят на биржу.
func (c *Config) IsLive() bool { return c.Runtime.Mode == ModeLive }
