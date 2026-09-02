package engine

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nd/mm-standx/internal/config"
	"github.com/nd/mm-standx/internal/domain/market"
	"github.com/nd/mm-standx/internal/domain/money"
	"github.com/nd/mm-standx/internal/domain/trading"
	"github.com/nd/mm-standx/internal/events"
	"github.com/nd/mm-standx/internal/ports"
)

// Залив закрывается мейкером, маркет — только остаток по таймауту.
//
// Разбор 2 сентября 2026: из 31.86 USD утренних комиссий 24.8 — тейкерские
// 4 bps на одном маркет-закрытии 14.363 XAU, при проскальзывании 0.1 USD.
// Мейкерское закрытие на том же заливе стоило бы 6.2.

func makerEngine(t *testing.T) (*Engine, *recordingGateway, *testClock) {
	t.Helper()
	e, gw, clk, _ := fillEngine(t)
	e.risk.FlattenPolicy = config.FlattenMakerThenMarket
	e.risk.MakerUnwindTimeout = 3 * time.Second
	// Без буфера post-only лучшая цена совпадает с вершиной и пересекает
	// стакан; в бою буфер 2 тика, стенд повторяет его.
	e.cfg.Quote.PostOnlyBufferTicks = 2
	// fillEngine не задаёт ни инструмент, ни стакан. Без инструмента тик
	// равен единице и буфер в два тика уводит цену на два доллара; без
	// стакана мейкерское закрытие честно уходит в маркет. Боевое золото:
	// тик 0.1, три знака объёма — как у dangerEngine.
	e.spec = market.SymbolSpec{
		Symbol:            "XAU-USD",
		PriceTickDecimals: 1,
		QtyTickDecimals:   3,
		MinOrderQty:       money.MustParseQty("0.001"),
		MaxOrderQty:       money.MustParseQty("100"),
		MaxLeverage:       10,
		Enabled:           true,
	}
	e.book = market.NewBook("XAU-USD",
		[]market.Level{{Price: money.MustParsePrice("4275"), Qty: money.MustParseQty("1")}},
		[]market.Level{{Price: money.MustParsePrice("4277"), Qty: money.MustParseQty("1")}},
		clk.now)
	return e, gw, clk
}

func limitCloses(gw *recordingGateway) []ports.PlaceRequest {
	var out []ports.PlaceRequest
	for _, r := range gw.placed {
		if r.Type == trading.Limit && r.ReduceOnly {
			out = append(out, r)
		}
	}
	return out
}

// Первый ответ на залив — post-only reduce-only лимит на вершине стакана,
// а не маркет.
func TestFillIsClosedAsMakerFirst(t *testing.T) {
	ctx := context.Background()
	e, gw, _ := makerEngine(t)

	e.onFill(ctx, fill("quote-1", market.Sell, "0.008", "4276.1"), 0)

	require.Empty(t, gw.marketOrders(), "no taker order while the maker close stands")
	closes := limitCloses(gw)
	require.Len(t, closes, 1)
	c := closes[0]
	require.Equal(t, market.Buy, c.Side, "a sell fill is unwound by a buy")
	require.Equal(t, trading.ALO, c.TimeInForce, "post-only, or it is not a maker close")
	require.True(t, c.ReduceOnly)
	// Стакан стенда: бид 4275, аск 4277, тик 0.1, буфер 2 тика — покупка
	// встаёт на 4276.8: лучшая цена, остающаяся мейкером.
	require.Equal(t, "4276.8", c.Price.String())
	require.NotNil(t, e.makerClose)
}

// Пока срок не вышел, восстановление по позиции не шлёт маркет поверх.
func TestRecoveryWaitsForTheMakerClose(t *testing.T) {
	ctx := context.Background()
	e, gw, clk := makerEngine(t)

	e.onFill(ctx, fill("quote-1", market.Sell, "0.008", "4276.1"), 0)
	e.position = money.MustParseQty("-0.008")

	for range 8 { // две секунды из трёх
		clk.advance(250 * time.Millisecond)
		e.onTick(ctx)
	}
	require.Empty(t, gw.marketOrders(), "the maker close has time left")
}

// По таймауту мейкерский ордер снимается, остаток уходит маркетом.
func TestMakerCloseTimesOutIntoMarket(t *testing.T) {
	ctx := context.Background()
	e, gw, clk := makerEngine(t)

	e.onFill(ctx, fill("quote-1", market.Sell, "0.008", "4276.1"), 0)
	e.position = money.MustParseQty("-0.008")
	id := e.makerClose.id

	clk.advance(3*time.Second + 100*time.Millisecond)
	e.onTick(ctx)

	require.Contains(t, gw.cancelled, id, "the resting maker close is revoked first")
	require.Len(t, gw.marketOrders(), 1, "then the remainder goes at market")
	require.Nil(t, e.makerClose)
}

// Отвергнутый post-only (стакан ушёл) — маркет немедленно, без ожидания срока.
func TestRejectedMakerCloseFallsBackAtOnce(t *testing.T) {
	ctx := context.Background()
	e, gw, _ := makerEngine(t)

	e.onFill(ctx, fill("quote-1", market.Sell, "0.008", "4276.1"), 0)
	e.position = money.MustParseQty("-0.008")
	id := e.makerClose.id

	e.handle(ctx, rejectEvent(id, "post only order would take liquidity"))

	require.Nil(t, e.makerClose)
	require.Len(t, gw.marketOrders(), 1, "no reason to wait: the maker order never stood")
}

// Исполнившееся мейкерское закрытие — конец эпизода без единого маркета.
func TestFilledMakerCloseEndsTheEpisode(t *testing.T) {
	ctx := context.Background()
	e, gw, clk := makerEngine(t)

	e.onFill(ctx, fill("quote-1", market.Sell, "0.008", "4276.1"), 0)
	e.position = money.MustParseQty("-0.008")
	id := e.makerClose.id

	e.onOrder(ctx, events.OrderUpdated{Order: trading.Order{
		ClOrdID: id, Symbol: "XAU-USD", Side: market.Buy, Status: trading.StatusFilled,
		Qty: money.MustParseQty("0.008"), FilledQty: money.MustParseQty("0.008"),
		AvgFillPrice: money.MustParsePrice("4276.8"),
	}})
	e.position = money.MustParseQty("0")

	require.Nil(t, e.makerClose)
	clk.advance(5 * time.Second)
	e.onTick(ctx)
	require.Empty(t, gw.marketOrders(), "a filled maker close needs no market order")
}

// Прежняя политика не меняется: immediate_market — маркет сразу.
func TestImmediateMarketPolicyIsUnchanged(t *testing.T) {
	ctx := context.Background()
	e, gw, _ := makerEngine(t)
	e.risk.FlattenPolicy = config.FlattenImmediateMarket

	e.onFill(ctx, fill("quote-1", market.Sell, "0.008", "4276.1"), 0)

	require.Len(t, gw.marketOrders(), 1)
	require.Empty(t, limitCloses(gw))
	require.Nil(t, e.makerClose)
}
