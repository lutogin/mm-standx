package main

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Касание определяется так же, как его считает теневой режим: рынок дошёл
// до нашей цены, когда встречная сторона стакана её пересекла.
func TestTouchIsCountedWhenTheOppositeSideReachesTheQuote(t *testing.T) {
	marks := []sample{{at: at(0), price: 100}}
	// Котировка на 10 bps: покупка по 99.9, продажа по 100.1.
	tops := []top{
		{at: at(100), bid: 99.95, ask: 100.05}, // рынок внутри, касания нет
		{at: at(200), bid: 99.85, ask: 99.90},  // лучшая продажа дошла до нашей покупки
	}

	events := touches(marks, tops, 10, touchCooldown)

	require.Len(t, events, 1)
	require.Equal(t, "buy", events[0].side)
	require.Equal(t, at(200), events[0].at)
}

func TestTouchIsCountedOnTheSellSideToo(t *testing.T) {
	marks := []sample{{at: at(0), price: 100}}
	tops := []top{{at: at(100), bid: 100.15, ask: 100.20}}

	events := touches(marks, tops, 10, touchCooldown)

	require.Len(t, events, 1)
	require.Equal(t, "sell", events[0].side)
}

// Стакан приходит несколько раз в секунду. Без склейки один свип давал бы
// десятки «касаний», и всякая частота ниже раздувалась бы во столько же раз.
func TestTouchBurstCollapsesIntoOneEvent(t *testing.T) {
	marks := []sample{{at: at(0), price: 100}}
	var tops []top
	for i := range 10 {
		tops = append(tops, top{at: at(100 + i*100), bid: 99.85, ask: 99.88})
	}

	events := touches(marks, tops, 10, 3*time.Second)

	require.Len(t, events, 1, "one sweep is one event")
}

// Котировка переанкеривается на каждом такте mark: та же вершина стакана
// после сдвига mark уже не касается.
func TestTouchFollowsTheMovingMark(t *testing.T) {
	marks := []sample{{at: at(0), price: 100}, {at: at(150), price: 99.8}}
	tops := []top{{at: at(200), bid: 99.75, ask: 99.78}}

	require.Empty(t, touches(marks, tops, 10, touchCooldown),
		"the quote moved down with the mark and is no longer in the way")
}

// Стакан, пришедший раньше первой справочной цены, не с чем сравнивать.
func TestTouchIgnoresBookBeforeTheFirstMark(t *testing.T) {
	marks := []sample{{at: at(500), price: 100}}
	tops := []top{{at: at(100), bid: 99.0, ask: 99.1}}

	require.Empty(t, touches(marks, tops, 10, touchCooldown))
}

// Экскурсия, а не знаковое изменение: движение, ушедшее и вернувшееся,
// для стоящего ордера так же опасно, а знаковая мера оценила бы его нулём.
func TestTriggerFiresOnARoundTrip(t *testing.T) {
	refs := []sample{
		{at: at(0), price: 100},
		{at: at(200), price: 100.06}, // +6 bps
		{at: at(400), price: 100.00}, // и обратно
	}

	fired := triggers(refs, time.Second, 5, triggerCooldown)

	require.NotEmpty(t, fired, "a move out and back is still a move")
	require.Equal(t, at(200), fired[0])
}

func TestTriggerRespectsTheThreshold(t *testing.T) {
	refs := []sample{
		{at: at(0), price: 100},
		{at: at(200), price: 100.03}, // 3 bps
	}

	require.Empty(t, triggers(refs, time.Second, 5, triggerCooldown))
	require.NotEmpty(t, triggers(refs, time.Second, 2, triggerCooldown))
}

// Окно скользящее: движение, вышедшее за его край, перестаёт считаться.
func TestTriggerForgetsMovesOlderThanTheWindow(t *testing.T) {
	refs := []sample{
		{at: at(0), price: 100},
		{at: at(100), price: 100.06},
		{at: at(5000), price: 100.06}, // прежний скачок уже вне односекундного окна
	}

	fired := triggers(refs, time.Second, 5, 100*time.Millisecond)

	require.Len(t, fired, 1)
	require.Equal(t, at(100), fired[0])
}

// Предупреждение, пришедшее позже, чем бот успевает отменить ордер, —
// не предупреждение.
func TestWarningTooLateToActOnDoesNotCount(t *testing.T) {
	events := []touchEvent{{at: at(10_000), side: "buy"}}

	inTime := assess(events, []time.Time{at(9_000)}, 6, time.Hour, 5*time.Second, 400*time.Millisecond)
	require.Equal(t, 1.0, inTime.warned)
	require.InDelta(t, 1000.0, inTime.leadMs, 1)

	tooLate := assess(events, []time.Time{at(9_800)}, 6, time.Hour, 5*time.Second, 400*time.Millisecond)
	require.Equal(t, 0.0, tooLate.warned)
}

// Слишком давнее срабатывание тоже не считается: к моменту касания бот уже
// вернулся бы в стакан.
func TestWarningOlderThanTheLookbackDoesNotCount(t *testing.T) {
	events := []touchEvent{{at: at(10_000), side: "buy"}}

	c := assess(events, []time.Time{at(1_000)}, 6, time.Hour, 5*time.Second, 400*time.Millisecond)

	require.Equal(t, 0.0, c.warned)
}

// Из нескольких подходящих срабатываний берётся последнее: раннее поглощается
// поздним, и медиана опережения иначе была бы завышена.
func TestClosestUsableWarningIsTheOneCounted(t *testing.T) {
	events := []touchEvent{{at: at(10_000), side: "buy"}}

	c := assess(events, []time.Time{at(6_000), at(9_000)}, 6, time.Hour, 5*time.Second, 400*time.Millisecond)

	require.Equal(t, 1.0, c.warned)
	require.InDelta(t, 1000.0, c.leadMs, 1)
}

// Цена защиты считается всегда: порог, снимающий котировки шестьдесят раз
// в час, «предупреждает» обо всём и не годится ни на что.
func TestTriggerRateIsReportedAlongsideCoverage(t *testing.T) {
	events := []touchEvent{{at: at(10_000), side: "buy"}}
	fired := []time.Time{at(1_000), at(9_000), at(20_000)}

	c := assess(events, fired, 2, 30*time.Minute, 5*time.Second, 400*time.Millisecond)

	require.Equal(t, 3, c.count)
	require.InDelta(t, 6.0, c.triggersPerHour, 0.01)
}
