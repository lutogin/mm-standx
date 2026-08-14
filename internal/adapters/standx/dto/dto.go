// Package dto описывает проводные структуры StandX.
//
// Числовые величины биржа передаёт строками, а целые — числами. Здесь всё
// принимается как есть, без интерпретации: разбор и приведение к доменным типам
// происходит в пакете mapper. Так ошибка формата отделена от ошибки смысла.
//
// The split above is a description of what the venue was observed doing, not a
// guarantee it gave: query_position_config returns leverage quoted. Fields that
// have been seen in both forms use Int, which accepts either.
package dto

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// Int is an integer the venue may send as a JSON number or as a quoted string.
//
// The cost of assuming one form is not a parse error someone reads and fixes:
// PositionConfig.Leverage was typed int, every read of the account leverage
// failed, and ApplyLeverage is deliberately non-fatal — it logs once and leaves
// the setting alone. The account therefore ran at the venue default of 9.49
// while the configuration said forty, and the only visible symptom was an
// accrual smaller than it should have been (PLAN.md §12.5).
type Int int

func (i *Int) UnmarshalJSON(raw []byte) error {
	var number int
	if err := json.Unmarshal(raw, &number); err == nil {
		*i = Int(number)
		return nil
	}

	var quoted string
	if err := json.Unmarshal(raw, &quoted); err != nil {
		return fmt.Errorf("value %s is neither a number nor a string", raw)
	}
	parsed, err := strconv.Atoi(strings.TrimSpace(quoted))
	if err != nil {
		return fmt.Errorf("quoted value %q is not an integer: %w", quoted, err)
	}
	*i = Int(parsed)
	return nil
}

// Envelope — общий конверт ответа торговых эндпойнтов.
//
// Code == 0 означает, что заявка принята к обработке, но не что она исполнена:
// результат матчинга приходит отдельным событием в Order Response Stream.
type Envelope struct {
	Code      int    `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id"`
	Status    string `json:"status"`
}

// BatchEnvelope — ответ пакетной отмены.
//
// Биржа отдаёт массив конвертов, по одному на ордер, хотя документация
// показывает одиночный объект. Тип принимает обе формы: расхождение обнаружено
// на живом счёте, и полагаться на то, что оно не изменится обратно, не стоит.
type BatchEnvelope []Envelope

func (b *BatchEnvelope) UnmarshalJSON(raw []byte) error {
	var many []Envelope
	if err := json.Unmarshal(raw, &many); err == nil {
		*b = many
		return nil
	}

	var one Envelope
	if err := json.Unmarshal(raw, &one); err != nil {
		return fmt.Errorf("batch response is neither an array nor an object: %w", err)
	}
	*b = BatchEnvelope{one}
	return nil
}

// FirstError returns the first non-zero code reported for the batch.
func (b BatchEnvelope) FirstError() *Envelope {
	for i := range b {
		if b[i].Code != 0 {
			return &b[i]
		}
	}
	return nil
}

// Page — постраничный ответ.
type Page[T any] struct {
	PageSize int `json:"page_size"`
	Result   []T `json:"result"`
	Total    int `json:"total"`
}

// Order — состояние ордера.
type Order struct {
	ID           int64  `json:"id"`
	ClOrdID      string `json:"cl_ord_id"`
	Symbol       string `json:"symbol"`
	Side         string `json:"side"`
	OrderType    string `json:"order_type"`
	TimeInForce  string `json:"time_in_force"`
	Status       string `json:"status"`
	Price        string `json:"price"`
	Qty          string `json:"qty"`
	FillQty      string `json:"fill_qty"`
	FillAvgPrice string `json:"fill_avg_price"`
	ReduceOnly   bool   `json:"reduce_only"`
	Leverage     string `json:"leverage"`
	PositionID   int64  `json:"position_id"`
	Source       string `json:"source"`
	Remark       string `json:"remark"`
	CreatedAt    string `json:"created_at"`
	UpdatedAt    string `json:"updated_at"`
}

// Position — открытая позиция.
type Position struct {
	ID          int64  `json:"id"`
	Symbol      string `json:"symbol"`
	Qty         string `json:"qty"`
	EntryPrice  string `json:"entry_price"`
	MarkPrice   string `json:"mark_price"`
	LiqPrice    string `json:"liq_price"`
	Leverage    string `json:"leverage"`
	MarginMode  string `json:"margin_mode"`
	MarginAsset string `json:"margin_asset"`
	Unrealized  string `json:"upnl"`
	Realized    string `json:"realized_pnl"`
	Status      string `json:"status"`
	UpdatedAt   string `json:"updated_at"`
	Time        string `json:"time"`
}

// Balance — сводка средств аккаунта.
type Balance struct {
	IsolatedBalance string `json:"isolated_balance"`
	IsolatedUPnL    string `json:"isolated_upnl"`
	CrossBalance    string `json:"cross_balance"`
	CrossMargin     string `json:"cross_margin"`
	CrossUPnL       string `json:"cross_upnl"`
	Locked          string `json:"locked"`
	CrossAvailable  string `json:"cross_available"`
	Balance         string `json:"balance"`
	UPnL            string `json:"upnl"`
	Equity          string `json:"equity"`
	PnLFreeze       string `json:"pnl_freeze"`
}

// WalletBalance — сообщение канала balance.
//
// Формат отличается от query_balance: здесь кошелёк с точки зрения учёта средств,
// без сводных полей equity и upnl. Для полной картины счёта нужен query_balance.
type WalletBalance struct {
	AccountType string `json:"account_type"`
	Token       string `json:"token"`
	Free        string `json:"free"`
	Locked      string `json:"locked"`
	Occupied    string `json:"occupied"`
	Total       string `json:"total"`
}

// Trade — наше исполнение.
type Trade struct {
	ID       int64  `json:"id"`
	OrderID  int64  `json:"order_id"`
	Symbol   string `json:"symbol"`
	Side     string `json:"side"`
	Price    string `json:"price"`
	Qty      string `json:"qty"`
	Value    string `json:"value"`
	Fee      string `json:"fee_qty"`
	FeeAsset string `json:"fee_asset"`
	PnL      string `json:"pnl"`
	Time     string `json:"created_at"`

	// IsTaker сообщает, какой стороной сделки мы были.
	//
	// Комиссии и результата канал `trade` не несёт вовсе — ни под каким
	// именем, это уведомление об исполнении, а не расчётная запись.
	// Спецификация описывает поля `fee_qty` и `pnl` у REST `query_trades`,
	// и там они есть; отсюда приходит только этот флаг, но его достаточно:
	// ставку выбираем по нему, а результат считаем по ценам сами.
	IsTaker bool `json:"is_taker"`
}

// SymbolInfo — торговая спецификация инструмента.
type SymbolInfo struct {
	Symbol            string `json:"symbol"`
	BaseAsset         string `json:"base_asset"`
	QuoteAsset        string `json:"quote_asset"`
	PriceTickDecimals int32  `json:"price_tick_decimals"`
	QtyTickDecimals   int32  `json:"qty_tick_decimals"`
	MinOrderQty       string `json:"min_order_qty"`
	MaxOrderQty       string `json:"max_order_qty"`
	MaxOpenOrders     string `json:"max_open_orders"`
	MaxPositionSize   string `json:"max_position_size"`
	MaxLeverage       string `json:"max_leverage"`
	DefLeverage       string `json:"def_leverage"`
	MakerFee          string `json:"maker_fee"`
	TakerFee          string `json:"taker_fee"`
	PriceCapRatio     string `json:"price_cap_ratio"`
	PriceFloorRatio   string `json:"price_floor_ratio"`
	Enabled           bool   `json:"enabled"`
}

// MarketOverview — сводка по всем рынкам. Единственный достоверный источник
// списка символов: перечисление в справочнике API устарело.
type MarketOverview struct {
	Summary struct {
		OpenInterestNotional string `json:"open_interest_notional"`
		SymbolCount          int    `json:"symbol_count"`
		VolumeQuote24h       string `json:"volume_quote_24h"`
	} `json:"summary"`
	Symbols []MarketSummary `json:"symbols"`
}

// MarketSummary — строка сводки по рынку.
type MarketSummary struct {
	Symbol      string `json:"symbol"`
	Base        string `json:"base"`
	Quote       string `json:"quote"`
	LastPrice   string `json:"last_price"`
	MarkPrice   string `json:"mark_price"`
	FundingRate string `json:"funding_rate"`
	Volume24h   string `json:"volume_24h"`
	Time        string `json:"time"`
}

// SymbolPrice — котировочный снимок инструмента. Поля last/mid/spread могут быть
// пустыми, если сделок давно не было.
type SymbolPrice struct {
	Symbol     string `json:"symbol"`
	Base       string `json:"base"`
	Quote      string `json:"quote"`
	MarkPrice  string `json:"mark_price"`
	IndexPrice string `json:"index_price"`
	LastPrice  string `json:"last_price"`
	MidPrice   string `json:"mid_price"`
	SpreadBid  string `json:"spread_bid"`
	SpreadAsk  string `json:"spread_ask"`
	Time       string `json:"time"`
}

// DepthBook — снимок стакана. Уровень передаётся парой ["цена", "объём"],
// порядок уровней биржей не гарантирован.
type DepthBook struct {
	Symbol string      `json:"symbol"`
	Bids   [][2]string `json:"bids"`
	Asks   [][2]string `json:"asks"`
}

// NewOrderRequest — тело запроса на размещение ордера.
//
// Порядок полей значим: тело сериализуется один раз и в этом же виде подписывается.
type NewOrderRequest struct {
	Symbol      string `json:"symbol"`
	Side        string `json:"side"`
	OrderType   string `json:"order_type"`
	Qty         string `json:"qty"`
	Price       string `json:"price,omitempty"`
	TimeInForce string `json:"time_in_force"`
	ReduceOnly  bool   `json:"reduce_only"`
	ClOrdID     string `json:"cl_ord_id,omitempty"`
	MarginMode  string `json:"margin_mode,omitempty"`
	Leverage    int    `json:"leverage,omitempty"`
}

// CancelOrderRequest — отмена одного ордера по клиентскому идентификатору.
type CancelOrderRequest struct {
	ClOrdID string `json:"cl_ord_id"`
}

// CancelOrdersRequest — пакетная отмена. Один запрос на всю сторону — основной
// инструмент аварийного съёма котировок.
type CancelOrdersRequest struct {
	ClOrdIDList []string `json:"cl_ord_id_list"`
}

// PositionConfig — ответ query_position_config: действующие настройки счёта
// по инструменту.
type PositionConfig struct {
	Symbol string `json:"symbol"`
	// Leverage arrives quoted — "40", not 40 — which is why it is Int.
	Leverage   Int    `json:"leverage"`
	MarginMode string `json:"margin_mode"`
}

// ChangeLeverageRequest — тело change_leverage.
type ChangeLeverageRequest struct {
	Symbol   string `json:"symbol"`
	Leverage int    `json:"leverage"`
}
