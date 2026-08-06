// Package reward кодирует экономику программы StandX Community Maker Yield.
//
// Пакет чистый: ни времени, ни ввода-вывода. Он отвечает на два вопроса —
// сколько «весит» котировка на заданной дистанции и во что превращается набор
// замеров за час. Параметры кривой и порогов приходят из конфигурации, потому
// что биржа меняет их без предупреждения, а таблица весов в документации
// противоречит формуле на той же странице.
package reward

import (
	"math"
	"sort"
	"time"

	"github.com/shopspring/decimal"

	"github.com/nd/mm-standx/internal/domain/money"
)

// Curve — кривая веса по дистанции от mark price.
//
// Вес линейно падает от MaxWeightPct на нулевой дистанции до MinWeightPct
// на границе полосы. За полосой ордер не засчитывается вовсе — это обрыв,
// а не плавное затухание, и именно он задаёт жёсткий потолок дистанции.
type Curve struct {
	BandBps      money.Bps
	MaxWeightPct float64
	MinWeightPct float64
}

// Weight возвращает множитель размера для дистанции d, где 1.0 соответствует
// ста процентам.
//
// Отрицательная дистанция означает, что mark ушёл за цену ордера. Такой ордер
// маркетабелен и в норме существовать не должен, но для устойчивости расчёта
// он оценивается как стоящий вплотную к mark.
func (c Curve) Weight(d money.Bps) float64 {
	if d > c.BandBps {
		return 0
	}
	if d < 0 {
		d = 0
	}
	span := c.MaxWeightPct - c.MinWeightPct
	return (c.MinWeightPct + span*float64(c.BandBps-d)/float64(c.BandBps)) / 100
}

// Tier — порог аптайма и множитель начисления.
type Tier struct {
	ThresholdPct float64
	Multiplier   float64
}

// Program — параметры начисления для одного инструмента.
type Program struct {
	Curve Curve
	// Tiers упорядочены по убыванию порога: сначала самый требовательный.
	Tiers []Tier
	// UnitSize — размер, дающий один Maker Hour.
	UnitSize decimal.Decimal
	// PerSideCap — потолок засчитываемого размера на сторону.
	PerSideCap decimal.Decimal
	// MaxMakerHoursPerHour — потолок начисления за час.
	MaxMakerHoursPerHour float64

	// OffHours describes when the reference market behind the instrument is
	// shut. The zero value means a 24/7 market.
	OffHours Schedule
	// OffHoursMultiplier scales the hour's accrual while the reference market
	// is shut. One means no reduction.
	//
	// StandX applies 10% to metals, oil and equities over weekends and
	// holidays. Presence during those hours costs the same fill risk and earns
	// a tenth, so the number belongs in the model rather than in a footnote.
	OffHoursMultiplier float64
}

// Multiplier returns the accrual factor in force at the given instant.
func (p Program) Multiplier(at time.Time) float64 {
	if p.OffHoursMultiplier < 0 || p.OffHoursMultiplier >= 1 {
		return 1
	}
	if p.OffHours.Closed(at) {
		return p.OffHoursMultiplier
	}
	return 1
}

// ScaledQty применяет вес дистанции к размеру ордера.
func (p Program) ScaledQty(d money.Bps, qty money.Qty) decimal.Decimal {
	return qty.Decimal().Mul(decimal.NewFromFloat(p.Curve.Weight(d)))
}

// SideQty складывает взвешенные размеры одной стороны и обрезает по потолку.
//
// Превышение потолка не приносит ничего: размер сверх него просто не считается.
// Это верхняя граница полезного капитала на инструмент.
func (p Program) SideQty(orders []Order) decimal.Decimal {
	total := decimal.Zero
	for _, o := range orders {
		total = total.Add(p.ScaledQty(o.DistanceBps, o.Qty))
	}
	if p.PerSideCap.IsPositive() && total.GreaterThan(p.PerSideCap) {
		return p.PerSideCap
	}
	return total
}

// Order — котировка, участвующая в расчёте.
type Order struct {
	DistanceBps money.Bps
	Qty         money.Qty
}

// Sample — значение, которое биржа фиксирует в момент замера.
//
// Берётся слабейшая сторона: односторонние котировки не квалифицируются,
// и перевес на одной стороне пропадает впустую.
func (p Program) Sample(bid, ask []Order) decimal.Decimal {
	return decimal.Min(p.SideQty(bid), p.SideQty(ask))
}

// MakerHours считает начисление за час по набору замеров.
//
// Замеры сортируются по убыванию, для каждого порога берётся значение
// на соответствующей позиции, и выбирается лучший исход. Отсутствующие
// котировки должны присутствовать в наборе нулями: именно они и делают
// метрику зависимой от аптайма.
//
// The instant selects the off-hours multiplier: the exchange applies it to the
// hour's outcome, after the tier maximum and the ceiling.
func (p Program) MakerHours(samples []decimal.Decimal, at time.Time) float64 {
	if len(samples) == 0 || !p.UnitSize.IsPositive() {
		return 0
	}

	sorted := make([]decimal.Decimal, len(samples))
	copy(sorted, samples)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].GreaterThan(sorted[j]) })

	best := 0.0
	for _, tier := range p.Tiers {
		x := percentile(sorted, tier.ThresholdPct)
		score, _ := x.Div(p.UnitSize).Float64()
		if candidate := score * tier.Multiplier; candidate > best {
			best = candidate
		}
	}
	if p.MaxMakerHoursPerHour > 0 && best > p.MaxMakerHoursPerHour {
		best = p.MaxMakerHoursPerHour
	}
	return best * p.Multiplier(at)
}

// percentile возвращает значение на позиции p процентов в наборе,
// отсортированном по убыванию.
//
// Позиция округляется вверх, то есть оценка получается пессимистичной:
// при ровно семидесяти процентах присутствия результат уже нулевой.
// Занижать собственную оценку безопаснее, чем завышать — ошибка приводит
// к избыточному аптайму, а не к потерянному часу.
func percentile(descending []decimal.Decimal, pct float64) decimal.Decimal {
	n := len(descending)
	index := int(math.Floor(pct / 100 * float64(n)))
	if index >= n {
		index = n - 1
	}
	if index < 0 {
		index = 0
	}
	return descending[index]
}

// RequiredQty возвращает размер на сторону, необходимый для заданного числа
// Maker Hours при котировании на дистанции d.
//
// Используется при расчёте аллокации: показывает, во что обходится цель
// в единицах базового актива.
func (p Program) RequiredQty(target float64, d money.Bps) decimal.Decimal {
	weight := p.Curve.Weight(d)
	if weight <= 0 || target <= 0 {
		return decimal.Zero
	}
	needed := p.UnitSize.Mul(decimal.NewFromFloat(target)).Div(decimal.NewFromFloat(weight))

	// Сверх потолка размер не засчитывается, поэтому и требовать больше незачем.
	if p.PerSideCap.IsPositive() {
		capped := p.PerSideCap.Div(decimal.NewFromFloat(weight))
		if needed.GreaterThan(capped) {
			return capped
		}
	}
	return needed
}
