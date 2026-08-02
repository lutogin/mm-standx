package auth

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/nd/mm-standx/internal/config"
	"github.com/nd/mm-standx/internal/ports"
)

// expectedDomain — домен, от имени которого StandX просит подписать сообщение.
const expectedDomain = "standx.com"

// Authenticator получает и обновляет JWT доступа к торговому API.
//
// Реализует ports.TokenProvider.
type Authenticator struct {
	cfg     config.Exchange
	wallet  *Wallet
	session *Session
	http    *http.Client
	clock   ports.Clock
	log     *slog.Logger

	mu        sync.Mutex
	token     string
	expiresAt time.Time
}

// NewAuthenticator собирает аутентификатор. Кошелёк обязателен только для режима
// live: в dry_run бот работает с публичными данными.
func NewAuthenticator(
	cfg config.Exchange,
	wallet *Wallet,
	session *Session,
	httpClient *http.Client,
	clock ports.Clock,
	log *slog.Logger,
) *Authenticator {
	return &Authenticator{
		cfg:     cfg,
		wallet:  wallet,
		session: session,
		http:    httpClient,
		clock:   clock,
		log:     log.With("component", "auth"),
	}
}

// Token возвращает действующий JWT, при необходимости выполняя вход заново.
func (a *Authenticator) Token(ctx context.Context) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	// Обновляемся заранее, чтобы запрос не улетел с токеном, протухшим в полёте.
	if a.token != "" && a.clock.Now().Add(a.cfg.TokenRefreshLead).Before(a.expiresAt) {
		return a.token, nil
	}

	token, expiresAt, err := a.login(ctx)
	if err != nil {
		return "", err
	}
	a.token, a.expiresAt = token, expiresAt
	a.log.Info("получен токен доступа", "expires_at", expiresAt, "address", a.wallet.Address())
	return token, nil
}

func (a *Authenticator) login(ctx context.Context) (string, time.Time, error) {
	signedData, err := a.prepareSignIn(ctx)
	if err != nil {
		return "", time.Time{}, err
	}

	claims, err := a.verifySignInRequest(signedData)
	if err != nil {
		return "", time.Time{}, err
	}

	signature, err := a.wallet.SignPersonal(claims.Message)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("auth: подпись сообщения входа: %w", err)
	}

	return a.exchangeForToken(ctx, signedData, signature)
}

type prepareSignInResponse struct {
	Success    bool   `json:"success"`
	SignedData string `json:"signedData"`
	Message    string `json:"message"`
}

func (a *Authenticator) prepareSignIn(ctx context.Context) (string, error) {
	body, err := json.Marshal(map[string]string{
		"address":   a.wallet.Address().String(),
		"requestId": a.session.RequestID(),
	})
	if err != nil {
		return "", fmt.Errorf("auth: сериализация prepare-signin: %w", err)
	}

	var resp prepareSignInResponse
	url := fmt.Sprintf("%s/v1/offchain/prepare-signin?chain=%s", a.cfg.AuthBaseURL, a.cfg.Chain)
	if err := a.postJSON(ctx, url, body, &resp); err != nil {
		return "", fmt.Errorf("auth: prepare-signin: %w", err)
	}
	if !resp.Success || resp.SignedData == "" {
		return "", fmt.Errorf("auth: prepare-signin отклонён: %s", resp.Message)
	}
	return resp.SignedData, nil
}

// signInClaims — полезная нагрузка signedData.
type signInClaims struct {
	Domain    string `json:"domain"`
	Address   string `json:"address"`
	RequestID string `json:"requestId"`
	Message   string `json:"message"`
	Exp       int64  `json:"exp"`
}

// verifySignInRequest проверяет, что нам предлагают подписать именно то, что мы
// запрашивали.
//
// Подписывать кошельком произвольный текст, пришедший от сервера, нельзя: подпись
// EIP-191 может быть использована как доказательство намерения в другом контексте.
// Сверяем домен, собственный адрес, идентификатор сессии и срок жизни — этого
// достаточно, чтобы подменённое сообщение не прошло.
func (a *Authenticator) verifySignInRequest(signedData string) (signInClaims, error) {
	claims, err := decodeJWTPayload[signInClaims](signedData)
	if err != nil {
		return signInClaims{}, fmt.Errorf("auth: разбор signedData: %w", err)
	}

	if claims.Domain != expectedDomain {
		return signInClaims{}, fmt.Errorf("auth: неожиданный домен в запросе подписи: %q", claims.Domain)
	}
	if !strings.EqualFold(claims.Address, a.wallet.Address().String()) {
		return signInClaims{}, fmt.Errorf("auth: запрос подписи адресован другому адресу: %q", claims.Address)
	}
	if claims.RequestID != a.session.RequestID() {
		return signInClaims{}, fmt.Errorf("auth: запрос подписи не соответствует ключу сессии")
	}
	if claims.Message == "" {
		return signInClaims{}, fmt.Errorf("auth: пустое сообщение для подписи")
	}
	if claims.Exp > 0 && a.clock.Now().After(time.Unix(claims.Exp, 0)) {
		return signInClaims{}, fmt.Errorf("auth: запрос подписи просрочен")
	}
	if !strings.Contains(claims.Message, expectedDomain) {
		return signInClaims{}, fmt.Errorf("auth: подписываемое сообщение не упоминает %s", expectedDomain)
	}
	return claims, nil
}

type loginResponse struct {
	Token   string `json:"token"`
	Address string `json:"address"`
	Chain   string `json:"chain"`
	Message string `json:"message"`
}

func (a *Authenticator) exchangeForToken(ctx context.Context, signedData, signature string) (string, time.Time, error) {
	body, err := json.Marshal(map[string]any{
		"signature":      signature,
		"signedData":     signedData,
		"expiresSeconds": int64(a.cfg.TokenTTL.Seconds()),
	})
	if err != nil {
		return "", time.Time{}, fmt.Errorf("auth: сериализация login: %w", err)
	}

	var resp loginResponse
	url := fmt.Sprintf("%s/v1/offchain/login?chain=%s", a.cfg.AuthBaseURL, a.cfg.Chain)
	if err := a.postJSON(ctx, url, body, &resp); err != nil {
		return "", time.Time{}, fmt.Errorf("auth: login: %w", err)
	}
	if resp.Token == "" {
		return "", time.Time{}, fmt.Errorf("auth: login не вернул токен: %s", resp.Message)
	}

	// Срок берём из самого токена: сервер вправе выдать меньше запрошенного.
	expiresAt := a.clock.Now().Add(a.cfg.TokenTTL)
	if claims, err := decodeJWTPayload[struct {
		Exp int64 `json:"exp"`
	}](resp.Token); err == nil && claims.Exp > 0 {
		expiresAt = time.Unix(claims.Exp, 0)
	}
	return resp.Token, expiresAt, nil
}

func (a *Authenticator) postJSON(ctx context.Context, url string, body []byte, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("построение запроса: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := a.http.Do(req)
	if err != nil {
		return fmt.Errorf("выполнение запроса: %w", err)
	}
	defer resp.Body.Close()

	payload, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("чтение ответа: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("статус %d: %s", resp.StatusCode, truncate(payload, 256))
	}
	if err := json.Unmarshal(payload, out); err != nil {
		return fmt.Errorf("разбор ответа: %w", err)
	}
	return nil
}

// decodeJWTPayload читает среднюю секцию JWT без проверки подписи.
//
// Подпись не проверяется намеренно: содержимое либо валидируется по смыслу
// (verifySignInRequest), либо используется только как подсказка о сроке жизни.
func decodeJWTPayload[T any](token string) (T, error) {
	var out T
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return out, fmt.Errorf("ожидалось 3 секции JWT, получено %d", len(parts))
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return out, fmt.Errorf("декодирование payload: %w", err)
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return out, fmt.Errorf("разбор payload: %w", err)
	}
	return out, nil
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "…"
}
