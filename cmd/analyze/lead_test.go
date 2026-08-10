package main

import (
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

var epoch = time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)

func at(ms int) time.Time { return epoch.Add(time.Duration(ms) * time.Millisecond) }

// Перенос последнего значения вперёд, а не интерполяция: интерполяция выдумала
// бы цену между наблюдениями и перенесла часть движения в момент до того, как
// оно произошло, — то есть подделала бы ровно ту величину, которую мы меряем.
func TestResampleCarriesTheLastValueForward(t *testing.T) {
	got := resample([]sample{
		{at: at(0), price: 100},
		{at: at(250), price: 101},
		{at: at(900), price: 105},
	}, epoch, 250*time.Millisecond, 5)

	require.Equal(t, []float64{100, 101, 101, 101, 105}, got)
}

// Точки сетки до первого наблюдения — не ноль и не плоскость, а отсутствие
// данных: плоскость означала бы нулевую доходность и разбавила бы корреляцию.
func TestResampleLeavesGapsBeforeTheFirstSample(t *testing.T) {
	got := resample([]sample{{at: at(500), price: 100}}, epoch, 250*time.Millisecond, 3)

	require.True(t, math.IsNaN(got[0]))
	require.True(t, math.IsNaN(got[1]))
	require.Equal(t, 100.0, got[2])
}

func TestLogReturnsAreInBasisPoints(t *testing.T) {
	got := logReturns([]float64{100, 100.1, 100.1})

	require.Len(t, got, 2)
	require.InDelta(t, 10.0, got[0], 0.01, "a 0.1% move is about 10 bps")
	require.Equal(t, 0.0, got[1])
}

func TestCorrelateIgnoresGaps(t *testing.T) {
	leader := []float64{1, math.NaN(), 3, 4}
	follower := []float64{1, 2, 3, 4}

	corr, pairs := correlate(leader, follower, 0)

	require.Equal(t, 3, pairs)
	require.InDelta(t, 1.0, corr, 1e-9)
}

// Ровный ряд не коррелирует ни с чем: нулевая дисперсия — это отсутствие
// наблюдения, а не идеальное совпадение.
func TestCorrelateRefusesAFlatSeries(t *testing.T) {
	corr, _ := correlate([]float64{1, 1, 1, 1}, []float64{1, 2, 3, 4}, 0)

	require.True(t, math.IsNaN(corr))
}

// Главный тест файла: на синтетическом ряду, где mark — это опорная цена,
// сдвинутая на известную величину, анализ обязан назвать именно эту величину.
func TestLeadLagRecoversAKnownShift(t *testing.T) {
	const step = 250 * time.Millisecond
	const shift = 4 // такта сетки, то есть одна секунда

	// Ступенчатая траектория: движение случается в известные моменты, между
	// ними цена стоит. Так проверяется именно сдвиг, а не гладкость.
	moves := []float64{0, 3, -2, 5, 0, -4, 1, 6, -3, 0, 2, -5, 4, 0, -1, 3, -2, 0, 5, -4}
	var refs, marks []sample
	refPrice, markPrice := 100.0, 100.0
	for i := range 60 {
		move := moves[i%len(moves)]
		refPrice *= 1 + move/10_000
		refs = append(refs, sample{at: at(i * int(step/time.Millisecond)), price: refPrice})

		if i >= shift {
			markPrice *= 1 + moves[(i-shift)%len(moves)]/10_000
		}
		marks = append(marks, sample{at: at(i * int(step/time.Millisecond)), price: markPrice})
	}

	result, ok := leadLag(refs, marks, leadOptions{grid: step, maxLag: 3 * time.Second})

	require.True(t, ok)
	require.Equal(t, time.Second, result.bestLag, "the reference was built to lead by one second")
	require.Greater(t, result.bestCorr, 0.9)
	require.Greater(t, result.bestCorr, result.atZero,
		"the peak has to beat the unshifted baseline, or it is the same tick seen twice")
}

// Поиск в обе стороны не декоративен: односторонний нашёл бы «опережение»
// в любых данных, потому что максимум у него всегда где-то есть.
func TestLeadLagReportsWhenTheMarkMovesFirst(t *testing.T) {
	const step = 250 * time.Millisecond
	moves := []float64{0, 3, -2, 5, 0, -4, 1, 6, -3, 0, 2, -5, 4, 0, -1, 3, -2, 0, 5, -4}

	var refs, marks []sample
	refPrice, markPrice := 100.0, 100.0
	for i := range 60 {
		markPrice *= 1 + moves[i%len(moves)]/10_000
		marks = append(marks, sample{at: at(i * int(step/time.Millisecond)), price: markPrice})

		if i >= 4 {
			refPrice *= 1 + moves[(i-4)%len(moves)]/10_000
		}
		refs = append(refs, sample{at: at(i * int(step/time.Millisecond)), price: refPrice})
	}

	result, ok := leadLag(refs, marks, leadOptions{grid: step, maxLag: 3 * time.Second})

	require.True(t, ok)
	require.Equal(t, -time.Second, result.bestLag,
		"a lagging reference must be reported as lagging, not dressed up as a lead")
}

func TestLeadLagNeedsOverlappingData(t *testing.T) {
	refs := []sample{{at: at(0), price: 100}, {at: at(1000), price: 101}}
	marks := []sample{{at: at(9000), price: 100}, {at: at(10000), price: 101}}

	_, ok := leadLag(refs, marks, leadOptions{grid: 250 * time.Millisecond, maxLag: time.Second})

	require.False(t, ok)
}
