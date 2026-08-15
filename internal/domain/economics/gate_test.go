package economics

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func conservativeGate() Gate {
	return Gate{BreakEvenUSDPerMakerHour: 22}
}

// Пока ставка пула неизвестна, шлюз запрещает не размер, а часы с урезанным
// начислением.
//
// Резать размер он больше не пытается, и это не послабление. Награда и цена
// залива линейны по нотионалу, поэтому их отношение — то есть сам факт
// окупаемости — от размера не зависит: маленькая позиция теряет ровно
// пропорционально меньше. Что действительно меняет отношение, так это
// множители: они режут начисление, оставляя риск исполнения прежним.
func TestUnknownRewardAllowsOnlyFullRateHours(t *testing.T) {
	g := conservativeGate()

	require.True(t, g.Evaluate(1, 1).Allowed, "full tier on an open market is measurable")
	require.False(t, g.Evaluate(0.5, 1).Allowed, "half accrual, unchanged fill risk")
	require.False(t, g.Evaluate(1, 0.1).Allowed, "a tenth of accrual for the same risk")
}

func TestKnownRewardMustCoverTierAndMarketMultipliers(t *testing.T) {
	g := conservativeGate()
	g.RewardRateLowerBoundUSDPerMakerHour = 50

	require.True(t, g.Evaluate(1, 1).Allowed)
	require.True(t, g.Evaluate(0.5, 1).Allowed, "25 USD/MH still clears the 22 break-even")
	require.False(t, g.Evaluate(1, 0.1).Allowed, "5 USD/MH does not")
	require.InDelta(t, 25, g.Evaluate(0.5, 1).EffectiveRate, 1e-9)
}

func TestMinimumTierMultiplierIsDerivedFromUnitEconomics(t *testing.T) {
	g := conservativeGate()
	require.Equal(t, 1.0, g.MinimumTierMultiplier())

	g.RewardRateLowerBoundUSDPerMakerHour = 50
	require.InDelta(t, 0.44, g.MinimumTierMultiplier(), 1e-9)
}
