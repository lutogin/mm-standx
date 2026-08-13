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

func inputs(allocation, available float64) Inputs {
	return Inputs{
		Budget: Budget{
			AllocationUSD: decimal.NewFromFloat(allocation),
			EquityUSD:     decimal.NewFromFloat(available),
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
	plan, err := Compute(inputs(0, 95))
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
	full := inputs(0, 95)
	full.Budget.UtilizationPct = 100

	conservative, err := Compute(inputs(0, 95))
	require.NoError(t, err)
	aggressive, err := Compute(full)
	require.NoError(t, err)

	require.True(t, conservative.PerSide.Lt(aggressive.PerSide),
		"reserving headroom must reduce the quoted size")
}

func TestAllocationCapsAvailableMargin(t *testing.T) {
	plan, err := Compute(inputs(20, 10_000))
	require.NoError(t, err)
	require.Contains(t, plan.Reason, "allocation")

	// 20 USD at 4x, halved, is 40 USD per side across three rungs.
	require.Equal(t, "0.0002", plan.Rungs[0].String())
}

// Size beyond the programme ceiling earns nothing while still carrying full
// execution risk, so it must never be quoted.
func TestRewardCeilingCapsSize(t *testing.T) {
	rich := inputs(0, 100_000_000)

	plan, err := Compute(rich)
	require.NoError(t, err)
	require.Contains(t, plan.Reason, "reward ceiling")

	// The nearest rung sits at 6.5 bps, where the weight is 96%.
	// The 20 unit cap therefore saturates at 20 / 0.96 units.
	require.InDelta(t, 20/0.96, plan.PerSide.Decimal().InexactFloat64(), 0.001)
}

// Quoting closer means more weight per unit, so less size is needed to reach
// the same ceiling. This is the coupling that makes static sizing wrong.
func TestCeilingDependsOnDistance(t *testing.T) {
	near := inputs(0, 100_000_000)
	near.Distances = []money.Bps{1, 2, 3}

	far := inputs(0, 100_000_000)
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
	tiny := inputs(0, 95)
	tiny.Spec.MinOrderQty = money.MustParseQty("0.001")

	plan, err := Compute(tiny)
	require.NoError(t, err)
	require.Len(t, plan.Rungs, 2, "the ladder must shrink to fit the minimum size")
	require.Contains(t, plan.Reason, "ladder reduced")
}

func TestFailsWhenCapitalCannotCoverOneRung(t *testing.T) {
	_, err := Compute(inputs(0, 1))
	require.ErrorContains(t, err, "minimum")
}

func TestRejectsInvalidInputs(t *testing.T) {
	noDistances := inputs(0, 95)
	noDistances.Distances = nil
	_, err := Compute(noDistances)
	require.ErrorContains(t, err, "ladder distances")

	noMark := inputs(0, 95)
	noMark.Mark = money.MustParsePrice("0")
	_, err = Compute(noMark)
	require.ErrorContains(t, err, "mark price")

	noMargin := inputs(0, 95)
	noMargin.Budget.InitialMarginPct = 0
	_, err = Compute(noMargin)
	require.ErrorContains(t, err, "initial margin")

	broke := inputs(0, 0)
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
	empty := inputs(0, 10_000)
	working := inputs(0, 10_000)

	first, err := Compute(empty)
	require.NoError(t, err)
	second, err := Compute(working)
	require.NoError(t, err)

	require.Equal(t, first.PerSide, second.PerSide,
		"size must not shrink because of margin we ourselves locked")
}

// Утилизация означает ровно то, что написано: долю капитала под маржой.
func TestUtilizationIsTheShareOfCapitalUnderMargin(t *testing.T) {
	in := inputs(0, 10_000)
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
	tight := inputs(0, 10_000)
	tight.Budget.InitialMarginPct = 20

	loose := inputs(0, 10_000)
	loose.Budget.InitialMarginPct = 10

	small, err := Compute(tight)
	require.NoError(t, err)
	big, err := Compute(loose)
	require.NoError(t, err)

	ratio := big.PerSide.Decimal().Div(small.PerSide.Decimal()).InexactFloat64()
	require.InDelta(t, 2.0, ratio, 0.01)
}
