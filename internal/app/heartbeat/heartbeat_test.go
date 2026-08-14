package heartbeat

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nd/mm-standx/internal/domain/market"
	"github.com/nd/mm-standx/internal/ports"
)

var epoch = time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)

type fixedClock struct{ now time.Time }

func (c *fixedClock) Now() time.Time                             { return c.now }
func (c *fixedClock) After(time.Duration) <-chan time.Time       { return make(chan time.Time) }
func (c *fixedClock) NewTicker(time.Duration) ports.Ticker       { return stubTicker{} }
func (c *fixedClock) Sleep(context.Context, time.Duration) error { return nil }

type stubTicker struct{}

func (stubTicker) C() <-chan time.Time { return make(chan time.Time) }
func (stubTicker) Stop()               {}

// stubActors — управляемый из теста ответ на вопрос «тикают ли акторы».
type stubActors struct {
	symbol market.Symbol
	silent time.Duration
	stale  bool
}

func (s *stubActors) Stalest(time.Time) (market.Symbol, time.Duration, bool) {
	return s.symbol, s.silent, s.stale
}

func writer(t *testing.T, actors ports.LivenessSource, clk *fixedClock) (*Writer, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "mmbot.hb")
	return New(path, time.Second, []market.Symbol{"XAU-USD"}, true,
		actors, clk, slog.New(slog.NewTextHandler(io.Discard, nil))), path
}

func TestATickingActorKeepsTheHeartbeatFresh(t *testing.T) {
	clk := &fixedClock{now: epoch}
	w, path := writer(t, &stubActors{}, clk)

	require.NoError(t, w.write())
	first, err := Read(path)
	require.NoError(t, err)
	require.Equal(t, epoch.UTC(), first.At.UTC())

	clk.now = epoch.Add(time.Minute)
	require.NoError(t, w.write())
	second, err := Read(path)
	require.NoError(t, err)
	require.Equal(t, epoch.Add(time.Minute).UTC(), second.At.UTC())
}

// Ровно тот отказ 13 августа: процесс жив и пишет, актор мёртв. Отметка обязана
// замереть, потому что сторож судит по её возрасту и никак иначе.
func TestAWedgedActorFreezesTheHeartbeat(t *testing.T) {
	clk := &fixedClock{now: epoch}
	actors := &stubActors{}
	w, path := writer(t, actors, clk)

	require.NoError(t, w.write())

	actors.symbol, actors.silent, actors.stale = "XAU-USD", 40*time.Minute, true
	clk.now = epoch.Add(40 * time.Minute)
	require.NoError(t, w.write())

	state, err := Read(path)
	require.NoError(t, err)
	require.Equal(t, epoch.UTC(), state.At.UTC(),
		"a live process with a dead actor must not look alive to the guardian")
}

// Замирание — не защёлка. Актор, вернувшийся после паузы защитного контура или
// закрытого базового рынка, обязан снова оживить отметку: постоянный ответ
// на транзиентное условие — тот самый дефект, который эта серия и закрывает
// (PLAN.md §14).
func TestAHeartbeatResumesWhenTheActorDoes(t *testing.T) {
	clk := &fixedClock{now: epoch}
	actors := &stubActors{}
	w, path := writer(t, actors, clk)

	actors.stale = true
	clk.now = epoch.Add(time.Minute)
	require.NoError(t, w.write())
	_, err := Read(path)
	require.Error(t, err, "nothing was ever written while the actor was silent")

	actors.stale = false
	clk.now = epoch.Add(2 * time.Minute)
	require.NoError(t, w.write())

	state, err := Read(path)
	require.NoError(t, err)
	require.Equal(t, epoch.Add(2*time.Minute).UTC(), state.At.UTC())
}

// Без источника живости поведение прежнее: признак жизни процесса. Нужно
// утилитам и тестам, которые акторов не поднимают вовсе.
func TestWithoutALivenessSourceTheWriterBehavesAsBefore(t *testing.T) {
	clk := &fixedClock{now: epoch}
	w, path := writer(t, nil, clk)

	require.NoError(t, w.write())
	state, err := Read(path)
	require.NoError(t, err)
	require.Equal(t, epoch.UTC(), state.At.UTC())
	require.Equal(t, []string{"XAU-USD"}, state.Symbols)
	require.True(t, state.Live)
}
