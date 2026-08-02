// Package ratelimit шейпит исходящие запросы ниже биржевого лимита.
package ratelimit

import (
	"context"
	"time"

	"golang.org/x/time/rate"

	"github.com/nd/mm-standx/internal/config"
	"github.com/nd/mm-standx/internal/ports"
)

// Limiter — токен-бакет с резервом под команды безопасности.
//
// Смысл резерва: бот, потративший весь бюджет запросов на перекотировки ради
// награды, обязан сохранить возможность снять ордер перед заливом. Поэтому
// запросы приоритета ниже PrioritySafety не имеют права опустошать бакет
// полностью — им доступна лишь часть сверх резерва.
//
// Резерв — мягкая гарантия: между проверкой остатка и захватом токена возможна
// гонка с другой горутиной. Худший случай — команда безопасности ждёт одно
// время пополнения (около 67 мс при 15 rps), что приемлемо.
type Limiter struct {
	lim            *rate.Limiter
	reserve        float64
	refillInterval time.Duration
	clock          ports.Clock
}

// New создаёт шейпер по конфигурации.
func New(cfg config.RateLimit, clock ports.Clock) *Limiter {
	burst := cfg.Burst
	if burst < 1 {
		burst = 1
	}
	return &Limiter{
		lim:            rate.NewLimiter(rate.Limit(cfg.SustainedRPS), burst),
		reserve:        float64(burst) * cfg.SafetyReservePct / 100,
		refillInterval: time.Duration(float64(time.Second) / cfg.SustainedRPS),
		clock:          clock,
	}
}

// Acquire блокирует до появления кредита либо до отмены контекста.
func (l *Limiter) Acquire(ctx context.Context, p ports.Priority) error {
	if p != ports.PrioritySafety {
		if err := l.waitAboveReserve(ctx); err != nil {
			return err
		}
	}
	return l.lim.Wait(ctx)
}

// Available возвращает число доступных запросов — для метрик.
func (l *Limiter) Available() float64 { return l.lim.Tokens() }

// waitAboveReserve ждёт, пока в бакете не окажется больше токенов, чем размер резерва.
func (l *Limiter) waitAboveReserve(ctx context.Context) error {
	for l.lim.Tokens() <= l.reserve {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-l.clock.After(l.refillInterval):
		}
	}
	return nil
}
