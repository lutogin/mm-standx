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

func tracker(minTierMultiplier float64) *Tracker {
	return New(program(), Policy{
		TargetPct:         78,
		HardFloorPct:      72,
		ReservePct:        8,
		MinTierMultiplier: minTierMultiplier,
		SampleInterval:    time.Second,
	})
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
	tr := tracker(1)

	s := tr.Assess(hourStart())
	require.Equal(t, Earning, s.Mode)
	require.Equal(t, 1.0, s.TierMultiplier)
	require.InDelta(t, 78, s.TargetPct, 1e-9)
	require.InDelta(t, 13*time.Minute+12*time.Second, s.DowntimeBudget, float64(time.Second))
}

// The credit is a step function, so once the target is secured further presence
// adds nothing and the sensible move is to back away from the market.
func TestBankedModeAfterTargetIsSecured(t *testing.T) {
	tr := tracker(1)

	// Quote continuously through 80% of the hour.
	at := fill(tr, 48*time.Minute, 48*time.Minute, 1500)

	s := tr.Assess(at)
	require.Equal(t, Banked, s.Mode)
	require.Contains(t, s.Reason, "secured")
	require.InDelta(t, 1.0, s.MakerHours, 1e-9)
}

// If even flawless quoting for the rest of the hour cannot reach the lowest
// tier, continuing to quote pays for fills and earns nothing in return.
func TestLostHourStopsTakingRisk(t *testing.T) {
	tr := tracker(1)

	// Nothing quoted for the first 55 minutes: at best 5 of 60 minutes remain,
	// far below the 50% floor.
	at := fill(tr, 55*time.Minute, 0, 1500)

	s := tr.Assess(at)
	require.Equal(t, Lost, s.Mode)
	require.Contains(t, s.Reason, "remains reachable")
	require.Zero(t, s.MakerHours)
}

// A slow start is still recoverable, and the tracker must not give up early.
func TestSlowStartRemainsRecoverable(t *testing.T) {
	tr := tracker(1)

	// Ten idle minutes leave fifty available: enough for both tiers.
	at := fill(tr, 10*time.Minute, 0, 1500)

	s := tr.Assess(at)
	require.Equal(t, Earning, s.Mode)
	require.Greater(t, s.BestPossiblePct, 70.0)
}

// The free downtime is the budget the volatility breaker spends. It must shrink
// as the hour is consumed, otherwise the breaker would overspend it.
func TestDowntimeBudgetShrinksAsTheHourIsSpent(t *testing.T) {
	tr := tracker(1)

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
	tr := tracker(1)

	at := fill(tr, 20*time.Minute, 10*time.Minute, 1500)

	s := tr.Assess(at)
	require.InDelta(t, 50.0, s.QualifiedPct, 0.5)
}

// Scoring is per calendar hour, so crossing the boundary must start fresh
// rather than carry the previous hour's history forward.
func TestHourBoundaryResetsTheWindow(t *testing.T) {
	tr := tracker(1)

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
	tr := tracker(1)

	// One unit of size held for the whole hour earns exactly one maker hour.
	at := fill(tr, time.Hour, time.Hour, 1500)
	require.InDelta(t, 1.0, tr.Assess(at).MakerHours, 1e-9)
}

// Starting or restarting in the middle of the hour must not claim the time
// before the process existed. That time is unknown locally and therefore zero.
func TestMidHourStartBackfillsMissingPresenceAsZero(t *testing.T) {
	tr := tracker(1)
	start := hourStart().Add(35 * time.Minute)

	for elapsed := time.Duration(0); elapsed < 10*time.Minute; elapsed += time.Second {
		tr.Sample(start.Add(elapsed), decimal.NewFromInt(1500))
	}

	s := tr.Assess(start.Add(10 * time.Minute))
	require.InDelta(t, 10.0/45.0*100, s.QualifiedPct, 0.1)
	require.InDelta(t, 10.0/60.0*100, s.BankedPct, 0.1)
	require.Equal(t, Lost, s.Mode, "only fifteen minutes remain, so the full tier is unreachable")
	require.Zero(t, s.MakerHours, "missing pre-start samples must prevent a phantom tier")
}

// A lower tier is pursued only when its reduced accrual still passes the
// economics gate. The same clock state is lost when the tier is excluded.
func TestStandardTierRequiresEconomicEligibility(t *testing.T) {
	allowed := tracker(0.5)
	blocked := tracker(1)

	for elapsed := time.Duration(0); elapsed < 40*time.Minute; elapsed += time.Second {
		value := decimal.Zero
		if elapsed < 20*time.Minute {
			value = decimal.NewFromInt(1500)
		}
		at := hourStart().Add(elapsed)
		allowed.Sample(at, value)
		blocked.Sample(at, value)
	}
	at := hourStart().Add(40 * time.Minute)

	standard := allowed.Assess(at)
	require.Equal(t, Earning, standard.Mode)
	require.Equal(t, 0.5, standard.TierMultiplier)
	require.InDelta(t, 58, standard.TargetPct, 1e-9)

	require.Equal(t, Lost, blocked.Assess(at).Mode)
}

// Час, который ещё берётся по порогу программы, не сдаётся из-за страхового
// запаса. Это регрессия на живой убыток, а не гипотеза.
//
// 18 августа 2026, час 13:00 UTC: после залива актор простоял двенадцать минут
// и к 13:45:33 держал 47.8% при прошедших 75.9%, то есть best possible 71.9%.
// Порог программы — 70%, HardFloorPct — 72. Прежний код сравнивал достижимое
// с HardFloorPct, объявлял «no economically eligible uptime tier remains»,
// снимал котировки до конца часа и делал собственный прогноз верным. StandX
// заплатил 0.82 Maker Hour по тиру 0.5x там, где полные 2.00 ещё брались.
func TestReachableProgrammeThresholdSurvivesTheSafetyCushion(t *testing.T) {
	tr := tracker(1)

	// 47.8% присутствия за 75.9% часа: ровно та точка, с которой актор сдался.
	elapsed := 2733 * time.Second
	quoting := 1721 * time.Second
	at := fill(tr, elapsed, quoting, 3000)

	s := tr.Assess(at.Add(time.Second))

	require.InDelta(t, 71.9, s.BestPossiblePct, 0.3,
		"фиксируем саму точку замера: дальше проверяется решение по ней")
	require.NotEqual(t, Lost, s.Mode,
		"порог программы 70%% ещё достижим — час обязан остаться в игре")
	require.Equal(t, Earning, s.Mode)
	require.Equal(t, 1.0, s.TierMultiplier,
		"это полный тир, а не запасной 0.5x")
	require.InDelta(t, 70, s.TargetPct, 1e-9,
		"цель падает до порога программы, а не до HardFloorPct, который уже недостижим")
}

// Ниже порога программы час действительно потерян: присутствие оплачивается
// исполнениями и не приносит зачёта. Правка выше не должна это отменять.
func TestUnreachableProgrammeThresholdStillLosesTheHour(t *testing.T) {
	tr := tracker(1)

	// 10% присутствия за 80% часа: best possible 30%, ниже и 70%, и 50%.
	at := fill(tr, 2880*time.Second, 288*time.Second, 3000)

	s := tr.Assess(at.Add(time.Second))

	require.Less(t, s.BestPossiblePct, 50.0)
	require.Equal(t, Lost, s.Mode)
}

// Пока запас достижим, целью остаётся именно он: правка меняет только исход
// в полосе между порогом программы и страховым запасом.
func TestComfortableTargetIsStillPreferredWhenReachable(t *testing.T) {
	tr := tracker(1)

	at := fill(tr, 600*time.Second, 600*time.Second, 3000)

	s := tr.Assess(at.Add(time.Second))

	require.Equal(t, Earning, s.Mode)
	require.InDelta(t, 78, s.TargetPct, 1e-9, "TargetPct достижим — берём его")
}

// Запасной тир выбирается, когда полный уже недостижим, и это отдельная ветка:
// у неё свой резерв и свой порог.
func TestLowerTierKeepsItsOwnReserve(t *testing.T) {
	tr := tracker(0.5)

	// 30% присутствия за 70% часа: best possible 60% — ниже 70, выше 50+8.
	at := fill(tr, 2520*time.Second, 1080*time.Second, 3000)

	s := tr.Assess(at.Add(time.Second))

	require.Equal(t, Earning, s.Mode)
	require.Equal(t, 0.5, s.TierMultiplier)
	require.InDelta(t, 58, s.TargetPct, 1e-9, "порог 50 плюс резерв 8")
}

// carryTracker samples every second and describes a gap of up to five seconds
// by the observation preceding it — the actor's cadence on the live account.
func carryTracker() *Tracker {
	return New(program(), Policy{
		TargetPct:         78,
		HardFloorPct:      72,
		ReservePct:        8,
		MinTierMultiplier: 1,
		SampleInterval:    time.Second,
		MaxCarryGap:       5 * time.Second,
	})
}

// A late tick is not an absence of quotes.
//
// The actor's tick slips regularly — "mark tick handled more than a tick late"
// appears tens of thousands of times in a live run — and every slot it skipped
// used to be recorded as zero. The orders it would have described were resting
// in the book throughout, and the exchange scored them: its report for 24 August
// credited 1.81-1.98 Maker Hours in the hours our own estimate put at 1.05-1.22.
//
// The rejected alternative is named by the second half: with MaxCarryGap at
// zero the same gap reads as downtime, which is what the code did before.
func TestShortGapIsDescribedByThePrecedingObservation(t *testing.T) {
	const size = 3000

	late := carryTracker()
	late.Sample(hourStart(), decimal.NewFromInt(size))
	late.Sample(hourStart().Add(4*time.Second), decimal.NewFromInt(size))

	require.Equal(t, 5, len(late.samples), "the grid is filled to the observed slot")
	require.Equal(t, 5, late.nonZero, "four late seconds are not four seconds of absence")

	strict := tracker(1) // MaxCarryGap is zero here
	strict.Sample(hourStart(), decimal.NewFromInt(size))
	strict.Sample(hourStart().Add(4*time.Second), decimal.NewFromInt(size))
	require.Equal(t, 2, strict.nonZero, "this is the behaviour the carry replaces")
}

// Past the carry gap the silence is a stall, and a stall must read as one.
func TestLongGapStillReadsAsDowntime(t *testing.T) {
	tr := carryTracker()
	tr.Sample(hourStart(), decimal.NewFromInt(3000))
	tr.Sample(hourStart().Add(20*time.Second), decimal.NewFromInt(3000))

	require.Equal(t, 21, len(tr.samples))
	require.Equal(t, 2, tr.nonZero,
		"twenty seconds of silence is not lateness, and carrying it would manufacture uptime")
}

// A process starting mid-hour has nothing to carry, and must not invent it.
//
// This is the invariant the zero-filling existed for, and it survives the
// change untouched: the carry needs a preceding observation, and before the
// first sample of the hour there is none.
func TestMidHourStartStillCountsTheMissedSlotsAsZero(t *testing.T) {
	tr := carryTracker()
	tr.Sample(hourStart().Add(30*time.Minute), decimal.NewFromInt(3000))

	require.Equal(t, 1801, len(tr.samples))
	require.Equal(t, 1, tr.nonZero, "a restart must never manufacture uptime")
}

// ---- Снимок часа переживает перезапуск --------------------------------------

// Деплой после середины часа больше не обнуляет набранное присутствие.
//
// Замер v1 за 9 сентября 2026: рестарт в 11:29 — и через минуту
// "no economically eligible uptime tier remains reachable", час закрылся
// на 0.00x. Биржа при этом считала книгу непрерывно и о подмене процесса
// не знала.
func TestSnapshotSurvivesRestartWithinTheHour(t *testing.T) {
	hourStart := time.Date(2026, 9, 9, 11, 0, 0, 0, time.UTC)
	policy := Policy{
		TargetPct: 80, HardFloorPct: 72, ReservePct: 8,
		SampleInterval: time.Second, MaxCarryGap: 5 * time.Second,
	}

	before := New(program(), policy)
	at := hourStart
	for range 1800 { // тридцать минут присутствия
		before.Sample(at, decimal.NewFromInt(60))
		at = at.Add(time.Second)
	}
	banked := before.Assess(at).BankedPct
	require.InDelta(t, 50, banked, 1, "half the hour is banked")

	// Процесс подменили: новый трекер поднимает снимок.
	after := New(program(), policy)
	restartAt := at.Add(10 * time.Second)
	require.True(t, after.Restore(before.Snapshot(), restartAt))

	got := after.Assess(restartAt)
	require.InDelta(t, banked, got.BankedPct, 1, "the banked half must come back")
	require.NotEqual(t, Lost, got.Mode, "an hour half secured is not a lost hour")
}

// Без снимка тот же рестарт объявляет час потерянным — это и была неисправность.
func TestRestartWithoutSnapshotLosesTheHour(t *testing.T) {
	hourStart := time.Date(2026, 9, 9, 11, 0, 0, 0, time.UTC)
	policy := Policy{
		TargetPct: 80, HardFloorPct: 72, ReservePct: 8,
		SampleInterval: time.Second, MaxCarryGap: 5 * time.Second,
	}
	fresh := New(program(), policy)
	require.Equal(t, Lost, fresh.Assess(hourStart.Add(31*time.Minute)).Mode)
}

// Простой между снимком и стартом ложится в сетку нулями, а не переносом:
// на выходе котировки снимаются, и книга всё это время была пуста.
func TestRestoreZeroesTheOutage(t *testing.T) {
	hourStart := time.Date(2026, 9, 9, 11, 0, 0, 0, time.UTC)
	policy := Policy{
		TargetPct: 80, HardFloorPct: 72, ReservePct: 8,
		SampleInterval: time.Second, MaxCarryGap: 30 * time.Second,
	}
	before := New(program(), policy)
	at := hourStart
	for range 600 { // десять минут
		before.Sample(at, decimal.NewFromInt(60))
		at = at.Add(time.Second)
	}

	after := New(program(), policy)
	// Простой короче MaxCarryGap: обычный путь заполнения перенёс бы сюда
	// последнее наблюдение, хотя ордеров в стакане не было.
	restartAt := at.Add(20 * time.Second)
	require.True(t, after.Restore(before.Snapshot(), restartAt))

	got := after.Assess(restartAt)
	require.InDelta(t, 10*100.0/60, got.BankedPct, 0.6,
		"the outage must count as absence, not as carried presence")
}

// Снимок чужого часа не принимается: иначе он выдумал бы присутствие.
func TestRestoreRefusesAnotherHour(t *testing.T) {
	policy := Policy{TargetPct: 80, HardFloorPct: 72, ReservePct: 8, SampleInterval: time.Second}
	before := New(program(), policy)
	at := time.Date(2026, 9, 9, 11, 0, 0, 0, time.UTC)
	for range 600 {
		before.Sample(at, decimal.NewFromInt(60))
		at = at.Add(time.Second)
	}
	after := New(program(), policy)
	require.False(t, after.Restore(before.Snapshot(), at.Add(time.Hour)))
	require.Zero(t, after.Assess(at.Add(time.Hour)).BankedPct)
}

// Пустой снимок — первый запуск, а не ошибка.
func TestRestoreIgnoresAnEmptySnapshot(t *testing.T) {
	policy := Policy{TargetPct: 80, HardFloorPct: 72, ReservePct: 8, SampleInterval: time.Second}
	tr := New(program(), policy)
	require.False(t, tr.Restore(State{}, time.Date(2026, 9, 9, 11, 0, 0, 0, time.UTC)))
}

// The cached estimate must follow every way the grid can change: an overwrite
// of the current slot, completed slots appended by Assess itself, a restored
// snapshot and the hour boundary. Each step is compared with the programme
// formula on the same grid, and each step must actually move the result, so
// a cache that never invalidates cannot pass.
func TestAssessTracksEveryChangeToTheGrid(t *testing.T) {
	tr := tracker(1)
	assess := func(at time.Time) float64 {
		got := tr.Assess(at).MakerHours
		require.Equal(t, tr.program.MakerHours(tr.samples, at), got,
			"the estimate at %s must match the grid it describes", at.Format(time.TimeOnly))
		return got
	}

	at := hourStart()
	for slot := 0; slot < 10; slot++ {
		at = hourStart().Add(time.Duration(slot) * time.Second)
		value := decimal.NewFromInt(1500)
		if slot >= 7 {
			value = decimal.Zero
		}
		tr.Sample(at, value)
	}
	partial := assess(at)
	require.Equal(t, partial, assess(at), "an unchanged grid gives the same estimate")

	tr.Sample(at, decimal.NewFromInt(1500))
	overwritten := assess(at)
	require.NotEqual(t, partial, overwritten, "overwriting the current slot must be seen")
	snapshot := tr.Snapshot()

	backfilled := assess(at.Add(20 * time.Second))
	require.NotEqual(t, overwritten, backfilled, "slots completed while no sample arrived must be seen")

	require.True(t, tr.Restore(snapshot, at.Add(time.Second)))
	restored := assess(at.Add(time.Second))
	require.NotEqual(t, backfilled, restored, "a restored snapshot must be seen")

	nextHour := hourStart().Add(time.Hour + 5*time.Second)
	require.NotEqual(t, restored, assess(nextHour), "the hour boundary must be seen")
}

// Assessing an unchanged hour must not sort it again. The actor assesses on
// every evaluation, up to forty a second, and sorting the full grid each time
// saturated the single-vCPU hosts late in every hour (15 September 2026).
//
// Allocations stand in for time here. The samples carry mixed decimal
// exponents, as distance-weighted sizes do in production: comparing those
// allocates, so an uncached assessment at minute 59 makes about 27 000
// allocations and a cached one a handful. Whole numbers would hide the defect,
// because equal exponents compare without allocating.
func TestRepeatedAssessmentDoesNotResortTheHour(t *testing.T) {
	tr := tracker(1)
	sizes := []decimal.Decimal{
		decimal.RequireFromString("63.614"), decimal.RequireFromString("63.62"),
		decimal.RequireFromString("63.6251"), decimal.Zero, decimal.RequireFromString("47.7105"),
	}
	at := hourStart()
	for slot := 0; slot < 59*60; slot++ {
		at = hourStart().Add(time.Duration(slot) * time.Second)
		tr.Sample(at, sizes[slot%len(sizes)])
	}
	tr.Assess(at)

	allocs := testing.AllocsPerRun(100, func() { tr.Assess(at) })
	require.Less(t, allocs, 100.0, "an unchanged full-hour grid was sorted again")
}
