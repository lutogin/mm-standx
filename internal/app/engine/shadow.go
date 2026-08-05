package engine

import (
	"time"

	"github.com/nd/mm-standx/internal/domain/market"
	"github.com/nd/mm-standx/internal/domain/money"
	"github.com/nd/mm-standx/internal/domain/quotes"
)

// countShadowFills считает, сколько раз рынок дошёл бы до наших котировок.
//
// В теневом режиме заглушка шлюза не выдумывает исполнений, и это правильно:
// достоверно смоделировать залив нельзя, потому что очередь в стакане нам
// не видна, а без неё неизвестно, досталось бы нам исполнение или ордерам
// впереди (PLAN.md §7.2). Но само событие «рынок пришёл на нашу цену»
// наблюдаемо напрямую и не требует никакой модели.
//
// Полученное число — верхняя оценка заливов: рынок мог коснуться уровня
// и не выбрать его целиком. Верхняя оценка здесь уместнее точной, потому что
// решение, которое она обслуживает, — стоит ли вообще выходить в бой на этой
// дистанции, — ошибаться должно в сторону осторожности.
//
// Учёт только наблюдает и ничего не предпринимает. Снимать котировки и уходить
// в cooldown по воображаемому заливу значило бы измерять аптайм собственной
// модели вместо аптайма стратегии.
func (e *Engine) countShadowFills(now time.Time) {
	// В бою исполнения приходят от биржи и авторитетны; догадки только
	// исказили бы статистику, смешавшись с фактами.
	if e.live {
		return
	}

	for _, ladder := range []*quotes.Ladder{e.bids, e.asks} {
		for _, slot := range ladder.Slots {
			if slot.Phase != quotes.Live || slot.ClOrdID == "" {
				continue
			}
			// Ордер стоит на цене P. Рынок дошёл до него, когда встречная
			// сторона стакана пересекла эту цену: для покупки — лучшая продажа
			// опустилась до P, для продажи — лучшая покупка поднялась до P.
			if !e.book.WouldCross(ladder.Side, slot.Price) {
				continue
			}
			// Один ордер считается один раз. Стакан приходит в несколько раз
			// чаще такта, и без этого одно касание дало бы десятки «заливов».
			if e.touched[slot.ClOrdID] {
				continue
			}
			e.touched[slot.ClOrdID] = true

			e.shadowFills++
			e.deps.Metrics.ShadowFill(e.symbol, ladder.Side)
			e.deps.Log.Warn("shadow fill: the market reached our quote",
				"symbol", e.symbol,
				"side", ladder.Side,
				"price", slot.Price,
				"qty", slot.Qty,
				"distance", e.distanceOf(ladder.Side, slot.Price),
				"total", e.shadowFills,
				"age", now.Sub(slot.Since).Round(time.Millisecond))
		}
	}
}

// distanceOf возвращает дистанцию цены от последнего mark, для лога.
func (e *Engine) distanceOf(side market.Side, price money.Price) money.Bps {
	d, err := market.DistanceBps(e.mark, side, price)
	if err != nil {
		return 0
	}
	return d
}
