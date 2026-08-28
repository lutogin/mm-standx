// Package wsconn содержит самовосстанавливающийся цикл WebSocket-соединения.
//
// Логика переподключения, плановой ротации и проверки живости одинакова для
// потока рыночных данных и для торгового канала, поэтому вынесена сюда.
// Прикладной протокол задаётся реализацией Handler.
package wsconn

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/nd/mm-standx/internal/config"
	"github.com/nd/mm-standx/internal/events"
	"github.com/nd/mm-standx/internal/ports"
)

const (
	// readLimit ограничивает размер одного сообщения.
	readLimit = 1 << 20
	// healthyAfter — время, после которого соединение считается здоровым
	// и задержка переподключения сбрасывается.
	healthyAfter = time.Minute
)

// Проверка живости построена на молчании, а не на пинге.
//
// Активный ping использовать нельзя: сервер StandX на клиентские пинги
// не отвечает. Документация описывает только обратное направление — сервер
// шлёт ping каждые 10 секунд, клиент обязан ответить pong. Попытка пинговать
// самим приводила к разрыву исправного соединения ровно через
// pingInterval + pingTimeout, то есть к постоянным переподключениям.
//
// Вместо этого каждое чтение ограничено дедлайном: сообщений нет дольше
// допустимого — соединение считается мёртвым. Канал, на котором данные
// не ходят сами по себе, обязан реализовать Heartbeater.

// ErrNotConnected возвращается при попытке отправить сообщение в отсутствие
// установленного соединения.
var ErrNotConnected = errors.New("wsconn: connection is not established")

// Sender отправляет сообщения в конкретное соединение.
//
// Передаётся в Handler.OnConnect, чтобы рукопожатие прикладного уровня могло
// писать в сокет до того, как тот будет открыт для остальных отправителей.
type Sender interface {
	Send(ctx context.Context, payload []byte) error
	SendJSON(ctx context.Context, v any) error
}

// Handler описывает прикладной протокол поверх соединения.
type Handler interface {
	// OnConnect выполняет рукопожатие: авторизацию, подписки. Ошибка приводит
	// к переподключению.
	OnConnect(ctx context.Context, s Sender) error
	// OnMessage обрабатывает входящее текстовое сообщение. Не должен блокироваться.
	OnMessage(ctx context.Context, payload []byte, received time.Time)
	// OnDisconnect уведомляет о разрыве, чтобы прикладной слой сбросил
	// состояние, привязанное к соединению.
	OnDisconnect(err error)
}

// Heartbeater реализуется каналами, по которым данные не идут постоянно.
//
// Запрос обязан гарантированно вызывать ответ сервера: именно ответ, а не сам
// факт отправки, сбрасывает таймер молчания. Отправка в полуоткрытое TCP-соединение
// проходит успешно и живость не доказывает.
type Heartbeater interface {
	Heartbeat(ctx context.Context, s Sender) error
}

// Optional реализуется каналами, потеря которых ухудшает сигнал, но не лишает
// нас права принимать решения.
//
// Признак нужен подписчикам StreamDisconnected. Актор снимает котировки
// на разрыве потока, и это верно для каналов биржи: без mark price мы торгуем
// вслепую. Внешний ориентир устроен наоборот — без него мы возвращаемся ровно
// к той картине, по которой работали до его появления, и съём котировок платил
// бы присутствием за сбой чужой площадки.
type Optional interface {
	Optional() bool
}

// optional сообщает, объявил ли обработчик свой поток необязательным.
func optional(h Handler) bool {
	o, ok := h.(Optional)
	return ok && o.Optional()
}

// Loop поддерживает соединение живым, переподключаясь при разрывах.
type Loop struct {
	name    string
	url     string
	cfg     config.Exchange
	handler Handler
	bus     *events.Bus
	clock   ports.Clock
	log     *slog.Logger
	// optional запоминает ответ обработчика один раз: он не меняется в течение
	// жизни соединения, а спрашивать его на каждом разрыве значило бы допускать,
	// что мог бы измениться.
	optional bool
	// healthyAfter is how long a session must last to be judged healthy. It is
	// a field rather than the bare constant so the rotation test can shrink it:
	// the property under test is that a planned rotation resets the backoff,
	// and waiting a real minute to observe it would price the test out of the
	// suite it belongs in.
	healthyAfter time.Duration

	// active хранит соединение, открытое для внешних отправителей. Устанавливается
	// только после успешного рукопожатия: до этого писать в сокет вправе лишь
	// сам Handler.
	mu     sync.RWMutex
	active *websocket.Conn

	cancel context.CancelFunc
	done   chan struct{}
}

func New(
	name, url string,
	cfg config.Exchange,
	handler Handler,
	bus *events.Bus,
	clock ports.Clock,
	log *slog.Logger,
) *Loop {
	return &Loop{
		name:         name,
		url:          url,
		cfg:          cfg,
		handler:      handler,
		bus:          bus,
		clock:        clock,
		log:          log.With("component", "wsconn", "stream", name),
		optional:     optional(handler),
		healthyAfter: healthyAfter,
		done:         make(chan struct{}),
	}
}

func (l *Loop) Name() string { return l.name }

// Start запускает фоновый цикл и сразу возвращает управление.
func (l *Loop) Start(context.Context) error {
	// Собственный контекст: жизнь соединения определяется вызовом Stop,
	// а не контекстом запуска, который fx закрывает сразу после Start.
	ctx, cancel := context.WithCancel(context.Background())
	l.cancel = cancel

	go func() {
		defer close(l.done)
		l.run(ctx)
	}()
	return nil
}

func (l *Loop) Stop(ctx context.Context) error {
	if l.cancel == nil {
		return nil
	}
	l.cancel()

	select {
	case <-l.done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("wsconn %s: did not stop in time: %w", l.name, ctx.Err())
	}
}

// Connected сообщает, готово ли соединение принимать сообщения.
func (l *Loop) Connected() bool {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.active != nil
}

// Send отправляет сообщение в текущее соединение.
func (l *Loop) Send(ctx context.Context, payload []byte) error {
	l.mu.RLock()
	conn := l.active
	l.mu.RUnlock()

	if conn == nil {
		return ErrNotConnected
	}
	// Библиотека допускает конкурентную запись, поэтому очередь отправителей
	// не нужна: кадры сериализуются внутри.
	return conn.Write(ctx, websocket.MessageText, payload)
}

func (l *Loop) SendJSON(ctx context.Context, v any) error {
	payload, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("wsconn %s: marshal: %w", l.name, err)
	}
	return l.Send(ctx, payload)
}

func (l *Loop) run(ctx context.Context) {
	delay := l.cfg.WSReconnectMin

	for ctx.Err() == nil {
		start := l.clock.Now()
		err := l.session(ctx)

		if ctx.Err() != nil {
			return
		}
		l.handler.OnDisconnect(err)
		events.Publish(l.bus, ctx, events.StreamDisconnected{
			Stream: l.name, Reason: err, At: l.clock.Now(), Optional: l.optional,
		})

		// Соединение, прожившее заметное время, считаем здоровым: иначе плановая
		// ротация раз в двадцать часов разгоняла бы экспоненту задержки.
		if l.clock.Now().Sub(start) > l.healthyAfter {
			delay = l.cfg.WSReconnectMin
		}
		l.log.Warn("connection lost, reconnecting", "error", err, "delay", delay)

		if sleepErr := l.clock.Sleep(ctx, withJitter(delay)); sleepErr != nil {
			return
		}
		delay = min(delay*2, l.cfg.WSReconnectMax)
	}
}

// session проживает одно соединение: подключение, рукопожатие, чтение до ошибки.
func (l *Loop) session(parent context.Context) error {
	// Биржа принудительно рвёт связь на 24-м часе — уходим на ротацию раньше,
	// чтобы разрыв случился в контролируемый момент.
	ctx, cancel := context.WithTimeout(parent, l.cfg.WSRotateAfter)
	defer cancel()

	conn, _, err := websocket.Dial(ctx, l.url, nil)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.CloseNow()
	conn.SetReadLimit(readLimit)

	// Рукопожатие пишет напрямую в новое соединение: до его завершения канал
	// закрыт для остальных отправителей, иначе ордер мог бы уйти раньше авторизации.
	if err := l.handler.OnConnect(ctx, &boundSender{conn: conn}); err != nil {
		return fmt.Errorf("handshake: %w", err)
	}

	l.mu.Lock()
	l.active = conn
	l.mu.Unlock()
	defer func() {
		l.mu.Lock()
		l.active = nil
		l.mu.Unlock()
	}()

	events.Publish(l.bus, ctx, events.StreamConnected{Stream: l.name, At: l.clock.Now()})
	l.log.Info("connection established")

	if beater, ok := l.handler.(Heartbeater); ok {
		beatCtx, stopBeat := context.WithCancel(ctx)
		defer stopBeat()
		go l.keepAlive(beatCtx, beater, &boundSender{conn: conn})
	}

	return l.readLoop(ctx, conn)
}

// keepAlive поддерживает поток ответов на молчащем канале.
//
// Сам по себе heartbeat живость не доказывает — это делает ответ сервера,
// который сбрасывает дедлайн чтения в readLoop.
func (l *Loop) keepAlive(ctx context.Context, beater Heartbeater, s Sender) {
	ticker := l.clock.NewTicker(l.cfg.WSHeartbeatInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C():
			if err := beater.Heartbeat(ctx, s); err != nil && ctx.Err() == nil {
				l.log.Warn("failed to send heartbeat", "error", err)
			}
		}
	}
}

// readLoop читает сообщения, считая затянувшееся молчание признаком разрыва.
func (l *Loop) readLoop(ctx context.Context, conn *websocket.Conn) error {
	for {
		readCtx, cancel := context.WithTimeout(ctx, l.cfg.WSIdleTimeout)
		typ, payload, err := conn.Read(readCtx)
		cancel()

		if err != nil {
			// Отличаем молчание собеседника от штатной остановки: в первом
			// случае истёк наш дедлайн, во втором закрыт родительский контекст.
			if ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded) {
				return fmt.Errorf("no messages for longer than %s", l.cfg.WSIdleTimeout)
			}
			return fmt.Errorf("read: %w", err)
		}
		if typ != websocket.MessageText {
			continue
		}
		l.handler.OnMessage(ctx, payload, l.clock.Now())
	}
}

// boundSender пишет в конкретное соединение в обход проверки готовности.
type boundSender struct{ conn *websocket.Conn }

func (b *boundSender) Send(ctx context.Context, payload []byte) error {
	return b.conn.Write(ctx, websocket.MessageText, payload)
}

func (b *boundSender) SendJSON(ctx context.Context, v any) error {
	payload, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("wsconn: marshal: %w", err)
	}
	return b.Send(ctx, payload)
}

// withJitter добавляет случайный разброс, чтобы несколько соединений
// не переподключались синхронно.
func withJitter(d time.Duration) time.Duration {
	return d + time.Duration(rand.Int64N(int64(d/2)+1))
}

var _ ports.Runnable = (*Loop)(nil)
var _ Sender = (*Loop)(nil)
