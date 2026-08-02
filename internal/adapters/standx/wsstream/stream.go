// Package wsstream подписывается на потоки рыночных данных и состояния аккаунта.
//
// Здесь только прикладной протокол StandX: авторизация, подписки и разбор
// сообщений. Переподключение, ротация и проверка живости обеспечиваются
// общим циклом из пакета wsconn.
package wsstream

import (
	"context"
	"errors"
	"log/slog"
	"sync"

	"github.com/nd/mm-standx/internal/adapters/wsconn"
	"github.com/nd/mm-standx/internal/config"
	"github.com/nd/mm-standx/internal/domain/market"
	"github.com/nd/mm-standx/internal/events"
	"github.com/nd/mm-standx/internal/ports"
)

// Subscription — подписка на канал. Symbol пуст для пользовательских каналов.
type Subscription struct {
	Channel string
	Symbol  market.Symbol
}

func (s Subscription) isUserChannel() bool { return s.Symbol == "" }

// New собирает поток рыночных данных.
//
// tokens может быть nil, если запрошены только публичные каналы.
func New(
	name string,
	url string,
	cfg config.Exchange,
	subs []Subscription,
	tokens ports.TokenProvider,
	bus *events.Bus,
	clock ports.Clock,
	recorder ports.Recorder,
	log *slog.Logger,
) *wsconn.Loop {
	h := &handler{
		name:     name,
		subs:     subs,
		tokens:   tokens,
		bus:      bus,
		recorder: recorder,
		log:      log.With("component", "wsstream", "stream", name),
	}
	return wsconn.New(name, url, cfg, h, bus, clock, log)
}

// handler реализует прикладной протокол потока.
type handler struct {
	name     string
	subs     []Subscription
	tokens   ports.TokenProvider
	bus      *events.Bus
	recorder ports.Recorder
	log      *slog.Logger

	// lastSeq отслеживает сквозную нумерацию сообщений соединения.
	seqMu   sync.Mutex
	lastSeq int64
}

// OnConnect выполняет авторизацию и подписку на каналы.
func (h *handler) OnConnect(ctx context.Context, s wsconn.Sender) error {
	var public, user []Subscription
	for _, sub := range h.subs {
		if sub.isUserChannel() {
			user = append(user, sub)
		} else {
			public = append(public, sub)
		}
	}

	if len(user) > 0 {
		if h.tokens == nil {
			return errors.New("user channels requested but no token provider is configured")
		}
		token, err := h.tokens.Token(ctx)
		if err != nil {
			return err
		}
		streams := make([]map[string]string, 0, len(user))
		for _, sub := range user {
			streams = append(streams, map[string]string{"channel": sub.Channel})
		}
		// Каналы передаём прямо в запросе авторизации: отдельная подписка после
		// auth дала бы окно, в котором обновления ордеров теряются.
		if err := s.SendJSON(ctx, map[string]any{
			"auth": map[string]any{"token": token, "streams": streams},
		}); err != nil {
			return err
		}
	}

	for _, sub := range public {
		if err := s.SendJSON(ctx, map[string]any{
			"subscribe": map[string]string{"channel": sub.Channel, "symbol": sub.Symbol.String()},
		}); err != nil {
			return err
		}
	}
	return nil
}

// OnDisconnect сбрасывает счётчик сообщений: нумерация начинается заново
// на каждом новом сокете.
func (h *handler) OnDisconnect(error) {
	h.seqMu.Lock()
	h.lastSeq = 0
	h.seqMu.Unlock()
}

var _ wsconn.Handler = (*handler)(nil)
