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

	require.True(t, e.thinSides(clk.now)[market.Sell])
	require.False(t, e.thinSides(clk.now)[market.Buy])
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

// Обвал глубины снимает уже стоящую сторону.
//
// Замер 2 сентября 2026: вердикт по глубине опередил исполнение на 405 мс,
// но ордер стоял с 09:05:19, под запрет на размещение не подпадал и дождался
// своей сделки. Круг запроса на снятие — десятки миллисекунд, запаса хватало
// с избытком.
func TestThinBookPullsRestingRungs(t *testing.T) {
	ctx := context.Background()
	e, gw, clk := thinEngine(t)
	e.cfg.Liquidity.PullOnThin = true
	e.handle(ctx, balanceEvent("10000", "10000", "0"))
	primeVolatility(t, e, clk)
	primeDepth(e, 6_000_000, clk.now.Add(-2*time.Minute), 60)

	liveRung(e, market.Sell, "ask-0", "4280", clk.now)
	e.liq.Observe(market.Sell, 500_000, clk.now)

	gw.cancelled = nil
	e.evaluate(ctx, true)
	require.Contains(t, gw.cancelled, trading.ClOrdID("ask-0"),
		"the side must leave the book the collapse is happening in")
}

// Выключенный съём оставляет только запрет на размещение.
func TestPullOnThinCanBeTurnedOff(t *testing.T) {
	ctx := context.Background()
	e, gw, clk := thinEngine(t)
	e.cfg.Liquidity.PullOnThin = false
	e.handle(ctx, balanceEvent("10000", "10000", "0"))
	primeVolatility(t, e, clk)
	primeDepth(e, 6_000_000, clk.now.Add(-2*time.Minute), 60)

	liveRung(e, market.Sell, "ask-0", "4280", clk.now)
	e.liq.Observe(market.Sell, 500_000, clk.now)

	gw.cancelled = nil
	e.evaluate(ctx, true)
	require.NotContains(t, gw.cancelled, trading.ClOrdID("ask-0"))
}

// Тонкий аск не трогает бид: глубина — свойство стороны.
func TestThinPullLeavesTheHealthySideAlone(t *testing.T) {
	ctx := context.Background()
	e, gw, clk := thinEngine(t)
	e.cfg.Liquidity.PullOnThin = true
	e.handle(ctx, balanceEvent("10000", "10000", "0"))
	primeVolatility(t, e, clk)
	primeDepth(e, 6_000_000, clk.now.Add(-2*time.Minute), 60)

	liveRung(e, market.Sell, "ask-0", "4280", clk.now)
	liveRung(e, market.Buy, "bid-0", "4270", clk.now)
	e.liq.Observe(market.Sell, 500_000, clk.now)
	e.liq.Observe(market.Buy, 6_100_000, clk.now)

	// Через pullThin напрямую: обычный путь планирования может снять бид
	// по своим причинам, и тогда тест мерил бы не то, что проверяет.
	gw.cancelled = nil
	require.True(t, e.pullThin(ctx, e.thinSides(clk.now), clk.now))
	require.Contains(t, gw.cancelled, trading.ClOrdID("ask-0"))
	require.NotContains(t, gw.cancelled, trading.ClOrdID("bid-0"))
}

// Сводка обязана показывать запрет: не котирующая сторона иначе выглядит
// в логе так же, как сторона, которой просто нечего делать.
func TestThinSummaryNamesTheClosedSide(t *testing.T) {
	e, _, clk := thinEngine(t)
	primeDepth(e, 6_000_000, clk.now.Add(-2*time.Minute), 60)

	e.liq.Observe(market.Sell, 500_000, clk.now)
	e.thinSides(clk.now)
	require.Equal(t, "ask", e.thinSummary())

	e.liq.Observe(market.Buy, 500_000, clk.now)
	e.thinSides(clk.now)
	require.Equal(t, "both", e.thinSummary())
}

// Незрелое окно не блокирует: молчание о стакане не повод уходить из него.
func TestGateStaysOpenUntilTheWindowIsMature(t *testing.T) {
	e, _, clk := thinEngine(t)
	e.liq.Observe(market.Sell, 1, clk.now)
	require.False(t, e.thinSides(clk.now)[market.Sell])
}
