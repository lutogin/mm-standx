// Package news keeps the economic calendar current and publishes it.
//
// The actor holds no decision of its own. It fetches, filters and republishes;
// what a release means for quoting is decided by the quoting actor against its
// own clock, so a missed message cannot leave the bot in the wrong posture.
package news

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/nd/mm-standx/internal/domain/calendar"
	"github.com/nd/mm-standx/internal/events"
	"github.com/nd/mm-standx/internal/ports"
)

// Config — параметры опроса календаря.
type Config struct {
	// Refresh — период повторного чтения. Расписание на неделю меняется редко,
	// поэтому частить незачем.
	Refresh time.Duration
	// MaxAge — возраст, после которого расписание считается несвежим.
	//
	// Свежесть здесь не про содержание, а про доверие: файл покрывает неделю
	// вперёд и от суточного возраста хуже не становится. Но расписание, которое
	// перестало читаться, могло и измениться — публикацию переносят, — и
	// молчаливо доверять ему сколько угодно долго нельзя.
	MaxAge time.Duration
	// Filter отбирает публикации, которые вообще касаются наших инструментов.
	Filter calendar.Filter
	// Policy — насколько широко расходится каждая поза вокруг публикации.
	Policy calendar.Policy
	// Timeout ограничивает одно обращение к издателю.
	Timeout time.Duration
}

// Poller — актор, поддерживающий календарь в актуальном состоянии.
type Poller struct {
	cfg    Config
	source ports.NewsSource
	bus    *events.Bus
	clock  ports.Clock
	log    *slog.Logger

	fetched time.Time
	events  []calendar.Event
	// nextFetch — когда позволено обратиться к издателю в следующий раз.
	// Отделено от fetched, потому что после неудачи повторять нужно раньше,
	// чем наступит обычный срок.
	nextFetch time.Time

	cancel context.CancelFunc
	done   chan struct{}
}

func New(
	cfg Config,
	source ports.NewsSource,
	bus *events.Bus,
	clock ports.Clock,
	log *slog.Logger,
) *Poller {
	return &Poller{
		cfg:    cfg,
		source: source,
		bus:    bus,
		clock:  clock,
		log:    log.With("component", "news"),
		done:   make(chan struct{}),
	}
}

func (p *Poller) Name() string { return "news" }

// Start читает календарь один раз синхронно, затем уходит в фон.
//
// Первое чтение синхронно намеренно: за ним следует публикация расписания,
// и без неё актор котирования начал бы работу, ничего не зная о ближайшей
// публикации. Ошибка при этом не фатальна — решение о том, котировать ли
// без календаря, принимается там, где известна цена вопроса.
func (p *Poller) Start(ctx context.Context) error {
	p.refresh(ctx)

	runCtx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel

	go func() {
		defer close(p.done)
		p.loop(runCtx)
	}()
	return nil
}

func (p *Poller) Stop(ctx context.Context) error {
	if p.cancel == nil {
		return nil
	}
	p.cancel()

	select {
	case <-p.done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("news: did not stop in time: %w", ctx.Err())
	}
}

const (
	// republishInterval — период повторной публикации уже известного расписания.
	//
	// Расписание рассылается заметно чаще, чем читается, и это не избыточность.
	// Публикация один раз при старте попадает в шину, на которую подписчик мог
	// ещё не подписаться: акторы котирования поднимаются после потоков данных,
	// и первое расписание уходило в пустоту — сводка сообщала «no calendar»
	// при исправно прочитанном календаре, а исправлялось это только следующим
	// чтением через два часа. Регулярная рассылка делает состояние сходящимся
	// независимо от порядка запуска.
	republishInterval = 30 * time.Second

	// retryInterval — отступ после неудачного чтения.
	//
	// Заметно короче обычного периода: недоступный издатель — повод повторить
	// скоро, а не ждать два часа. И заметно длиннее рассылки: долбить чужой
	// сервер каждые полминуты во время его аварии незачем.
	retryInterval = 5 * time.Minute
)

func (p *Poller) loop(ctx context.Context) {
	ticker := p.clock.NewTicker(republishInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C():
			now := p.clock.Now()
			if !now.Before(p.nextFetch) {
				p.refresh(ctx)
				continue
			}
			p.publish(now)
		}
	}
}

// refresh читает календарь и публикует его.
//
// Публикация происходит и при неудаче: подписчику нужен не только состав
// расписания, но и его возраст — по нему он решает, доверять ли ему вообще.
// Прежний список при этом сохраняется: файл покрывает неделю, и уже известные
// публикации остаются верными, даже когда издатель недоступен.
func (p *Poller) refresh(ctx context.Context) {
	fetchCtx, cancel := context.WithTimeout(ctx, p.cfg.Timeout)
	defer cancel()

	now := p.clock.Now()
	raw, err := p.source.Events(fetchCtx)
	if err != nil {
		p.nextFetch = now.Add(retryInterval)
		age := "never"
		if !p.fetched.IsZero() {
			age = now.Sub(p.fetched).Truncate(time.Second).String()
		}
		p.log.Error("failed to read the economic calendar, keeping what we have",
			"error", err, "age", age, "events", len(p.events), "retry_in", retryInterval)
		p.publish(now)
		return
	}

	p.events = p.cfg.Filter.Select(raw)
	p.fetched = now
	p.nextFetch = now.Add(p.cfg.Refresh)
	p.publish(now)

	attrs := []any{"events", len(p.events), "of", len(raw)}
	if next, ok := calendar.New(p.cfg.Policy, p.events).Next(now); ok {
		attrs = append(attrs,
			"next", next.At.UTC().Format(time.RFC3339),
			"title", next.Title,
			"in", next.At.Sub(now).Truncate(time.Minute).String())
	}
	p.log.Info("economic calendar refreshed", attrs...)
}

func (p *Poller) publish(now time.Time) {
	events.Publish(p.bus, context.Background(), events.NewsCalendarUpdated{
		Calendar: calendar.New(p.cfg.Policy, p.events),
		Fetched:  p.fetched,
		At:       now,
	})
}

var _ ports.Runnable = (*Poller)(nil)
