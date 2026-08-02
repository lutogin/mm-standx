// Package di объявляет модули внедрения зависимостей.
//
// Здесь только связывание: реализации из adapters подставляются под интерфейсы
// из ports. Логики быть не должно — исключение составляет выбор реализации
// по режиму работы, и он вынесен в явные конструкторы с комментариями.
package di

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"go.uber.org/fx"
	"go.uber.org/fx/fxevent"

	"github.com/nd/mm-standx/internal/adapters/clock"
	"github.com/nd/mm-standx/internal/adapters/dryrun"
	"github.com/nd/mm-standx/internal/adapters/ratelimit"
	"github.com/nd/mm-standx/internal/adapters/recorder"
	"github.com/nd/mm-standx/internal/adapters/standx/auth"
	"github.com/nd/mm-standx/internal/adapters/standx/gateway"
	"github.com/nd/mm-standx/internal/adapters/standx/rest"
	"github.com/nd/mm-standx/internal/adapters/standx/wsapi"
	"github.com/nd/mm-standx/internal/config"
	"github.com/nd/mm-standx/internal/events"
	"github.com/nd/mm-standx/internal/ports"
)

// SessionID — идентификатор сессии, общий для REST и ws-клиента.
//
// Отдельный тип, а не голая строка: fx различает зависимости по типу, и голый
// string немедленно столкнулся бы с любой другой строковой зависимостью.
type SessionID string

// Core — инфраструктура, не зависящая от биржи.
var Core = fx.Module("core",
	fx.Provide(
		provideLogger,
		provideClock,
		provideEventBus,
		provideRateLimiter,
		provideHTTPClient,
		provideSessionID,
		provideRecorder,
	),
	fx.WithLogger(provideFxLogger),
)

// provideFxLogger приглушает служебный вывод контейнера.
//
// fx по умолчанию печатает весь граф зависимостей со стектрейсами, что полностью
// топит прикладные сообщения. Граф полезен при отладке сборки, поэтому он
// доступен на уровне debug и молчит на остальных.
func provideFxLogger(cfg *config.Config, log *slog.Logger) fxevent.Logger {
	if strings.EqualFold(cfg.Runtime.LogLevel, "debug") {
		return &fxevent.SlogLogger{Logger: log.With("component", "fx")}
	}
	return fxevent.NopLogger
}

// StandX — клиенты биржи.
var StandX = fx.Module("standx",
	fx.Provide(
		provideWallet,
		provideSession,
		provideTokenProvider,
		provideBodySigner,
		provideRESTClient,
		provideExchangeReader,
		provideWSAPIClient,
		provideOrderGateway,
	),
)

func provideLogger(cfg *config.Config) *slog.Logger {
	level := slog.LevelInfo
	switch strings.ToLower(cfg.Runtime.LogLevel) {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}

	opts := &slog.HandlerOptions{Level: level}
	var handler slog.Handler = slog.NewTextHandler(os.Stdout, opts)
	if cfg.Runtime.LogFormat == "json" {
		handler = slog.NewJSONHandler(os.Stdout, opts)
	}
	return slog.New(handler).With("mode", string(cfg.Runtime.Mode))
}

func provideClock() ports.Clock { return clock.New() }

// provideEventBus связывает панику обработчика события с аварийной остановкой.
//
// Паника означает, что состояние подписчика неконсистентно. Продолжать
// торговать в такой ситуации опаснее, чем остановиться.
func provideEventBus(log *slog.Logger, clk ports.Clock) *events.Bus {
	var bus *events.Bus
	bus = events.New(func(topic string, recovered any) {
		log.Error("паника в обработчике события — аварийная остановка",
			"topic", topic, "recovered", recovered)
		events.Publish(bus, context.Background(), events.KillSwitchTripped{
			Reason: fmt.Sprintf("паника обработчика %s: %v", topic, recovered),
			At:     clk.Now(),
		})
	})
	return bus
}

func provideRateLimiter(cfg *config.Config, clk ports.Clock) ports.RateLimiter {
	return ratelimit.New(cfg.RateLimit, clk)
}

// provideHTTPClient настраивает транспорт под долгоживущий процесс с частыми
// мелкими запросами: соединения переиспользуются, чтобы не платить рукопожатием
// TLS за каждый ордер.
func provideHTTPClient(cfg *config.Config) *http.Client {
	transport := &http.Transport{
		DialContext:         (&net.Dialer{Timeout: 3 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		MaxIdleConns:        64,
		MaxIdleConnsPerHost: 32,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 5 * time.Second,
		ForceAttemptHTTP2:   true,
	}
	return &http.Client{Transport: transport, Timeout: cfg.Exchange.RequestTimeout}
}

func provideSessionID() (SessionID, error) {
	id, err := auth.NewRequestID()
	if err != nil {
		return "", fmt.Errorf("генерация session_id: %w", err)
	}
	return SessionID(id), nil
}

// provideRecorder включает запись потока, если задан каталог.
func provideRecorder(cfg *config.Config, clk ports.Clock, lc fx.Lifecycle) (ports.Recorder, error) {
	if cfg.Runtime.RecordingDir == "" {
		return recorder.Nop{}, nil
	}
	rec, err := recorder.NewFile(cfg.Runtime.RecordingDir, clk)
	if err != nil {
		return nil, err
	}
	lc.Append(fx.Hook{OnStop: func(context.Context) error { return rec.Close() }})
	return rec, nil
}

// provideWallet возвращает nil, если приватный ключ не задан.
//
// Это штатный режим наблюдения: публичные потоки работают без учётных данных,
// что позволяет собирать рыночные данные и гонять dry-run до открытия счёта.
func provideWallet(cfg *config.Config, log *slog.Logger) (*auth.Wallet, error) {
	if cfg.Exchange.WalletPrivateKey.IsEmpty() {
		log.Warn("STANDX_WALLET_PK не задан — доступны только публичные данные")
		return nil, nil
	}
	return auth.NewWallet(cfg.Exchange.WalletPrivateKey)
}

func provideSession(clk ports.Clock) (*auth.Session, error) {
	return auth.NewSession(func() int64 { return clk.Now().UnixMilli() })
}

func provideBodySigner(session *auth.Session) ports.BodySigner { return session }

func provideTokenProvider(
	cfg *config.Config,
	wallet *auth.Wallet,
	session *auth.Session,
	httpClient *http.Client,
	clk ports.Clock,
	log *slog.Logger,
) ports.TokenProvider {
	if wallet == nil {
		return unavailableTokens{}
	}
	return auth.NewAuthenticator(cfg.Exchange, wallet, session, httpClient, clk, log)
}

// unavailableTokens подставляется вместо аутентификатора, когда кошелёк не задан.
// Любая попытка обратиться к приватному эндпойнту завершится понятной ошибкой,
// а не паникой на nil.
type unavailableTokens struct{}

func (unavailableTokens) Token(context.Context) (string, error) {
	return "", errors.New("учётные данные не заданы: установите STANDX_WALLET_PK")
}

func provideRESTClient(
	cfg *config.Config,
	httpClient *http.Client,
	tokens ports.TokenProvider,
	signer ports.BodySigner,
	limiter ports.RateLimiter,
	clk ports.Clock,
	sessionID SessionID,
	log *slog.Logger,
) *rest.Client {
	return rest.New(cfg.Exchange, httpClient, tokens, signer, limiter, clk, string(sessionID), log)
}

func provideExchangeReader(c *rest.Client) ports.ExchangeReader { return c }

// provideWSAPIClient создаёт торговый WebSocket-канал.
//
// Возвращает nil без учётных данных или вне режима live: держать
// авторизованное соединение, которым некому пользоваться, незачем.
func provideWSAPIClient(
	cfg *config.Config,
	wallet *auth.Wallet,
	sessionID SessionID,
	tokens ports.TokenProvider,
	signer ports.BodySigner,
	limiter ports.RateLimiter,
	bus *events.Bus,
	clk ports.Clock,
	log *slog.Logger,
) *wsapi.Client {
	if wallet == nil || !cfg.IsLive() {
		return nil
	}
	return wsapi.New(cfg.Exchange, cfg.Health, string(sessionID), tokens, signer, limiter, bus, clk, log)
}

// provideOrderGateway выбирает шлюз по режиму работы.
//
// В dry_run в граф зависимостей попадает заглушка, а не боевой клиент: путь
// отправки настоящего ордера физически отсутствует, и забыть проверку режима
// в каком-нибудь новом месте невозможно.
func provideOrderGateway(
	cfg *config.Config,
	ws *wsapi.Client,
	c *rest.Client,
	log *slog.Logger,
) ports.OrderGateway {
	if !cfg.IsLive() {
		return dryrun.New(log)
	}
	log.Warn("режим live: ордера уходят на биржу")

	// Присваивание через промежуточную переменную не косметика: nil-указатель,
	// уложенный в интерфейс напрямую, дал бы интерфейс, не равный nil,
	// и проверка готовности канала свалилась бы в разыменование.
	var fast gateway.FastGateway
	if ws != nil {
		fast = ws
	}
	return gateway.New(fast, c, log)
}
