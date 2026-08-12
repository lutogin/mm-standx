package news

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nd/mm-standx/internal/domain/calendar"
	"github.com/nd/mm-standx/internal/events"
	"github.com/nd/mm-standx/internal/ports"
)

var (
	now     = time.Date(2026, 8, 12, 10, 0, 0, 0, time.UTC)
	release = time.Date(2026, 8, 12, 12, 30, 0, 0, time.UTC)
)

func config() Config {
	return Config{
		Refresh: time.Hour,
		MaxAge:  6 * time.Hour,
		Filter: calendar.Filter{
			Currencies: []string{"USD"},
			Impacts:    []calendar.Impact{calendar.ImpactHigh},
		},
		Policy: calendar.Policy{
			DefensiveBefore: 15 * time.Minute,
			DefensiveAfter:  15 * time.Minute,
			HardBefore:      time.Minute,
			HardAfter:       time.Minute,
		},
		Timeout: time.Second,
	}
}

func newPoller(t *testing.T, source ports.NewsSource) (*Poller, *[]events.NewsCalendarUpdated) {
	t.Helper()

	bus := events.New(func(string, any) { t.Fatal("handler panicked") })
	var seen []events.NewsCalendarUpdated
	events.Subscribe(bus, "test", func(_ context.Context, ev events.NewsCalendarUpdated) {
		seen = append(seen, ev)
	})

	p := New(config(), source, bus, &fixedClock{now: now}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return p, &seen
}

func TestPublishedCalendarCarriesOnlyTheSelectedReleases(t *testing.T) {
	p, seen := newPoller(t, sourceOf(
		calendar.Event{At: release, Title: "CPI m/m", Currency: "USD", Impact: calendar.ImpactHigh},
		calendar.Event{At: release, Title: "Bank Lending", Currency: "JPY", Impact: calendar.ImpactLow},
	))

	p.refresh(context.Background())

	require.Len(t, *seen, 1)
	published := (*seen)[0]
	require.Equal(t, now, published.Fetched)
	require.Len(t, published.Calendar.Events(), 1)
	require.Equal(t, "CPI m/m", published.Calendar.Events()[0].Title)
}

// Расписание публикуется целиком, а не как команда: подписчик считает позу
// по своим часам, и пропущенное сообщение не оставляет его в неверной позе.
func TestPublishedCalendarIsUsableOnItsOwn(t *testing.T) {
	p, seen := newPoller(t, sourceOf(
		calendar.Event{At: release, Title: "CPI m/m", Currency: "USD", Impact: calendar.ImpactHigh},
	))

	p.refresh(context.Background())
	published := (*seen)[0].Calendar

	require.Equal(t, calendar.Normal, published.Assess(release.Add(-time.Hour)).Posture)
	require.Equal(t, calendar.Defensive, published.Assess(release.Add(-10*time.Minute)).Posture)
	require.Equal(t, calendar.Hard, published.Assess(release).Posture)
}

// Неудачное чтение не отменяет уже известного расписания: недельный файл
// покрывает неделю, и публикации из него остаются верными.
func TestFailedRefreshKeepsThePreviousCalendar(t *testing.T) {
	source := &flakySource{events: []calendar.Event{
		{At: release, Title: "CPI m/m", Currency: "USD", Impact: calendar.ImpactHigh},
	}}
	p, seen := newPoller(t, source)

	p.refresh(context.Background())
	source.fail = true
	p.refresh(context.Background())

	require.Len(t, *seen, 2)
	require.Len(t, (*seen)[1].Calendar.Events(), 1,
		"the known release survives an unavailable publisher")
	require.Equal(t, now, (*seen)[1].Fetched, "but its age does not move forward")
}

// Публикация происходит и при неудаче: подписчику нужен возраст расписания,
// по нему он решает, доверять ли ему вообще.
func TestFirstFailurePublishesAnEmptyCalendarWithNoTimestamp(t *testing.T) {
	p, seen := newPoller(t, &flakySource{fail: true})

	p.refresh(context.Background())

	require.Len(t, *seen, 1)
	require.True(t, (*seen)[0].Fetched.IsZero(), "never read is not the same as read long ago")
	require.Empty(t, (*seen)[0].Calendar.Events())
}

// Первое чтение синхронно: без него актор котирования начал бы работу,
// ничего не зная о ближайшей публикации.
func TestStartReadsTheCalendarBeforeReturning(t *testing.T) {
	p, seen := newPoller(t, sourceOf(
		calendar.Event{At: release, Title: "CPI m/m", Currency: "USD", Impact: calendar.ImpactHigh},
	))

	require.NoError(t, p.Start(context.Background()))
	t.Cleanup(func() { _ = p.Stop(context.Background()) })

	require.NotEmpty(t, *seen)
}

// Ошибка чтения не мешает запуску: решение о том, котировать ли без календаря,
// принимается там, где известна цена вопроса.
func TestStartSurvivesAnUnavailablePublisher(t *testing.T) {
	p, _ := newPoller(t, &flakySource{fail: true})

	require.NoError(t, p.Start(context.Background()))
	require.NoError(t, p.Stop(context.Background()))
}

// Расписание рассылается чаще, чем читается, и это не избыточность.
// Единственная публикация при старте попадает в шину, на которую подписчик мог
// ещё не подписаться: акторы котирования поднимаются позже, и первое
// расписание уходило в пустоту — сводка сообщала «no calendar» при исправно
// прочитанном календаре, а исправлялось это только следующим чтением.
func TestCalendarIsRepublishedWithoutRefetching(t *testing.T) {
	source := &countingSource{events: []calendar.Event{
		{At: release, Title: "CPI m/m", Currency: "USD", Impact: calendar.ImpactHigh},
	}}
	p, seen := newPoller(t, source)

	p.refresh(context.Background())
	require.Equal(t, 1, source.calls)

	// Второй такт наступает раньше срока следующего чтения.
	p.publish(now)

	require.Len(t, *seen, 2)
	require.Equal(t, 1, source.calls, "republishing must not cost a request")
	require.Len(t, (*seen)[1].Calendar.Events(), 1)
}

// Неудача сдвигает следующее чтение на короткий отступ, а не на полный период:
// недоступный издатель — повод повторить скоро, а не ждать два часа.
func TestFailureRetriesSoonerThanTheRefreshPeriod(t *testing.T) {
	source := &countingSource{fail: true}
	p, _ := newPoller(t, source)

	p.refresh(context.Background())

	require.Equal(t, now.Add(retryInterval), p.nextFetch)
	require.Less(t, retryInterval, config().Refresh)
}

type countingSource struct {
	events []calendar.Event
	fail   bool
	calls  int
}

func (s *countingSource) Events(context.Context) ([]calendar.Event, error) {
	s.calls++
	if s.fail {
		return nil, errors.New("publisher is unavailable")
	}
	return s.events, nil
}

func sourceOf(events ...calendar.Event) ports.NewsSource {
	return &flakySource{events: events}
}

type flakySource struct {
	events []calendar.Event
	fail   bool
}

func (s *flakySource) Events(context.Context) ([]calendar.Event, error) {
	if s.fail {
		return nil, errors.New("publisher is unavailable")
	}
	return s.events, nil
}

// fixedClock — часы, стоящие на месте: поллер решений по времени не принимает,
// он лишь штампует возраст расписания.
type fixedClock struct{ now time.Time }

func (c *fixedClock) Now() time.Time                             { return c.now }
func (c *fixedClock) After(time.Duration) <-chan time.Time       { return make(chan time.Time) }
func (c *fixedClock) NewTicker(time.Duration) ports.Ticker       { return stoppedTicker{} }
func (c *fixedClock) Sleep(context.Context, time.Duration) error { return nil }

type stoppedTicker struct{}

func (stoppedTicker) C() <-chan time.Time { return make(chan time.Time) }
func (stoppedTicker) Stop()               {}

var _ ports.Clock = (*fixedClock)(nil)
