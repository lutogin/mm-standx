package wsconn

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/require"

	"github.com/nd/mm-standx/internal/adapters/clock"
	"github.com/nd/mm-standx/internal/config"
	"github.com/nd/mm-standx/internal/events"
)

// Признак необязательности объявляет сам обработчик: цикл соединения ничего
// не знает о том, чей это канал, и знать не должен.
func TestOptionalIsTakenFromTheHandler(t *testing.T) {
	require.False(t, optional(plainHandler{}), "a handler that says nothing is required")
	require.False(t, optional(declaredHandler{optional: false}),
		"a handler may declare itself required explicitly")
	require.True(t, optional(declaredHandler{optional: true}))
}

// The venue drops the socket at the twenty-fourth hour, so the loop rotates
// earlier and takes the break at a moment of its own choosing. The rotation had
// never been exercised: every prior session ended for some other reason long
// before the deadline, and PLAN.md §0 listed it as unverified because of that.
//
// Here the deadline is milliseconds instead of hours and the server does
// nothing at all — it neither closes nor stops answering — so the only thing
// that can end a session is the rotation itself.
func TestSessionRotatesWithoutHelpFromTheServer(t *testing.T) {
	url := websocketServer(t, func(ctx context.Context, c *websocket.Conn) {
		// Hold the connection open and say nothing. A server-side close, or a
		// read deadline expiring, would end the session for a reason other than
		// the one under test.
		<-ctx.Done()
	})

	sessions := newSessionCounter()
	loop := testLoop(t, url, sessions.bus, func(l *Loop) {
		l.cfg.WSRotateAfter = 80 * time.Millisecond
	})
	startLoop(t, loop)

	require.Eventually(t, func() bool { return sessions.count() >= 3 },
		5*time.Second, 5*time.Millisecond,
		"the loop must end the session on its own and reconnect")
}

// Планово разорванное соединение — не признак неисправности, и задержка
// переподключения после него обязана вернуться к минимуму.
//
// Без сброса экспонента растёт от ротации к ротации: двадцать часов, сорок,
// восемьдесят — и через несколько суток процесс ждёт WSReconnectMax после
// каждого планового разрыва, то есть простаивает по собственному решению.
func TestPlannedRotationDoesNotRatchetTheBackoff(t *testing.T) {
	url := websocketServer(t, func(ctx context.Context, c *websocket.Conn) {
		<-ctx.Done()
	})

	sessions := newSessionCounter()
	loop := testLoop(t, url, sessions.bus, func(l *Loop) {
		l.cfg.WSRotateAfter = 20 * time.Millisecond
		l.cfg.WSReconnectMin = 20 * time.Millisecond
		l.cfg.WSReconnectMax = 2 * time.Second
		// A session outliving this counts as healthy. Production uses a minute
		// against a twenty-hour rotation; the ratio here is deliberately alike.
		l.healthyAfter = 5 * time.Millisecond
	})

	start := time.Now()
	startLoop(t, loop)

	// Eight sessions of 20 ms with a reset wait of 20 ms between them run in
	// about 300 ms, and up to 380 ms once the jitter adds its half. Ratcheting
	// instead, the seven waits would be 20, 40, 80, 160, 320, 640 and 1280 ms,
	// so the same eight sessions could not finish inside three seconds.
	require.Eventually(t, func() bool { return sessions.count() >= 8 },
		5*time.Second, time.Millisecond)
	require.Less(t, time.Since(start), 800*time.Millisecond,
		"the wait after a healthy session must return to WSReconnectMin")
}

// Соединение, оборвавшееся сразу, здоровым не считается, и задержка растёт.
// Это та же строка кода, но противоположный её исход: без него сброс означал бы
// отсутствие экспоненты вообще, то есть переподключения без передышки в тот
// самый момент, когда бирже плохо.
func TestShortSessionKeepsTheBackoffGrowing(t *testing.T) {
	url := websocketServer(t, func(ctx context.Context, c *websocket.Conn) {
		_ = c.Close(websocket.StatusNormalClosure, "immediate")
	})

	sessions := newSessionCounter()
	loop := testLoop(t, url, sessions.bus, func(l *Loop) {
		l.cfg.WSReconnectMin = 20 * time.Millisecond
		l.cfg.WSReconnectMax = time.Second
		l.healthyAfter = time.Hour // nothing here can qualify as healthy
	})

	start := time.Now()
	startLoop(t, loop)

	// 20 + 40 + 80 + 160 = 300 ms of waiting before the fifth session, against
	// 80 ms if the delay were reset every time.
	require.Eventually(t, func() bool { return sessions.count() >= 5 },
		5*time.Second, time.Millisecond)
	require.Greater(t, time.Since(start), 200*time.Millisecond,
		"a session that never became healthy must keep doubling the wait")
}

// websocketServer serves one upgraded connection per request and returns its
// ws:// address. serve owns the connection and returns when it is done; its
// context is cancelled once the test ends.
func websocketServer(t *testing.T, serve func(context.Context, *websocket.Conn)) string {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		serve(ctx, c)
	}))
	t.Cleanup(func() {
		cancel()
		srv.Close()
	})
	return "ws" + strings.TrimPrefix(srv.URL, "http")
}

// testLoop builds a loop against the test server with timings short enough for
// a unit test. tune runs after construction, so a case states only the values
// its own claim rests on.
func testLoop(t *testing.T, url string, bus *events.Bus, tune func(*Loop)) *Loop {
	t.Helper()

	cfg := config.Exchange{
		WSRotateAfter:  time.Hour,
		WSReconnectMin: 5 * time.Millisecond,
		WSReconnectMax: time.Second,
		// Silence must not be what ends a session in these tests: the server
		// deliberately says nothing, and a read deadline would then be
		// indistinguishable from the rotation under test.
		WSIdleTimeout: time.Hour,
	}
	l := New("test", url, cfg, plainHandler{}, bus, clock.New(),
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	tune(l)
	return l
}

func startLoop(t *testing.T, l *Loop) {
	t.Helper()
	require.NoError(t, l.Start(context.Background()))
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		require.NoError(t, l.Stop(ctx))
	})
}

// sessionCounter counts completed handshakes.
//
// Sessions are counted from the loop's own StreamConnected event rather than
// from accepted dials on the server: a dial whose handshake failed is not a
// session, and the two numbers part company exactly there.
type sessionCounter struct {
	bus *events.Bus
	n   atomic.Int64
}

func newSessionCounter() *sessionCounter {
	s := &sessionCounter{bus: events.New(nil)}
	events.Subscribe(s.bus, "test", func(context.Context, events.StreamConnected) {
		s.n.Add(1)
	})
	return s
}

func (s *sessionCounter) count() int { return int(s.n.Load()) }

type plainHandler struct{}

func (plainHandler) OnConnect(context.Context, Sender) error      { return nil }
func (plainHandler) OnMessage(context.Context, []byte, time.Time) {}
func (plainHandler) OnDisconnect(error)                           {}

type declaredHandler struct {
	plainHandler
	optional bool
}

func (h declaredHandler) Optional() bool { return h.optional }

var (
	_ Handler  = plainHandler{}
	_ Optional = declaredHandler{}
)
