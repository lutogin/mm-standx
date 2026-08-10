// Package calendar decides what posture a scheduled economic release demands.
//
// It exists because the rest of the risk model is reactive, and against a
// scheduled print reaction is not available. A cancel takes about 400 ms round
// trip (PLAN.md §2.4); a CPI or payrolls release moves gold tens of basis
// points inside the first second. An order resting 7 bps from the mark is taken
// before any signal we could act on has finished travelling. The only defence
// against an event whose time is published in advance is to not be there.
//
// The package is pure: no clock, no I/O. Time arrives as an argument.
package calendar

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Impact is the release's expected market effect as the calendar grades it.
type Impact string

const (
	ImpactHigh   Impact = "high"
	ImpactMedium Impact = "medium"
	ImpactLow    Impact = "low"
	// ImpactHoliday marks a market closure rather than a release. The reference
	// market schedule (reward.Schedule) already covers closures, so this never
	// triggers a posture, but it survives parsing so that a mistyped filter
	// fails loudly instead of silently matching nothing.
	ImpactHoliday Impact = "holiday"
)

// ParseImpact normalises the calendar's spelling.
func ParseImpact(raw string) Impact { return Impact(strings.ToLower(strings.TrimSpace(raw))) }

// Event is one scheduled release.
type Event struct {
	At       time.Time
	Title    string
	Currency string
	Impact   Impact
}

// Posture is what the calendar demands of the quoting actor.
//
// Ordered by strictness: a larger value wins when windows overlap.
type Posture uint8

const (
	// Normal means the calendar has nothing to say.
	Normal Posture = iota
	// Defensive means quoting continues at the outer edge of the reward band
	// with reduced size.
	//
	// Leaving the book entirely would be the intuitive response and it is the
	// wrong one. The programme scores a percentile of the samples, so a stretch
	// of zeros drags the whole hour down to the next tier or past it, while the
	// same stretch spent quoting far out and small keeps the hour payable and
	// still cuts the loss if the move arrives early. Defensive is never worse
	// than absence in reward terms, and the fill risk it carries is a fraction
	// of the normal distance.
	Defensive
	// Hard means out of the book.
	Hard
)

func (p Posture) String() string {
	switch p {
	case Normal:
		return "normal"
	case Defensive:
		return "defensive"
	default:
		return "hard"
	}
}

// Policy is how far around a release each posture reaches.
type Policy struct {
	DefensiveBefore time.Duration
	DefensiveAfter  time.Duration
	HardBefore      time.Duration
	HardAfter       time.Duration
}

// Validate checks that the windows nest.
func (p Policy) Validate() error {
	for name, d := range map[string]time.Duration{
		"defensiveBefore": p.DefensiveBefore,
		"defensiveAfter":  p.DefensiveAfter,
		"hardBefore":      p.HardBefore,
		"hardAfter":       p.HardAfter,
	} {
		if d < 0 {
			return fmt.Errorf("calendar: %s must not be negative, got %s", name, d)
		}
	}
	// A hard window reaching past the defensive one is not wrong so much as
	// certainly unintended: it would mean leaving the book outright where the
	// configuration says to stand back and keep quoting.
	if p.HardBefore > p.DefensiveBefore {
		return fmt.Errorf("calendar: hardBefore (%s) must not exceed defensiveBefore (%s)",
			p.HardBefore, p.DefensiveBefore)
	}
	if p.HardAfter > p.DefensiveAfter {
		return fmt.Errorf("calendar: hardAfter (%s) must not exceed defensiveAfter (%s)",
			p.HardAfter, p.DefensiveAfter)
	}
	return nil
}

// Filter selects the releases an instrument actually cares about.
type Filter struct {
	Currencies []string
	Impacts    []Impact
}

// Match reports whether the event passes the filter. An empty list accepts
// nothing: a filter that matched everything by omission would quietly blank the
// book on every low-impact number in the week.
func (f Filter) Match(e Event) bool {
	return contains(f.Currencies, strings.ToUpper(strings.TrimSpace(e.Currency))) &&
		containsImpact(f.Impacts, e.Impact)
}

// Select returns the matching events in chronological order.
func (f Filter) Select(events []Event) []Event {
	out := make([]Event, 0, len(events))
	for _, e := range events {
		if f.Match(e) {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if strings.EqualFold(strings.TrimSpace(v), want) {
			return true
		}
	}
	return false
}

func containsImpact(list []Impact, want Impact) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// Verdict is the posture in force and why.
type Verdict struct {
	Posture Posture
	Reason  string
	// Until is when the posture lifts if nothing else intervenes. Zero for
	// Normal, which has no end.
	Until time.Time
}

// Calendar answers what posture is in force, given a fixed set of releases.
//
// The zero value is a calendar with no releases and demands nothing, which is
// the correct behaviour before the first fetch succeeds: an empty calendar must
// not be mistaken for a quiet week, and the decision about whether to quote
// without one belongs to the caller, not here.
type Calendar struct {
	policy Policy
	events []Event
}

// New builds a calendar from events that are already filtered.
func New(policy Policy, events []Event) Calendar {
	sorted := append([]Event(nil), events...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].At.Before(sorted[j].At) })
	return Calendar{policy: policy, events: sorted}
}

// Events returns the releases the calendar holds, in chronological order.
func (c Calendar) Events() []Event { return c.events }

// Next returns the first release at or after the given instant.
func (c Calendar) Next(now time.Time) (Event, bool) {
	for _, e := range c.events {
		if !e.At.Before(now) {
			return e, true
		}
	}
	return Event{}, false
}

// Assess returns the strictest posture in force.
//
// Strictest wins because releases cluster: a calendar publishes Core CPI m/m,
// Core CPI y/y, CPI m/m and CPI y/y at the same instant, and the defensive
// window of one overlapping the hard window of another must not soften it.
func (c Calendar) Assess(now time.Time) Verdict {
	best := Verdict{Posture: Normal}

	for _, e := range c.events {
		candidate, ok := c.posture(e, now)
		if !ok {
			continue
		}
		if candidate.Posture > best.Posture ||
			(candidate.Posture == best.Posture && candidate.Until.After(best.Until)) {
			best = candidate
		}
	}
	return best
}

func (c Calendar) posture(e Event, now time.Time) (Verdict, bool) {
	switch {
	case within(now, e.At.Add(-c.policy.HardBefore), e.At.Add(c.policy.HardAfter)):
		return Verdict{
			Posture: Hard,
			Reason:  fmt.Sprintf("%s (%s) is releasing", e.Title, e.Currency),
			Until:   e.At.Add(c.policy.HardAfter),
		}, true
	case within(now, e.At.Add(-c.policy.DefensiveBefore), e.At.Add(c.policy.DefensiveAfter)):
		return Verdict{
			Posture: Defensive,
			Reason:  fmt.Sprintf("%s (%s) at %s", e.Title, e.Currency, e.At.UTC().Format("15:04Z")),
			Until:   e.At.Add(c.policy.DefensiveAfter),
		}, true
	}
	return Verdict{}, false
}

// within reports whether t lies in [from, to].
//
// Both ends are inclusive on purpose. The alternative leaves a single instant
// at the boundary in which the guard is not in force, and the boundary of a
// blackout is the least useful place to be exact.
func within(t, from, to time.Time) bool { return !t.Before(from) && !t.After(to) }

// Blackout returns how much hard-blackout time falls inside [from, to).
//
// The quantity the hour's downtime budget is charged against. The tracker
// counts samples, so it cannot know about downtime that has not happened yet;
// a release scheduled eleven minutes from now is certain downtime, and charging
// it in advance is the only way to notice that the hour has already been spent
// before spending fourteen more minutes of fill risk earning it.
//
// Overlapping windows are merged: four CPI lines published at the same instant
// are one blackout, not four.
func (c Calendar) Blackout(from, to time.Time) time.Duration {
	if !to.After(from) {
		return 0
	}

	var total time.Duration
	var openFrom, openTo time.Time

	flush := func() {
		if openTo.After(openFrom) {
			total += openTo.Sub(openFrom)
		}
		openFrom, openTo = time.Time{}, time.Time{}
	}

	// Events are sorted and every window has the same shape, so the starts
	// arrive in order and one pass merges them.
	for _, e := range c.events {
		start, end := clip(e.At.Add(-c.policy.HardBefore), e.At.Add(c.policy.HardAfter), from, to)
		if !end.After(start) {
			continue
		}
		if openTo.IsZero() || start.After(openTo) {
			flush()
			openFrom, openTo = start, end
			continue
		}
		if end.After(openTo) {
			openTo = end
		}
	}
	flush()
	return total
}

func clip(start, end, from, to time.Time) (time.Time, time.Time) {
	if start.Before(from) {
		start = from
	}
	if end.After(to) {
		end = to
	}
	return start, end
}
