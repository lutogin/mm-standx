package engine

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nd/mm-standx/internal/config"
	"github.com/nd/mm-standx/internal/domain/market"
	"github.com/nd/mm-standx/internal/domain/money"
	"github.com/nd/mm-standx/internal/domain/policy"
	"github.com/nd/mm-standx/internal/domain/trading"
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
			Health: config.Health{AckTimeout: 2 * time.Second},
			Config: config.Symbol{
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

	// A mature estimate is a precondition for quoting at all: the actor refuses
	// to place orders while the risk measure rests on too few observations.
	for i := range 50 {
		price := money.MustParsePrice("4276")
		if i%2 == 1 {
			price = money.MustParsePrice("4276.1")
		}
		e.vol.Observe(price, clk.now)
		clk.advance(1400 * time.Millisecond)
	}
	require.True(t, e.vol.Estimate(clk.now).Ready)

	e.mark = money.MustParsePrice("4276")
	// A two dollar spread puts the pull threshold near 2.3 bps, so a rung one
	// dollar away from the mark is unambiguously in danger.
	e.book = market.NewBook("XAU-USD",
		[]market.Level{{Price: money.MustParsePrice("4275"), Qty: money.MustParseQty("1")}},
		[]market.Level{{Price: money.MustParsePrice("4277"), Qty: money.MustParseQty("1")}},
		clk.now)
	return e, gw, clk
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
