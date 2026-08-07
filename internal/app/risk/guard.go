// Package risk связывает учёт стоимости стратегии с остановкой торговли.
//
// Компонент существует отдельно от акторов намеренно. Лимиты заданы на аккаунт,
// а не на инструмент: убыток на одном рынке расходует тот же депозит, что и
// убыток на другом. Актор, знающий только свой символ, не может судить о таком
// лимите, а раздать каждому по копии счётчика значило бы получить столько
// разных представлений о дневном результате, сколько включено инструментов.
package risk

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/nd/mm-standx/internal/config"
	"github.com/nd/mm-standx/internal/domain/market"
	domain "github.com/nd/mm-standx/internal/domain/risk"
	"github.com/nd/mm-standx/internal/domain/trading"
	"github.com/nd/mm-standx/internal/events"
	"github.com/nd/mm-standx/internal/ports"
)

// stateName — имя снимка в хранилище состояния.
const stateName = "risk-ledger"

// Guard ведёт учёт и останавливает торговлю при достижении лимита.
type Guard struct {
	limits domain.Limits
	// notionalLimit is checked at startup rather than continuously, so the
	// ledger has no use for it. It is carried here to be reported: a limit
	// nobody can read off the running process is a limit nobody can trust.
	notionalLimit     float64
	fillBurstLimit    int
	fillBurstWindow   time.Duration
	fillBurstCooldown time.Duration
	// fees — ставки комиссий, по которым считается стоимость исполнения,
	// когда биржа её не присылает.
	fees     config.Fees
	ledger   *domain.Ledger
	store    ports.StateStore
	bus      *events.Bus
	clock    ports.Clock
	metrics  ports.Metrics
	log      *slog.Logger
	interval time.Duration

	// Всё состояние правится только в собственной горутине: события приходят
	// из читателей WebSocket, поэтому попадают сначала в очередь.
	inbox       chan func(time.Time)
	unsubscribe []func()
	cancel      context.CancelFunc
	done        chan struct{}

	announced bool
	// nextSuspectLog прореживает предупреждение о канале сделок.
	nextSuspectLog time.Time
}

// suspectLogInterval — как часто повторяется предупреждение о канале сделок.
const suspectLogInterval = 10 * time.Minute

// inboxSize — глубина очереди. Переполнение здесь означало бы потерю учёта
// исполнения, поэтому запас берётся с избытком: события редки по замыслу.
const inboxSize = 512

// New собирает сторожа.
func New(
	cfg config.Risk,
	interval time.Duration,
	store ports.StateStore,
	bus *events.Bus,
	clk ports.Clock,
	metrics ports.Metrics,
	log *slog.Logger,
) *Guard {
	limits := domain.Limits{
		MaxDailyLossUSD: cfg.MaxDailyLossUSD,
		MaxFillsPerHour: cfg.MaxFillsPerHour,
		TripOnDrawdown:  cfg.KillSwitchOnDrawdown,
	}
	return &Guard{
		limits:            limits,
		notionalLimit:     cfg.MaxNotionalUSD,
		fillBurstLimit:    cfg.FillBurstLimit,
		fillBurstWindow:   cfg.FillBurstWindow,
		fillBurstCooldown: cfg.FillBurstCooldown,
		fees:              cfg.Fees,
		ledger:            domain.New(limits),
		store:             store,
		bus:               bus,
		clock:             clk,
		metrics:           metrics,
		log:               log.With("component", "risk"),
		interval:          interval,
		inbox:             make(chan func(time.Time), inboxSize),
		done:              make(chan struct{}),
	}
}

func (g *Guard) Name() string { return "risk" }

func (g *Guard) Start(context.Context) error {
	// Действующие лимиты печатаются при запуске. Защита, о настройках которой
	// нельзя спросить работающий процесс, проверяется только чтением конфига
	// — то есть предположением о том, какой файл он прочитал и что из него
	// применилось. Для контура, стоящего между ошибкой и депозитом, этого мало.
	g.log.Info("risk limits in force",
		"max_daily_loss_usd", g.limits.MaxDailyLossUSD,
		"max_fills_per_hour", g.limits.MaxFillsPerHour,
		"max_notional_usd", g.notionalLimit,
		"fill_burst_limit", g.fillBurstLimit,
		"fill_burst_window", g.fillBurstWindow,
		"fill_burst_cooldown", g.fillBurstCooldown,
		"trip_on_drawdown", g.limits.TripOnDrawdown,
		"persisted", g.store != nil)

	// The same values as metrics: the startup line answers the question only
	// while it is still in the log buffer, and every one of these can be
	// overridden from the environment. During an incident the answer has to be
	// available from the running process, not reconstructed from the image.
	g.metrics.RiskLimit("max_daily_loss_usd", g.limits.MaxDailyLossUSD)
	g.metrics.RiskLimit("max_fills_per_hour", float64(g.limits.MaxFillsPerHour))
	g.metrics.RiskLimit("max_notional_usd", g.notionalLimit)
	g.metrics.RiskLimit("fill_burst_limit", float64(g.fillBurstLimit))
	g.metrics.RiskLimit("fill_burst_window_seconds", g.fillBurstWindow.Seconds())
	g.metrics.RiskLimit("fill_burst_cooldown_seconds", g.fillBurstCooldown.Seconds())
	g.metrics.RiskLimit("trip_on_drawdown", boolAsFloat(g.limits.TripOnDrawdown))

	g.restore()

	g.unsubscribe = []func(){
		events.Subscribe(g.bus, g.Name(), func(_ context.Context, ev events.QuoteFilled) {
			g.submit(func(now time.Time) {
				g.ledger.OnFill(now, domain.Fill{
					Symbol:        ev.Symbol,
					At:            ev.At,
					CostUSD:       ev.EstimatedCostUSD,
					StartsEpisode: ev.StartsEpisode,
				})
				g.metrics.Fill(ev.Symbol, ev.Side)
				cost, _ := ev.EstimatedCostUSD.Float64()
				g.metrics.FlattenCost(ev.Symbol, cost)
			})
		}),
		events.Subscribe(g.bus, g.Name(), func(_ context.Context, ev events.TradeExecuted) {
			exec := g.execution(ev.Trade)
			g.submit(func(now time.Time) {
				g.ledger.OnTrade(now, exec)
			})
		}),
	}

	ctx, cancel := context.WithCancel(context.Background())
	g.cancel = cancel

	go func() {
		defer close(g.done)
		g.loop(ctx)
	}()
	return nil
}

func (g *Guard) Stop(ctx context.Context) error {
	for _, unsub := range g.unsubscribe {
		unsub()
	}
	if g.cancel == nil {
		return nil
	}
	g.cancel()

	select {
	case <-g.done:
	case <-ctx.Done():
		return fmt.Errorf("risk: did not stop in time: %w", ctx.Err())
	}
	// Снимок на выходе: между последней периодической записью и завершением
	// могли пройти исполнения, а терять их — значит начинать следующие сутки
	// с заниженным убытком.
	g.persist()
	return nil
}

// restore поднимает состояние прошлого запуска.
//
// Нечитаемый снимок не считается основанием начать с чистого листа. Обнулить
// счётчик убытка из-за повреждённого файла — ровно тот исход, ради исключения
// которого снимок и существует, поэтому вместо обнуления взводится остановка:
// человек разберётся быстрее, чем депозит закончится.
func (g *Guard) restore() {
	now := g.clock.Now()
	if g.store == nil {
		g.log.Warn("risk state is not persisted: a restart will clear the daily counters")
		return
	}

	var state domain.State
	found, err := g.store.Load(stateName, &state)
	switch {
	case err != nil:
		g.log.Error("failed to read the risk ledger, halting", "error", err)
		g.ledger.Trip(now, "risk ledger state is unreadable: "+err.Error())
		return
	case !found:
		g.log.Info("no previous risk ledger, starting fresh")
		return
	}

	g.ledger.Restore(state, now)
	if g.ledger.Tripped() {
		g.log.Error("restored a tripped kill switch: trading stays halted until the state is cleared",
			"path", stateName)
	} else {
		v := g.ledger.Assess(now)
		g.log.Info("risk ledger restored",
			"realized_pnl_usd", fmt.Sprintf("%.2f", v.ReportedNetUSD),
			"estimated_cost_usd", fmt.Sprintf("%.2f", v.EstimatedCostUSD),
			"episodes_today", v.EpisodesToday)
	}
}

func (g *Guard) submit(f func(time.Time)) {
	select {
	case g.inbox <- f:
	default:
		// Очередь защитного контура переполниться не должна вовсе. Если это
		// произошло, учёт уже неполон, и единственный честный ответ — остановка.
		g.log.Error("risk inbox overflow, halting")
		g.ledger.Trip(g.clock.Now(), "risk inbox overflow: accounting is incomplete")
	}
}

func (g *Guard) loop(ctx context.Context) {
	ticker := g.clock.NewTicker(g.interval)
	defer ticker.Stop()

	// Первая оценка немедленно: восстановленная остановка обязана дойти
	// до акторов раньше, чем они успеют выставить первую котировку.
	g.evaluate(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case apply := <-g.inbox:
			apply(g.clock.Now())
			g.evaluate(ctx)
		case <-ticker.C():
			g.evaluate(ctx)
			g.persist()
		}
	}
}

// evaluate публикует вердикт и, при срабатывании, останавливает торговлю.
func (g *Guard) evaluate(ctx context.Context) {
	now := g.clock.Now()
	v := g.ledger.Assess(now)

	g.metrics.RealizedPnL(v.ReportedNetUSD)
	g.metrics.EstimatedCost(v.EstimatedCostUSD)
	g.metrics.FillsLastHour(v.FillsLastHour)
	g.metrics.KillSwitch(v.Tripped)

	// Вердикт выносится на каждом событии, а неисправный канал остаётся
	// неисправным: без прореживания одно и то же предупреждение вытеснило бы
	// из лога всё остальное ровно тогда, когда лог нужен.
	if v.TradeChannelSuspect && !now.Before(g.nextSuspectLog) {
		g.nextSuspectLog = now.Add(suspectLogInterval)
		g.log.Warn("fills recorded but no trade messages received: "+
			"the exchange trade channel may be broken, falling back to the cost model",
			"episodes_today", v.EpisodesToday)
	}
	if !v.Tripped || g.announced {
		return
	}

	g.announced = true
	g.log.Error("kill switch tripped, trading halted",
		"reason", v.Reason,
		"loss_usd", fmt.Sprintf("%.2f", v.LossUSD),
		"fills_last_hour", v.FillsLastHour)

	// Снимок пишется до публикации: если процесс погибнет в момент остановки,
	// перезапуск обязан застать защиту уже взведённой.
	g.persist()
	events.Publish(g.bus, ctx, events.KillSwitchTripped{Reason: v.Reason, At: now})
}

func (g *Guard) persist() {
	if g.store == nil {
		return
	}
	if err := g.store.Save(stateName, g.ledger.State()); err != nil {
		g.log.Error("failed to persist the risk ledger", "error", err)
	}
}

// execution переводит исполнение в деньги.
//
// Знак берётся от стороны: продажа приносит денег, покупка уносит. Комиссия
// считается по ставке, соответствующей роли в сделке — биржа её не присылает,
// но присылает флаг `is_taker`, а ставки одинаковы для всех рынков StandX
// и заданы конфигурацией.
//
// Если комиссия всё же пришла, берётся она: собственный расчёт нужен как
// замена отсутствующему полю, а не как замена биржевому ответу. Так же
// поступает и REST-путь, где поле есть.
func (g *Guard) execution(t trading.Trade) domain.Execution {
	notional := t.Qty.Decimal().Abs().Mul(t.Price.Decimal())

	qty := t.Qty.Decimal().Abs()
	cash := notional
	if t.Side == market.Buy {
		cash = cash.Neg()
	} else {
		qty = qty.Neg()
	}

	fee := t.Fee.Abs()
	if fee.IsZero() {
		rate := g.fees.MakerBps
		if t.IsTaker {
			rate = g.fees.TakerBps
		}
		fee = notional.Mul(rate.Ratio())
	}
	return domain.Execution{QtyBase: qty, CashUSD: cash, FeeUSD: fee}
}

// boolAsFloat renders a flag as a gauge value.
func boolAsFloat(v bool) float64 {
	if v {
		return 1
	}
	return 0
}

var _ ports.Runnable = (*Guard)(nil)
