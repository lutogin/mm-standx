package reference

import (
	"encoding/json"
	"log/slog"
	"strings"

	"github.com/nd/mm-standx/internal/config"
	"github.com/nd/mm-standx/internal/domain/money"
)

// Decoder восстанавливает ряд середин рынка из записанных кадров одного фида.
//
// Существует ради того, чтобы офлайновый разбор читал запись ровно так же, как
// живой обработчик читал сокет. Два парсера над одними и теми же байтами рано
// или поздно расходятся, и расхождение проявится ошибкой в ответе на
// единственный вопрос, ради которого запись и хранится.
//
// Площадка распознаётся по самим кадрам, а не берётся из конфигурации: запись
// несёт инструмент биржи в имени потока, но не площадку, и выводить её из
// сегодняшних настроек значило бы разбирать вчерашнюю запись сегодняшними
// глазами.
type Decoder struct {
	h *handler
}

// NewDecoder создаёт декодер, ещё не определившийся с площадкой.
func NewDecoder() *Decoder { return &Decoder{} }

// probe — минимум полей, по которым различаются площадки.
//
// Объёмы объявлены по той же причине, что и в binanceBookTicker: без них
// разбор без учёта регистра затирает цену объёмом.
type probe struct {
	Symbol string `json:"s"`
	Bid    string `json:"b"`
	BidQty string `json:"B"`
	Ask    string `json:"a"`
	AskQty string `json:"A"`
	Topic  string `json:"topic"`
}

// Mid возвращает середину рынка, которую несёт кадр.
//
// Второй результат false для кадров без цены — подтверждений подписки, ответов
// на ping, приращений, не тронувших вершину стакана, — и для кадров чужого
// формата. Это штатный исход, а не ошибка.
func (d *Decoder) Mid(payload []byte) (money.Price, bool) {
	if d.h == nil {
		provider, symbol, ok := recognise(payload)
		if !ok {
			return money.Price{}, false
		}
		d.h = &handler{
			provider: provider,
			venueSym: symbol,
			log:      slog.New(slog.DiscardHandler),
		}
	}
	return d.h.mid(payload)
}

// Venue сообщает распознанную площадку и её символ. До первого разобранного
// кадра оба значения пусты.
func (d *Decoder) Venue() (provider, symbol string) {
	if d.h == nil {
		return "", ""
	}
	return d.h.provider, d.h.venueSym
}

// recognise определяет площадку по форме кадра.
//
// Спот и фьючерсы Binance отдают одинаковую форму, и различить их по кадру
// нельзя. Для разбора это безразлично: адрес подключения на разбор не влияет,
// а любой из двух вариантов ведёт к одному и тому же коду.
func recognise(payload []byte) (provider, symbol string, ok bool) {
	var p probe
	if err := json.Unmarshal(payload, &p); err != nil {
		return "", "", false
	}

	const topicPrefix = "tickers."
	if strings.HasPrefix(p.Topic, topicPrefix) {
		return config.ProviderBybitLinear, strings.TrimPrefix(p.Topic, topicPrefix), true
	}
	if p.Symbol != "" && p.Bid != "" && p.Ask != "" {
		return config.ProviderBinanceFutures, strings.ToUpper(p.Symbol), true
	}
	return "", "", false
}
