// Package gateway маршрутизирует торговые команды между WebSocket и REST.
package gateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/nd/mm-standx/internal/adapters/standx/wsapi"
	"github.com/nd/mm-standx/internal/domain/trading"
	"github.com/nd/mm-standx/internal/ports"
)

// Router выбирает канал доставки для каждой команды.
//
// Правило выбора разное, потому что каналы отличаются по двум осям.
// WebSocket быстрее: кадр без HTTP-заголовков, без риска рукопожатия TLS
// под всплеском, команды уходят конвейером. REST дешевле по бюджету запросов
// там, где есть пакетный эндпойнт: одна отмена шести ордеров стоит один кредит
// вместо шести.
//
// Отсюда маршруты:
//
//   - размещение и одиночная отмена — WebSocket, ради задержки;
//   - пакетная отмена — REST, ради кредитов: оборот сети у обоих каналов
//     одинаков, а соединение остаётся тёплым за счёт трафика сверки;
//   - при отказе основного канала — запасной, с оговоркой про идемпотентность.
type Router struct {
	ws   FastGateway
	rest ports.OrderGateway
	log  *slog.Logger
}

// FastGateway — торговый канал, умеющий сообщить о своей готовности.
//
// Интерфейс объявлен здесь, а не в пакете реализации: потребитель определяет,
// что ему нужно от поставщика.
type FastGateway interface {
	ports.OrderGateway
	Connected() bool
}

// New собирает маршрутизатор. ws может быть nil — тогда всё идёт через REST.
func New(ws FastGateway, rest ports.OrderGateway, log *slog.Logger) *Router {
	return &Router{ws: ws, rest: rest, log: log.With("component", "gateway.router")}
}

// Place размещает ордер, предпочитая WebSocket.
//
// Размещение не идемпотентно, поэтому на запасной канал команда уходит только
// когда точно известно, что она не покинула процесс. Если подтверждения просто
// не дождались, ордер мог оказаться в стакане: повторять нельзя, расхождение
// найдёт сверка.
func (r *Router) Place(ctx context.Context, req ports.PlaceRequest) error {
	if !r.wsReady() {
		return r.rest.Place(ctx, req)
	}

	err := r.ws.Place(ctx, req)
	if err == nil {
		return nil
	}
	if !errors.Is(err, wsapi.ErrNotSent) {
		return err
	}

	r.log.Warn("falling back to REST for placement: command never left the trading channel",
		"cl_ord_id", req.ClOrdID, "error", err)
	if restErr := r.rest.Place(ctx, req); restErr != nil {
		return fmt.Errorf("both channels failed: ws: %w; rest: %w", err, restErr)
	}
	return nil
}

// Cancel отменяет ордер, предпочитая WebSocket.
//
// Отмена идемпотентна: повторная отмена уже снятого ордера безвредна. Поэтому
// на запасной канал уходим при любой ошибке — потерять возможность снять
// котировку опаснее, чем отправить лишний запрос.
func (r *Router) Cancel(ctx context.Context, id trading.ClOrdID, p ports.Priority) error {
	if !r.wsReady() {
		return r.rest.Cancel(ctx, id, p)
	}

	err := r.ws.Cancel(ctx, id, p)
	if err == nil {
		return nil
	}

	r.log.Warn("falling back to REST for cancel", "cl_ord_id", id, "error", err)
	if restErr := r.rest.Cancel(ctx, id, p); restErr != nil {
		return fmt.Errorf("both channels failed: ws: %w; rest: %w", err, restErr)
	}
	return nil
}

// CancelBatch снимает пачку ордеров, предпочитая пакетный REST-эндпойнт.
//
// Это единственная команда, где REST выигрывает: протокол торгового канала
// не знает пакетной отмены, и снятие шести ордеров стоило бы там шесть кредитов
// против одного. Задержка при этом сопоставима — оба варианта укладываются
// в один оборот сети.
func (r *Router) CancelBatch(ctx context.Context, ids []trading.ClOrdID, p ports.Priority) error {
	if len(ids) == 0 {
		return nil
	}

	err := r.rest.CancelBatch(ctx, ids, p)
	if err == nil {
		return nil
	}
	if !r.wsReady() {
		return err
	}

	r.log.Warn("falling back to a WebSocket pipeline for batch cancel", "count", len(ids), "error", err)
	if wsErr := r.ws.CancelBatch(ctx, ids, p); wsErr != nil {
		return fmt.Errorf("both channels failed: rest: %w; ws: %w", err, wsErr)
	}
	return nil
}

func (r *Router) wsReady() bool { return r.ws != nil && r.ws.Connected() }

var _ ports.OrderGateway = (*Router)(nil)
