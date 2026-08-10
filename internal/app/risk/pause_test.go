package risk

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"

	domain "github.com/nd/mm-standx/internal/domain/risk"
	"github.com/nd/mm-standx/internal/events"
	"github.com/nd/mm-standx/internal/ports"
)

type stubClock struct{ now time.Time }

func (c *stubClock) Now() time.Time { return c.now }
func (c *stubClock) After(time.Duration) <-chan time.Time {
	return make(chan time.Time)
}
func (c *stubClock) NewTicker(time.Duration) ports.Ticker       { return stubTicker{} }
func (c *stubClock) Sleep(context.Context, time.Duration) error { return nil }

type stubTicker struct{}

func (stubTicker) C() <-chan time.Time { return make(chan time.Time) }
func (stubTicker) Stop()               {}

func guard(t *testing.T, limits domain.Limits, now time.Time) (*Guard, *stubClock, *events.Bus) {
	t.Helper()

	clk := &stubClock{now: now}
	bus := events.New(nil)
	return &Guard{
		limits:  limits,
		ledger:  domain.New(limits),
		bus:     bus,
		clock:   clk,
		metrics: ports.NopMetrics{},
		log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}, clk, bus
}

func at(hour, minute int) time.Time {
	return time.Date(2026, 8, 10, hour, minute, 0, 0, time.UTC)
}

// Частота заливов обязана доходить до акторов как пауза со сроком, а не как
// остановка. Именно эта разница и была отказом: условие спадало через час,
// а запрет держался до полуночи UTC и переживал перезапуск.
func TestFillRatePublishesAPauseWithADeadline(t *testing.T) {
	ctx := context.Background()
	g, clk, bus := guard(t, domain.Limits{MaxFillsPerHour: 3, MaxDailyLossUSD: 100, TripOnDrawdown: true}, at(10, 5))

	var paused []events.QuotingPaused
	var resumed int
	var tripped int
	events.Subscribe(bus, "test", func(_ context.Context, ev events.QuotingPaused) {
		paused = append(paused, ev)
	})
	events.Subscribe(bus, "test", func(_ context.Context, ev events.QuotingResumed) { resumed++ })
	events.Subscribe(bus, "test", func(_ context.Context, ev events.KillSwitchTripped) { tripped++ })

	for i := range 3 {
		g.ledger.OnFill(at(10, i), domain.Fill{At: at(10, i), StartsEpisode: true})
	}
	g.evaluate(ctx)

	require.Len(t, paused, 1)
	require.Equal(t, at(11, 0), paused[0].Until)
	require.Zero(t, tripped, "a sliding-hour rate must never arm the persistent switch")

	// Повторная оценка того же состояния молчит: объявляются изменения.
	g.evaluate(ctx)
	require.Len(t, paused, 1)

	clk.now = at(11, 1)
	g.evaluate(ctx)
	require.Equal(t, 1, resumed, "the guard has to announce the release too")
	require.Zero(t, tripped)
}

// Остановка снимается сменой суток UTC, и актору об этом нужно сказать: у него
// запрет хранится флагом, поднять который самому ему нечем.
func TestClearedKillSwitchIsAnnounced(t *testing.T) {
	ctx := context.Background()
	g, clk, bus := guard(t, domain.Limits{MaxDailyLossUSD: 10, TripOnDrawdown: true}, at(10, 0))

	var tripped, cleared int
	events.Subscribe(bus, "test", func(_ context.Context, ev events.KillSwitchTripped) { tripped++ })
	events.Subscribe(bus, "test", func(_ context.Context, ev events.KillSwitchCleared) { cleared++ })

	g.ledger.OnTrade(at(10, 0), domain.Execution{
		CashUSD: decimal.NewFromInt(-20),
		FeeUSD:  decimal.Zero,
	})
	g.evaluate(ctx)
	require.Equal(t, 1, tripped)
	require.Zero(t, cleared)

	// Следующие сутки UTC: учёт начинается с чистого листа.
	clk.now = at(10, 0).Add(24 * time.Hour)
	g.evaluate(ctx)
	require.Equal(t, 1, cleared)
}
