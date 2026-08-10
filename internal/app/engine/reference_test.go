package engine

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"math"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"

	"github.com/nd/mm-standx/internal/domain/market"
	"github.com/nd/mm-standx/internal/domain/money"
	"github.com/nd/mm-standx/internal/domain/trading"
	"github.com/nd/mm-standx/internal/events"
	"github.com/nd/mm-standx/internal/ports"
)

// Разрыв канала биржи означает торговлю вслепую: котировки снимаются немедленно.
func TestDisconnectedExchangeStreamPullsTheBook(t *testing.T) {
	ctx := context.Background()
	e, gw, clk := dangerEngine(t)

	liveRung(e, market.Buy, "bid-1", "4273", clk.now)
	liveRung(e, market.Sell, "ask-1", "4279", clk.now)

	e.handle(ctx, events.StreamDisconnected{Stream: "market", Reason: errors.New("read: EOF"), At: clk.now})

	require.ElementsMatch(t, []trading.ClOrdID{"bid-1", "ask-1"}, gw.cancelled)
}

// Разрыв внешнего ориентира возвращает нас ровно к той картине, по которой
// актор работал до его появления: быстрый сигнал снова берётся из середины
// стакана. Снимать котировки в этом случае значило бы платить присутствием —
// единственным, за что здесь платят, — за сбой на чужой площадке.
func TestDisconnectedReferenceFeedKeepsTheBook(t *testing.T) {
	ctx := context.Background()
	e, gw, clk := dangerEngine(t)

	var buf bytes.Buffer
	e.deps.Log = slog.New(slog.NewTextHandler(&buf, nil))

	liveRung(e, market.Buy, "bid-1", "4273", clk.now)
	liveRung(e, market.Sell, "ask-1", "4279", clk.now)

	e.handle(ctx, events.StreamDisconnected{
		Stream:   "reference[XAU-USD]",
		Reason:   errors.New("read: EOF"),
		At:       clk.now,
		Optional: true,
	})

	require.Empty(t, gw.cancelled, "an advisory feed must not take the book down")
	require.Contains(t, buf.String(), "optional stream disconnected")
}

// referenceEngine — актор с поднятым внешним ориентиром.
//
// Опорные точки обоих быстрых сигналов ставятся в момент такта mark, поэтому
// тесту нужен настоящий такт, а не присвоение полей: иначе проверялось бы
// не то, как связаны сигнал и такт, а то, как тест их расставил.
func referenceEngine(t *testing.T) (*Engine, *recordingGateway, *testClock) {
	t.Helper()
	e, gw, clk := dangerEngine(t)
	e.refStaleAfter = 2 * time.Second
	// Без баланса расчёт размера падает штатно и снимает всё подряд, а тест
	// потерял бы предмет: проверяется реакция на цену, а не на пустой счёт.
	e.available = decimal.NewFromInt(1000)

	e.handle(context.Background(), events.ReferencePriceUpdated{
		Reference: market.ReferencePrice{
			Symbol: "XAU-USD", Venue: "binance_futures",
			Price: money.MustParsePrice("4400"), Received: clk.now,
		},
	})
	e.handle(context.Background(), events.MarkPriceUpdated{
		Mark: market.MarkPrice{Symbol: "XAU-USD", Price: money.MustParsePrice("4276"), Received: clk.now},
	})
	return e, gw, clk
}

// pushReference подаёт цену ориентира, обходя ограничение частоты пересчёта:
// оно защищает бюджет процессора, а не задаёт поведение.
func pushReference(e *Engine, price string, at time.Time) {
	e.lastReferenceEval = time.Time{}
	e.handle(context.Background(), events.ReferencePriceUpdated{
		Reference: market.ReferencePrice{
			Symbol: "XAU-USD", Venue: "binance_futures",
			Price: money.MustParsePrice(price), Received: at,
		},
	})
}

// Смысл всей затеи: движение на внешней площадке снимает сторону раньше,
// чем оно доходит до стакана биржи. Стакан здесь неподвижен — по нему нас
// ничто не тревожит, — и съём происходит только потому, что ориентир ушёл.
func TestReferenceMovePullsTheSideBeforeTheBookMoves(t *testing.T) {
	e, gw, clk := referenceEngine(t)

	// Продажа стоит на целевой дистанции, в середине мёртвой зоны: трогать
	// её незачем, пока рынок не пришёл.
	liveRung(e, market.Sell, "ask-1", "4278.4", clk.now)
	e.evaluate(context.Background(), false)
	require.Empty(t, gw.cancelled, "a quiet market takes nothing off")

	// Ориентир прошёл вверх 10 bps: оценка mark обгоняет нашу продажу,
	// хотя стакан StandX стоит на прежнем месте.
	clk.advance(200 * time.Millisecond)
	pushReference(e, "4404.4", clk.now)

	require.Contains(t, gw.cancelled, trading.ClOrdID("ask-1"),
		"the reference is the only thing that moved, and it has to be enough")
}

// Устаревший ориентир не правит ничем. Замолчавшая площадка опаснее
// отсутствующей: она выглядит как исправная защита, ничего не защищая.
func TestStaleReferenceIsIgnored(t *testing.T) {
	e, gw, clk := referenceEngine(t)

	liveRung(e, market.Sell, "ask-1", "4279", clk.now)

	// Цена ориентира ушла далеко, но пришла слишком давно.
	pushReference(e, "4408.8", clk.now)
	e.referenceAt = clk.now.Add(-5 * time.Second)
	gw.cancelled = nil

	e.evaluate(context.Background(), false)

	require.Empty(t, gw.cancelled, "a price that stopped arriving must not steer anything")
	require.Equal(t, "book (reference stale)", e.fastSignal())
}

// Без ориентира быстрый сигнал остаётся тем, чем был: середина стакана.
func TestWithoutAReferenceTheBookMidStillDrivesTheFastSignal(t *testing.T) {
	e, _, clk := dangerEngine(t)
	require.Equal(t, "book", e.fastSignal())

	e.handle(context.Background(), events.MarkPriceUpdated{
		Mark: market.MarkPrice{Symbol: "XAU-USD", Price: money.MustParsePrice("4276"), Received: clk.now},
	})
	e.book = market.NewBook("XAU-USD",
		[]market.Level{{Price: money.MustParsePrice("4283"), Qty: money.MustParseQty("1")}},
		[]market.Level{{Price: money.MustParsePrice("4285"), Qty: money.MustParseQty("1")}},
		clk.now)

	require.Greater(t, e.markEstimate().Decimal().InexactFloat64(), 4276.0,
		"the book mid moved up, so the estimate has to follow it")
}

// Опорная точка ставится тактом mark, а не первым попавшимся событием:
// смещение считается от неё, и сдвинуть её значило бы обнулить накопленное
// движение ровно перед тем, как оно понадобится.
func TestReferenceAnchorIsResetByTheMarkTick(t *testing.T) {
	e, _, clk := referenceEngine(t)

	pushReference(e, "4404.4", clk.now)
	drift, ok := e.referenceDrift()
	require.True(t, ok)
	require.InDelta(t, 10.0, float64(drift), 0.1, "10 bps above the anchor")

	// Официальная цена догнала ориентир: тот же уход в 10 bps приходит
	// официальным каналом, и накопленное смещение обнуляется вместе с ним.
	clk.advance(time.Second)
	e.handle(context.Background(), events.MarkPriceUpdated{
		Mark: market.MarkPrice{Symbol: "XAU-USD", Price: money.MustParsePrice("4280.3"), Received: clk.now},
	})

	drift, ok = e.referenceDrift()
	require.True(t, ok)
	require.InDelta(t, 0.0, float64(drift), 0.001, "the official price caught up, the drift starts over")
}

// Смена источника печатается как событие: замолчавший ориентир иначе
// не проявляется ничем, кроме переставшего приходить предупреждения,
// а отсутствие предупреждения ненаблюдаемо по определению.
func TestFastSignalSourceChangeIsLogged(t *testing.T) {
	e, _, clk := dangerEngine(t)
	e.refStaleAfter = 2 * time.Second

	var buf bytes.Buffer
	e.deps.Log = slog.New(slog.NewTextHandler(&buf, nil))

	pushReference(e, "4400", clk.now)
	require.Contains(t, buf.String(), "fast signal now leads on the external reference")

	buf.Reset()
	clk.advance(5 * time.Second)
	e.evaluate(context.Background(), false)
	require.Contains(t, buf.String(), "external reference is stale")

	// Повтора нет: состояние не менялось, значит и события не было.
	buf.Reset()
	e.evaluate(context.Background(), false)
	require.NotContains(t, buf.String(), "external reference is stale")
}

// Возраст ориентира обязан оставаться конечным: почасовая свёртка кладёт
// квантили показателей в JSON, а json.Encode на ±Inf возвращает ошибку —
// одна ненастроенная площадка стоила бы всей истории за час.
func TestReferenceStalenessStaysFiniteBeforeTheFirstPrice(t *testing.T) {
	e, _, clk := dangerEngine(t)
	e.refStaleAfter = 2 * time.Second

	seen := &recordingStaleness{}
	e.deps.Metrics = seen

	e.evaluate(context.Background(), false)
	require.Len(t, seen.ages, 1)
	require.False(t, math.IsInf(seen.ages[0], 0), "a feed that never spoke still needs a number")

	clk.advance(time.Minute)
	e.evaluate(context.Background(), false)
	require.Greater(t, seen.ages[1], seen.ages[0], "silence has to look like it is getting worse")
}

// Инструмент без ориентира не публикует его возраст вовсе: вечно устаревший
// ряд у того, кто ничего не ждёт, — это ложная тревога, а не наблюдение.
func TestUnconfiguredReferenceReportsNothing(t *testing.T) {
	e, _, _ := dangerEngine(t)
	seen := &recordingStaleness{}
	e.deps.Metrics = seen

	e.evaluate(context.Background(), false)

	require.Empty(t, seen.ages)
}

type recordingStaleness struct {
	ports.NopMetrics
	ages []float64
}

func (r *recordingStaleness) ReferenceStaleness(_ market.Symbol, seconds float64) {
	r.ages = append(r.ages, seconds)
}

// Разговорчивая площадка не имеет права остановить актора: очередь роняет
// промежуточный снимок ровно так же, как снимок стакана.
func TestReferenceFloodIsDroppedInsteadOfHalting(t *testing.T) {
	e, _, clk := referenceEngine(t)

	for range inboxSize + 10 {
		e.enqueue(events.ReferencePriceUpdated{
			Reference: market.ReferencePrice{Symbol: "XAU-USD", Price: money.MustParsePrice("4400"), Received: clk.now},
		})
	}

	require.False(t, e.halted, "a chatty venue is not a reason to stop trading")
}
