package engine

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nd/mm-standx/internal/domain/market"
	"github.com/nd/mm-standx/internal/domain/money"
)

// Цена новой ступени считается от того ориентира, что ближе к ордеру.
//
// Замер 2 сентября 2026, 11:53:29-31: классификатор по быстрой оценке трижды
// снял продажу как «closer than target range», планировщик по официальному
// mark трижды поставил её на ту же цену 4334.9. За день таких пар 32 из 240.

func markBased(e *Engine, side market.Side, d money.Bps) money.Price {
	return e.spec.SnapPrice(market.PriceAtDistance(e.mark, side, d), side)
}

// Ориентир ушёл вверх: продажа отодвигается, покупка остаётся где была.
func TestEstimateAheadPushesTheSellFurther(t *testing.T) {
	e, _, clk := referenceEngine(t)
	clk.advance(200 * time.Millisecond)
	pushReference(e, "4400.88", clk.now) // +2 bps по ориентиру

	sell, ok := e.placementPrice(market.Sell, 7.0)
	require.True(t, ok)
	require.Equal(t, 1, sell.Cmp(markBased(e, market.Sell, 7.0)),
		"a sell must stand further than the stale mark alone would put it")

	buy, ok := e.placementPrice(market.Buy, 7.0)
	require.True(t, ok)
	require.Equal(t, 0, buy.Cmp(markBased(e, market.Buy, 7.0)),
		"an estimate moving away must never pull a quote closer")
}

// Ориентир ушёл вниз: зеркально.
func TestEstimateBehindPushesTheBuyFurther(t *testing.T) {
	e, _, clk := referenceEngine(t)
	clk.advance(200 * time.Millisecond)
	pushReference(e, "4399.12", clk.now) // -2 bps

	buy, ok := e.placementPrice(market.Buy, 7.0)
	require.True(t, ok)
	require.Equal(t, -1, buy.Cmp(markBased(e, market.Buy, 7.0)))

	sell, ok := e.placementPrice(market.Sell, 7.0)
	require.True(t, ok)
	require.Equal(t, 0, sell.Cmp(markBased(e, market.Sell, 7.0)))
}

// Ступень, которую оценка вытолкнула за край полосы, не ставится: ордер вне
// полосы не стоит ничего, и оправдать его нечем.
func TestEstimateBeyondTheBandSkipsThePlacement(t *testing.T) {
	e, _, clk := referenceEngine(t)
	clk.advance(200 * time.Millisecond)
	pushReference(e, "4404.4", clk.now) // +10 bps: дальше любой полосы

	_, ok := e.placementPrice(market.Sell, 7.0)
	require.False(t, ok)
}

// Без ориентира всё как раньше: цена от официального mark.
func TestNoEstimateFallsBackToTheMark(t *testing.T) {
	e, _, _ := dangerEngine(t)
	e.mark = money.MustParsePrice("4276")

	price, ok := e.placementPrice(market.Sell, 7.0)
	require.True(t, ok)
	require.Equal(t, 0, price.Cmp(markBased(e, market.Sell, 7.0)))
}
