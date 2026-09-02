package liquidity

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nd/mm-standx/internal/domain/market"
)

var epoch = time.Date(2026, 9, 1, 22, 0, 0, 0, time.UTC)

func cfg() Config {
	return Config{Window: 30 * time.Minute, MinSamples: 40, ThinFraction: 0.70}
}

// fill кормит оценку ровной глубиной, чтобы норма была известна.
func fill(e *Estimator, side market.Side, depth float64, n int) time.Time {
	at := epoch
	for range n {
		e.Observe(side, depth, at)
		at = at.Add(time.Second)
	}
	return at
}

// Обычный стакан тонким не считается.
func TestNormalDepthIsNotThin(t *testing.T) {
	e := New(cfg())
	at := fill(e, market.Sell, 6_000_000, 60)

	e.Observe(market.Sell, 5_800_000, at)
	v := e.Verdict(market.Sell, at)
	require.True(t, v.Ready)
	require.False(t, v.Thin)
}

// Провал глубины ниже доли от нормы закрывает сторону.
//
// Числа — эпизод 1 сентября 2026, 22:00:58: глубина 0.23 от собственной
// медианы, залив на 10 XAU через считанные секунды после постановки.
func TestCollapsedDepthIsThin(t *testing.T) {
	e := New(cfg())
	at := fill(e, market.Sell, 6_000_000, 60)

	e.Observe(market.Sell, 1_380_000, at) // 0.23 от нормы
	v := e.Verdict(market.Sell, at)
	require.True(t, v.Thin)
	require.InDelta(t, 4_200_000, v.ThresholdUSD, 1)
}

// Порог 0.70 обязан ловить и первый из двух десятитонных заливов — тот,
// что стоял на 0.62 от нормы и при 0.60 проходил бы мимо ворот.
func TestThresholdCatchesTheSixtyTwoPercentCase(t *testing.T) {
	e := New(cfg())
	at := fill(e, market.Sell, 6_000_000, 60)

	e.Observe(market.Sell, 3_720_000, at) // 0.62
	require.True(t, e.Verdict(market.Sell, at).Thin)

	loose := New(Config{Window: 30 * time.Minute, MinSamples: 40, ThinFraction: 0.60})
	at2 := fill(loose, market.Sell, 6_000_000, 60)
	loose.Observe(market.Sell, 3_720_000, at2)
	require.False(t, loose.Verdict(market.Sell, at2).Thin,
		"0.60 lets this one through — that is why the default is 0.70")
}

// Стороны судятся независимо: тонкий аск не закрывает бид.
func TestSidesAreIndependent(t *testing.T) {
	e := New(cfg())
	at := fill(e, market.Sell, 6_000_000, 60)
	at = fill(e, market.Buy, 6_000_000, 60)

	e.Observe(market.Sell, 500_000, at)
	e.Observe(market.Buy, 6_100_000, at)

	require.True(t, e.Verdict(market.Sell, at).Thin)
	require.False(t, e.Verdict(market.Buy, at).Thin)
}

// Незрелая оценка не имеет права держать актора вне рынка.
func TestImmatureWindowNeverBlocks(t *testing.T) {
	e := New(cfg())
	at := fill(e, market.Sell, 6_000_000, 5)
	e.Observe(market.Sell, 1, at)

	v := e.Verdict(market.Sell, at)
	require.False(t, v.Ready)
	require.False(t, v.Thin, "silence about the book is not a reason to leave it")
}

// Ноль в доле выключает проверку целиком.
func TestZeroFractionDisablesTheGate(t *testing.T) {
	off := Config{Window: 30 * time.Minute, MinSamples: 40}
	e := New(off)
	at := fill(e, market.Sell, 6_000_000, 60)
	e.Observe(market.Sell, 1, at)
	require.False(t, e.Verdict(market.Sell, at).Thin)
}

// Наблюдения старше окна забываются, иначе норма вечно помнит вчерашний рынок.
func TestWindowForgetsOldSamples(t *testing.T) {
	e := New(cfg())
	fill(e, market.Sell, 10_000_000, 60)

	at := epoch.Add(2 * time.Hour)
	for range 60 {
		e.Observe(market.Sell, 2_000_000, at)
		at = at.Add(time.Second)
	}
	v := e.Verdict(market.Sell, at)
	require.InDelta(t, 2_000_000, v.MedianUSD, 1,
		"the norm must be today's book, not a two-hour-old one")
	require.False(t, v.Thin)
}

// Медиана, а не среднее: одна крупная заявка не имеет права поднять порог.
func TestOneWhaleDoesNotMoveTheNorm(t *testing.T) {
	e := New(cfg())
	at := fill(e, market.Sell, 6_000_000, 60)
	e.Observe(market.Sell, 900_000_000, at)
	at = at.Add(time.Second)

	e.Observe(market.Sell, 5_500_000, at)
	require.False(t, e.Verdict(market.Sell, at).Thin,
		"a mean would have declared an ordinary book thin")
}

// Замеры чаще SampleEvery отбрасываются.
//
// Такт актора идёт по рыночным данным — 3.4 обновления стакана в секунду
// по замеру 1 сентября 2026. Без прореживания окно меряло бы частоту событий,
// а не время: MinSamples в 300 набирался бы за полторы минуты вместо пяти.
func TestSampleEveryThinsTheStream(t *testing.T) {
	c := cfg()
	c.SampleEvery = time.Second
	e := New(c)

	at := epoch
	for range 100 {
		e.Observe(market.Sell, 6_000_000, at)
		at = at.Add(250 * time.Millisecond) // четыре раза в секунду, как в бою
	}
	// 100 вызовов за 25 секунд дают около 25 замеров, а не 100.
	require.Less(t, len(e.asks), 30)
	require.Greater(t, len(e.asks), 20)
}

// Окно меряет время: при прореживании его ёмкость задана длительностью,
// а не тем, как часто рынок шевелится.
func TestWindowLengthIsTimeNotEventCount(t *testing.T) {
	c := Config{Window: 10 * time.Minute, SampleEvery: time.Second,
		MinSamples: 300, ThinFraction: 0.70}
	e := New(c)

	at := epoch
	for range 6000 { // 25 минут при четырёх вызовах в секунду
		e.Observe(market.Sell, 6_000_000, at)
		at = at.Add(250 * time.Millisecond)
	}
	require.LessOrEqual(t, len(e.asks), 601,
		"a ten-minute window sampled once a second holds ten minutes of samples")
}

// Прореживание не мешает оценке созреть и сработать.
func TestThrottledEstimateStillGates(t *testing.T) {
	c := Config{Window: 10 * time.Minute, SampleEvery: time.Second,
		MinSamples: 300, ThinFraction: 0.70}
	e := New(c)

	at := epoch
	for range 400 {
		e.Observe(market.Sell, 6_000_000, at)
		at = at.Add(time.Second)
	}
	require.True(t, e.Verdict(market.Sell, at).Ready)

	e.Observe(market.Sell, 1_380_000, at)
	require.True(t, e.Verdict(market.Sell, at).Thin)
}
