package reward

import (
	"fmt"
	"strings"
	"time"
)

// Schedule describes when the reference market behind a synthetic instrument
// is shut.
//
// StandX pays a reduced multiplier for those hours: gold, silver, oil and the
// equities track venues that close for the weekend, and an hour of presence
// while they are shut is worth a tenth of a normal one. The fill risk over the
// same hour does not shrink at all, so the difference decides whether quoting
// makes sense — it is not a reporting detail.
//
// The zero value describes a 24/7 market and is never closed.
type Schedule struct {
	// Weekly is the recurring shutdown window. A zero window means none.
	Weekly Window
	// Holidays are whole UTC days on which the reference market is shut.
	//
	// The exchange publishes no machine-readable calendar, so the list is
	// maintained by hand. An empty list is not an assertion that there are no
	// holidays — it means nobody has entered them yet.
	Holidays []time.Time
}

// Window is a recurring weekly interval, closed at the start and open at the
// end, expressed in UTC.
//
// The interval is allowed to wrap around the end of the week, which is the
// normal case: the reference markets close on Friday evening and reopen on
// Sunday night.
type Window struct {
	From WeekMinute
	To   WeekMinute
	set  bool
}

// WeekMinute counts minutes from Monday 00:00 UTC.
type WeekMinute int

const minutesPerWeek WeekMinute = 7 * 24 * 60

// NewWindow builds a window from two week positions.
func NewWindow(from, to WeekMinute) Window {
	return Window{From: from, To: to, set: true}
}

// IsZero reports that no window was configured.
func (w Window) IsZero() bool { return !w.set }

// Contains reports whether the instant falls inside the window.
func (w Window) Contains(t time.Time) bool {
	if !w.set || w.From == w.To {
		return false
	}
	at := weekMinuteOf(t)
	if w.From < w.To {
		return at >= w.From && at < w.To
	}
	// The window wraps through Sunday midnight.
	return at >= w.From || at < w.To
}

func weekMinuteOf(t time.Time) WeekMinute {
	u := t.UTC()
	// time.Weekday counts from Sunday; the week here starts on Monday so that
	// a Friday-to-Sunday window is the only one that wraps.
	day := (int(u.Weekday()) + 6) % 7
	return WeekMinute(day*24*60 + u.Hour()*60 + u.Minute())
}

// ParseWindow reads a window written as two "Fri 21:00" style bounds.
//
// The textual form is deliberate: a configuration file has to answer "when is
// this market shut" to a person reading it, and a pair of minute offsets does
// not.
func ParseWindow(from, to string) (Window, error) {
	if strings.TrimSpace(from) == "" && strings.TrimSpace(to) == "" {
		return Window{}, nil
	}
	f, err := ParseWeekMinute(from)
	if err != nil {
		return Window{}, fmt.Errorf("window start: %w", err)
	}
	t, err := ParseWeekMinute(to)
	if err != nil {
		return Window{}, fmt.Errorf("window end: %w", err)
	}
	if f == t {
		return Window{}, fmt.Errorf("window start and end are the same instant (%s)", from)
	}
	return NewWindow(f, t), nil
}

var weekdayNames = map[string]int{
	"mon": 0, "tue": 1, "wed": 2, "thu": 3, "fri": 4, "sat": 5, "sun": 6,
}

// ParseWeekMinute reads a "Fri 21:00" style position within the week, in UTC.
func ParseWeekMinute(s string) (WeekMinute, error) {
	fields := strings.Fields(strings.ToLower(strings.TrimSpace(s)))
	if len(fields) != 2 {
		return 0, fmt.Errorf("expected a weekday and a time like \"Fri 21:00\", got %q", s)
	}
	day, ok := weekdayNames[fields[0][:min(3, len(fields[0]))]]
	if !ok {
		return 0, fmt.Errorf("unknown weekday %q in %q", fields[0], s)
	}
	clock, err := time.Parse("15:04", fields[1])
	if err != nil {
		return 0, fmt.Errorf("time %q in %q: expected HH:MM", fields[1], s)
	}
	return WeekMinute(day*24*60 + clock.Hour()*60 + clock.Minute()), nil
}

// Closed reports whether the reference market is shut at the given instant.
func (s Schedule) Closed(t time.Time) bool {
	if s.Weekly.Contains(t) {
		return true
	}
	day := t.UTC().Truncate(24 * time.Hour)
	for _, h := range s.Holidays {
		if h.UTC().Truncate(24 * time.Hour).Equal(day) {
			return true
		}
	}
	return false
}
