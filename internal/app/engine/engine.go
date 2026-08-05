// Package engine содержит актора котирования — по одному на инструмент.
//
// Актор владеет всем своим состоянием единолично и обрабатывает события строго
// последовательно, поэтому мьютексов здесь нет и быть не должно. Всё, что
// приходит извне, попадает в очередь; всё, что уходит наружу, проходит через
// порты.
package engine

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/shopspring/decimal"

	"github.com/nd/mm-standx/internal/config"
	"github.com/nd/mm-standx/internal/domain/market"
	"github.com/nd/mm-standx/internal/domain/money"
	"github.com/nd/mm-standx/internal/domain/policy"
	"github.com/nd/mm-standx/internal/domain/quotes"
	"github.com/nd/mm-standx/internal/domain/reward"
	"github.com/nd/mm-standx/internal/domain/trading"
	"github.com/nd/mm-standx/internal/domain/uptime"
	"github.com/nd/mm-standx/internal/domain/volatility"
	"github.com/nd/mm-standx/internal/events"
	"github.com/nd/mm-standx/internal/ports"
)

const (
	// inboxSize — глубина очереди актора.
	//
	// Переполнение означает, что актор не успевает за потоком, то есть его
	// решения уже опираются на устаревшую картину. Правильная реакция —
	// остановка, а не молчаливая потеря событий.
	inboxSize = 1024

	// tickInterval — период служебного тика: таймауты подтверждений,
	// выборка аптайма, проверка устаревания цены.
	tickInterval = 250 * time.Millisecond

	// bankedCaution — во сколько раз увеличивается требуемый запас, когда
	// часовая цель уже взята. Награда от дальнейшего присутствия не растёт,
	// поэтому разумно отойти от рынка.
	bankedCaution = 2.0
)

// Deps — внешние зависимости актора.
type Deps struct {
	Gateway ports.OrderGateway
	// Reader нужен только для сверки при завершении: локальному состоянию
	// на выходе доверять нельзя, а цена ошибки — оставленные в стакане ордера.
	Reader  ports.ExchangeReader
	Bus     *events.Bus
	Clock   ports.Clock
	Metrics ports.Metrics
	Log     *slog.Logger
}

// Params — всё, что актор знает о своём инструменте на момент сборки.
//
// Структура, а не список аргументов: параметров дюжина, и позиционный вызов
// с двумя соседними config-полями одного типа — это ошибка, которую компилятор
// не поймает, а рынок поймает.
type Params struct {
	Symbol     market.Symbol
	Spec       market.SymbolSpec
	Config     config.Symbol
	Health     config.Health
	Risk       config.Risk
	Live       bool
	Program    reward.Program
	Appetite   policy.Appetite
	Bounds     policy.Bounds
	Volatility volatility.Config

	// MaxNetPosition — экспозиция, выше которой актор перестаёт котировать
	// и закрывает позицию. Ноль означает, что позиция не допускается вовсе:
	// стратегия зарабатывает на присутствии в стакане, а не на направлении.
	MaxNetPosition money.Qty
}

// Engine — актор котирования одного инструмента.
type Engine struct {
	symbol market.Symbol
	spec   market.SymbolSpec
	cfg    config.Symbol
	health config.Health
	risk   config.Risk
	live   bool

	program        reward.Program
	appetite       policy.Appetite
	bounds         policy.Bounds
	maxNetPosition money.Qty
	ladder         policy.Ladder
	rules          quotes.Rules

	bids *quotes.Ladder
	asks *quotes.Ladder
	vol  *volatility.Estimator
	hour *uptime.Tracker

	// Рыночное состояние.
	mark          money.Price
	markAt        time.Time
	book          market.Book
	bookMidAtMark money.Price

	// Состояние счёта.
	position  money.Qty
	available decimal.Decimal

	// Управляющее состояние.
	halted          bool
	cooldownUntil   time.Time
	flattenInFlight bool
	pendingFill     money.Qty
	fillDeadline    time.Time
	lastStatus      time.Time
	lastAction      time.Time
	blocked         string
	target          policy.Quote

	// pendingWithdraw — ордера, снятие которых не удалось отправить.
	//
	// Съём котировок — аварийный путь, и молчаливая потеря команды на нём
	// недопустима: ступени уже помечены снимаемыми, то есть локально мы считаем
	// сторону пустой, а на бирже она стоит. Сверка нашла бы расхождение через
	// несколько секунд, но эти секунды приходятся ровно на тот момент, ради
	// которого съём и затевался.
	pendingWithdraw  []trading.ClOrdID
	withdrawRetryAt  time.Time
	withdrawAttempts int

	// filled хранит уже учтённый объём исполнения по каждому ордеру.
	//
	// Биржа присылает накопленное количество, а не приращение, поэтому без
	// этой памяти повторное сообщение по тому же ордеру засчиталось бы как
	// новый залив — и в дебаунсе закрытия, и в учёте риска.
	filled map[trading.ClOrdID]money.Qty

	// touched и shadowFills — учёт теневого режима: сколько раз рынок дошёл бы
	// до наших котировок. Считается по ордеру, а не по обновлению стакана:
	// стакан приходит в несколько раз чаще такта, и одно касание иначе дало бы
	// десятки мнимых заливов.
	touched     map[trading.ClOrdID]bool
	shadowFills int

	deps        Deps
	inbox       chan events.Event
	unsubscribe []func()
	cancel      context.CancelFunc
	done        chan struct{}
}

// New собирает актора.
func New(p Params, deps Deps) *Engine {
	layers := max(p.Config.Quote.LadderLayers, 1)

	if deps.Metrics == nil {
		deps.Metrics = ports.NopMetrics{}
	}

	return &Engine{
		symbol:         p.Symbol,
		spec:           p.Spec,
		cfg:            p.Config,
		health:         p.Health,
		risk:           p.Risk,
		live:           p.Live,
		program:        p.Program,
		appetite:       p.Appetite,
		bounds:         p.Bounds,
		maxNetPosition: p.MaxNetPosition,
		ladder:         policy.Ladder{Layers: layers, SpreadBps: p.Config.Quote.LadderSpreadBps},
		rules: quotes.Rules{
			MinRest:    p.Config.Quote.MinRest,
			AckTimeout: p.Health.AckTimeout,
		},
		bids:     quotes.New(market.Buy, layers),
		asks:     quotes.New(market.Sell, layers),
		vol:      volatility.New(p.Volatility),
		hour:     uptime.New(p.Program, p.Config.Uptime.TargetPct),
		position: money.MustParseQty("0"),
		filled:   make(map[trading.ClOrdID]money.Qty),
		touched:  make(map[trading.ClOrdID]bool),
		deps:     deps,
		inbox:    make(chan events.Event, inboxSize),
		done:     make(chan struct{}),
	}
}

func (e *Engine) Name() string { return "engine[" + e.symbol.String() + "]" }

// Start подписывается на события и запускает цикл.
func (e *Engine) Start(context.Context) error {
	e.subscribe()

	ctx, cancel := context.WithCancel(context.Background())
	e.cancel = cancel

	go func() {
		defer close(e.done)
		e.loop(ctx)
	}()
	return nil
}

// Stop освобождает стакан и останавливает цикл.
//
// Порядок действий продиктован тем, что у StandX нет отмены ордеров по разрыву
// соединения. Всё, что осталось в стакане после выхода процесса, продолжит
// исполняться, а следить за этим будет некому.
func (e *Engine) Stop(ctx context.Context) error {
	// Отписка первой: обработка новых событий во время уборки только помешает,
	// а актор может попытаться выставить ордер прямо перед выходом.
	for _, unsub := range e.unsubscribe {
		unsub()
	}
	e.halted = true

	e.withdraw(ctx, ports.CauseShutdown, "process is stopping")
	e.clearRemaining(ctx)
	e.closePositionOnExit(ctx)

	if e.cancel == nil {
		return nil
	}
	e.cancel()

	select {
	case <-e.done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("%s: did not stop in time: %w", e.Name(), ctx.Err())
	}
}

// shutdownSweeps — сколько раз проверяется, что стакан действительно пуст.
//
// Отмена асинхронна: успешный ответ означает приём заявки, а не снятие ордера.
// Единственный способ убедиться — перечитать список и повторить.
const shutdownSweeps = 3

// clearRemaining добивает всё, что осталось в стакане.
//
// Снятия по локальному списку недостаточно: актор мог потерять ордер при
// разрыве, не дождаться подтверждения или просто разойтись с биржей. На выходе
// авторитетен только ответ биржи.
func (e *Engine) clearRemaining(ctx context.Context) {
	if e.deps.Reader == nil {
		return
	}

	for sweep := range shutdownSweeps {
		if ctx.Err() != nil {
			e.deps.Log.Error("shutdown interrupted with orders possibly still resting",
				"symbol", e.symbol)
			return
		}

		orders, err := e.deps.Reader.OpenOrders(ctx, e.symbol)
		if err != nil {
			e.deps.Log.Error("failed to verify the book is clear", "symbol", e.symbol, "error", err)
			return
		}
		if len(orders) == 0 {
			if sweep > 0 {
				e.deps.Log.Info("book is clear", "symbol", e.symbol)
			}
			return
		}

		ids := make([]trading.ClOrdID, 0, len(orders))
		for _, o := range orders {
			ids = append(ids, o.ClOrdID)
		}
		e.deps.Log.Warn("orders still resting on shutdown, cancelling",
			"symbol", e.symbol, "count", len(ids), "sweep", sweep+1)

		if err := e.deps.Gateway.CancelBatch(ctx, ids, ports.PrioritySafety); err != nil {
			e.deps.Log.Error("failed to cancel remaining orders", "symbol", e.symbol, "error", err)
			return
		}
		// Отмена подтверждается не мгновенно; даём бирже такт на применение.
		if err := e.deps.Clock.Sleep(ctx, e.health.AckTimeout); err != nil {
			return
		}
	}

	e.deps.Log.Error("orders may still be resting after shutdown sweeps", "symbol", e.symbol)
}

// closePositionOnExit закрывает позицию, если она осталась.
//
// Стратегия позиций не держит по замыслу, поэтому открытая позиция на выходе —
// это авария, а не намерение. Оставить её без присмотра опаснее, чем закрыть
// по рынку: следить за ликвидацией будет некому.
func (e *Engine) closePositionOnExit(ctx context.Context) {
	if e.deps.Reader == nil {
		return
	}
	positions, err := e.deps.Reader.Positions(ctx, e.symbol)
	if err != nil {
		e.deps.Log.Error("failed to check position on shutdown", "symbol", e.symbol, "error", err)
		return
	}

	net := money.MustParseQty("0")
	for _, p := range positions {
		if p.Symbol == e.symbol {
			net = net.Add(p.Qty)
		}
	}
	if net.IsZero() {
		return
	}

	e.deps.Log.Error("position open on shutdown, closing at market", "symbol", e.symbol, "qty", net)
	e.position = net
	e.flattenInFlight = false
	e.flatten(ctx)
}

// subscribe направляет интересующие события в очередь актора.
//
// Обработчики шины обязаны не блокироваться, поэтому здесь происходит только
// передача в очередь; вся работа идёт в собственной горутине.
func (e *Engine) subscribe() {
	e.unsubscribe = []func(){
		events.Subscribe(e.deps.Bus, e.Name(), func(_ context.Context, ev events.MarkPriceUpdated) {
			if ev.Mark.Symbol == e.symbol {
				e.enqueue(ev)
			}
		}),
		events.Subscribe(e.deps.Bus, e.Name(), func(_ context.Context, ev events.BookUpdated) {
			if ev.Book.Symbol == e.symbol {
				e.enqueue(ev)
			}
		}),
		events.Subscribe(e.deps.Bus, e.Name(), func(_ context.Context, ev events.OrderUpdated) {
			if ev.Order.Symbol == e.symbol {
				e.enqueue(ev)
			}
		}),
		events.Subscribe(e.deps.Bus, e.Name(), func(_ context.Context, ev events.OrderRejected) {
			e.enqueue(ev)
		}),
		events.Subscribe(e.deps.Bus, e.Name(), func(_ context.Context, ev events.PositionUpdated) {
			if ev.Position.Symbol == e.symbol {
				e.enqueue(ev)
			}
		}),
		events.Subscribe(e.deps.Bus, e.Name(), func(_ context.Context, ev events.BalanceUpdated) {
			e.enqueue(ev)
		}),
		events.Subscribe(e.deps.Bus, e.Name(), func(_ context.Context, ev events.OpenOrdersSnapshot) {
			if ev.Symbol == e.symbol {
				e.enqueue(ev)
			}
		}),
		events.Subscribe(e.deps.Bus, e.Name(), func(_ context.Context, ev events.MarkPriceStale) {
			if ev.Symbol == e.symbol {
				e.enqueue(ev)
			}
		}),
		events.Subscribe(e.deps.Bus, e.Name(), func(_ context.Context, ev events.StreamDisconnected) {
			e.enqueue(ev)
		}),
		events.Subscribe(e.deps.Bus, e.Name(), func(_ context.Context, ev events.KillSwitchTripped) {
			e.enqueue(ev)
		}),
	}
}

func (e *Engine) enqueue(ev events.Event) {
	select {
	case e.inbox <- ev:
	default:
		// Стакан приходит в несколько раз чаще mark price. При переполнении
		// лучше потерять промежуточный снимок, чем остановить актор целиком
		// или заблокировать более важные события.
		if _, ok := ev.(events.BookUpdated); ok {
			return
		}
		e.deps.Log.Error("engine inbox overflow, halting", "symbol", e.symbol)
		e.halted = true
	}
}

func (e *Engine) loop(ctx context.Context) {
	ticker := e.deps.Clock.NewTicker(tickInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-e.inbox:
			e.handle(ctx, ev)
		case <-ticker.C():
			e.onTick(ctx)
		}
	}
}

func (e *Engine) handle(ctx context.Context, ev events.Event) {
	switch v := ev.(type) {
	case events.MarkPriceUpdated:
		e.onMarkPrice(ctx, v)
	case events.BookUpdated:
		e.onBook(ctx, v)
	case events.OrderUpdated:
		e.onOrder(ctx, v)
	case events.OrderRejected:
		e.onOrderRejected(ctx, v)
	case events.PositionUpdated:
		e.position = v.Position.Qty
		now := e.deps.Clock.Now()
		if !e.position.IsZero() && e.pendingFill.IsPositive() && !now.Before(e.fillDeadline) {
			e.flatten(ctx)
		}
	case events.BalanceUpdated:
		e.available = v.Balance.Available
	case events.OpenOrdersSnapshot:
		e.reconcile(ctx, v)
	case events.MarkPriceStale:
		e.withdraw(ctx, ports.CauseStaleMark, "no mark price updates")
	case events.StreamDisconnected:
		// История скачков описывает рынок до разрыва, а склейка через паузу
		// дала бы ложное наблюдение. Право котировать зарабатывается заново.
		e.vol.Reset()
		e.withdraw(ctx, ports.CauseDisconnected, v.Stream)
	case events.KillSwitchTripped:
		e.halted = true
		e.withdraw(ctx, ports.CauseKillSwitch, v.Reason)
	}
}

// onMarkPrice — основной такт стратегии.
func (e *Engine) onMarkPrice(ctx context.Context, ev events.MarkPriceUpdated) {
	e.mark = ev.Mark.Price
	e.markAt = ev.Mark.Received
	e.vol.Observe(e.mark, e.markAt)

	// Середина стакана запоминается именно в момент такта: дальнейший её
	// сдвиг и служит оценкой смещения mark между тактами.
	if mid, ok := e.bookMid(); ok {
		e.bookMidAtMark = mid
	}
	e.evaluate(ctx, true)
}

// onBook обновляет быстрый сигнал.
//
// Стакан приходит чаще такта и не несёт официальной цены, поэтому по нему
// разрешено только снимать котировки, но не выставлять: размещение по оценке
// дало бы ордер вне полосы награды (PLAN.md §4.8).
func (e *Engine) onBook(ctx context.Context, ev events.BookUpdated) {
	// Биржа иногда присылает снимок без одной из сторон. Затирать рабочий
	// стакан таким сообщением нельзя: halfSpread станет нечитаемым, и актор
	// перестанет котировать до следующего полного снимка.
	if _, ok := ev.Book.BestBid(); !ok {
		return
	}
	if _, ok := ev.Book.BestAsk(); !ok {
		return
	}
	e.book = ev.Book
	// Учёт теневых заливов идёт до пересчёта: evaluate может снять ступень,
	// и тогда касание, ради наблюдения которого прогон и делается, пропало бы.
	e.countShadowFills(e.deps.Clock.Now())
	e.evaluate(ctx, false)
}

func (e *Engine) onOrder(ctx context.Context, ev events.OrderUpdated) {
	ladder := e.ladderFor(ev.Order.Side)
	if ladder != nil {
		ladder.OnOrderUpdate(ev.Order, e.deps.Clock.Now())
	}
	if ev.Order.FilledQty.IsPositive() {
		e.onFill(ctx, ev.Order)
	}
	// Порядок важен: исполнение учитывается до уборки, иначе последнее
	// сообщение по ордеру — оно же и терминальное — потеряло бы свой объём.
	if ev.Order.Status.IsTerminal() {
		e.forget(ev.Order.ClOrdID)
	}
	if !ev.Order.FilledQty.IsPositive() {
		e.evaluate(ctx, true)
	}
}

// onOrderRejected обрабатывает отказ биржи на этапе матчинга.
//
// Основной случай — отклонение post-only: котировка оказалась бы тейкером,
// то есть стакан ушёл дальше, чем мы считали. Ступень освобождается сразу,
// не дожидаясь таймаута подтверждения: ордера нет, и держать ступень занятой
// две секунды значит терять присутствие впустую.
func (e *Engine) onOrderRejected(ctx context.Context, ev events.OrderRejected) {
	now := e.deps.Clock.Now()
	rejected := trading.Order{ClOrdID: ev.ClOrdID, Status: trading.StatusRejected}

	for _, ladder := range []*quotes.Ladder{e.bids, e.asks} {
		if _, matched := ladder.OnOrderUpdate(rejected, now); matched {
			e.deps.Log.Warn("order rejected",
				"symbol", e.symbol, "cl_ord_id", ev.ClOrdID, "code", ev.Code, "reason", ev.Reason)
			e.deps.Metrics.ALOReject(e.symbol)
			break
		}
	}
	e.forget(ev.ClOrdID)
	e.evaluate(ctx, true)
}

// forget убирает завершённый ордер из вспомогательных карт.
func (e *Engine) forget(id trading.ClOrdID) {
	delete(e.filled, id)
	delete(e.touched, id)
	e.forgetWithdrawal([]trading.ClOrdID{id})
}

func (e *Engine) onTick(ctx context.Context) {
	now := e.deps.Clock.Now()

	// Устаревшая справочная цена означает торговлю вслепую.
	if e.mark.IsPositive() {
		e.deps.Metrics.MarkStaleness(e.symbol, now.Sub(e.markAt).Seconds())
		if now.Sub(e.markAt) > e.health.MarkStale {
			e.withdraw(ctx, ports.CauseStaleMark, "no mark price updates")
		}
	}
	// Повтор неудавшегося съёма идёт раньше всего остального: пока он не прошёл,
	// в стакане стоят ордера, которые мы уже считаем снятыми.
	e.retryWithdrawal(ctx, now)
	// Дебаунс частичных исполнений: свип даёт их десятками, и закрывать
	// каждое отдельным маркет-ордером значило бы платить комиссию многократно.
	if e.pendingFill.IsPositive() && !now.Before(e.fillDeadline) {
		e.flatten(ctx)
	}
	// Нули до готовности оценщика не должны попадать в учёт часа: иначе
	// при старте в середине часа актор мгновенно объявит его потерянным.
	if e.vol.Estimate(now).Ready {
		e.sampleUptime(now)
	}
	e.evaluate(ctx, true)
}

func (e *Engine) ladderFor(side market.Side) *quotes.Ladder {
	switch side {
	case market.Buy:
		return e.bids
	case market.Sell:
		return e.asks
	default:
		return nil
	}
}

// bookMid возвращает середину стакана.
func (e *Engine) bookMid() (money.Price, bool) {
	bid, hasBid := e.book.BestBid()
	ask, hasAsk := e.book.BestAsk()
	if !hasBid || !hasAsk {
		return money.Price{}, false
	}
	return bid.Add(ask).MulRatio(decimal.NewFromFloat(0.5)), true
}

// halfSpreadBps — расстояние от mark до вершины стакана.
//
// Именно на эту величину котировка «утоплена» относительно того, что видит
// рынок, поэтому она входит в расчёт требуемой дистанции.
func (e *Engine) halfSpreadBps() (money.Bps, bool) {
	bid, hasBid := e.book.BestBid()
	ask, hasAsk := e.book.BestAsk()
	if !hasBid || !hasAsk {
		return 0, false
	}
	spread, err := money.RelativeBps(bid, ask)
	if err != nil {
		return 0, false
	}
	return spread / 2, true
}

// markEstimate восстанавливает текущий mark по сдвигу середины стакана.
//
// Официальная цена приходит раз в три секунды, и всё это время мы формально
// слепы. Середина стакана движется согласованно с mark, поэтому её смещение
// с момента последнего такта служит оценкой смещения самого mark.
//
// Значение допускается только к порогам опасности: цена ордера и попадание
// в полосу считаются по официальному mark, где оценке места нет.
func (e *Engine) markEstimate() money.Price {
	mid, ok := e.bookMid()
	if !ok || !e.bookMidAtMark.IsPositive() || !e.mark.IsPositive() {
		return e.mark
	}
	drift, err := money.RelativeBps(e.bookMidAtMark, mid)
	if err != nil {
		return e.mark
	}
	return e.mark.MulRatio(decimal.NewFromInt(1).Add(drift.Ratio()))
}

// sampleUptime фиксирует наблюдение тем же способом, каким это делает биржа.
func (e *Engine) sampleUptime(now time.Time) {
	if !e.mark.IsPositive() {
		return
	}
	e.hour.Sample(now, e.program.Sample(e.sideOrders(e.bids), e.sideOrders(e.asks)))
}

// sideOrders переводит живые ступени в форму, понятную расчёту награды.
func (e *Engine) sideOrders(l *quotes.Ladder) []reward.Order {
	var out []reward.Order
	for _, s := range l.Slots {
		if s.Phase != quotes.Live {
			continue
		}
		d, err := market.DistanceBps(e.mark, l.Side, s.Price)
		if err != nil || d < 0 || d > e.bounds.MaxBps {
			continue
		}
		out = append(out, reward.Order{DistanceBps: d, Qty: s.Qty})
	}
	return out
}

var _ ports.Runnable = (*Engine)(nil)
