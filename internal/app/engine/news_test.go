package engine

import (
	"bytes"
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"

	"github.com/nd/mm-standx/internal/domain/calendar"
	"github.com/nd/mm-standx/internal/domain/market"
	"github.com/nd/mm-standx/internal/domain/trading"
	"github.com/nd/mm-standx/internal/domain/uptime"
	"github.com/nd/mm-standx/internal/events"
	"github.com/nd/mm-standx/internal/ports"
)

var newsPolicy = calendar.Policy{
	DefensiveBefore: 15 * time.Minute,
	DefensiveAfter:  15 * time.Minute,
	HardBefore:      time.Minute,
	HardAfter:       time.Minute,
}

// newsEngine — актор с включённым блэкаутом и публикацией в известный момент.
//
// Час выбран так, чтобы публикация попала в его середину: dangerEngine ставит
// часы на 14:00 UTC, поэтому CPI в 14:30 — это ровно та геометрия, ради которой
// проверка бюджета простоя и заведена.
func newsEngine(t *testing.T, release time.Time) (*Engine, *recordingGateway, *testClock) {
	t.Helper()
	e, gw, clk := dangerEngine(t)
	e.available, e.equity = decimal.NewFromInt(1000), decimal.NewFromInt(1000)
	// Аллокация обязана быть связывающим ограничением, иначе её урезание
	// в защитной позе ничего не меняет и тест проверяет пустоту.
	e.cfg.AllocationUSD = 60
	// Настоящая политика присутствия вместо нулевой: с порогами по умолчанию
	// час немедленно оказывается «взятым», и путь бюджета простоя недостижим.
	e.hour = uptime.New(e.program, uptime.Policy{
		TargetPct:      80,
		HardFloorPct:   72,
		ReservePct:     8,
		SampleInterval: tickInterval,
	})
	e.newsGuard = NewsGuard{
		Enabled:               true,
		MaxAge:                6 * time.Hour,
		FailOpen:              true,
		DefensiveInsetBps:     1.5,
		DefensiveSizeMultiple: 0.5,
	}
	e.handle(context.Background(), events.NewsCalendarUpdated{
		Calendar: calendar.New(newsPolicy, []calendar.Event{
			{At: release, Title: "CPI m/m", Currency: "USD", Impact: calendar.ImpactHigh},
		}),
		Fetched: clk.now,
		At:      clk.now,
	})
	return e, gw, clk
}

// Публикация — единственный отрезок, где никакая дистанция внутри полосы
// не спасает: круг отмены занимает 400 мс, а золото на CPI проходит десятки
// базисных пунктов за первую секунду. Ответ здесь один — отсутствие.
func TestReleaseTakesTheBookDown(t *testing.T) {
	clkStart := time.Date(2026, 8, 12, 14, 0, 0, 0, time.UTC)
	e, gw, clk := newsEngine(t, clkStart.Add(30*time.Minute))
	clk.now = clkStart.Add(30 * time.Minute)

	liveRung(e, market.Buy, "bid-1", "4273", clk.now)
	liveRung(e, market.Sell, "ask-1", "4279", clk.now)

	e.evaluate(context.Background(), true)

	require.ElementsMatch(t, []trading.ClOrdID{"bid-1", "ask-1"}, gw.cancelled)
	require.Contains(t, e.blockedReason(e.target), "")
}

// Перед публикацией бот не уходит, а отходит: зачёт считается перцентилем
// замеров, и полчаса нулей утягивают весь час на тир ниже, тогда как те же
// полчаса у края полосы оставляют час оплачиваемым.
func TestApproachingReleaseWidensInsteadOfLeaving(t *testing.T) {
	start := time.Date(2026, 8, 12, 14, 0, 0, 0, time.UTC)
	e, gw, clk := newsEngine(t, start.Add(30*time.Minute))

	// За десять минут до публикации: защитное окно, но не жёсткое.
	clk.now = start.Add(20 * time.Minute)
	primeVolatility(t, e, clk)
	clk.now = start.Add(20 * time.Minute)

	e.evaluate(context.Background(), true)

	require.NotEmpty(t, gw.placed, "the book stays populated through the approach")
	require.GreaterOrEqual(t, float64(e.target.Distance), 8.0,
		"the quote has to stand against the outer edge of the band")
	require.True(t, e.target.Feasible)
}

// Размер режется вместе с отходом: по зачёту это бесплатно, а потери
// от исполнения размеру прямо пропорциональны.
func TestDefensivePostureAlsoCutsTheSize(t *testing.T) {
	start := time.Date(2026, 8, 12, 14, 0, 0, 0, time.UTC)
	// Публикация в начале часа, оценка через пять минут после неё: защитное
	// окно ещё действует, а час далеко не исчерпан и котирование продолжается.
	e, _, clk := newsEngine(t, start.Add(10*time.Minute))

	clk.now = start.Add(15 * time.Minute)
	primeVolatility(t, e, clk)
	clk.now = start.Add(15 * time.Minute)

	e.evaluate(context.Background(), true)
	defensive, defensiveDistance := e.perSide, e.target.Distance
	require.True(t, defensive.IsPositive(), "the defensive posture still quotes")

	// Такт вперёд: ограничение темпа перестановки иначе обрывает пересчёт
	// раньше расчёта размера, и сравнивались бы два одинаковых снимка.
	e.newsGuard.Enabled = false
	clk.advance(time.Second)
	e.evaluate(context.Background(), true)

	require.True(t, defensive.Lt(e.perSide),
		"a defensive quote is smaller than the same quote outside the window")
	require.Greater(t, float64(defensiveDistance), float64(e.target.Distance),
		"and it stands further from the mark")
}

// Учёт аптайма считает замеры и о будущем простое знать не может. Публикация
// через одиннадцать минут — это простой, который наверняка будет, и списать
// его заранее нужно затем, чтобы не котировать четверть часа под полным риском
// ради часа, который к тому моменту уже не оплатится.
func TestPlannedBlackoutIsChargedAgainstTheHourInAdvance(t *testing.T) {
	start := time.Date(2026, 8, 12, 14, 0, 0, 0, time.UTC)
	// Публикация через одиннадцать минут: защитное окно уже действует, час ещё
	// оплачивается, но простоя в нём накопилось столько, что двух минут
	// затемнения он больше не выдержит.
	e, gw, clk := newsEngine(t, start.Add(40*time.Minute))

	var buf bytes.Buffer
	e.deps.Log = slog.New(slog.NewTextHandler(&buf, nil))

	clk.now = start.Add(29 * time.Minute)
	primeVolatility(t, e, clk)
	clk.now = start.Add(29 * time.Minute)

	spendTheHour(t, e, clk.now, 2*time.Minute)
	liveRung(e, market.Buy, "bid-1", "4273", clk.now)

	e.evaluate(context.Background(), true)

	require.Contains(t, gw.cancelled, trading.ClOrdID("bid-1"))
	require.Contains(t, buf.String(), "scheduled blackout")
}

// Бюджет простоя обязан оставаться в силе, пока затемнение в него помещается:
// правило существует, чтобы не котировать ради потерянного часа, а не чтобы
// уходить из стакана всякий раз, когда впереди публикация.
func TestPlannedBlackoutThatFitsTheBudgetChangesNothing(t *testing.T) {
	start := time.Date(2026, 8, 12, 14, 0, 0, 0, time.UTC)
	e, gw, clk := newsEngine(t, start.Add(30*time.Minute))

	clk.now = start.Add(20 * time.Minute)
	primeVolatility(t, e, clk)
	clk.now = start.Add(20 * time.Minute)

	e.evaluate(context.Background(), true)

	require.NotEmpty(t, gw.placed, "two minutes of blackout fit in this hour comfortably")
}

// Календарь не читается, режим fail-open: бот продолжает работать. Остановка
// на сбое третьей стороны стоит присутствия каждый раз, а слепота — только
// в те редкие часы, когда публикацию перенесли ровно в окно недоступности.
func TestUnavailableCalendarKeepsQuotingWhenFailOpen(t *testing.T) {
	e, gw, clk := dangerEngine(t)
	e.available, e.equity = decimal.NewFromInt(1000), decimal.NewFromInt(1000)
	e.newsGuard = NewsGuard{
		Enabled: true, MaxAge: time.Hour, FailOpen: true,
		DefensiveInsetBps: 1.5, DefensiveSizeMultiple: 0.5,
	}

	e.evaluate(context.Background(), true)

	require.Empty(t, gw.cancelled)
	require.NotEmpty(t, gw.placed)
	_ = clk
}

// В режиме fail-closed нечитаемое расписание означает торговлю вслепую.
func TestUnavailableCalendarStopsQuotingWhenFailClosed(t *testing.T) {
	e, gw, clk := dangerEngine(t)
	e.available, e.equity = decimal.NewFromInt(1000), decimal.NewFromInt(1000)
	e.newsGuard = NewsGuard{
		Enabled: true, MaxAge: time.Hour, FailOpen: false,
		DefensiveInsetBps: 1.5, DefensiveSizeMultiple: 0.5,
	}
	liveRung(e, market.Buy, "bid-1", "4273", clk.now)

	e.evaluate(context.Background(), true)

	require.Contains(t, gw.cancelled, trading.ClOrdID("bid-1"))
	require.Empty(t, gw.placed)
}

// Устаревшее расписание в fail-closed равно отсутствующему: публикацию могли
// перенести, а мы об этом не узнали.
func TestStaleCalendarStopsQuotingWhenFailClosed(t *testing.T) {
	start := time.Date(2026, 8, 12, 14, 0, 0, 0, time.UTC)
	e, gw, clk := newsEngine(t, start.Add(30*time.Minute))
	e.newsGuard.FailOpen = false
	e.newsGuard.MaxAge = time.Hour
	e.newsFetched = clk.now.Add(-2 * time.Hour)

	liveRung(e, market.Buy, "bid-1", "4273", clk.now)
	e.evaluate(context.Background(), true)

	require.Contains(t, gw.cancelled, trading.ClOrdID("bid-1"))
}

// Выключенный блэкаут ничего не трогает.
func TestDisabledGuardIgnoresTheCalendarEntirely(t *testing.T) {
	start := time.Date(2026, 8, 12, 14, 0, 0, 0, time.UTC)
	e, gw, clk := newsEngine(t, start.Add(30*time.Minute))
	e.newsGuard.Enabled = false
	clk.now = start.Add(30 * time.Minute) // ровно момент публикации
	primeVolatility(t, e, clk)
	clk.now = start.Add(30 * time.Minute)

	e.evaluate(context.Background(), true)

	require.Empty(t, gw.cancelled)
	require.Equal(t, "off", e.newsSummary(clk.now))
}

// Смена позы печатается как событие: календарь тих часами, и переход — это
// единственное, что в нём происходит.
func TestPostureChangeIsLoggedOnce(t *testing.T) {
	start := time.Date(2026, 8, 12, 14, 0, 0, 0, time.UTC)
	e, _, clk := newsEngine(t, start.Add(30*time.Minute))

	var buf bytes.Buffer
	e.deps.Log = slog.New(slog.NewTextHandler(&buf, nil))

	clk.now = start.Add(20 * time.Minute)
	e.evaluate(context.Background(), true)
	require.Contains(t, buf.String(), "standing back for an upcoming release")

	buf.Reset()
	e.evaluate(context.Background(), true)
	require.NotContains(t, buf.String(), "standing back",
		"the posture did not change, so nothing happened")

	buf.Reset()
	clk.now = start.Add(30 * time.Minute)
	e.evaluate(context.Background(), true)
	require.Contains(t, buf.String(), "leaving the book for a scheduled release")
}

// Сводка отвечает на оба вопроса разом: почему бот стоит далеко и когда он
// уйдёт из стакана.
func TestSummaryCarriesThePostureAndTheNextRelease(t *testing.T) {
	start := time.Date(2026, 8, 12, 14, 0, 0, 0, time.UTC)
	e, _, _ := newsEngine(t, start.Add(30*time.Minute))

	require.Equal(t, "normal, next in 30m0s", e.newsSummary(start))
	require.Equal(t, "defensive, next in 10m0s", e.newsSummary(start.Add(20*time.Minute)))
	require.Equal(t, "hard, next in 0s", e.newsSummary(start.Add(30*time.Minute)))
	require.Equal(t, "normal, nothing scheduled", e.newsSummary(start.Add(time.Hour)))
}

// spendTheHour доводит учёт присутствия до состояния, в котором простоя
// осталось меньше, чем требует затемнение.
//
// Настоящими замерами, а не подменой поля: бюджет выводится из них, и
// выдуманное значение проверяло бы арифметику теста, а не актора.
func spendTheHour(t *testing.T, e *Engine, now time.Time, blackout time.Duration) {
	t.Helper()

	hourStart := now.UTC().Truncate(time.Hour)
	for at := hourStart; at.Before(now); at = at.Add(tickInterval) {
		e.hour.Sample(at, decimal.Zero)
	}

	snapshot := e.hour.Assess(now)
	require.Equal(t, uptime.Earning, snapshot.Mode,
		"the budget only means anything while the hour is still being earned")
	require.Less(t, snapshot.DowntimeBudget, blackout,
		"the fixture has to leave less budget than the blackout needs")
}

var _ ports.Metrics = ports.NopMetrics{}
