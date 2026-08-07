package stats_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nd/mm-standx/internal/adapters/clock"
	"github.com/nd/mm-standx/internal/app/stats"
	"github.com/nd/mm-standx/internal/ports"
)

// fakeSource replays a scripted sequence of snapshots, one per Snapshot call.
type fakeSource struct {
	mu     sync.Mutex
	frames [][]ports.MetricSample
	n      int
}

func (f *fakeSource) Snapshot() []ports.MetricSample {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.n >= len(f.frames) {
		return nil
	}
	out := f.frames[f.n]
	f.n++
	return out
}

func (f *fakeSource) taken() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.n
}

func gauge(v float64) []ports.MetricSample {
	return []ports.MetricSample{{
		Name: "mm_half_spread_bps", Labels: map[string]string{"symbol": "XAU-USD"},
		Kind: ports.KindGauge, Value: v,
	}}
}

func counter(v float64) []ports.MetricSample {
	return []ports.MetricSample{{
		Name: "mm_fills_total", Labels: map[string]string{"symbol": "XAU-USD"},
		Kind: ports.KindCounter, Value: v,
	}}
}

func read(t *testing.T, dir string) []stats.Record {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)

	var hourly string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "hourly-") {
			require.Empty(t, hourly, "one hourly file per day")
			hourly = e.Name()
		}
	}
	require.NotEmpty(t, hourly, "the completed hour must be archived")

	data, err := os.ReadFile(filepath.Join(dir, hourly))
	require.NoError(t, err)

	var out []stats.Record
	dec := json.NewDecoder(bytes.NewReader(data))
	for {
		var r stats.Record
		if err := dec.Decode(&r); err != nil {
			break
		}
		out = append(out, r)
	}
	return out
}

func collector(t *testing.T, src ports.MetricsSource) (*stats.Collector, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "stats")
	c := stats.New(dir, time.Millisecond, src, clock.New(),
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	return c, dir
}

// A gauge is only meaningful at the instant it is read, so the hour is described
// by its quantiles: the median says what the hour was usually like, the edges
// say what it was like when it mattered.
func TestGaugeIsSummarisedByQuantiles(t *testing.T) {
	src := &fakeSource{}
	for _, v := range []float64{5, 1, 9, 3, 7, 2, 8, 4, 6, 10} {
		src.frames = append(src.frames, gauge(v))
	}

	c, dir := collector(t, src)
	require.NoError(t, c.Start(t.Context()))
	require.Eventually(t, func() bool { return src.taken() >= 10 }, time.Second, 5*time.Millisecond)
	require.NoError(t, c.Stop(t.Context()))

	recs := read(t, dir)
	require.Len(t, recs, 1)
	r := recs[0]

	require.Equal(t, "mm_half_spread_bps", r.Metric)
	require.Equal(t, "XAU-USD", r.Labels["symbol"])
	require.Equal(t, 10, r.Samples)
	require.EqualValues(t, 1, *r.Min)
	require.EqualValues(t, 10, *r.Max)
	require.EqualValues(t, 6, *r.P50)
	require.True(t, r.Partial, "an hour cut short by shutdown is marked")
}

// A counter is cumulative, so the hour is described by how much it advanced —
// and a series that did not exist when the hour began advanced from zero.
//
// The baseline used to be the first value seen inside the hour, which sounds
// equivalent and is not: a labelled counter materialises in the registry only
// with its first event, so that event became the baseline and vanished from the
// report. The fill of 6 August at 14:34 was recorded live and came out of the
// rollup as delta 0.
func TestCounterCountsEventsThatCreatedTheSeries(t *testing.T) {
	src := &fakeSource{frames: [][]ports.MetricSample{
		counter(10), counter(12), counter(17),
	}}

	c, dir := collector(t, src)
	require.NoError(t, c.Start(t.Context()))
	require.Eventually(t, func() bool { return src.taken() >= 3 }, time.Second, 5*time.Millisecond)
	require.NoError(t, c.Stop(t.Context()))

	recs := read(t, dir)
	require.Len(t, recs, 1)
	require.EqualValues(t, 17, *recs[0].Delta,
		"the registry starts at zero with the process, so all seventeen happened here")
	require.Nil(t, recs[0].P50, "a counter has no meaningful median")
}

// A counter that goes backwards did not record a negative number of events.
// Only a process restart can do that, and the collector restarts with it, so
// the clamp is a guard rather than a path the data normally takes.
func TestCounterResetDoesNotProduceNegativeDelta(t *testing.T) {
	src := &fakeSource{frames: [][]ports.MetricSample{
		counter(100), counter(101), counter(2),
	}}

	c, dir := collector(t, src)
	require.NoError(t, c.Start(t.Context()))
	require.Eventually(t, func() bool { return src.taken() >= 3 }, time.Second, 5*time.Millisecond)
	require.NoError(t, c.Stop(t.Context()))

	require.GreaterOrEqual(t, *read(t, dir)[0].Delta, 0.0, "never a negative count")
}

func TestDisabledWithoutDirectory(t *testing.T) {
	c := stats.New("", time.Millisecond, &fakeSource{}, clock.New(),
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.NoError(t, c.Start(t.Context()))
	require.NoError(t, c.Stop(t.Context()))
}

// The hourly rollup is only complete at the end of an hour, so without a live
// snapshot the first fifty-nine minutes of a run would show nothing at all —
// indistinguishable from a broken collector for whoever just started the bot.
func TestCurrentHourIsReadableImmediately(t *testing.T) {
	src := &fakeSource{frames: [][]ports.MetricSample{gauge(3), gauge(5), gauge(4)}}

	c, dir := collector(t, src)
	require.NoError(t, c.Start(t.Context()))
	require.Eventually(t, func() bool {
		_, err := os.Stat(filepath.Join(dir, "current.json"))
		return err == nil
	}, time.Second, 5*time.Millisecond, "the snapshot appears without waiting for the hour")

	data, err := os.ReadFile(filepath.Join(dir, "current.json"))
	require.NoError(t, err)
	require.Contains(t, string(data), "mm_half_spread_bps")
	require.Contains(t, string(data), `"partial":true`)

	require.NoError(t, c.Stop(t.Context()))
}

// The snapshot is written far less often than samples are taken: measurements
// accumulate in memory and only the result reaches disk. Writing every fifteen
// seconds would churn the disk for data nobody reads more than a few times an
// hour. The first write is immediate, so a fresh start shows something at once.
func TestSnapshotIsWrittenOnceThenThrottled(t *testing.T) {
	src := &fakeSource{}
	for range 40 {
		src.frames = append(src.frames, gauge(1))
	}

	c, dir := collector(t, src)
	require.NoError(t, c.Start(t.Context()))
	require.Eventually(t, func() bool { return src.taken() >= 40 }, time.Second, 5*time.Millisecond)

	info, err := os.Stat(filepath.Join(dir, "current.json"))
	require.NoError(t, err, "the first snapshot is immediate")
	first := info.ModTime()

	require.Eventually(t, func() bool { return src.taken() >= 40 }, time.Second, 5*time.Millisecond)
	info, err = os.Stat(filepath.Join(dir, "current.json"))
	require.NoError(t, err)
	require.Equal(t, first, info.ModTime(), "forty samples, still one write")

	require.NoError(t, c.Stop(t.Context()))
}

// steppedClock lets a test cross an hour boundary without waiting for one.
// Only Now is controlled; the ticker stays real so the collector loop runs.
type steppedClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *steppedClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *steppedClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func (c *steppedClock) After(d time.Duration) <-chan time.Time { return time.After(d) }
func (c *steppedClock) NewTicker(d time.Duration) ports.Ticker { return clock.New().NewTicker(d) }
func (c *steppedClock) Sleep(ctx context.Context, d time.Duration) error {
	return clock.New().Sleep(ctx, d)
}

// gatedSource hands out frames only up to a limit the test raises, so the
// clock can be moved across an hour boundary without racing the collector loop.
type gatedSource struct {
	mu     sync.Mutex
	frames [][]ports.MetricSample
	n      int
	limit  int
}

func (g *gatedSource) Snapshot() []ports.MetricSample {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.n >= g.limit || g.n >= len(g.frames) {
		return nil
	}
	out := g.frames[g.n]
	g.n++
	return out
}

func (g *gatedSource) taken() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.n
}

func (g *gatedSource) allow(n int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.limit = n
}

// The second hour counts from where the first one ended, not from zero and not
// from whatever the first sample of that hour happened to see.
func TestCounterBaselineCarriesAcrossTheHour(t *testing.T) {
	src := &gatedSource{limit: 3, frames: [][]ports.MetricSample{
		counter(1), counter(2), counter(3), counter(5), counter(8),
	}}
	clk := &steppedClock{now: time.Date(2026, 8, 7, 14, 0, 0, 0, time.UTC)}

	dir := t.TempDir()
	c := stats.New(dir, time.Millisecond, src, clk,
		slog.New(slog.NewTextHandler(io.Discard, nil)))

	require.NoError(t, c.Start(t.Context()))
	require.Eventually(t, func() bool { return src.taken() == 3 }, time.Second, 5*time.Millisecond)

	clk.advance(time.Hour)
	src.allow(5)
	require.Eventually(t, func() bool { return src.taken() == 5 }, time.Second, 5*time.Millisecond)
	require.NoError(t, c.Stop(t.Context()))

	byHour := map[string]float64{}
	for _, r := range read(t, dir) {
		if r.Delta != nil {
			byHour[r.Hour] = *r.Delta
		}
	}
	require.Len(t, byHour, 2, "two hours were rolled up")
	require.EqualValues(t, 3, byHour["2026-08-07T14:00:00Z"], "the series was born and reached three")
	require.EqualValues(t, 5, byHour["2026-08-07T15:00:00Z"], "8 − 3, across the boundary")
}
