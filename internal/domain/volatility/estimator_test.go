package volatility

import (
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"

	"github.com/nd/mm-standx/internal/domain/money"
)

var epoch = time.Unix(1_754_897_028, 0).UTC()

func config() Config {
	return Config{Window: 20 * time.Minute, MinSamples: 40, FullConfidenceAt: 100}
}

// feed replays a price path at the real three second cadence.
func feed(e *Estimator, start money.Price, movesBps []float64) time.Time {
	at := epoch
	price := start
	e.Observe(price, at)

	for _, bps := range movesBps {
		at = at.Add(3 * time.Second)
		ratio := decimal.NewFromFloat(1 + bps/10_000)
		price = price.MulRatio(ratio)
		e.Observe(price, at)
	}
	return at
}

func flat(n int) []float64 { return make([]float64, n) }

// A cold estimator must not authorise quoting. This is the warm-up: expressed
// as a property of the estimate rather than a separate startup phase, so that
// it also covers reconnects and stream gaps.
func TestColdEstimatorIsNotReady(t *testing.T) {
	e := New(config())

	est := e.Estimate(epoch)
	require.False(t, est.Ready, "quoting must wait for evidence")
	require.Zero(t, est.Samples)
}

func TestReadinessArrivesWithSamples(t *testing.T) {
	e := New(config())

	at := feed(e, money.MustParsePrice("63400"), flat(39))
	require.False(t, e.Estimate(at).Ready, "39 samples are not enough")

	at = at.Add(3 * time.Second)
	e.Observe(money.MustParsePrice("63400"), at)
	require.True(t, e.Estimate(at).Ready, "40 samples unlock quoting")
}

// At three seconds per tick the warm-up takes minutes, not hours: two for the
// minimum, five for a mature estimate.
func TestWarmupDuration(t *testing.T) {
	cfg := config()
	require.Equal(t, 2*time.Minute, time.Duration(cfg.MinSamples)*3*time.Second)
	require.Equal(t, 5*time.Minute, time.Duration(cfg.FullConfidenceAt)*3*time.Second)
}

// The measured tail is ten times fatter than the body, so the window maximum is
// the risk measure and the percentile is only for observability.
func TestMaximumDrivesTheEstimate(t *testing.T) {
	e := New(config())

	moves := flat(99)
	moves[50] = 4.4 // the outlier BTC actually produced on 2026-08-02
	at := feed(e, money.MustParsePrice("63400"), moves)

	est := e.Estimate(at)
	require.InDelta(t, 4.4, est.JumpBps.Float64(), 0.01, "the outlier must set the risk measure")
	require.Less(t, est.P95Bps.Float64(), 1.0, "the percentile stays blind to it")
}

// Caution must persist for the length of the window, then fade by itself.
func TestOutlierFadesWithTheWindow(t *testing.T) {
	e := New(Config{Window: time.Minute, MinSamples: 2, FullConfidenceAt: 5})

	moves := flat(19)
	moves[0] = 5.0
	at := feed(e, money.MustParsePrice("100"), moves)
	require.InDelta(t, 5.0, e.Estimate(at).JumpBps.Float64(), 0.01)

	// Once the window has rolled past the outlier the estimate relaxes.
	require.Zero(t, e.Estimate(at.Add(2*time.Minute)).Samples)
}

// A thin sample cannot have seen the tail, so the margin is inflated until it
// matures. Without this, the first minutes of a run would quote too close.
func TestThinSampleInflatesTheMargin(t *testing.T) {
	e := New(config())

	moves := flat(49)
	moves[0] = 2.0
	at := feed(e, money.MustParsePrice("100"), moves)

	est := e.Estimate(at)
	require.True(t, est.Ready)
	require.Less(t, est.Confidence, 1.0)
	require.Greater(t, est.EffectiveJump().Float64(), est.JumpBps.Float64(),
		"an immature estimate must be treated as worse than observed")
}

func TestMatureSampleIsTakenAtFaceValue(t *testing.T) {
	e := New(config())

	moves := flat(120)
	moves[0] = 2.0
	at := feed(e, money.MustParsePrice("100"), moves)

	est := e.Estimate(at)
	require.Equal(t, 1.0, est.Confidence)
	require.InDelta(t, est.JumpBps.Float64(), est.EffectiveJump().Float64(), 1e-9)
}

// A gap in the stream is not a tick. Recording the whole gap as a single jump
// would inflate the estimate on evidence that never existed.
func TestGapsAreNotCountedAsJumps(t *testing.T) {
	e := New(config())

	e.Observe(money.MustParsePrice("100"), epoch)
	e.Observe(money.MustParsePrice("110"), epoch.Add(time.Hour))

	require.Zero(t, e.Estimate(epoch.Add(time.Hour)).Samples,
		"a one hour gap must not become a 1000 bps jump")
}

// After a disconnect the history describes a different market, and splicing
// across the break would manufacture a jump that never happened.
func TestResetClearsHistory(t *testing.T) {
	e := New(config())
	at := feed(e, money.MustParsePrice("100"), flat(60))
	require.True(t, e.Estimate(at).Ready)

	e.Reset()
	require.False(t, e.Estimate(at).Ready, "a reconnect must re-earn the right to quote")

	// The first observation after a reset only sets the reference point.
	e.Observe(money.MustParsePrice("200"), at.Add(3*time.Second))
	require.Zero(t, e.Estimate(at.Add(3*time.Second)).Samples)
}

func TestDirectionIsIrrelevant(t *testing.T) {
	up := New(config())
	down := New(config())

	atUp := feed(up, money.MustParsePrice("100"), []float64{3.0})
	atDown := feed(down, money.MustParsePrice("100"), []float64{-3.0})

	require.InDelta(t,
		up.Estimate(atUp).JumpBps.Float64(),
		down.Estimate(atDown).JumpBps.Float64(), 0.01,
		"a quote can be reached from either side")
}
