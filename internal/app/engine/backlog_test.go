package engine

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nd/mm-standx/internal/domain/market"
	"github.com/nd/mm-standx/internal/domain/money"
	"github.com/nd/mm-standx/internal/events"
)

// Быстрый поток не имеет права копиться в очереди актора.
//
// 13 августа копился: стакан и внешний ориентир лежали в той же очереди, что
// исполнения и обновления ордеров, заняли все 1024 слота, и первое же событие
// о собственном ордере не поместилось. Актор остановился до конца прогона.
//
// Тесты этого файла закрывают именно свойство «копиться нечему», а не «актор
// успевает». Второе — обещание о скорости, которое ломается молча; первое —
// свойство устройства, которое ломается компилятором или этими тестами.

// snapshot собирает двусторонний снимок стакана.
func snapshot(sym market.Symbol, bid, ask string, at time.Time) market.Book {
	return market.NewBook(sym,
		[]market.Level{{Price: money.MustParsePrice(bid), Qty: money.MustParseQty("1")}},
		[]market.Level{{Price: money.MustParsePrice(ask), Qty: money.MustParseQty("1")}},
		at)
}

// subscribed включает подписки актора и снимает их по завершении теста.
func subscribed(t *testing.T, e *Engine) {
	t.Helper()
	e.subscribe()
	t.Cleanup(func() {
		for _, un := range e.unsubscribe {
			un()
		}
	})
}

// Сколько бы стакан ни приходил, очередь актора не растёт ни на одно сообщение.
func TestBookFloodNeverEntersTheActorQueue(t *testing.T) {
	e, _, clk := dangerEngine(t)
	subscribed(t, e)

	const flood = 50_000
	for range flood {
		events.Publish(e.deps.Bus, context.Background(), events.BookUpdated{
			Book: snapshot(e.symbol, "4275", "4277", clk.now),
		})
		events.Publish(e.deps.Bus, context.Background(), events.ReferencePriceUpdated{
			Reference: market.ReferencePrice{
				Symbol: e.symbol, Price: money.MustParsePrice("4276"), Received: clk.now,
			},
		})
	}

	require.Zero(t, len(e.inbox),
		"the market firehose must not occupy a single slot of the actor queue")
	require.Zero(t, e.dropped.Load(),
		"nothing was dropped because nothing was queued")
	require.LessOrEqual(t, len(e.wake), 1,
		"wake-ups coalesce: one pending signal, not one per message")
}

// Даже при забитой под завязку очереди быстрый поток не роняет актор.
//
// Это ровно та ситуация 13 августа: очередь полна, приходит стакан. Раньше он
// попадал в enqueue, отбрасывался и — если бы это было событие счёта — вешал
// актор навсегда. Теперь он до очереди вообще не доходит.
func TestBookFloodIsHarmlessWhileTheQueueIsFull(t *testing.T) {
	ctx := context.Background()
	e, _, clk := dangerEngine(t)
	subscribed(t, e)

	// Очередь заполняется событиями счёта — теми, что в неё и должны попадать.
	fillInbox(e)
	occupied := len(e.inbox)

	for range 10_000 {
		events.Publish(e.deps.Bus, ctx, events.BookUpdated{
			Book: snapshot(e.symbol, "4275", "4277", clk.now),
		})
	}
	require.Equal(t, occupied, len(e.inbox),
		"a full queue must not even be offered market data, let alone shed it")

	e.markAt = clk.now
	e.onTick(ctx)

	require.Zero(t, e.dropped.Load(),
		"market data never reaches the queue, so no own-account event is ever lost to it")
	require.True(t, e.resyncUntil.IsZero(), "and therefore never pauses quoting")
	require.False(t, e.halted, "and never halts the actor")
}

// Схлопывание не теряет последнюю величину — только промежуточные, которые
// следующее сообщение приносит заново.
func TestCoalescingKeepsTheNewestSnapshot(t *testing.T) {
	ctx := context.Background()
	e, _, clk := dangerEngine(t)
	subscribed(t, e)

	for _, bid := range []string{"4270", "4271", "4272"} {
		events.Publish(e.deps.Bus, ctx, events.BookUpdated{
			Book: snapshot(e.symbol, bid, "4277", clk.now),
		})
	}
	events.Publish(e.deps.Bus, ctx, events.ReferencePriceUpdated{
		Reference: market.ReferencePrice{
			Symbol: e.symbol, Price: money.MustParsePrice("4299"), Received: clk.now,
		},
	})

	e.onMarket(ctx)

	best, ok := e.book.BestBid()
	require.True(t, ok)
	require.Equal(t, "4272", best.String(), "the actor sees the newest book, not the oldest")
	require.Equal(t, "4299", e.reference.String())

	// Ячейки опустошены: повторное пробуждение не должно пересчитывать заново.
	require.Nil(t, e.bookIn.Load())
	require.Nil(t, e.referenceIn.Load())
}

// Односторонний снимок по-прежнему отбрасывается, а не затирает рабочий стакан.
//
// Свойство существовало до разделения входа и обязано было его пережить:
// затёртый стакан делает halfSpread нечитаемым, и актор перестаёт котировать
// до следующего полного снимка.
func TestOneSidedSnapshotStillDoesNotOverwriteTheBook(t *testing.T) {
	ctx := context.Background()
	e, _, _ := dangerEngine(t)

	before, ok := e.book.BestBid()
	require.True(t, ok)

	e.bookIn.Store(&market.Book{
		Symbol: e.symbol,
		Bids:   []market.Level{{Price: money.MustParsePrice("4275"), Qty: money.MustParseQty("1")}},
	})
	e.onMarket(ctx)

	after, ok := e.book.BestBid()
	require.True(t, ok)
	require.Equal(t, before.String(), after.String(), "a half snapshot is discarded, not applied")
}

// Опоздавший такт mark не имеет права стереть быстрый сигнал.
//
// Отставший актор сперва применяет свежайший ориентир из ячейки и лишь потом
// достаёт из очереди такт, снятый секунду назад. Заякорить такт на текущем
// значении — значит объявить снос нулевым: оценка схлопывается к самому mark,
// движение за время отставания исчезает, и котировка, которую рынок уже прошёл,
// проходит как безопасная. До разделения входа это было невозможно — очередь
// была общей, и к моменту такта в полях лежало последнее, что пришло до него.
func TestALateMarkDoesNotEraseTheFastSignal(t *testing.T) {
	ctx := context.Background()
	e, _, clk := dangerEngine(t)

	e.refStaleAfter = 3 * time.Second

	tick := clk.now
	// Предыдущий такт: он задаёт интервал, с которым сравнивается опоздание.
	e.markAt = tick.Add(-1700 * time.Millisecond)
	e.referenceAtMark = money.MustParsePrice("4276")

	// Актор отстал на две секунды, рынок за это время ушёл на 25 bps вверх.
	clk.advance(2 * time.Second)
	e.reference = money.MustParsePrice("4286.69")
	e.referenceAt = clk.now

	// И только теперь из очереди достаётся такт двухсекундной давности.
	e.onMarkPrice(ctx, events.MarkPriceUpdated{
		Mark: market.MarkPrice{Symbol: e.symbol, Price: money.MustParsePrice("4276"), Received: tick},
	})

	drift, ok := e.referenceDrift()
	require.True(t, ok, "the reference is fresh, so drift must be available")
	require.Greater(t, float64(drift), 20.0,
		"the 25 bps the market moved while we were behind must survive the late tick")

	require.NotEqual(t, "4286.69", e.referenceAtMark.String(),
		"anchoring on data newer than the tick is what erases the signal")
}

// В обычном случае — актор не отстаёт — опора обновляется как прежде.
func TestAPromptMarkReanchorsAsBefore(t *testing.T) {
	ctx := context.Background()
	e, _, clk := dangerEngine(t)

	e.reference = money.MustParsePrice("4280")
	e.referenceAt = clk.now.Add(-5 * time.Millisecond)

	e.onMarkPrice(ctx, events.MarkPriceUpdated{
		Mark: market.MarkPrice{Symbol: e.symbol, Price: money.MustParsePrice("4276"), Received: clk.now},
	})

	require.Equal(t, "4280", e.referenceAtMark.String(),
		"a tick that arrived after the reference anchors on it, as it always did")
}

// Обычный джиттер разбора очереди не имеет права удерживать прежнюю опору.
//
// Первая версия сторожа сравнивала отметки напрямую и срабатывала на любом
// опережении: на проде это дало 12% тактов с лагами 0–100 мс, кратными
// интервалу между сообщениями Binance. В этом режиме удержание хуже якорения
// в семнадцать раз — ошибка от лага в 0.1 с против ошибки за целый такт
// в 1.7 с, — то есть сторож систематически завышал снос и толкал в лишние
// аварийные съёмы.
func TestOrdinaryJitterStillReanchors(t *testing.T) {
	ctx := context.Background()
	e, _, clk := dangerEngine(t)
	e.refStaleAfter = 3 * time.Second

	tick := clk.now
	e.markAt = tick.Add(-1700 * time.Millisecond)
	e.referenceAtMark = money.MustParsePrice("4276")

	// Ориентир опередил такт на сотню миллисекунд — джиттер, а не затык.
	clk.advance(100 * time.Millisecond)
	e.reference = money.MustParsePrice("4276.5")
	e.referenceAt = clk.now

	e.onMarkPrice(ctx, events.MarkPriceUpdated{
		Mark: market.MarkPrice{Symbol: e.symbol, Price: money.MustParsePrice("4276"), Received: tick},
	})

	require.Equal(t, "4276.5", e.referenceAtMark.String(),
		"a lag far shorter than the tick must re-anchor, not hold a whole tick of stale drift")
}
