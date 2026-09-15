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
	// MaxCarryGap is how long a missed observation is still described by the
	// preceding one.
	//
	// The actor's tick is not punctual, and every slot it skipped used to be
	// recorded as zero — as an absence of quotes — while the orders in question
	// were resting in the book and being scored by the exchange. Carrying the
	// last observation across a short gap states what was true; zeroing it
	// states the opposite.
	//
	// Past this gap the silence is no longer lateness but a stall, and a stall
	// must read as one. Zero disables carrying entirely.
	MaxCarryGap time.Duration
}

// Tracker accumulates the normalized sample grid for the current UTC hour.
type Tracker struct {
	program reward.Program
	policy  Policy

	hour     time.Time
	samples  []decimal.Decimal
	nonZero  int
	lastSlot int

	// version advances on every change to the grid. The cached programme
	// estimate is valid only for the version and multiplier it was taken at.
	version          uint64
	cached           bool
	cachedVersion    uint64
	cachedMultiplier float64
	cachedMakerHours float64
}

// State is the scoring hour as it stood when the process last stopped.
//
// WHY THIS HAS TO SURVIVE A RESTART. Presence is banked per UTC hour, and the
// tracker deliberately zeroes every slot before a mid-hour process start so
// that a restart cannot manufacture uptime. That rule is right about the past
// it did not observe and wrong about the past it did: the exchange samples the
// book continuously and has no idea the process was replaced. A deploy at
// minute 31 therefore left the actor believing the hour was empty while the
// exchange had already counted half of it, and Assess retired the hour as
// unreachable — see the Lost path.
//
// Measured on v1, 9 September 2026: a restart at 11:29 was followed one minute
// later by "no economically eligible uptime tier remains reachable", and the
// hour closed at 0.00x. Every deploy past the half hour costs the remainder of
// that hour outright.
type State struct {
	// Hour is the UTC hour the samples belong to. A snapshot from any other
	// hour is discarded rather than trusted.
	Hour time.Time `json:"hour"`
	// LastSlot is the last grid position that carries an observation.
	LastSlot int `json:"last_slot"`
	// Samples is the normalized grid, in order.
	Samples []decimal.Decimal `json:"samples"`
}

// Snapshot exports the current hour for persistence.
func (t *Tracker) Snapshot() State {
	out := State{Hour: t.hour, LastSlot: t.lastSlot, Samples: make([]decimal.Decimal, len(t.samples))}
	copy(out.Samples, t.samples)
	return out
}

// Restore adopts a snapshot of the hour now in progress and reports whether it
// was used.
//
// THE GAP IS ZEROED, NOT CARRIED. Between the snapshot and this call the
// process was down, and shutdown withdraws the quotes before it exits
// (Engine.Stop), so the book was empty for the whole interval. The ordinary
// fill path would carry the last observation across a gap shorter than
// MaxCarryGap, which is right for an actor that was merely late and wrong for
// one that was not running: it would claim presence that was deliberately
// removed. Restoring therefore closes the gap with zeros explicitly.
//
// A snapshot from another hour is refused. Nothing else needs checking: the
// samples describe slots the exchange did observe, and the outage that follows
// them lands in the grid as the absence it was.
func (t *Tracker) Restore(s State, now time.Time) bool {
	if len(s.Samples) == 0 || s.LastSlot < 0 {
		return false
	}
	if !s.Hour.UTC().Equal(now.UTC().Truncate(time.Hour)) {
		return false
	}

	t.hour = s.Hour.UTC()
	t.samples = append(t.samples[:0], s.Samples...)
	t.lastSlot = s.LastSlot
	t.version++
	t.nonZero = 0
	for _, v := range t.samples {
		if v.IsPositive() {
			t.nonZero++
		}
	}
	// Простой между снимком и стартом — это отсутствие котировок, и в сетке
	// он обязан выглядеть нулями.
	t.fillWith(t.slot(now), decimal.Zero)
	return true
}

// New creates an hourly tracker.
func New(program reward.Program, policy Policy) *Tracker {
	if policy.SampleInterval <= 0 {
		policy.SampleInterval = time.Second
	}
	return &Tracker{program: program, policy: policy, lastSlot: -1}
}

// Sample records one observation. Slots missed by a late tick are filled with
// the preceding observation for up to MaxCarryGap, because the orders it
// described did not leave the book while the actor was late. Everything else —
// a longer gap, and every slot before a mid-hour process start — is inserted as
// zero. That part stays deliberately pessimistic: a restart must never
// manufacture uptime that the exchange did not observe.
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
		if !previous.Equal(value) {
			t.version++
		}
		t.samples[len(t.samples)-1] = value
		return
	}

	t.samples = append(t.samples, value)
	if value.IsPositive() {
		t.nonZero++
	}
	t.version++
	t.lastSlot = slot
}

func (t *Tracker) resetHour(now time.Time) {
	hour := now.UTC().Truncate(time.Hour)
	if !hour.Equal(t.hour) {
		t.hour = hour
		t.samples = t.samples[:0]
		t.nonZero = 0
		t.lastSlot = -1
		t.version++
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
	if t.lastSlot < 0 || slot <= t.lastSlot+1 {
		t.fillWith(slot, decimal.Zero)
		return
	}

	previous := t.samples[len(t.samples)-1]
	gap := time.Duration(slot-t.lastSlot-1) * t.policy.SampleInterval
	if gap > t.policy.MaxCarryGap {
		previous = decimal.Zero
	}
	t.fillWith(slot, previous)
}

func (t *Tracker) fillWith(slot int, value decimal.Decimal) {
	for missing := t.lastSlot + 1; missing < slot; missing++ {
		t.samples = append(t.samples, value)
		if value.IsPositive() {
			t.nonZero++
		}
		t.lastSlot = missing
		t.version++
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
		MakerHours:      t.makerHoursAt(now),
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

// makerHoursAt returns the programme estimate for the current grid, reusing the
// previous result while neither the grid nor the accrual multiplier changed.
//
// WHY THIS IS CACHED. MakerHours copies and sorts every sample of the hour, and
// the actor assesses the hour on every evaluation: the book, the mark and the
// reference feed, the last one alone up to forty times a second. The cost grows
// with the elapsed hour, from 0.02 ms and 506 allocations per call at minute one
// to 0.44 ms and 27 000 allocations (830 KB) at minute 59 on a development
// machine. The production hosts have a single vCPU, and every hour the actor
// climbed from about 10% of it to saturation in the last quarter. On v1,
// 15 September 2026, user time stood at 96-98% from 16:40 to 17:00, the actor
// fell behind its own acknowledgement timeout, and 76 of 78 placements between
// 16:56 and 16:59 were cancelled as "placement was never acknowledged" long
// before they could rest the three seconds the programme requires.
//
// The grid changes at most once per sample slot, so the sort now runs about
// once a second however often the hour is assessed.
func (t *Tracker) makerHoursAt(now time.Time) float64 {
	multiplier := t.program.Multiplier(now)
	if t.cached && t.cachedVersion == t.version && t.cachedMultiplier == multiplier {
		return t.cachedMakerHours
	}
	t.cachedMakerHours = t.program.MakerHours(t.samples, now)
	t.cachedVersion, t.cachedMultiplier, t.cached = t.version, multiplier, true
	return t.cachedMakerHours
}

// bestTier returns the strongest eligible reachable tier and its active safety
// target. If the preferred reserve is no longer reachable, the programme
// threshold remains the last-resort target rather than abandoning a payable
// hour early.
//
// ELIGIBILITY IS DECIDED BY THE PROGRAMME THRESHOLD AND BY NOTHING ELSE.
// Every configured number above that threshold — TargetPct, HardFloorPct,
// ReservePct — is a preference about how much cushion to aim for, and a
// preference that vetoes a payable hour is not a cushion, it is a loss.
//
// This was a live defect, not a hypothetical. HardFloorPct stood in as the
// eligibility floor for the top tier, so the reachable band between the
// programme's 70% and the configured 72% was discarded. Measured consequence
// on 18 August 2026, hour 13:00 UTC: at 13:45:33 the actor held 47.8% banked
// with 24.1% of the hour left, i.e. a best possible 71.9%. The 70% tier was
// still winnable and the actor declared "no economically eligible uptime tier
// remains", stopped quoting for the remaining fourteen minutes and made its
// own forecast come true. StandX paid 0.82 Maker Hours for that hour through
// the 0.5x tier where a full 2.00 was still reachable — the single most
// expensive decision of the day, roughly seven times the cost of the fill
// that started it.
//
// The cushions survive as a descending ladder of targets: aim for the most
// comfortable one still reachable, and never let any of them decide whether
// the hour is worth playing at all.
func (t *Tracker) bestTier(bestPossible float64) (reward.Tier, float64, bool) {
	for i, tier := range t.program.Tiers {
		if tier.Multiplier+1e-12 < t.policy.MinTierMultiplier {
			continue
		}
		if bestPossible+1e-12 < tier.ThresholdPct {
			continue
		}

		// Ordered from the most comfortable target to the programme threshold
		// itself; the first one still reachable is what the hour aims at.
		targets := []float64{tier.ThresholdPct + t.policy.ReservePct, tier.ThresholdPct}
		if i == 0 {
			targets = []float64{t.policy.TargetPct, t.policy.HardFloorPct, tier.ThresholdPct}
		}

		target := tier.ThresholdPct
		for _, candidate := range targets {
			if candidate > 100 {
				candidate = 100
			}
			if bestPossible+1e-12 >= candidate {
				target = candidate
				break
			}
		}
		return tier, target, true
	}
	return reward.Tier{}, 0, false
}

// Samples returns the normalized observation count in the current hour.
func (t *Tracker) Samples() int { return len(t.samples) }
