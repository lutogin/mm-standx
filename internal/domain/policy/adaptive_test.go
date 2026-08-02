package policy

import (
	"testing"

	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"

	"github.com/nd/mm-standx/internal/domain/money"
)

func appetite() Appetite {
	return Appetite{
		JumpMultiple:   2.0,
		FloorMarginBps: 1.0,
		DeadBandJumps:  1.5,
		MinDeadBandBps: 0.5,
	}
}

func bounds() Bounds { return Bounds{MinBps: 4.0, MaxBps: 9.5} }

// Measured on 2026-08-02, see PLAN.md section 1.4.
var (
	btcConditions = Conditions{HalfSpreadBps: 0.63, JumpBps: 0.45, Ready: true}
	xauConditions = Conditions{HalfSpreadBps: 1.23, JumpBps: 0.30, Ready: true}
	xagConditions = Conditions{HalfSpreadBps: 2.54, JumpBps: 5.00, Ready: true}
)

// The same appetite produces different distances per market, because the risk
// each market carries is different. A fixed threshold would silently mean a
// different level of exposure on every instrument.
func TestTargetAdaptsToMarketConditions(t *testing.T) {
	a, b := appetite(), bounds()

	btc := a.Target(btcConditions, b)
	require.True(t, btc.Feasible)
	require.InDelta(t, 4.0, btc.Distance.Float64(), 1e-9, "a calm, tight market lets us sit at the floor")

	xau := a.Target(xauConditions, b)
	require.True(t, xau.Feasible)
	require.InDelta(t, 4.0, xau.Distance.Float64(), 1e-9)

	// Silver: half-spread 2.54 plus two jumps of 5.00 plus the floor margin.
	xag := a.Target(xagConditions, b)
	require.False(t, xag.Feasible,
		"silver's jump profile leaves no safe distance inside the band")
	require.Contains(t, xag.Reason, "reward band")
}

// Quoting without a risk estimate is a blind bet. The check lives inside the
// distance rule so that no caller can forget it, and it covers reconnects and
// stream gaps as well as the cold start.
func TestUnreadyEstimateForbidsQuoting(t *testing.T) {
	a, b := appetite(), bounds()

	cold := btcConditions
	cold.Ready = false

	q := a.Target(cold, b)
	require.False(t, q.Feasible)
	require.Contains(t, q.Reason, "enough observations")
}

// Refusing to quote is a legitimate answer, and a cheap one: the step-shaped
// uptime credit leaves roughly eighteen free minutes per hour.
func TestInfeasibleWhenRiskExceedsBand(t *testing.T) {
	a, b := appetite(), bounds()

	violent := Conditions{HalfSpreadBps: 1.0, JumpBps: 20, Ready: true}
	require.False(t, a.Target(violent, b).Feasible)
}

// Widening happens by itself as conditions deteriorate; there is no separate
// "widen on volatility" rule to keep in sync.
func TestDistanceGrowsWithRisk(t *testing.T) {
	a, b := appetite(), Bounds{MinBps: 0, MaxBps: 100}

	rapid.Check(t, func(t *rapid.T) {
		spread := rapid.Float64Range(0, 5).Draw(t, "halfSpread")
		calm := rapid.Float64Range(0, 5).Draw(t, "calmJump")
		wild := rapid.Float64Range(calm, 10).Draw(t, "wildJump")

		quiet := a.Target(Conditions{HalfSpreadBps: money.Bps(spread), JumpBps: money.Bps(calm), Ready: true}, b)
		rough := a.Target(Conditions{HalfSpreadBps: money.Bps(spread), JumpBps: money.Bps(wild), Ready: true}, b)

		require.LessOrEqual(t, quiet.Distance.Float64(), rough.Distance.Float64()+1e-9,
			"more violent conditions must never pull the quote closer")
	})
}

func TestDistanceGrowsWithSpread(t *testing.T) {
	a, b := appetite(), Bounds{MinBps: 0, MaxBps: 100}

	rapid.Check(t, func(t *rapid.T) {
		jump := rapid.Float64Range(0, 5).Draw(t, "jump")
		tight := rapid.Float64Range(0, 5).Draw(t, "tightSpread")
		wide := rapid.Float64Range(tight, 10).Draw(t, "wideSpread")

		narrow := a.Target(Conditions{HalfSpreadBps: money.Bps(tight), JumpBps: money.Bps(jump), Ready: true}, b)
		broad := a.Target(Conditions{HalfSpreadBps: money.Bps(wide), JumpBps: money.Bps(jump), Ready: true}, b)

		require.LessOrEqual(t, narrow.Distance.Float64(), broad.Distance.Float64()+1e-9,
			"a wider spread must push the quote further from the mark")
	})
}

// Depth, not distance from the mark, is what actually protects the quote.
// Whatever distance is chosen, the depth must absorb the configured number of
// jumps plus the floor margin.
func TestChosenDistanceAlwaysCoversTheRisk(t *testing.T) {
	a := appetite()
	b := Bounds{MinBps: 0, MaxBps: 100}

	rapid.Check(t, func(t *rapid.T) {
		c := Conditions{Ready: true,
			HalfSpreadBps: money.Bps(rapid.Float64Range(0, 8).Draw(t, "halfSpread")),
			JumpBps:       money.Bps(rapid.Float64Range(0, 8).Draw(t, "jump")),
		}

		q := a.Target(c, b)
		require.True(t, q.Feasible)
		require.GreaterOrEqual(t, q.Margin.Float64(), a.FloorMarginBps.Float64()-1e-9,
			"depth must exceed the jump allowance by at least the floor margin")
	})
}

// The floor exists precisely for the moments when measured risk looks
// deceptively small, as it does when a market is simply frozen.
func TestFloorHoldsWhenMarketLooksTooCalm(t *testing.T) {
	a, b := appetite(), bounds()

	frozen := Conditions{HalfSpreadBps: 0, JumpBps: 0, Ready: true}
	q := a.Target(frozen, b)

	require.True(t, q.Feasible)
	require.Equal(t, b.MinBps, q.Distance, "the floor must hold even at zero measured risk")
}

// Every derived threshold set must stay internally consistent, otherwise the
// classifier would produce contradictory decisions.
func TestDerivedThresholdsAreAlwaysOrdered(t *testing.T) {
	a := appetite()
	b := bounds()

	rapid.Check(t, func(t *rapid.T) {
		c := Conditions{Ready: true,
			HalfSpreadBps: money.Bps(rapid.Float64Range(0, 4).Draw(t, "halfSpread")),
			JumpBps:       money.Bps(rapid.Float64Range(0, 2).Draw(t, "jump")),
		}

		q := a.Target(c, b)
		if !q.Feasible {
			return
		}
		th := a.Thresholds(q, c, b)
		require.NoError(t, th.Validate(), "derived thresholds must satisfy the ordering invariant")
	})
}

// A quiet market earns a narrow dead band, a violent one a wide band: the
// alternative is either wasted requests in a storm or lost reward in a calm.
func TestDeadBandScalesWithVolatility(t *testing.T) {
	a, b := appetite(), bounds()

	calm := Conditions{HalfSpreadBps: 0.5, JumpBps: 0.1, Ready: true}
	rough := Conditions{HalfSpreadBps: 0.5, JumpBps: 1.5, Ready: true}

	calmBand := a.Thresholds(a.Target(calm, b), calm, b)
	roughBand := a.Thresholds(a.Target(rough, b), rough, b)

	calmWidth := calmBand.MaxKeepBps - calmBand.MinKeepBps
	roughWidth := roughBand.MaxKeepBps - roughBand.MinKeepBps
	require.Greater(t, roughWidth.Float64(), calmWidth.Float64(),
		"a more volatile market must get a wider dead band")
}

// The target must rest quietly the moment it is placed, otherwise the bot would
// requote the order it has just created.
func TestTargetRestsInsideItsOwnDeadBand(t *testing.T) {
	a, b := appetite(), bounds()

	for name, c := range map[string]Conditions{"btc": btcConditions, "xau": xauConditions} {
		t.Run(name, func(t *testing.T) {
			q := a.Target(c, b)
			require.True(t, q.Feasible)

			th := a.Thresholds(q, c, b)
			require.Equal(t, Hold, th.Classify(q.Distance).Action)
		})
	}
}
