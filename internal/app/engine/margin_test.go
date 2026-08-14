package engine

import (
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"

	"github.com/nd/mm-standx/internal/domain/market"
)

// Доля блокировки измеряется у биржи, а не берётся из файла.
//
// Настроенное число говорит, во что мы верим, и стареет молча. 10.542% сняты
// 12 августа при плече 9.49; после того как плечо стало 40, биржа держит
// 2.667%, и размер, посчитанный по старому числу, вчетверо ниже того, что
// позволяет капитал. Расхождение было видно только в сводке и ни на что
// не влияло.

func TestMarginRateIsMeasuredWhenThereIsSomethingToMeasure(t *testing.T) {
	e, _, clk := dangerEngine(t)
	e.cfg.SymbolRisk.Leverage = 40
	e.cfg.SymbolRisk.InitialMarginPct = 10.542

	liveRung(e, market.Buy, "bid-1", "4000", clk.now)
	liveRung(e, market.Sell, "ask-1", "4000", clk.now)
	// 0.008 по 4000 на каждой стороне — 64 USD нотионала.
	// Биржа держит под ними 1.707 USD, то есть 2.667%.
	e.locked = decimal.NewFromFloat(1.7069)

	rate, source := e.marginRate()
	require.InDelta(t, 2.667, rate, 0.01)
	require.Equal(t, "measured", source)
}

// До первых живых ордеров мерить нечего, и берётся настроенное — оно
// консервативнее, а это верный перекос для величины, задающей размер.
func TestMarginRateFallsBackToTheConfiguredValue(t *testing.T) {
	e, _, _ := dangerEngine(t)
	e.cfg.SymbolRisk.Leverage = 40
	e.cfg.SymbolRisk.InitialMarginPct = 10.542

	rate, source := e.marginRate()
	require.Equal(t, 10.542, rate)
	require.Equal(t, "configured", source)
}

// Главная защита. Испорченное измерение не имеет права раздуть размер сильнее,
// чем физически разрешает плечо: initial margin не бывает ниже 1/плечо.
func TestABrokenMeasurementCannotOversizeBeyondTheLeverageFloor(t *testing.T) {
	e, _, clk := dangerEngine(t)
	e.cfg.SymbolRisk.Leverage = 40
	e.cfg.SymbolRisk.InitialMarginPct = 10.542

	liveRung(e, market.Buy, "bid-1", "4000", clk.now)
	liveRung(e, market.Sell, "ask-1", "4000", clk.now)
	// Биржа сообщила смехотворную блокировку — например, снимок баланса
	// опередил подтверждение ордеров.
	e.locked = decimal.NewFromFloat(0.001)

	rate, source := e.marginRate()
	require.Equal(t, 2.5, rate, "1/40 is the floor the exchange cannot go below")
	require.Contains(t, source, "clamped")
}

// Позиция тоже держит маржу, поэтому во время залива отношение уезжает вверх.
// Это безопасная сторона: завышенная доля даёт меньший размер, а не больший.
func TestAnInflatedMeasurementIsAcceptedAsIs(t *testing.T) {
	e, _, clk := dangerEngine(t)
	e.cfg.SymbolRisk.Leverage = 40
	e.cfg.SymbolRisk.InitialMarginPct = 10.542

	liveRung(e, market.Buy, "bid-1", "4000", clk.now)
	liveRung(e, market.Sell, "ask-1", "4000", clk.now)
	e.locked = decimal.NewFromFloat(12.8) // 20% от 64 USD

	rate, source := e.marginRate()
	require.InDelta(t, 20.0, rate, 0.01)
	require.Equal(t, "measured", source)
}
