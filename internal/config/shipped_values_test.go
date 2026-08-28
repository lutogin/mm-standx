package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"

	"github.com/nd/mm-standx/internal/domain/economics"
	"github.com/nd/mm-standx/internal/domain/market"
	"github.com/nd/mm-standx/internal/domain/money"
	"github.com/nd/mm-standx/internal/domain/policy"
	"github.com/nd/mm-standx/internal/domain/reward"
	"github.com/nd/mm-standx/internal/domain/sizing"
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

// Срок возврата цели обязан быть задан, и ноль здесь — не «выключено».
//
// Гистерезис цели построен на пороге расхождения (minDeadBandBps), а свежая
// цель снизу зажата полом: minBps плюс половина ширины лесенки. Значит
// расхождение с ней ограничено сверху тем, насколько цель отодвинулась от пола,
// и весь диапазон [пол, пол + minDeadBandBps) становится ловушкой: попав туда,
// цель не возвращается уже ничем. На золоте это 7.4-8.6 bps, то есть больше
// базисного пункта полосы шириной в десять.
//
// Замер боевого счёта 22-24 августа: центр простоял на 8.36-8.52 более двадцати
// часов подряд при требовании по риску около 3.1. Средний вес ступеней вышел
// 0.672 вместо 0.816 — пятая часть начисления за дистанцию, которую никто
// не выбирал.
//
// Отвергнутая альтернатива — ноль, то есть прежнее поведение. Граница, которая
// её отвергает, проверяется здесь же: ловушка существует ровно потому, что
// minDeadBandBps не помещается между полом и целью.
func TestShippedTargetRelaxReleasesTheDistanceRatchet(t *testing.T) {
	gold := shippedConfig(t).Symbols["XAU-USD"]
	ladder := policy.Ladder{
		Layers:    gold.Quote.LadderLayers,
		SpreadBps: gold.Quote.LadderSpreadBps,
	}

	floor := gold.Appetite.MinBps + ladder.HalfWidth()
	trap := floor + gold.Appetite.MinDeadBandBps
	require.Less(t, trap, gold.Appetite.MaxBps,
		"ловушка обязана быть внутри полосы, иначе доказывать нечего: "+
			"цель, попавшая между %s и %s, порогом расхождения не возвращается")

	require.Positive(t, gold.Appetite.TargetRelaxAfter,
		"без срока диапазон %s..%s удерживает цель до перезапуска", floor, trap)
	require.LessOrEqual(t, gold.Appetite.TargetRelaxAfter, 5*time.Minute,
		"всплеск не вправе стоить веса дольше, чем рынок после него остывает: "+
			"замер 19 августа дал полураспад беспокойства в четыре минуты")
}

func TestNegativeTargetRelaxIsRejected(t *testing.T) {
	_, err := Load(writeConfig(t, minimalConfig+"\ndefaults:\n  appetite:\n    targetRelaxAfter: -1s\n"))
	require.ErrorContains(t, err, "targetRelaxAfter")
}

// liveGoldSpec — инструмент, каким его отдаёт биржа: десятицентовый шаг цены
// и три знака в количестве. Ни минимум, ни максимум ордера здесь не связывают,
// и это намеренно: предмет проверки — потолок программы и бюджет маржи, а не
// лимиты контракта.
func liveGoldSpec() market.SymbolSpec {
	return market.SymbolSpec{
		Symbol:            "XAU-USD",
		PriceTickDecimals: 1,
		QtyTickDecimals:   3,
		MinOrderQty:       money.MustParseQty("0.001"),
		MaxOrderQty:       money.MustParseQty("100"),
		MaxOpenOrders:     100,
	}
}

// Замеры боевого счёта, от которых считается этот файл.
const (
	// liveEquityUSD — счёт 28 августа 2026 (PLAN.md §0, комментарий
	// к utilizationPct).
	liveEquityUSD = 20596
	// liveMarkUSD — цена золота из standxctl symbols, PLAN.md §1.3.
	liveMarkUSD = 4076.5
	// liveMarginPct — доля нотионала, которую биржа заблокировала при плече 40.
	// Оценка того же дня давала 2.667%; списала биржа по 2.887%.
	//
	// Расхождение и есть причина, по которой бюджет ограничен ещё и суммой,
	// которую сообщает сама биржа (sizing.Budget.SpendableUSD): 2.667 — это
	// оценка, а не свойство.
	liveMarginPct         = 2.667
	liveMarginPctObserved = 2.887
)

// livePlan считает план размера по действующей конфигурации для лесенки из
// layers ступеней, ближняя из которых стоит на полу дистанции.
func livePlan(t *testing.T, cfg *Config, layers int, marginPct float64) (sizing.Plan, []money.Bps) {
	t.Helper()
	gold := cfg.Symbols["XAU-USD"]

	// Ступени расставляются вокруг центра, а не от него: policy.Ladder.Anchor
	// отодвигает наружу центр, чтобы ближняя ступень встала ровно на дистанцию,
	// которую потребовал риск. Здесь эта дистанция — жёсткий пол appetite.
	ladder := policy.Ladder{Layers: layers, SpreadBps: gold.Quote.LadderSpreadBps}
	distances := ladder.Distances(gold.Appetite.MinBps + ladder.HalfWidth())

	plan, err := sizing.Compute(sizing.Inputs{
		Budget: sizing.Budget{
			EquityUSD:        decimal.NewFromInt(liveEquityUSD),
			InitialMarginPct: marginPct,
			UtilizationPct:   cfg.Risk.UtilizationPct,
		},
		Spec: liveGoldSpec(),
		Program: reward.Program{
			Curve: reward.Curve{
				BandBps:      cfg.Reward.BandBps,
				MaxWeightPct: cfg.Reward.MaxWeightPct,
				MinWeightPct: cfg.Reward.MinWeightPct,
			},
			UnitSize:             requireDecimal(t, gold.Program.UnitSize),
			PerSideCap:           requireDecimal(t, gold.Program.PerSideCap),
			MaxMakerHoursPerHour: gold.Program.MaxMakerHoursPerHour,
		},
		Mark:      money.MustParsePrice("4076.5"),
		Distances: distances,
	})
	require.NoError(t, err)
	return plan, distances
}

// scoredSide — взвешенная сторона, то самое X_side, которое биржа кладёт
// в перцентиль. Считается суммой по ступеням, каждая со своим весом.
func scoredSide(cfg *Config, plan sizing.Plan, distances []money.Bps) float64 {
	curve := reward.Curve{
		BandBps:      cfg.Reward.BandBps,
		MaxWeightPct: cfg.Reward.MaxWeightPct,
		MinWeightPct: cfg.Reward.MinWeightPct,
	}
	out := 0.0
	for i, r := range plan.Rungs {
		out += r.Decimal().InexactFloat64() * curve.Weight(distances[i])
	}
	return out
}

// marginShare — какая доля счёта уходит под маржу обеих сторон.
func marginShare(plan sizing.Plan, marginPct float64) float64 {
	notional := plan.PerSide.Decimal().InexactFloat64() * liveMarkUSD * 2
	return notional * marginPct / 100 / liveEquityUSD * 100
}

func requireDecimal(t *testing.T, s string) decimal.Decimal {
	t.Helper()
	d, err := decimal.NewFromString(s)
	require.NoError(t, err)
	return d
}

// Счёт обязан доходить до почасового потолка, и до 28 августа 2026 он до него
// не доходил по вине расчёта размера, а не рынка.
//
// sizing.usefulCeiling делил перекрытие на вес БЛИЖНЕЙ ступени, тогда как
// начисление считает сумму по всем ступеням, каждая со своим весом. Сторона
// при этом планировалась под цель, которой лесенка достичь не может:
// 60 / 0.880 x 0.816 = 55.6 против потолка 60.
//
// Отчёт Maker Yield History по XAU-USD за 24 августа, часы 06:00-14:00 UTC:
// P70 = 54.3…59.3, Maker Hours 1.81…1.98 при потолке 2.00. Предсказание старой
// формулы лежит внутри измеренного интервала — недобор объяснялся ею целиком,
// а не аптаймом и не заливами.
func TestShippedSizingReachesTheHourlyCeilingOnTheLiveAccount(t *testing.T) {
	cfg := shippedConfig(t)
	gold := cfg.Symbols["XAU-USD"]
	perSideCap := requireDecimal(t, gold.Program.PerSideCap).InexactFloat64()

	plan, distances := livePlan(t, cfg, gold.Quote.LadderLayers, liveMarginPct)
	require.Contains(t, plan.Reason, "reward ceiling",
		"на боевом счёте связывать обязан потолок программы, а не капитал")

	scored := scoredSide(cfg, plan, distances)
	require.InDelta(t, perSideCap, scored, 0.01,
		"взвешенная сторона обязана вставать ровно на потолок")
	require.InDelta(t, gold.Program.MaxMakerHoursPerHour, scored/requireDecimal(t, gold.Program.UnitSize).InexactFloat64(), 0.01,
		"это и есть 2.00 Maker Hour в час")

	// Отвергнутая формула названа явно, чтобы её нельзя было вернуть молча.
	curve := reward.Curve{BandBps: cfg.Reward.BandBps, MaxWeightPct: cfg.Reward.MaxWeightPct, MinWeightPct: cfg.Reward.MinWeightPct}
	mean := 0.0
	for _, d := range distances {
		mean += curve.Weight(d)
	}
	mean /= float64(len(distances))
	byNearest := perSideCap / curve.Weight(distances[0]) * mean
	require.Less(t, byNearest, perSideCap*0.94,
		"деление на вес ближней ступени не доносит сторону до потолка")

	// Возросший размер обязан помещаться в бюджет маржи, иначе выигрыш
	// в зачёте оплачивался бы отказами «balance not enough».
	require.Less(t, marginShare(plan, liveMarginPct), cfg.Risk.UtilizationPct,
		"план обязан оставаться внутри utilizationPct")
}

// Меньше ступеней — больше запаса по марже под тем же зачётом.
//
// Это вход в решение о числе ступеней, а не само решение: лесенка существует
// ради присутствия во время перестановки, и отказ от неё оплачивается
// концентрацией размера на ближней ступени. Здесь фиксируется только цена
// в марже, которую видно из конфигурации.
//
// Замеренная ставка блокировки — величина неточная: 2.667% дала оценка того же
// дня, 2.887% списала биржа. При верхнем значении трёхступенчатая лесенка
// в бюджет уже не помещается и падает обратно под ограничение капитала,
// то есть до потолка часа не доходит.
func TestFewerRungsBuyMarginHeadroomAtTheHourlyCeiling(t *testing.T) {
	cfg := shippedConfig(t)
	perSideCap := requireDecimal(t, cfg.Symbols["XAU-USD"].Program.PerSideCap).InexactFloat64()

	var shares []float64
	for layers := 1; layers <= 3; layers++ {
		plan, distances := livePlan(t, cfg, layers, liveMarginPct)
		require.InDelta(t, perSideCap, scoredSide(cfg, plan, distances), 0.01,
			"каждая геометрия обязана доходить до потолка при %d ступенях", layers)
		shares = append(shares, marginShare(plan, liveMarginPct))
	}

	require.Less(t, shares[0], shares[1])
	require.Less(t, shares[1], shares[2])
	require.InDelta(t, 0.927, shares[0]/shares[2], 0.005,
		"один ордер берёт тот же зачёт примерно на 7%% меньшем нотионале")

	// При верхней оценке ставки блокировки трёх ступеней бюджет уже не несёт.
	three, distances := livePlan(t, cfg, 3, liveMarginPctObserved)
	require.Contains(t, three.Reason, "margin budget")
	require.Less(t, scoredSide(cfg, three, distances), perSideCap,
		"под 2.887% трёхступенчатая лесенка до потолка часа не доходит")

	one, oneDistances := livePlan(t, cfg, 1, liveMarginPctObserved)
	require.Contains(t, one.Reason, "reward ceiling")
	require.InDelta(t, perSideCap, scoredSide(cfg, one, oneDistances), 0.01,
		"одиночный ордер доходит до потолка и под верхней оценкой ставки")
}
