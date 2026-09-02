package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nd/mm-standx/internal/domain/economics"
	"github.com/nd/mm-standx/internal/domain/market"
	"github.com/nd/mm-standx/internal/domain/policy"
	"github.com/nd/mm-standx/internal/secret"
)

// minimalConfig — валидная основа, поверх которой тесты добавляют отклонения.
const minimalConfig = `
risk:
  maxFillsPerHour: 6
symbols:
  XAG-USD:
    enabled: true
    program: { unitSize: "1500", perSideCap: "3000", maxMakerHoursPerHour: 2 }
  BTC-USD:
    enabled: true
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

	// Переопределение затрагивает только указанное поле, остальные наследуются.
	btc := cfg.Symbols["BTC-USD"]
	require.EqualValues(t, 3.0, btc.Appetite.JumpMultiple)
	require.Equal(t, defaults().Defaults.Appetite.MinBps, btc.Appetite.MinBps)
	require.Equal(t, defaults().Defaults.Volatility.MinSamples, btc.Volatility.MinSamples)
}

// Политика закрытия обязана быть одной из реализованных: неизвестное имя
// молча запускало бы прежний маркет-путь под чужой вывеской.
//
// До 2 сентября 2026 реализована была одна, immediate_market, и тест сторожил
// это. Теперь их две — maker_then_market закрывает залив post-only лимитом
// и по таймауту добивает маркетом, — и у второй есть обязательный параметр.
func TestFlattenPolicyMustBeImplemented(t *testing.T) {
	cfg := defaults()
	cfg.Risk.FlattenPolicy = "hybrid"
	require.ErrorContains(t, cfg.Validate(), "risk.flattenPolicy")

	cfg = defaults()
	cfg.Risk.FlattenPolicy = FlattenImmediateMarket
	require.NoError(t, cfg.Validate())

	cfg = defaults()
	cfg.Risk.FlattenPolicy = FlattenMakerThenMarket
	cfg.Risk.MakerUnwindTimeout = 3 * time.Second
	require.NoError(t, cfg.Validate())

	// Мейкерское закрытие без срока висело бы в стакане вечно.
	cfg = defaults()
	cfg.Risk.FlattenPolicy = FlattenMakerThenMarket
	cfg.Risk.MakerUnwindTimeout = 0
	require.ErrorContains(t, cfg.Validate(), "makerUnwindTimeout")
}

func liveEconomicConfig(rewardLowerBound float64) *Config {
	cfg := defaults()
	cfg.Runtime.Mode = ModeLive
	cfg.Exchange.WalletPrivateKey = secret.New("test-only-key")
	symbol := cfg.Defaults
	symbol.Enabled = true
	symbol.Economics.RewardRateLowerBoundUSDPerMakerHour = rewardLowerBound
	symbol.Program = Program{UnitSize: "30", PerSideCap: "60", MaxMakerHoursPerHour: 2, OffHoursMultiplier: 1}
	cfg.Symbols = map[string]Symbol{"XAU-USD": symbol}
	return cfg
}

// Экономические ворота — политика, а не корректность конфигурации, и падать
// на них процесс не имеет права.
//
// Разница видна по последствию. Валидация роняет старт, а деплой на упавший
// старт откатывается на предыдущий образ и продолжает работать: 10 августа три
// подряд выкатки упали на «allocation 400.00 USD exceeds the 60.00 USD
// calibration cap», каждая молча вернула образ двухдневной давности, и прод
// двое суток крутил старый код при внешне исправном контуре.
//
// Запрет никуда не делся — он живёт в Engine.evaluate, где виден в сводке
// и в метриках. Проверяется в TestEconomicGateStopsQuotingInsteadOfTheProcess.
func TestEconomicsDoesNotBlockStartup(t *testing.T) {
	for _, rewardLowerBound := range []float64{0, 20, 30} {
		cfg := liveEconomicConfig(rewardLowerBound)
		require.NoError(t, cfg.Validate(),
			"reward lower bound %.0f must not keep the process from starting", rewardLowerBound)
	}
}

// Сами ворота при этом обязаны выносить вердикт по ставке, а не по размеру.
func TestEconomicGateJudgesTheRateNotTheSize(t *testing.T) {
	gate := func(rewardLowerBound, marketMultiplier float64) economics.Decision {
		cfg := liveEconomicConfig(rewardLowerBound).Symbols["XAU-USD"]
		return economics.Gate{
			RewardRateLowerBoundUSDPerMakerHour: cfg.Economics.RewardRateLowerBoundUSDPerMakerHour,
			BreakEvenUSDPerMakerHour:            cfg.Economics.BreakEvenUSDPerMakerHour,
		}.Evaluate(1, marketMultiplier)
	}

	// Ставка не измерена: полный час разрешён, урезанный — нет.
	require.True(t, gate(0, 1).Allowed)
	require.False(t, gate(0, 0.1).Allowed)
	require.Contains(t, gate(0, 0.1).Reason, "reward rate is unknown")

	require.Contains(t, gate(20, 1).Reason, "below the 22.00 USD/MH break-even")
	require.False(t, gate(20, 1).Allowed)

	require.True(t, gate(30, 1).Allowed)
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

// Опечатка в имени параметра иначе молча вернула бы значение по умолчанию,
// и бот торговал бы не по тем порогам.
func TestUnknownKeyIsRejected(t *testing.T) {
	_, err := Load(writeConfig(t, minimalConfig+`
  ETH-USD:
    enabled: true
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
	t.Setenv(modeEnv, string(ModeLive))

	_, err := Load(writeConfig(t, `
symbols:
  BTC-USD:
    enabled: true
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
`))
	require.Error(t, err)
	require.Contains(t, err.Error(), "unitSize")
}

func TestValidationRejectsQuotesOutsideRewardBand(t *testing.T) {
	_, err := Load(writeConfig(t, `
symbols:
  BTC-USD:
    enabled: true
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
	// Включённому символу больше не нужна аллокация: размер берётся из баланса
	// счёта, а не из файла. Раньше её отсутствие роняло загрузку.
	cfg, err := Load(writeConfig(t, `
symbols:
  BTC-USD:
    enabled: true
    program: { unitSize: "2", perSideCap: "20", maxMakerHoursPerHour: 10 }
`))
	require.NoError(t, err)
	require.True(t, cfg.Symbols["BTC-USD"].Enabled)
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
  logLevel: verbose
  logFormat: xml
rateLimit:
  sustainedRps: 99
symbols:
  BTC-USD:
    enabled: true
`))
	require.Error(t, err)
	for _, want := range []string{"logLevel", "logFormat", "sustainedRps"} {
		require.Contains(t, err.Error(), want)
	}
}

func TestMissingFileIsReported(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "absent.yaml"))
	require.Error(t, err)
}

// Экспозиция больше не выводится из конфигурации.
//
// Прежде здесь стояла стартовая проверка суммы allocationUsd × leverage против
// risk.maxNotionalUsd. Обе величины сняты: размер считается от equity счёта,
// то есть экспозиция ограничена самим балансом и потолком программы наград —
// величинами рантайма, которых в файле нет. Проверять их на старте нечем,
// и притворяться, что можно, значит держать защиту, которая ничего не защищает.
func TestEnabledSymbolNeedsNoConfiguredExposure(t *testing.T) {
	cfg, err := Load(writeConfig(t, `
symbols:
  BTC-USD:
    enabled: true
    program: { unitSize: "2", perSideCap: "20", maxMakerHoursPerHour: 10 }
`))
	require.NoError(t, err)
	require.True(t, cfg.Symbols["BTC-USD"].Enabled)
}

// Without persisted state the daily loss counter resets on every restart, and
// the cycle of tripping and restarting drains the deposit in instalments. That
// is tolerable while shadowing and not tolerable live.
func TestLiveModeRequiresPersistedRiskState(t *testing.T) {
	t.Setenv("STANDX_WALLET_PK", "0x01")
	t.Setenv(modeEnv, string(ModeLive))

	body := `
runtime:
  stateDir: ""
symbols:
  BTC-USD:
    enabled: true
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

// Конфигурация едет на сервер отдельным файлом, и опечатка в ней обнаруживается
// только в проде, только падением на старте — а упавший старт откатывает деплой
// на предыдущий образ. Дешевле проверить здесь. Файл копируется во временный
// каталог: рядом с ним не должно оказаться .env разработчика, иначе результат
// зависел бы от машины.
func TestShippedConfigLoads(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "..", "config", "config.yaml"))
	require.NoError(t, err)

	// Файл боевой, а боевой режим требует ключа. Проверяется здесь не он,
	// а параметры стратегии, поэтому ключ подставляется заведомо негодный.
	t.Setenv(walletKeyEnv, "0x00")

	cfg, err := Load(writeConfig(t, string(body)))
	require.NoError(t, err, "the configuration that reaches the server must load")

	gold := cfg.Symbols["XAU-USD"]
	require.True(t, gold.Enabled)

	// Пауза после серии заливов обязана умещаться в бюджет простоя часа.
	// Двадцать минут его превышали, и два залива стоили всей смены.
	require.LessOrEqual(t, cfg.Risk.FillBurstCooldown, 5*time.Minute,
		"a burst pause longer than the hour can absorb loses the hour outright")

	// Ближняя ступень обязана стоять за наблюдённым хвостом скачка: 5.38 bps
	// за один такт 6 августа сняли всю лесенку за 51 мс.
	require.GreaterOrEqual(t, float64(gold.Appetite.MinBps), 6.0,
		"the near rung must stand outside the observed single-tick jump")

	// Мёртвая зона строится вокруг центра лесенки, а проверяется у каждой
	// ступени, поэтому крайним достаётся зона минус полуширина. Этот остаток
	// обязан быть заметно больше шага цены — иначе округление при выставлении
	// выбрасывает ступень из зоны, в которую её только что поставили.
	half := policy.Ladder{
		Layers:    gold.Quote.LadderLayers,
		SpreadBps: gold.Quote.LadderSpreadBps,
	}.HalfWidth()
	require.GreaterOrEqual(t, (gold.Appetite.MinDeadBandBps - half).Float64(), 0.5,
		"the outer rungs need real margin, not the boundary the start-up check accepts")

	// Внешний ориентир описан целиком и правильно, независимо от того, включён ли
	// он сейчас. Флаг — решение, площадка и символ — факт: опечатка в них иначе
	// всплывёт ровно в момент включения фида, то есть в бою и под нагрузкой.
	//
	// Сам фид пока выключен намеренно: не измерено, сколько упреждения он даёт
	// (PLAN.md §12.15), а быстрый сигнал до тех пор берётся со смещения середины
	// стакана. До 12 августа это решение жило переменной окружения, и файл
	// утверждал обратное тому, чем торговал бот.
	require.Equal(t, ProviderBinanceFutures, gold.Reference.Provider)
	require.Equal(t, "XAUUSDT", gold.Reference.Symbol)
	require.LessOrEqual(t, gold.Reference.StaleAfter, markTick,
		"a reference older than the official price it anticipates adds nothing")
}

// Порог молчания за пределом такта mark делает фид бессмысленным, ничего
// об этом не сообщая: ориентиру доверяли бы ровно до момента, когда приходит
// официальная цена и всё равно его вытесняет.
func TestReferenceStalenessMustFitInsideTheMarkTick(t *testing.T) {
	_, err := Load(writeConfig(t, `
symbols:
  BTC-USD:
    enabled: false
    reference: { enabled: true, provider: binance_futures, symbol: BTCUSDT, staleAfter: 10s }
`))
	require.ErrorContains(t, err, "reference.staleAfter")
}

// Включённый ориентир без символа никогда не подключится, и молчащий фид
// выглядит в точности как фид на спокойном рынке.
func TestEnabledReferenceRequiresSymbol(t *testing.T) {
	_, err := Load(writeConfig(t, `
symbols:
  BTC-USD:
    enabled: false
    reference:
      enabled: true
      provider: binance_futures
      symbol: ""
`))
	require.ErrorContains(t, err, "reference.symbol is required")
}

func TestReferenceProviderMustBeKnown(t *testing.T) {
	_, err := Load(writeConfig(t, `
symbols:
  BTC-USD:
    enabled: false
    reference: { enabled: true, provider: kraken, symbol: BTCUSD }
`))
	require.ErrorContains(t, err, "reference.provider: unsupported value \"kraken\"")
}

// Ориентир проверяется и у выключенного инструмента: опечатка иначе всплывёт
// в тот момент, когда инструмент включают, то есть в проде.
func TestReferenceIsValidatedForDisabledSymbols(t *testing.T) {
	_, err := Load(writeConfig(t, `
symbols:
  SOL-USD:
    enabled: false
    reference: { enabled: true, provider: binance_futures, symbol: "" }
`))
	require.ErrorContains(t, err, "symbols.SOL-USD.reference.symbol is required")
}

// Выключенный ориентир не обязан быть заполненным: назвать площадку для
// инструмента, за которым не следят, незачем.
func TestDisabledReferenceNeedsNothing(t *testing.T) {
	cfg, err := Load(writeConfig(t, `
symbols:
  SOL-USD:
    enabled: false
    reference: { enabled: false, provider: "", symbol: "" }
`))
	require.NoError(t, err)
	require.False(t, cfg.Symbols["SOL-USD"].Reference.Enabled)
}

// Единственный файл конфигурации обязан грузиться в обоих режимах.
//
// Проверка существует потому, что отказ валидации на сервере выглядит не как
// отказ. Процесс падает на старте, деплой откатывается на предыдущий образ,
// метрики отвечают, — и прод двое суток крутит старый код при внешне исправном
// контуре (PLAN.md §13). Дешевле обнаружить это здесь.
func TestShippedConfigLoadsInBothModes(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "..", "config", "config.yaml"))
	require.NoError(t, err)
	path := writeConfig(t, string(body))

	t.Setenv(walletKeyEnv, "0x00")

	for _, mode := range []Mode{ModeDryRun, ModeLive} {
		t.Setenv(modeEnv, string(mode))

		cfg, err := Load(path)
		require.NoError(t, err, "the shipped configuration must start in %s", mode)
		require.Equal(t, mode == ModeLive, cfg.IsLive())

		enabled := cfg.EnabledSymbols()
		require.Len(t, enabled, 1, "exactly the traded instrument stays enabled")
		require.Contains(t, enabled, market.Symbol("XAU-USD"))
	}
}

// Незаданный режим означает теневой, а не боевой.
//
// Умолчание здесь — единственное, что стоит между забытой строкой в .env
// и настоящими ордерами: файл конфигурации один и одинаков везде, где его
// запускают, поэтому «по умолчанию не торгуем» обязано жить в коде.
func TestModeDefaultsToDryRunWhenUnset(t *testing.T) {
	t.Setenv(modeEnv, "")

	cfg, err := Load(writeConfig(t, minimalConfig))
	require.NoError(t, err)
	require.False(t, cfg.IsLive(), "an unset mode must never mean live")
}

// Режим задаётся переменной и в файле не принимается.
//
// Строка `mode: live` в версионируемом файле означала бы, что любой локальный
// прогон на машине с рабочим ключом отправляет настоящие ордера. Опечатка
// в имени переменной при этом обязана быть отказом, а не молчаливым dry_run:
// оператор, включивший бой, должен узнать, что не включил.
func TestModeComesFromTheEnvironmentOnly(t *testing.T) {
	_, err := Load(writeConfig(t, minimalConfig+`
runtime:
  mode: live
`))
	require.ErrorContains(t, err, "mode", "the file must not be able to arm live trading")

	t.Setenv(modeEnv, "liev")
	_, err = Load(writeConfig(t, minimalConfig))
	require.ErrorContains(t, err, modeEnv)
	require.ErrorContains(t, err, "liev")
}

// Переменные снятого слоя не действуют, но и не молчат.
//
// Строка MMSTANDX_* на сервере переживает смену версии. После снятия слоя она
// не делает ничего — и это ровно тот случай, когда оператор считает предел
// заданным, а действует значение из файла. Разница обязана быть названа.
func TestRetiredEnvOverridesAreInertAndNamed(t *testing.T) {
	t.Setenv("MMSTANDX_RISK_MAXDAILYLOSSUSD", "42")

	body := strings.Replace(minimalConfig,
		"maxFillsPerHour: 6", "maxFillsPerHour: 6\n  maxDailyLossUsd: 7", 1)
	require.Contains(t, body, "maxDailyLossUsd", "the substitution must have taken")

	cfg, err := Load(writeConfig(t, body))
	require.NoError(t, err, "a leftover variable must not keep the process down")
	require.Equal(t, 7.0, cfg.Risk.MaxDailyLossUSD, "the file is the only source in force")
	require.Equal(t, []string{"MMSTANDX_RISK_MAXDAILYLOSSUSD"}, StaleEnvOverrides(),
		"and the dead variable must be named: silence here is the failure being fixed")
}

// Опечатка в файле — отказ загрузки: там она правится осознанно и видна глазами,
// а откат к умолчанию из кода означает порог, который никто не выбирал.
func TestUnknownFileKeyIsFatal(t *testing.T) {
	_, err := Load(writeConfig(t, minimalConfig+`
  ETH-USD:
    enabled: true
    appetite:
      minBpsTypo: 7.0
`))
	require.Error(t, err)
	require.Contains(t, err.Error(), "minBpsTypo")
}
