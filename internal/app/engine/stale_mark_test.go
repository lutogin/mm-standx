package engine

import (
	"bytes"
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nd/mm-standx/internal/domain/market"
	"github.com/nd/mm-standx/internal/domain/trading"
	"github.com/nd/mm-standx/internal/events"
)

// staleMarkEngine — актор с поднятым ориентиром и включённым удержанием.
//
// referenceEngine ставит опорную точку ориентира настоящим тактом mark:
// 4400 на ориентире против 4276 на бирже. Смещение считается от этой точки,
// а не от разницы площадок, поэтому абсолютные цены здесь роли не играют.
func staleMarkEngine(t *testing.T) (*Engine, *recordingGateway, *testClock, *bytes.Buffer) {
	t.Helper()
	e, gw, clk := referenceEngine(t)
	e.health.MarkStale = 10 * time.Second
	e.health.MarkStaleHoldBps = 5

	var buf bytes.Buffer
	e.deps.Log = slog.New(slog.NewTextHandler(&buf, nil))
	return e, gw, clk, &buf
}

// armStaleMark переводит стенд в состояние «mark молчит, ориентир свежий»
// и только после этого ставит ступени в стакан.
//
// Порядок здесь — предмет, а не оформление. Приход цены ориентира сам по себе
// вызывает пересчёт, и он переставляет лесенку; ступени, выставленные до него,
// к моменту события о молчании уже сняты, и тест доказывал бы не то, что нужно.
func armStaleMark(e *Engine, gw *recordingGateway, clk *testClock, reference string) {
	clk.advance(11 * time.Second)
	pushReference(e, reference, clk.now)
	liveRung(e, market.Buy, "bid-1", "4273", clk.now)
	liveRung(e, market.Sell, "ask-1", "4279", clk.now)
	gw.cancelled = nil
}

// staleMark подаёт событие о молчании канала официальной цены.
func staleMark(e *Engine, clk *testClock) {
	e.handle(context.Background(), events.MarkPriceStale{
		Symbol: "XAU-USD", Since: clk.now, Age: 11 * time.Second,
	})
}

// Молчащий канал mark при живом и спокойном ориентире не снимает стакан.
//
// Регрессия на измеренный убыток: 18 августа 2026 канал mark молчал по 10-12
// секунд подряд, безусловный съём дал 1514 выходов за сутки, а час 06:00 UTC
// закрылся нулём Maker Hours при открытом и спокойном рынке. Зачёт считается
// перцентилем односекундных замеров, поэтому секунда вне стакана — это ноль
// в наборе, решающем весь час, и уход обходится дороже любой другой реакции.
func TestStaleMarkHoldsTheBookWhileTheReferenceVouchesForIt(t *testing.T) {
	e, gw, clk, buf := staleMarkEngine(t)

	// Ориентир пришёл только что и стоит там же, где был на такте mark.
	armStaleMark(e, gw, clk, "4400.4")

	staleMark(e, clk)

	require.Empty(t, gw.cancelled,
		"тишина в канале публикации — не движение цены, и общего ответа у них быть не должно")
	require.Contains(t, buf.String(), "holding the resting quotes on the reference",
		"удержание, которого не видно в логе, неотличимо от умершего актора")
}

// Удержание кончается ровно там, где кончается картина: ориентир, ушедший
// за порог, больше ни за что не ручается.
func TestStaleMarkPullsTheBookOnceTheReferenceMoves(t *testing.T) {
	e, gw, clk, _ := staleMarkEngine(t)

	// 4400 → 4404 это 9.1 bps, вдвое дальше порога удержания.
	armStaleMark(e, gw, clk, "4404")

	staleMark(e, clk)

	require.ElementsMatch(t, []trading.ClOrdID{"bid-1", "ask-1"}, gw.cancelled,
		"за порогом геометрия ордеров уже не та, для которой они считались")
}

// Замолчавший ориентир опаснее отсутствующего: он выглядит как исправная
// защита, ничего не защищая. Удерживать по нему нельзя.
func TestStaleMarkPullsTheBookWhenTheReferenceIsStaleToo(t *testing.T) {
	e, gw, clk, _ := staleMarkEngine(t)

	armStaleMark(e, gw, clk, "4400")
	// refStaleAfter в этом стенде — две секунды.
	e.referenceAt = clk.now.Add(-5 * time.Second)

	staleMark(e, clk)

	require.ElementsMatch(t, []trading.ClOrdID{"bid-1", "ask-1"}, gw.cancelled)
}

// Ноль в конфиге возвращает прежнее безусловное поведение. Это не мёртвая
// ветка: она нужна, чтобы правку можно было выключить значением, а не выкаткой.
func TestZeroHoldRestoresTheUnconditionalWithdrawal(t *testing.T) {
	e, gw, clk, _ := staleMarkEngine(t)
	e.health.MarkStaleHoldBps = 0

	armStaleMark(e, gw, clk, "4400")

	staleMark(e, clk)

	require.ElementsMatch(t, []trading.ClOrdID{"bid-1", "ask-1"}, gw.cancelled)
}

// Переход печатается один раз: канал замолкает десятками раз за сутки,
// и строка на каждом такте утопила бы лог, ради которого она заведена.
func TestStaleMarkHoldIsAnnouncedOnce(t *testing.T) {
	e, _, clk, buf := staleMarkEngine(t)

	for range 3 {
		clk.advance(11 * time.Second)
		pushReference(e, "4400", clk.now)
		staleMark(e, clk)
	}

	require.Equal(t, 1, bytes.Count(buf.Bytes(), []byte("holding the resting quotes")))
}

// Официальная цена снимает удержание: пока её нет, актор не вправе выставлять,
// а как только она пришла — держать больше нечего.
func TestOfficialMarkClearsTheHold(t *testing.T) {
	e, _, clk, buf := staleMarkEngine(t)

	clk.advance(11 * time.Second)
	pushReference(e, "4400", clk.now)
	staleMark(e, clk)
	require.True(t, e.staleMarkHeld)

	e.handle(context.Background(), events.MarkPriceUpdated{
		Mark: market.MarkPrice{Symbol: "XAU-USD", Price: e.mark, Received: clk.now},
	})
	require.False(t, e.staleMarkHeld)

	clk.advance(11 * time.Second)
	pushReference(e, "4400", clk.now)
	staleMark(e, clk)
	require.Equal(t, 2, bytes.Count(buf.Bytes(), []byte("holding the resting quotes")),
		"новое молчание — новое событие, и оно обязано быть видно")
}
