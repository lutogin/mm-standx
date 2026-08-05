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

// The first few updates go into measuring the instrument's own cadence and
// produce no jumps, so readiness arrives that many ticks later.
func TestReadinessArrivesWithSamples(t *testing.T) {
	e := New(config())

	warmup := 39 + minGapsForTick - 1
	at := feed(e, money.MustParsePrice("63400"), flat(warmup))
	require.False(t, e.Estimate(at).Ready, "39 samples are not enough")

	at = at.Add(3 * time.Second)
	e.Observe(money.MustParsePrice("63400"), at)
	require.True(t, e.Estimate(at).Ready, "40 samples unlock quoting")
}

// At three seconds per tick the warm-up takes minutes, not hours: a little over
// two for the minimum, five for a mature estimate.
func TestWarmupDuration(t *testing.T) {
	cfg := config()
	ticks := cfg.MinSamples + minGapsForTick - 1
	require.Less(t, time.Duration(ticks)*3*time.Second, 3*time.Minute)
	require.Equal(t, 5*time.Minute, time.Duration(cfg.FullConfidenceAt)*3*time.Second)
}

// The three second cadence was measured on BTC, ETH and the metals. Less liquid
// instruments update far more slowly, and a hardcoded tick discarded almost
// every observation they produced: they never left warm-up and never quoted —
// silent not because they were dangerous but because the yardstick was foreign.
func TestSlowInstrumentStillWarmsUp(t *testing.T) {
	e := New(config())

	at := epoch
	price := money.MustParsePrice("323.75")
	e.Observe(price, at)
	for range 60 {
		at = at.Add(25 * time.Second) // TSLA-USD cadence, not BTC's
		price = price.MulRatio(decimal.NewFromFloat(1.0001))
		e.Observe(price, at)
	}

	est := e.Estimate(at)
	require.True(t, est.Ready, "a slow instrument must warm up on its own cadence")
	require.Equal(t, 25*time.Second, est.Tick)
	require.InDelta(t, 1.0, est.JumpBps.Float64(), 0.01)
}

// The blind window is whatever the instrument's cadence happens to be, so the
// gap filter has to reject stalls relative to that cadence, not to a constant.
func TestStallIsRejectedRelativeToTheMeasuredTick(t *testing.T) {
	e := New(config())

	at := epoch
	price := money.MustParsePrice("100")
	e.Observe(price, at)
	for range 20 {
		at = at.Add(25 * time.Second)
		e.Observe(price, at)
	}
	before := e.Estimate(at).Samples

	// Four cadences of silence, then a large move: a stall, not a tick.
	at = at.Add(100 * time.Second)
	e.Observe(price.MulRatio(decimal.NewFromFloat(1.005)), at)

	require.Equal(t, before, e.Estimate(at).Samples, "a stall must not become a 50 bps jump")
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
	e := New(Config{Window: 2 * time.Minute, MinSamples: 2, FullConfidenceAt: 5})

	moves := flat(29)
	moves[10] = 5.0
	at := feed(e, money.MustParsePrice("100"), moves)
	require.InDelta(t, 5.0, e.Estimate(at).JumpBps.Float64(), 0.01)

	// Once the window has rolled past the outlier the estimate relaxes.
	require.Zero(t, e.Estimate(at.Add(4*time.Minute)).Samples)
}

// A thin sample cannot have seen the tail, so the margin is inflated until it
// matures. Without this, the first minutes of a run would quote too close.
func TestThinSampleInflatesTheMargin(t *testing.T) {
	e := New(config())

	moves := flat(49)
	moves[10] = 2.0
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
	moves[10] = 2.0
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

	atUp := feed(up, money.MustParsePrice("100"), append(flat(10), 3.0))
	atDown := feed(down, money.MustParsePrice("100"), append(flat(10), -3.0))

	require.InDelta(t,
		up.Estimate(atUp).JumpBps.Float64(),
		down.Estimate(atDown).JumpBps.Float64(), 0.01,
		"a quote can be reached from either side")
}

// --- Decaying memory of large jumps ---

func memoryConfig(halfLife time.Duration) Config {
	c := config()
	c.MemoryHalfLife = halfLife
	return c
}

// The maturity correction cannot cover the case it was written for. Inflation
// multiplies the measured jump, and a frozen market produces a mature estimate
// of a jump close to zero: any multiple of zero is zero. This is precisely the
// weekend, when the underlying markets are shut — and Monday arrives with the
// volatility Friday had.
func TestMemoryOutlivesTheWindow(t *testing.T) {
	e := New(memoryConfig(6 * time.Hour))

	// One violent tick, then a market that goes completely still for an hour.
	at := feed(e, money.MustParsePrice("100"), append(append(flat(10), 4.4), flat(1200)...))

	est := e.Estimate(at)
	require.Zero(t, est.WindowMaxBps, "the jump has long left the twenty minute window")
	require.Greater(t, float64(est.MemoryBps), 3.0, "but the market showed what it can do")
	require.Equal(t, est.MemoryBps, est.JumpBps, "and memory is what binds the estimate")
}

func TestMemoryDecaysByHalfEveryHalfLife(t *testing.T) {
	e := New(memoryConfig(time.Hour))
	at := feed(e, money.MustParsePrice("100"), append(flat(10), 8))

	require.InDelta(t, 8, float64(e.Estimate(at).MemoryBps), 0.01)
	require.InDelta(t, 4, float64(e.Estimate(at.Add(time.Hour)).MemoryBps), 0.01)
	require.InDelta(t, 1, float64(e.Estimate(at.Add(3*time.Hour)).MemoryBps), 0.01)
}

// Memory is a floor, never a ceiling. It decays while the window does not, so
// a jump still inside the window always dominates its own faded record.
func TestWindowWinsOverAFadedRecord(t *testing.T) {
	// Half-life shorter than the window, so the record fades while the sample
	// that produced it is still being counted.
	e := New(memoryConfig(10 * time.Minute))
	at := feed(e, money.MustParsePrice("100"), append(append(flat(10), 5), flat(300)...))

	est := e.Estimate(at)
	require.InDelta(t, 5, float64(est.WindowMaxBps), 0.01, "still inside the twenty minute window")
	require.InDelta(t, 5*0.354, float64(est.MemoryBps), 0.05, "faded by a jump and a half")
	require.Equal(t, est.WindowMaxBps, est.JumpBps)
}

// A disconnect is no evidence that the market calmed down; if anything, load
// spikes and volatility spikes arrive together. Forgetting the memory would
// bring the bot back from a reconnect with an understated risk measure and move
// it closer to the market exactly when there is least reason to.
func TestReconnectForgetsTheWindowButNotTheMemory(t *testing.T) {
	e := New(memoryConfig(6 * time.Hour))
	at := feed(e, money.MustParsePrice("100"), append([]float64{5}, flat(60)...))

	before := e.Estimate(at).MemoryBps
	e.Reset()
	after := e.Estimate(at)

	require.Zero(t, after.Samples, "the window is gone, quoting has to be re-earned")
	require.False(t, after.Ready)
	require.Equal(t, before, after.MemoryBps, "the risk measure is not")
	require.Equal(t, before, after.JumpBps)
}

// Zero half-life keeps the original behaviour: the estimate is the window
// maximum and nothing else.
func TestMemoryIsOptional(t *testing.T) {
	e := New(config())
	at := feed(e, money.MustParsePrice("100"), append(append(flat(10), 4.4), flat(1200)...))

	est := e.Estimate(at)
	require.Zero(t, est.MemoryBps)
	require.Zero(t, est.JumpBps)
}
