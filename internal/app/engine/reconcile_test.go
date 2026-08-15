package engine

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nd/mm-standx/internal/domain/market"
	"github.com/nd/mm-standx/internal/domain/trading"
	"github.com/nd/mm-standx/internal/events"
)

// Сверка обязана переживать собственную задержку.
//
// Снимок берётся у биржи, а разбирается в акторе спустя сеть и очередь событий.
// За это время ордер успевает стать терминальным, и наивная сверка объявляла
// его сиротой: 14 августа это дало 209 эпизодов отмены и 366 ответов
// «order is not open», причём отмены шли с приоритетом безопасности, то есть
// из резерва, отложенного под аварийный съём.

func snapshotOf(sym market.Symbol, at time.Time, ids ...trading.ClOrdID) events.OpenOrdersSnapshot {
	orders := make([]trading.Order, 0, len(ids))
	for _, id := range ids {
		orders = append(orders, trading.Order{ClOrdID: id, Symbol: sym, Status: trading.StatusOpen})
	}
	return events.OpenOrdersSnapshot{Symbol: sym, Orders: orders, At: at}
}

// Ордер, ушедший из лесенки после снятия снимка, не сирота: следующий снимок
// его уже не покажет.
func TestAnOrderTerminalSinceTheSnapshotIsNotCancelled(t *testing.T) {
	ctx := context.Background()
	e, gw, clk := dangerEngine(t)

	// Снимок ещё помнит ордер, лесенка — уже нет.
	e.reconcile(ctx, snapshotOf(e.symbol, clk.now, "gone-1"))
	require.Empty(t, gw.cancelled, "one sighting is not enough to call an order ownerless")

	// Следующий снимок его не содержит — он и правда исполнился.
	clk.advance(5 * time.Second)
	e.reconcile(ctx, snapshotOf(e.symbol, clk.now))
	require.Empty(t, gw.cancelled, "and it must never be cancelled")
}

// Настоящая сирота от прошлого запуска никуда не девается и снимается.
func TestAPersistentOrphanIsCancelledOnTheSecondSnapshot(t *testing.T) {
	ctx := context.Background()
	e, gw, clk := dangerEngine(t)

	e.reconcile(ctx, snapshotOf(e.symbol, clk.now, "leftover-1"))
	require.Empty(t, gw.cancelled)

	clk.advance(5 * time.Second)
	e.reconcile(ctx, snapshotOf(e.symbol, clk.now, "leftover-1"))
	require.Equal(t, []trading.ClOrdID{"leftover-1"}, gw.cancelled,
		"an order still unowned a full reconcile later is genuinely ours to cancel")
}

// Наши собственные живые ступени сиротами не считаются никогда.
func TestOwnLiveRungsAreNeverOrphans(t *testing.T) {
	ctx := context.Background()
	e, gw, clk := dangerEngine(t)

	liveRung(e, market.Buy, "bid-1", "4273", clk.now)
	for range 3 {
		clk.advance(5 * time.Second)
		e.reconcile(ctx, snapshotOf(e.symbol, clk.now, "bid-1"))
	}
	require.Empty(t, gw.cancelled)
}
