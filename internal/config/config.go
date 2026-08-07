// Package config загружает и валидирует конфигурацию бота.
//
// Источники в порядке возрастания приоритета: значения по умолчанию, YAML-файл,
// переменные окружения с префиксом MMSTANDX_. Секреты берутся только из окружения.
//
// Валидация строгая и происходит на старте: неверно упорядоченные пороги дистанций
// или отсутствующая аллокация — это ошибки, которые в бою стоят денег, поэтому
// процесс не должен подниматься с некорректным конфигом.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/go-viper/mapstructure/v2"
	"github.com/joho/godotenv"
	"github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/env"
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
	Defaults  Symbol            `koanf:"defaults"`
	Symbols   map[string]Symbol `koanf:"symbols"`
}

// Runtime — параметры процесса.
type Runtime struct {
	Mode          Mode          `koanf:"mode"`
	LogLevel      string        `koanf:"logLevel"`
	LogFormat     string        `koanf:"logFormat"` // json | text
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
	// Symbols — какие инструменты торговать, поверх флагов enabled в файле.
	//
	// Существует ради окружения: MMSTANDX_RUNTIME_SYMBOLS=XAU-USD выбирает
	// инструмент, не трогая конфигурацию. Один и тот же файл едет на сервер,
	// а решение о том, что торговать, принимается при запуске.
	//
	// Список только отбирает из описанных в файле и не может добавить
	// неизвестный инструмент: параметры начисления и аллокация публикуются
	// биржей и подбираются осознанно, а не задаются переменной среды.
	Symbols []string `koanf:"symbols"`
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
	MaxDailyLossUSD      float64            `koanf:"maxDailyLossUsd"`
	MaxNotionalUSD       float64            `koanf:"maxNotionalUsd"`
	MaxFillsPerHour      int                `koanf:"maxFillsPerHour"`
	MarginMode           trading.MarginMode `koanf:"marginMode"`
	FlattenPolicy        string             `koanf:"flattenPolicy"`
	FlattenDebounce      time.Duration      `koanf:"flattenDebounce"`
	PostFillCooldown     time.Duration      `koanf:"postFillCooldown"`
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
	MarkStale         time.Duration `koanf:"markStale"`
	ReconcileInterval time.Duration `koanf:"reconcileInterval"`
	AckTimeout        time.Duration `koanf:"ackTimeout"`
	HeartbeatInterval time.Duration `koanf:"heartbeatInterval"`
}

// Symbol — настройки котирования одного инструмента.
type Symbol struct {
	Enabled       bool       `koanf:"enabled"`
	AllocationUSD float64    `koanf:"allocationUsd"`
	Quote         Quote      `koanf:"quote"`
	Appetite      Appetite   `koanf:"appetite"`
	Volatility    Volatility `koanf:"volatility"`
	Program       Program    `koanf:"program"`
	Uptime        Uptime     `koanf:"uptime"`
	SymbolRisk    SymbolRisk `koanf:"risk"`
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
	Leverage           int    `koanf:"leverage"`
	MaxNetPositionBase string `koanf:"maxNetPositionBase"`
}

const (
	// envPrefix — префикс переменных окружения, переопределяющих конфигурацию.
	envPrefix = "MMSTANDX_"
	// dotenvPath — файл с секретами, подхватываемый при запуске.
	dotenvPath = ".env"
	// walletKeyEnv — переменная с приватным ключом кошелька.
	walletKeyEnv = "STANDX_WALLET_PK"
)

// LoadDotEnv подхватывает переменные из файла .env, если он существует.
//
// Файл ищется рядом с конфигурацией, а не в текущем каталоге: процесс могут
// запустить откуда угодно, а тесты — вообще из каталога пакета. Привязка
// к расположению конфигурации делает поиск предсказуемым.
//
// Уже установленные переменные окружения не перезаписываются: то, что задано
// явно при запуске, приоритетнее файла.
func LoadDotEnv(nearPath string) error {
	candidates := []string{filepath.Join(filepath.Dir(nearPath), dotenvPath)}
	if nearPath == "" {
		candidates = []string{dotenvPath}
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

// Load читает конфигурацию из значений по умолчанию, файла и окружения,
// применяет блок defaults к каждому символу и валидирует результат.
func Load(path string) (*Config, error) {
	if err := LoadDotEnv(path); err != nil {
		return nil, err
	}

	k := koanf.New(".")

	// Значения по умолчанию задают канонический набор ключей с правильным
	// регистром — по нему разрешаются переменные окружения.
	if err := k.Load(structs.Provider(*defaults(), "koanf"), nil); err != nil {
		return nil, fmt.Errorf("load defaults: %w", err)
	}
	if err := k.Load(file.Provider(path), yaml.Parser()); err != nil {
		return nil, fmt.Errorf("load config %s: %w", path, err)
	}
	if err := k.Load(env.Provider(envPrefix, ".", envKeyResolver(k)), nil); err != nil {
		return nil, fmt.Errorf("load env overrides: %w", err)
	}

	cfg := defaults()
	if err := unmarshal(k, "", cfg); err != nil {
		return nil, fmt.Errorf("decode config: %w", err)
	}

	// Каждый символ наследует блок defaults, затем накрывается своими полями.
	// Merge делается кодом, а не YAML-якорями: якоря не переживают env-override.
	resolved := make(map[string]Symbol, len(cfg.Symbols))
	for name := range cfg.Symbols {
		sym := cfg.Defaults
		if err := unmarshal(k, "symbols."+name, &sym); err != nil {
			return nil, fmt.Errorf("decode symbol %s: %w", name, err)
		}
		resolved[name] = sym
	}
	cfg.Symbols = resolved
	if err := cfg.applySymbolSelection(); err != nil {
		return nil, err
	}

	cfg.Exchange.WalletPrivateKey = secret.New(os.Getenv(walletKeyEnv))

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}
	return cfg, nil
}

// envKeyResolver сопоставляет переменную окружения с ключом конфигурации.
//
// Имена переменных пишутся в верхнем регистре с подчёркиваниями, а ключи
// конфигурации — в camelCase, поэтому прямое преобразование их не находит:
// MMSTANDX_RUNTIME_STATSINTERVAL никогда не совпало бы с runtime.statsInterval.
// Резолвер строит индекс уже известных ключей и ищет в нём без учёта регистра.
//
// Не найденное имя возвращается как есть: тогда неизвестный ключ дойдёт до
// декодера и будет отвергнут, а не проигнорирован молча.
func envKeyResolver(k *koanf.Koanf) func(string) string {
	index := make(map[string]string)
	for _, key := range k.Keys() {
		index[strings.ToLower(strings.ReplaceAll(key, ".", "_"))] = key
	}

	return func(name string) string {
		trimmed := strings.ToLower(strings.TrimPrefix(name, envPrefix))
		if key, ok := index[trimmed]; ok {
			return key
		}
		return strings.ReplaceAll(trimmed, "_", ".")
	}
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

// applySymbolSelection сужает набор инструментов до перечисленных в runtime.symbols.
//
// Неизвестное имя — ошибка, а не пустой результат: молча запустить бота вообще
// без инструментов из-за опечатки в переменной среды хуже, чем не запустить.
func (c *Config) applySymbolSelection() error {
	if len(c.Runtime.Symbols) == 0 {
		return nil
	}

	wanted := make(map[string]bool, len(c.Runtime.Symbols))
	for _, name := range c.Runtime.Symbols {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if _, ok := c.Symbols[name]; !ok {
			known := make([]string, 0, len(c.Symbols))
			for s := range c.Symbols {
				known = append(known, s)
			}
			sort.Strings(known)
			return fmt.Errorf("runtime.symbols: %q is not described in the configuration; known: %s",
				name, strings.Join(known, ", "))
		}
		wanted[name] = true
	}

	for name, sym := range c.Symbols {
		sym.Enabled = wanted[name]
		c.Symbols[name] = sym
	}
	return nil
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
