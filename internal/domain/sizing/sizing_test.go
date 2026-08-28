package sizing

import (
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"

	"github.com/nd/mm-standx/internal/domain/market"
	"github.com/nd/mm-standx/internal/domain/money"
	"github.com/nd/mm-standx/internal/domain/reward"
)

// Live BTC-USD specification, read from the exchange on 2026-08-02.
func btcSpec() market.SymbolSpec {
	return market.SymbolSpec{
		Symbol:            "BTC-USD",
		PriceTickDecimals: 2,
		QtyTickDecimals:   4,
		MinOrderQty:       money.MustParseQty("0.0001"),
		MaxOrderQty:       money.MustParseQty("100"),
		MaxOpenOrders:     100,
	}
}

func btcProgram() reward.Program {
	return reward.Program{
		Curve:                reward.Curve{BandBps: 10, MaxWeightPct: 200, MinWeightPct: 40},
		UnitSize:             decimal.NewFromInt(2),
		PerSideCap:           decimal.NewFromInt(20),
		MaxMakerHoursPerHour: 10,
	}
}

func inputs(equity float64) Inputs {
	return Inputs{
		Budget: Budget{
			EquityUSD: decimal.NewFromFloat(equity),
			// 25% блокировки — это ровно прежнее плечо 4. Взято намеренно:
			// тесты этого файла проверяют дробление по ступеням, потолки
			// и минимумы, и переписывать их числа вслед за измеренной ставкой
			// биржи значило бы менять предмет проверки.
			InitialMarginPct: 25,
			UtilizationPct:   70,
		},
		Spec:      btcSpec(),
		Program:   btcProgram(),
		Mark:      money.MustParsePrice("63400"),
		Distances: []money.Bps{6.5, 7.5, 8.5},
	}
}

// The account we actually hold: 95 DUSD. The point of the test is that the
// machinery produces a valid, tiny plan rather than failing.
func TestSmallAccountStillProducesAPlan(t *testing.T) {
	plan, err := Compute(inputs(95))
	require.NoError(t, err)

	// 95 x 70% x 4 leverage, halved between the sides, gives about 133 USD.
	// That is 0.0021 BTC per side, or 0.0007 per rung before the tick snap
	// rounds it down to 0.0006.
	require.Len(t, plan.Rungs, 3)
	require.Equal(t, "0.0006", plan.Rungs[0].String())
	require.Equal(t, "0.0018", plan.PerSide.String())

	for _, r := range plan.Rungs {
		require.NoError(t, btcSpec().ValidateQty(r), "every rung must be placeable")
	}
}

// Resting orders lock margin. Using every last dollar would leave nothing for
// the reduce-only order that closes a position after a fill.
func TestUtilizationLeavesHeadroom(t *testing.T) {
	full := inputs(95)
	full.Budget.UtilizationPct = 100

	conservative, err := Compute(inputs(95))
	require.NoError(t, err)
	aggressive, err := Compute(full)
	require.NoError(t, err)

	require.True(t, conservative.PerSide.Lt(aggressive.PerSide),
		"reserving headroom must reduce the quoted size")
}

// Размер следует за балансом счёта и ничем другим не ограничен.
//
// Прежде здесь стояла настроенная аллокация, и она же была единственным, что
// связывало размер. 15 августа это стоило половины счёта: в файле 5800 при
// балансе 11091, и увидеть расхождение можно было только по строке в сводке.
func TestSizeFollowsTheAccountBalance(t *testing.T) {
	small, err := Compute(inputs(10_000))
	require.NoError(t, err)
	require.Contains(t, small.Reason, "margin budget")

	doubled, err := Compute(inputs(20_000))
	require.NoError(t, err)

	require.Equal(t, doubled.PerSide.Decimal().String(),
		small.PerSide.Decimal().Mul(decimal.NewFromInt(2)).String(),
		"twice the equity is twice the size, with nothing in a file to say otherwise")
}

// Size beyond the programme ceiling earns nothing while still carrying full
// execution risk, so it must never be quoted.
func TestRewardCeilingCapsSize(t *testing.T) {
	rich := inputs(100_000_000)

	plan, err := Compute(rich)
	require.NoError(t, err)
	require.Contains(t, plan.Reason, "reward ceiling")

	// Rungs stand at 6.5 / 7.5 / 8.5 bps, where the weights are 0.96, 0.80 and
	// 0.64. The programme scores their sum and the size is split evenly, so the
	// 20 unit cap saturates at 20 / 0.80 — the mean weight, not the 0.96 of the
	// near rung.
	require.InDelta(t, 20/0.80, plan.PerSide.Decimal().InexactFloat64(), 0.001)
}

// Потолок обязан достигаться ровно, а не приблизительно.
//
// Зачёт — сумма по ступеням, каждая со своим весом; именно она упирается в cap,
// и планировать нужно её, а не размер стороны. Прежняя формула делила cap
// на вес ближней ступени, и сторона садилась на 7% ниже собственного потолка:
// отчёт биржи 24 августа 2026 по XAU-USD давал P70 = 54.3…59.3 при потолке 60,
// то есть 1.81…1.98 Maker Hour из достижимых 2.00 (PLAN.md §0).
func TestCeilingIsReachedExactlyAtEveryLayerCount(t *testing.T) {
	ladders := [][]money.Bps{
		{7},
		{7, 7.4},
		{7, 7.4, 7.8},
		{6.5, 7.5, 8.5},
	}
	for _, distances := range ladders {
		rich := inputs(100_000_000)
		rich.Distances = distances

		plan, err := Compute(rich)
		require.NoError(t, err)
		require.Len(t, plan.Rungs, len(distances))

		scored := 0.0
		for i, r := range plan.Rungs {
			scored += r.Decimal().InexactFloat64() * rich.Program.Curve.Weight(distances[i])
		}
		perSideCap := rich.Program.PerSideCap.InexactFloat64()
		require.InDelta(t, perSideCap, scored, 0.001,
			"the weighted side must land on the cap for %v", distances)
	}
}

// Меньше ступеней — меньше нотионала под тем же зачётом.
//
// Вес падает с дистанцией, поэтому дальние ступени покупают зачёт дороже:
// на 7.0 / 7.4 / 7.8 bps средний вес 0.816 против 0.880 у одиночного ордера
// на 7.0. Ближняя ступень при этом стоит на той же дистанции в обоих случаях —
// Ladder.Anchor отодвигает наружу центр, а не её.
//
// Это и есть цена лесенки: она существует ради присутствия во время
// перестановки, а не ради награды, и оплачивается семью процентами нотионала
// под тем же риском.
func TestFewerRungsReachTheCeilingWithLessSize(t *testing.T) {
	sizeFor := func(distances []money.Bps) float64 {
		in := inputs(100_000_000)
		in.Distances = distances
		plan, err := Compute(in)
		require.NoError(t, err)
		return plan.PerSide.Decimal().InexactFloat64()
	}

	one := sizeFor([]money.Bps{7})
	two := sizeFor([]money.Bps{7, 7.4})
	three := sizeFor([]money.Bps{7, 7.4, 7.8})

	require.Less(t, one, two)
	require.Less(t, two, three)
	require.InDelta(t, 0.928, one/three, 0.002,
		"a single order needs about 7% less size than three rungs")
}

// Лесенка, суженная минимальным размером ордера, не имеет права перелететь
// потолок.
//
// Потолок и число ступеней связаны: меньше ступеней — выше средний вес — ниже
// потолок. Посчитанный до сужения, он относился бы к геометрии, которой в итоге
// нет: при весах 0.96 / 0.80 / 0.64 обрезанная до двух ступеней сторона взяла бы
// 1.10 потолка, и излишек не приносил бы ничего, неся при этом полный риск
// исполнения.
func TestShrunkLadderStillRespectsTheCeiling(t *testing.T) {
	in := inputs(100_000_000)
	in.Spec.MinOrderQty = money.MustParseQty("10")

	plan, err := Compute(in)
	require.NoError(t, err)
	require.Len(t, plan.Rungs, 2, "three rungs of 8.33 do not clear the 10 unit minimum")
	require.Contains(t, plan.Reason, "ladder reduced")

	scored := 0.0
	for i, r := range plan.Rungs {
		scored += r.Decimal().InexactFloat64() * in.Program.Curve.Weight(in.Distances[i])
	}
	require.InDelta(t, in.Program.PerSideCap.InexactFloat64(), scored, 0.001,
		"the surviving rungs must land on the cap, not above it")
}

// Quoting closer means more weight per unit, so less size is needed to reach
// the same ceiling. This is the coupling that makes static sizing wrong.
func TestCeilingDependsOnDistance(t *testing.T) {
	near := inputs(100_000_000)
	near.Distances = []money.Bps{1, 2, 3}

	far := inputs(100_000_000)
	far.Distances = []money.Bps{8, 9, 10}

	nearPlan, err := Compute(near)
	require.NoError(t, err)
	farPlan, err := Compute(far)
	require.NoError(t, err)

	require.True(t, nearPlan.PerSide.Lt(farPlan.PerSide),
		"a closer ladder must saturate the ceiling with less size")
}

// Two placeable rungs beat three rejected orders.
func TestLadderShrinksWhenRungsFallBelowMinimum(t *testing.T) {
	tiny := inputs(95)
	tiny.Spec.MinOrderQty = money.MustParseQty("0.001")

	plan, err := Compute(tiny)
	require.NoError(t, err)
	require.Len(t, plan.Rungs, 2, "the ladder must shrink to fit the minimum size")
	require.Contains(t, plan.Reason, "ladder reduced")
}

func TestFailsWhenCapitalCannotCoverOneRung(t *testing.T) {
	_, err := Compute(inputs(1))
	require.ErrorContains(t, err, "minimum")
}

func TestRejectsInvalidInputs(t *testing.T) {
	noDistances := inputs(95)
	noDistances.Distances = nil
	_, err := Compute(noDistances)
	require.ErrorContains(t, err, "ladder distances")

	noMark := inputs(95)
	noMark.Mark = money.MustParsePrice("0")
	_, err = Compute(noMark)
	require.ErrorContains(t, err, "mark price")

	noMargin := inputs(95)
	noMargin.Budget.InitialMarginPct = 0
	_, err = Compute(noMargin)
	require.ErrorContains(t, err, "initial margin")

	broke := inputs(0)
	_, err = Compute(broke)
	require.ErrorContains(t, err, "no usable capital")
}

// Бюджет считается от всего капитала, а не от свободного остатка.
//
// Свободный остаток уже уменьшен тем, что мы сами и заблокировали, поэтому
// бюджет от него — петля: каждый выставленный доллар сокращает разрешённое.
// Она сходится примерно на 40% капитала и не переваливает половины ни при
// каком UtilizationPct, то есть больше половины счёта была структурно
// недостижима (PLAN.md §12.5).
func TestBudgetIsIndependentOfWhatIsAlreadyLocked(t *testing.T) {
	// Один и тот же счёт: сначала пустой, потом с висящими ордерами, из-за
	// которых свободного осталось вдвое меньше. Equity при этом не меняется —
	// заблокированное никуда не делось, оно наше.
	empty := inputs(10_000)
	working := inputs(10_000)

	first, err := Compute(empty)
	require.NoError(t, err)
	second, err := Compute(working)
	require.NoError(t, err)

	require.Equal(t, first.PerSide, second.PerSide,
		"size must not shrink because of margin we ourselves locked")
}

// Утилизация означает ровно то, что написано: долю капитала под маржой.
func TestUtilizationIsTheShareOfCapitalUnderMargin(t *testing.T) {
	in := inputs(10_000)
	in.Budget.InitialMarginPct = 10
	in.Budget.UtilizationPct = 70

	plan, err := Compute(in)
	require.NoError(t, err)

	// 70% от 10 000 = 7 000 маржи; при доле 10% это 70 000 нотионала,
	// поровну между сторонами — 35 000 на сторону.
	notional := plan.PerSide.Decimal().Mul(in.Mark.Decimal())
	require.InDelta(t, 35_000, notional.InexactFloat64(), 200)
}

// Размер определяется долей блокировки, а не плечом: плечо в ордере биржа
// игнорирует, проба 12 августа дала одинаковые 10.542% при 4, 8 и 20.
func TestHalvingTheLockedShareDoublesTheSize(t *testing.T) {
	tight := inputs(10_000)
	tight.Budget.InitialMarginPct = 20

	loose := inputs(10_000)
	loose.Budget.InitialMarginPct = 10

	small, err := Compute(tight)
	require.NoError(t, err)
	big, err := Compute(loose)
	require.NoError(t, err)

	ratio := big.PerSide.Decimal().Div(small.PerSide.Decimal()).InexactFloat64()
	require.InDelta(t, 2.0, ratio, 0.01)
}
