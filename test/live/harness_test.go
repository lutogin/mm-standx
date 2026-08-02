//go:build live

// Package live contains the smoke test that exercises the real exchange with
// real orders. It is excluded from ordinary builds by the `live` build tag and
// additionally refuses to run without STANDX_LIVE=1, because every scenario
// here spends real money.
package live

import (
	"context"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
	"go.uber.org/fx"

	"github.com/nd/mm-standx/internal/adapters/standx/rest"
	"github.com/nd/mm-standx/internal/config"
	"github.com/nd/mm-standx/internal/di"
	"github.com/nd/mm-standx/internal/domain/market"
	"github.com/nd/mm-standx/internal/domain/money"
	"github.com/nd/mm-standx/internal/domain/trading"
	"github.com/nd/mm-standx/internal/events"
	"github.com/nd/mm-standx/internal/ports"
)

const (
	// targetNotionalUSD keeps every test order small enough that a worst-case
	// fill costs a fraction of a cent, while staying above any undocumented
	// minimum notional the exchange might enforce.
	targetNotionalUSD = 5

	// maxNotionalUSD is a hard ceiling checked before every order. It exists to
	// turn a units/price mix-up into a failed assertion instead of a real trade.
	maxNotionalUSD = 25

	eventTimeout = 15 * time.Second
)

// harness wires the real dependency graph and records order lifecycle events.
type harness struct {
	t       *testing.T
	cfg     *config.Config
	gateway ports.OrderGateway
	client  *rest.Client
	bus     *events.Bus
	log     *slog.Logger
	app     *fx.App

	symbol market.Symbol
	spec   market.SymbolSpec

	mu        sync.Mutex
	updates   map[trading.ClOrdID][]trading.Order
	rejects   map[trading.ClOrdID]events.OrderRejected
	connected map[string]bool
	watchers  []chan struct{}
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	if os.Getenv("STANDX_LIVE") != "1" {
		t.Skip("refusing to trade without STANDX_LIVE=1")
	}

	cfg, err := config.Load(configPath())
	require.NoError(t, err, "configuration must load")

	// The smoke test exists to exercise the real order path, so the live gateway
	// is wired deliberately rather than by accident.
	cfg.Runtime.Mode = config.ModeLive
	cfg.Runtime.RecordingDir = ""
	// Cross margin avoids the per-symbol margin transfer that isolated mode
	// requires; the smoke test is about the order path, not margin plumbing.
	cfg.Risk.MarginMode = trading.Cross
	require.NoError(t, cfg.Validate(), "configuration must stay valid after overrides")

	h := &harness{
		t:         t,
		cfg:       cfg,
		updates:   make(map[trading.ClOrdID][]trading.Order),
		rejects:   make(map[trading.ClOrdID]events.OrderRejected),
		connected: make(map[string]bool),
		symbol:    symbolUnderTest(cfg),
	}

	h.app = fx.New(
		fx.Supply(cfg),
		fx.NopLogger,
		di.Core,
		di.StandX,
		di.Runtime,
		fx.Populate(&h.gateway, &h.client, &h.bus, &h.log),
	)
	require.NoError(t, h.app.Err(), "dependency graph must build")

	// Subscribing before start avoids losing the connection events the readiness
	// check below depends on.
	h.subscribe()

	startCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	require.NoError(t, h.app.Start(startCtx), "application must start")

	// Placing an order before the user stream is authenticated loses its
	// lifecycle events: the exchange reports order state only on that stream.
	h.awaitStreams("user", "order-api")

	spec, err := h.client.SymbolSpec(context.Background(), h.symbol)
	require.NoError(t, err, "symbol specification must load")
	h.spec = spec

	t.Cleanup(h.shutdown)
	return h
}

func configPath() string {
	if p := os.Getenv("STANDX_CONFIG"); p != "" {
		return p
	}
	return "../../config.yaml"
}

// symbolUnderTest picks the instrument to trade. The default is the cheapest
// enabled market, overridable for a targeted run.
func symbolUnderTest(cfg *config.Config) market.Symbol {
	if s := os.Getenv("STANDX_SMOKE_SYMBOL"); s != "" {
		return market.Symbol(s)
	}
	return "XAG-USD"
}

func (h *harness) subscribe() {
	events.Subscribe(h.bus, "smoke", func(_ context.Context, e events.OrderUpdated) {
		h.mu.Lock()
		h.updates[e.Order.ClOrdID] = append(h.updates[e.Order.ClOrdID], e.Order)
		h.notifyLocked()
		h.mu.Unlock()
	})
	events.Subscribe(h.bus, "smoke", func(_ context.Context, e events.OrderRejected) {
		h.mu.Lock()
		h.rejects[e.ClOrdID] = e
		h.notifyLocked()
		h.mu.Unlock()
	})
	events.Subscribe(h.bus, "smoke", func(_ context.Context, e events.StreamConnected) {
		h.mu.Lock()
		h.connected[e.Stream] = true
		h.notifyLocked()
		h.mu.Unlock()
	})
}

// awaitStreams blocks until every named stream reports a live connection.
func (h *harness) awaitStreams(names ...string) {
	h.t.Helper()

	notify, release := h.watch()
	defer release()

	deadline := time.After(eventTimeout)
	for {
		h.mu.Lock()
		missing := make([]string, 0, len(names))
		for _, name := range names {
			if !h.connected[name] {
				missing = append(missing, name)
			}
		}
		h.mu.Unlock()

		if len(missing) == 0 {
			return
		}
		select {
		case <-notify:
		case <-deadline:
			h.t.Fatalf("streams not connected within %s: %v", eventTimeout, missing)
		}
	}
}

// notifyLocked wakes every waiter. Called with the mutex held.
func (h *harness) notifyLocked() {
	for _, ch := range h.watchers {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

func (h *harness) watch() (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)

	h.mu.Lock()
	h.watchers = append(h.watchers, ch)
	index := len(h.watchers) - 1
	h.mu.Unlock()

	return ch, func() {
		h.mu.Lock()
		h.watchers[index] = make(chan struct{}, 1) // detach without reindexing
		h.mu.Unlock()
	}
}

// awaitOrder blocks until an order update satisfies the predicate.
func (h *harness) awaitOrder(id trading.ClOrdID, want func(trading.Order) bool) (trading.Order, bool) {
	h.t.Helper()

	notify, release := h.watch()
	defer release()

	deadline := time.After(eventTimeout)
	for {
		h.mu.Lock()
		history := append([]trading.Order(nil), h.updates[id]...)
		h.mu.Unlock()

		for _, o := range history {
			if want(o) {
				return o, true
			}
		}

		select {
		case <-notify:
		case <-deadline:
			return trading.Order{}, false
		}
	}
}

// awaitRejection blocks until the exchange reports the order as rejected.
//
// Rejection arrives through two independent paths: the trading channel response
// and the order stream. Either one satisfies the wait.
func (h *harness) awaitRejection(id trading.ClOrdID) bool {
	h.t.Helper()

	notify, release := h.watch()
	defer release()

	deadline := time.After(eventTimeout)
	for {
		h.mu.Lock()
		_, viaChannel := h.rejects[id]
		history := append([]trading.Order(nil), h.updates[id]...)
		h.mu.Unlock()

		if viaChannel {
			return true
		}
		for _, o := range history {
			if o.Status == trading.StatusRejected {
				return true
			}
		}

		select {
		case <-notify:
		case <-deadline:
			return false
		}
	}
}

// markPrice reads the current reference price.
func (h *harness) markPrice() money.Price {
	h.t.Helper()

	mark, err := h.client.SymbolPrice(context.Background(), h.symbol)
	require.NoError(h.t, err, "mark price must be readable")
	require.True(h.t, mark.Price.IsPositive(), "mark price must be positive")
	return mark.Price
}

func (h *harness) book() market.Book {
	h.t.Helper()

	book, err := h.client.DepthBook(context.Background(), h.symbol)
	require.NoError(h.t, err, "order book must be readable")
	return book
}

// testQty derives an order size near targetNotionalUSD, floored at the
// instrument minimum and snapped to the quantity tick.
func (h *harness) testQty(mark money.Price) money.Qty {
	h.t.Helper()

	qty, err := money.QtyForNotional(decimal.NewFromInt(targetNotionalUSD), mark)
	require.NoError(h.t, err)

	qty = h.spec.SnapQty(qty)
	if qty.Lt(h.spec.MinOrderQty) {
		qty = h.spec.MinOrderQty
	}
	require.NoError(h.t, h.spec.ValidateQty(qty), "test size must satisfy instrument limits")

	notional := money.Notional(mark, qty)
	require.True(h.t, notional.LessThanOrEqual(decimal.NewFromInt(maxNotionalUSD)),
		"test order notional %s exceeds the %d USD safety ceiling", notional, maxNotionalUSD)
	return qty
}

// place sends an order and returns its client identifier.
func (h *harness) place(side market.Side, price money.Price, qty money.Qty, tif trading.TimeInForce, reduceOnly bool) trading.ClOrdID {
	h.t.Helper()

	id := newClOrdID()
	req := ports.PlaceRequest{
		ClOrdID:     id,
		Symbol:      h.symbol,
		Side:        side,
		Type:        trading.Limit,
		TimeInForce: tif,
		Price:       h.spec.SnapPrice(price, side),
		Qty:         qty,
		ReduceOnly:  reduceOnly,
		MarginMode:  h.cfg.Risk.MarginMode,
		Leverage:    h.cfg.Defaults.SymbolRisk.Leverage,
		Priority:    ports.PriorityQuote,
	}

	h.t.Logf("place %s %s %s @ %s (%s)", tif, side, req.Qty, req.Price, id)
	require.NoError(h.t, h.gateway.Place(context.Background(), req), "placing an order must succeed")
	return id
}

func (h *harness) openOrders() []trading.Order {
	h.t.Helper()

	orders, err := h.client.OpenOrders(context.Background(), h.symbol)
	require.NoError(h.t, err, "open orders must be readable")
	return orders
}

func (h *harness) position() money.Qty {
	h.t.Helper()

	positions, err := h.client.Positions(context.Background(), h.symbol)
	require.NoError(h.t, err, "positions must be readable")

	total := money.MustParseQty("0")
	for _, p := range positions {
		if p.Symbol == h.symbol {
			total = total.Add(p.Qty)
		}
	}
	return total
}

// shutdown leaves the account exactly as it was found: no resting orders,
// no open position. Failures here are reported loudly because leftovers on a
// live account keep costing money after the test process exits.
func (h *harness) shutdown() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if orders := h.openOrders(); len(orders) > 0 {
		ids := make([]trading.ClOrdID, 0, len(orders))
		for _, o := range orders {
			ids = append(ids, o.ClOrdID)
		}
		h.t.Logf("cleanup: cancelling %d leftover orders", len(ids))
		if err := h.gateway.CancelBatch(ctx, ids, ports.PrioritySafety); err != nil {
			h.t.Errorf("cleanup: failed to cancel leftover orders: %v", err)
		}
	}

	if pos := h.position(); !pos.IsZero() {
		h.t.Logf("cleanup: flattening leftover position %s", pos)
		if err := h.flatten(ctx, pos); err != nil {
			h.t.Errorf("cleanup: failed to flatten position %s: %v", pos, err)
		}
	}

	stopCtx, stopCancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer stopCancel()
	if err := h.app.Stop(stopCtx); err != nil {
		h.t.Errorf("application did not stop cleanly: %v", err)
	}
}

// flatten closes an open position with a reduce-only market order.
func (h *harness) flatten(ctx context.Context, pos money.Qty) error {
	side := market.Sell
	if pos.IsNegative() {
		side = market.Buy
	}

	return h.gateway.Place(ctx, ports.PlaceRequest{
		ClOrdID:     newClOrdID(),
		Symbol:      h.symbol,
		Side:        side,
		Type:        trading.Market,
		TimeInForce: trading.IOC,
		Qty:         pos.Abs(),
		// reduce_only removes any chance of flipping the position if the
		// quantity is stale by the time the order reaches the matching engine.
		ReduceOnly: true,
		MarginMode: h.cfg.Risk.MarginMode,
		Leverage:   h.cfg.Defaults.SymbolRisk.Leverage,
		Priority:   ports.PrioritySafety,
	})
}
