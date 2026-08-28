package engine

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nd/mm-standx/internal/domain/market"
	"github.com/nd/mm-standx/internal/domain/quotes"
	"github.com/nd/mm-standx/internal/domain/trading"
	"github.com/nd/mm-standx/internal/events"
)

// Отказ «balance not enough» не имеет права идти в горячий цикл.
//
// Замер боевого счёта 28 августа 2026, 13:10-13:15 UTC: 550 отказов за пять
// минут, 1.8 в секунду. Освобождённая ступень немедленно шла на повторное
// заполнение, денег снова не хватало, и цикл держался ровно столько, сколько
// держалась причина. Это 14% всего ордерного трафика окна, потраченные
// на один и тот же ответ биржи.

func rejectEvent(id trading.ClOrdID, reason string) events.OrderRejected {
	return events.OrderRejected{ClOrdID: id, Code: 400, Reason: reason}
}

// Ступень запирается на настроенный срок, а не освобождается немедленно.
func TestBalanceRejectLocksTheRung(t *testing.T) {
	ctx := context.Background()
	e, _, clk := dangerEngine(t)
	e.rules.BalanceRejectBackoff = 5 * time.Second

	liveRung(e, market.Buy, "bid-0", "4270", clk.now)
	e.handle(ctx, rejectEvent("bid-0", "balance not enough"))

	slot := e.ladderFor(market.Buy).Slots[0]
	require.Equal(t, quotes.Empty, slot.Phase, "the rung is freed, the order does not exist")
	require.True(t, slot.RefillNotBefore.After(clk.now),
		"but it must not be refilled at once: the next attempt gets the same answer")
	require.Equal(t, clk.now.Add(5*time.Second), slot.RefillNotBefore)
}

// Отказ post-only этой задержки не получает.
//
// Он означает, что стакан ушёл, а не что кончились деньги. Правильный ответ —
// пересчитать цену и выставиться снова, немедленно; держать ступень пустой
// значит платить присутствием за чужое движение.
func TestPostOnlyRejectDoesNotLockTheRung(t *testing.T) {
	ctx := context.Background()
	e, _, clk := dangerEngine(t)
	e.rules.BalanceRejectBackoff = 5 * time.Second

	liveRung(e, market.Buy, "bid-0", "4270", clk.now)
	e.handle(ctx, rejectEvent("bid-0", "post only order would take liquidity"))

	slot := e.ladderFor(market.Buy).Slots[0]
	require.Equal(t, quotes.Empty, slot.Phase)
	require.True(t, slot.RefillNotBefore.IsZero() || !slot.RefillNotBefore.After(clk.now),
		"a book that moved is not a budget that ran out")
}

// Формулировку присылает биржа, и обрамление у неё своё. Распознавание
// не должно зависеть ни от регистра, ни от того, что стоит вокруг.
func TestBalanceRejectRecognisedRegardlessOfWrapping(t *testing.T) {
	for _, reason := range []string{
		"balance not enough",
		"Balance not enough",
		"400: BALANCE NOT ENOUGH for order 12625764138",
	} {
		require.True(t, insufficientBalance(rejectEvent("x", reason)), reason)
	}
	for _, reason := range []string{
		"post only order would take liquidity",
		"order already canceled: 12625764138",
		"",
	} {
		require.False(t, insufficientBalance(rejectEvent("x", reason)), reason)
	}
}

// Ноль возвращает прежнее поведение: немедленный повтор.
func TestZeroBackoffKeepsTheOldBehaviour(t *testing.T) {
	ctx := context.Background()
	e, _, clk := dangerEngine(t)
	e.rules.BalanceRejectBackoff = 0

	liveRung(e, market.Buy, "bid-0", "4270", clk.now)
	e.handle(ctx, rejectEvent("bid-0", "balance not enough"))

	slot := e.ladderFor(market.Buy).Slots[0]
	require.True(t, slot.RefillNotBefore.IsZero() || !slot.RefillNotBefore.After(clk.now))
}

// Запертая ступень действительно не заполняется, пока срок не вышел, —
// и заполняется, когда вышел. Это и есть та экономия запросов, ради которой
// задержка заведена.
func TestLockedRungIsNotRefilledUntilTheBackoffExpires(t *testing.T) {
	ctx := context.Background()
	e, gw, clk := dangerEngine(t)
	e.rules.BalanceRejectBackoff = 5 * time.Second

	primeVolatility(t, e, clk)
	e.handle(ctx, balanceEvent("10000", "10000", "0"))

	liveRung(e, market.Buy, "bid-0", "4270", clk.now)
	e.handle(ctx, rejectEvent("bid-0", "balance not enough"))

	gw.placed = nil
	clk.advance(4 * time.Second)
	e.evaluate(ctx, true)
	for _, o := range gw.placed {
		require.NotEqual(t, market.Buy, o.Side, "the rung is still locked")
	}

	clk.advance(2 * time.Second)
	e.evaluate(ctx, true)
	require.NotEmpty(t, gw.placed, "once the backoff expires the rung comes back")
}
