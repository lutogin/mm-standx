package engine

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nd/mm-standx/internal/domain/market"
	"github.com/nd/mm-standx/internal/domain/money"
	"github.com/nd/mm-standx/internal/domain/quotes"
	"github.com/nd/mm-standx/internal/domain/trading"
	"github.com/nd/mm-standx/internal/ports"
)

// countingMetrics records only what the shadow accounting emits.
type countingMetrics struct {
	ports.NopMetrics
	shadow map[market.Side]int
}

func (m *countingMetrics) ShadowFill(_ market.Symbol, side market.Side) {
	m.shadow[side]++
}

const (
	// A quiet book around a mark of 100: the spread is one basis point wide.
	bestBid = "99.995"
	bestAsk = "100.005"
)

func book(bid, ask string) market.Book {
	return market.NewBook("BTC-USD",
		[]market.Level{{Price: money.MustParsePrice(bid), Qty: money.MustParseQty("1")}},
		[]market.Level{{Price: money.MustParsePrice(ask), Qty: money.MustParseQty("1")}},
		time.Time{})
}

// shadowEngine builds an actor with a single live rung per side, placed at the
// given prices, in dry run.
func shadowEngine(t *testing.T, bidPrice, askPrice string) (*Engine, *countingMetrics) {
	t.Helper()

	m := &countingMetrics{shadow: make(map[market.Side]int)}
	e := New(
		Params{Symbol: "BTC-USD", Live: false},
		Deps{Metrics: m, Log: slog.New(slog.NewTextHandler(io.Discard, nil))},
	)
	e.mark = money.MustParsePrice("100")

	for _, rung := range []struct {
		ladder *quotes.Ladder
		price  string
	}{{e.bids, bidPrice}, {e.asks, askPrice}} {
		id := trading.ClOrdID("rung-" + rung.price)
		rung.ladder.OnPlaceSent(0, id, money.MustParsePrice(rung.price), money.MustParseQty("1"), time.Time{})
		rung.ladder.OnOrderUpdate(trading.Order{ClOrdID: id, Status: trading.StatusOpen}, time.Time{})
	}
	return e, m
}

func TestUntouchedQuotesAreNotCounted(t *testing.T) {
	e, m := shadowEngine(t, "99.94", "100.06")
	e.book = book(bestBid, bestAsk)

	e.countShadowFills(time.Time{})
	require.Zero(t, e.shadowFills)
	require.Empty(t, m.shadow)
}

// The market reaching a resting bid means the opposite side of the book crossed
// its price: someone sold down through the level we were sitting on.
func TestQuoteIsCountedWhenTheMarketReachesIt(t *testing.T) {
	e, m := shadowEngine(t, "99.94", "100.06")
	e.book = book("99.93", "99.94")

	e.countShadowFills(time.Time{})
	require.Equal(t, 1, e.shadowFills)
	require.Equal(t, 1, m.shadow[market.Buy])
	require.Zero(t, m.shadow[market.Sell], "the ask side was never reached")
}

// The book arrives several times per tick. Without per-order memory a single
// touch would be reported as dozens of fills and the estimate would be useless.
func TestOneTouchIsCountedOnce(t *testing.T) {
	e, m := shadowEngine(t, "99.94", "100.06")

	for range 50 {
		e.book = book("99.93", "99.94")
		e.countShadowFills(time.Time{})
	}
	require.Equal(t, 1, e.shadowFills)
	require.Equal(t, 1, m.shadow[market.Buy])
}

// A re-placed rung is a new order and may be reached again.
func TestRequotedRungCanBeCountedAgain(t *testing.T) {
	e, m := shadowEngine(t, "99.94", "100.06")

	e.book = book("99.93", "99.94")
	e.countShadowFills(time.Time{})

	e.forget("rung-99.94")
	next := trading.ClOrdID("rung-again")
	e.bids.OnPlaceSent(0, next, money.MustParsePrice("99.94"), money.MustParseQty("1"), time.Time{})
	e.bids.OnOrderUpdate(trading.Order{ClOrdID: next, Status: trading.StatusOpen}, time.Time{})

	e.countShadowFills(time.Time{})
	require.Equal(t, 2, e.shadowFills)
	require.Equal(t, 2, m.shadow[market.Buy])
}

// In live mode the exchange reports fills and its word is authoritative;
// guesses would only contaminate the statistics by mixing with facts.
func TestLiveModeDoesNotGuess(t *testing.T) {
	e, m := shadowEngine(t, "99.94", "100.06")
	e.live = true
	e.book = book("99.93", "99.94")

	e.countShadowFills(time.Time{})
	require.Zero(t, e.shadowFills)
	require.Empty(t, m.shadow)
}
