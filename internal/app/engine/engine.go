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
	"github.com/nd/mm-standx/internal/domain/calendar"
	"github.com/nd/mm-standx/internal/domain/economics"
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
	Economics  economics.Gate

	// ReferenceStaleAfter — сколько молчания внешнего ориентира возвращает
	// быстрый сигнал к середине стакана. Ноль означает, что ориентир для
	// инструмента не настроен, и путь не задействуется вовсе.
	ReferenceStaleAfter time.Duration
	// News — блэкаут вокруг запланированных публикаций.
	News NewsGuard

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
	economics      economics.Gate
	appetite       policy.Appetite
	bounds         policy.Bounds
	maxNetPosition money.Qty
	ladder         policy.Ladder
	rules          quotes.Rules
	// refStaleAfter — сколько молчания делает ориентир непригодным.
	// Ноль означает, что ориентир для инструмента не настроен вовсе.
	refStaleAfter time.Duration
	newsGuard     NewsGuard

	bids *quotes.Ladder
	asks *quotes.Ladder
	vol  *volatility.Estimator
	hour *uptime.Tracker

	// Рыночное состояние.
	mark          money.Price
	markAt        time.Time
	book          market.Book
	bookMidAtMark money.Price

	// Внешний ориентир: цена площадки, движения которой опережают mark.
	//
	// referenceAtMark запоминается в момент такта ровно так же, как
	// bookMidAtMark: смещение с этой точки и есть оценка смещения mark
	// (PLAN.md §4.7). Нулевое значение означает, что ориентир не настроен
	// или ещё не пришёл, и быстрый сигнал берётся из стакана.
	reference       money.Price
	referenceAt     time.Time
	referenceAtMark money.Price
	// referenceLeading помнит, чем питался быстрый сигнал в прошлый раз:
	// смена источника печатается один раз, а не на каждом такте.
	referenceLeading bool
	// lastReferenceEval ограничивает частоту пересчётов по ориентиру,
	// см. referenceReactionFloor.
	lastReferenceEval time.Time
	// referenceWaitSince — с какого момента актор ждёт первую цену ориентира.
	// Нужен, чтобы возраст в метрике оставался конечным до её прихода.
	referenceWaitSince time.Time

	// Календарь публикаций и момент, когда его последний раз удалось прочитать.
	//
	// Хранится расписание целиком, а не текущая поза: поза вычисляется по
	// собственным часам на каждом пересчёте. Сообщение о переходе могло бы
	// не прийти, и тогда актор стоял бы в стакане на публикации либо
	// не возвращался после неё; расписание же остаётся верным независимо
	// от того, пришло ли следующее сообщение.
	news        calendar.Calendar
	newsFetched time.Time
	// newsPosture помнит предыдущую позу: переход печатается один раз.
	newsPosture calendar.Posture

	// Состояние счёта.
	position  money.Qty
	available decimal.Decimal

	// Управляющее состояние.
	halted bool
	// haltReason — почему актор остановлен.
	//
	// Хранится потому, что остановленный актор обязан объяснять себя. Прежде
	// он возвращался из evaluate первой же строкой, не написав ничего: в логе
	// это выглядело неотличимо от смерти процесса, а метрика показывала ноль,
	// который одинаково означает и «не котирую», и «сломан».
	haltReason string

	// riskPauseUntil — срок паузы, объявленной защитным контуром.
	//
	// Именно срок, а не флаг: снятие сообщением может не прийти, и тогда актор
	// молчит до конца запуска. Событие QuotingResumed снимает паузу досрочно,
	// но и без него она истекает сама.
	riskPauseUntil  time.Time
	riskPauseReason string

	cooldownUntil time.Time
	pendingFill   money.Qty
	fillDeadline  time.Time
	lastStatus    time.Time
	lastAction    time.Time
	blocked       string
	// blockedSince и nextStallLog поднимают затянувшееся молчание из сводки
	// в тревогу. Ноль в blockedSince означает, что актор котирует.
	blockedSince   time.Time
	nextStallLog   time.Time
	target         policy.Quote
	fillEpisodes   []time.Time
	fillBurstUntil time.Time

	// sizeLimit и perSide — чем ограничен размер стороны и каким он вышел.
	//
	// Держатся ради сводки. Вопрос «упирается размер в аллокацию или в остаток
	// свободной маржи» решает, какую долю депозита вообще можно выделить,
	// и вывести его нельзя — только измерить.
	sizeLimit string
	perSide   money.Qty
	// rungs — сколько ступеней помещается в полосу прямо сейчас. Держится
	// ради сводки: сужение лесенки меняет и геометрию, и размер, и по логу
	// это должно быть видно без вывода из соседних чисел.
	rungs int

	// flattenOrders — закрывающие ордера, отправленные на биржу, с моментом
	// отправки.
	//
	// Память нужна ровно для одного: опознать собственное закрытие среди
	// сообщений об исполнении. Без неё залив закрывается маркет-ордером,
	// исполнение этого ордера приходит обратно как ещё один залив, и актор
	// заводит эпизод, закрыть который уже нечем — позиция к тому моменту
	// плоская. Итог — вечное «no quoting while exposure is open».
	flattenOrders map[trading.ClOrdID]time.Time

	// flattenRetryAt is the earliest time for a position-based recovery close.
	//
	// Ответ на market IOC и обновление позиции приходят разными сообщениями
	// и не мгновенно. Без паузы каждый служебный тик слал бы ещё один
	// закрывающий ордер: reduce_only спасёт от переворота, но не от комиссии.
	//
	// A newly observed fill delta bypasses this deadline and is closed
	// immediately. The deadline only prevents the slower position snapshot from
	// duplicating that close while its acknowledgement is in flight.
	//
	// It also serves as the "close in flight" signal that blocks quoting.
	// Признаком нарочно выбран срок, а не флаг: флаг требует снятия сообщением,
	// которое может не прийти, и тогда актор молчит до конца запуска.
	flattenRetryAt time.Time

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
	minTierMultiplier := 0.0
	if p.Live && p.Economics.BreakEvenUSDPerMakerHour > 0 {
		minTierMultiplier = p.Economics.MinimumTierMultiplier()
	}

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
		refStaleAfter:  p.ReferenceStaleAfter,
		newsGuard:      p.News,
		ladder:         policy.Ladder{Layers: layers, SpreadBps: p.Config.Quote.LadderSpreadBps},
		rules: quotes.Rules{
			MinRest:    p.Config.Quote.MinRest,
			AckTimeout: p.Health.AckTimeout,
		},
		bids: quotes.New(market.Buy, layers),
		asks: quotes.New(market.Sell, layers),
		vol:  volatility.New(p.Volatility),
		hour: uptime.New(p.Program, uptime.Policy{
			TargetPct:         p.Config.Uptime.TargetPct,
			HardFloorPct:      p.Config.Uptime.HardFloorPct,
			ReservePct:        p.Config.Uptime.ReservePct,
			MinTierMultiplier: minTierMultiplier,
			SampleInterval:    tickInterval,
		}),
		economics:     p.Economics,
		position:      money.MustParseQty("0"),
		filled:        make(map[trading.ClOrdID]money.Qty),
		touched:       make(map[trading.ClOrdID]bool),
		flattenOrders: make(map[trading.ClOrdID]time.Time),
		deps:          deps,
		inbox:         make(chan events.Event, inboxSize),
		done:          make(chan struct{}),
	}
}

func (e *Engine) Name() string { return "engine[" + e.symbol.String() + "]" }

// Start подписывается на события и запускает цикл.
func (e *Engine) Start(context.Context) error {
	if e.economics.BreakEvenUSDPerMakerHour > 0 {
		prefix := "economics." + e.symbol.String() + "."
		e.deps.Log.Info("economic gate in force",
			"symbol", e.symbol,
			"reward_rate_lower_bound_usd_per_maker_hour", e.economics.RewardRateLowerBoundUSDPerMakerHour,
			"break_even_usd_per_maker_hour", e.economics.BreakEvenUSDPerMakerHour,
			"calibration_max_allocation_usd", e.economics.CalibrationMaxAllocationUSD,
			"allocation_usd", e.cfg.AllocationUSD)
		e.deps.Metrics.RiskLimit(prefix+"reward_rate_lower_bound_usd_per_maker_hour",
			e.economics.RewardRateLowerBoundUSDPerMakerHour)
		e.deps.Metrics.RiskLimit(prefix+"break_even_usd_per_maker_hour",
			e.economics.BreakEvenUSDPerMakerHour)
		e.deps.Metrics.RiskLimit(prefix+"calibration_max_allocation_usd",
			e.economics.CalibrationMaxAllocationUSD)
	}
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
	// Пауза между попытками относится к штатной работе: на выходе времени
	// на ещё один такт нет, а позиция обязана быть закрыта.
	e.flattenRetryAt = time.Time{}
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
		events.Subscribe(e.deps.Bus, e.Name(), func(_ context.Context, ev events.ReferencePriceUpdated) {
			if ev.Reference.Symbol == e.symbol {
				e.enqueue(ev)
			}
		}),
		events.Subscribe(e.deps.Bus, e.Name(), func(_ context.Context, ev events.NewsCalendarUpdated) {
			e.enqueue(ev)
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
		events.Subscribe(e.deps.Bus, e.Name(), func(_ context.Context, ev events.KillSwitchCleared) {
			e.enqueue(ev)
		}),
		events.Subscribe(e.deps.Bus, e.Name(), func(_ context.Context, ev events.QuotingPaused) {
			e.enqueue(ev)
		}),
		events.Subscribe(e.deps.Bus, e.Name(), func(_ context.Context, ev events.QuotingResumed) {
			e.enqueue(ev)
		}),
	}
}

func (e *Engine) enqueue(ev events.Event) {
	select {
	case e.inbox <- ev:
	default:
		// Стакан и внешний ориентир приходят в разы чаще mark price — ориентир
		// на всплеске может давать сотни сообщений в секунду. При переполнении
		// лучше потерять промежуточный снимок, чем остановить актор целиком
		// или заблокировать более важные события: остановиться из-за того, что
		// чужая площадка разговорилась, было бы прямо противоположно смыслу.
		switch ev.(type) {
		case events.BookUpdated, events.ReferencePriceUpdated:
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
	case events.ReferencePriceUpdated:
		e.onReference(ctx, v)
	case events.NewsCalendarUpdated:
		e.news, e.newsFetched = v.Calendar, v.Fetched
		e.evaluate(ctx, true)
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
		// Разрыв необязательного потока не отменяет права котировать.
		//
		// Внешний ориентир улучшает быстрый сигнал, но не является его
		// единственным источником: без него актор возвращается к смещению
		// середины стакана, то есть ровно к той картине, по которой работал
		// до появления фида. Снимать котировки в этом случае значило бы платить
		// присутствием — единственным, за что здесь платят, — за сбой на чужой
		// площадке.
		if v.Optional {
			e.deps.Log.Warn("optional stream disconnected, continuing on exchange data",
				"symbol", e.symbol, "stream", v.Stream, "error", v.Reason)
			return
		}
		// Окно наблюдений здесь намеренно не сбрасывается.
		//
		// Склейку через паузу закрывает сам оценщик: разность за пропуск длиннее
		// трёх тактов он не записывает, а окно забывает только по измеренному
		// перерыву (volatility.windowStaleGap). Разрыв соединения этого не
		// доказывает: 10 августа биржа отдала 36 разрывов за восемь часов, почти
		// все короче секунды, и каждый стоил минуты прогрева — при том что
		// присутствие и есть предмет заработка. Хуже того, пока окно пусто,
		// ограничение памяти не действует (Estimate.bounded требует Ready),
		// и устаревший рекорд правит мерой риска без всякого противовеса.
		e.withdraw(ctx, ports.CauseDisconnected, v.Stream)
	case events.KillSwitchTripped:
		e.halted = true
		e.haltReason = v.Reason
		e.withdraw(ctx, ports.CauseKillSwitch, v.Reason)

	case events.KillSwitchCleared:
		// Остановка снята — чаще всего сменой суток UTC. Без этой ветки запрет
		// оставался в акторе навсегда: поднять его было нечем, и переживший
		// полночь процесс продолжал молчать с уже пустым основанием.
		if e.halted {
			e.deps.Log.Warn("kill switch cleared, resuming", "symbol", e.symbol,
				"previous_reason", e.haltReason)
		}
		e.halted = false
		e.haltReason = ""
		e.evaluate(ctx, true)

	case events.QuotingPaused:
		e.riskPauseUntil, e.riskPauseReason = v.Until, v.Reason
		e.withdraw(ctx, ports.CauseRiskPause, v.Reason)

	case events.QuotingResumed:
		e.riskPauseUntil, e.riskPauseReason = time.Time{}, ""
		e.evaluate(ctx, true)
	}
}

// onMarkPrice — основной такт стратегии.
func (e *Engine) onMarkPrice(ctx context.Context, ev events.MarkPriceUpdated) {
	e.mark = ev.Mark.Price
	e.markAt = ev.Mark.Received
	e.vol.Observe(e.mark, e.markAt)

	// Обе опорные точки запоминаются именно в момент такта: дальнейшее их
	// смещение и служит оценкой смещения mark между тактами.
	if mid, ok := e.bookMid(); ok {
		e.bookMidAtMark = mid
	}
	e.referenceAtMark = e.reference
	e.evaluate(ctx, true)
}

// referenceReactionFloor — минимальный интервал между пересчётами по ориентиру.
//
// Не экономия ради экономии, а предел полезного. Реакция упирается в круг
// отмены, около 400 мс (PLAN.md §2.4), и решение, принятое на двадцать
// миллисекунд раньше, уходит на биржу в тот же момент — разброс сети один
// заведомо больше. Зато порог делает нагрузку независимой от разговорчивости
// площадки: спокойное золото даёт 70 обновлений в секунду, а BTC на всплеске —
// сотни, и без ограничения частота пересчёта следовала бы за ними.
const referenceReactionFloor = 25 * time.Millisecond

// onReference принимает цену внешней площадки.
//
// Пересчёт здесь такой же, как по стакану, и с тем же ограничением: событие
// не несёт официальной цены, поэтому выставлять по нему нельзя — только
// снимать (PLAN.md §4.8). Право снимать и есть весь смысл фида: движение
// на быстрой площадке случается раньше, чем арбитражёр успевает прийти
// с ним к нашей котировке.
func (e *Engine) onReference(ctx context.Context, ev events.ReferencePriceUpdated) {
	if !ev.Reference.Price.IsPositive() {
		return
	}
	e.reference = ev.Reference.Price
	e.referenceAt = ev.Reference.Received
	// Первая цена после старта или после разрыва становится опорной сама:
	// без этого смещение считалось бы от нуля, а такт mark мог не наступить
	// ещё три секунды.
	if !e.referenceAtMark.IsPositive() {
		e.referenceAtMark = e.reference
	}

	// Состояние обновляется всегда, пересчёт — не всегда: пропущенный такт
	// не теряется, ближайший пересчёт из любого источника увидит уже свежую
	// цену.
	now := e.deps.Clock.Now()
	if now.Sub(e.lastReferenceEval) < referenceReactionFloor {
		return
	}
	e.lastReferenceEval = now
	e.evaluate(ctx, false)
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
	if _, ours := e.flattenOrders[ev.Order.ClOrdID]; ours {
		e.onFlattenUpdate(ctx, ev.Order)
		return
	}

	_, knownQuote := e.filled[ev.Order.ClOrdID]
	matched := false
	ladder := e.ladderFor(ev.Order.Side)
	if ladder != nil {
		_, matched = ladder.OnOrderUpdate(ev.Order, e.deps.Clock.Now())
	}
	if ev.Order.FilledQty.IsPositive() && (knownQuote || matched) {
		e.onFill(ctx, ev.Order)
	} else if ev.Order.FilledQty.IsPositive() {
		// PositionUpdated remains the authority for account exposure and will
		// flatten an unexpected position. It must not turn a manual or replayed
		// terminal order into a quote-fill episode in the risk ledger.
		e.deps.Log.Warn("ignoring fill update for an untracked order",
			"symbol", e.symbol, "cl_ord_id", ev.Order.ClOrdID)
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

// onFlattenUpdate применяет сообщение о судьбе закрывающего ордера.
//
// Его исполнение не является заливом: стоимость закрытия уже заложена
// в оценку, опубликованную при заливе (publishFill), а эпизод там же и заведён.
// Пропустить этот ордер в общий путь учёта значит посчитать один залив дважды
// и оставить эпизод незакрытым навсегда.
func (e *Engine) onFlattenUpdate(ctx context.Context, o trading.Order) {
	if o.FilledQty.IsPositive() {
		e.deps.Log.Info("flatten order filled",
			"symbol", e.symbol, "side", o.Side,
			"qty", o.FilledQty, "price", o.AvgFillPrice)
	}
	if o.Status.IsTerminal() {
		delete(e.flattenOrders, o.ClOrdID)
		// Частичное исполнение оставляет позицию открытой. Повтор назначает
		// служебный тик, который увидит ненулевую позицию.
		if o.Qty.IsPositive() && o.FilledQty.Lt(o.Qty) {
			e.deps.Log.Warn("flatten order closed short of the position",
				"symbol", e.symbol, "filled", o.FilledQty, "requested", o.Qty)
		}
	}
	e.evaluate(ctx, true)
}

// pruneFlattenOrders забывает закрывающие ордера, о которых биржа промолчала.
//
// Терминальное сообщение чистит запись само; предел по возрасту нужен на случай,
// когда оно не придёт вовсе, и не даёт карте расти в течение суток работы.
func (e *Engine) pruneFlattenOrders(now time.Time) {
	for id, at := range e.flattenOrders {
		if now.Sub(at) > flattenMemory {
			delete(e.flattenOrders, id)
		}
	}
}

// flattenMemory — сколько помнить отправленный закрывающий ордер.
const flattenMemory = 5 * time.Minute

// onOrderRejected обрабатывает отказ биржи на этапе матчинга.
//
// Основной случай — отклонение post-only: котировка оказалась бы тейкером,
// то есть стакан ушёл дальше, чем мы считали. Ступень освобождается сразу,
// не дожидаясь таймаута подтверждения: ордера нет, и держать ступень занятой
// две секунды значит терять присутствие впустую.
func (e *Engine) onOrderRejected(ctx context.Context, ev events.OrderRejected) {
	now := e.deps.Clock.Now()

	// Отказ по закрывающему ордеру означает открытую позицию без закрытия.
	// Ступеней он не касается, а повтор назначит служебный тик по истечении
	// паузы: биржа только что отказала, и немедленная вторая попытка получит
	// тот же ответ.
	if _, ours := e.flattenOrders[ev.ClOrdID]; ours {
		delete(e.flattenOrders, ev.ClOrdID)
		e.deps.Log.Error("flatten order rejected, the position stays open",
			"symbol", e.symbol, "cl_ord_id", ev.ClOrdID, "code", ev.Code, "reason", ev.Reason)
		e.deps.Metrics.OrderFailed(e.symbol, "flatten")
		return
	}

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
	e.pruneFlattenOrders(now)
	// Immediate closes are sent from onFill. This path reconciles residual
	// exposure after the acknowledgement timeout, or closes an unexpected
	// position whose order update was not observed. With the default position
	// limit, every non-zero position is handled here.
	if e.overPositionLimit() || (e.pendingFill.IsPositive() && !now.Before(e.fillDeadline)) {
		e.flatten(ctx)
	}
	// Warm-up is real downtime from the programme's point of view. Omitting it
	// manufactured uptime after every start and restart in the middle of an
	// hour, so every tick is sampled even before the risk estimate is ready.
	e.sampleUptime(now)
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

// markEstimate восстанавливает текущий mark по смещению опережающего сигнала.
//
// Официальная цена приходит раз в три секунды, и всё это время мы формально
// слепы. Оба доступных источника движутся согласованно с mark, поэтому их
// смещение с момента последнего такта служит оценкой смещения самого mark.
//
// Предпочитается внешний ориентир. Разница между источниками не в точности,
// а в причинности: нас исполняет арбитражёр, увидевший движение на быстрой
// площадке, и стакан StandX сдвигает тот самый ордер, который нас и забирает.
// Середина стакана поэтому предупреждает одновременно с заливом, то есть
// не предупреждает; внешняя площадка показывает то же движение раньше.
//
// Значение допускается только к порогам опасности: цена ордера и попадание
// в полосу считаются по официальному mark, где оценке места нет.
func (e *Engine) markEstimate() money.Price {
	if !e.mark.IsPositive() {
		return e.mark
	}
	drift, ok := e.referenceDrift()
	if !ok {
		if drift, ok = e.bookDrift(); !ok {
			return e.mark
		}
	}
	return e.mark.MulRatio(decimal.NewFromInt(1).Add(drift.Ratio()))
}

// referenceDrift — смещение внешней площадки с момента последнего такта mark.
//
// Отказ возвращается, пока ориентир не настроен, не пришёл или замолчал.
// Порог молчания короче такта mark намеренно: ориентир старше самой
// официальной цены не добавляет к ней ничего, а править решением по цене,
// которая перестала двигаться, — худший исход из возможных, потому что
// выглядит он как работающая защита.
func (e *Engine) referenceDrift() (money.Bps, bool) {
	if e.refStaleAfter <= 0 || !e.reference.IsPositive() || !e.referenceAtMark.IsPositive() {
		return 0, false
	}
	if e.deps.Clock.Now().Sub(e.referenceAt) > e.refStaleAfter {
		return 0, false
	}
	drift, err := money.RelativeBps(e.referenceAtMark, e.reference)
	if err != nil {
		return 0, false
	}
	return drift, true
}

// bookDrift — смещение середины стакана с момента последнего такта mark.
func (e *Engine) bookDrift() (money.Bps, bool) {
	mid, ok := e.bookMid()
	if !ok || !e.bookMidAtMark.IsPositive() {
		return 0, false
	}
	drift, err := money.RelativeBps(e.bookMidAtMark, mid)
	if err != nil {
		return 0, false
	}
	return drift, true
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
