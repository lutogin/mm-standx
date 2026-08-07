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
	"sync/atomic"
	"time"

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
		clock:    clock,
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
	clock    ports.Clock
	bus      *events.Bus
	recorder ports.Recorder
	log      *slog.Logger

	// lastSeq отслеживает сквозную нумерацию сообщений соединения.
	seqMu   sync.Mutex
	lastSeq int64

	// tradeShapeWarned гасит повтор предупреждения о разборе сделки: форма
	// сообщения одна на всё соединение, и печатать её на каждой сделке значит
	// заполнить лог одним и тем же.
	tradeShapeWarned atomic.Bool
}

// warnOnce печатает предупреждение первый раз и молчит дальше.
func (h *handler) warnOnce(flag *atomic.Bool, msg string, attrs ...any) {
	if flag.Swap(true) {
		return
	}
	h.log.Warn(msg, attrs...)
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

	for i, sub := range public {
		// Подписки разносятся во времени. Найдено измерением: одиннадцать
		// инструментов дают двадцать два кадра подряд, и последний из них
		// биржа молча теряла — XAU-USD не присылал ни одного сообщения,
		// оставаясь при этом в списке инструментов биржи. Отказа в ответ
		// не приходит, поэтому единственная защита — не упираться в предел.
		if i > 0 {
			if err := h.clock.Sleep(ctx, subscribePacing); err != nil {
				return err
			}
		}
		if err := s.SendJSON(ctx, map[string]any{
			"subscribe": map[string]string{"channel": sub.Channel, "symbol": sub.Symbol.String()},
		}); err != nil {
			return err
		}
	}
	h.log.Info("subscribed", "channels", len(public)+len(user))
	return nil
}

// subscribePacing — пауза между кадрами подписки.
//
// Десять в секунду при биржевом пределе около двадцати двух: запас двукратный,
// а вся подписка на одиннадцать инструментов занимает пару секунд, что рядом
// с многоминутным прогревом оценки риска незаметно.
const subscribePacing = 100 * time.Millisecond

// OnDisconnect сбрасывает счётчик сообщений: нумерация начинается заново
// на каждом новом сокете.
func (h *handler) OnDisconnect(error) {
	h.seqMu.Lock()
	h.lastSeq = 0
	h.seqMu.Unlock()
}

var _ wsconn.Handler = (*handler)(nil)
