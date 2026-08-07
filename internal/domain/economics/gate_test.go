package economics

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func conservativeGate() Gate {
	return Gate{
		BreakEvenUSDPerMakerHour:    22,
		CalibrationMaxAllocationUSD: 60,
	}
}

func TestUnknownRewardAllowsOnlySmallFullRateCalibration(t *testing.T) {
	g := conservativeGate()

	require.True(t, g.Evaluate(60, 1, 1).Allowed)
	require.False(t, g.Evaluate(61, 1, 1).Allowed)
	require.False(t, g.Evaluate(60, 0.5, 1).Allowed)
	require.False(t, g.Evaluate(60, 1, 0.1).Allowed)
}

func TestKnownRewardMustCoverTierAndMarketMultipliers(t *testing.T) {
	g := conservativeGate()
	g.RewardRateLowerBoundUSDPerMakerHour = 50

	require.True(t, g.Evaluate(10_000, 1, 1).Allowed)
	require.True(t, g.Evaluate(10_000, 0.5, 1).Allowed)
	require.False(t, g.Evaluate(10_000, 1, 0.1).Allowed)
	require.InDelta(t, 25, g.Evaluate(10_000, 0.5, 1).EffectiveRate, 1e-9)
}

func TestMinimumTierMultiplierIsDerivedFromUnitEconomics(t *testing.T) {
	g := conservativeGate()
	require.Equal(t, 1.0, g.MinimumTierMultiplier())

	g.RewardRateLowerBoundUSDPerMakerHour = 50
	require.InDelta(t, 0.44, g.MinimumTierMultiplier(), 1e-9)
}
