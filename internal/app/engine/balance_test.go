package engine

import (
	"context"
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"

	"github.com/nd/mm-standx/internal/domain/market"
	"github.com/nd/mm-standx/internal/domain/trading"
	"github.com/nd/mm-standx/internal/events"
)

// Размер берётся из баланса счёта и больше ниоткуда.
//
// Настроенной аллокации нет с 15 августа. До этого она была единственным, что
// связывало размер, и отставала от баланса вручную: 5800 в файле при 11091
// на счёте — половина денег простаивала, и сказать об этом могла только строка
// size_limit в сводке.

func balanceEvent(equity, available, locked string) events.BalanceUpdated {
	return events.BalanceUpdated{Balance: trading.Balance{
		Equity:    decimal.RequireFromString(equity),
		Available: decimal.RequireFromString(available),
		Locked:    decimal.RequireFromString(locked),
	}}
}

// Событие баланса доезжает до полей, из которых считается размер.
func TestBalanceEventLandsInTheFieldsSizingReads(t *testing.T) {
	ctx := context.Background()
	e, _, _ := dangerEngine(t)

	e.handle(ctx, balanceEvent("11091.87", "10275.90", "815.97"))

	require.Equal(t, "11091.87", e.equity.String())
	require.Equal(t, "10275.9", e.available.String())
	require.Equal(t, "815.97", e.locked.String())
}

// Больше денег на счёте — больше котировка, без единой правки конфигурации.
func TestQuotedSizeFollowsTheBalance(t *testing.T) {
	ctx := context.Background()
	e, gw, clk := dangerEngine(t)

	e.handle(ctx, balanceEvent("1000", "1000", "0"))
	e.evaluate(ctx, true)
	require.NotEmpty(t, gw.placed, "a funded account quotes")
	small := gw.placed[0].Qty
	// Биржа подтверждает размещение и затем снимает его: ступень свободна,
	// и следующий такт котирует новым размером. Без ответа биржи ступень
	// осталась бы в полёте, а такт ушёл бы на её отзыв.
	gw.confirmQuotes(e, clk.now)
	gw.releaseQuotes(e, clk.now)

	gw.placed = nil
	e.handle(ctx, balanceEvent("10000", "10000", "0"))
	clk.advance(e.cfg.Quote.RequoteCooldown + e.cfg.Quote.MinRest)
	primeVolatility(t, e, clk)
	e.evaluate(ctx, true)
	require.NotEmpty(t, gw.placed)

	require.True(t, gw.placed[0].Qty.Gt(small),
		"ten times the equity must quote more, with nothing configured to permit it")
}

// Пустой счёт котировать не даёт, и объясняет почему.
func TestAnEmptyAccountRefusesToQuoteWithAReason(t *testing.T) {
	ctx := context.Background()
	e, gw, _ := dangerEngine(t)

	e.handle(ctx, balanceEvent("0", "0", "0"))
	e.evaluate(ctx, true)

	require.Empty(t, gw.placed)
	require.NotEmpty(t, e.blocked, "a silent refusal is the failure this codebase keeps fixing")
}

// Доля блокировки измеряется по locked из того же события.
func TestLockedFromTheBalanceFeedsTheMeasuredMarginRate(t *testing.T) {
	ctx := context.Background()
	e, _, clk := dangerEngine(t)
	e.cfg.SymbolRisk.Leverage = 40

	liveRung(e, market.Buy, "bid-1", "4000", clk.now)
	liveRung(e, market.Sell, "ask-1", "4000", clk.now)
	// 0.008 x 4000 на каждой стороне — 64 USD нотионала.
	e.handle(ctx, balanceEvent("11091.87", "11090.16", "1.7069"))

	rate, ok := e.measuredMarginRate()
	require.True(t, ok)
	require.InDelta(t, 2.667, rate, 0.01)
}
