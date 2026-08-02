//go:build live

package live

import (
	"context"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/stretchr/testify/require"

	"github.com/nd/mm-standx/internal/domain/market"
	"github.com/nd/mm-standx/internal/domain/money"
	"github.com/nd/mm-standx/internal/domain/trading"
	"github.com/nd/mm-standx/internal/ports"
)

func newClOrdID() trading.ClOrdID { return trading.ClOrdID(ulid.Make().String()) }

func isOpen(o trading.Order) bool     { return o.Status == trading.StatusOpen }
func isCanceled(o trading.Order) bool { return o.Status == trading.StatusCanceled }

// TestSmoke walks the order path on the real exchange in the order the strategy
// depends on it. Subtests share state deliberately: each one builds on the book
// state the previous one left behind.
func TestSmoke(t *testing.T) {
	h := newHarness(t)
	t.Logf("symbol %s, price tick %d decimals, min qty %s",
		h.symbol, h.spec.PriceTickDecimals, h.spec.MinOrderQty)

	mark := h.markPrice()
	qty := h.testQty(mark)
	t.Logf("mark %s, test size %s (~%s USD)", mark, qty, money.Notional(mark, qty))

	var resting trading.ClOrdID

	// The whole strategy rests on post-only orders sitting quietly inside the
	// reward band. If an order placed at a safe distance does not reach the
	// book, nothing else matters.
	t.Run("post-only order rests on the book", func(t *testing.T) {
		resting = h.place(market.Buy, market.PriceAtDistance(mark, market.Buy, 9), qty, trading.ALO, false)

		order, ok := h.awaitOrder(resting, isOpen)
		require.True(t, ok, "order must reach the book within %s", eventTimeout)
		require.Equal(t, h.symbol, order.Symbol)
		require.True(t, order.FilledQty.IsZero(), "a quote 9 bps away must not fill")

		found := false
		for _, o := range h.openOrders() {
			if o.ClOrdID == resting {
				found = true
			}
		}
		require.True(t, found, "the exchange must report the order as open")
	})

	// This is the single most important guarantee in the design: ALO makes it
	// impossible to become a taker by mistake. Without it, an arithmetic slip in
	// the distance calculation would cross the spread and pay the taker fee.
	t.Run("crossing post-only order is rejected", func(t *testing.T) {
		book := h.book()
		ask, ok := book.BestAsk()
		require.True(t, ok, "the book must have an ask side")

		// Deliberately price the buy above the best ask.
		crossing := market.PriceAtDistance(ask, market.Sell, 20)
		id := h.place(market.Buy, crossing, qty, trading.ALO, false)

		require.True(t, h.awaitRejection(id),
			"a crossing ALO order must be rejected, not filled")

		for _, o := range h.openOrders() {
			require.NotEqual(t, id, o.ClOrdID, "a rejected order must not rest on the book")
		}
	})

	// Requoting is the operation the bot performs thousands of times a day, and
	// the ordering of place and cancel decides whether the side ever goes empty.
	// These two measurements size the ladder: see PLAN.md sections 4.2 and 4.3.
	t.Run("requote timing", func(t *testing.T) {
		t.Run("place-first keeps the side populated", func(t *testing.T) {
			replacement := h.place(market.Buy, market.PriceAtDistance(mark, market.Buy, 7), qty, trading.ALO, false)

			_, ok := h.awaitOrder(replacement, isOpen)
			require.True(t, ok, "replacement must reach the book")
			openedAt := time.Now()

			require.NoError(t, h.gateway.Cancel(context.Background(), resting, ports.PriorityQuote))
			_, ok = h.awaitOrder(resting, isCanceled)
			require.True(t, ok, "the superseded order must be cancelled")
			cancelledAt := time.Now()

			t.Logf("place-first overlap: %s with both orders resting", cancelledAt.Sub(openedAt).Round(time.Millisecond))
			resting = replacement
		})

		t.Run("cancel-first leaves a measurable gap", func(t *testing.T) {
			require.NoError(t, h.gateway.Cancel(context.Background(), resting, ports.PrioritySafety))
			_, ok := h.awaitOrder(resting, isCanceled)
			require.True(t, ok, "the order must be cancelled")
			emptyAt := time.Now()

			replacement := h.place(market.Buy, market.PriceAtDistance(mark, market.Buy, 8), qty, trading.ALO, false)
			_, ok = h.awaitOrder(replacement, isOpen)
			require.True(t, ok, "replacement must reach the book")

			// This gap is exactly what the ladder exists to hide. If it turns out
			// to be small relative to the exchange sampling interval, fewer rungs
			// are needed.
			t.Logf("cancel-first gap: %s with the side empty", time.Since(emptyAt).Round(time.Millisecond))
			resting = replacement
		})
	})

	// Emergency withdrawal of a whole side must work in one request.
	t.Run("batch cancel clears the side", func(t *testing.T) {
		second := h.place(market.Buy, market.PriceAtDistance(mark, market.Buy, 9), qty, trading.ALO, false)
		_, ok := h.awaitOrder(second, isOpen)
		require.True(t, ok, "second order must reach the book")

		require.NoError(t, h.gateway.CancelBatch(context.Background(),
			[]trading.ClOrdID{resting, second}, ports.PrioritySafety))

		for _, id := range []trading.ClOrdID{resting, second} {
			_, ok := h.awaitOrder(id, isCanceled)
			require.True(t, ok, "order %s must be cancelled", id)
		}
		require.Empty(t, h.openOrders(), "no orders may remain after a batch cancel")
	})

	// The second core hypothesis: when a fill does happen, the position can be
	// closed immediately. This is the only scenario that intentionally pays the
	// taker fee, and it does so twice on a few dollars of notional.
	t.Run("forced fill is flattened", func(t *testing.T) {
		require.True(t, h.position().IsZero(), "test must start flat")

		book := h.book()
		ask, ok := book.BestAsk()
		require.True(t, ok, "the book must have an ask side")

		// GTC rather than ALO: here we want to be the taker.
		crossing := market.PriceAtDistance(ask, market.Sell, 20)
		id := h.place(market.Buy, crossing, qty, trading.GTC, false)

		filled, ok := h.awaitOrder(id, func(o trading.Order) bool { return o.FilledQty.IsPositive() })
		require.True(t, ok, "a crossing GTC order must fill")
		t.Logf("filled %s at %s", filled.FilledQty, filled.AvgFillPrice)

		position := h.awaitPosition(func(q money.Qty) bool { return !q.IsZero() })
		require.False(t, position.IsZero(), "the fill must open a position")
		t.Logf("position after fill: %s", position)

		start := time.Now()
		require.NoError(t, h.flatten(context.Background(), position), "flatten must be accepted")

		flat := h.awaitPosition(func(q money.Qty) bool { return q.IsZero() })
		require.True(t, flat.IsZero(), "position must return to zero, got %s", flat)
		t.Logf("flattened in %s", time.Since(start).Round(time.Millisecond))
	})

	// The exchange reports no side field on a position, so the code assumes a
	// short is expressed as a negative quantity. That assumption decides which
	// way the closing order goes: if it is wrong, a "flatten" would double the
	// position instead of closing it. Only a real short can settle it.
	t.Run("short position carries a negative sign", func(t *testing.T) {
		require.True(t, h.position().IsZero(), "test must start flat")

		book := h.book()
		bid, ok := book.BestBid()
		require.True(t, ok, "the book must have a bid side")

		crossing := market.PriceAtDistance(bid, market.Buy, 20)
		id := h.place(market.Sell, crossing, qty, trading.GTC, false)

		_, ok = h.awaitOrder(id, func(o trading.Order) bool { return o.FilledQty.IsPositive() })
		require.True(t, ok, "a crossing sell must fill")

		position := h.awaitPosition(func(q money.Qty) bool { return !q.IsZero() })
		t.Logf("position after selling short: %s", position)
		require.True(t, position.IsNegative(),
			"a short position must be reported as a negative quantity, got %s", position)

		// Close immediately rather than waiting to see whether it closes itself:
		// an unhedged position left open for the duration of a poll loop is
		// exactly the exposure this whole project exists to avoid.
		require.NoError(t, h.flatten(context.Background(), position), "flatten must be accepted")

		flat := h.awaitPosition(func(q money.Qty) bool { return q.IsZero() })
		require.True(t, flat.IsZero(), "short position must close, got %s", flat)
	})
}

// awaitPosition polls the position until it satisfies the predicate.
//
// Polling rather than event-driven waiting is deliberate: the position channel
// and the REST snapshot can disagree briefly, and the snapshot is the one the
// reconciliation loop trusts.
func (h *harness) awaitPosition(want func(money.Qty) bool) money.Qty {
	h.t.Helper()

	deadline := time.Now().Add(eventTimeout)
	var last money.Qty
	for time.Now().Before(deadline) {
		last = h.position()
		if want(last) {
			return last
		}
		time.Sleep(250 * time.Millisecond)
	}
	return last
}
