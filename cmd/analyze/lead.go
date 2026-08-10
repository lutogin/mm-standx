package main

import (
	"math"
	"time"
)

// Lead-lag: does the external venue actually move first, and by how much?
//
// The whole case for the reference feed rests on one claim — that the flow
// reaching a resting quote is arbitrage against a move that already happened
// somewhere else. If that is true, the reference series leads the exchange mark
// by a measurable interval, and the feed buys us exactly that interval of
// warning. If it is false, the feed is an extra socket and an extra way to be
// wrong. The measurement below is what separates the two, and it has to be made
// before any of it is wired into a decision.

// leadOptions carries the analysis parameters shared by both reports.
type leadOptions struct {
	grid      time.Duration
	maxLag    time.Duration
	quoteBps  float64
	excursion time.Duration
	lookback  time.Duration
	reaction  time.Duration
}

// leadResult is the outcome of the cross-correlation.
type leadResult struct {
	// BestLag is how far the reference leads the mark. Negative means the mark
	// moved first, which would refute the premise rather than confirm it.
	bestLag  time.Duration
	bestCorr float64
	// atZero is the correlation without any shift, kept as the baseline: a peak
	// that barely beats it is not a lead, it is the same tick seen twice.
	atZero float64
	points int
	table  []lagPoint
}

type lagPoint struct {
	lag  time.Duration
	corr float64
}

// resample carries the last observed price forward onto an evenly spaced grid.
//
// Forward fill, not interpolation: interpolation would invent a price between
// two observations and place part of a move before it happened, which is
// precisely the thing being measured. Grid points before the first observation
// come back as NaN and are dropped downstream rather than treated as flat.
func resample(samples []sample, from time.Time, step time.Duration, n int) []float64 {
	out := make([]float64, n)
	for i := range out {
		out[i] = math.NaN()
	}
	if len(samples) == 0 || n == 0 || step <= 0 {
		return out
	}

	next := 0
	last := math.NaN()
	for i := range out {
		at := from.Add(time.Duration(i) * step)
		for next < len(samples) && !samples[next].at.After(at) {
			last = samples[next].price
			next++
		}
		out[i] = last
	}
	return out
}

// logReturns converts a level series into returns in basis points.
//
// Returns rather than levels because two random walks are correlated at any lag
// simply by being random walks; their increments are not.
func logReturns(level []float64) []float64 {
	if len(level) < 2 {
		return nil
	}
	out := make([]float64, len(level)-1)
	for i := 1; i < len(level); i++ {
		prev, cur := level[i-1], level[i]
		if !(prev > 0) || !(cur > 0) {
			out[i-1] = math.NaN()
			continue
		}
		out[i-1] = math.Log(cur/prev) * 10_000
	}
	return out
}

// correlate returns the Pearson correlation between the leader shifted forward
// by lag steps and the follower, along with the number of pairs used.
//
// A positive lag means the leader is compared against a later follower, that is,
// the leader is claimed to move first.
func correlate(leader, follower []float64, lag int) (float64, int) {
	var n int
	var sx, sy, sxx, syy, sxy float64

	for i := range follower {
		j := i - lag
		if j < 0 || j >= len(leader) {
			continue
		}
		x, y := leader[j], follower[i]
		if math.IsNaN(x) || math.IsNaN(y) {
			continue
		}
		n++
		sx += x
		sy += y
		sxx += x * x
		syy += y * y
		sxy += x * y
	}
	if n < 2 {
		return math.NaN(), n
	}

	cov := sxy - sx*sy/float64(n)
	vx := sxx - sx*sx/float64(n)
	vy := syy - sy*sy/float64(n)
	if vx <= 0 || vy <= 0 {
		return math.NaN(), n
	}
	return cov / math.Sqrt(vx*vy), n
}

// leadLag scans shifts in both directions and reports the one that explains the
// mark best.
//
// Both directions on purpose. A scan that only looked forward would report a
// lead no matter what the data says, because the maximum of a one-sided search
// is always somewhere.
func leadLag(refs, marks []sample, opts leadOptions) (leadResult, bool) {
	if len(refs) < 2 || len(marks) < 2 || opts.grid <= 0 {
		return leadResult{}, false
	}

	from := marks[0].at
	if refs[0].at.After(from) {
		from = refs[0].at
	}
	until := marks[len(marks)-1].at
	if refs[len(refs)-1].at.Before(until) {
		until = refs[len(refs)-1].at
	}
	if !until.After(from) {
		return leadResult{}, false
	}

	n := int(until.Sub(from)/opts.grid) + 1
	refReturns := logReturns(resample(refs, from, opts.grid, n))
	markReturns := logReturns(resample(marks, from, opts.grid, n))

	maxLag := int(opts.maxLag / opts.grid)
	result := leadResult{bestCorr: math.Inf(-1)}
	for lag := -maxLag; lag <= maxLag; lag++ {
		corr, pairs := correlate(refReturns, markReturns, lag)
		if math.IsNaN(corr) {
			continue
		}
		shift := time.Duration(lag) * opts.grid
		result.table = append(result.table, lagPoint{lag: shift, corr: corr})
		if lag == 0 {
			result.atZero = corr
		}
		if corr > result.bestCorr {
			result.bestCorr, result.bestLag, result.points = corr, shift, pairs
		}
	}
	if len(result.table) == 0 {
		return leadResult{}, false
	}
	return result, true
}
