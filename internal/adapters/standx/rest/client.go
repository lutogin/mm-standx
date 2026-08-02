// Package rest реализует HTTP-клиент StandX поверх портов приложения.
package rest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/nd/mm-standx/internal/config"
	"github.com/nd/mm-standx/internal/ports"
)

// maxAttempts — суммарное число попыток одного запроса, включая первую.
const maxAttempts = 4

// APIError — ошибка, возвращённая биржей.
type APIError struct {
	StatusCode int
	Code       int
	Message    string
	RequestID  string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("standx: http %d, code %d: %s (request_id=%s)", e.StatusCode, e.Code, e.Message, e.RequestID)
}

// ErrRateLimited возвращается при HTTP 429.
var ErrRateLimited = errors.New("standx: превышен лимит запросов")

func (e *APIError) Is(target error) bool {
	return target == ErrRateLimited && e.StatusCode == http.StatusTooManyRequests
}

// Client — клиент REST API StandX.
type Client struct {
	cfg       config.Exchange
	http      *http.Client
	tokens    ports.TokenProvider
	signer    ports.BodySigner
	limiter   ports.RateLimiter
	clock     ports.Clock
	log       *slog.Logger
	sessionID string
}

// New собирает клиент.
//
// sessionID обязан совпадать с идентификатором сессии ws-клиента: иначе результаты
// матчинга ордеров не придут ни в один поток.
func New(
	cfg config.Exchange,
	httpClient *http.Client,
	tokens ports.TokenProvider,
	signer ports.BodySigner,
	limiter ports.RateLimiter,
	clock ports.Clock,
	sessionID string,
	log *slog.Logger,
) *Client {
	return &Client{
		cfg:       cfg,
		http:      httpClient,
		tokens:    tokens,
		signer:    signer,
		limiter:   limiter,
		clock:     clock,
		sessionID: sessionID,
		log:       log.With("component", "standx.rest"),
	}
}

// request описывает один вызов API.
type request struct {
	method   string
	path     string
	query    url.Values
	body     any
	auth     bool // добавить Authorization
	signed   bool // добавить подпись тела
	priority ports.Priority
}

// do выполняет запрос с шейпингом, ретраями и разбором ответа в out.
func (c *Client) do(ctx context.Context, r request, out any) error {
	var lastErr error

	for attempt := range maxAttempts {
		if attempt > 0 {
			if err := c.clock.Sleep(ctx, backoff(attempt)); err != nil {
				return err
			}
		}
		if err := c.limiter.Acquire(ctx, r.priority); err != nil {
			return fmt.Errorf("%s %s: шейпер: %w", r.method, r.path, err)
		}

		err := c.attempt(ctx, r, out)
		if err == nil {
			return nil
		}
		lastErr = err

		if !c.retryable(r, err) {
			return err
		}
		c.log.Warn("повтор запроса",
			"method", r.method, "path", r.path, "attempt", attempt+1, "error", err)
	}
	return fmt.Errorf("%s %s: исчерпаны попытки: %w", r.method, r.path, lastErr)
}

// retryable решает, имеет ли смысл повторять запрос.
//
// Лимит запросов означает, что заявка гарантированно не дошла до матчинга —
// такой запрос безопасно повторить в любом случае. Ошибка сервера на POST
// оставляет исход неопределённым, поэтому торговые команды не переотправляются:
// цена ложного повтора — лишний ордер в стакане.
func (c *Client) retryable(r request, err error) bool {
	if errors.Is(err, ErrRateLimited) {
		return true
	}
	if r.method != http.MethodGet {
		return false
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.StatusCode >= 500
	}
	// Сетевая ошибка на чтении безопасна для повтора.
	return true
}

func (c *Client) attempt(ctx context.Context, r request, out any) error {
	var body []byte
	if r.body != nil {
		var err error
		// Сериализуем ровно один раз: подписывается тот же байтовый срез,
		// который уходит в сеть.
		body, err = json.Marshal(r.body)
		if err != nil {
			return fmt.Errorf("сериализация тела: %w", err)
		}
	}

	endpoint := c.cfg.RESTBaseURL + r.path
	if len(r.query) > 0 {
		endpoint += "?" + r.query.Encode()
	}

	req, err := http.NewRequestWithContext(ctx, r.method, endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("построение запроса: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	if r.auth {
		token, err := c.tokens.Token(ctx)
		if err != nil {
			return fmt.Errorf("получение токена: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if r.signed {
		h, err := c.signer.Sign(body)
		if err != nil {
			return fmt.Errorf("подпись тела: %w", err)
		}
		req.Header.Set("x-request-sign-version", h.Version)
		req.Header.Set("x-request-id", h.RequestID)
		req.Header.Set("x-request-timestamp", strconv.FormatInt(h.Timestamp, 10))
		req.Header.Set("x-request-signature", h.Signature)
		req.Header.Set("x-session-id", c.sessionID)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("выполнение запроса: %w", err)
	}
	defer resp.Body.Close()

	payload, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return fmt.Errorf("чтение ответа: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return parseError(resp.StatusCode, payload)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(payload, out); err != nil {
		return fmt.Errorf("разбор ответа: %w", err)
	}
	return nil
}

func parseError(status int, payload []byte) error {
	apiErr := &APIError{StatusCode: status, Message: truncate(payload, 256)}

	var envelope struct {
		Code      int    `json:"code"`
		Message   string `json:"message"`
		RequestID string `json:"request_id"`
	}
	if err := json.Unmarshal(payload, &envelope); err == nil && envelope.Message != "" {
		apiErr.Code = envelope.Code
		apiErr.Message = envelope.Message
		apiErr.RequestID = envelope.RequestID
	}
	return apiErr
}

// backoff возвращает экспоненциальную задержку со случайным разбросом,
// чтобы параллельные клиенты не повторяли запросы синхронно.
func backoff(attempt int) time.Duration {
	base := time.Duration(1<<uint(attempt-1)) * 200 * time.Millisecond
	jitter := time.Duration(rand.Int64N(int64(base / 2)))
	return base + jitter
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "…"
}
