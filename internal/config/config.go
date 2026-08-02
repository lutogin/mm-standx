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
}

// Health — пороги контроля живости.
type Health struct {
	MarkStale         time.Duration `koanf:"markStale"`
	DisconnectPull    time.Duration `koanf:"disconnectPull"`
	ReconcileInterval time.Duration `koanf:"reconcileInterval"`
	AckTimeout        time.Duration `koanf:"ackTimeout"`
	HeartbeatInterval time.Duration `koanf:"heartbeatInterval"`
}

// Symbol — настройки котирования одного инструмента.
type Symbol struct {
	Enabled       bool       `koanf:"enabled"`
	AllocationUSD float64    `koanf:"allocationUsd"`
	Quote         Quote      `koanf:"quote"`
	Volatility    Volatility `koanf:"volatility"`
	Uptime        Uptime     `koanf:"uptime"`
	SymbolRisk    SymbolRisk `koanf:"risk"`
}

// Quote — геометрия котировок и гистерезис.
//
// Пороги обязаны быть упорядочены: pull < panic < minKeep < target < maxKeep ≤ band.
// Порядок проверяется при валидации, см. PLAN.md §4.1.
type Quote struct {
	TargetBps       money.Bps     `koanf:"targetBps"`
	MinKeepBps      money.Bps     `koanf:"minKeepBps"`
	MaxKeepBps      money.Bps     `koanf:"maxKeepBps"`
	PanicBps        money.Bps     `koanf:"panicBps"`
	PullBps         money.Bps     `koanf:"pullBps"`
	LadderLayers    int           `koanf:"ladderLayers"`
	LadderSpreadBps money.Bps     `koanf:"ladderSpreadBps"`
	MinRest         time.Duration `koanf:"minRest"`
	RequoteCooldown time.Duration `koanf:"requoteCooldown"`
}

// Volatility — параметры адаптации к волатильности и аварийного съёма.
type Volatility struct {
	EWMAHalfLife  time.Duration `koanf:"ewmaHalfLife"`
	WidenSigmaBps money.Bps     `koanf:"widenSigmaBps"`
	PullSigmaBps  money.Bps     `koanf:"pullSigmaBps"`
	JumpBps       money.Bps     `koanf:"jumpBps"`
	WidenFactorK  float64       `koanf:"widenFactorK"`
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
