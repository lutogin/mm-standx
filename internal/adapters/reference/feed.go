// Package reference subscribes to an external venue whose price leads the
// exchange's own mark.
//
// The exchange publishes a mark about once every three seconds. What reaches a
// resting quote inside that window is, in the usual case, arbitrage against a
// move that already happened on a faster venue: the taker sees the move there
// and hits our stale price here. Watching that venue directly is therefore
// watching the cause rather than the effect, which is the whole point of this
// package — the exchange's own book moves at the same instant as the order that
// fills us, and that is too late to be a warning.
//
// The feed is advisory by construction. Its loss returns us to exactly the
// picture we had before it existed, so the connection reports itself as
// optional and no subscriber is allowed to stop quoting over it.
package reference

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/shopspring/decimal"

	"github.com/nd/mm-standx/internal/adapters/wsconn"
	"github.com/nd/mm-standx/internal/config"
	"github.com/nd/mm-standx/internal/domain/market"
	"github.com/nd/mm-standx/internal/domain/money"
	"github.com/nd/mm-standx/internal/events"
	"github.com/nd/mm-standx/internal/ports"
)

// New собирает поток внешнего ориентира для одного инструмента биржи.
//
// symbol — инструмент StandX, cfg.Symbol — его имя на внешней площадке.
// Соответствие подбирается человеком и не проверяется здесь: убедиться, что
// XAUUSDT действительно отслеживает то же золото, программе неоткуда.
func New(
	symbol market.Symbol,
	ref config.Reference,
	exchange config.Exchange,
	bus *events.Bus,
	clock ports.Clock,
	recorder ports.Recorder,
	log *slog.Logger,
) (*wsconn.Loop, error) {
	url, err := endpoint(ref.Provider, ref.Symbol)
	if err != nil {
		return nil, err
	}

	name := StreamName(symbol)
	h := &handler{
		name:     name,
		symbol:   symbol,
		provider: ref.Provider,
		venueSym: strings.ToUpper(strings.TrimSpace(ref.Symbol)),
		bus:      bus,
		clock:    clock,
		recorder: recorder,
		log:      log.With("component", "reference", "stream", name, "venue", ref.Provider),
	}

	// Bybit молчит, пока не подписан, и требует прикладного ping: без него
	// соединение живо, а данных нет. Binance отдаёт поток сразу и на прикладной
	// ping отвечает ошибкой, поэтому heartbeat реализован только там, где нужен.
	var wsh wsconn.Handler = h
	if isBybit(ref.Provider) {
		wsh = &bybitHandler{handler: h}
	}
	return wsconn.New(name, url, exchange, wsh, bus, clock, log), nil
}

// StreamName — имя потока в логах и в записи.
//
// Несёт инструмент биржи, а не имя символа на площадке: запись разбирается
// офлайн, и восстанавливать соответствие по конвенциям чужих площадок значило бы
// хранить его в двух местах.
func StreamName(symbol market.Symbol) string { return "reference[" + symbol.String() + "]" }

// endpoint возвращает адрес потока лучших котировок площадки.
//
// Адреса зашиты: это не настройка, а часть протокола конкретной площадки,
// и вынести их в конфигурацию значило бы разрешить подключить binance_futures
// к чему угодно.
func endpoint(provider, symbol string) (string, error) {
	venue := strings.ToLower(strings.TrimSpace(symbol))
	if venue == "" {
		return "", fmt.Errorf("reference: symbol is required for provider %q", provider)
	}

	switch provider {
	case config.ProviderBinanceFutures:
		return "wss://fstream.binance.com/ws/" + venue + "@bookTicker", nil
	case config.ProviderBinanceSpot:
		return "wss://stream.binance.com:9443/ws/" + venue + "@bookTicker", nil
	case config.ProviderBybitLinear:
		return "wss://stream.bybit.com/v5/public/linear", nil
	case config.ProviderBybitSpot:
		return "wss://stream.bybit.com/v5/public/spot", nil
	default:
		return "", fmt.Errorf("reference: unsupported provider %q, known: %s",
			provider, strings.Join(config.ReferenceProviders(), ", "))
	}
}

func isBybit(provider string) bool {
	return provider == config.ProviderBybitLinear || provider == config.ProviderBybitSpot
}

// handler разбирает прикладной протокол площадки.
//
// Всё состояние принадлежит одной горутине цикла соединения: OnConnect,
// OnMessage и OnDisconnect вызываются последовательно из неё, поэтому
// синхронизации здесь нет и быть не должно.
type handler struct {
	name     string
	symbol   market.Symbol
	provider string
	venueSym string
	bus      *events.Bus
	clock    ports.Clock
	recorder ports.Recorder
	log      *slog.Logger

	// bid и ask держатся между сообщениями: Bybit шлёт приращения и опускает
	// вершину стакана в тех обновлениях, где она не менялась.
	bid money.Price
	ask money.Price

	// parseWarned гасит повтор жалобы на неразобранное сообщение: форма одна
	// на всё соединение, и печатать её на каждом кадре значит залить лог.
	parseWarned bool
}

// Optional объявляет поток необязательным: см. wsconn.Optional.
func (h *handler) Optional() bool { return true }

// OnConnect подписывается там, где это требуется протоколом площадки.
func (h *handler) OnConnect(ctx context.Context, s wsconn.Sender) error {
	if !isBybit(h.provider) {
		// Binance отдаёт поток по адресу подключения, отдельная подписка
		// не нужна и приводит к ошибке.
		return nil
	}
	return s.SendJSON(ctx, map[string]any{
		"op":   "subscribe",
		"args": []string{"tickers." + h.venueSym},
	})
}

// OnDisconnect забывает вершину стакана.
//
// Сохранять её через разрыв нельзя: следующее соединение начинается со снимка,
// и до него склеенная из двух сессий середина описывала бы рынок, которого
// не было.
func (h *handler) OnDisconnect(error) {
	h.bid, h.ask = money.Price{}, money.Price{}
}

// OnMessage записывает кадр и публикует середину, если она в нём есть.
func (h *handler) OnMessage(ctx context.Context, payload []byte, received time.Time) {
	if h.recorder != nil {
		if err := h.recorder.Record(h.name, received, payload); err != nil {
			h.log.Error("failed to record message", "error", err)
		}
	}

	mid, ok := h.mid(payload)
	if !ok {
		return
	}
	events.Publish(h.bus, ctx, events.ReferencePriceUpdated{
		Reference: market.ReferencePrice{
			Symbol:   h.symbol,
			Venue:    h.provider,
			Price:    mid,
			Received: received,
		},
	})
}

// mid извлекает середину рынка из одного кадра.
//
// Второй результат false для кадров без цены: подтверждений подписки, ответов
// на ping и приращений, не тронувших вершину стакана. Это штатный исход,
// а не ошибка разбора, поэтому он молчит.
func (h *handler) mid(payload []byte) (money.Price, bool) {
	if isBybit(h.provider) {
		return h.bybitMid(payload)
	}
	return h.binanceMid(payload)
}

// binanceBookTicker — кадр потока <symbol>@bookTicker.
//
// Форма одинакова на споте и фьючерсах с точностью до полей, которые мы
// не читаем.
//
// Объёмы объявлены не для того, чтобы их использовать. Площадка называет цену
// бида "b", а его объём — "B", и разбор JSON в Go при отсутствии точного
// совпадения принимает совпадение без учёта регистра: без отдельного поля
// под "B" объём затирал бы только что разобранную цену, и серединой рынка
// оказывалось бы полусумма двух объёмов. Ошибка тихая — числа остаются
// правдоподобными.
type binanceBookTicker struct {
	Symbol string `json:"s"`
	Bid    string `json:"b"`
	BidQty string `json:"B"`
	Ask    string `json:"a"`
	AskQty string `json:"A"`
}

func (h *handler) binanceMid(payload []byte) (money.Price, bool) {
	var t binanceBookTicker
	if err := json.Unmarshal(payload, &t); err != nil {
		h.warnUnparsed(payload, err)
		return money.Price{}, false
	}
	// Площадка присылает и служебные кадры — ответы на подписку, сообщения
	// об ошибках. У них нет цен, и это не повод жаловаться.
	if t.Bid == "" || t.Ask == "" {
		return money.Price{}, false
	}
	// Символ приходит в каждом кадре, и чужой означал бы, что мы читаем не тот
	// поток: молча усреднить его — значит принимать решения по чужому рынку.
	if t.Symbol != "" && !strings.EqualFold(t.Symbol, h.venueSym) {
		h.log.Error("reference frame carries a foreign symbol, ignoring",
			"expected", h.venueSym, "got", t.Symbol)
		return money.Price{}, false
	}

	bid, bidErr := money.ParsePrice(t.Bid)
	ask, askErr := money.ParsePrice(t.Ask)
	if bidErr != nil || askErr != nil {
		h.warnUnparsed(payload, fmt.Errorf("bid %v, ask %v", bidErr, askErr))
		return money.Price{}, false
	}
	h.bid, h.ask = bid, ask
	return midOf(bid, ask)
}

// bybitMessage — конверт потока tickers.
type bybitMessage struct {
	Topic string `json:"topic"`
	Data  struct {
		Bid1Price string `json:"bid1Price"`
		Ask1Price string `json:"ask1Price"`
		LastPrice string `json:"lastPrice"`
	} `json:"data"`
}

func (h *handler) bybitMid(payload []byte) (money.Price, bool) {
	var msg bybitMessage
	if err := json.Unmarshal(payload, &msg); err != nil {
		h.warnUnparsed(payload, err)
		return money.Price{}, false
	}
	if msg.Topic != "tickers."+h.venueSym {
		// Подтверждения подписки и ответы на ping приходят по тому же сокету.
		return money.Price{}, false
	}

	if p, err := money.ParsePrice(msg.Data.Bid1Price); err == nil && p.IsPositive() {
		h.bid = p
	}
	if p, err := money.ParsePrice(msg.Data.Ask1Price); err == nil && p.IsPositive() {
		h.ask = p
	}
	if mid, ok := midOf(h.bid, h.ask); ok {
		return mid, true
	}
	// Пока вершина стакана не собрана из приращений, последняя сделка — лучшее,
	// что есть. Она шумнее середины, но движение крупнее шума видит и она.
	if p, err := money.ParsePrice(msg.Data.LastPrice); err == nil && p.IsPositive() {
		return p, true
	}
	return money.Price{}, false
}

// midOf возвращает середину рынка. Перевёрнутый стакан отбрасывается: он
// означает рассинхронизацию приращений, а не цену.
func midOf(bid, ask money.Price) (money.Price, bool) {
	if !bid.IsPositive() || !ask.IsPositive() || ask.Lt(bid) {
		return money.Price{}, false
	}
	return bid.Add(ask).MulRatio(decimal.NewFromFloat(0.5)), true
}

func (h *handler) warnUnparsed(payload []byte, err error) {
	if h.parseWarned {
		return
	}
	h.parseWarned = true
	h.log.Warn("failed to parse a reference frame", "error", err, "payload", truncate(payload))
}

func truncate(b []byte) string {
	const limit = 256
	if len(b) <= limit {
		return string(b)
	}
	return string(b[:limit]) + "…"
}

// bybitHandler добавляет прикладной ping.
//
// Площадка закрывает молчащее соединение через двадцать секунд, а поток
// tickers на спокойном рынке молчит дольше. Ответ сервера на ping заодно
// сбрасывает дедлайн чтения, то есть служит и проверкой живости.
type bybitHandler struct{ *handler }

func (b *bybitHandler) Heartbeat(ctx context.Context, s wsconn.Sender) error {
	return s.SendJSON(ctx, map[string]any{"op": "ping"})
}

var (
	_ wsconn.Handler     = (*handler)(nil)
	_ wsconn.Optional    = (*handler)(nil)
	_ wsconn.Heartbeater = (*bybitHandler)(nil)
)
