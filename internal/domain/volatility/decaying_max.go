package volatility

import (
	"math"
	"time"

	"github.com/nd/mm-standx/internal/domain/money"
)

// decayingMax is a maximum that forgets exponentially rather than reverting on
// a fixed clock or falling off a sliding window's edge all at once.
//
// Later, smaller observations cannot lower it — only elapsed time can, and only
// by half every halfLife. This is the shape a measure of risk needs precisely
// because it is a maximum: smoothing would blur away the very outlier the
// mechanism exists to remember, and reverting to the mean the moment a bigger
// observation passes would erase the caution before the market has actually
// calmed. A comparable observation before full decay re-arms the clock, so a
// genuine cluster of jumps keeps the record elevated for as long as it keeps
// producing them — it is the isolated, un-repeated outlier that tapers away.
//
// Extracted as its own type because the estimator now holds two of these at
// different timescales: a fast one driving the primary risk measure, a slow
// one acting as an outer floor. See estimator.go for how they compose.
type decayingMax struct {
	halfLife time.Duration
	value    money.Bps
	at       time.Time
}

// observe folds a new jump into the record.
func (d *decayingMax) observe(jump money.Bps, at time.Time) {
	if d.halfLife <= 0 {
		return
	}
	d.value = max(d.decayed(at), jump)
	d.at = at
}

// decayed returns the record brought forward to now.
//
// Does not mutate: the estimate is read on every service tick, and mutating on
// read would turn polling frequency into a parameter of the risk model.
func (d *decayingMax) decayed(now time.Time) money.Bps {
	if d.halfLife <= 0 || d.value <= 0 || d.at.IsZero() {
		return 0
	}
	age := now.Sub(d.at)
	if age <= 0 {
		return d.value
	}
	return d.value * money.Bps(math.Exp2(-age.Seconds()/d.halfLife.Seconds()))
}
