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

	l.OnTrade(at(10, 0), risk.Execution{CashUSD: usd("-60"), FeeUSD: usd("1")})
	require.False(t, l.Assess(at(10, 0)).Tripped)

	l.OnTrade(at(10, 5), risk.Execution{CashUSD: usd("-38"), FeeUSD: usd("1")})

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
	positive.OnTrade(at(10, 0), risk.Execution{CashUSD: usd("0"), FeeUSD: usd("7")})

	negative := risk.New(limits())
	negative.OnTrade(at(10, 0), risk.Execution{CashUSD: usd("0"), FeeUSD: usd("-7")})

	require.Equal(t,
		positive.Assess(at(10, 0)).ReportedNetUSD,
		negative.Assess(at(10, 0)).ReportedNetUSD)
	require.InDelta(t, -7, positive.Assess(at(10, 0)).ReportedNetUSD, 0.001)
}

// The fill rate is a sliding-hour measure, so its answer has to expire on the
// same schedule. Tripping the persistent switch on it was the single largest
// cause of the live wedge: the episodes left the window an hour later, the flag
// stayed until midnight UTC, and it survived the restart that was supposed to
// clear it.
func TestFillFrequencyPausesButDoesNotTrip(t *testing.T) {
	l := risk.New(limits())

	for i := range 5 {
		l.OnFill(at(10, i), risk.Fill{At: at(10, i), CostUSD: usd("0.01"), StartsEpisode: true})
	}

	v := l.Assess(at(10, 5))
	require.False(t, v.Tripped, "a transient rate must not arm the persistent switch")
	require.True(t, v.Paused(at(10, 5)))
	require.Equal(t, 5, v.FillsLastHour)
	require.Contains(t, v.PauseReason, "fills in the last hour")

	// The pause ends when the oldest episode leaves the window and not a second
	// later: it lasts exactly as long as the condition that caused it.
	require.Equal(t, at(11, 0), v.PausedUntil)
	require.True(t, l.Assess(at(10, 59)).Paused(at(10, 59)))

	released := l.Assess(at(11, 1))
	require.False(t, released.Paused(at(11, 1)), "the pause must release itself")
	require.False(t, released.Tripped)
	require.Equal(t, 3, released.FillsLastHour)
}

// Every fresh episode pushes the deadline out, so a market that keeps hitting
// the quotes keeps the actor away without anyone arming anything.
func TestEachNewEpisodeExtendsThePause(t *testing.T) {
	l := risk.New(limits())

	for i := range 5 {
		l.OnFill(at(10, i), risk.Fill{At: at(10, i), StartsEpisode: true})
	}
	require.Equal(t, at(11, 0), l.Assess(at(10, 5)).PausedUntil)

	l.OnFill(at(10, 20), risk.Fill{At: at(10, 20), StartsEpisode: true})
	require.Equal(t, at(11, 1), l.Assess(at(10, 20)).PausedUntil,
		"with six episodes the window must shed two before quoting resumes")
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

// While the exchange is talking, it is the measure of the day's loss.
//
// The cost model is gross turnover, not loss: it only grows, and nothing in it
// subtracts the reward the strategy is paid for the same presence. Letting it
// speak over a healthy channel halts on volume — at ~6 bps a fill and a 10 USD
// limit, about a dozen fills, which a profitable day reaches easily.
func TestCostModelDoesNotSpeakOverAHealthyTradeChannel(t *testing.T) {
	l := risk.New(limits())

	l.OnFill(at(10, 0), risk.Fill{At: at(10, 0), CostUSD: usd("30"), StartsEpisode: true})
	l.OnTrade(at(10, 0), risk.Execution{CashUSD: usd("-5"), FeeUSD: usd("0")})

	v := l.Assess(at(10, 0))
	require.False(t, v.TradeChannelSuspect)
	require.InDelta(t, 5, v.LossUSD, 0.001, "the exchange figure stands on its own")
	require.InDelta(t, 30, v.EstimatedCostUSD, 0.001, "the model is still reported")

	l.OnTrade(at(10, 1), risk.Execution{CashUSD: usd("-80"), FeeUSD: usd("0")})
	require.InDelta(t, 85, l.Assess(at(10, 1)).LossUSD, 0.001)
}

// A profitable day must not be halted by its own turnover.
func TestTurnoverAloneDoesNotTripAProfitableDay(t *testing.T) {
	l := risk.New(risk.Limits{MaxDailyLossUSD: 10, TripOnDrawdown: true})

	for i := range 20 {
		l.OnFill(at(10, i), risk.Fill{At: at(10, i), CostUSD: usd("6"), StartsEpisode: true})
		l.OnTrade(at(10, i), risk.Execution{CashUSD: usd("1"), FeeUSD: usd("0")})
	}

	v := l.Assess(at(10, 20))
	require.False(t, v.Tripped)
	require.InDelta(t, 120, v.EstimatedCostUSD, 0.001)
	require.InDelta(t, 20, v.ReportedNetUSD, 0.001)
}

// A restart must not clear the guard: otherwise the cycle of tripping and
// restarting drains the deposit in instalments.
func TestTrippedStateSurvivesRestartWithinTheDay(t *testing.T) {
	l := risk.New(limits())
	l.OnTrade(at(10, 0), risk.Execution{CashUSD: usd("-200"), FeeUSD: usd("0")})
	require.True(t, l.Assess(at(10, 0)).Tripped)

	restored := risk.New(limits())
	restored.Restore(l.State(), at(14, 0))

	v := restored.Assess(at(14, 0))
	require.True(t, v.Tripped)
	require.Contains(t, v.Reason, "daily loss")
}

func TestNewDayClearsTheLedger(t *testing.T) {
	l := risk.New(limits())
	l.OnTrade(at(23, 0), risk.Execution{CashUSD: usd("-200"), FeeUSD: usd("0")})
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
	l.OnTrade(at(10, 0), risk.Execution{CashUSD: usd("-500"), FeeUSD: usd("0")})

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

// The order update and the trade message travel different channels, and the
// first routinely arrives ahead of the second. Suspecting the channel on that
// difference means crying wolf on every first fill of the day.
func TestTradeChannelIsNotSuspectedImmediately(t *testing.T) {
	now := time.Date(2026, 8, 6, 14, 34, 38, 0, time.UTC)
	l := risk.New(risk.Limits{MaxDailyLossUSD: 35, MaxFillsPerHour: 20, TripOnDrawdown: true})

	l.OnFill(now, risk.Fill{Symbol: "XAU-USD", At: now, CostUSD: decimal.NewFromFloat(0.2), StartsEpisode: true})
	require.False(t, l.Assess(now).TradeChannelSuspect,
		"the trade message is milliseconds behind, not missing")

	// It arrives, as it should, and the question is settled for the day.
	l.OnTrade(now.Add(50*time.Millisecond), risk.Execution{CashUSD: decimal.NewFromFloat(-0.1), FeeUSD: decimal.NewFromFloat(0.05)})
	require.False(t, l.Assess(now.Add(time.Hour)).TradeChannelSuspect)
}

// Silence past the grace is a real fault: the daily loss then rests on the cost
// model alone, and that has to be visible.
func TestSilentTradeChannelIsReportedAfterTheGrace(t *testing.T) {
	now := time.Date(2026, 8, 6, 14, 34, 38, 0, time.UTC)
	l := risk.New(risk.Limits{MaxDailyLossUSD: 35, MaxFillsPerHour: 20, TripOnDrawdown: true})

	l.OnFill(now, risk.Fill{Symbol: "XAU-USD", At: now, CostUSD: decimal.NewFromFloat(0.2), StartsEpisode: true})

	require.False(t, l.Assess(now.Add(risk.TradeChannelGrace-time.Second)).TradeChannelSuspect)
	require.True(t, l.Assess(now.Add(risk.TradeChannelGrace)).TradeChannelSuspect)
}

// State written before the field existed carries no first-episode timestamp.
// Reading it as "long ago" would announce a broken channel on every restart.
func TestRestoredStateWithoutATimestampIsNotSuspect(t *testing.T) {
	now := time.Date(2026, 8, 6, 14, 34, 38, 0, time.UTC)
	l := risk.New(risk.Limits{MaxDailyLossUSD: 35})

	l.Restore(risk.State{Day: now.UTC().Truncate(24 * time.Hour), EpisodesToday: 3}, now)
	require.False(t, l.Assess(now).TradeChannelSuspect)
}

// An open leg is not a loss.
//
// The cash flow shows minus the whole notional between the fill and the close,
// and the two are 340 milliseconds apart. Read as a result it trips a thirty
// dollar daily limit on a three hundred dollar round trip — which is exactly
// what halted the live bot on 7 August, on the first fill after deploy.
func TestOpenLegDoesNotTripTheSwitch(t *testing.T) {
	now := at(12, 29)
	l := risk.New(risk.Limits{MaxDailyLossUSD: 30, MaxFillsPerHour: 20, TripOnDrawdown: true})

	buy := func(qty, notional, fee string) risk.Execution {
		return risk.Execution{
			QtyBase: usd(qty), CashUSD: usd("-" + notional), FeeUSD: usd(fee),
		}
	}

	l.OnTrade(now, buy("0.061", "263.24", "0.0263"))
	require.False(t, l.Assess(now).Tripped, "a bought leg is a position, not a loss")
	require.InDelta(t, 0, l.Assess(now).ReportedNetUSD, 1e-9, "nothing is realised yet")

	l.OnTrade(now, buy("0.009", "38.84", "0.0039"))
	require.False(t, l.Assess(now).Tripped)

	// The close brings the position back to flat and the round trip is realised.
	l.OnTrade(now, risk.Execution{
		QtyBase: usd("-0.070"), CashUSD: usd("302.008"), FeeUSD: usd("0.1208"),
	})

	v := l.Assess(now)
	require.False(t, v.Tripped)
	require.InDelta(t, -0.221, v.ReportedNetUSD, 0.005, "the episode cost, as measured by hand")
	require.InDelta(t, 0, v.OpenLegUSD, 1e-9, "nothing left open")
}

// A leg that never closes must stay visible rather than quietly vanish from the
// daily result: a lost execution message understates the loss by that leg.
func TestUnclosedLegStaysVisible(t *testing.T) {
	now := at(12, 29)
	l := risk.New(limits())

	l.OnTrade(now, risk.Execution{
		QtyBase: usd("0.061"), CashUSD: usd("-263.24"), FeeUSD: usd("0.0263"),
	})

	v := l.Assess(now)
	require.InDelta(t, 0, v.ReportedNetUSD, 1e-9)
	require.InDelta(t, -263.27, v.OpenLegUSD, 0.01, "the leg is reported, not counted as loss")
}

// cooldownLimits — те же пределы, но остановка по убытку снимается сама.
func cooldownLimits() risk.Limits {
	l := limits()
	l.Cooldown = 90 * time.Minute
	l.MaxAutoResumes = 3
	return l
}

// Остановка по дневному убытку прежде держалась до полуночи UTC. 27 августа
// свип одной ступени в 14:46 снял котирование на девять часов при событии
// длиной в две секунды — несоразмерность, ради которой заведён Cooldown.
func TestDailyLossTripIsReleasedAfterTheCooldown(t *testing.T) {
	l := risk.New(cooldownLimits())
	l.OnTrade(at(10, 0), risk.Execution{CashUSD: usd("-120"), FeeUSD: usd("0")})

	v := l.Assess(at(10, 0))
	require.True(t, v.Tripped)
	require.Equal(t, at(11, 30), v.TrippedUntil, "the release time is announced when the switch trips")

	require.True(t, l.Assess(at(11, 29)).Tripped, "not a second early")

	v = l.Assess(at(11, 30))
	require.False(t, v.Tripped)
	require.Empty(t, v.Reason)
}

// Снятие обязано перенести начало отсчёта. Убыток, взведший остановку, уже
// реализован и до полуночи никуда не денется: оставь отсчёт от начала суток —
// и первая же оценка после снятия взведёт остановку снова, в тот же миг.
func TestReleaseRebasesTheLossWindow(t *testing.T) {
	l := risk.New(cooldownLimits())
	l.OnTrade(at(10, 0), risk.Execution{CashUSD: usd("-120"), FeeUSD: usd("0")})
	require.True(t, l.Assess(at(10, 0)).Tripped)

	v := l.Assess(at(11, 30))
	require.False(t, v.Tripped)
	require.Zero(t, v.LossUSD, "the window starts over")
	require.InDelta(t, -120, v.ReportedNetUSD, 0.001, "the day total is not forgotten")

	// Половина предела в новом окне ещё не останавливает.
	l.OnTrade(at(12, 0), risk.Execution{CashUSD: usd("-50"), FeeUSD: usd("0")})
	require.False(t, l.Assess(at(12, 0)).Tripped)

	// А целый — останавливает снова.
	l.OnTrade(at(12, 30), risk.Execution{CashUSD: usd("-50"), FeeUSD: usd("0")})
	require.True(t, l.Assess(at(12, 30)).Tripped)
}

// Перенос отсчёта превращает дневной предел в оконный, и ограничивает сутки
// уже не он, а число окон: без потолка сутки стоили бы предел столько раз,
// сколько окон в них помещается.
func TestAutoResumesAreCappedPerDay(t *testing.T) {
	l := risk.New(cooldownLimits())

	now := at(0, 0)
	for range 3 {
		l.OnTrade(now, risk.Execution{CashUSD: usd("-120"), FeeUSD: usd("0")})
		require.True(t, l.Assess(now).Tripped)
		now = now.Add(90 * time.Minute)
		require.False(t, l.Assess(now).Tripped)
	}

	l.OnTrade(now, risk.Execution{CashUSD: usd("-120"), FeeUSD: usd("0")})
	v := l.Assess(now)
	require.True(t, v.Tripped)
	require.Zero(t, v.AutoResumesLeft)
	require.True(t, v.TrippedUntil.IsZero(), "the budget is spent: this one holds until midnight UTC")
	require.True(t, l.Assess(now.Add(6*time.Hour)).Tripped)
}

// Бюджет автоснятий дневной, как и сам предел.
func TestNewDayRestoresTheAutoResumeBudget(t *testing.T) {
	l := risk.New(cooldownLimits())
	l.OnTrade(at(23, 0), risk.Execution{CashUSD: usd("-120"), FeeUSD: usd("0")})
	require.True(t, l.Assess(at(23, 0)).Tripped)

	next := risk.New(cooldownLimits())
	next.Restore(l.State(), at(23, 0).Add(2*time.Hour))

	v := next.Assess(at(23, 0).Add(2 * time.Hour))
	require.False(t, v.Tripped)
	require.Equal(t, 3, v.AutoResumesLeft)
	require.Zero(t, v.ReportedNetUSD, "and the day starts from a clean sheet")
}

// Cooldown касается только убытка. Нечитаемый снимок через полтора часа
// читаемым не станет, а решение человека отменяет человек.
func TestExternalTripIgnoresTheCooldown(t *testing.T) {
	l := risk.New(cooldownLimits())
	l.Trip(at(10, 0), "risk ledger state is unreadable")

	v := l.Assess(at(10, 0))
	require.True(t, v.Tripped)
	require.True(t, v.TrippedUntil.IsZero())
	require.True(t, l.Assess(at(23, 0)).Tripped, "and it is still standing hours later")
}

// Ноль оставляет прежнее поведение целиком: до полуночи UTC.
func TestZeroCooldownHoldsUntilMidnight(t *testing.T) {
	l := risk.New(limits())
	l.OnTrade(at(10, 0), risk.Execution{CashUSD: usd("-120"), FeeUSD: usd("0")})

	v := l.Assess(at(10, 0))
	require.True(t, v.Tripped)
	require.True(t, v.TrippedUntil.IsZero())
	require.Zero(t, v.AutoResumesLeft)
	require.True(t, l.Assess(at(23, 59)).Tripped)
}

// Взведённая остановка со сроком переживает перезапуск вместе со сроком:
// рестарт в середине паузы не должен ни продлевать её, ни отменять.
func TestPendingReleaseSurvivesRestart(t *testing.T) {
	l := risk.New(cooldownLimits())
	l.OnTrade(at(10, 0), risk.Execution{CashUSD: usd("-120"), FeeUSD: usd("0")})
	require.True(t, l.Assess(at(10, 0)).Tripped)

	restored := risk.New(cooldownLimits())
	restored.Restore(l.State(), at(10, 30))
	require.True(t, restored.Assess(at(10, 30)).Tripped)

	v := restored.Assess(at(11, 30))
	require.False(t, v.Tripped, "the deadline is the one set before the restart, not after it")
}
