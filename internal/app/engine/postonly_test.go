package engine

import (
	"bytes"
	"context"
	"log/slog"
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"

	"github.com/nd/mm-standx/internal/domain/market"
	"github.com/nd/mm-standx/internal/domain/money"
)

// В обычной обстановке котировка стоит глубже вершины на несколько базисных
// пунктов, и буфер до неё не достаёт. Защита, срабатывающая постоянно, была бы
// не защитой, а сдвигом всей геометрии.
func TestPostOnlyBufferDoesNotTouchAnOrdinaryQuote(t *testing.T) {
	e, gw, clk := dangerEngine(t)
	e.available = decimal.NewFromInt(1000)
	e.cfg.Quote.PostOnlyBufferTicks = 2

	e.evaluate(context.Background(), true)

	require.NotEmpty(t, gw.placed)
	for _, req := range gw.placed {
		distance, err := market.DistanceBps(e.mark, req.Side, req.Price)
		require.NoError(t, err)
		require.InDelta(t, float64(e.target.Distance), float64(distance), 0.2,
			"the quote stands where the risk model put it")
	}
	_ = clk
}

// Стакан ушёл дальше, чем успела обновиться справочная цена: геометрия
// котировки построена на устаревшем mark, и отправлять её незачем. Прежде такой
// ордер уходил на биржу и возвращался отказом post-only — круг запроса и пустая
// ступень, — либо вставал на вершину и забирался ближайшим движением.
func TestQuotePushedOutOfTheBandIsNotSent(t *testing.T) {
	e, gw, clk := dangerEngine(t)
	e.available = decimal.NewFromInt(1000)
	e.cfg.Quote.PostOnlyBufferTicks = 2

	var buf bytes.Buffer
	e.deps.Log = slog.New(slog.NewTextHandler(&buf, nil))

	// Рынок провалился далеко ниже mark: наша покупка на 7 bps оказывается
	// выше лучшей продажи.
	e.book = market.NewBook("XAU-USD",
		[]market.Level{{Price: money.MustParsePrice("4270"), Qty: money.MustParseQty("1")}},
		[]market.Level{{Price: money.MustParsePrice("4270.2"), Qty: money.MustParseQty("1")}},
		clk.now)

	e.evaluate(context.Background(), true)

	for _, req := range gw.placed {
		require.NotEqual(t, market.Buy, req.Side,
			"a buy built on a stale mark must not reach the exchange")
	}
	require.Contains(t, buf.String(), "post-only buffer pushed out of the band")
}

// На спокойном стакане буфер не меняет ничего из того, что уходит на биржу.
//
// Сравниваются два независимых актора, а не два пересчёта одного: после первого
// размещения ступени заняты, и второй пересчёт ничего не отправит независимо
// от настройки — тест проверял бы работу лесенки вместо буфера.
func TestZeroBufferLeavesTheOldPath(t *testing.T) {
	placedPrices := func(buffer int) []money.Price {
		e, gw, _ := dangerEngine(t)
		e.available = decimal.NewFromInt(1000)
		e.cfg.Quote.PostOnlyBufferTicks = buffer
		e.evaluate(context.Background(), true)

		out := make([]money.Price, 0, len(gw.placed))
		for _, req := range gw.placed {
			out = append(out, req.Price)
		}
		return out
	}

	require.NotEmpty(t, placedPrices(0))
	require.Equal(t, placedPrices(0), placedPrices(2))
}
