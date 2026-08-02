// Package clock содержит реализацию ports.Clock поверх системного времени.
package clock

import (
	"context"
	"time"

	"github.com/nd/mm-standx/internal/ports"
)

// System — часы, работающие от реального времени.
type System struct{}

func New() ports.Clock { return System{} }

func (System) Now() time.Time { return time.Now() }

func (System) After(d time.Duration) <-chan time.Time { return time.After(d) }

func (System) NewTicker(d time.Duration) ports.Ticker {
	return &systemTicker{t: time.NewTicker(d)}
}

// Sleep ждёт указанное время, прерываясь при отмене контекста.
func (System) Sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

type systemTicker struct{ t *time.Ticker }

func (s *systemTicker) C() <-chan time.Time { return s.t.C }
func (s *systemTicker) Stop()               { s.t.Stop() }
