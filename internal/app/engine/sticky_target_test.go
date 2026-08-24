package engine

import (
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"

	"github.com/nd/mm-standx/internal/config"
	"github.com/nd/mm-standx/internal/domain/market"
	"github.com/nd/mm-standx/internal/domain/money"
	"github.com/nd/mm-standx/internal/domain/policy"
	"github.com/nd/mm-standx/internal/domain/quotes"
	"github.com/nd/mm-standx/internal/domain/reward"
)

// stickyEngine — минимальный актор для проверки одного правила.
//
// Собран полем, а не конструктором, намеренно: stickyTarget — чистая функция
// от конфигурации и запомненной цели, и заводить ради неё шлюз, часы и оценщик
// волатильности значило бы проверять их, а не её.
func stickyEngine(relaxAfter time.Duration) *Engine {
	return &Engine{
		cfg: config.Symbol{
			Appetite: config.Appetite{
				MinDeadBandBps:   1.2,
				TargetRelaxAfter: relaxAfter,
			},
		},
	}
}

func feasible(distance money.Bps) policy.Quote {
	return policy.Quote{Distance: distance, Feasible: true}
}

// Цель обязана вернуться к полу, когда всплеск прошёл.
//
// Тот самый разрыв, ради которого правка сделана. Свежая цель снизу зажата
// полом minBps плюс половина ширины лесенки — на золоте это 7.4. Всплеск,
// отодвинувший цель на 8.4, оставляет до пола ровно 1.0, то есть меньше
// minDeadBandBps в 1.2: порога расхождения не хватает, чтобы разрешить возврат,
// а меньше пола требование не бывает. Без отсчёта времени цель стоит снаружи
// до следующего перезапуска — на боевом счёте это наблюдалось двадцать часов
// подряд.
//
// Отвергнутая альтернатива названа здесь же вторым прогоном: targetRelaxAfter
// в ноль — это прежнее поведение, и оно обязано провалить первую половину.
func TestTargetReturnsToTheFloorAfterTheSpikePasses(t *testing.T) {
	start := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)

	t.Run("with a relax window the target comes back", func(t *testing.T) {
		e := stickyEngine(time.Minute)
		e.target = feasible(8.4)

		require.EqualValues(t, 8.4, e.stickyTarget(start, feasible(7.4)).Distance,
			"мгновенный возврат вернул бы дрожание, ради которого зона и заведена")
		require.EqualValues(t, 8.4, e.stickyTarget(start.Add(59*time.Second), feasible(7.4)).Distance)
		require.EqualValues(t, 7.4, e.stickyTarget(start.Add(time.Minute), feasible(7.4)).Distance,
			"минута спокойствия — это уже не дрожание")
	})

	t.Run("without one it never does", func(t *testing.T) {
		e := stickyEngine(0)
		e.target = feasible(8.4)

		for _, after := range []time.Duration{0, time.Minute, time.Hour, 24 * time.Hour} {
			require.EqualValues(t, 8.4, e.stickyTarget(start.Add(after), feasible(7.4)).Distance,
				"храповик: расхождение 1.0 меньше порога 1.2, а меньше пола требование не опустится")
		}
	})
}

// Осторожность применяется немедленно, а отсчёт возврата начинается заново.
//
// Иначе всплеск, пришедший на пятьдесят девятой секунде, всё равно пустил бы
// лесенку внутрь на шестидесятой.
func TestWideningAppliesAtOnceAndRestartsTheCountdown(t *testing.T) {
	start := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	e := stickyEngine(time.Minute)
	e.target = feasible(8.4)

	require.EqualValues(t, 8.4, e.stickyTarget(start, feasible(7.4)).Distance)
	require.EqualValues(t, 9.0, e.stickyTarget(start.Add(30*time.Second), feasible(9.0)).Distance,
		"отойти дальше разрешено сразу")

	require.EqualValues(t, 9.0, e.stickyTarget(start.Add(31*time.Second), feasible(8.2)).Distance)
	require.EqualValues(t, 9.0, e.stickyTarget(start.Add(90*time.Second), feasible(8.2)).Distance,
		"отсчёт пошёл заново от момента расширения")
	require.EqualValues(t, 8.2, e.stickyTarget(start.Add(91*time.Second), feasible(8.2)).Distance)
}

// Требование, дотянувшееся до стоящей цели, обнуляет отсчёт: отступать
// больше не от чего, и следующий возврат обязан ждать полную минуту заново.
func TestTouchingTheStandingTargetResetsTheCountdown(t *testing.T) {
	start := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	e := stickyEngine(time.Minute)
	e.target = feasible(8.4)

	require.EqualValues(t, 8.4, e.stickyTarget(start, feasible(7.4)).Distance)
	require.EqualValues(t, 8.4, e.stickyTarget(start.Add(50*time.Second), feasible(8.4)).Distance)

	require.EqualValues(t, 8.4, e.stickyTarget(start.Add(51*time.Second), feasible(7.4)).Distance)
	require.EqualValues(t, 8.4, e.stickyTarget(start.Add(110*time.Second), feasible(7.4)).Distance,
		"минута отсчитывается от последнего касания, а не от первого расхождения")
	require.EqualValues(t, 7.4, e.stickyTarget(start.Add(111*time.Second), feasible(7.4)).Distance)
}

// Крупное расхождение не ждёт ничего: срок нужен там, где порога не хватает.
func TestLargeNarrowingStillAppliesImmediately(t *testing.T) {
	start := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	e := stickyEngine(time.Minute)
	e.target = feasible(9.0)

	require.EqualValues(t, 7.4, e.stickyTarget(start, feasible(7.4)).Distance,
		"расхождение 1.6 больше порога 1.2 — ждать нечего")
}

// Замер часа берёт полосу у программы наград, а не у границ котирования.
//
// Это разные числа: bounds.MaxBps — докуда мы согласны отпускать ордер прежде
// чем переставить, Curve.BandBps — докуда за него платит биржа. Первое строго
// меньше, и разница списывалась в ноль. Выпадание за 9.5 — самый частый повод
// к перестановке (18 882 раза за прогон), так что вычиталось это постоянно,
// и оценка часа выходила заниженной против начисленного биржей: 1.05-1.22
// против 1.81-1.98 по отчёту StandX за 24 августа.
func TestHourSampleCountsRungsPaidByTheExchangeBeyondOurQuotingBound(t *testing.T) {
	e := &Engine{
		program: reward.Program{
			Curve:      reward.Curve{BandBps: 10, MaxWeightPct: 200, MinWeightPct: 40},
			UnitSize:   decimal.NewFromInt(30),
			PerSideCap: decimal.NewFromInt(60),
		},
		bounds: policy.Bounds{MinBps: 7.0, MaxBps: 9.5},
	}
	e.mark = money.MustParsePrice("4000")

	// 9.7 bps под mark: за нашей границей перестановки, но внутри полосы биржи.
	bids := quotes.New(market.Buy, 1)
	bids.Slots[0] = quotes.Slot{
		Phase: quotes.Live,
		Price: money.MustParsePrice("3996.12"),
		Qty:   money.MustParseQty("10"),
	}

	orders := e.sideOrders(bids)
	require.Len(t, orders, 1, "ступень внутри полосы награды обязана попасть в замер")
	require.InDelta(t, 9.7, float64(orders[0].DistanceBps), 0.05)

	// За полосой награды вес обнуляется, и там нулю самое место.
	outside := quotes.New(market.Buy, 1)
	outside.Slots[0] = quotes.Slot{
		Phase: quotes.Live,
		Price: money.MustParsePrice("3995.6"), // 11 bps
		Qty:   money.MustParseQty("10"),
	}
	require.Empty(t, e.sideOrders(outside), "за полосой биржа не платит, и замер не вправе считать")
}
