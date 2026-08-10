package main

import (
	"fmt"
	"math"
	"os"
	"sort"
	"text/tabwriter"
	"time"
)

// touchCooldown collapses one sweep of the book into one touch event.
//
// Three seconds is the programme's own minimum resting time: below it an order
// earns nothing anyway, so two touches closer together than that cannot be two
// separate orders of ours being taken.
const touchCooldown = 3 * time.Second

// triggerCooldown keeps one move from being counted as a stream of triggers.
//
// It is the recovery time a guard would use in practice: after pulling, the bot
// stays out until the venue settles, so a second trigger inside this window
// would not have changed anything.
const triggerCooldown = 5 * time.Second

// thresholds are the reference excursions the counterfactual is run against.
//
// The range is bounded below by the noise of a liquid venue and above by the
// reward band itself: a ten basis point move has already carried the mark out
// of the band, and a guard that only fires there is a guard that fires after.
var thresholds = []float64{2, 3, 4, 6, 8, 10}

func reportReference(stats map[string]*series, opts leadOptions) {
	symbols := make([]string, 0, len(stats))
	for symbol, s := range stats {
		if len(s.refs) > 0 {
			symbols = append(symbols, symbol)
		}
	}
	if len(symbols) == 0 {
		fmt.Println("\nno reference feed in these recordings: " +
			"enable symbols.<pair>.reference and record again")
		return
	}
	sort.Strings(symbols)

	for _, symbol := range symbols {
		s := stats[symbol]
		fmt.Printf("\n=== %s vs %s ===\n", symbol, s.venue)
		fmt.Printf("reference samples: %d over %s\n",
			len(s.refs), s.lastAt.Sub(s.firstAt).Round(time.Minute))

		reportLead(s, opts)
		reportTouches(s, opts)
	}
}

func reportLead(s *series, opts leadOptions) {
	result, ok := leadLag(s.refs, s.marks, opts)
	if !ok {
		fmt.Println("\nlead-lag: not enough overlapping data")
		return
	}

	fmt.Printf("\nlead-lag (grid %s, returns in bps)\n", opts.grid)
	fmt.Printf("  best shift: %s, correlation %.3f (unshifted %.3f, %d pairs)\n",
		result.bestLag, result.bestCorr, result.atZero, result.points)

	switch {
	case result.bestLag > 0:
		fmt.Printf("  the reference moves first by about %s\n", result.bestLag)
	case result.bestLag < 0:
		fmt.Printf("  the mark moves first by about %s: the premise does not hold here\n",
			-result.bestLag)
	default:
		fmt.Println("  no measurable lead at this grid: try a finer -grid")
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "  SHIFT\tCORR")
	for _, p := range result.table {
		// The full curve is noise; the shape near the peak is the evidence.
		if p.lag < result.bestLag-2*opts.grid || p.lag > result.bestLag+2*opts.grid {
			continue
		}
		fmt.Fprintf(w, "  %s\t%.3f\n", p.lag, p.corr)
	}
	w.Flush()
}

func reportTouches(s *series, opts leadOptions) {
	span := s.lastAt.Sub(s.firstAt)
	events := touches(s.marks, s.tops, opts.quoteBps, touchCooldown)

	fmt.Printf("\ntouches of a %.1f bps quote: %d", opts.quoteBps, len(events))
	if span > 0 {
		fmt.Printf(" (%.2f/h)", float64(len(events))/span.Hours())
	}
	fmt.Println()
	if len(events) == 0 {
		fmt.Println("  nothing to explain: no touch in this recording")
		return
	}

	fmt.Printf("  a warning counts when it fires between %s and %s before the touch\n",
		opts.lookback, opts.reaction)

	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "  THRESHOLD\tWARNED\tMEDIAN LEAD\tPULLS/h\tPULLS PER TOUCH AVOIDED")
	for _, threshold := range thresholds {
		fired := triggers(s.refs, opts.excursion, threshold, triggerCooldown)
		c := assess(events, fired, threshold, span, opts.lookback, opts.reaction)

		avoided := c.warned * float64(len(events))
		cost := math.Inf(1)
		if avoided > 0 {
			cost = float64(c.count) / avoided
		}
		fmt.Fprintf(w, "  %.0f bps\t%.0f%%\t%.0f ms\t%.1f\t%s\n",
			c.thresholdBps, c.warned*100, c.leadMs, c.triggersPerHour, ratio(cost))
	}
	w.Flush()

	fmt.Println("\n  WARNED  - share of touches the reference had already flagged in time")
	fmt.Println("  PULLS/h - how often the threshold would take the book down")
	fmt.Println("  the last column is the price of the guard: pulls paid per touch avoided")
}

func ratio(v float64) string {
	if math.IsInf(v, 1) {
		return "-"
	}
	return fmt.Sprintf("%.1f", v)
}
