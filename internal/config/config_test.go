package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nd/mm-standx/internal/domain/market"
)

// minimalConfig — валидная основа, поверх которой тесты добавляют отклонения.
const minimalConfig = `
runtime:
  mode: dry_run
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
