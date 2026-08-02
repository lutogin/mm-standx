// Package wsapi отправляет торговые команды по WebSocket.
//
// Это горячий путь размещения и отмены. В сравнении с REST даже при живом
// keep-alive он выигрывает трижды: кадр не несёт HTTP-заголовков (а Bearer-токен
// StandX сам по себе занимает сотни байт), под всплеском не возникает риска
// открытия нового TCP-соединения с рукопожатием TLS, и команды уходят конвейером
// без ожидания ответа на предыдущую.
//
// Пакетной отмены протокол не предусматривает — только order:new и order:cancel.
// Выбор между конвейером одиночных отмен и пакетным REST делает gateway.Router.
package wsapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/nd/mm-standx/internal/adapters/standx/auth"
	"github.com/nd/mm-standx/internal/adapters/standx/dto"
	"github.com/nd/mm-standx/internal/adapters/wsconn"
	"github.com/nd/mm-standx/internal/config"
	"github.com/nd/mm-standx/internal/domain/trading"
	"github.com/nd/mm-standx/internal/events"
	"github.com/nd/mm-standx/internal/ports"
)

// Методы протокола.
const (
	methodAuthLogin   = "auth:login"
	methodOrderNew    = "order:new"
	methodOrderCancel = "order:cancel"
)

// pendingTTL — сколько заявка остаётся в таблице ожидания после ответа.
//
// Биржа отвечает дважды: сначала accepted от шлюза, затем итог матчинга.
// Запись переживает первый ответ, чтобы отклонение ALO нашло, к какому
// клиентскому идентификатору относится.
const pendingTTL = 30 * time.Second

var (
	// ErrNotSent означает, что команда заведомо не покинула процесс.
	//
	// Только при этой ошибке безопасно повторить неидемпотентную операцию
	// по запасному каналу: во всех прочих случаях заявка могла дойти до биржи.
	ErrNotSent = errors.New("wsapi: command was not sent")

	// ErrAckTimeout означает, что команда ушла, но подтверждения не было.
	//
	// Исход неопределён. Повторять размещение нельзя — в стакане может
	// оказаться два ордера; расхождение устраняет сверка.
	ErrAckTimeout = errors.New("wsapi: exchange did not acknowledge the command")
)

// request — конверт запроса торгового канала.
type request struct {
	SessionID string            `json:"session_id"`
	RequestID string            `json:"request_id"`
	Method    string            `json:"method"`
	Header    map[string]string `json:"header,omitempty"`
	// Params — JSON-строка, а не вложенный объект: так требует протокол.
	Params string `json:"params"`
}

// response — ответ торгового канала.
type response struct {
	Code      int    `json:"code"`
	Status    string `json:"status"`
	Message   string `json:"message"`
	RequestID string `json:"request_id"`
}

// isTerminal сообщает, что ответ несёт итог, а не подтверждение приёма шлюзом.
func (r response) isTerminal() bool { return r.Status != "accepted" }

type pending struct {
	clOrdID trading.ClOrdID
	ch      chan response
	expires time.Time
}

// Client — торговый WebSocket-клиент.
type Client struct {
	loop       *wsconn.Loop
	sessionID  string
	tokens     ports.TokenProvider
	signer     ports.BodySigner
	limiter    ports.RateLimiter
	clock      ports.Clock
	bus        *events.Bus
	ackTimeout time.Duration
	log        *slog.Logger

	mu      sync.Mutex
	pending map[string]*pending
}

// New собирает торговый клиент вместе с его соединением.
func New(
	cfg config.Exchange,
	health config.Health,
	sessionID string,
	tokens ports.TokenProvider,
	signer ports.BodySigner,
	limiter ports.RateLimiter,
	bus *events.Bus,
	clock ports.Clock,
	log *slog.Logger,
) *Client {
	c := &Client{
		sessionID:  sessionID,
		tokens:     tokens,
		signer:     signer,
		limiter:    limiter,
		clock:      clock,
		bus:        bus,
		ackTimeout: health.AckTimeout,
		log:        log.With("component", "standx.wsapi"),
		pending:    make(map[string]*pending),
	}
	c.loop = wsconn.New("order-api", cfg.WSAPIURL, cfg, c, bus, clock, log)
	return c
}

// Runnable возвращает соединение для регистрации в жизненном цикле приложения.
func (c *Client) Runnable() ports.Runnable { return c.loop }

// Connected сообщает, готов ли канал принимать команды.
func (c *Client) Connected() bool { return c.loop.Connected() }

// --- Прикладной протокол соединения ---

// OnConnect авторизует канал. До завершения авторизации соединение закрыто
// для остальных отправителей, поэтому ордер не может уйти раньше логина.
func (c *Client) OnConnect(ctx context.Context, s wsconn.Sender) error {
	return c.login(ctx, s)
}

// Heartbeat повторяет авторизацию, чтобы поддерживать поток ответов.
//
// По торговому каналу данные сами по себе не идут: между ордерами он молчит
// сколько угодно долго. Пинговать нельзя — сервер StandX на клиентские пинги
// не отвечает, поэтому живость доказывается прикладным запросом, ответ
// на который сбрасывает дедлайн молчания. Повторный логин выбран как
// идемпотентная операция без побочных эффектов.
func (c *Client) Heartbeat(ctx context.Context, s wsconn.Sender) error {
	return c.login(ctx, s)
}

func (c *Client) login(ctx context.Context, s wsconn.Sender) error {
	token, err := c.tokens.Token(ctx)
	if err != nil {
		return fmt.Errorf("obtain token: %w", err)
	}
	params, err := json.Marshal(map[string]string{"token": token})
	if err != nil {
		return fmt.Errorf("marshal login params: %w", err)
	}
	requestID, err := auth.NewRequestID()
	if err != nil {
		return err
	}
	return s.SendJSON(ctx, request{
		SessionID: c.sessionID,
		RequestID: requestID,
		Method:    methodAuthLogin,
		Params:    string(params),
	})
}

// OnMessage маршрутизирует ответ ожидающему запросу и публикует отклонения.
func (c *Client) OnMessage(ctx context.Context, payload []byte, received time.Time) {
	var resp response
	if err := json.Unmarshal(payload, &resp); err != nil {
		c.log.Error("failed to parse response", "error", err)
		return
	}

	c.mu.Lock()
	p := c.pending[resp.RequestID]
	c.mu.Unlock()

	if p == nil {
		// Ответ на авторизацию либо запоздавший итог по уже забытой заявке.
		if resp.Code != 0 {
			c.log.Error("trading channel error", "code", resp.Code, "message", resp.Message)
		}
		return
	}

	// Отклонение публикуем здесь, а не в ожидающем вызове: итог матчинга
	// приходит вторым сообщением, когда вызывающий уже получил управление.
	if resp.Code != 0 {
		c.log.Warn("request rejected", "cl_ord_id", p.clOrdID, "code", resp.Code, "message", resp.Message)
		events.Publish(c.bus, ctx, events.OrderRejected{
			ClOrdID: p.clOrdID,
			Code:    resp.Code,
			Reason:  resp.Message,
			At:      received,
		})
	}

	// Отправка неблокирующая: ожидающий мог уйти по таймауту.
	select {
	case p.ch <- resp:
	default:
	}

	if resp.isTerminal() {
		c.forget(resp.RequestID)
	}
}

// OnDisconnect освобождает ожидающих: ответы по этому соединению уже не придут.
func (c *Client) OnDisconnect(err error) {
	c.mu.Lock()
	stranded := c.pending
	c.pending = make(map[string]*pending)
	c.mu.Unlock()

	if len(stranded) == 0 {
		return
	}
	c.log.Warn("trading channel dropped with in-flight requests",
		"count", len(stranded), "error", err)

	for _, p := range stranded {
		select {
		case p.ch <- response{Code: -1, Message: "connection lost"}:
		default:
		}
	}
}

// --- Шлюз ордеров ---

// Place отправляет заявку на размещение ордера.
func (c *Client) Place(ctx context.Context, req ports.PlaceRequest) error {
	body := dto.NewOrderRequest{
		Symbol:      req.Symbol.String(),
		Side:        req.Side.String(),
		OrderType:   string(req.Type),
		Qty:         req.Qty.String(),
		TimeInForce: string(req.TimeInForce),
		ReduceOnly:  req.ReduceOnly,
		ClOrdID:     req.ClOrdID.String(),
		MarginMode:  string(req.MarginMode),
		Leverage:    req.Leverage,
	}
	if req.Type == trading.Limit {
		body.Price = req.Price.String()
	}
	return c.call(ctx, methodOrderNew, body, req.ClOrdID, req.Priority)
}

// Cancel отменяет ордер по клиентскому идентификатору.
func (c *Client) Cancel(ctx context.Context, id trading.ClOrdID, p ports.Priority) error {
	return c.call(ctx, methodOrderCancel, dto.CancelOrderRequest{ClOrdID: id.String()}, id, p)
}

// CancelBatch отменяет пачку ордеров конвейером одиночных команд.
//
// Все кадры уходят до ожидания ответов, поэтому суммарная задержка близка
// к одному обороту сети независимо от размера пачки. Платой служат N кредитов
// шейпера вместо одного у пакетного REST-эндпойнта — выбор между ними делает
// gateway.Router.
func (c *Client) CancelBatch(ctx context.Context, ids []trading.ClOrdID, p ports.Priority) error {
	var errs []error
	for _, id := range ids {
		if err := c.Cancel(ctx, id, p); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// call отправляет команду и ждёт подтверждения приёма.
//
// Ожидается только первый ответ: итог матчинга приходит асинхронно событием,
// и держать вызывающего до него значило бы блокировать контур котирования.
func (c *Client) call(
	ctx context.Context,
	method string,
	params any,
	clOrdID trading.ClOrdID,
	priority ports.Priority,
) error {
	// Считает ли биржа сообщения торгового канала в тот же бюджет кредитов,
	// что и REST, документация не уточняет. Считаем, что да: переоценка расхода
	// стоит доли миллисекунды, недооценка — отказа с кодом 429.
	if err := c.limiter.Acquire(ctx, priority); err != nil {
		return fmt.Errorf("%s %s: rate shaper: %w: %w", method, clOrdID, ErrNotSent, err)
	}

	payload, err := json.Marshal(params)
	if err != nil {
		return fmt.Errorf("%s %s: marshal params: %w: %w", method, clOrdID, ErrNotSent, err)
	}
	// Подписывается ровно та строка params, которая уйдёт в сеть.
	headers, err := c.signer.Sign(payload)
	if err != nil {
		return fmt.Errorf("%s %s: sign: %w: %w", method, clOrdID, ErrNotSent, err)
	}
	requestID, err := auth.NewRequestID()
	if err != nil {
		return fmt.Errorf("%s %s: %w: %w", method, clOrdID, ErrNotSent, err)
	}

	waiter := c.remember(requestID, clOrdID)
	defer c.scheduleForget(requestID)

	err = c.loop.SendJSON(ctx, request{
		SessionID: c.sessionID,
		RequestID: requestID,
		Method:    method,
		Header: map[string]string{
			// Версию схемы документация торгового канала не упоминает, но REST
			// её требует, и лишнее поле безопаснее отсутствующего.
			"x-request-sign-version": headers.Version,
			"x-request-id":           headers.RequestID,
			"x-request-timestamp":    fmt.Sprint(headers.Timestamp),
			"x-request-signature":    headers.Signature,
		},
		Params: string(payload),
	})
	if err != nil {
		c.forget(requestID)
		// Ошибка записи в сокет означает, что кадр не ушёл: библиотека
		// не рапортует успех при частичной отправке.
		return fmt.Errorf("%s %s: send: %w: %w", method, clOrdID, ErrNotSent, err)
	}

	select {
	case resp := <-waiter:
		if resp.Code != 0 {
			return fmt.Errorf("%s %s: exchange rejected the request: code %d: %s",
				method, clOrdID, resp.Code, resp.Message)
		}
		return nil
	case <-c.clock.After(c.ackTimeout):
		return fmt.Errorf("%s %s: %w within %s", method, clOrdID, ErrAckTimeout, c.ackTimeout)
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *Client) remember(requestID string, clOrdID trading.ClOrdID) <-chan response {
	// Буфер на два сообщения: подтверждение приёма и итог матчинга.
	ch := make(chan response, 2)

	c.mu.Lock()
	c.pending[requestID] = &pending{
		clOrdID: clOrdID,
		ch:      ch,
		expires: c.clock.Now().Add(pendingTTL),
	}
	c.mu.Unlock()
	return ch
}

func (c *Client) forget(requestID string) {
	c.mu.Lock()
	delete(c.pending, requestID)
	c.mu.Unlock()
}

// scheduleForget убирает запись, если итог так и не пришёл: иначе таблица
// ожидания росла бы без границы на каждом потерянном ответе.
func (c *Client) scheduleForget(requestID string) {
	time.AfterFunc(pendingTTL, func() { c.forget(requestID) })
}

var (
	_ wsconn.Handler     = (*Client)(nil)
	_ wsconn.Heartbeater = (*Client)(nil)
	_ ports.OrderGateway = (*Client)(nil)
)
