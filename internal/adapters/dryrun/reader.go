package dryrun

import (
	"context"

	"github.com/nd/mm-standx/internal/domain/market"
	"github.com/nd/mm-standx/internal/domain/trading"
	"github.com/nd/mm-standx/internal/ports"
)

// Reader подменяет список висящих ордеров теневым, оставляя всё остальное
// настоящим.
//
// Сверка спрашивает биржу, что у неё стоит, и снимает расхождения. В теневом
// режиме на бирже не стоит ничего, поэтому вопрос, заданный ей, всегда даёт
// один и тот же ответ: наших ордеров нет. Сверка добросовестно объявляла бы
// призрачной каждую ступень, освобождала её и выставляла заново — до двух
// тысяч раз за час. Измерялась бы при этом частота этой самой ошибки,
// а не поведение стратегии.
//
// Подменяется ровно один метод. Баланс нужен настоящий: от него считается
// размер, и подставить сюда выдуманное число значило бы проверять стратегию
// на несуществующем счёте. Позиции тоже настоящие — открытая позиция обязана
// останавливать котирование независимо от режима.
type Reader struct {
	exchange ports.ExchangeReader
	gateway  *Gateway
}

// NewReader оборачивает настоящий читатель биржи.
func NewReader(exchange ports.ExchangeReader, gateway *Gateway) *Reader {
	return &Reader{exchange: exchange, gateway: gateway}
}

func (r *Reader) OpenOrders(_ context.Context, s market.Symbol) ([]trading.Order, error) {
	return r.gateway.OpenOrders(s), nil
}

func (r *Reader) Symbols(ctx context.Context) ([]market.Symbol, error) {
	return r.exchange.Symbols(ctx)
}

func (r *Reader) SymbolSpec(ctx context.Context, s market.Symbol) (market.SymbolSpec, error) {
	return r.exchange.SymbolSpec(ctx, s)
}

func (r *Reader) Positions(ctx context.Context, s market.Symbol) ([]trading.Position, error) {
	return r.exchange.Positions(ctx, s)
}

func (r *Reader) Balance(ctx context.Context) (trading.Balance, error) {
	return r.exchange.Balance(ctx)
}

var _ ports.ExchangeReader = (*Reader)(nil)
