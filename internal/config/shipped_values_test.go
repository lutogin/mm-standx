package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nd/mm-standx/internal/domain/economics"
	"github.com/nd/mm-standx/internal/domain/reward"
)

// shippedConfig — тот самый файл, который едет на сервер.
func shippedConfig(t *testing.T) *Config {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("..", "..", "config", "config.yaml"))
	require.NoError(t, err)
	t.Setenv(walletKeyEnv, "0x00")
	cfg, err := Load(writeConfig(t, string(body)))
	require.NoError(t, err)
	return cfg
}

// Кривая веса — не вопрос вкуса, и её нельзя тихо вернуть к прежнему значению.
//
// Страница кампании StandX объявляет max_weight = 200% и приводит формулу,
// дающую те же 200%, но одна строка её таблицы (5 bps → 80%) отвечает кривой
// со 120%. Здесь стояло 120, то есть выбрана была таблица против формулы.
//
// Спор решён замером, а не чтением. 18 августа 2026 лесенка стояла тремя
// ступенями по 27.276 на 7.0 / 7.4 / 7.8 bps:
//
//	при 200:  0.880 + 0.816 + 0.752 = 2.448  →  66.8
//	при 120:  0.640 + 0.608 + 0.576 = 1.824  →  49.8
//
// Отчёт биржи за часы 07-11 UTC: P70 = 67.8…72.2. Кривая 200 сходится,
// кривая 120 промахивается в полтора раза.
//
// Цена ошибки не в отчётности, а в размере: sizing.usefulCeiling делит
// перекрытие на вес, и заниженный вес заставлял держать на стороне примерно
// на 20% больше, чем зачёт способен принять. Излишек не приносил ничего,
// нёс полный риск исполнения и съедал маржу — 1449 отказов «balance not
// enough» за сутки.
//
// МЕНЯТЬ ТОЛЬКО ВМЕСТЕ С НОВЫМ ОТЧЁТОМ БИРЖИ, подтверждающим другую кривую.
func TestShippedRewardCurveFollowsTheMeasuredWeights(t *testing.T) {
	cfg := shippedConfig(t)

	require.EqualValues(t, 200, cfg.Reward.MaxWeightPct,
		"замеренная кривая, а не строка из таблицы документации")
	require.EqualValues(t, 40, cfg.Reward.MinWeightPct)
	require.EqualValues(t, 10, cfg.Reward.BandBps)

	// Сама граница, на которой держится решение. Ни один аптайм и ни одно
	// расположение ступеней не поднимут взвешенную сторону выше, чем
	// наибольший планировавшийся размер, умноженный на вес ближайшей
	// поставленной ступени.
	curve := reward.Curve{
		BandBps:      cfg.Reward.BandBps,
		MaxWeightPct: cfg.Reward.MaxWeightPct,
		MinWeightPct: cfg.Reward.MinWeightPct,
	}
	const (
		maxPlannedPerSide = 87.402 // максимум per_side за часы 09-12 UTC
		nearestRungBps    = 7.0    // ближайшая когда-либо поставленная ступень
		highestP70        = 72.20  // наибольший P70 из отчёта за те же часы
		lowestP70         = 68.80  // наименьший P70 из отчёта за те же часы
	)
	ceiling := maxPlannedPerSide * curve.Weight(nearestRungBps)

	// Начисление считается по P70, и настроенная кривая обязана быть способна
	// его дать. 87.402 x 0.88 = 76.91 против наибольшего наблюдённого 72.20.
	require.GreaterOrEqual(t, ceiling, highestP70,
		"кривая обязана быть способна дать опубликованный биржей зачёт")

	// Отвергнутая альтернатива названа явно, чтобы её нельзя было вернуть
	// молча: на кривой со 120% потолок равен 87.402 x 0.64 = 55.94, то есть
	// ниже даже наименьшего наблюдённого P70. Это и есть всё доказательство.
	rejected := reward.Curve{BandBps: 10, MaxWeightPct: 120, MinWeightPct: 40}
	require.Less(t, maxPlannedPerSide*rejected.Weight(nearestRungBps), lowestP70,
		"кривая со 120% не даёт наблюдённого зачёта ни при каком аптайме")
}

// Ставка пула измерена, и шлюз обязан это знать: пока здесь стоял ноль,
// MinimumTierMultiplier возвращал единицу и тир 0.5x объявлялся неокупаемым
// до всякого замера — отказ от половины дохода в часах, где полный тир уже
// недостижим.
func TestShippedEconomicsUsesTheMeasuredPoolRate(t *testing.T) {
	cfg := shippedConfig(t)
	gold := cfg.Symbols["XAU-USD"]

	require.Greater(t, gold.Economics.RewardRateLowerBoundUSDPerMakerHour, 0.0,
		"калибровка закончена: шесть закрытых дней дают 0.79…0.86 DUSD за Maker Hour")
	require.LessOrEqual(t, gold.Economics.RewardRateLowerBoundUSDPerMakerHour, 0.789,
		"нижняя граница наблюдений, а не среднее: шлюз обязан ошибаться в отказ")

	// Порог окупаемости обязан пропускать половинный тир и отвергать
	// внесессионный множитель. Это и есть всё различие, ради которого
	// шлюз существует.
	gate := economics.Gate{
		RewardRateLowerBoundUSDPerMakerHour: gold.Economics.RewardRateLowerBoundUSDPerMakerHour,
		BreakEvenUSDPerMakerHour:            gold.Economics.BreakEvenUSDPerMakerHour,
	}
	require.True(t, gate.Evaluate(1.0, 1.0).Allowed, "полный тир в сессии")
	require.True(t, gate.Evaluate(0.5, 1.0).Allowed,
		"половинный тир платит вдвое меньше при том же риске — но платит")
	require.False(t, gate.Evaluate(1.0, 0.1).Allowed,
		"десятая доля начисления при неизменном риске не окупается")
}

// Удержание при молчащем канале mark обязано быть включённым и обязано
// оставаться внутри полосы награды.
func TestShippedStaleMarkHoldIsInsideTheRewardBand(t *testing.T) {
	cfg := shippedConfig(t)

	require.Greater(t, float64(cfg.Health.MarkStaleHoldBps), 0.0,
		"ноль возвращает безусловный съём, стоивший 1514 выходов за сутки")
	require.LessOrEqual(t, cfg.Health.MarkStaleHoldBps, cfg.Reward.BandBps,
		"за полосой удерживать нечего: ступень уже вне зачёта")
}

func TestNegativeStaleMarkHoldIsRejected(t *testing.T) {
	_, err := Load(writeConfig(t, minimalConfig+"\nhealth:\n  markStaleHoldBps: -1\n"))
	require.ErrorContains(t, err, "health.markStaleHoldBps")
}

func TestStaleMarkHoldBeyondTheRewardBandIsRejected(t *testing.T) {
	_, err := Load(writeConfig(t, minimalConfig+"\nhealth:\n  markStaleHoldBps: 25\n"))
	require.ErrorContains(t, err, "health.markStaleHoldBps")
}

// Запрет на возврат ступени обязан ехать включённым и обязан покрывать
// измеренный интервал, а не круглое число.
//
// 18 августа 2026 защита сняла биды в 13:30:17.250, заполнение пустых ступеней
// вернуло их в 13:30:19.201 — через 1.95 секунды, — и в 13:30:22.486 их снесло.
// Полутора секунд, принятых у сравнимого оператора на TS, здесь не хватило бы:
// возврат случился позже этого срока.
//
// Верхняя граница столь же обязательна. Запрет дольше минимального времени
// жизни ордера означал бы, что ступень стоит в стакане меньше, чем ждёт
// снаружи, — присутствие, купленное за присутствие.
func TestShippedRefillLockoutCoversTheMeasuredReturn(t *testing.T) {
	gold := shippedConfig(t).Symbols["XAU-USD"]

	const measuredReturn = 1950 * time.Millisecond // 13:30:17.250 → 13:30:19.201
	require.Greater(t, gold.Quote.RefillLockout, measuredReturn,
		"запрет обязан пережить тот самый возврат, который стоил 1.18 Maker Hour")
	require.LessOrEqual(t, gold.Quote.RefillLockout, 2*gold.Quote.MinRest,
		"ступень не должна ждать снаружи дольше, чем живёт внутри")
}

func TestNegativeRefillLockoutIsRejected(t *testing.T) {
	_, err := Load(writeConfig(t, minimalConfig+"\ndefaults:\n  quote:\n    refillLockout: -1s\n"))
	require.ErrorContains(t, err, "refillLockout")
}
