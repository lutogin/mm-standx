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
	"github.com/nd/mm-standx/internal/domain/trading"
	"github.com/nd/mm-standx/internal/events"
	"github.com/nd/mm-standx/internal/ports"
)

// --- test doubles ---

type stubTicker struct{ c chan time.Time }

func (t stubTicker) C() <-chan time.Time { return t.c }
func (t stubTicker) Stop()               {}

// testClock moves only when the test says so: every deadline in the actor is
// expressed in wall time, and a real clock would make the assertions racy.
type testClock struct{ now time.Time }

func (c *testClock) Now() time.Time                             { return c.now }
func (c *testClock) After(time.Duration) <-chan time.Time       { return make(chan time.Time) }
func (c *testClock) NewTicker(time.Duration) ports.Ticker       { return stubTicker{c: make(chan time.Time)} }
func (c *testClock) Sleep(context.Context, time.Duration) error { return nil }
func (c *testClock) advance(d time.Duration)                    { c.now = c.now.Add(d) }

type recordingGateway struct {
	placed    []ports.PlaceRequest
	cancelled []trading.ClOrdID
}

func (g *recordingGateway) Place(_ context.Context, req ports.PlaceRequest) error {
	g.placed = append(g.placed, req)
	return nil
}

func (g *recordingGateway) Cancel(_ context.Context, id trading.ClOrdID, _ ports.Priority) error {
	g.cancelled = append(g.cancelled, id)
	return nil
}

func (g *recordingGateway) CancelBatch(_ context.Context, ids []trading.ClOrdID, _ ports.Priority) error {
	g.cancelled = append(g.cancelled, ids...)
	return nil
}

// marketOrders returns only the closing orders: quotes are limit orders.
func (g *recordingGateway) marketOrders() []ports.PlaceRequest {
	var out []ports.PlaceRequest
	for _, r := range g.placed {
		if r.Type == trading.Market {
			out = append(out, r)
		}
	}
	return out
}

// fillEngine builds an actor that already holds a mark price and is otherwise
// idle, plus a counter of the fill episodes it reports to the risk ledger.
func fillEngine(t *testing.T) (*Engine, *recordingGateway, *testClock, *int) {
	t.Helper()

	clk := &testClock{now: time.Date(2026, 8, 6, 14, 34, 0, 0, time.UTC)}
	gw := &recordingGateway{}
	bus := events.New(nil)

	episodes := 0
	events.Subscribe(bus, "test", func(_ context.Context, ev events.QuoteFilled) {
		if ev.StartsEpisode {
			episodes++
		}
	})

	e := New(
		Params{
			Symbol: "XAU-USD",
			Live:   true,
			Health: config.Health{AckTimeout: 2 * time.Second},
			Risk: config.Risk{
				FlattenDebounce:  150 * time.Millisecond,
				PostFillCooldown: 3 * time.Second,
				Fees:             config.Fees{MakerBps: 1, TakerBps: 4},
			},
			MaxNetPosition: money.MustParseQty("0"),
		},
		Deps{
			Gateway: gw,
			Bus:     bus,
			Clock:   clk,
			Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		},
	)
	e.mark = money.MustParsePrice("4276")
	return e, gw, clk, &episodes
}

func fill(id trading.ClOrdID, side market.Side, qty, price string) trading.Order {
	return trading.Order{
		ClOrdID:      id,
		Symbol:       "XAU-USD",
		Side:         side,
		Status:       trading.StatusFilled,
		Qty:          money.MustParseQty(qty),
		FilledQty:    money.MustParseQty(qty),
		AvgFillPrice: money.MustParsePrice(price),
	}
}

// The closing order fills like any other, and its report comes back through the
// same channel. Counted as a fill, it opens an episode that nothing can close —
// the position is already flat by then, so the actor stops quoting for good.
// This is the regression that wedged the live run of 2026-08-06.
func TestClosingOrderFillDoesNotOpenAnEpisode(t *testing.T) {
	ctx := context.Background()
	e, gw, clk, episodes := fillEngine(t)

	// A quote is hit and the exchange reports the resulting short.
	e.onFill(ctx, fill("quote-1", market.Sell, "0.008", "4276.1"))
	e.position = money.MustParseQty("-0.008")
	require.Equal(t, 1, *episodes)

	// The debounce expires and the actor closes at market.
	clk.advance(200 * time.Millisecond)
	e.onTick(ctx)

	closing := gw.marketOrders()
	require.Len(t, closing, 1, "the position must be closed exactly once")
	require.True(t, closing[0].ReduceOnly)
	require.True(t, e.pendingFill.IsZero())

	// The exchange reports the close: the position goes flat and the closing
	// order comes back filled.
	e.position = money.MustParseQty("0")
	e.onOrder(ctx, events.OrderUpdated{
		Order: fill(closing[0].ClOrdID, market.Buy, "0.008", "4276.8"),
	})

	require.Equal(t, 1, *episodes, "the close is part of the episode, not a new one")
	require.True(t, e.pendingFill.IsZero(), "the closing fill must not re-arm the episode")

	// And the actor is free to quote again once the cooldown expires.
	clk.advance(e.risk.PostFillCooldown + time.Second)
	e.onTick(ctx)
	require.Len(t, gw.marketOrders(), 1, "no position is open, nothing to close")
	require.False(t, clk.now.Before(e.cooldownUntil) && e.pendingFill.IsPositive())
	require.True(t, e.pendingFill.IsZero())
}

// A fill that never produces a position — the opposite side netted it out, or
// the exchange simply never reported one — must not hold the actor hostage.
func TestFillWithoutAPositionClearsTheEpisode(t *testing.T) {
	ctx := context.Background()
	e, gw, clk, _ := fillEngine(t)

	e.onFill(ctx, fill("quote-1", market.Sell, "0.008", "4276.1"))
	require.True(t, e.pendingFill.IsPositive())

	// Within the grace the actor waits: the position update may still be in flight.
	clk.advance(200 * time.Millisecond)
	e.onTick(ctx)
	require.True(t, e.pendingFill.IsPositive(), "too early to give up on the position")

	clk.advance(flattenGrace)
	e.onTick(ctx)
	require.True(t, e.pendingFill.IsZero(), "the episode must not outlive the grace")
	require.Empty(t, gw.marketOrders(), "there was no position to close")
}

// The exchange reports accumulated volume, and the close is one order among
// many messages. Repeating it must not send a second market order.
func TestRepeatedClosingReportsSendOneOrder(t *testing.T) {
	ctx := context.Background()
	e, gw, clk, _ := fillEngine(t)

	e.onFill(ctx, fill("quote-1", market.Sell, "0.008", "4276.1"))
	e.position = money.MustParseQty("-0.008")

	clk.advance(200 * time.Millisecond)
	e.onTick(ctx)
	closing := gw.marketOrders()
	require.Len(t, closing, 1)

	// The position update lags: several ticks pass with the short still on file.
	for range 8 {
		clk.advance(tickInterval)
		e.onTick(ctx)
	}
	require.Len(t, gw.marketOrders(), 2,
		"one retry after the ack timeout, not one per tick")

	for _, o := range gw.marketOrders() {
		e.onOrder(ctx, events.OrderUpdated{Order: fill(o.ClOrdID, market.Buy, "0.008", "4276.8")})
	}
	require.True(t, e.pendingFill.IsZero())
}
