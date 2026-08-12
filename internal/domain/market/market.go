// Package market описывает инструменты биржи и геометрию котировок вокруг mark price.
//
// Здесь живёт вся арифметика "цена ↔ дистанция в базисных пунктах" — единственное
// место, где она разрешена. Слои выше оперируют дистанциями, а не ценами.
package market

import (
	"fmt"
	"sort"
	"time"

	"github.com/shopspring/decimal"

	"github.com/nd/mm-standx/internal/domain/money"
)

// Symbol — идентификатор торговой пары, например "BTC-USD".
type Symbol string

func (s Symbol) String() string { return string(s) }

// Side — сторона ордера.
type Side string

const (
	Buy  Side = "buy"
	Sell Side = "sell"
)

func (s Side) Valid() bool { return s == Buy || s == Sell }

func (s Side) Opposite() Side {
	if s == Buy {
		return Sell
	}
	return Buy
}

func (s Side) String() string { return string(s) }

// SymbolSpec — торговые ограничения инструмента, полученные из query_symbol_info.
//
// Загружается на старте и обновляется периодически: биржа может менять тики и лимиты.
type SymbolSpec struct {
	Symbol            Symbol
	PriceTickDecimals int32
	QtyTickDecimals   int32
	MinOrderQty       money.Qty
	MaxOrderQty       money.Qty
	MaxOpenOrders     int
	MaxPositionSize   money.Qty
	MaxLeverage       int
	MakerFee          decimal.Decimal // доля, не проценты: 0.0001 = 1 bp
	TakerFee          decimal.Decimal
	Enabled           bool
}

// SnapPrice приводит цену к тику, округляя всегда в сторону от mark price:
// покупку вниз, продажу вверх. Так дистанция после снэпа может только вырасти,
// то есть ордер становится безопаснее, а не ближе к исполнению.
func (s SymbolSpec) SnapPrice(p money.Price, side Side) money.Price {
	if side == Buy {
		return p.SnapDown(s.PriceTickDecimals)
	}
	return p.SnapUp(s.PriceTickDecimals)
}

// SnapQty приводит количество к тику округлением вниз: недобрать размер безопаснее,
// чем превысить лимит инструмента.
func (s SymbolSpec) SnapQty(q money.Qty) money.Qty {
	return q.SnapDown(s.QtyTickDecimals)
}

// ValidateQty проверяет количество против лимитов инструмента.
func (s SymbolSpec) ValidateQty(q money.Qty) error {
	if !q.IsPositive() {
		return fmt.Errorf("%s: qty must be positive, got %s", s.Symbol, q)
	}
	if q.Lt(s.MinOrderQty) {
		return fmt.Errorf("%s: qty %s below min %s", s.Symbol, q, s.MinOrderQty)
	}
	if s.MaxOrderQty.IsPositive() && q.Gt(s.MaxOrderQty) {
		return fmt.Errorf("%s: qty %s above max %s", s.Symbol, q, s.MaxOrderQty)
	}
	return nil
}

// MarkPrice — снимок справочной цены, относительно которой считается вся программа наград.
type MarkPrice struct {
	Symbol   Symbol
	Price    money.Price
	Received time.Time // локальное время получения, а не время биржи
}

// ReferencePrice — снимок цены с внешней площадки, чьи движения опережают mark.
//
// Держится отдельно от MarkPrice намеренно. Награда считается только от mark,
// и смешать эти две цены в одном типе значило бы однажды выставить ордер
// по чужой площадке — то есть вне полосы. Опорная цена допускается лишь туда,
// где ошибка стоит лишней осторожности: к порогам опасности (PLAN.md §4.7).
type ReferencePrice struct {
	// Symbol — инструмент биржи, за которым следит эта цена, а не имя символа
	// на внешней площадке.
	Symbol Symbol
	// Venue — площадка и тип рынка, для логов и метрик.
	Venue    string
	Price    money.Price
	Received time.Time // локальное время получения
}

// PriceAtDistance возвращает цену ордера, стоящего на дистанции d от mark price.
//
// Покупка размещается ниже mark, продажа — выше. Отрицательная дистанция даёт
// цену по другую сторону от mark, то есть заведомо пересекающий ордер.
func PriceAtDistance(mark money.Price, side Side, d money.Bps) money.Price {
	ratio := d.Ratio()
	if side == Buy {
		return mark.MulRatio(decimal.NewFromInt(1).Sub(ratio))
	}
	return mark.MulRatio(decimal.NewFromInt(1).Add(ratio))
}

// DistanceBps возвращает дистанцию ордера от mark price в базисных пунктах.
//
// Значение положительно, пока ордер находится на своей стороне от mark: покупка
// ниже, продажа выше. Отрицательное значение означает, что mark ушёл за цену
// ордера и тот стал маркетабельным — это сигнал немедленной опасности.
func DistanceBps(mark money.Price, side Side, orderPrice money.Price) (money.Bps, error) {
	rel, err := money.RelativeBps(mark, orderPrice)
	if err != nil {
		return 0, err
	}
	if side == Buy {
		return -rel, nil
	}
	return rel, nil
}

// Level — уровень стакана.
type Level struct {
	Price money.Price
	Qty   money.Qty
}

// Book — снимок стакана.
//
// Биржа не гарантирует порядок уровней ни в REST, ни в WS, поэтому конструктор
// сортирует их принудительно: биды по убыванию, аски по возрастанию.
type Book struct {
	Symbol   Symbol
	Bids     []Level
	Asks     []Level
	Received time.Time
}

// NewBook сортирует уровни и возвращает готовый к использованию снимок.
func NewBook(symbol Symbol, bids, asks []Level, received time.Time) Book {
	sorted := Book{Symbol: symbol, Bids: bids, Asks: asks, Received: received}
	sort.Slice(sorted.Bids, func(i, j int) bool { return sorted.Bids[i].Price.Gt(sorted.Bids[j].Price) })
	sort.Slice(sorted.Asks, func(i, j int) bool { return sorted.Asks[i].Price.Lt(sorted.Asks[j].Price) })
	return sorted
}

// BestBid возвращает лучшую цену покупки. Второе значение false, если сторона пуста.
func (b Book) BestBid() (money.Price, bool) {
	if len(b.Bids) == 0 {
		return money.Price{}, false
	}
	return b.Bids[0].Price, true
}

// BestAsk возвращает лучшую цену продажи. Второе значение false, если сторона пуста.
func (b Book) BestAsk() (money.Price, bool) {
	if len(b.Asks) == 0 {
		return money.Price{}, false
	}
	return b.Asks[0].Price, true
}

// Tick — шаг цены инструмента.
func (s SymbolSpec) Tick() decimal.Decimal { return decimal.New(1, -s.PriceTickDecimals) }

// ClampPostOnly отодвигает цену от противоположной вершины стакана на buffer тиков.
//
// Проверка на пересечение отвечает на вопрос «отвергнет ли биржа», и этого мало.
// Ордер, поставленный ровно на вершину, биржа примет, а рынок заберёт первым же
// движением: между «не тейкер» и «не будет исполнен через мгновение» лежит
// расстояние, которое надо назвать явно.
//
// Смещение всегда направлено от mark price, никогда к нему: покупка уезжает
// вниз, продажа вверх. Отсюда и односторонняя проверка у вызывающего — после
// зажима дистанция способна только вырасти, то есть выйти за полосу награды,
// но не подойти к рынку ближе разрешённого.
//
// Зажим срабатывает редко и ровно в тех случаях, ради которых заведён: когда
// стакан ушёл дальше, чем успела обновиться справочная цена. В обычной
// обстановке котировка стоит на несколько базисных пунктов глубже вершины,
// и предел до неё не достаёт.
func ClampPostOnly(price money.Price, side Side, book Book, spec SymbolSpec, buffer int) money.Price {
	if buffer <= 0 {
		return price
	}
	offset := spec.Tick().Mul(decimal.NewFromInt(int64(buffer)))

	if side == Buy {
		ask, ok := book.BestAsk()
		if !ok {
			return price
		}
		// Снэп в ту же сторону, что и смещение: округление вверх вернуло бы
		// часть только что отданного запаса.
		if limit := ask.Sub(money.NewPrice(offset)).SnapDown(spec.PriceTickDecimals); limit.Lt(price) {
			return limit
		}
		return price
	}

	bid, ok := book.BestBid()
	if !ok {
		return price
	}
	if limit := bid.Add(money.NewPrice(offset)).SnapUp(spec.PriceTickDecimals); limit.Gt(price) {
		return limit
	}
	return price
}

// WouldCross сообщает, пересечёт ли лимитный ордер противоположную сторону стакана.
//
// Биржа отклонит такой ордер при time_in_force=alo, но проверка на нашей стороне
// экономит запрос и, что важнее, ловит ошибку в расчёте дистанции до отправки.
func (b Book) WouldCross(side Side, price money.Price) bool {
	if side == Buy {
		ask, ok := b.BestAsk()
		return ok && price.Gte(ask)
	}
	bid, ok := b.BestBid()
	return ok && price.Lte(bid)
}

// DepthNotional возвращает нотионал, стоящий на стороне side в пределах
// within базисных пунктов от опорной цены.
//
// Меряет то, чего не видно по спреду: сколько рынку придётся выкупить, прежде
// чем он дойдёт до нашей котировки. Узкий спред при пустом стакане за вершиной
// опаснее широкого при плотном — небольшой рыночный ордер проходит сквозь
// разреженные уровни на несколько базисных пунктов и достаёт котировку,
// которая по спреду выглядела защищённой.
func (b Book) DepthNotional(ref money.Price, side Side, within money.Bps) decimal.Decimal {
	total := decimal.Zero
	if !ref.IsPositive() || within <= 0 {
		return total
	}

	levels, bound := b.Bids, ref.MulRatio(decimal.NewFromInt(1).Sub(within.Ratio()))
	if side == Sell {
		levels, bound = b.Asks, ref.MulRatio(decimal.NewFromInt(1).Add(within.Ratio()))
	}

	for _, l := range levels {
		// Уровни отсортированы от вершины наружу, поэтому первый вышедший
		// за границу завершает обход.
		if side == Buy && l.Price.Lt(bound) {
			break
		}
		if side == Sell && l.Price.Gt(bound) {
			break
		}
		total = total.Add(l.Price.Decimal().Mul(l.Qty.Decimal()))
	}
	return total
}
