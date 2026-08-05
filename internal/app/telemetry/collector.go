// Package telemetry переводит в метрики то, что не принадлежит ни одному актору.
//
// Здоровье соединений и остаток кредитов шейпера — свойства процесса целиком,
// а не отдельного инструмента. Раздать их акторам значило бы получить столько
// счётчиков переподключений, сколько включено символов, и ни одного верного.
package telemetry

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/nd/mm-standx/internal/events"
	"github.com/nd/mm-standx/internal/ports"
)

// Collector подписывается на события соединений и опрашивает шейпер.
type Collector struct {
	interval time.Duration
	limiter  ports.RateLimiter
	bus      *events.Bus
	clock    ports.Clock
	metrics  ports.Metrics
	log      *slog.Logger

	unsubscribe []func()
	cancel      context.CancelFunc
	done        chan struct{}
}

// New собирает сборщик.
func New(
	interval time.Duration,
	limiter ports.RateLimiter,
	bus *events.Bus,
	clk ports.Clock,
	m ports.Metrics,
	log *slog.Logger,
) *Collector {
	return &Collector{
		interval: interval,
		limiter:  limiter,
		bus:      bus,
		clock:    clk,
		metrics:  m,
		log:      log.With("component", "telemetry"),
		done:     make(chan struct{}),
	}
}

func (c *Collector) Name() string { return "telemetry" }

func (c *Collector) Start(context.Context) error {
	c.unsubscribe = []func(){
		// Считается именно восстановление, а не разрыв: разрыв, после которого
		// соединение не поднялось, виден по метрикам как отсутствие данных,
		// тогда как череда успешных переподключений иначе выглядела бы
		// как безупречная работа.
		events.Subscribe(c.bus, c.Name(), func(_ context.Context, e events.StreamConnected) {
			c.metrics.StreamReconnect(e.Stream)
		}),
		events.Subscribe(c.bus, c.Name(), func(_ context.Context, e events.OrderRejected) {
			c.log.Debug("order rejected", "cl_ord_id", e.ClOrdID, "code", e.Code, "reason", e.Reason)
		}),
	}

	ctx, cancel := context.WithCancel(context.Background())
	c.cancel = cancel

	go func() {
		defer close(c.done)
		c.loop(ctx)
	}()
	return nil
}

func (c *Collector) Stop(ctx context.Context) error {
	for _, unsub := range c.unsubscribe {
		unsub()
	}
	if c.cancel == nil {
		return nil
	}
	c.cancel()

	select {
	case <-c.done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("telemetry: did not stop in time: %w", ctx.Err())
	}
}

func (c *Collector) loop(ctx context.Context) {
	ticker := c.clock.NewTicker(c.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C():
			if c.limiter != nil {
				c.metrics.RateLimitTokens(c.limiter.Available())
			}
		}
	}
}

var _ ports.Runnable = (*Collector)(nil)
