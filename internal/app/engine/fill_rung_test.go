package engine

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nd/mm-standx/internal/domain/market"
	"github.com/nd/mm-standx/internal/domain/money"
	"github.com/nd/mm-standx/internal/domain/quotes"
	"github.com/nd/mm-standx/internal/domain/trading"
	"github.com/nd/mm-standx/internal/events"
)

// Ступень, на которой стоял залитый ордер, — вход в решение о геометрии
// лесенки, и восстановить её задним числом нельзя ничем.
//
// Лесенка ограничивает объём одного касания только если рынок останавливается
// на ближней ступени. Если он снимает сторону свипом, дальние ступени защиты
// не покупают, а вес теряют: зачёт считается по среднему весу ступеней, и три
// ступени на 7.0 / 7.4 / 7.8 bps дают 0.816 против 0.880 у одиночного ордера.
// Обе картины есть в записях — три ступени за 51 мс 6 августа, шесть
// исполнений за 1.8 с 18 августа, — и обе про аварийные эпизоды. Как выглядит
// обычный залив, не знает никто, потому что метрика была размечена символом
// и стороной, но не ступенью.
func TestFillCarriesTheRungItStoodOn(t *testing.T) {
	ctx := context.Background()
	e, _, clk, _ := fillEngine(t)

	var filled []events.QuoteFilled
	events.Subscribe(e.deps.Bus, "rung-test", func(_ context.Context, ev events.QuoteFilled) {
		filled = append(filled, ev)
	})

	// Лесенка из трёх ступеней: помощник строит актора с одной, а различить
	// ступени можно только там, где их больше одной.
	e.asks = quotes.New(market.Sell, 3)

	// Средняя ступень стоит в стакане.
	const slot = 1
	e.asks.OnPlaceSent(slot, "quote-1", money.MustParsePrice("4279"),
		money.MustParseQty("0.008"), clk.now)
	e.asks.OnOrderUpdate(trading.Order{
		ClOrdID: "quote-1", Symbol: "XAU-USD", Side: market.Sell,
		Status: trading.StatusOpen,
	}, clk.now)

	e.onOrder(ctx, events.OrderUpdated{
		Order: fill("quote-1", market.Sell, "0.008", "4276.1"),
	})

	require.Len(t, filled, 1)
	require.Equal(t, slot, filled[0].Rung, "the fill must name the rung it came from")
}

// Залив, который больше не сопоставляется со ступенью, обязан оставаться
// неразмеченным, а не записываться в нулевую.
//
// Ноль — это ближняя ступень, то есть ровно тот исход, ради различения которого
// разметка и заведена: приписав ей всё неопознанное, мы получили бы картину
// «заливают только ближнюю» независимо от того, что делает рынок.
//
// Случай не гипотетический. Ступень освобождается по таймауту подтверждения
// (quotes.Ladder.Plan) и при сверке с биржей, а частично исполненный ордер
// при этом остаётся в учёте залитого объёма: слота уже нет, ордер ещё есть.
func TestUnattributedFillIsNotCountedAsTheNearRung(t *testing.T) {
	ctx := context.Background()
	e, _, clk, _ := fillEngine(t)
	e.asks = quotes.New(market.Sell, 3)

	var filled []events.QuoteFilled
	events.Subscribe(e.deps.Bus, "rung-test", func(_ context.Context, ev events.QuoteFilled) {
		filled = append(filled, ev)
	})

	e.asks.OnPlaceSent(0, "quote-1", money.MustParsePrice("4279"),
		money.MustParseQty("0.008"), clk.now)
	e.asks.OnOrderUpdate(trading.Order{
		ClOrdID: "quote-1", Symbol: "XAU-USD", Side: market.Sell,
		Status: trading.StatusOpen,
	}, clk.now)

	// Частичное исполнение: ордер жив, ступень занята, объём учтён.
	partial := fill("quote-1", market.Sell, "0.008", "4276.1")
	partial.Status = trading.StatusOpen
	partial.FilledQty = money.MustParseQty("0.005")
	e.onOrder(ctx, events.OrderUpdated{Order: partial})

	// Ступень освободилась помимо сообщения об ордере — так делает таймаут
	// подтверждения и так делает сверка.
	e.asks = quotes.New(market.Sell, 3)

	e.onOrder(ctx, events.OrderUpdated{
		Order: fill("quote-1", market.Sell, "0.008", "4276.1"),
	})

	require.Len(t, filled, 2)
	require.Equal(t, 0, filled[0].Rung)
	require.Equal(t, events.RungUnknown, filled[1].Rung,
		"a fill that no longer maps to a slot must not be attributed")
}
