package main

import (
	"math"
	"sort"
	"time"
)

// Touch clustering: would the reference have warned us in time?
//
// A correlation says the venue leads on average. It does not say the lead is
// there at the only moments that cost money. This part answers the narrower
// question directly: take every instant the exchange book reached where our
// quote would have been standing, and ask whether the external venue had
// already moved by then — early enough to cancel.
//
// The comparison that matters is against the base rate. "Eighty percent of
// touches were preceded by a reference move" means nothing until it is set
// against how often such a move happens at all: a threshold that fires every
// twenty seconds would score the same and would keep us out of the book.
// The table below therefore reports both sides — how many touches a threshold
// would have avoided, and how many times an hour it would have pulled us out.

// touchEvent is one moment at which the market reached a hypothetical quote.
type touchEvent struct {
	at   time.Time
	side string
}

// touches replays the recording and reports when a quote standing quoteBps away
// from the mark would have been reached.
//
// The quote is re-anchored on every mark update, which is the limiting case of
// what the bot does. The count is an upper bound on fills: the market may reach
// a price without taking the whole resting size, and the queue ahead of us is
// not observable (PLAN.md §7.2). An upper bound is the right error here — the
// decision it serves is whether to spend effort avoiding these events.
//
// cooldown collapses a burst into one event. The book arrives several times per
// second, so without it a single sweep would be counted as dozens of touches
// and every rate below would be inflated by the same factor.
func touches(marks []sample, tops []top, quoteBps float64, cooldown time.Duration) []touchEvent {
	if len(marks) == 0 || len(tops) == 0 || quoteBps <= 0 {
		return nil
	}

	ratio := quoteBps / 10_000
	var out []touchEvent
	var lastBuy, lastSell time.Time

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

		// Our bid stands below the mark and is reached when the best ask falls
		// to it; our ask stands above and is reached when the best bid rises.
		if t.ask <= mark*(1-ratio) && (lastBuy.IsZero() || t.at.Sub(lastBuy) >= cooldown) {
			out = append(out, touchEvent{at: t.at, side: "buy"})
			lastBuy = t.at
		}
		if t.bid >= mark*(1+ratio) && (lastSell.IsZero() || t.at.Sub(lastSell) >= cooldown) {
			out = append(out, touchEvent{at: t.at, side: "sell"})
			lastSell = t.at
		}
	}
	return out
}

// triggers reports when the reference excursion over a trailing window first
// reaches thresholdBps.
//
// Excursion, not signed change: a move that goes out and comes back is exactly
// as dangerous to a resting quote as one that stays, and a signed measure would
// score it as nothing.
func triggers(refs []sample, window time.Duration, thresholdBps float64, cooldown time.Duration) []time.Time {
	if len(refs) == 0 || window <= 0 || thresholdBps <= 0 {
		return nil
	}

	var out []time.Time
	var last time.Time
	// A monotone deque each would be faster; the window holds seconds of data
	// and this runs offline once, so the straightforward scan stays.
	start := 0
	for i, cur := range refs {
		for refs[start].at.Before(cur.at.Add(-window)) {
			start++
		}
		lo, hi := math.Inf(1), math.Inf(-1)
		for _, s := range refs[start : i+1] {
			lo = math.Min(lo, s.price)
			hi = math.Max(hi, s.price)
		}
		if lo <= 0 {
			continue
		}
		if (hi-lo)/lo*10_000 < thresholdBps {
			continue
		}
		if last.IsZero() || cur.at.Sub(last) >= cooldown {
			out = append(out, cur.at)
			last = cur.at
		}
	}
	return out
}

// coverage is what one threshold would have bought and what it would have cost.
type coverage struct {
	thresholdBps float64
	// warned is the share of touches preceded by a trigger early enough to act.
	warned float64
	// leadMs is the median warning among those, in milliseconds.
	leadMs float64
	// triggersPerHour is the cost side: every trigger is a pull, and a pull
	// is downtime, and downtime is the only thing the programme pays for.
	triggersPerHour float64
	count           int
}

// assess measures one threshold against the recorded touches.
//
// A trigger counts only if it fired at least `reaction` before the touch:
// warning that arrives while the cancel is already too late is not warning.
func assess(
	events []touchEvent,
	fired []time.Time,
	thresholdBps float64,
	span time.Duration,
	lookback, reaction time.Duration,
) coverage {
	out := coverage{thresholdBps: thresholdBps, count: len(fired)}
	if span > 0 {
		out.triggersPerHour = float64(len(fired)) / span.Hours()
	}
	if len(events) == 0 {
		return out
	}

	var leads []float64
	covered := 0
	for _, e := range events {
		deadline := e.at.Add(-reaction)
		earliest := e.at.Add(-lookback)

		// The latest trigger at or before the deadline is the one that would
		// have acted; anything earlier is subsumed by it.
		i := sort.Search(len(fired), func(i int) bool { return fired[i].After(deadline) }) - 1
		if i < 0 || fired[i].Before(earliest) {
			continue
		}
		covered++
		leads = append(leads, float64(e.at.Sub(fired[i]).Microseconds())/1000)
	}

	out.warned = float64(covered) / float64(len(events))
	out.leadMs = pct(leads, 50)
	return out
}
