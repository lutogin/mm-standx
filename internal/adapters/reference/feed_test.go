package reference

import (
	"context"
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nd/mm-standx/internal/adapters/wsconn"
	"github.com/nd/mm-standx/internal/config"
	"github.com/nd/mm-standx/internal/domain/market"
	"github.com/nd/mm-standx/internal/events"
)

func newHandler(t *testing.T, provider, venueSymbol string) *handler {
	t.Helper()
	return &handler{
		name:     StreamName("BTC-USD"),
		symbol:   "BTC-USD",
		provider: provider,
		venueSym: venueSymbol,
		log:      slog.New(slog.DiscardHandler),
	}
}

func TestBinanceBookTickerYieldsTheMid(t *testing.T) {
	h := newHandler(t, config.ProviderBinanceFutures, "BTCUSDT")

	mid, ok := h.mid([]byte(`{"e":"bookTicker","u":400900217,"s":"BTCUSDT",` +
		`"b":"100.00","B":"31.21","a":"102.00","A":"40.66"}`))

	require.True(t, ok)
	require.Equal(t, "101", mid.String())
}

// Спотовый кадр отличается только полями, которых мы не читаем.
func TestBinanceSpotFrameParsesTheSameWay(t *testing.T) {
	h := newHandler(t, config.ProviderBinanceSpot, "BTCUSDT")

	mid, ok := h.mid([]byte(`{"u":400900217,"s":"BTCUSDT","b":"10.0","B":"1","a":"10.2","A":"1"}`))

	require.True(t, ok)
	require.Equal(t, "10.1", mid.String())
}

// Служебные кадры площадки приходят по тому же сокету и цен не несут.
// Это штатный исход, а не ошибка: он обязан молчать, а не публиковать ноль.
func TestFramesWithoutPricesAreSkipped(t *testing.T) {
	h := newHandler(t, config.ProviderBinanceFutures, "BTCUSDT")

	for _, payload := range []string{
		`{"result":null,"id":1}`,
		`{"error":{"code":-1121,"msg":"Invalid symbol."}}`,
		`{"s":"BTCUSDT","b":"","a":""}`,
		`not json at all`,
	} {
		_, ok := h.mid([]byte(payload))
		require.False(t, ok, payload)
	}
}

// Чужой символ означает, что мы читаем не тот поток. Усреднить его молча —
// значит принимать решения об одном рынке по движению другого.
func TestForeignSymbolIsRefused(t *testing.T) {
	h := newHandler(t, config.ProviderBinanceFutures, "BTCUSDT")

	_, ok := h.mid([]byte(`{"s":"ETHUSDT","b":"100.00","a":"102.00"}`))

	require.False(t, ok)
}

// Перевёрнутый стакан означает рассинхронизацию приращений, а не цену:
// середина между ними описывала бы рынок, которого не было.
func TestCrossedBookIsRefused(t *testing.T) {
	h := newHandler(t, config.ProviderBinanceFutures, "BTCUSDT")

	_, ok := h.mid([]byte(`{"s":"BTCUSDT","b":"102.00","a":"100.00"}`))

	require.False(t, ok)
}

func TestBybitSnapshotYieldsTheMid(t *testing.T) {
	h := newHandler(t, config.ProviderBybitLinear, "BTCUSDT")

	mid, ok := h.mid([]byte(`{"topic":"tickers.BTCUSDT","type":"snapshot",` +
		`"data":{"symbol":"BTCUSDT","bid1Price":"100.00","ask1Price":"102.00","lastPrice":"101.5"}}`))

	require.True(t, ok)
	require.Equal(t, "101", mid.String())
}

// Bybit шлёт приращения и опускает вершину стакана там, где она не менялась.
// Без памяти о предыдущем кадре такое обновление теряло бы половину рынка.
func TestBybitDeltaKeepsTheUntouchedSide(t *testing.T) {
	h := newHandler(t, config.ProviderBybitLinear, "BTCUSDT")

	_, ok := h.mid([]byte(`{"topic":"tickers.BTCUSDT","type":"snapshot",` +
		`"data":{"bid1Price":"100.00","ask1Price":"102.00"}}`))
	require.True(t, ok)

	mid, ok := h.mid([]byte(`{"topic":"tickers.BTCUSDT","type":"delta","data":{"ask1Price":"104.00"}}`))

	require.True(t, ok)
	require.Equal(t, "102", mid.String(), "the bid must survive a delta that did not carry it")
}

// Пока вершина стакана не собрана, последняя сделка — лучшее, что есть:
// она шумнее середины, но движение крупнее шума видит и она.
func TestBybitFallsBackToLastPrice(t *testing.T) {
	h := newHandler(t, config.ProviderBybitLinear, "BTCUSDT")

	mid, ok := h.mid([]byte(`{"topic":"tickers.BTCUSDT","type":"delta","data":{"lastPrice":"99.5"}}`))

	require.True(t, ok)
	require.Equal(t, "99.5", mid.String())
}

func TestBybitIgnoresOtherTopics(t *testing.T) {
	h := newHandler(t, config.ProviderBybitLinear, "BTCUSDT")

	for _, payload := range []string{
		`{"success":true,"ret_msg":"subscribe","op":"subscribe"}`,
		`{"op":"pong","args":["1700000000000"]}`,
		`{"topic":"tickers.ETHUSDT","data":{"bid1Price":"1","ask1Price":"2"}}`,
	} {
		_, ok := h.mid([]byte(payload))
		require.False(t, ok, payload)
	}
}

// Вершина стакана не переживает разрыв: следующее соединение начинается
// со снимка, и склеенная из двух сессий середина описывала бы рынок,
// которого не было.
func TestDisconnectForgetsTheTopOfBook(t *testing.T) {
	h := newHandler(t, config.ProviderBybitLinear, "BTCUSDT")

	_, ok := h.mid([]byte(`{"topic":"tickers.BTCUSDT","data":{"bid1Price":"100","ask1Price":"102"}}`))
	require.True(t, ok)

	h.OnDisconnect(nil)

	_, ok = h.mid([]byte(`{"topic":"tickers.BTCUSDT","type":"delta","data":{"ask1Price":"104"}}`))
	require.False(t, ok, "a delta must not be combined with the previous connection's book")
}

func TestEndpointPerProvider(t *testing.T) {
	cases := map[string]string{
		config.ProviderBinanceFutures: "wss://fstream.binance.com/ws/btcusdt@bookTicker",
		config.ProviderBinanceSpot:    "wss://stream.binance.com:9443/ws/btcusdt@bookTicker",
		config.ProviderBybitLinear:    "wss://stream.bybit.com/v5/public/linear",
		config.ProviderBybitSpot:      "wss://stream.bybit.com/v5/public/spot",
	}
	for provider, want := range cases {
		got, err := endpoint(provider, "BTCUSDT")
		require.NoError(t, err, provider)
		require.Equal(t, want, got, provider)
	}

	_, err := endpoint("kraken", "BTCUSD")
	require.ErrorContains(t, err, "unsupported provider")

	_, err = endpoint(config.ProviderBinanceFutures, "  ")
	require.ErrorContains(t, err, "symbol is required")
}

// Bybit молчит, пока не подписан. Binance отдаёт поток по адресу подключения
// и на подписку отвечает ошибкой.
func TestOnConnectSubscribesOnlyWhereRequired(t *testing.T) {
	bybit := newHandler(t, config.ProviderBybitLinear, "BTCUSDT")
	sent := &recordingSender{}
	require.NoError(t, bybit.OnConnect(context.Background(), sent))
	require.Len(t, sent.frames, 1)
	require.JSONEq(t, `{"op":"subscribe","args":["tickers.BTCUSDT"]}`, string(sent.frames[0]))

	binance := newHandler(t, config.ProviderBinanceFutures, "BTCUSDT")
	quiet := &recordingSender{}
	require.NoError(t, binance.OnConnect(context.Background(), quiet))
	require.Empty(t, quiet.frames)
}

// Потеря ориентира возвращает нас ровно к той картине, что была до него.
// Съём котировок в этом случае платил бы присутствием за чужой сбой.
func TestFeedDeclaresItselfOptional(t *testing.T) {
	require.True(t, newHandler(t, config.ProviderBinanceFutures, "BTCUSDT").Optional())
}

func TestOnMessagePublishesTheReference(t *testing.T) {
	bus := events.New(func(string, any) { t.Fatal("handler panicked") })
	var got []market.ReferencePrice
	events.Subscribe(bus, "test", func(_ context.Context, ev events.ReferencePriceUpdated) {
		got = append(got, ev.Reference)
	})

	h := newHandler(t, config.ProviderBinanceFutures, "BTCUSDT")
	h.bus = bus
	h.recorder = &countingRecorder{}

	at := time.Unix(1_700_000_000, 0).UTC()
	h.OnMessage(context.Background(), []byte(`{"s":"BTCUSDT","b":"100.00","a":"102.00"}`), at)
	h.OnMessage(context.Background(), []byte(`{"result":null,"id":1}`), at)

	require.Len(t, got, 1, "only frames carrying a price become events")
	require.Equal(t, market.Symbol("BTC-USD"), got[0].Symbol)
	require.Equal(t, config.ProviderBinanceFutures, got[0].Venue)
	require.Equal(t, "101", got[0].Price.String())
	require.Equal(t, at, got[0].Received)

	// Запись ведётся по всем кадрам, включая служебные: анализ разбирает
	// поток офлайн, и решать за него, что там лишнее, здесь нельзя.
	require.Equal(t, 2, h.recorder.(*countingRecorder).calls)
}

type recordingSender struct{ frames [][]byte }

func (s *recordingSender) Send(_ context.Context, payload []byte) error {
	s.frames = append(s.frames, payload)
	return nil
}

func (s *recordingSender) SendJSON(ctx context.Context, v any) error {
	payload, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return s.Send(ctx, payload)
}

type countingRecorder struct{ calls int }

func (r *countingRecorder) Record(string, time.Time, []byte) error { r.calls++; return nil }
func (r *countingRecorder) Close() error                           { return nil }

var _ wsconn.Sender = (*recordingSender)(nil)
