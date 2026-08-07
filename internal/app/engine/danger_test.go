package engine

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"

	"github.com/nd/mm-standx/internal/config"
	"github.com/nd/mm-standx/internal/domain/market"
	"github.com/nd/mm-standx/internal/domain/money"
	"github.com/nd/mm-standx/internal/domain/policy"
	"github.com/nd/mm-standx/internal/domain/quotes"
	"github.com/nd/mm-standx/internal/domain/reward"
	"github.com/nd/mm-standx/internal/domain/trading"
	"github.com/nd/mm-standx/internal/domain/uptime"
	"github.com/nd/mm-standx/internal/domain/volatility"
	"github.com/nd/mm-standx/internal/events"
	"github.com/nd/mm-standx/internal/ports"
)

// dangerEngine builds an actor whose risk estimate is mature and whose book is
// wide enough for the danger thresholds to be comfortably above zero.
func dangerEngine(t *testing.T) (*Engine, *recordingGateway, *testClock) {
	t.Helper()

	clk := &testClock{now: time.Date(2026, 8, 6, 14, 0, 0, 0, time.UTC)}
	gw := &recordingGateway{}

	e := New(
		Params{
			Symbol: "XAU-USD",
			Live:   true,
			// The live instrument: a ten cent price tick and three decimals of
			// quantity. Without it sizing snaps every rung to zero and the
			// actor refuses to place for a reason that has nothing to do with
			// the case under test.
			Spec: market.SymbolSpec{
				Symbol:            "XAU-USD",
				PriceTickDecimals: 1,
				QtyTickDecimals:   3,
				MinOrderQty:       money.MustParseQty("0.001"),
				MaxOrderQty:       money.MustParseQty("100"),
				MaxLeverage:       10,
				Enabled:           true,
			},
			Program: reward.Program{
				Curve:                reward.Curve{BandBps: 10, MaxWeightPct: 200, MinWeightPct: 40},
				Tiers:                []reward.Tier{{ThresholdPct: 70, Multiplier: 1}, {ThresholdPct: 50, Multiplier: 0.5}},
				UnitSize:             decimal.NewFromInt(30),
				PerSideCap:           decimal.NewFromInt(60),
				MaxMakerHoursPerHour: 2,
				OffHoursMultiplier:   1,
			},
			Health: config.Health{AckTimeout: 2 * time.Second},
			Config: config.Symbol{
				SymbolRisk: config.SymbolRisk{Leverage: 4},
				Quote: config.Quote{
					LadderLayers:    1,
					MinRest:         3200 * time.Millisecond,
					RequoteCooldown: 250 * time.Millisecond,
				},
			},
			Appetite: policy.Appetite{
				JumpMultiple:       1.25,
				FloorMarginBps:     1.5,
				DeadBandJumps:      2.0,
				MinDeadBandBps:     1.2,
				ReentryDeadBandBps: 1.0,
			},
			Bounds: policy.Bounds{MinBps: 5.5, MaxBps: 9.5},
			Volatility: volatility.Config{
				Window:           20 * time.Minute,
				MinSamples:       40,
				FullConfidenceAt: 100,
				MemoryHalfLife:   6 * time.Hour,
			},
			MaxNetPosition: money.MustParseQty("0"),
		},
		Deps{
			Gateway: gw,
			Bus:     events.New(nil),
			Clock:   clk,
			Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		},
	)

	primeVolatility(t, e, clk)

	e.mark = money.MustParsePrice("4276")
	// A two dollar spread puts the pull threshold near 2.3 bps, so a rung one
	// dollar away from the mark is unambiguously in danger.
	e.book = market.NewBook("XAU-USD",
		[]market.Level{{Price: money.MustParsePrice("4275"), Qty: money.MustParseQty("1")}},
		[]market.Level{{Price: money.MustParsePrice("4277"), Qty: money.MustParseQty("1")}},
		clk.now)
	return e, gw, clk
}

// primeVolatility feeds the estimator enough observations to leave warm-up.
//
// A mature estimate is a precondition for quoting at all, and the window is
// twenty minutes wide: a test that moves the clock further than that has to
// prime again, or the actor declines for a reason unrelated to the case.
func primeVolatility(t *testing.T, e *Engine, clk *testClock) {
	t.Helper()

	clk.now = clk.now.Add(-70 * time.Second)
	for i := range 50 {
		price := money.MustParsePrice("4276")
		if i%2 == 1 {
			price = money.MustParsePrice("4276.1")
		}
		e.vol.Observe(price, clk.now)
		clk.advance(1400 * time.Millisecond)
	}
	require.True(t, e.vol.Estimate(clk.now).Ready)
}

// liveRung puts a confirmed order on the given side at the given price.
func liveRung(e *Engine, side market.Side, id trading.ClOrdID, price string, at time.Time) {
	l := e.ladderFor(side)
	l.OnPlaceSent(0, id, money.MustParsePrice(price), money.MustParseQty("0.008"), at)
	l.OnOrderUpdate(trading.Order{ClOrdID: id, Status: trading.StatusOpen}, at)
}

// The requote cooldown exists to protect the request budget, not to delay a
// panic pull. This rung is still one measured jump away from being touched;
// waiting until the pull threshold would spend the only warning the fast book
// signal provides.
func TestPanicPullIsNotDelayedByTheRequoteCooldown(t *testing.T) {
	ctx := context.Background()
	e, gw, clk := dangerEngine(t)

	liveRung(e, market.Sell, "ask-1", "4277", clk.now)

	// The actor acted just now: the cooldown is fully in force.
	e.lastAction = clk.now
	e.evaluate(ctx, false)

	require.Contains(t, gw.cancelled, trading.ClOrdID("ask-1"),
		"a rung inside the panic zone must be pulled regardless of the cooldown")
}

// A panic pull is one exchange batch, not one acknowledged request per rung.
// The latter keeps the final orders exposed for several network round trips.
func TestPanicPullCancelsEveryExposedRungInOneBatch(t *testing.T) {
	ctx := context.Background()
	e, gw, clk := dangerEngine(t)
	e.bids = quotes.New(market.Buy, 3)
	e.asks = quotes.New(market.Sell, 3)

	for i, id := range []trading.ClOrdID{"ask-1", "ask-2", "ask-3"} {
		e.asks.OnPlaceSent(i, id, money.MustParsePrice("4277"), money.MustParseQty("0.008"), clk.now)
		e.asks.OnOrderUpdate(trading.Order{ClOrdID: id, Status: trading.StatusOpen}, clk.now)
	}

	e.evaluate(ctx, false)

	require.Equal(t, 1, gw.batchCalls)
	require.Zero(t, gw.cancelCalls)
	require.ElementsMatch(t,
		[]trading.ClOrdID{"ask-1", "ask-2", "ask-3"}, gw.cancelled)
}

// The cooldown must still do its job on the reward path, or the actor would
// requote on every book update and exhaust the request budget in a minute.
func TestSafeRungIsNotRequotedDuringTheCooldown(t *testing.T) {
	ctx := context.Background()
	e, gw, clk := dangerEngine(t)

	// Well outside the reward band: the normal path would want to requote it.
	liveRung(e, market.Sell, "ask-1", "4300", clk.now)

	e.lastAction = clk.now
	e.evaluate(ctx, false)

	require.Empty(t, gw.cancelled)
	require.Empty(t, gw.placed)
}

var _ ports.OrderGateway = (*recordingGateway)(nil)

// offHoursEngine is the danger fixture with the gold schedule attached: the
// reference market shuts on Friday evening and the exchange pays a tenth for
// every hour it stays shut.
func offHoursEngine(t *testing.T) (*Engine, *recordingGateway, *testClock) {
	t.Helper()
	e, gw, clk := dangerEngine(t)

	window, err := reward.ParseWindow("Fri 20:00", "Mon 00:00")
	require.NoError(t, err)
	e.program.OffHours = reward.Schedule{Weekly: window}
	e.program.OffHoursMultiplier = 0.1
	e.hour = uptime.New(e.program, uptime.Policy{
		TargetPct: 80, HardFloorPct: 72, ReservePct: 8, SampleInterval: tickInterval,
	})
	return e, gw, clk
}

// Presence while the reference market is shut carries the full fill risk and
// earns a tenth. Quoting through a weekend is a loss the bot took silently
// until the multiplier reached the model.
func TestQuotesAreWithdrawnWhileTheReferenceMarketIsClosed(t *testing.T) {
	ctx := context.Background()
	e, gw, clk := offHoursEngine(t)

	liveRung(e, market.Buy, "bid-1", "4273", clk.now)
	liveRung(e, market.Sell, "ask-1", "4279", clk.now)

	clk.now = time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC) // Saturday
	e.evaluate(ctx, true)

	require.ElementsMatch(t,
		[]trading.ClOrdID{"bid-1", "ask-1"}, gw.cancelled,
		"both sides come off while the hour accrues at ten percent")
	require.Empty(t, gw.placed)
}

// The same actor must quote normally once the reference market reopens.
func TestQuotingResumesWhenTheReferenceMarketOpens(t *testing.T) {
	ctx := context.Background()
	e, gw, clk := offHoursEngine(t)

	clk.now = time.Date(2026, 8, 10, 9, 0, 0, 0, time.UTC) // Monday morning
	primeVolatility(t, e, clk)
	e.available = decimal.NewFromInt(1000)
	e.evaluate(ctx, true)

	require.NotEmpty(t, gw.placed, "an open market is quoted as usual")
}

// An operator reading the log has to see why the actor is idle. Without this
// the weekend is indistinguishable from a crash: the same total silence.
func TestClosedMarketIsReportedInTheStatusLog(t *testing.T) {
	ctx := context.Background()
	e, _, clk := offHoursEngine(t)

	var buf bytes.Buffer
	e.deps.Log = slog.New(slog.NewTextHandler(&buf, nil))

	clk.now = time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC) // Saturday
	e.evaluate(ctx, true)

	require.Contains(t, buf.String(), "engine status")
	require.Contains(t, buf.String(), "the reference market is closed")
}

// wideBook replaces the book with one whose half spread is the given width in
// basis points, which is what actually pushes the requirement over the band.
func wideBook(e *Engine, halfSpreadBps float64, at time.Time) {
	mark := 4276.0
	off := mark * halfSpreadBps / 10_000
	e.book = market.NewBook("XAU-USD",
		[]market.Level{{
			Price: money.MustParsePrice(fmt.Sprintf("%.4f", mark-off)),
			Qty:   money.MustParseQty("1"),
		}},
		[]market.Level{{
			Price: money.MustParsePrice(fmt.Sprintf("%.4f", mark+off)),
			Qty:   money.MustParseQty("1"),
		}},
		at)
}

// The feasibility threshold was the one decision in this loop without a dead
// band, and the market sat on it: on 6 August the half spread swung between
// 0.12 and 3.40 bps within an hour, and every excursion past the band pulled the
// whole ladder and every return placed it again — 564 withdrawals in one hour at
// 99% uptime.
func TestQuotingDoesNotResumeInsideTheReentryDeadBand(t *testing.T) {
	ctx := context.Background()
	e, gw, clk := dangerEngine(t)
	e.available = decimal.NewFromInt(1000)

	// Wide enough that the requirement no longer fits the band: with a jump of
	// 0.23 bps the fixed part is 1.79, so nine of half spread puts it at 10.8.
	wideBook(e, 9.0, clk.now)
	e.evaluate(ctx, true)
	require.False(t, e.target.Feasible, "the actor steps aside on the excursion")

	// The spread comes back to just inside the band. Without hysteresis this is
	// where the ladder went back up, only to come off on the next tick.
	before := len(gw.placed)
	wideBook(e, 7.0, clk.now)
	clk.advance(time.Second)
	e.evaluate(ctx, true)
	require.Equal(t, before, len(gw.placed),
		"barely inside is not far enough to re-enter")

	// Clear of the band by the dead band: now it is worth going back in.
	wideBook(e, 5.0, clk.now)
	clk.advance(time.Second)
	e.evaluate(ctx, true)
	require.Greater(t, len(gw.placed), before, "clear of the edge, quoting resumes")
}

// Leaving keeps no dead band at all: the same asymmetry as everywhere else in
// this loop, caution immediately and greed on delay.
func TestLeavingTheBandIsImmediate(t *testing.T) {
	ctx := context.Background()
	e, gw, clk := dangerEngine(t)
	e.available = decimal.NewFromInt(1000)

	wideBook(e, 5.0, clk.now)
	e.evaluate(ctx, true)
	require.True(t, e.target.Feasible)
	liveRung(e, market.Buy, "bid-1", "4273", clk.now)
	liveRung(e, market.Sell, "ask-1", "4279", clk.now)

	wideBook(e, 9.0, clk.now)
	clk.advance(time.Second)
	e.evaluate(ctx, true)

	require.False(t, e.target.Feasible)
	require.ElementsMatch(t, []trading.ClOrdID{"bid-1", "ask-1"}, gw.cancelled,
		"no dead band on the way out")
}
