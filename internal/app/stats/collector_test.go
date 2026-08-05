package stats_test

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nd/mm-standx/internal/adapters/clock"
	"github.com/nd/mm-standx/internal/app/stats"
	"github.com/nd/mm-standx/internal/ports"
)

// fakeSource replays a scripted sequence of snapshots, one per Snapshot call.
type fakeSource struct {
	frames [][]ports.MetricSample
	n      int
}

func (f *fakeSource) Snapshot() []ports.MetricSample {
	if f.n >= len(f.frames) {
		return nil
	}
	out := f.frames[f.n]
	f.n++
	return out
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
	require.Len(t, entries, 1, "one file per day")

	data, err := os.ReadFile(filepath.Join(dir, entries[0].Name()))
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
	require.Eventually(t, func() bool { return src.n >= 10 }, time.Second, 5*time.Millisecond)
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

// A counter is cumulative, so the hour is described by how much it advanced.
func TestCounterIsSummarisedByDelta(t *testing.T) {
	src := &fakeSource{frames: [][]ports.MetricSample{
		counter(10), counter(12), counter(17),
	}}

	c, dir := collector(t, src)
	require.NoError(t, c.Start(t.Context()))
	require.Eventually(t, func() bool { return src.n >= 3 }, time.Second, 5*time.Millisecond)
	require.NoError(t, c.Stop(t.Context()))

	recs := read(t, dir)
	require.Len(t, recs, 1)
	require.EqualValues(t, 7, *recs[0].Delta, "17 − 10, not the raw 17")
	require.Nil(t, recs[0].P50, "a counter has no meaningful median")
}

// A restart resets counters to zero. A negative delta would not mean a negative
// number of events, so it is clamped rather than recorded as nonsense.
func TestCounterResetDoesNotProduceNegativeDelta(t *testing.T) {
	src := &fakeSource{frames: [][]ports.MetricSample{
		counter(100), counter(101), counter(2),
	}}

	c, dir := collector(t, src)
	require.NoError(t, c.Start(t.Context()))
	require.Eventually(t, func() bool { return src.n >= 3 }, time.Second, 5*time.Millisecond)
	require.NoError(t, c.Stop(t.Context()))

	require.EqualValues(t, 0, *read(t, dir)[0].Delta)
}

func TestDisabledWithoutDirectory(t *testing.T) {
	c := stats.New("", time.Millisecond, &fakeSource{}, clock.New(),
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.NoError(t, c.Start(t.Context()))
	require.NoError(t, c.Stop(t.Context()))
}
