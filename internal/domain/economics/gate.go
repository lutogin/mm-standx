// Package economics contains the pure profitability gates used before quoting.
package economics

import "fmt"

// Gate prevents live quoting when the measured reward does not cover the
// conservative cost of fills. A zero reward lower bound means that the rate is
// still unknown and only a deliberately small calibration allocation is safe.
type Gate struct {
	RewardRateLowerBoundUSDPerMakerHour float64
	BreakEvenUSDPerMakerHour            float64
	CalibrationMaxAllocationUSD         float64
}

// Decision is the result of applying the economic gate to one quoting mode.
type Decision struct {
	Allowed       bool
	EffectiveRate float64
	Reason        string
}

// Evaluate checks an allocation at the current uptime-tier and market
// multipliers. Both multipliers reduce earned Maker Hours without reducing the
// notional exposed to fills, so they reduce the economically comparable rate.
func (g Gate) Evaluate(allocationUSD, tierMultiplier, marketMultiplier float64) Decision {
	if g.RewardRateLowerBoundUSDPerMakerHour <= 0 {
		if allocationUSD <= 0 || allocationUSD > g.CalibrationMaxAllocationUSD {
			return Decision{Reason: fmt.Sprintf(
				"reward rate is unknown; allocation %.2f USD exceeds the %.2f USD calibration cap",
				allocationUSD, g.CalibrationMaxAllocationUSD)}
		}
		if tierMultiplier < 1 || marketMultiplier < 1 {
			return Decision{Reason: "reward rate is unknown; reduced-accrual quoting is disabled during calibration"}
		}
		return Decision{Allowed: true, Reason: "small-allocation reward calibration"}
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
