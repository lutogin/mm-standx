// Package watchdog следит за свежестью справочной цены.
//
// Вся стратегия строится вокруг mark price: дистанции, веса, решения о переносе.
// Торговать по устаревшей цене — значит держать ордера там, где рынка уже нет,
// поэтому пропажа обновлений обязана приводить к съёму котировок.
package watchdog

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/nd/mm-standx/internal/domain/market"
	"github.com/nd/mm-standx/internal/events"
	"github.com/nd/mm-standx/internal/ports"
)

// checkDivisor определяет частоту проверок относительно порога устаревания:
// проверяем вчетверо чаще, чтобы задержка обнаружения не превышала четверти порога.
const checkDivisor = 4

// Watchdog публикует MarkPriceStale, когда обновления справочной цены прекращаются.
type Watchdog struct {
	threshold time.Duration
	bus       *events.Bus
	clock     ports.Clock
	log       *slog.Logger

	mu       sync.Mutex
	lastSeen map[market.Symbol]time.Time
	stale    map[market.Symbol]bool

	unsubscribe func()
	cancel      context.CancelFunc
	done        chan struct{}
}

// New создаёт сторож с указанным порогом устаревания.
func New(threshold time.Duration, bus *events.Bus, clk ports.Clock, log *slog.Logger) *Watchdog {
	return &Watchdog{
		threshold: threshold,
		bus:       bus,
		clock:     clk,
		log:       log.With("component", "watchdog"),
		lastSeen:  make(map[market.Symbol]time.Time),
		stale:     make(map[market.Symbol]bool),
		done:      make(chan struct{}),
	}
}

func (w *Watchdog) Name() string { return "watchdog" }

func (w *Watchdog) Start(context.Context) error {
	w.unsubscribe = events.Subscribe(w.bus, w.Name(), w.onMarkPrice)

	ctx, cancel := context.WithCancel(context.Background())
	w.cancel = cancel

	go func() {
		defer close(w.done)
		w.loop(ctx)
	}()
	return nil
}

func (w *Watchdog) Stop(ctx context.Context) error {
	if w.unsubscribe != nil {
		w.unsubscribe()
	}
	if w.cancel == nil {
		return nil
	}
	w.cancel()

	select {
	case <-w.done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("watchdog: did not stop in time: %w", ctx.Err())
	}
}

// onMarkPrice запоминает момент последнего обновления.
//
// Обработчик вызывается синхронно из читателя WebSocket, поэтому делает
// минимум работы: снимает блокировку, пишет отметку времени и возвращается.
func (w *Watchdog) onMarkPrice(ctx context.Context, e events.MarkPriceUpdated) {
	w.mu.Lock()
	symbol := e.Mark.Symbol
	w.lastSeen[symbol] = e.Mark.Received
	recovered := w.stale[symbol]
	w.stale[symbol] = false
	w.mu.Unlock()

	if recovered {
		w.log.Info("mark price is updating again", "symbol", symbol)
	}
	_ = ctx
}

func (w *Watchdog) loop(ctx context.Context) {
	ticker := w.clock.NewTicker(w.threshold / checkDivisor)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C():
			w.check(ctx)
		}
	}
}

// check находит инструменты с протухшей ценой и публикует событие ровно один раз
// на эпизод: повторять его каждый тик значило бы засорять шину.
func (w *Watchdog) check(ctx context.Context) {
	now := w.clock.Now()

	type staleSymbol struct {
		symbol market.Symbol
		since  time.Time
		age    time.Duration
	}
	var found []staleSymbol

	w.mu.Lock()
	for symbol, seen := range w.lastSeen {
		age := now.Sub(seen)
		if age > w.threshold && !w.stale[symbol] {
			w.stale[symbol] = true
			found = append(found, staleSymbol{symbol: symbol, since: seen, age: age})
		}
	}
	w.mu.Unlock()

	// Публикуем вне блокировки: обработчики могут обратиться к сторожу обратно.
	for _, s := range found {
		w.log.Error("mark price is stale", "symbol", s.symbol, "age", s.age)
		events.Publish(w.bus, ctx, events.MarkPriceStale{
			Symbol: s.symbol,
			Since:  s.since,
			Age:    s.age,
		})
	}
}

var _ ports.Runnable = (*Watchdog)(nil)
