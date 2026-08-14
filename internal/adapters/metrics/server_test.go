package metrics

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nd/mm-standx/internal/domain/market"
	"github.com/nd/mm-standx/internal/ports"
)

type fixedClock struct{ now time.Time }

func (c *fixedClock) Now() time.Time                             { return c.now }
func (c *fixedClock) After(time.Duration) <-chan time.Time       { return make(chan time.Time) }
func (c *fixedClock) NewTicker(time.Duration) ports.Ticker       { return stubTicker{} }
func (c *fixedClock) Sleep(context.Context, time.Duration) error { return nil }

type stubTicker struct{}

func (stubTicker) C() <-chan time.Time { return make(chan time.Time) }
func (stubTicker) Stop()               {}

type stubActors struct {
	silent time.Duration
	stale  bool
}

func (s *stubActors) Stalest(time.Time) (market.Symbol, time.Duration, bool) {
	return "XAU-USD", s.silent, s.stale
}

// probe поднимает сервер на свободном порту и опрашивает /healthz.
func probe(t *testing.T, actors ports.LivenessSource) *http.Response {
	t.Helper()

	s := NewServer("127.0.0.1:0", New(), actors, &fixedClock{now: time.Now()},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.NoError(t, s.Start(context.Background()))
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.Stop(ctx)
	})

	resp, err := http.Get("http://" + s.listener.Addr().String() + "/healthz")
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func TestHealthzIsOkWhileActorsTick(t *testing.T) {
	resp := probe(t, &stubActors{})
	require.Equal(t, http.StatusOK, resp.StatusCode)
}

// Главное свойство правки. Сторожа в Docker нет, поэтому замерший актор виден
// боевой поставке только отсюда: 13 августа контейнер отвечал healthy тридцать
// пять минут при мёртвой торговле, и healthcheck не имел оснований возразить.
func TestHealthzFailsWhenAnActorStoppedTicking(t *testing.T) {
	resp := probe(t, &stubActors{silent: 40 * time.Minute, stale: true})

	require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Contains(t, string(body), "XAU-USD",
		"the probe names which actor went silent, not just that something did")
}

// Утилиты поднимают сервер без акторов: проба обязана остаться пробой процесса.
func TestHealthzWithoutALivenessSourceReportsTheProcess(t *testing.T) {
	resp := probe(t, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
}
