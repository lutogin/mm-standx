package sizing

import (
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
)

// Бюджет не вправе просить больше, чем биржа готова заблокировать.
//
// Эпизод боевого счёта 28 августа 2026, 13:10 UTC, воспроизведён числами.
// Бюджет от equity считается по ИЗМЕРЕННОЙ доле блокировки, и она промахнулась
// на 7.6%: 2.667% против фактических 2.887%. Планировали занять 85% капитала,
// заняли 92%, на последнюю ступень не хватило — и дальше пять минут отказов.
func TestSpendableCapsTheBudgetWhenTheMarginRateIsUnderestimated(t *testing.T) {
	in := inputs(20_596)
	in.Budget.InitialMarginPct = 2.667
	in.Budget.UtilizationPct = 85

	// Без потолка бюджет — это 85% от equity, то есть 17 507 USD маржи.
	uncapped, reason := usableCapital(in.Budget)
	require.InDelta(t, 17_506.6, uncapped.InexactFloat64(), 1)
	require.Contains(t, reason, "margin budget")

	// Биржа в тот момент показывала 20 596 свободных и ничего заблокированного,
	// но у неё же стоит и её собственный предел. Возьмём его ниже намерения.
	in.Budget.SpendableUSD = decimal.NewFromInt(12_000)
	capped, reason := usableCapital(in.Budget)
	require.Equal(t, "12000", capped.String())
	require.Contains(t, reason, "free balance the venue reports",
		"the binding constraint must be named, or the size line lies about why")
}

// Потолок именно потолок, а не замена бюджету: когда биржа показывает больше,
// чем мы согласны занять, решает намерение.
func TestSpendableDoesNotRaiseTheBudget(t *testing.T) {
	in := inputs(10_000)
	in.Budget.SpendableUSD = decimal.NewFromInt(1_000_000)

	capital, reason := usableCapital(in.Budget)
	require.Equal(t, "7000", capital.String(), "70% of equity, as configured")
	require.Contains(t, reason, "margin budget")
}

// Отсутствие снимка баланса не должно обнулять размер.
//
// Ноль означает «биржа ещё ничего не сказала», а не «денег нет». Спутать эти
// два состояния значит молча не котировать на старте.
func TestZeroSpendableLeavesTheOldBehaviour(t *testing.T) {
	in := inputs(10_000)
	require.True(t, in.Budget.SpendableUSD.IsZero())

	plan, err := Compute(in)
	require.NoError(t, err)
	require.Contains(t, plan.Reason, "margin budget")
	require.True(t, plan.PerSide.IsPositive())
}

// Главное свойство суммы available + locked: она не зависит от того, стоим ли
// мы сейчас в стакане.
//
// Ради этого потолок и складывается из двух слагаемых. От одного available
// бюджет превращается в петлю — каждый выставленный доллар сокращает
// разрешённое, — и сходится примерно на 40% капитала при любой утилизации.
func TestSpendableIsIndependentOfOurOwnRestingOrders(t *testing.T) {
	// Один и тот же счёт в двух состояниях: пустой стакан и рабочий,
	// где половина свободного ушла под наши же ордера.
	idle := inputs(10_000)
	idle.Budget.SpendableUSD = decimal.NewFromInt(4_000).Add(decimal.Zero) // available 4000, locked 0

	working := inputs(10_000)
	working.Budget.SpendableUSD = decimal.NewFromInt(1_500).Add(decimal.NewFromInt(2_500)) // available 1500, locked 2500

	first, err := Compute(idle)
	require.NoError(t, err)
	second, err := Compute(working)
	require.NoError(t, err)

	require.Equal(t, first.PerSide, second.PerSide,
		"margin we ourselves locked must return to the ceiling, not shrink it")
}

// Потолок падает от того, что действительно ушло со счёта: маржа под открытой
// позицией в available+locked не возвращается, и размер обязан это заметить.
func TestSpendableFallsWithMarginHeldUnderAPosition(t *testing.T) {
	before := inputs(10_000)
	before.Budget.SpendableUSD = decimal.NewFromInt(4_000)

	after := inputs(10_000)
	after.Budget.SpendableUSD = decimal.NewFromInt(2_000) // половина ушла под позицию

	big, err := Compute(before)
	require.NoError(t, err)
	small, err := Compute(after)
	require.NoError(t, err)

	require.True(t, small.PerSide.Lt(big.PerSide))
	require.Contains(t, small.Reason, "free balance the venue reports")
}
