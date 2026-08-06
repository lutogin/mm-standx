package reward

import (
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
)

// goldSchedule is the window shipped for XAU-USD: deliberately wider than the
// reference venue's real hours, because guessing "open" costs money and
// guessing "closed" costs idle time.
func goldSchedule(t *testing.T) Schedule {
	t.Helper()
	w, err := ParseWindow("Fri 20:00", "Mon 00:00")
	require.NoError(t, err)
	return Schedule{Weekly: w}
}

func TestWeeklyWindowWrapsThroughSunday(t *testing.T) {
	s := goldSchedule(t)

	open := []time.Time{
		time.Date(2026, 8, 6, 16, 0, 0, 0, time.UTC),  // Thursday afternoon
		time.Date(2026, 8, 7, 19, 59, 0, 0, time.UTC), // Friday, a minute before the close
		time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC),  // Monday, the instant it reopens
		time.Date(2026, 8, 10, 9, 0, 0, 0, time.UTC),  // Monday morning
	}
	for _, at := range open {
		require.False(t, s.Closed(at), "%s must count as open", at.Format(time.RFC1123))
	}

	closed := []time.Time{
		time.Date(2026, 8, 7, 20, 0, 0, 0, time.UTC),  // Friday, the instant it closes
		time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC),  // Saturday
		time.Date(2026, 8, 9, 23, 59, 0, 0, time.UTC), // Sunday, a minute before Monday
	}
	for _, at := range closed {
		require.True(t, s.Closed(at), "%s must count as closed", at.Format(time.RFC1123))
	}
}

func TestHolidaysCloseTheWholeDay(t *testing.T) {
	s := goldSchedule(t)
	s.Holidays = []time.Time{time.Date(2026, 12, 25, 0, 0, 0, 0, time.UTC)}

	require.True(t, s.Closed(time.Date(2026, 12, 25, 3, 0, 0, 0, time.UTC)))
	require.True(t, s.Closed(time.Date(2026, 12, 25, 23, 30, 0, 0, time.UTC)))
	require.False(t, s.Closed(time.Date(2026, 12, 24, 23, 30, 0, 0, time.UTC)))
}

func TestEmptyScheduleIsAlwaysOpen(t *testing.T) {
	var s Schedule
	require.False(t, s.Closed(time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)),
		"a 24/7 market never closes, not even on a Saturday")
}

func TestParseWindowRejectsNonsense(t *testing.T) {
	for _, bad := range [][2]string{
		{"Fri", "Mon 00:00"},
		{"Xyz 20:00", "Mon 00:00"},
		{"Fri 25:00", "Mon 00:00"},
		{"Fri 20:00", "Fri 20:00"},
	} {
		_, err := ParseWindow(bad[0], bad[1])
		require.Error(t, err, "%q → %q must be rejected", bad[0], bad[1])
	}

	empty, err := ParseWindow("", "")
	require.NoError(t, err)
	require.True(t, empty.IsZero(), "an unset window means a 24/7 market")
}

// The multiplier applies to the hour's outcome, after the tier maximum and the
// per-hour ceiling. Quoting through a closed weekend earns a tenth of a normal
// hour while carrying exactly the same fill risk.
func TestOffHoursMultiplierScalesTheHour(t *testing.T) {
	p := silverProgram()
	p.OffHours = goldSchedule(t)
	p.OffHoursMultiplier = 0.1

	samples := make([]decimal.Decimal, 100)
	for i := range samples {
		samples[i] = decimal.NewFromInt(1500)
	}

	weekday := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	saturday := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)

	require.InDelta(t, 1.0, p.MakerHours(samples, weekday), 1e-9)
	require.InDelta(t, 0.1, p.MakerHours(samples, saturday), 1e-9,
		"the closed reference market cuts the hour to a tenth")

	require.InDelta(t, 1.0, p.Multiplier(weekday), 1e-9)
	require.InDelta(t, 0.1, p.Multiplier(saturday), 1e-9)
}

// The ceiling is applied before the multiplier, matching the SIP-5A wording:
// the hour's outcome is bounded, then scaled.
func TestCeilingAppliesBeforeTheMultiplier(t *testing.T) {
	p := silverProgram()
	p.OffHours = goldSchedule(t)
	p.OffHoursMultiplier = 0.1

	samples := make([]decimal.Decimal, 100)
	for i := range samples {
		samples[i] = decimal.NewFromInt(1_000_000)
	}
	saturday := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)

	require.InDelta(t, p.MaxMakerHoursPerHour*0.1, p.MakerHours(samples, saturday), 1e-9)
}

// A 24/7 instrument must be unaffected however the multiplier field is left.
func TestCryptoProgramIgnoresTheMultiplier(t *testing.T) {
	p := silverProgram()
	p.OffHoursMultiplier = 1

	samples := make([]decimal.Decimal, 100)
	for i := range samples {
		samples[i] = decimal.NewFromInt(1500)
	}
	require.InDelta(t, 1.0,
		p.MakerHours(samples, time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)), 1e-9)
}
