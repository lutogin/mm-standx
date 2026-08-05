// Package account периодически считывает состояние счёта и публикует его в шину.
//
// Пакет существует потому, что пользовательские каналы WebSocket управляются
// изменениями: пока баланс и позиции не меняются, событий нет вовсе. Актор,
// подписанный только на них, никогда не узнает, что на счёте есть средства,
// и будет молча отказываться котировать.
//
// Опрос заодно служит основой сверки: снимок из REST — единственный источник,
// с которым можно сравнить накопленное локальное состояние.
package account

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/nd/mm-standx/internal/domain/market"
	"github.com/nd/mm-standx/internal/domain/money"
	"github.com/nd/mm-standx/internal/domain/trading"
	"github.com/nd/mm-standx/internal/events"
	"github.com/nd/mm-standx/internal/ports"
)

// requestTimeout ограничивает один цикл опроса.
const requestTimeout = 10 * time.Second

// Poller публикует снимки счёта с постоянным интервалом.
type Poller struct {
	symbols  []market.Symbol
	interval time.Duration
	reader   ports.ExchangeReader
	bus      *events.Bus
	clock    ports.Clock
	log      *slog.Logger

	cancel context.CancelFunc
	done   chan struct{}
}

func New(
	symbols []market.Symbol,
	interval time.Duration,
	reader ports.ExchangeReader,
	bus *events.Bus,
	clock ports.Clock,
	log *slog.Logger,
) *Poller {
	return &Poller{
		symbols:  symbols,
		interval: interval,
		reader:   reader,
		bus:      bus,
		clock:    clock,
		log:      log.With("component", "account"),
		done:     make(chan struct{}),
	}
}

func (p *Poller) Name() string { return "account" }

func (p *Poller) Start(context.Context) error {
	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel

	go func() {
		defer close(p.done)
		p.loop(ctx)
	}()
	return nil
}

func (p *Poller) Stop(ctx context.Context) error {
	if p.cancel == nil {
		return nil
	}
	p.cancel()

	select {
	case <-p.done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("account poller: did not stop in time: %w", ctx.Err())
	}
}

func (p *Poller) loop(ctx context.Context) {
	// Первый снимок снимается немедленно: без него акторы не знают
	// ни доступных средств, ни собственных позиций.
	p.poll(ctx)

	ticker := p.clock.NewTicker(p.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C():
			p.poll(ctx)
		}
	}
}

func (p *Poller) poll(parent context.Context) {
	ctx, cancel := context.WithTimeout(parent, requestTimeout)
	defer cancel()

	if balance, err := p.reader.Balance(ctx); err != nil {
		p.log.Warn("failed to read balance", "error", err)
	} else {
		balance.UpdatedAt = p.clock.Now()
		events.Publish(p.bus, ctx, events.BalanceUpdated{Balance: balance})
	}

	for _, symbol := range p.symbols {
		p.pollPosition(ctx, symbol)
		p.pollOrders(ctx, symbol)
	}
}

// pollOrders публикует снимок висящих ордеров для сверки.
func (p *Poller) pollOrders(ctx context.Context, symbol market.Symbol) {
	// Отметка ставится до запроса, а не после. Ответ описывает состояние
	// не позднее момента отправки, и сверка по этой отметке решает, мог ли
	// ордер вообще попасть в снимок. Отметка после ответа была бы позже
	// истины на время оборота сети, и ордер, выставленный в этот промежуток,
	// ошибочно считался бы призраком.
	at := p.clock.Now()

	orders, err := p.reader.OpenOrders(ctx, symbol)
	if err != nil {
		p.log.Warn("failed to read open orders", "symbol", symbol, "error", err)
		return
	}
	events.Publish(p.bus, ctx, events.OpenOrdersSnapshot{
		Symbol: symbol,
		Orders: orders,
		At:     at,
	})
}

// pollPosition публикует позицию по инструменту.
//
// Отсутствие позиции публикуется явным нулём: подписчик обязан отличать
// «позиции нет» от «сведений ещё не поступало», иначе после закрытия
// он будет вечно считать себя в позиции.
func (p *Poller) pollPosition(ctx context.Context, symbol market.Symbol) {
	positions, err := p.reader.Positions(ctx, symbol)
	if err != nil {
		p.log.Warn("failed to read positions", "symbol", symbol, "error", err)
		return
	}

	net := trading.Position{Symbol: symbol, Qty: money.MustParseQty("0"), UpdatedAt: p.clock.Now()}
	for _, position := range positions {
		if position.Symbol != symbol {
			continue
		}
		net.Qty = net.Qty.Add(position.Qty)
		net.EntryPrice = position.EntryPrice
		net.MarkPrice = position.MarkPrice
		net.LiqPrice = position.LiqPrice
		net.Leverage = position.Leverage
		net.MarginMode = position.MarginMode
		net.Unrealized = position.Unrealized
	}
	events.Publish(p.bus, ctx, events.PositionUpdated{Position: net})
}

var _ ports.Runnable = (*Poller)(nil)
