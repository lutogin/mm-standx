// Package observer ведёт сводную статистику потока рыночных данных.
//
// На фазе наблюдения это основной способ убедиться, что подписки живы, а данные
// приходят с ожидаемой частотой. Позже сводка становится входом для калибровки:
// по ней видно, насколько инструмент подвижен и стоит ли на нём котировать.
package observer

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/nd/mm-standx/internal/domain/market"
	"github.com/nd/mm-standx/internal/domain/money"
	"github.com/nd/mm-standx/internal/events"
	"github.com/nd/mm-standx/internal/ports"
)

// symbolStats — накопленная статистика по инструменту за интервал отчёта.
type symbolStats struct {
	marks      int
	books      int
	lastMark   money.Price
	firstMark  money.Price
	maxMoveBps money.Bps
	spreadBps  money.Bps
	hasSpread  bool
}

// Observer периодически печатает сводку по каждому инструменту.
type Observer struct {
	interval time.Duration
	bus      *events.Bus
	clock    ports.Clock
	log      *slog.Logger

	mu    sync.Mutex
	stats map[market.Symbol]*symbolStats

	unsubscribe []func()
	cancel      context.CancelFunc
	done        chan struct{}
}

func New(interval time.Duration, bus *events.Bus, clk ports.Clock, log *slog.Logger) *Observer {
	return &Observer{
		interval: interval,
		bus:      bus,
		clock:    clk,
		log:      log.With("component", "observer"),
		stats:    make(map[market.Symbol]*symbolStats),
		done:     make(chan struct{}),
	}
}

func (o *Observer) Name() string { return "observer" }

func (o *Observer) Start(context.Context) error {
	o.unsubscribe = []func(){
		events.Subscribe(o.bus, o.Name(), o.onMarkPrice),
		events.Subscribe(o.bus, o.Name(), o.onBook),
	}

	ctx, cancel := context.WithCancel(context.Background())
	o.cancel = cancel

	go func() {
		defer close(o.done)
		o.loop(ctx)
	}()
	return nil
}

func (o *Observer) Stop(ctx context.Context) error {
	for _, unsub := range o.unsubscribe {
		unsub()
	}
	if o.cancel == nil {
		return nil
	}
	o.cancel()

	select {
	case <-o.done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("observer: не остановился вовремя: %w", ctx.Err())
	}
}

func (o *Observer) onMarkPrice(_ context.Context, e events.MarkPriceUpdated) {
	o.mu.Lock()
	defer o.mu.Unlock()

	s := o.statsFor(e.Mark.Symbol)
	if s.marks == 0 {
		s.firstMark = e.Mark.Price
	} else if move, err := money.RelativeBps(s.lastMark, e.Mark.Price); err == nil {
		// Максимальный шаг между соседними тиками — грубая оценка того,
		// насколько резко цена способна подойти к нашей котировке.
		if move.Abs() > s.maxMoveBps {
			s.maxMoveBps = move.Abs()
		}
	}
	s.lastMark = e.Mark.Price
	s.marks++
}

func (o *Observer) onBook(_ context.Context, e events.BookUpdated) {
	o.mu.Lock()
	defer o.mu.Unlock()

	s := o.statsFor(e.Book.Symbol)
	s.books++

	bid, hasBid := e.Book.BestBid()
	ask, hasAsk := e.Book.BestAsk()
	if !hasBid || !hasAsk {
		return
	}
	// Ширина спреда решает, помещается ли котировка внутрь полосы награды,
	// не оказавшись при этом на вершине стакана.
	if spread, err := money.RelativeBps(bid, ask); err == nil {
		s.spreadBps, s.hasSpread = spread, true
	}
}

// statsFor возвращает запись статистики. Вызывается под удерживаемой блокировкой.
func (o *Observer) statsFor(symbol market.Symbol) *symbolStats {
	s, ok := o.stats[symbol]
	if !ok {
		s = &symbolStats{}
		o.stats[symbol] = s
	}
	return s
}

func (o *Observer) loop(ctx context.Context) {
	ticker := o.clock.NewTicker(o.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C():
			o.report()
		}
	}
}

// report печатает сводку и обнуляет счётчики.
func (o *Observer) report() {
	o.mu.Lock()
	snapshot := o.stats
	o.stats = make(map[market.Symbol]*symbolStats, len(snapshot))
	o.mu.Unlock()

	if len(snapshot) == 0 {
		o.log.Warn("за интервал не получено ни одного обновления")
		return
	}

	seconds := o.interval.Seconds()
	for symbol, s := range snapshot {
		attrs := []any{
			"symbol", symbol,
			"mark", s.lastMark,
			"marks_per_sec", fmt.Sprintf("%.1f", float64(s.marks)/seconds),
			"books_per_sec", fmt.Sprintf("%.1f", float64(s.books)/seconds),
			"max_tick_move", s.maxMoveBps,
		}
		if s.hasSpread {
			attrs = append(attrs, "spread", s.spreadBps)
		}
		if drift, err := money.RelativeBps(s.firstMark, s.lastMark); err == nil {
			attrs = append(attrs, "drift", drift)
		}
		o.log.Info("сводка рынка", attrs...)
	}
}

var _ ports.Runnable = (*Observer)(nil)
