// Package uptime tracks qualifying presence and chooses the safest useful mode
// inside each UTC scoring hour.
package uptime

import (
	"fmt"
	"time"

	"github.com/shopspring/decimal"

	"github.com/nd/mm-standx/internal/domain/reward"
)

// Mode is the quoting mode for the remainder of the hour.
type Mode uint8

const (
	// Earning means that presence can still secure an economically eligible tier.
	Earning Mode = iota
	// Banked means that the configured safety target has already been secured.
	Banked
	// Lost means that no economically eligible tier remains reachable.
	Lost
)

func (m Mode) String() string {
	switch m {
	case Earning:
		return "earning"
	case Banked:
		return "banked"
	default:
		return "lost"
	}
}

// Policy controls the safety margin above the programme thresholds.
type Policy struct {
	// TargetPct is the preferred full-tier target.
	TargetPct float64
	// HardFloorPct is the lowest presence accepted as a safely secured full tier.
	HardFloorPct float64
	// ReservePct is added to lower-tier thresholds when enough time remains.
	ReservePct float64
	// MinTierMultiplier excludes tiers whose reduced accrual cannot cover risk.
	MinTierMultiplier float64
	// SampleInterval is the actor's fixed observation cadence.
	SampleInterval time.Duration
}

// Tracker accumulates the normalized sample grid for the current UTC hour.
type Tracker struct {
	program reward.Program
	policy  Policy

	hour     time.Time
	samples  []decimal.Decimal
	nonZero  int
	lastSlot int
}

// New creates an hourly tracker.
func New(program reward.Program, policy Policy) *Tracker {
	if policy.SampleInterval <= 0 {
		policy.SampleInterval = time.Second
	}
	return &Tracker{program: program, policy: policy, lastSlot: -1}
}

// Sample records one observation. Missing grid slots, including every slot
// before a mid-hour process start, are inserted as zero. This is deliberately
// pessimistic: a restart must never manufacture uptime that the exchange did
// not observe.
func (t *Tracker) Sample(now time.Time, value decimal.Decimal) {
	t.resetHour(now)
	slot := t.slot(now)
	t.fillBefore(slot)

	if slot == t.lastSlot && len(t.samples) > 0 {
		previous := t.samples[len(t.samples)-1]
		if previous.IsPositive() && !value.IsPositive() {
			t.nonZero--
		} else if !previous.IsPositive() && value.IsPositive() {
			t.nonZero++
		}
		t.samples[len(t.samples)-1] = value
		return
	}

	t.samples = append(t.samples, value)
	if value.IsPositive() {
		t.nonZero++
	}
	t.lastSlot = slot
}

func (t *Tracker) resetHour(now time.Time) {
	hour := now.UTC().Truncate(time.Hour)
	if !hour.Equal(t.hour) {
		t.hour = hour
		t.samples = t.samples[:0]
		t.nonZero = 0
		t.lastSlot = -1
	}
}

func (t *Tracker) slot(now time.Time) int {
	hour := now.UTC().Truncate(time.Hour)
	elapsed := now.UTC().Sub(hour)
	if elapsed < 0 {
		elapsed = 0
	}
	return int(elapsed / t.policy.SampleInterval)
}

func (t *Tracker) fillBefore(slot int) {
	for missing := t.lastSlot + 1; missing < slot; missing++ {
		t.samples = append(t.samples, decimal.Zero)
		t.lastSlot = missing
	}
}

// Snapshot is the current state of the scoring hour.
type Snapshot struct {
	Mode Mode
	// QualifiedPct is the share of observed grid slots with two-sided liquidity.
	QualifiedPct float64
	// ElapsedPct is the elapsed share of the UTC hour.
	ElapsedPct float64
	// BankedPct is the absolute share of the hour already observed as qualified.
	BankedPct float64
	// BestPossiblePct is the ceiling if every remaining slot qualifies.
	BestPossiblePct float64
	// DowntimeBudget is the remaining time that can be idle without losing the
	// active safety target.
	DowntimeBudget time.Duration
	// MakerHours is the conservative estimate from the normalized samples.
	MakerHours float64
	// Multiplier is the reference-market accrual factor for this hour.
	Multiplier float64
	// TierThresholdPct and TierMultiplier identify the tier currently pursued.
	TierThresholdPct float64
	TierMultiplier   float64
	// TargetPct is the safety-adjusted presence target currently pursued.
	TargetPct float64
	Reason    string
}

// Assess evaluates the hour and chooses the best economically eligible tier.
func (t *Tracker) Assess(now time.Time) Snapshot {
	// Completed slots since the last actor sample are zero. The current slot is
	// left untouched because its observation may arrive later in the same tick.
	t.resetHour(now)
	t.fillBefore(t.slot(now))

	elapsed := now.UTC().Sub(now.UTC().Truncate(time.Hour))
	elapsedPct := float64(elapsed) / float64(time.Hour) * 100
	bankedPct := float64(t.nonZero) * float64(t.policy.SampleInterval) / float64(time.Hour) * 100
	if bankedPct > elapsedPct {
		bankedPct = elapsedPct
	}

	s := Snapshot{
		ElapsedPct:      elapsedPct,
		BankedPct:       bankedPct,
		BestPossiblePct: bankedPct + (100 - elapsedPct),
		MakerHours:      t.program.MakerHours(t.samples, now),
		Multiplier:      t.program.Multiplier(now),
	}
	if len(t.samples) > 0 {
		s.QualifiedPct = float64(t.nonZero) / float64(len(t.samples)) * 100
	}

	tier, target, ok := t.bestTier(s.BestPossiblePct)
	if !ok {
		s.Mode = Lost
		s.Reason = "no economically eligible uptime tier remains reachable"
		return s
	}
	s.TierThresholdPct = tier.ThresholdPct
	s.TierMultiplier = tier.Multiplier
	s.TargetPct = target

	if s.BankedPct >= target {
		s.Mode = Banked
		s.Reason = fmt.Sprintf("%.0f%% safety target for the %.2fx tier is secured", target, tier.Multiplier)
		return s
	}

	s.Mode = Earning
	s.Reason = fmt.Sprintf("accumulating uptime for the %.2fx tier", tier.Multiplier)
	if slack := s.BestPossiblePct - target; slack > 0 {
		s.DowntimeBudget = time.Duration(slack / 100 * float64(time.Hour))
	}
	return s
}

// bestTier returns the strongest eligible reachable tier and its active safety
// target. If the preferred reserve is no longer reachable, the programme
// threshold remains the last-resort target rather than abandoning a payable
// hour early.
func (t *Tracker) bestTier(bestPossible float64) (reward.Tier, float64, bool) {
	for i, tier := range t.program.Tiers {
		if tier.Multiplier+1e-12 < t.policy.MinTierMultiplier {
			continue
		}

		desired := tier.ThresholdPct + t.policy.ReservePct
		floor := tier.ThresholdPct
		if i == 0 {
			desired = t.policy.TargetPct
			floor = t.policy.HardFloorPct
		}
		if desired > 100 {
			desired = 100
		}
		if bestPossible < floor {
			continue
		}
		if bestPossible < desired {
			desired = floor
		}
		return tier, desired, true
	}
	return reward.Tier{}, 0, false
}

// Samples returns the normalized observation count in the current hour.
func (t *Tracker) Samples() int { return len(t.samples) }
