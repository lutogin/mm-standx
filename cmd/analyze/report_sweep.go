package main

import (
	"fmt"
	"os"
	"sort"
	"text/tabwriter"
	"time"
)

// sweepOptions are the ladder geometries the depth report is run against.
type sweepOptions struct {
	// near is where the risk estimate lets the nearest order stand.
	near float64
	// spacing separates consecutive rungs, in bps.
	spacing float64
	// maxLayers is the widest ladder compared; every count below it is
	// reported too, because the report exists to choose between them.
	maxLayers int
	// window is how long after the first touch the market is still credited
	// with the same sweep.
	window time.Duration
}

// reportSweeps compares ladder geometries on the only two axes that separate
// them: how much size each needs to reach the same accrual, and how much of
// that size one touch takes.
//
// Reward and fill cost are both linear in size, so the ratio of the two columns
// is the whole decision and the per-side cap cancels out of it. Everything is
// therefore reported relative to the single order, which needs the least size
// and risks all of it at once.
func reportSweeps(symbol string, s *series, opts sweepOptions) {
	span := s.lastAt.Sub(s.firstAt)
	if len(s.marks) == 0 || len(s.tops) == 0 {
		return
	}

	fmt.Printf("\nladder depth at %.1f bps, %.1f bps apart, sweep window %s\n",
		opts.near, opts.spacing, opts.window)

	base, ok := sizePerCap(opts.near, opts.spacing, 1)
	if !ok {
		fmt.Println("  the near rung lies outside the reward band: nothing to compare")
		return
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "  RUNGS\tTOUCHES\tTOUCHES/h\tMEAN DEPTH\tFULL SWEEPS\tSIZE\tTAKEN PER TOUCH")
	for layers := 1; layers <= opts.maxLayers; layers++ {
		distances := ladderDistances(opts.near, opts.spacing, layers)
		size, ok := sizePerCap(opts.near, opts.spacing, layers)
		if !ok {
			fmt.Fprintf(w, "  %d\tthe outer rung falls outside the reward band\n", layers)
			continue
		}

		episodes := sweeps(s.marks, s.tops, distances, opts.window, touchCooldown)
		mean, full := depthProfile(episodes, layers)

		perHour := 0.0
		if span > 0 {
			perHour = float64(len(episodes)) / span.Hours()
		}
		// One rung holds size/layers, so a sweep of `mean` rungs takes that
		// much times the depth.
		taken := size / float64(layers) * mean

		fmt.Fprintf(w, "  %d\t%d\t%.2f\t%.2f\t%.0f%%\t%.3f\t%.3f\n",
			layers, len(episodes), perHour, mean, full*100, size/base, taken/base)
	}
	w.Flush()

	fmt.Println("\n  SIZE            - side size needed for the same accrual, relative to one order")
	fmt.Println("  TAKEN PER TOUCH - size one touch removes, on the same scale")
	fmt.Println("  a ladder is worth its weight only where TAKEN falls faster than SIZE rises")
}

// sizePerCap returns the side size that saturates the per-side cap, expressed
// in units of that cap.
//
// The programme scores the sum over the rungs, each at its own weight, while
// the size is split evenly, so the side reaches `size x mean(weight)`. Hence
// the divisor is the mean weight — the same correction sizing.usefulCeiling
// carries, and for the same reason.
func sizePerCap(near, spacing float64, layers int) (float64, bool) {
	total := 0.0
	for _, d := range ladderDistances(near, spacing, layers) {
		wt := rewardWeight(d)
		if wt <= 0 {
			return 0, false // outside the band: the rung earns nothing at all
		}
		total += wt
	}
	return float64(layers) / total, true
}

// rewardWeight is the live proximity curve: 200% at the mark falling linearly
// to 40% at the edge of a ten basis point band, and nothing beyond it.
//
// Кривая проверена отчётом биржи, а не прочитана со страницы кампании: строка
// её таблицы (5 bps → 80%) отвечает другой кривой, чем приведённая там же
// формула. См. TestShippedRewardCurveFollowsTheMeasuredWeights.
func rewardWeight(distanceBps float64) float64 {
	const (
		band = 10.0
		high = 2.0
		low  = 0.4
	)
	if distanceBps > band {
		return 0
	}
	if distanceBps < 0 {
		distanceBps = 0
	}
	return low + (high-low)*(band-distanceBps)/band
}

// depthProfile reports the mean sweep depth and the share of touches that took
// the whole ladder.
//
// Both are needed. The mean is what the size comparison rests on; the share of
// full sweeps is what says whether the mean describes a distribution or hides
// two of them, which is exactly the doubt the incident record leaves.
func depthProfile(episodes []sweepEpisode, layers int) (mean, full float64) {
	if len(episodes) == 0 {
		return 0, 0
	}
	sum, complete := 0, 0
	for _, e := range episodes {
		sum += e.depth
		if e.depth >= layers {
			complete++
		}
	}
	return float64(sum) / float64(len(episodes)), float64(complete) / float64(len(episodes))
}

// reportSweepsForAll prints the depth comparison for every recorded instrument.
//
// It needs no reference feed, only the exchange's own mark and book, so it runs
// over all symbols rather than the subset the lead-lag report covers.
func reportSweepsForAll(stats map[string]*series, opts sweepOptions) {
	symbols := make([]string, 0, len(stats))
	for symbol := range stats {
		symbols = append(symbols, symbol)
	}
	sort.Strings(symbols)

	for _, symbol := range symbols {
		fmt.Printf("\n=== %s ladder geometry ===\n", symbol)
		reportSweeps(symbol, stats[symbol], opts)
	}
}
