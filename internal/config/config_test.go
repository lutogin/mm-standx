package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nd/mm-standx/internal/domain/market"
	"github.com/nd/mm-standx/internal/secret"
)

// minimalConfig — валидная основа, поверх которой тесты добавляют отклонения.
const minimalConfig = `
runtime:
  mode: dry_run
risk:
  # 70k выделенных средств при плече 4 дают 280k нотионала — общий лимит
  # обязан их вмещать, иначе конфигурация отвергается на старте.
  maxNotionalUsd: 300000
symbols:
  XAG-USD:
    enabled: true
    allocationUsd: 40000
    program: { unitSize: "1500", perSideCap: "3000", maxMakerHoursPerHour: 2 }
  BTC-USD:
    enabled: true
    allocationUsd: 30000
    program: { unitSize: "2", perSideCap: "20", maxMakerHoursPerHour: 10 }
    appetite:
      jumpMultiple: 3.0
`

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	return path
}

func TestLoadAppliesDefaultsToSymbols(t *testing.T) {
	cfg, err := Load(writeConfig(t, minimalConfig))
	require.NoError(t, err)

	// Символ, не переопределивший ничего, получает весь блок defaults.
	silver := cfg.Symbols["XAG-USD"]
	require.Equal(t, defaults().Defaults.Appetite.JumpMultiple, silver.Appetite.JumpMultiple)
	require.Equal(t, defaults().Defaults.Quote.LadderLayers, silver.Quote.LadderLayers)
	require.Equal(t, 40000.0, silver.AllocationUSD)

	// Переопределение затрагивает только указанное поле, остальные наследуются.
	btc := cfg.Symbols["BTC-USD"]
	require.EqualValues(t, 3.0, btc.Appetite.JumpMultiple)
	require.Equal(t, defaults().Defaults.Appetite.MinBps, btc.Appetite.MinBps)
	require.Equal(t, defaults().Defaults.Volatility.MinSamples, btc.Volatility.MinSamples)
}

// Advertising an unimplemented flatten policy is unsafe: the process would
// start successfully and still execute the immediate-market path. Reject such
// a configuration until the alternative policy has an implementation.
func TestOnlyImplementedFlattenPolicyIsAccepted(t *testing.T) {
	cfg := defaults()
	cfg.Risk.FlattenPolicy = "hybrid"

	require.ErrorContains(t, cfg.Validate(), "only \"immediate_market\" is implemented")
}

func liveEconomicConfig(allocation, rewardLowerBound float64) *Config {
	cfg := defaults()
	cfg.Runtime.Mode = ModeLive
	cfg.Exchange.WalletPrivateKey = secret.New("test-only-key")
	symbol := cfg.Defaults
	symbol.Enabled = true
	symbol.AllocationUSD = allocation
	symbol.Economics.RewardRateLowerBoundUSDPerMakerHour = rewardLowerBound
	symbol.Program = Program{UnitSize: "30", PerSideCap: "60", MaxMakerHoursPerHour: 2, OffHoursMultiplier: 1}
	cfg.Symbols = map[string]Symbol{"XAU-USD": symbol}
	return cfg
}

func TestLiveScalingRequiresARewardLowerBoundAboveBreakEven(t *testing.T) {
	unknown := liveEconomicConfig(1_000, 0)
	require.ErrorContains(t, unknown.Validate(), "exceeds the 60.00 USD calibration cap")

	unprofitable := liveEconomicConfig(1_000, 20)
	require.ErrorContains(t, unprofitable.Validate(), "below the 22.00 USD/MH break-even")

	profitable := liveEconomicConfig(1_000, 30)
	require.NoError(t, profitable.Validate())
}

func TestEnabledSymbols(t *testing.T) {
	cfg, err := Load(writeConfig(t, minimalConfig+`
  ETH-USD:
    enabled: false
`))
	require.NoError(t, err)

	enabled := cfg.EnabledSymbols()
	require.Len(t, enabled, 2)
	require.Contains(t, enabled, market.Symbol("XAG-USD"))
	require.Contains(t, enabled, market.Symbol("BTC-USD"))
	require.NotContains(t, enabled, market.Symbol("ETH-USD"))
}

// Имена переменных окружения пишутся в верхнем регистре, а ключи конфигурации —
// в camelCase. Без разрешения регистра переопределение молча не срабатывало бы.
func TestEnvOverrideResolvesCamelCaseKeys(t *testing.T) {
	t.Setenv("MMSTANDX_RUNTIME_STATSINTERVAL", "6s")
	t.Setenv("MMSTANDX_RATELIMIT_SUSTAINEDRPS", "9")
	t.Setenv("MMSTANDX_HEALTH_MARKSTALE", "12s")

	cfg, err := Load(writeConfig(t, minimalConfig))
	require.NoError(t, err)

	require.Equal(t, 6*time.Second, cfg.Runtime.StatsInterval)
	require.Equal(t, 9.0, cfg.RateLimit.SustainedRPS)
	require.Equal(t, 12*time.Second, cfg.Health.MarkStale)
}

func TestEnvOverrideReachesSymbolFields(t *testing.T) {
	t.Setenv("MMSTANDX_SYMBOLS_BTC-USD_ALLOCATIONUSD", "12345")

	cfg, err := Load(writeConfig(t, minimalConfig))
	require.NoError(t, err)
	require.Equal(t, 12345.0, cfg.Symbols["BTC-USD"].AllocationUSD)
}

// Опечатка в имени параметра иначе молча вернула бы значение по умолчанию,
// и бот торговал бы не по тем порогам.
func TestUnknownKeyIsRejected(t *testing.T) {
	_, err := Load(writeConfig(t, minimalConfig+`
  ETH-USD:
    enabled: true
    allocationUsd: 1000
    quote:
      targetBpsTypo: 7.0
`))
	require.Error(t, err)
	require.Contains(t, err.Error(), "targetBpsTypo")
}

// Приватный ключ читается только из окружения. Поле помечено koanf:"-",
// поэтому попытка задать его в файле упирается в проверку неизвестных ключей.
func TestPrivateKeyInFileIsRejected(t *testing.T) {
	_, err := Load(writeConfig(t, minimalConfig+`
exchange:
  walletPrivateKey: "0xdeadbeef"
`))
	require.Error(t, err)
}

func TestWalletKeyComesFromEnvironment(t *testing.T) {
	t.Setenv("STANDX_WALLET_PK", "0x4646464646464646464646464646464646464646464646464646464646464646")

	cfg, err := Load(writeConfig(t, minimalConfig))
	require.NoError(t, err)
	require.False(t, cfg.Exchange.WalletPrivateKey.IsEmpty())
	// Секрет не должен печататься ни при каком способе вывода.
	require.Equal(t, "[REDACTED]", cfg.Exchange.WalletPrivateKey.String())
}

func TestLiveModeRequiresWalletKey(t *testing.T) {
	_, err := Load(writeConfig(t, `
runtime:
  mode: live
symbols:
  BTC-USD:
    enabled: true
    allocationUsd: 1000
`))
	require.Error(t, err)
	require.Contains(t, err.Error(), "STANDX_WALLET_PK")
}

// Границы должны оставлять правилу вычисления дистанции место для манёвра.
func TestValidationRejectsMisorderedBounds(t *testing.T) {
	_, err := Load(writeConfig(t, `
symbols:
  BTC-USD:
    enabled: true
    allocationUsd: 1000
    program: { unitSize: "2", perSideCap: "20", maxMakerHoursPerHour: 10 }
    appetite:
      minBps: 9.0
      maxBps: 4.0
`))
	require.Error(t, err)
	require.Contains(t, err.Error(), "minBps < maxBps")
}

// Параметры начисления публикуются биржей и не имеют разумных умолчаний:
// включённый символ без них считал бы награду по нулевой единице.
func TestEnabledSymbolRequiresProgrammeParameters(t *testing.T) {
	_, err := Load(writeConfig(t, `
symbols:
  BTC-USD:
    enabled: true
    allocationUsd: 1000
`))
	require.Error(t, err)
	require.Contains(t, err.Error(), "unitSize")
}

func TestValidationRejectsQuotesOutsideRewardBand(t *testing.T) {
	_, err := Load(writeConfig(t, `
symbols:
  BTC-USD:
    enabled: true
    allocationUsd: 1000
    program: { unitSize: "2", perSideCap: "20", maxMakerHoursPerHour: 10 }
    appetite:
      maxBps: 12.0
`))
	require.Error(t, err)
	require.Contains(t, err.Error(), "bandBps")
}

// Зрелость оценки не может наступать раньше готовности: иначе поправка
// на незрелость никогда не применяется.
func TestValidationRejectsInvertedConfidenceThresholds(t *testing.T) {
	_, err := Load(writeConfig(t, minimalConfig+`
defaults:
  volatility:
    minSamples: 200
    fullConfidenceAt: 50
`))
	require.Error(t, err)
	require.Contains(t, err.Error(), "fullConfidenceAt")
}

func TestValidationRejectsEnabledSymbolWithoutAllocation(t *testing.T) {
	_, err := Load(writeConfig(t, `
symbols:
  BTC-USD:
    enabled: true
`))
	require.Error(t, err)
	require.Contains(t, err.Error(), "allocationUsd")
}

func TestValidationRejectsRateLimitAboveExchangeCeiling(t *testing.T) {
	_, err := Load(writeConfig(t, minimalConfig+`
rateLimit:
  sustainedRps: 40
`))
	require.Error(t, err)
	require.Contains(t, err.Error(), "sustainedRps")
}

// Ошибки собираются целиком, а не по одной за запуск.
func TestValidationReportsAllProblemsAtOnce(t *testing.T) {
	_, err := Load(writeConfig(t, `
runtime:
  mode: dry_run
  logLevel: verbose
  logFormat: xml
rateLimit:
  sustainedRps: 99
symbols:
  BTC-USD:
    enabled: true
`))
	require.Error(t, err)
	for _, want := range []string{"logLevel", "logFormat", "sustainedRps", "allocationUsd"} {
		require.Contains(t, err.Error(), want)
	}
}

func TestMissingFileIsReported(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "absent.yaml"))
	require.Error(t, err)
}

// The aggregate notional limit is a config-time invariant, not a runtime one:
// allocations are static, so the ceiling is known before the first order. There
// is no point discovering a known-in-advance mistake once money is in the market.
func TestAggregateNotionalMustFitTheRiskLimit(t *testing.T) {
	_, err := Load(writeConfig(t, `
runtime:
  mode: dry_run
risk:
  maxNotionalUsd: 1000
symbols:
  BTC-USD:
    enabled: true
    allocationUsd: 500
    program: { unitSize: "2", perSideCap: "20", maxMakerHoursPerHour: 10 }
`))
	require.ErrorContains(t, err, "risk.maxNotionalUsd")
	require.ErrorContains(t, err, "2000 USD of notional")
}

// Without persisted state the daily loss counter resets on every restart, and
// the cycle of tripping and restarting drains the deposit in instalments. That
// is tolerable while shadowing and not tolerable live.
func TestLiveModeRequiresPersistedRiskState(t *testing.T) {
	t.Setenv("STANDX_WALLET_PK", "0x01")

	body := `
runtime:
  mode: live
  stateDir: ""
symbols:
  BTC-USD:
    enabled: true
    allocationUsd: 60
    program: { unitSize: "2", perSideCap: "20", maxMakerHoursPerHour: 10 }
`
	_, err := Load(writeConfig(t, body))
	require.ErrorContains(t, err, "runtime.stateDir is required in live mode")

	_, err = Load(writeConfig(t, strings.Replace(body, `stateDir: ""`, `stateDir: data/state`, 1)))
	require.NoError(t, err)
}

// Memory shorter than the observation window could never bind the estimate: it
// would fade before the window forgot the same jump.
func TestMemoryHalfLifeMustOutliveTheWindow(t *testing.T) {
	_, err := Load(writeConfig(t, minimalConfig+`
defaults:
  volatility:
    window: 20m
    memoryHalfLife: 5m
`))
	require.ErrorContains(t, err, "can never bind the estimate")
}

// One file ships to the server; what to trade is decided at launch. The list
// only selects among instruments described in the file — programme parameters
// and allocation are published by the exchange and chosen deliberately, not
// set by an environment variable.
func TestRuntimeSymbolsSelectsWhatToTrade(t *testing.T) {
	t.Setenv("MMSTANDX_RUNTIME_SYMBOLS", "BTC-USD")

	cfg, err := Load(writeConfig(t, minimalConfig))
	require.NoError(t, err)

	enabled := cfg.EnabledSymbols()
	require.Len(t, enabled, 1)
	require.Contains(t, enabled, market.Symbol("BTC-USD"))
	require.NotContains(t, enabled, market.Symbol("XAG-USD"),
		"the selection overrides enabled: true from the file")
}

// A typo must not start a bot with no instruments at all: silence is the one
// failure mode that looks exactly like working correctly.
func TestUnknownSymbolInSelectionIsRejected(t *testing.T) {
	t.Setenv("MMSTANDX_RUNTIME_SYMBOLS", "XAU-USB")

	_, err := Load(writeConfig(t, minimalConfig))
	require.ErrorContains(t, err, "XAU-USB")
	require.ErrorContains(t, err, "known:")
}

// Selecting an instrument the file left disabled is the point of the knob, but
// it still has to be configured well enough to trade.
func TestSelectionStillRequiresValidSymbolConfig(t *testing.T) {
	body := minimalConfig + `
  ETH-USD:
    enabled: false
    program: { unitSize: "60", perSideCap: "600", maxMakerHoursPerHour: 10 }
`
	t.Setenv("MMSTANDX_RUNTIME_SYMBOLS", "ETH-USD")
	_, err := Load(writeConfig(t, body))
	require.ErrorContains(t, err, "allocationUsd")
}
