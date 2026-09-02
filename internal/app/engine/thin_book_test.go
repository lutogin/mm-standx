package engine

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nd/mm-standx/internal/domain/liquidity"
	"github.com/nd/mm-standx/internal/domain/market"
	"github.com/nd/mm-standx/internal/domain/trading"
)

// Тонкий стакан закрывает размещение на своей стороне и только на ней.

// primeDepth задаёт норму глубины по обеим сторонам.
func primeDepth(e *Engine, depth float64, at time.Time, n int) {
	for range n {
		e.liq.Observe(market.Buy, depth, at)
		e.liq.Observe(market.Sell, depth, at)
		at = at.Add(time.Second)
	}
}

func thinEngine(t *testing.T) (*Engine, *recordingGateway, *testClock) {
	t.Helper()
	e, gw, clk := dangerEngine(t)
	e.liq = liquidity.New(liquidity.Config{
		Window: 30 * time.Minute, MinSamples: 40, ThinFraction: 0.70,
	})
	return e, gw, clk
}

func placedOn(gw *recordingGateway, side market.Side) int {
	n := 0
	for _, r := range gw.placed {
		if r.Side == side && r.Type == trading.Limit {
			n++
		}
	}
	return n
}

// Обвал глубины на одной стороне не даёт выставить туда новый ордер,
// а вторая сторона продолжает работать.
func TestThinSideStopsPlacingWhileTheOtherKeepsQuoting(t *testing.T) {
	ctx := context.Background()
	e, _, clk := thinEngine(t)
	e.handle(ctx, balanceEvent("10000", "10000", "0"))
	primeVolatility(t, e, clk)
	primeDepth(e, 6_000_000, clk.now.Add(-2*time.Minute), 60)

	// Аск обвалился до 0.23 нормы — ровно эпизод 22:00:58.
	e.liq.Observe(market.Sell, 1_380_000, clk.now)
	e.liq.Observe(market.Buy, 6_000_000, clk.now)

	require.True(t, e.thinBook(market.Sell, clk.now))
	require.False(t, e.thinBook(market.Buy, clk.now))
}

// Обычный стакан ничего не запрещает.
func TestNormalBookPlacesOnBothSides(t *testing.T) {
	ctx := context.Background()
	e, gw, clk := thinEngine(t)
	e.handle(ctx, balanceEvent("10000", "10000", "0"))
	primeVolatility(t, e, clk)
	// Норма берётся из того же стакана, что увидит evaluate: иначе тест
	// объявляет тонким собственный стенд.
	bid, ask := e.bookDepth()
	at := clk.now.Add(-2 * time.Minute)
	for range 60 {
		e.liq.Observe(market.Buy, bid, at)
		e.liq.Observe(market.Sell, ask, at)
		at = at.Add(time.Second)
	}

	e.evaluate(ctx, true)
	require.Positive(t, placedOn(gw, market.Buy)+placedOn(gw, market.Sell),
		"a normal book must not be gated")
	require.Equal(t, "no", e.thinSummary())
}

// Запрет касается только новых ордеров: уже стоящую ступень он не снимает.
func TestThinBookDoesNotCancelRestingRungs(t *testing.T) {
	ctx := context.Background()
	e, gw, clk := thinEngine(t)
	e.handle(ctx, balanceEvent("10000", "10000", "0"))
	primeVolatility(t, e, clk)
	primeDepth(e, 6_000_000, clk.now.Add(-2*time.Minute), 60)

	liveRung(e, market.Sell, "ask-0", "4280", clk.now)
	e.liq.Observe(market.Sell, 500_000, clk.now)

	gw.cancelled = nil
	e.evaluate(ctx, true)
	require.NotContains(t, gw.cancelled, trading.ClOrdID("ask-0"),
		"a rung that already earns must not be pulled for a thin book alone")
}

// Сводка обязана показывать запрет: не котирующая сторона иначе выглядит
// в логе так же, как сторона, которой просто нечего делать.
func TestThinSummaryNamesTheClosedSide(t *testing.T) {
	e, _, clk := thinEngine(t)
	primeDepth(e, 6_000_000, clk.now.Add(-2*time.Minute), 60)

	e.liq.Observe(market.Sell, 500_000, clk.now)
	e.thinBook(market.Sell, clk.now)
	require.Equal(t, "ask", e.thinSummary())

	e.liq.Observe(market.Buy, 500_000, clk.now)
	e.thinBook(market.Buy, clk.now)
	require.Equal(t, "both", e.thinSummary())
}

// Незрелое окно не блокирует: молчание о стакане не повод уходить из него.
func TestGateStaysOpenUntilTheWindowIsMature(t *testing.T) {
	e, _, clk := thinEngine(t)
	e.liq.Observe(market.Sell, 1, clk.now)
	require.False(t, e.thinBook(market.Sell, clk.now))
}
