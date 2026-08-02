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
	store   SessionStore
	http    *http.Client
	clock   ports.Clock
	log     *slog.Logger

	mu        sync.Mutex
	token     string
	expiresAt time.Time
}

// NewAuthenticator собирает аутентификатор.
//
// wallet может быть nil: при действующем кэше сессии приватный ключ кошелька
// не нужен вовсе, он требуется только чтобы выпустить новый токен.
func NewAuthenticator(
	cfg config.Exchange,
	wallet *Wallet,
	session *Session,
	store SessionStore,
	httpClient *http.Client,
	clock ports.Clock,
	log *slog.Logger,
) *Authenticator {
	return &Authenticator{
		cfg:     cfg,
		wallet:  wallet,
		session: session,
		store:   store,
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
	if a.token != "" && a.fresh(a.expiresAt) {
		return a.token, nil
	}
	if token, expiresAt, ok := a.fromCache(); ok {
		a.token, a.expiresAt = token, expiresAt
		a.log.Info("using cached session", "expires_at", expiresAt)
		return token, nil
	}

	if a.wallet == nil {
		return "", fmt.Errorf(
			"%w: no valid cached session and no wallet key, set STANDX_WALLET_PK",
			ports.ErrNoCredentials)
	}

	token, expiresAt, err := a.login(ctx)
	if err != nil {
		return "", err
	}
	a.token, a.expiresAt = token, expiresAt
	a.log.Info("obtained access token", "expires_at", expiresAt, "address", a.wallet.Address())

	a.persist(token, expiresAt)
	return token, nil
}

// Address возвращает адрес кошелька, если он известен.
func (a *Authenticator) Address() (Address, bool) {
	if a.wallet == nil {
		return "", false
	}
	return a.wallet.Address(), true
}

func (a *Authenticator) fresh(expiresAt time.Time) bool {
	return a.clock.Now().Add(a.cfg.TokenRefreshLead).Before(expiresAt)
}

// fromCache поднимает сессию, сохранённую предыдущим запуском.
func (a *Authenticator) fromCache() (string, time.Time, bool) {
	cached, err := a.store.Load()
	if err != nil {
		a.log.Warn("failed to read cached session", "error", err)
		return "", time.Time{}, false
	}

	var address string
	if a.wallet != nil {
		address = a.wallet.Address().String()
	}
	if !cached.Valid(a.clock.Now(), a.cfg.TokenRefreshLead, address, a.cfg.Chain) {
		return "", time.Time{}, false
	}

	// Токен связан с публичным ключом ed25519, предъявленным при входе.
	// Если ключ сессии не тот, подпись тела не пройдёт проверку на сервере,
	// и лучше это обнаружить здесь, чем на первом торговом запросе.
	seed, err := cached.Seed()
	if err != nil || !bytes.Equal(seed, a.session.Seed()) {
		a.log.Warn("cached session does not match the current key, re-login required")
		return "", time.Time{}, false
	}
	return cached.Token, cached.ExpiresAt, true
}

func (a *Authenticator) persist(token string, expiresAt time.Time) {
	err := a.store.Save(&CachedSession{
		Address:     a.wallet.Address().String(),
		Chain:       a.cfg.Chain,
		Token:       token,
		ExpiresAt:   expiresAt,
		SessionSeed: base64.StdEncoding.EncodeToString(a.session.Seed()),
	})
	if err != nil {
		// Не критично: без кэша следующий запуск просто выполнит вход заново.
		a.log.Warn("failed to persist session", "error", err)
	}
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
		return "", time.Time{}, fmt.Errorf("auth: sign login message: %w", err)
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
		return "", fmt.Errorf("auth: marshal prepare-signin: %w", err)
	}

	var resp prepareSignInResponse
	url := fmt.Sprintf("%s/v1/offchain/prepare-signin?chain=%s", a.cfg.AuthBaseURL, a.cfg.Chain)
	if err := a.postJSON(ctx, url, body, &resp); err != nil {
		return "", fmt.Errorf("auth: prepare-signin: %w", err)
	}
	if !resp.Success || resp.SignedData == "" {
		return "", fmt.Errorf("auth: prepare-signin rejected: %s", resp.Message)
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
		return signInClaims{}, fmt.Errorf("auth: parse signedData: %w", err)
	}

	if claims.Domain != expectedDomain {
		return signInClaims{}, fmt.Errorf("auth: unexpected domain in signing request: %q", claims.Domain)
	}
	if !strings.EqualFold(claims.Address, a.wallet.Address().String()) {
		return signInClaims{}, fmt.Errorf("auth: signing request targets a different address: %q", claims.Address)
	}
	if claims.RequestID != a.session.RequestID() {
		return signInClaims{}, fmt.Errorf("auth: signing request does not match the session key")
	}
	if claims.Message == "" {
		return signInClaims{}, fmt.Errorf("auth: empty message to sign")
	}
	if claims.Exp > 0 && a.clock.Now().After(time.Unix(claims.Exp, 0)) {
		return signInClaims{}, fmt.Errorf("auth: signing request has expired")
	}
	if !strings.Contains(claims.Message, expectedDomain) {
		return signInClaims{}, fmt.Errorf("auth: message to sign does not mention %s", expectedDomain)
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
		return "", time.Time{}, fmt.Errorf("auth: marshal login: %w", err)
	}

	var resp loginResponse
	url := fmt.Sprintf("%s/v1/offchain/login?chain=%s", a.cfg.AuthBaseURL, a.cfg.Chain)
	if err := a.postJSON(ctx, url, body, &resp); err != nil {
		return "", time.Time{}, fmt.Errorf("auth: login: %w", err)
	}
	if resp.Token == "" {
		return "", time.Time{}, fmt.Errorf("auth: login returned no token: %s", resp.Message)
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
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := a.http.Do(req)
	if err != nil {
		return fmt.Errorf("execute request: %w", err)
	}
	defer resp.Body.Close()

	payload, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d: %s", resp.StatusCode, truncate(payload, 256))
	}
	if err := json.Unmarshal(payload, out); err != nil {
		return fmt.Errorf("parse response: %w", err)
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
		return out, fmt.Errorf("want 3 JWT segments, got %d", len(parts))
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return out, fmt.Errorf("decode payload: %w", err)
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return out, fmt.Errorf("parse payload: %w", err)
	}
	return out, nil
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "…"
}
