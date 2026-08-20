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
	// WindowHalfLife is required (Config's own doc: a load-bearing parameter
	// should not have a quiet off switch). Set equal to Window so tests running
	// over the usual few minutes of simulated time see negligible decay and the
	// pre-19-August assertions about "the outlier drives the estimate" keep
	// meaning what they said; tests that specifically probe decay timing set
	// their own shorter half-life.
	return Config{
		Window: 20 * time.Minute, WindowHalfLife: 20 * time.Minute,
		MinSamples: 40, FullConfidenceAt: 100,
	}
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

	// The last 49 ticks after the outlier (147s) already cost fast a little of
	// its twenty-minute half-life — expected now that fast, not a flat raw
	// max, is what drives JumpBps. Tolerance widened from 0.01 to cover that
	// real decay without hiding an actual regression (P95 would leak far more
	// than this if the outlier stopped setting the measure at all).
	est := e.Estimate(at)
	require.InDelta(t, 4.4, est.JumpBps.Float64(), 0.5, "the outlier must set the risk measure")
	require.Less(t, est.P95Bps.Float64(), 1.0, "the percentile stays blind to it")
}

// An outlier's grip on the estimate fades continuously, not at the raw
// window's edge.
//
// Before 19 August, caution held at a flat plateau for as long as the tick
// stayed inside Window, then dropped all at once. Measured on the live account
// that day, where Window was eight minutes: 52% of these plateaus outlasted
// five minutes, and almost all of the long ones were a single unchanging
// number — one tick, not a cluster, held by the shape of the window rather
// than by anything still happening. fast is deliberately shorter-lived than
// Window so relief arrives on its own clock.
func TestOutlierFadesBeforeTheRawWindowWould(t *testing.T) {
	cfg := Config{Window: 20 * time.Minute, WindowHalfLife: time.Minute, MinSamples: 2, FullConfidenceAt: 5}
	e := New(cfg)

	// At least minGapsForTick (8) quiet ticks before the spike, or the tick
	// calibration is not known yet and Observe drops the jump on the floor
	// before it ever reaches fast — a warm-up rule this test must respect
	// like any other caller, not a special case. The spike is also the last
	// element: feed's returned time is the spike's own, not a few ticks past
	// it, which is what "fresh, full caution" below actually means to check.
	moves := flat(10)
	moves = append(moves, 5.0)
	at := feed(e, money.MustParsePrice("100"), moves)
	require.InDelta(t, 5.0, e.Estimate(at).JumpBps.Float64(), 0.01, "fresh, full caution")

	// One half-life later the raw sample is still well inside the twenty
	// minute window — the old mechanism would still report a flat 5.0 here.
	later := e.Estimate(at.Add(time.Minute))
	require.InDelta(t, 5.0, later.WindowMaxBps.Float64(), 0.01,
		"the raw window has not forgotten anything yet")
	require.InDelta(t, 2.5, later.JumpBps.Float64(), 0.1,
		"but the primary measure has already halved")
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
// of a jump close to zero: any multiple of zero is zero.
//
// The case is not the weekend — that is handled by reward.Schedule, which stops
// the actor quoting outright. It is the intraday shape the live fill of
// 6 August took: hours of sub-basis-point ticks, then 5.38 bps in a single one.
// The window is twenty minutes wide and would have forgotten the burst long
// before the market did.
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

// Memory is a floor, never a ceiling: whichever record is still less decayed
// governs, and a memory tuned to fade faster than fast must not drag the
// estimate down below what fast still shows.
func TestFastWinsOverAFasterFadingMemory(t *testing.T) {
	// config()'s fast half-life is twenty minutes; memory here fades in five,
	// so after fifteen minutes memory has faded further than fast has.
	e := New(memoryConfig(5 * time.Minute))
	at := feed(e, money.MustParsePrice("100"), append(append(flat(10), 5), flat(300)...))

	est := e.Estimate(at)
	require.Less(t, float64(est.MemoryBps), float64(est.FastBps),
		"memory fades faster in this configuration")
	require.Equal(t, est.FastBps, est.JumpBps, "the less-decayed record governs")
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

// Disabling memory removes only the slow floor, not the measure itself: fast
// is a separate, always-on term and keeps working without it.
func TestMemoryIsOptional(t *testing.T) {
	e := New(config())
	at := feed(e, money.MustParsePrice("100"), append(append(flat(10), 4.4), flat(1200)...))

	est := e.Estimate(at)
	require.Zero(t, est.MemoryBps, "memory never armed")
	require.Greater(t, est.JumpBps.Float64(), 0.0,
		"fast does not depend on memory being configured")
}

// --- The record bounded by the live window ---

func boundedConfig(halfLife time.Duration, multiple float64) Config {
	c := memoryConfig(halfLife)
	c.MemoryWindowMultiple = multiple
	return c
}

// calm returns a path that jitters by a fraction of a basis point, the way a
// quiet gold session actually looks. A perfectly flat path is a different case
// and is covered separately.
func calm(n int) []float64 {
	out := make([]float64, n)
	for i := range out {
		if i%2 == 0 {
			out[i] = 0.5
		} else {
			out[i] = -0.5
		}
	}
	return out
}

// The live episode of 7 August, reproduced.
//
// One tick of 15 bps, then a market that goes truly quiet. The unbounded
// record kept demanding fifteen for most of a working day; bounded by a
// multiple of a reference that itself converges towards what the market
// currently shows, it does not.
//
// The horizon moved from one hour to three after 19 August, when fast replaced
// the raw window as bounded's reference (see bounded's own doc for why the raw
// window could no longer be used there: it would have let the original
// plateau bug back in through this exact door). fast's own half-life means
// bounded's ceiling now takes longer than the old flat 20-minute cliff to
// first start relaxing after a spike this large — proportionally, exactly as
// it should: a graduated release tied to the size of the spike is the
// improvement over a fixed cliff that released a 3 bps tick and a 15 bps one
// on the same clock. By three hours (nine of fast's own half-lives) the two
// paths are unambiguously apart either way.
//
// Truly flat, not calm(): calm's continuous ±0.5 bps ticks would keep
// re-arming fast at that floor forever — real ambient noise, correctly
// tracked, but it would leave the bounded side settling at 0.5×multiple
// instead of decaying toward what an empty market actually shows.
func TestBoundedRecordFollowsTheWindowBackDown(t *testing.T) {
	spike := append(append(flat(50), 15.0), flat(3600)...)

	unbounded := New(memoryConfig(6 * time.Hour))
	at := feed(unbounded, money.MustParsePrice("100"), spike)
	loose := unbounded.Estimate(at)

	bounded := New(boundedConfig(6*time.Hour, 3))
	at = feed(bounded, money.MustParsePrice("100"), spike)
	tight := bounded.Estimate(at)

	require.Zero(t, loose.WindowMaxBps, "nothing moved for the whole raw window")
	require.Greater(t, loose.JumpBps.Float64(), 8.0,
		"unbounded, one observation still governs hours later")
	require.Less(t, tight.JumpBps.Float64(), 0.5,
		"bounded, the record has decayed to near what an empty market shows")
}

// While the burst is still inside the window the bound must not touch it:
// that is the moment the record exists for.
func TestBoundDoesNotWeakenProtectionDuringTheBurst(t *testing.T) {
	e := New(boundedConfig(6*time.Hour, 3))
	at := feed(e, money.MustParsePrice("100"), append(append(calm(50), 15.0), calm(50)...))

	est := e.Estimate(at)
	require.InDelta(t, 15.0, est.WindowMaxBps.Float64(), 0.1)
	require.InDelta(t, 15.0, est.JumpBps.Float64(), 0.1,
		"full protection while the market has just shown what it can do")
}

// A window that has not warmed up says nothing about the market, and reading
// its maximum as evidence of calm would erase the record exactly when it is the
// only thing left — right after a reconnect.
func TestBoundIsNotAppliedBeforeTheEstimateIsReady(t *testing.T) {
	e := New(boundedConfig(6*time.Hour, 3))
	at := feed(e, money.MustParsePrice("100"), append(append(calm(50), 15.0), calm(50)...))

	e.Reset()
	after := at.Add(time.Minute)
	est := e.Estimate(after)

	require.False(t, est.Ready, "the window was cleared by the reconnect")
	require.Greater(t, est.JumpBps.Float64(), 14.0,
		"the record survives the reconnect unbounded")
}

// A reference of exactly zero carries no information about current magnitude
// and must not erase memory by multiplying it by zero.
//
// Exercised directly rather than through feed(): that is now the point of
// decaying instead of snapshotting a raw window. Before 19 August a market
// that had not ticked at all since a spike reached this state the moment the
// spike aged out of Window, a real and fairly common shape. fast
// decays continuously instead, so reaching literal zero from a real spike
// takes on the order of fifty half-lives — not a scenario ordinary replay can
// reach, which is exactly the property the fix set out to have. The exemption
// itself is worth keeping as a defensive floor regardless: something upstream
// could still hand it exactly zero.
func TestZeroFastRecordDoesNotEraseMemory(t *testing.T) {
	e := New(boundedConfig(6*time.Hour, 3))
	e.memory.observe(15.0, epoch)

	got := e.bounded(Estimate{Ready: true, MemoryBps: e.memory.decayed(epoch)}, 0)
	require.InDelta(t, 15.0, float64(got), 0.01, "must not be multiplied down to zero")
}

// Zero keeps the previous behaviour, so the bound can be switched off in place.
func TestZeroMultipleLeavesTheRecordUnbounded(t *testing.T) {
	spike := append(append(calm(50), 15.0), calm(1200)...)

	off := New(boundedConfig(6*time.Hour, 0))
	at := feed(off, money.MustParsePrice("100"), spike)

	require.Greater(t, off.Estimate(at).JumpBps.Float64(), 13.0)
}

// Разрыв соединения сам по себе не доказывает, что рынок стал другим.
//
// 10 августа биржа отдала 36 разрывов за восемь часов, почти все короче секунды
// (502 на редиале). Пока окно обнулялось на каждом, актор терял минуту прогрева
// per разрыв — минуту присутствия, то есть предмета заработка, — и, что хуже,
// оставался с неограниченной памятью: Estimate.bounded требует Ready.
func TestShortGapKeepsTheWindowAndTheRightToQuote(t *testing.T) {
	e := New(config())
	moves := make([]float64, 60)
	for i := range moves {
		moves[i] = 0.2
	}
	at := feed(e, money.MustParsePrice("4300"), moves)

	before := e.Estimate(at)
	require.True(t, before.Ready)

	// Пять тактов тишины: больше терпимости к пропуску, меньше перерыва.
	at = at.Add(15 * time.Second)
	e.Observe(money.MustParsePrice("4305"), at)

	after := e.Estimate(at)
	require.True(t, after.Ready, "a blink must not cost the right to quote")
	require.Equal(t, before.Samples, after.Samples,
		"the splice is skipped as a sample, but the window survives")
}

// Настоящий перерыв — другое дело: накопленные замеры описывают рынок,
// которого больше нет, и право котировать зарабатывается заново.
func TestLongOutageForgetsTheWindow(t *testing.T) {
	cfg := config()
	cfg.MemoryHalfLife = 6 * time.Hour
	e := New(cfg)
	moves := make([]float64, 60)
	for i := range moves {
		moves[i] = 0.2
	}
	at := feed(e, money.MustParsePrice("4300"), moves)
	require.True(t, e.Estimate(at).Ready)

	at = at.Add(2 * time.Minute)
	e.Observe(money.MustParsePrice("4330"), at)

	out := e.Estimate(at)
	require.False(t, out.Ready, "the window no longer describes this market")
	require.Zero(t, out.Samples)
	require.Positive(t, out.MemoryBps, "the record of large jumps outlives the window")
}
