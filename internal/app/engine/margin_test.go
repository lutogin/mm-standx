package engine

import (
	"context"
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"

	"github.com/nd/mm-standx/internal/domain/market"
	"github.com/nd/mm-standx/internal/domain/money"
	"github.com/nd/mm-standx/internal/domain/quotes"
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

// Опустевший стакан не должен обнулять знание о доле блокировки.
//
// Мерить можно только по живым ордерам, а собирать лесенку заново приходится
// ровно тогда, когда их нет: 14 августа съём по устаревшему mark случался
// каждые 67 секунд. Откат к настроенному значению давал размер вчетверо
// меньше — 6.27 против 24.9 XAU на сторону — и именно в момент возврата
// в стакан.
func TestTheLastMeasurementSurvivesAnEmptyBook(t *testing.T) {
	e, _, clk := dangerEngine(t)
	e.cfg.SymbolRisk.Leverage = 40
	e.cfg.SymbolRisk.InitialMarginPct = 10.542

	liveRung(e, market.Buy, "bid-1", "4000", clk.now)
	liveRung(e, market.Sell, "ask-1", "4000", clk.now)
	e.locked = decimal.NewFromFloat(1.7069)

	rate, source := e.marginRate()
	require.InDelta(t, 2.667, rate, 0.01)
	require.Equal(t, "measured", source)

	// Котировки сняты — мерить нечем.
	for _, l := range []*quotes.Ladder{e.bids, e.asks} {
		for i := range l.Slots {
			l.Slots[i].Phase = quotes.Empty
		}
	}
	e.locked = decimal.Zero

	rate, source = e.marginRate()
	require.InDelta(t, 2.667, rate, 0.01,
		"the rate is a property of the account, not of whether we are quoting")
	require.Equal(t, "last measured", source)
}

// До первого в жизни процесса измерения выбора нет — берётся настроенное.
func TestBeforeTheFirstMeasurementTheConfiguredValueIsUsed(t *testing.T) {
	e, _, _ := dangerEngine(t)
	e.cfg.SymbolRisk.Leverage = 40
	e.cfg.SymbolRisk.InitialMarginPct = 10.542

	rate, source := e.marginRate()
	require.Equal(t, 10.542, rate)
	require.Equal(t, "configured", source)
}

// Измерение в середине съёма запоминать нельзя.
//
// 15 августа уход на выходные снял котировки, и последнее измерение снялось
// ровно тогда, когда ордера уже отменялись, а locked ещё держался: 9.572%
// вместо 2.667%. С этим числом бот встретил бы понедельник и выставил бы втрое
// меньше нужного, выправившись только со второго цикла.
func TestAMeasurementTakenMidWithdrawalIsNotRemembered(t *testing.T) {
	e, _, clk := dangerEngine(t)
	e.cfg.SymbolRisk.Leverage = 40
	e.cfg.SymbolRisk.InitialMarginPct = 10.542

	// Сперва чистая картина: обе стороны стоят, позиция плоская.
	liveRung(e, market.Buy, "bid-1", "4000", clk.now)
	liveRung(e, market.Sell, "ask-1", "4000", clk.now)
	e.locked = decimal.NewFromFloat(1.7069)

	rate, _ := e.marginRate()
	require.InDelta(t, 2.667, rate, 0.01)

	// Теперь съём: одна сторона уже снимается, locked ещё прежний.
	for i := range e.asks.Slots {
		if e.asks.Slots[i].Phase == quotes.Live {
			e.asks.Slots[i].Phase = quotes.Cancelling
		}
	}

	require.False(t, e.cleanMeasurement(), "a side in flight is not a measurable picture")

	rate, source := e.marginRate()
	require.InDelta(t, 2.667, rate, 0.01,
		"the remembered clean rate must survive the withdrawal, not be replaced by it")
	require.Equal(t, "measured", source)
}

// Открытая позиция тоже держит маржу и попадает в locked мимо лесенки.
func TestAnOpenPositionBlocksRemembering(t *testing.T) {
	e, _, clk := dangerEngine(t)
	e.cfg.SymbolRisk.Leverage = 40
	e.cfg.SymbolRisk.InitialMarginPct = 10.542

	liveRung(e, market.Buy, "bid-1", "4000", clk.now)
	liveRung(e, market.Sell, "ask-1", "4000", clk.now)
	e.locked = decimal.NewFromFloat(1.7069)
	e.position = money.MustParseQty("0.5")

	require.False(t, e.cleanMeasurement())

	_, source := e.marginRate()
	require.Equal(t, "measured, not yet clean", source,
		"it is used for this pass and deliberately not carried forward")
}

// utilizationPct больше не константа кода, а поле конфига (internal/config.Risk):
// прежде значение (менявшееся от 70 к 95 и затем к 85 без единого коммита,
// отслеживаемого правкой файла) нельзя было ни увидеть в работающем процессе,
// ни поправить без пересборки образа — в отличие от каждого соседнего предела
// риска. Тест проверяет сквозной путь: смена risk.UtilizationPct меняет
// действительно котируемый размер.
func TestUtilizationFromConfigScalesTheQuotedSize(t *testing.T) {
	ctx := context.Background()
	e, gw, clk := dangerEngine(t)
	e.handle(ctx, balanceEvent("10000", "10000", "0"))

	e.risk.UtilizationPct = 50
	e.evaluate(ctx, true)
	require.NotEmpty(t, gw.placed)
	half := gw.placed[0].Qty
	gw.confirmQuotes(e, clk.now)
	gw.releaseQuotes(e, clk.now)

	gw.placed = nil
	e.risk.UtilizationPct = 100
	clk.advance(e.cfg.Quote.RequoteCooldown + e.cfg.Quote.MinRest)
	primeVolatility(t, e, clk)
	e.evaluate(ctx, true)
	require.NotEmpty(t, gw.placed)

	require.True(t, gw.placed[0].Qty.Gt(half),
		"doubling the configured utilization must double the usable margin budget")
}
