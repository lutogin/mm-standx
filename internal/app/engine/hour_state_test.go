package engine

import (
	"context"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"

	"github.com/nd/mm-standx/internal/domain/uptime"
)

// memStore — хранилище снимков в памяти, повторяющее контракт ports.StateStore.
type memStore struct {
	saved   map[string]uptime.State
	loadErr error
	saveErr error
}

func newMemStore() *memStore { return &memStore{saved: map[string]uptime.State{}} }

func (m *memStore) Load(name string, into any) (bool, error) {
	if m.loadErr != nil {
		return false, m.loadErr
	}
	s, ok := m.saved[name]
	if !ok {
		return false, nil
	}
	*(into.(*uptime.State)) = s
	return true, nil
}

func (m *memStore) Save(name string, value any) error {
	if m.saveErr != nil {
		return m.saveErr
	}
	m.saved[name] = value.(uptime.State)
	return nil
}

// Присутствие текущего часа переносится через перезапуск.
//
// Замер v1 за 9 сентября 2026: рестарт в 11:29, через минуту час объявлен
// недостижимым, закрылся на 0.00x. Биржа считала книгу непрерывно.
func TestHourlyPresenceSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	e, _, clk := dangerEngine(t)
	st := newMemStore()
	e.deps.Store = st

	// Полчаса присутствия, затем остановка.
	at := clk.now.UTC().Truncate(time.Hour)
	for range 1800 {
		e.hour.Sample(at, decimal.NewFromInt(60))
		at = at.Add(time.Second)
	}
	banked := e.hour.Assess(at).BankedPct
	require.InDelta(t, 50, banked, 2)
	e.saveHour()
	require.Len(t, st.saved, 1, "the snapshot must be written under the symbol's name")

	// Новый процесс в том же часе поднимает снимок.
	fresh, _, clk2 := dangerEngine(t)
	fresh.deps.Store = st
	clk2.now = at.Add(10 * time.Second)
	fresh.restoreHour()

	require.InDelta(t, banked, fresh.hour.Assess(clk2.now).BankedPct, 2,
		"the banked half hour must come back")
	_ = ctx
}

// Снимок пишется ДО снятия котировок: он описывает час таким, каким его
// видела биржа, а не таким, каким он стал после уборки.
func TestSnapshotIsTakenBeforeQuotesAreWithdrawn(t *testing.T) {
	e, _, clk := dangerEngine(t)
	st := newMemStore()
	e.deps.Store = st

	at := clk.now.UTC().Truncate(time.Hour)
	for range 600 {
		e.hour.Sample(at, decimal.NewFromInt(60))
		at = at.Add(time.Second)
	}
	clk.now = at
	require.NoError(t, e.Stop(context.Background()))

	saved := st.saved[e.hourStateName()]
	positive := 0
	for _, v := range saved.Samples {
		if v.IsPositive() {
			positive++
		}
	}
	require.Positive(t, positive, "the snapshot must record the presence, not the cleanup")
}

// Отсутствие хранилища и нечитаемый снимок не мешают старту: час просто
// начинается с нуля, как до появления механизма.
func TestMissingOrBrokenSnapshotDoesNotBlockStart(t *testing.T) {
	e, _, _ := dangerEngine(t)
	e.deps.Store = nil
	require.NotPanics(t, e.restoreHour)
	require.NotPanics(t, e.saveHour)

	e2, _, _ := dangerEngine(t)
	broken := newMemStore()
	broken.loadErr = context.DeadlineExceeded
	e2.deps.Store = broken
	require.NotPanics(t, e2.restoreHour)
	require.Zero(t, e2.hour.Samples())
}

// Имя снимка привязано к инструменту: часы у символов независимы.
func TestSnapshotNameIsPerSymbol(t *testing.T) {
	e, _, _ := dangerEngine(t)
	require.Equal(t, "uptime-XAU-USD", e.hourStateName())
}
