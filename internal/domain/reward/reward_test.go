package reward

import (
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"

	"github.com/nd/mm-standx/internal/domain/money"
)

// midWeek is a Thursday noon: an hour when every reference market is open, so
// the off-hours multiplier plays no part in the accrual tests.
var midWeek = time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)

func liveCurve() Curve {
	return Curve{BandBps: 10, MaxWeightPct: 200, MinWeightPct: 40}
}

// silverProgram mirrors the published XAG-USD parameters.
func silverProgram() Program {
	return Program{
		Curve:                liveCurve(),
		Tiers:                []Tier{{ThresholdPct: 70, Multiplier: 1.0}, {ThresholdPct: 50, Multiplier: 0.5}},
		UnitSize:             decimal.NewFromInt(1500),
		PerSideCap:           decimal.NewFromInt(3000),
		MaxMakerHoursPerHour: 2,
	}
}

func TestWeightAtCurveEndpoints(t *testing.T) {
	c := liveCurve()

	require.InDelta(t, 2.0, c.Weight(0), 1e-9, "at the mark the order counts double")
	require.InDelta(t, 0.4, c.Weight(10), 1e-9, "at the band edge only the floor remains")
	require.InDelta(t, 1.2, c.Weight(5), 1e-9, "the curve is linear between the endpoints")
}

// Outside the band the weight drops to zero rather than decaying. That cliff is
// what makes maxKeepBps a hard ceiling rather than a preference.
func TestWeightOutsideBandIsZero(t *testing.T) {
	c := liveCurve()

	require.Zero(t, c.Weight(10.01))
	require.Zero(t, c.Weight(50))
}

// A crossing order is priced as if it stood at the mark. It should never exist,
// but the estimator must not produce a nonsensical number if it does.
func TestWeightClampsCrossingOrders(t *testing.T) {
	c := liveCurve()
	require.Equal(t, c.Weight(0), c.Weight(-5))
}

func TestWeightDecreasesWithDistance(t *testing.T) {
	c := liveCurve()

	rapid.Check(t, func(t *rapid.T) {
		near := rapid.Float64Range(0, 10).Draw(t, "near")
		far := rapid.Float64Range(near, 10).Draw(t, "far")

		require.GreaterOrEqual(t, c.Weight(money.Bps(near))+1e-12, c.Weight(money.Bps(far)),
			"a closer quote must never weigh less")
	})
}

func TestSideQtyRespectsCap(t *testing.T) {
	p := silverProgram()

	// 3000 units at the band edge scale to 1200, well below the cap.
	modest := p.SideQty([]Order{{DistanceBps: 10, Qty: money.MustParseQty("3000")}})
	require.Equal(t, "1200", modest.String())

	// 3000 units at the mark scale to 6000 and get clipped to the cap.
	oversized := p.SideQty([]Order{{DistanceBps: 0, Qty: money.MustParseQty("3000")}})
	require.Equal(t, "3000", oversized.String(), "size beyond the cap is ignored")
}

func TestSideQtySumsLadderRungs(t *testing.T) {
	p := silverProgram()

	total := p.SideQty([]Order{
		{DistanceBps: 6.5, Qty: money.MustParseQty("100")},
		{DistanceBps: 7.5, Qty: money.MustParseQty("100")},
		{DistanceBps: 8.5, Qty: money.MustParseQty("100")},
	})
	// Rung weights are 96%, 80% and 64%, so 100 units each contribute 240.
	require.InDelta(t, 240.0, total.InexactFloat64(), 1e-6)
}

// Only the weaker side counts, so one-sided quoting earns nothing and skewing
// size between sides is wasted capital.
func TestSampleTakesTheWeakerSide(t *testing.T) {
	p := silverProgram()

	bid := []Order{{DistanceBps: 5, Qty: money.MustParseQty("1000")}}
	ask := []Order{{DistanceBps: 5, Qty: money.MustParseQty("400")}}

	require.Equal(t, p.SideQty(ask).String(), p.Sample(bid, ask).String())
	require.True(t, p.Sample(bid, nil).IsZero(), "one-sided quoting does not qualify")
}

// This is the central insight of the whole strategy: the reward is a step
// function of uptime, not a linear one. Quoting the entire hour earns exactly
// as much as quoting seventy percent of it.
func TestMakerHoursIsAStepFunctionOfUptime(t *testing.T) {
	p := silverProgram()
	const size = 1500 // exactly one unit

	uptimeSamples := func(pct int) []decimal.Decimal {
		out := make([]decimal.Decimal, 100)
		for i := range out {
			if i < pct {
				out[i] = decimal.NewFromInt(size)
			} else {
				out[i] = decimal.Zero
			}
		}
		return out
	}

	full := p.MakerHours(uptimeSamples(100), midWeek)
	require.InDelta(t, 1.0, full, 1e-9, "continuous quoting earns one maker hour")

	require.InDelta(t, full, p.MakerHours(uptimeSamples(75), midWeek), 1e-9,
		"seventy-five percent uptime earns the same as one hundred")

	require.InDelta(t, 0.5, p.MakerHours(uptimeSamples(60), midWeek), 1e-9,
		"between the tiers only half is credited")

	require.Zero(t, p.MakerHours(uptimeSamples(40), midWeek),
		"below the lowest tier nothing is credited")
}

// The free downtime this implies is the budget the volatility breaker spends.
func TestDowntimeBudgetIsRealSlack(t *testing.T) {
	p := silverProgram()

	samples := make([]decimal.Decimal, 100)
	for i := range samples {
		if i < 78 {
			samples[i] = decimal.NewFromInt(1500)
		} else {
			samples[i] = decimal.Zero
		}
	}
	require.InDelta(t, 1.0, p.MakerHours(samples, midWeek), 1e-9,
		"twenty-two percent of the hour may be spent with no quotes at all")
}

func TestMakerHoursRespectsHourlyCeiling(t *testing.T) {
	p := silverProgram()

	samples := make([]decimal.Decimal, 100)
	for i := range samples {
		samples[i] = decimal.NewFromInt(1_000_000)
	}
	require.Equal(t, p.MaxMakerHoursPerHour, p.MakerHours(samples, midWeek))
}

func TestMakerHoursHandlesEmptyInput(t *testing.T) {
	require.Zero(t, silverProgram().MakerHours(nil, midWeek))
}

func TestRequiredQty(t *testing.T) {
	p := silverProgram()

	// One maker hour at 8 bps, where the weight is 72%.
	required := p.RequiredQty(1, 8)
	require.InDelta(t, 1500/0.72, required.InexactFloat64(), 1e-6)

	// The cap bounds how much size can ever be useful.
	require.InDelta(t, 3000/0.72, p.RequiredQty(100, 8).InexactFloat64(), 1e-6)

	require.True(t, p.RequiredQty(1, 20).IsZero(), "outside the band no size helps")
	require.True(t, p.RequiredQty(0, 8).IsZero())
}

// Quoting closer buys the same credit with less capital, which is the trade the
// proximity curve exists to offer.
func TestCloserQuotesNeedLessCapital(t *testing.T) {
	p := silverProgram()

	rapid.Check(t, func(t *rapid.T) {
		near := rapid.Float64Range(0, 9).Draw(t, "near")
		far := rapid.Float64Range(near, 10).Draw(t, "far")

		require.LessOrEqual(t,
			p.RequiredQty(0.5, money.Bps(near)).InexactFloat64(),
			p.RequiredQty(0.5, money.Bps(far)).InexactFloat64()+1e-9)
	})
}
