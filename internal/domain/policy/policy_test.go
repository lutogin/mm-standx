package policy

import (
	"testing"

	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"

	"github.com/nd/mm-standx/internal/domain/money"
)

func thresholds() Thresholds {
	return Thresholds{
		PullBps:    1.0,
		PanicBps:   2.5,
		MinKeepBps: 5.0,
		TargetBps:  7.5,
		MaxKeepBps: 9.2,
		BandBps:    10.0,
	}
}

func TestClassifyZones(t *testing.T) {
	th := thresholds()

	cases := []struct {
		distance money.Bps
		action   Action
		sequence Sequence
		urgency  Urgency
	}{
		{-3.0, Pull, CancelFirst, UrgencySafety}, // mark crossed the order
		{0.5, Pull, CancelFirst, UrgencySafety},
		{1.0, Requote, CancelFirst, UrgencySafety},
		{2.4, Requote, CancelFirst, UrgencySafety},
		{2.5, Requote, PlaceFirst, UrgencySafety},
		{4.9, Requote, PlaceFirst, UrgencySafety},
		{5.0, Hold, PlaceFirst, UrgencyNone},
		{7.5, Hold, PlaceFirst, UrgencyNone},
		{9.2, Hold, PlaceFirst, UrgencyNone},
		{9.3, Requote, PlaceFirst, UrgencyReward},
		{10.0, Requote, PlaceFirst, UrgencyReward},
		{15.0, Requote, PlaceFirst, UrgencyReward},
	}

	for _, c := range cases {
		got := th.Classify(c.distance)
		require.Equal(t, c.action, got.Action, "distance %s: action", c.distance)
		require.Equal(t, c.urgency, got.Urgency, "distance %s: urgency", c.distance)
		if got.Action != Hold {
			require.Equal(t, c.sequence, got.Sequence, "distance %s: sequence", c.distance)
		}
	}
}

// The dead band is the only zone that produces no requests. Everything outside
// it must produce an action, otherwise an order could drift out of the reward
// band and stay there.
func TestDeadBandIsExactlyTheKeepRange(t *testing.T) {
	th := thresholds()

	rapid.Check(t, func(t *rapid.T) {
		d := money.Bps(rapid.Float64Range(-20, 40).Draw(t, "distance"))
		inKeepRange := d >= th.MinKeepBps && d <= th.MaxKeepBps

		require.Equal(t, inKeepRange, th.Classify(d).Action == Hold,
			"distance %s: hold must coincide with the keep range", d)
	})
}

// Approaching price must never be handled place-first: publishing a new order
// while the endangered one is still resting doubles the exposure at exactly
// the wrong moment.
func TestDangerZoneAlwaysCancelsFirst(t *testing.T) {
	th := thresholds()

	rapid.Check(t, func(t *rapid.T) {
		// The danger zone is open at the top: at exactly panicBps the order is
		// already handled place-first, because presence outweighs a risk that
		// has not materialised yet.
		d := money.Bps(rapid.Float64Range(-10, th.PanicBps.Float64()-0.001).Draw(t, "distance"))
		got := th.Classify(d)

		require.Equal(t, CancelFirst, got.Sequence, "distance %s must cancel first", d)
		require.Equal(t, UrgencySafety, got.Urgency)
	})
}

// Drifting outward costs rewards but carries no execution risk, so presence in
// the book takes precedence.
func TestOutwardDriftAlwaysPlacesFirst(t *testing.T) {
	th := thresholds()

	rapid.Check(t, func(t *rapid.T) {
		d := money.Bps(rapid.Float64Range(th.MaxKeepBps.Float64()+0.01, 100).Draw(t, "distance"))
		got := th.Classify(d)

		require.Equal(t, Requote, got.Action)
		require.Equal(t, PlaceFirst, got.Sequence, "distance %s must place first", d)
		require.Equal(t, UrgencyReward, got.Urgency)
	})
}

// A quote resting anywhere inside the dead band must survive an arbitrary walk
// that stays inside it: this is what keeps the request rate bounded.
func TestNoFlappingInsideDeadBand(t *testing.T) {
	th := thresholds()

	rapid.Check(t, func(t *rapid.T) {
		steps := rapid.SliceOfN(rapid.Float64Range(
			th.MinKeepBps.Float64(), th.MaxKeepBps.Float64()), 1, 200).Draw(t, "walk")

		for _, step := range steps {
			require.Equal(t, Hold, th.Classify(money.Bps(step)).Action,
				"distance %f inside the dead band must not trigger a requote", step)
		}
	})
}

func TestValidateRejectsMisorderedThresholds(t *testing.T) {
	require.NoError(t, thresholds().Validate())

	broken := thresholds()
	broken.MinKeepBps = broken.TargetBps + 1
	require.ErrorContains(t, broken.Validate(), "minKeep < target")

	negative := thresholds()
	negative.PullBps = -1
	require.ErrorContains(t, negative.Validate(), "negative")

	outside := thresholds()
	outside.MaxKeepBps = outside.BandBps + 1
	require.ErrorContains(t, outside.Validate(), "reward band")
}

func TestLadderDistancesAreCenteredOnTarget(t *testing.T) {
	ladder := Ladder{Layers: 3, SpreadBps: 1.0}
	require.Equal(t, []money.Bps{6.5, 7.5, 8.5}, ladder.Distances(7.5))

	require.Equal(t, []money.Bps{7.5}, Ladder{Layers: 1}.Distances(7.5))
	require.Equal(t, []money.Bps{7.0, 8.0}, Ladder{Layers: 2, SpreadBps: 1.0}.Distances(7.5))
}

func TestLadderMeanEqualsTarget(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		layers := rapid.IntRange(1, 9).Draw(t, "layers")
		spread := money.Bps(rapid.Float64Range(0.1, 2).Draw(t, "spread"))
		target := money.Bps(rapid.Float64Range(1, 20).Draw(t, "target"))

		distances := Ladder{Layers: layers, SpreadBps: spread}.Distances(target)
		require.Len(t, distances, layers)

		sum := 0.0
		for _, d := range distances {
			sum += d.Float64()
		}
		require.InDelta(t, target.Float64(), sum/float64(layers), 1e-9,
			"the ladder must stay centered on the target distance")
	})
}

// A rung outside the keep range would be requoted the instant it appears,
// producing an endless loop of placements.
func TestLadderFitsWithinKeepRange(t *testing.T) {
	th := thresholds()

	require.True(t, Ladder{Layers: 3, SpreadBps: 1.0}.Fits(th))
	require.True(t, Ladder{Layers: 1}.Fits(th))
	require.False(t, Ladder{Layers: 5, SpreadBps: 2.0}.Fits(th),
		"a wide ladder must be rejected")

	// Every rung of a fitting ladder must land in the dead band.
	for _, d := range (Ladder{Layers: 3, SpreadBps: 1.0}).Distances(th.TargetBps) {
		require.Equal(t, Hold, th.Classify(d).Action, "rung at %s must rest quietly", d)
	}
}

// The measured requote latency is about 400 ms (PLAN.md section 2.4); during
// that window a single-rung side contributes nothing at all.
func TestPresenceLoss(t *testing.T) {
	require.Equal(t, 1.0, Ladder{Layers: 1}.PresenceLoss())
	require.InDelta(t, 0.5, Ladder{Layers: 2}.PresenceLoss(), 1e-9)
	require.InDelta(t, 1.0/3, Ladder{Layers: 3, SpreadBps: 1}.PresenceLoss(), 1e-9)
}
