package engine

import (
	"bytes"
	"context"
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
				JumpMultiple:   1.25,
				FloorMarginBps: 1.5,
				DeadBandJumps:  2.0,
				MinDeadBandBps: 1.2,
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

// The requote cooldown exists to protect the request budget, not to delay the
// emergency pull. While it guarded both, every action left the actor blind for
// a quarter of a second — longer than the interval between book updates.
func TestDangerPullIsNotDelayedByTheRequoteCooldown(t *testing.T) {
	ctx := context.Background()
	e, gw, clk := dangerEngine(t)

	liveRung(e, market.Sell, "ask-1", "4277", clk.now)

	// The actor acted just now: the cooldown is fully in force.
	e.lastAction = clk.now
	e.evaluate(ctx, false)

	require.Contains(t, gw.cancelled, trading.ClOrdID("ask-1"),
		"a rung the market has reached must be pulled regardless of the cooldown")
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
	e.hour = uptime.New(e.program, 80)
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
