package engine

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nd/mm-standx/internal/domain/money"
	"github.com/nd/mm-standx/internal/domain/trading"
	"github.com/nd/mm-standx/internal/events"
)

// Повтор закрытия замедляется, но не прекращается.
//
// Замер боевого счёта 25-28 августа 2026: 4 эпизода из 15 породили дубли,
// худший — шесть закрывающих ордеров на один залив 0.03 XAU при ответе биржи
// через 11.49 с, пять из них отвергнуты. Подавлять повтор нельзя: молчание
// биржи неотличимо от потерянного ордера, а пропущенное закрытие стоит
// открытой позиции, тогда как дубль — только запроса.

func positionEvent(qty string) events.PositionUpdated {
	return events.PositionUpdated{Position: trading.Position{
		Symbol: "XAU-USD",
		Qty:    money.MustParseQty(qty),
	}}
}

// Первая попытка приходит через AckTimeout — скорость восстановления
// в обычном случае не меняется.
func TestFirstRetryStillComesAtTheAckTimeout(t *testing.T) {
	ctx := context.Background()
	e, gw, clk := dangerEngine(t)

	e.handle(ctx, balanceEvent("10000", "10000", "0"))
	e.handle(ctx, positionEvent("0.03"))

	e.flatten(ctx)
	require.Len(t, gw.placed, 1)

	clk.advance(e.health.AckTimeout + 100*time.Millisecond)
	e.flatten(ctx)
	require.Len(t, gw.placed, 2, "a silent venue must be treated as a lost order")
}

// Шаг удваивается, поэтому молчание биржи не превращается в шторм.
func TestRepeatedAttemptsBackOff(t *testing.T) {
	ctx := context.Background()
	e, gw, clk := dangerEngine(t)

	e.handle(ctx, balanceEvent("10000", "10000", "0"))
	e.handle(ctx, positionEvent("0.03"))

	e.flatten(ctx)
	require.Len(t, gw.placed, 1)

	// Ровно эпизод 15:56: биржа молчит 11.5 секунды, такт идёт каждые 250 мс.
	for range 46 {
		clk.advance(250 * time.Millisecond)
		e.flatten(ctx)
	}
	// Попытки в 0, +2 и +6 секунд; следующая пришлась бы на +14, за пределы
	// эпизода. Прежний фиксированный шаг в 2 с давал шесть — ровно столько
	// и напечатал боевой лог 28 августа в 15:56.
	require.Len(t, gw.placed, 3)
}

// Пауза не растёт без предела: открытая позиция обязана получать попытку
// в разумное время, сколько бы их уже ни было.
func TestBackoffIsCapped(t *testing.T) {
	ctx := context.Background()
	e, _, clk := dangerEngine(t)

	e.handle(ctx, balanceEvent("10000", "10000", "0"))
	e.handle(ctx, positionEvent("0.03"))
	for range 20 {
		e.flattenAttempts++
	}
	require.Equal(t, flattenBackoffCap, e.flattenBackoff())
	_ = clk
}

// Закрытая позиция обнуляет отсчёт: следующий эпизод начинается с быстрой
// попытки, а не с накопленной паузы.
func TestClosedPositionResetsTheAttemptCounter(t *testing.T) {
	ctx := context.Background()
	e, _, clk := dangerEngine(t)

	e.handle(ctx, balanceEvent("10000", "10000", "0"))
	e.handle(ctx, positionEvent("0.03"))
	e.flatten(ctx)
	clk.advance(e.health.AckTimeout + 100*time.Millisecond)
	e.flatten(ctx)
	require.Positive(t, e.flattenAttempts)

	e.handle(ctx, positionEvent("0"))
	e.flatten(ctx)
	require.Zero(t, e.flattenAttempts)
	require.Equal(t, e.health.AckTimeout, e.flattenBackoff())
}

// Отказ «reduce only превышает позицию» означает закрытую позицию,
// а не оставшуюся открытой.
func TestReduceOnlyOverflowIsNotAnAlarm(t *testing.T) {
	for _, reason := range []string{
		"reduce only order size exceeds position size",
		"REDUCE ONLY ORDER SIZE EXCEEDS POSITION SIZE",
	} {
		require.True(t, closedAlready(rejectEvent("x", reason)), reason)
	}
	for _, reason := range []string{"balance not enough", "post only would take liquidity", ""} {
		require.False(t, closedAlready(rejectEvent("x", reason)), reason)
	}
}

// Учёт отправленного объёма освобождается терминальным ответом.
func TestOutstandingFlattenTracksTheSentQuantity(t *testing.T) {
	ctx := context.Background()
	e, _, clk := dangerEngine(t)

	e.handle(ctx, balanceEvent("10000", "10000", "0"))
	e.handle(ctx, positionEvent("0.03"))
	e.flatten(ctx)

	require.Equal(t, "0.03", e.outstandingFlatten(clk.now).String())
	clk.advance(flattenInFlight + time.Second)
	require.True(t, e.outstandingFlatten(clk.now).IsZero(),
		"a close the venue never answered stops occupying the accounting")
}
