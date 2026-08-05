package risk_test

import (
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"

	"github.com/nd/mm-standx/internal/domain/risk"
)

func at(hour, minute int) time.Time {
	return time.Date(2026, 8, 3, hour, minute, 0, 0, time.UTC)
}

func usd(v string) decimal.Decimal { return decimal.RequireFromString(v) }

func limits() risk.Limits {
	return risk.Limits{MaxDailyLossUSD: 100, MaxFillsPerHour: 5, TripOnDrawdown: true}
}

func TestDailyLossTripsTheSwitch(t *testing.T) {
	l := risk.New(limits())

	l.OnTrade(at(10, 0), usd("-60"), usd("1"))
	require.False(t, l.Assess(at(10, 0)).Tripped)

	l.OnTrade(at(10, 5), usd("-38"), usd("1"))

	v := l.Assess(at(10, 5))
	require.True(t, v.Tripped)
	require.InDelta(t, 100, v.LossUSD, 0.001)
	require.Contains(t, v.Reason, "daily loss")
}

// A fee is always a cost, whatever sign the exchange puts on it: the StandX
// convention is unverified, and erring towards optimism would overstate how
// much of the daily limit is still available.
func TestFeeIsAlwaysACostRegardlessOfSign(t *testing.T) {
	positive := risk.New(limits())
	positive.OnTrade(at(10, 0), usd("0"), usd("7"))

	negative := risk.New(limits())
	negative.OnTrade(at(10, 0), usd("0"), usd("-7"))

	require.Equal(t,
		positive.Assess(at(10, 0)).ReportedNetUSD,
		negative.Assess(at(10, 0)).ReportedNetUSD)
	require.InDelta(t, -7, positive.Assess(at(10, 0)).ReportedNetUSD, 0.001)
}

func TestFillFrequencyTripsIndependentlyOfMoney(t *testing.T) {
	l := risk.New(limits())

	for i := range 5 {
		l.OnFill(at(10, i), risk.Fill{At: at(10, i), CostUSD: usd("0.01"), StartsEpisode: true})
	}

	v := l.Assess(at(10, 5))
	require.True(t, v.Tripped)
	require.Equal(t, 5, v.FillsLastHour)
	require.Contains(t, v.Reason, "fills in the last hour")
}

func TestEpisodesLeaveTheRollingHour(t *testing.T) {
	l := risk.New(limits())

	for i := range 4 {
		l.OnFill(at(10, i), risk.Fill{At: at(10, i), StartsEpisode: true})
	}
	require.Equal(t, 4, l.Assess(at(10, 30)).FillsLastHour)
	require.Equal(t, 0, l.Assess(at(11, 30)).FillsLastHour)
}

// The cost model stands in for the exchange when the trade channel is silent.
// Otherwise a broken channel would look exactly like break-even trading.
func TestCostModelSubstitutesForASilentTradeChannel(t *testing.T) {
	l := risk.New(risk.Limits{MaxDailyLossUSD: 50, TripOnDrawdown: true})

	for i := range 10 {
		l.OnFill(at(10, i), risk.Fill{At: at(10, i), CostUSD: usd("6"), StartsEpisode: true})
	}

	v := l.Assess(at(10, 10))
	require.True(t, v.Tripped)
	require.True(t, v.TradeChannelSuspect)
	require.InDelta(t, 60, v.LossUSD, 0.001)
}

// The worse of the two estimates wins: the exchange figure is more accurate
// when it arrives, but its silence must not read as an absence of loss.
func TestWorseOfTheTwoEstimatesWins(t *testing.T) {
	l := risk.New(limits())

	l.OnFill(at(10, 0), risk.Fill{At: at(10, 0), CostUSD: usd("30"), StartsEpisode: true})
	l.OnTrade(at(10, 0), usd("-5"), usd("0"))

	require.InDelta(t, 30, l.Assess(at(10, 0)).LossUSD, 0.001)

	l.OnTrade(at(10, 1), usd("-80"), usd("0"))
	require.InDelta(t, 85, l.Assess(at(10, 1)).LossUSD, 0.001)
}

// A restart must not clear the guard: otherwise the cycle of tripping and
// restarting drains the deposit in instalments.
func TestTrippedStateSurvivesRestartWithinTheDay(t *testing.T) {
	l := risk.New(limits())
	l.OnTrade(at(10, 0), usd("-200"), usd("0"))
	require.True(t, l.Assess(at(10, 0)).Tripped)

	restored := risk.New(limits())
	restored.Restore(l.State(), at(14, 0))

	v := restored.Assess(at(14, 0))
	require.True(t, v.Tripped)
	require.Contains(t, v.Reason, "daily loss")
}

func TestNewDayClearsTheLedger(t *testing.T) {
	l := risk.New(limits())
	l.OnTrade(at(23, 0), usd("-200"), usd("0"))
	require.True(t, l.Assess(at(23, 0)).Tripped)

	next := risk.New(limits())
	next.Restore(l.State(), at(23, 0).Add(2*time.Hour))

	v := next.Assess(at(23, 0).Add(2 * time.Hour))
	require.False(t, v.Tripped)
	require.Zero(t, v.LossUSD)
}

// Midnight must not clear the trailing-hour counter: its window is shorter
// than a day, and clearing it would blind the bot to frequent fills for an hour.
func TestDayRolloverKeepsTheRollingHour(t *testing.T) {
	l := risk.New(limits())

	start := time.Date(2026, 8, 3, 23, 40, 0, 0, time.UTC)
	for i := range 4 {
		l.OnFill(start.Add(time.Duration(i)*time.Minute),
			risk.Fill{At: start.Add(time.Duration(i) * time.Minute), StartsEpisode: true})
	}

	v := l.Assess(start.Add(25 * time.Minute)) // already the next UTC day
	require.Equal(t, 4, v.FillsLastHour)
	require.Zero(t, v.EpisodesToday, "the daily counter resets, the hourly one does not")
}

func TestDrawdownTripCanBeDisabled(t *testing.T) {
	l := risk.New(risk.Limits{MaxDailyLossUSD: 10, MaxFillsPerHour: 5, TripOnDrawdown: false})
	l.OnTrade(at(10, 0), usd("-500"), usd("0"))

	v := l.Assess(at(10, 0))
	require.False(t, v.Tripped)
	require.InDelta(t, 500, v.LossUSD, 0.001, "the loss is still measured")
}

func TestExternalTripIsSticky(t *testing.T) {
	l := risk.New(limits())
	l.Trip(at(10, 0), "handler panic")

	require.True(t, l.Assess(at(10, 0)).Tripped)
	require.Equal(t, "handler panic", l.Assess(at(11, 0)).Reason)
}

// Frequency is counted in episodes, cost in fills. A sweep produces dozens of
// partial fills at once: counting them individually would measure the size of
// someone else's order rather than the rate of our own mistakes, while ignoring
// their cost would understate what the episode actually charged us.
func TestPartialFillsCostSeparatelyButCountAsOneEpisode(t *testing.T) {
	l := risk.New(limits())

	l.OnFill(at(10, 0), risk.Fill{At: at(10, 0), CostUSD: usd("2"), StartsEpisode: true})
	for range 20 {
		l.OnFill(at(10, 0), risk.Fill{At: at(10, 0), CostUSD: usd("2")})
	}

	v := l.Assess(at(10, 0))
	require.Equal(t, 1, v.FillsLastHour)
	require.False(t, v.Tripped)
	require.InDelta(t, 42, v.EstimatedCostUSD, 0.001)
}
