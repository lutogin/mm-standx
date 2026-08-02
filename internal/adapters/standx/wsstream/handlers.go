package wsstream

import (
	"context"
	"encoding/json"
	"time"

	"github.com/nd/mm-standx/internal/adapters/standx/dto"
	"github.com/nd/mm-standx/internal/adapters/standx/mapper"
	"github.com/nd/mm-standx/internal/events"
)

// Имена каналов StandX.
const (
	ChannelPrice     = "price"
	ChannelDepthBook = "depth_book"
	ChannelOrder     = "order"
	ChannelPosition  = "position"
	ChannelBalance   = "balance"
	ChannelTrade     = "trade"
	channelAuth      = "auth"
)

// message — конверт сообщения потока.
type message struct {
	Seq     int64           `json:"seq"`
	Channel string          `json:"channel"`
	Symbol  string          `json:"symbol"`
	Data    json.RawMessage `json:"data"`

	// Ошибки уровня соединения приходят без канала, плоским объектом.
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// OnMessage разбирает сообщение и публикует соответствующее доменное событие.
//
// Ошибка разбора одного сообщения не рвёт соединение: пропуск тика обходится
// дешевле, чем переподключение, стоящее нам присутствия в стакане.
func (h *handler) OnMessage(ctx context.Context, payload []byte, received time.Time) {
	if h.recorder != nil {
		if err := h.recorder.Record(h.name, received, payload); err != nil {
			h.log.Error("не удалось записать сообщение", "error", err)
		}
	}

	var msg message
	if err := json.Unmarshal(payload, &msg); err != nil {
		h.log.Error("не удалось разобрать сообщение", "error", err, "payload", truncate(payload))
		return
	}

	if msg.Channel == "" && msg.Code != 0 {
		h.log.Error("ошибка потока", "code", msg.Code, "message", msg.Message)
		return
	}
	h.checkSequence(msg)

	switch msg.Channel {
	case ChannelPrice:
		h.handlePrice(ctx, msg, received)
	case ChannelDepthBook:
		h.handleDepthBook(ctx, msg, received)
	case ChannelOrder:
		h.handleOrder(ctx, msg)
	case ChannelPosition:
		h.handlePosition(ctx, msg)
	case ChannelBalance:
		h.handleBalance(ctx, msg, received)
	case ChannelTrade:
		h.handleTrade(ctx, msg)
	case channelAuth:
		h.handleAuth(msg)
	default:
		h.log.Debug("неизвестный канал", "channel", msg.Channel)
	}
}

// checkSequence отслеживает пропуски в нумерации сообщений.
//
// Счётчик сквозной для соединения, а не для канала. Пропуск означает потерянное
// обновление: стакан или состояние ордера могли разойтись с действительностью,
// и это повод для внеочередной сверки.
func (h *handler) checkSequence(msg message) {
	if msg.Seq == 0 {
		return
	}
	h.seqMu.Lock()
	defer h.seqMu.Unlock()

	if h.lastSeq != 0 && msg.Seq > h.lastSeq+1 {
		h.log.Warn("пропуск в нумерации сообщений",
			"expected", h.lastSeq+1, "got", msg.Seq, "lost", msg.Seq-h.lastSeq-1)
	}
	if msg.Seq > h.lastSeq {
		h.lastSeq = msg.Seq
	}
}

func (h *handler) handlePrice(ctx context.Context, msg message, received time.Time) {
	var d dto.SymbolPrice
	if err := json.Unmarshal(msg.Data, &d); err != nil {
		h.log.Error("разбор price", "error", err)
		return
	}
	mark, err := mapper.MarkPrice(d, received)
	if err != nil {
		h.log.Error("маппинг mark price", "error", err, "symbol", d.Symbol)
		return
	}
	events.Publish(h.bus, ctx, events.MarkPriceUpdated{Mark: mark})
}

func (h *handler) handleDepthBook(ctx context.Context, msg message, received time.Time) {
	var d dto.DepthBook
	if err := json.Unmarshal(msg.Data, &d); err != nil {
		h.log.Error("разбор depth_book", "error", err)
		return
	}
	if d.Symbol == "" {
		d.Symbol = msg.Symbol
	}
	book, err := mapper.Book(d, received)
	if err != nil {
		h.log.Error("маппинг стакана", "error", err, "symbol", d.Symbol)
		return
	}
	events.Publish(h.bus, ctx, events.BookUpdated{Book: book})
}

func (h *handler) handleOrder(ctx context.Context, msg message) {
	var d dto.Order
	if err := json.Unmarshal(msg.Data, &d); err != nil {
		h.log.Error("разбор order", "error", err)
		return
	}
	order, err := mapper.Order(d)
	if err != nil {
		h.log.Error("маппинг ордера", "error", err)
		return
	}
	events.Publish(h.bus, ctx, events.OrderUpdated{Order: order})
}

func (h *handler) handlePosition(ctx context.Context, msg message) {
	var d dto.Position
	if err := json.Unmarshal(msg.Data, &d); err != nil {
		h.log.Error("разбор position", "error", err)
		return
	}
	position, err := mapper.Position(d)
	if err != nil {
		h.log.Error("маппинг позиции", "error", err)
		return
	}
	events.Publish(h.bus, ctx, events.PositionUpdated{Position: position})
}

func (h *handler) handleBalance(ctx context.Context, msg message, received time.Time) {
	var d dto.WalletBalance
	if err := json.Unmarshal(msg.Data, &d); err != nil {
		h.log.Error("разбор balance", "error", err)
		return
	}
	balance := mapper.WalletBalance(d)
	balance.UpdatedAt = received
	events.Publish(h.bus, ctx, events.BalanceUpdated{Balance: balance})
}

func (h *handler) handleTrade(ctx context.Context, msg message) {
	var d dto.Trade
	if err := json.Unmarshal(msg.Data, &d); err != nil {
		h.log.Error("разбор trade", "error", err)
		return
	}
	trade, err := mapper.Trade(d)
	if err != nil {
		h.log.Error("маппинг сделки", "error", err)
		return
	}
	events.Publish(h.bus, ctx, events.TradeExecuted{Trade: trade})
}

func (h *handler) handleAuth(msg message) {
	var d struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	if err := json.Unmarshal(msg.Data, &d); err != nil {
		h.log.Error("разбор ответа авторизации", "error", err)
		return
	}
	if d.Code != 200 {
		h.log.Error("авторизация потока отклонена", "code", d.Code, "message", d.Msg)
		return
	}
	h.log.Info("поток авторизован")
}

func truncate(b []byte) string {
	const limit = 256
	if len(b) <= limit {
		return string(b)
	}
	return string(b[:limit]) + "…"
}
