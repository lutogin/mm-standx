package uptime

import (
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"

	"github.com/nd/mm-standx/internal/domain/reward"
)

func program() reward.Program {
	return reward.Program{
		Tiers:                []reward.Tier{{ThresholdPct: 70, Multiplier: 1.0}, {ThresholdPct: 50, Multiplier: 0.5}},
		UnitSize:             decimal.NewFromInt(1500),
		PerSideCap:           decimal.NewFromInt(3000),
		MaxMakerHoursPerHour: 2,
	}
}

// hourStart is a UTC hour boundary, which is the unit the programme scores in.
func hourStart() time.Time { return time.Date(2026, 8, 3, 10, 0, 0, 0, time.UTC) }

// fill samples the tracker every second for the given span, quoting `size`
// during the first `quotingFor` and nothing afterwards.
func fill(tr *Tracker, span, quotingFor time.Duration, size int64) time.Time {
	at := hourStart()
	for elapsed := time.Duration(0); elapsed < span; elapsed += time.Second {
		at = hourStart().Add(elapsed)
		value := decimal.Zero
		if elapsed < quotingFor {
			value = decimal.NewFromInt(size)
		}
		tr.Sample(at, value)
	}
	return at
}

func TestFreshHourStartsEarning(t *testing.T) {
	tr := New(program(), 78)

	s := tr.Assess(hourStart())
	require.Equal(t, Earning, s.Mode)
	require.Equal(t, time.Hour, s.DowntimeBudget)
}

// The credit is a step function, so once the target is secured further presence
// adds nothing and the sensible move is to back away from the market.
func TestBankedModeAfterTargetIsSecured(t *testing.T) {
	tr := New(program(), 78)

	// Quote continuously through 80% of the hour.
	at := fill(tr, 48*time.Minute, 48*time.Minute, 1500)

	s := tr.Assess(at)
	require.Equal(t, Banked, s.Mode)
	require.Contains(t, s.Reason, "adds nothing")
	require.InDelta(t, 1.0, s.MakerHours, 1e-9)
}

// If even flawless quoting for the rest of the hour cannot reach the lowest
// tier, continuing to quote pays for fills and earns nothing in return.
func TestLostHourStopsTakingRisk(t *testing.T) {
	tr := New(program(), 78)

	// Nothing quoted for the first 55 minutes: at best 5 of 60 minutes remain,
	// far below the 50% floor.
	at := fill(tr, 55*time.Minute, 0, 1500)

	s := tr.Assess(at)
	require.Equal(t, Lost, s.Mode)
	require.Contains(t, s.Reason, "unreachable")
	require.Zero(t, s.MakerHours)
}

// A slow start is still recoverable, and the tracker must not give up early.
func TestSlowStartRemainsRecoverable(t *testing.T) {
	tr := New(program(), 78)

	// Ten idle minutes leave fifty available: enough for both tiers.
	at := fill(tr, 10*time.Minute, 0, 1500)

	s := tr.Assess(at)
	require.Equal(t, Earning, s.Mode)
	require.Greater(t, s.BestPossiblePct, 70.0)
}

// The free downtime is the budget the volatility breaker spends. It must shrink
// as the hour is consumed, otherwise the breaker would overspend it.
func TestDowntimeBudgetShrinksAsTheHourIsSpent(t *testing.T) {
	tr := New(program(), 78)

	early := fill(tr, 5*time.Minute, 5*time.Minute, 1500)
	earlyBudget := tr.Assess(early).DowntimeBudget

	late := fill(tr, 30*time.Minute, 20*time.Minute, 1500)
	lateBudget := tr.Assess(late).DowntimeBudget

	require.Less(t, lateBudget, earlyBudget, "spent downtime must reduce the remaining budget")
	require.Positive(t, lateBudget)
}

// Gaps in presence are what the zero samples represent; without them the metric
// would be blind to downtime entirely.
func TestZeroSamplesCountAgainstUptime(t *testing.T) {
	tr := New(program(), 78)

	at := fill(tr, 20*time.Minute, 10*time.Minute, 1500)

	s := tr.Assess(at)
	require.InDelta(t, 50.0, s.QualifiedPct, 0.5)
}

// Scoring is per calendar hour, so crossing the boundary must start fresh
// rather than carry the previous hour's history forward.
func TestHourBoundaryResetsTheWindow(t *testing.T) {
	tr := New(program(), 78)

	fill(tr, 30*time.Minute, 30*time.Minute, 1500)
	require.Positive(t, tr.Samples())

	next := hourStart().Add(time.Hour)
	tr.Sample(next, decimal.NewFromInt(1500))

	require.Equal(t, 1, tr.Samples(), "a new hour must start from scratch")
	require.InDelta(t, 0.0, tr.Assess(next).ElapsedPct, 0.1)
}

// The estimate must match what the exchange reports, since it is computed the
// same way. A divergence would mean the model is wrong.
func TestMakerHoursMatchesTheProgrammeFormula(t *testing.T) {
	tr := New(program(), 78)

	// One unit of size held for the whole hour earns exactly one maker hour.
	at := fill(tr, time.Hour, time.Hour, 1500)
	require.InDelta(t, 1.0, tr.Assess(at).MakerHours, 1e-9)
}
