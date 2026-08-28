package main

import (
	"time"
)

// Sweep depth: does a ladder limit what one touch takes?
//
// A ladder costs weight and it is the mean rung weight that the programme
// scores, so three rungs at 7.0 / 7.4 / 7.8 bps count as 0.816 against 0.880
// for a single order at 7.0. The only thing that buys back is size: if the
// market stops at the near rung, a fill takes a third of the side instead of
// all of it.
//
// Whether it stops is a property of the market, not of the design, and the
// record holds only its extremes — three rungs in 51 ms on 6 August, six fills
// in 1.8 s on 18 August. Both are incident reports. What an ordinary touch does
// is what this part measures.

// sweepEpisode is one touch of the near rung together with how far the market
// carried on past it.
type sweepEpisode struct {
	at   time.Time
	side string
	// depth is how many rungs the market reached, from 1 to len(distances).
	depth int
}

// sweeps replays the recording and reports, for every touch of the near rung,
// the deepest rung the market reached within window.
//
// THE LADDER DOES NOT MOVE WHILE THE SWEEP RUNS, and the model must not either.
// Rung prices are anchored on the mark in force when the near rung was first
// reached and held for the whole window. Re-anchoring on later marks — which is
// what touches does, being the limiting case of a quote that follows the mark —
// would walk the outer rungs away from an advancing market and report a
// shallower sweep than happened. The mark ticks once every three seconds; the
// sweep of 6 August took 51 ms.
//
// The depth is an upper bound on rungs filled, for the same reason touches is
// an upper bound on fills: the market can reach a price without taking the size
// resting there, and the queue ahead of us is not observable (PLAN.md §7.2).
// The error runs the same way for every layer count, so the comparison between
// them survives it.
func sweeps(
	marks []sample,
	tops []top,
	distances []float64,
	window, cooldown time.Duration,
) []sweepEpisode {
	if len(marks) == 0 || len(tops) == 0 || len(distances) == 0 {
		return nil
	}

	var out []sweepEpisode
	// One episode in flight per side: the two sides are reached by opposite
	// moves and cannot be part of the same sweep.
	open := map[string]*sweepEpisode{}
	last := map[string]time.Time{}
	anchor := map[string]float64{}

	flush := func(side string) {
		if ep := open[side]; ep != nil {
			out = append(out, *ep)
			delete(open, side)
		}
	}

	next := 0
	mark := 0.0
	for _, t := range tops {
		for next < len(marks) && !marks[next].at.After(t.at) {
			mark = marks[next].price
			next++
		}
		if mark <= 0 {
			continue // the book arrived before the first mark
		}

		for _, side := range []string{"buy", "sell"} {
			if ep := open[side]; ep != nil && t.at.Sub(ep.at) > window {
				flush(side)
			}

			depth := reached(t, anchorOr(anchor, side, mark, open), side, distances)
			if depth == 0 {
				continue
			}
			if ep := open[side]; ep != nil {
				ep.depth = max(ep.depth, depth)
				continue
			}
			if seen, ok := last[side]; ok && t.at.Sub(seen) < cooldown {
				continue
			}
			last[side] = t.at
			anchor[side] = mark
			open[side] = &sweepEpisode{at: t.at, side: side, depth: depth}
		}
	}
	flush("buy")
	flush("sell")
	return out
}

// anchorOr returns the mark an open episode was anchored on, or the current one
// when no episode is in flight.
func anchorOr(anchor map[string]float64, side string, current float64, open map[string]*sweepEpisode) float64 {
	if open[side] != nil {
		return anchor[side]
	}
	return current
}

// reached counts how many rungs the market has passed, given the mark the
// ladder is anchored on.
//
// Our bid stands below the mark and is reached when the best ask falls to it;
// our ask stands above and is reached when the best bid rises. Distances are
// ordered from the near rung outward, so the count is the length of the prefix
// the market crossed.
func reached(t top, mark float64, side string, distances []float64) int {
	depth := 0
	for _, d := range distances {
		ratio := d / 10_000
		if side == "buy" && t.ask <= mark*(1-ratio) {
			depth++
			continue
		}
		if side == "sell" && t.bid >= mark*(1+ratio) {
			depth++
			continue
		}
		break
	}
	return depth
}

// ladderDistances places layers rungs spacing apart, the near one at near.
//
// Ladder geometry mirrors policy.Ladder.Anchor: the risk estimate answers where
// the nearest order may stand, and the rest of the ladder is built outward from
// there. Centring on it instead would push the near rung inside what the
// estimate allows.
func ladderDistances(near, spacing float64, layers int) []float64 {
	out := make([]float64, layers)
	for i := range out {
		out[i] = near + float64(i)*spacing
	}
	return out
}
