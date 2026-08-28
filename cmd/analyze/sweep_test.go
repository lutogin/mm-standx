package main

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Мелкое движение доходит до ближней ступени и останавливается.
func TestSweepStoppingAtTheNearRungIsDepthOne(t *testing.T) {
	marks := []sample{{at: at(0), price: 100}}
	// Ступени бида на 7.0 / 7.4 / 7.8 bps: 99.930, 99.926, 99.922.
	tops := []top{{at: at(100), bid: 99.90, ask: 99.929}}

	episodes := sweeps(marks, tops, ladderDistances(7, 0.4, 3), 2*time.Second, touchCooldown)

	require.Len(t, episodes, 1)
	require.Equal(t, "buy", episodes[0].side)
	require.Equal(t, 1, episodes[0].depth)
}

// Свип сквозь всю сторону — тот исход, при котором лесенка не покупает ничего.
func TestSweepThroughEveryRungIsFullDepth(t *testing.T) {
	marks := []sample{{at: at(0), price: 100}}
	tops := []top{{at: at(100), bid: 99.85, ask: 99.90}}

	episodes := sweeps(marks, tops, ladderDistances(7, 0.4, 3), 2*time.Second, touchCooldown)

	require.Len(t, episodes, 1)
	require.Equal(t, 3, episodes[0].depth)
}

// Углубление внутри одного окна — продолжение того же свипа, а не второй.
//
// Считать его вторым касанием значило бы объявить лесенку защитной ровно там,
// где она не сработала: два события глубины один вместо одного глубины два.
func TestDeepeningInsideTheWindowExtendsTheSameEpisode(t *testing.T) {
	marks := []sample{{at: at(0), price: 100}}
	tops := []top{
		{at: at(100), bid: 99.90, ask: 99.929}, // near rung
		{at: at(300), bid: 99.88, ask: 99.921}, // and on through the rest
	}

	episodes := sweeps(marks, tops, ladderDistances(7, 0.4, 3), 2*time.Second, touchCooldown)

	require.Len(t, episodes, 1, "one move is one episode")
	require.Equal(t, 3, episodes[0].depth)
}

// ЛЕСЕНКА ВО ВРЕМЯ СВИПА НЕ ДВИГАЕТСЯ, И МОДЕЛЬ НЕ ИМЕЕТ ПРАВА ДВИГАТЬ ЕЁ ТОЖЕ.
//
// Такт mark идёт раз в три секунды, а свип 6 августа занял 51 мс. Пересчёт
// ступеней по пришедшему следом такту увёл бы дальние от наступающего рынка
// и показал бы свип мельче, чем он был.
func TestRungsStayAnchoredWhileTheSweepRuns(t *testing.T) {
	// Второй такт двигает mark вслед за рынком; ступени обязаны остаться
	// на цене, посчитанной в момент первого касания.
	marks := []sample{{at: at(0), price: 100}, {at: at(150), price: 99.9}}
	tops := []top{
		{at: at(100), bid: 99.90, ask: 99.929},
		{at: at(200), bid: 99.88, ask: 99.921},
	}

	episodes := sweeps(marks, tops, ladderDistances(7, 0.4, 3), 2*time.Second, touchCooldown)

	require.Len(t, episodes, 1)
	require.Equal(t, 3, episodes[0].depth,
		"the ladder that was standing there is the one the market swept")
}

// Два движения, разделённые окном и паузой, — два эпизода.
func TestSeparateMovesAreSeparateEpisodes(t *testing.T) {
	marks := []sample{{at: at(0), price: 100}}
	tops := []top{
		{at: at(100), bid: 99.90, ask: 99.929},
		{at: at(6000), bid: 99.90, ask: 99.929},
	}

	episodes := sweeps(marks, tops, ladderDistances(7, 0.4, 3), 2*time.Second, touchCooldown)

	require.Len(t, episodes, 2)
}

// Обе стороны учитываются независимо: их достают противоположные движения,
// и одним свипом они быть не могут.
func TestBothSidesAreTrackedIndependently(t *testing.T) {
	marks := []sample{{at: at(0), price: 100}}
	tops := []top{
		{at: at(100), bid: 99.90, ask: 99.929},   // reaches the bid ladder
		{at: at(6000), bid: 100.075, ask: 100.2}, // reaches the ask ladder
	}

	episodes := sweeps(marks, tops, ladderDistances(7, 0.4, 3), 2*time.Second, touchCooldown)

	require.Len(t, episodes, 2)
	require.Equal(t, "buy", episodes[0].side)
	require.Equal(t, "sell", episodes[1].side)
}

// Размер стороны считается по среднему весу ступеней, а не по весу ближней:
// начисление — сумма по всем, а размер делится поровну. Это та же поправка,
// что несёт sizing.usefulCeiling, и она же решает сравнение геометрий.
func TestSizeForTheSameAccrualGrowsWithTheLadder(t *testing.T) {
	one, ok := sizePerCap(7, 0.4, 1)
	require.True(t, ok)
	three, ok := sizePerCap(7, 0.4, 3)
	require.True(t, ok)

	// Веса 0.880 / 0.816 / 0.752: средний 0.816 против 0.880 у одиночного.
	require.InDelta(t, 1/0.880, one, 0.0001)
	require.InDelta(t, 1/0.816, three, 0.0001)
	require.InDelta(t, 0.927, one/three, 0.001,
		"a single order reaches the same accrual on about 7% less size")
}

// Ступень за краем полосы не приносит ничего, и геометрия, которая её содержит,
// сравнению не подлежит: делить было бы не на что.
func TestLadderReachingOutsideTheBandIsRejected(t *testing.T) {
	_, ok := sizePerCap(9.5, 0.4, 3)
	require.False(t, ok, "the outer rung at 10.3 bps earns nothing")
}

func TestDepthProfileReportsTheMeanAndTheShareOfFullSweeps(t *testing.T) {
	episodes := []sweepEpisode{{depth: 1}, {depth: 3}, {depth: 3}, {depth: 1}}

	mean, full := depthProfile(episodes, 3)

	require.InDelta(t, 2.0, mean, 0.0001)
	require.InDelta(t, 0.5, full, 0.0001)
}
