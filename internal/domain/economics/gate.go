// Package economics contains the pure profitability gates used before quoting.
package economics

import "fmt"

// Gate prevents live quoting when the measured reward does not cover the
// conservative cost of fills. A zero reward lower bound means that the rate is
// still unknown and only a deliberately small calibration allocation is safe.
type Gate struct {
	RewardRateLowerBoundUSDPerMakerHour float64
	BreakEvenUSDPerMakerHour            float64
}

// Decision is the result of applying the economic gate to one quoting mode.
type Decision struct {
	Allowed       bool
	EffectiveRate float64
	Reason        string
}

// Evaluate answers whether quoting pays at the current uptime-tier and market
// multipliers. Both multipliers reduce earned Maker Hours without reducing the
// notional exposed to fills, so they reduce the economically comparable rate.
//
// Size is deliberately not an input any more. It used to be: while the reward
// rate was unknown the gate capped the allocation, which made the protection a
// number in a file that had to be kept in step with the balance by hand. It
// never was — on 15 August the cap stood at 5800 against a balance of 11091,
// and half the account sat idle without anything saying so.
//
// The protection that survives is the one that does not depend on size at all.
// Reward and fill cost are both linear in notional (PLAN.md section 1), so the
// ratio between them — and therefore whether quoting pays — is the same at any
// size. What an unknown rate justifies is refusing the hours that pay a
// fraction while costing the same risk, and that is exactly the check below.
func (g Gate) Evaluate(tierMultiplier, marketMultiplier float64) Decision {
	if g.RewardRateLowerBoundUSDPerMakerHour <= 0 {
		if tierMultiplier < 1 || marketMultiplier < 1 {
			return Decision{Reason: "reward rate is unknown; reduced-accrual quoting is disabled during calibration"}
		}
		return Decision{Allowed: true, Reason: "full-rate calibration while the reward rate is unknown"}
	}

	effective := g.RewardRateLowerBoundUSDPerMakerHour * tierMultiplier * marketMultiplier
	if effective < g.BreakEvenUSDPerMakerHour {
		return Decision{
			EffectiveRate: effective,
			Reason: fmt.Sprintf(
				"reward lower bound %.2f USD/MH after multipliers is below the %.2f USD/MH break-even",
				effective, g.BreakEvenUSDPerMakerHour),
		}
	}
	return Decision{Allowed: true, EffectiveRate: effective, Reason: "reward lower bound covers break-even"}
}

// MinimumTierMultiplier returns the weakest uptime tier that can pay for its
// risk. During calibration only the full tier is eligible.
func (g Gate) MinimumTierMultiplier() float64 {
	if g.RewardRateLowerBoundUSDPerMakerHour <= 0 {
		return 1
	}
	return g.BreakEvenUSDPerMakerHour / g.RewardRateLowerBoundUSDPerMakerHour
}
