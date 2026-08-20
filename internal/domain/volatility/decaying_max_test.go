package volatility

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

var decayEpoch = time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)

func TestDecayingMaxHalvesEveryHalfLife(t *testing.T) {
	var d decayingMax
	d.halfLife = time.Hour
	d.observe(8, decayEpoch)

	require.InDelta(t, 8, float64(d.decayed(decayEpoch)), 1e-9)
	require.InDelta(t, 4, float64(d.decayed(decayEpoch.Add(time.Hour))), 1e-9)
	require.InDelta(t, 1, float64(d.decayed(decayEpoch.Add(3*time.Hour))), 1e-9)
}

// A smaller observation must never pull the record down — only the clock can.
func TestDecayingMaxIgnoresASmallerObservation(t *testing.T) {
	var d decayingMax
	d.halfLife = time.Hour
	d.observe(8, decayEpoch)
	d.observe(1, decayEpoch.Add(time.Minute))

	require.InDelta(t, 8, float64(d.decayed(decayEpoch.Add(time.Minute))), 0.1,
		"one minute of a one hour half-life is negligible decay, and 1 must not have won")
}

// A comparable or bigger observation before full decay re-arms the record from
// its own moment, not from the original one — a genuine cluster keeps the
// record elevated for as long as it keeps producing them.
func TestDecayingMaxReArmsOnARepeatObservation(t *testing.T) {
	var d decayingMax
	d.halfLife = time.Hour
	d.observe(8, decayEpoch)

	d.observe(8, decayEpoch.Add(2*time.Hour)) // a second, equal jump two half-lives in
	require.InDelta(t, 8, float64(d.decayed(decayEpoch.Add(2*time.Hour))), 1e-9,
		"the second jump resets the clock, not just the value")
	require.InDelta(t, 4, float64(d.decayed(decayEpoch.Add(3*time.Hour))), 1e-9,
		"decay from the second jump's own moment, one half-life later")
}

// Sequential small observations compose the same as one direct decay call —
// this is what lets Observe() call remember() on every tick, including flat
// ones, without the decay clock silently resetting to "always fresh".
func TestDecayingMaxComposesAcrossManySmallSteps(t *testing.T) {
	direct := decayingMax{halfLife: time.Hour}
	direct.observe(8, decayEpoch)
	want := direct.decayed(decayEpoch.Add(time.Hour))

	stepped := decayingMax{halfLife: time.Hour}
	stepped.observe(8, decayEpoch)
	at := decayEpoch
	for range 60 {
		at = at.Add(time.Minute)
		stepped.observe(0, at) // sixty flat, zero-magnitude ticks
	}

	require.InDelta(t, float64(want), float64(stepped.decayed(at)), 1e-6)
}

func TestDecayingMaxStartsAtZero(t *testing.T) {
	var d decayingMax
	d.halfLife = time.Hour
	require.Zero(t, d.decayed(decayEpoch))
}

// Zero half-life is the documented off switch: observe becomes a no-op and the
// record never leaves zero, matching Config.WindowHalfLife's "required, not
// optional" contract having somewhere to actually disable to in tests.
func TestDecayingMaxWithZeroHalfLifeNeverRecords(t *testing.T) {
	var d decayingMax
	d.observe(100, decayEpoch)
	require.Zero(t, d.decayed(decayEpoch))
	require.Zero(t, d.decayed(decayEpoch.Add(time.Second)))
}

// decayed must not mutate: it is read on every service tick, and a mutating
// read would turn polling frequency into a parameter of the risk model.
func TestDecayingMaxDecayedDoesNotMutate(t *testing.T) {
	var d decayingMax
	d.halfLife = time.Hour
	d.observe(8, decayEpoch)

	_ = d.decayed(decayEpoch.Add(10 * time.Hour))
	require.InDelta(t, 4, float64(d.decayed(decayEpoch.Add(time.Hour))), 1e-9,
		"an earlier read must see the same record as if the later read never happened")
}
