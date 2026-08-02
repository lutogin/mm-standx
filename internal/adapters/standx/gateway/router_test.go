package gateway

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nd/mm-standx/internal/adapters/standx/wsapi"
	"github.com/nd/mm-standx/internal/domain/trading"
	"github.com/nd/mm-standx/internal/ports"
)

// stubGateway записывает вызовы и отдаёт заданные ошибки.
type stubGateway struct {
	connected bool

	placed    []trading.ClOrdID
	cancelled []trading.ClOrdID
	batches   [][]trading.ClOrdID
	placeErr  error
	cancelErr error
	batchErr  error
}

func (s *stubGateway) Connected() bool { return s.connected }

func (s *stubGateway) Place(_ context.Context, req ports.PlaceRequest) error {
	s.placed = append(s.placed, req.ClOrdID)
	return s.placeErr
}

func (s *stubGateway) Cancel(_ context.Context, id trading.ClOrdID, _ ports.Priority) error {
	s.cancelled = append(s.cancelled, id)
	return s.cancelErr
}

func (s *stubGateway) CancelBatch(_ context.Context, ids []trading.ClOrdID, _ ports.Priority) error {
	s.batches = append(s.batches, ids)
	return s.batchErr
}

func newRouter(ws *stubGateway, rest *stubGateway) *Router {
	var fast FastGateway
	if ws != nil {
		fast = ws
	}
	return New(fast, rest, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

const orderID = trading.ClOrdID("01K2BK4ZKQE0C308SRD39P8N9Z")

func placeRequest() ports.PlaceRequest {
	return ports.PlaceRequest{ClOrdID: orderID, Symbol: "BTC-USD", Priority: ports.PriorityQuote}
}

func TestPlacePrefersWebSocket(t *testing.T) {
	ws := &stubGateway{connected: true}
	rest := &stubGateway{}

	require.NoError(t, newRouter(ws, rest).Place(context.Background(), placeRequest()))

	require.Equal(t, []trading.ClOrdID{orderID}, ws.placed)
	require.Empty(t, rest.placed, "REST must not be used while the trading channel is alive")
}

func TestPlaceUsesRestWhenWebSocketDown(t *testing.T) {
	ws := &stubGateway{connected: false}
	rest := &stubGateway{}

	require.NoError(t, newRouter(ws, rest).Place(context.Background(), placeRequest()))

	require.Empty(t, ws.placed)
	require.Equal(t, []trading.ClOrdID{orderID}, rest.placed)
}

// Размещение не идемпотентно. Повторять его по запасному каналу можно только
// когда точно известно, что команда не покинула процесс.
func TestPlaceFallsBackOnlyWhenCommandWasNotSent(t *testing.T) {
	ws := &stubGateway{
		connected: true,
		placeErr:  errors.Join(errors.New("socket closed"), wsapi.ErrNotSent),
	}
	rest := &stubGateway{}

	require.NoError(t, newRouter(ws, rest).Place(context.Background(), placeRequest()))
	require.Equal(t, []trading.ClOrdID{orderID}, rest.placed, "a command that was never sent is safe to retry")
}

// Неподтверждённая заявка могла дойти до биржи: повтор создал бы второй ордер
// в стакане, поэтому маршрутизатор обязан вернуть ошибку и оставить разбор сверке.
func TestPlaceDoesNotRetryAfterAckTimeout(t *testing.T) {
	ws := &stubGateway{connected: true, placeErr: wsapi.ErrAckTimeout}
	rest := &stubGateway{}

	err := newRouter(ws, rest).Place(context.Background(), placeRequest())

	require.ErrorIs(t, err, wsapi.ErrAckTimeout)
	require.Empty(t, rest.placed, "a retry could duplicate the order")
}

// Явное отклонение означает, что заявка дошла и была рассмотрена. Повторять её
// бессмысленно и опасно.
func TestPlaceDoesNotRetryAfterRejection(t *testing.T) {
	ws := &stubGateway{connected: true, placeErr: errors.New("alo order rejected")}
	rest := &stubGateway{}

	err := newRouter(ws, rest).Place(context.Background(), placeRequest())

	require.Error(t, err)
	require.Empty(t, rest.placed)
}

func TestPlaceReportsBothFailures(t *testing.T) {
	ws := &stubGateway{connected: true, placeErr: wsapi.ErrNotSent}
	rest := &stubGateway{placeErr: errors.New("http 500")}

	err := newRouter(ws, rest).Place(context.Background(), placeRequest())

	require.ErrorContains(t, err, "ws:")
	require.ErrorContains(t, err, "rest:")
}

// Отмена идемпотентна, поэтому запасной канал используется при любой ошибке:
// потерять возможность снять котировку опаснее лишнего запроса.
func TestCancelFallsBackOnAnyError(t *testing.T) {
	for name, wsErr := range map[string]error{
		"not sent":       wsapi.ErrNotSent,
		"no acknowledge": wsapi.ErrAckTimeout,
		"arbitrary":      errors.New("something went wrong"),
	} {
		t.Run(name, func(t *testing.T) {
			ws := &stubGateway{connected: true, cancelErr: wsErr}
			rest := &stubGateway{}

			require.NoError(t, newRouter(ws, rest).Cancel(context.Background(), orderID, ports.PrioritySafety))
			require.Equal(t, []trading.ClOrdID{orderID}, rest.cancelled)
		})
	}
}

func TestCancelPrefersWebSocket(t *testing.T) {
	ws := &stubGateway{connected: true}
	rest := &stubGateway{}

	require.NoError(t, newRouter(ws, rest).Cancel(context.Background(), orderID, ports.PrioritySafety))

	require.Equal(t, []trading.ClOrdID{orderID}, ws.cancelled)
	require.Empty(t, rest.cancelled)
}

// Пакетная отмена — единственная команда, где REST выигрывает: торговый канал
// не знает пакетного метода, и снятие шести ордеров стоило бы там шесть кредитов.
func TestCancelBatchPrefersRest(t *testing.T) {
	ws := &stubGateway{connected: true}
	rest := &stubGateway{}
	ids := []trading.ClOrdID{"a", "b", "c"}

	require.NoError(t, newRouter(ws, rest).CancelBatch(context.Background(), ids, ports.PrioritySafety))

	require.Equal(t, [][]trading.ClOrdID{ids}, rest.batches)
	require.Empty(t, ws.batches)
}

func TestCancelBatchFallsBackToWebSocket(t *testing.T) {
	ws := &stubGateway{connected: true}
	rest := &stubGateway{batchErr: errors.New("http 503")}
	ids := []trading.ClOrdID{"a", "b"}

	require.NoError(t, newRouter(ws, rest).CancelBatch(context.Background(), ids, ports.PrioritySafety))
	require.Equal(t, [][]trading.ClOrdID{ids}, ws.batches)
}

func TestCancelBatchIgnoresEmptyInput(t *testing.T) {
	ws := &stubGateway{connected: true}
	rest := &stubGateway{}

	require.NoError(t, newRouter(ws, rest).CancelBatch(context.Background(), nil, ports.PrioritySafety))
	require.Empty(t, rest.batches)
	require.Empty(t, ws.batches)
}

// Без учётных данных торговый канал отсутствует. Нетипизированный nil здесь
// критичен: указатель, уложенный в интерфейс, дал бы панику при проверке готовности.
func TestRouterWorksWithoutWebSocket(t *testing.T) {
	rest := &stubGateway{}
	router := newRouter(nil, rest)

	require.NoError(t, router.Place(context.Background(), placeRequest()))
	require.NoError(t, router.Cancel(context.Background(), orderID, ports.PrioritySafety))
	require.NoError(t, router.CancelBatch(context.Background(), []trading.ClOrdID{"a"}, ports.PrioritySafety))

	require.Equal(t, []trading.ClOrdID{orderID}, rest.placed)
	require.Equal(t, []trading.ClOrdID{orderID}, rest.cancelled)
	require.Len(t, rest.batches, 1)
}
