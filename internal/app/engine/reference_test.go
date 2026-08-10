package engine

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nd/mm-standx/internal/domain/market"
	"github.com/nd/mm-standx/internal/domain/trading"
	"github.com/nd/mm-standx/internal/events"
)

// Разрыв канала биржи означает торговлю вслепую: котировки снимаются немедленно.
func TestDisconnectedExchangeStreamPullsTheBook(t *testing.T) {
	ctx := context.Background()
	e, gw, clk := dangerEngine(t)

	liveRung(e, market.Buy, "bid-1", "4273", clk.now)
	liveRung(e, market.Sell, "ask-1", "4279", clk.now)

	e.handle(ctx, events.StreamDisconnected{Stream: "market", Reason: errors.New("read: EOF"), At: clk.now})

	require.ElementsMatch(t, []trading.ClOrdID{"bid-1", "ask-1"}, gw.cancelled)
}

// Разрыв внешнего ориентира возвращает нас ровно к той картине, по которой
// актор работал до его появления: быстрый сигнал снова берётся из середины
// стакана. Снимать котировки в этом случае значило бы платить присутствием —
// единственным, за что здесь платят, — за сбой на чужой площадке.
func TestDisconnectedReferenceFeedKeepsTheBook(t *testing.T) {
	ctx := context.Background()
	e, gw, clk := dangerEngine(t)

	var buf bytes.Buffer
	e.deps.Log = slog.New(slog.NewTextHandler(&buf, nil))

	liveRung(e, market.Buy, "bid-1", "4273", clk.now)
	liveRung(e, market.Sell, "ask-1", "4279", clk.now)

	e.handle(ctx, events.StreamDisconnected{
		Stream:   "reference[XAU-USD]",
		Reason:   errors.New("read: EOF"),
		At:       clk.now,
		Optional: true,
	})

	require.Empty(t, gw.cancelled, "an advisory feed must not take the book down")
	require.Contains(t, buf.String(), "optional stream disconnected")
}
