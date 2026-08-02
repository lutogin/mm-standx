// Package money содержит числовые примитивы для цен и количеств.
//
// Все денежные вычисления идут через decimal.Decimal. float64 в этом пакете
// допустим только для Bps: базисные пункты — пороговая величина контура управления,
// а не деньги. Любая цена, уходящая на биржу, обязана пройти через снэп к тику.
//
// Price и Qty — разные типы намеренно: компилятор не даст сложить цену с количеством.
package money

import (
	"encoding/json"
	"fmt"

	"github.com/shopspring/decimal"
)

// BpsScale — количество базисных пунктов в единице.
const BpsScale = 10_000

// Price — цена инструмента. Всегда положительна.
type Price struct{ d decimal.Decimal }

// Qty — количество в базовом активе. Всегда неотрицательно.
type Qty struct{ d decimal.Decimal }

// Bps — расстояние или доля в базисных пунктах.
//
// Это единственное место в проекте, где допустим float64: величина используется
// только для сравнения с порогами конфигурации и для метрик. Цена по дистанции
// вычисляется в decimal, см. market.PriceAtDistance.
type Bps float64

// --- Price ---

func NewPrice(d decimal.Decimal) Price { return Price{d: d} }

func ParsePrice(s string) (Price, error) {
	d, err := decimal.NewFromString(s)
	if err != nil {
		return Price{}, fmt.Errorf("parse price %q: %w", s, err)
	}
	return Price{d: d}, nil
}

// MustParsePrice предназначена для констант и тестов, где вход заведомо валиден.
func MustParsePrice(s string) Price {
	p, err := ParsePrice(s)
	if err != nil {
		panic(err)
	}
	return p
}

func (p Price) Decimal() decimal.Decimal { return p.d }
func (p Price) String() string           { return p.d.String() }
func (p Price) IsZero() bool             { return p.d.IsZero() }
func (p Price) IsPositive() bool         { return p.d.IsPositive() }

func (p Price) Cmp(o Price) int  { return p.d.Cmp(o.d) }
func (p Price) Lt(o Price) bool  { return p.d.LessThan(o.d) }
func (p Price) Lte(o Price) bool { return p.d.LessThanOrEqual(o.d) }
func (p Price) Gt(o Price) bool  { return p.d.GreaterThan(o.d) }
func (p Price) Gte(o Price) bool { return p.d.GreaterThanOrEqual(o.d) }

func (p Price) Add(o Price) Price { return Price{d: p.d.Add(o.d)} }
func (p Price) Sub(o Price) Price { return Price{d: p.d.Sub(o.d)} }

// MulRatio умножает цену на безразмерный коэффициент (например 1.0008).
func (p Price) MulRatio(r decimal.Decimal) Price { return Price{d: p.d.Mul(r)} }

// SnapDown округляет цену вниз до заданного числа знаков после запятой.
func (p Price) SnapDown(places int32) Price { return Price{d: p.d.RoundFloor(places)} }

// SnapUp округляет цену вверх до заданного числа знаков после запятой.
func (p Price) SnapUp(places int32) Price { return Price{d: p.d.RoundCeil(places)} }

func (p Price) MarshalJSON() ([]byte, error) { return json.Marshal(p.d.String()) }

func (p *Price) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	// Биржа отдаёт незаполненные цены пустой строкой или null.
	if s == "" {
		*p = Price{d: decimal.Zero}
		return nil
	}
	parsed, err := ParsePrice(s)
	if err != nil {
		return err
	}
	*p = parsed
	return nil
}

// --- Qty ---

func NewQty(d decimal.Decimal) Qty { return Qty{d: d} }

func ParseQty(s string) (Qty, error) {
	d, err := decimal.NewFromString(s)
	if err != nil {
		return Qty{}, fmt.Errorf("parse qty %q: %w", s, err)
	}
	return Qty{d: d}, nil
}

func MustParseQty(s string) Qty {
	q, err := ParseQty(s)
	if err != nil {
		panic(err)
	}
	return q
}

func (q Qty) Decimal() decimal.Decimal { return q.d }
func (q Qty) String() string           { return q.d.String() }
func (q Qty) IsZero() bool             { return q.d.IsZero() }
func (q Qty) IsPositive() bool         { return q.d.IsPositive() }
func (q Qty) IsNegative() bool         { return q.d.IsNegative() }

func (q Qty) Cmp(o Qty) int  { return q.d.Cmp(o.d) }
func (q Qty) Lt(o Qty) bool  { return q.d.LessThan(o.d) }
func (q Qty) Lte(o Qty) bool { return q.d.LessThanOrEqual(o.d) }
func (q Qty) Gt(o Qty) bool  { return q.d.GreaterThan(o.d) }
func (q Qty) Gte(o Qty) bool { return q.d.GreaterThanOrEqual(o.d) }

func (q Qty) Add(o Qty) Qty                  { return Qty{d: q.d.Add(o.d)} }
func (q Qty) Sub(o Qty) Qty                  { return Qty{d: q.d.Sub(o.d)} }
func (q Qty) Abs() Qty                       { return Qty{d: q.d.Abs()} }
func (q Qty) Neg() Qty                       { return Qty{d: q.d.Neg()} }
func (q Qty) MulRatio(r decimal.Decimal) Qty { return Qty{d: q.d.Mul(r)} }

// SnapDown округляет количество вниз: превысить лимит размера опаснее, чем недобрать.
func (q Qty) SnapDown(places int32) Qty { return Qty{d: q.d.RoundFloor(places)} }

func (q Qty) MarshalJSON() ([]byte, error) { return json.Marshal(q.d.String()) }

func (q *Qty) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	if s == "" {
		*q = Qty{d: decimal.Zero}
		return nil
	}
	parsed, err := ParseQty(s)
	if err != nil {
		return err
	}
	*q = parsed
	return nil
}

// --- Производные величины ---

// Notional возвращает стоимость позиции в котируемом активе.
func Notional(p Price, q Qty) decimal.Decimal { return p.d.Mul(q.d.Abs()) }

// QtyForNotional возвращает количество, дающее заданную стоимость по указанной цене.
func QtyForNotional(notional decimal.Decimal, p Price) (Qty, error) {
	if !p.IsPositive() {
		return Qty{}, fmt.Errorf("qty for notional: price must be positive, got %s", p)
	}
	return Qty{d: notional.Div(p.d)}, nil
}

// --- Bps ---

func (b Bps) Float64() float64 { return float64(b) }

func (b Bps) String() string { return fmt.Sprintf("%.2fbps", float64(b)) }

// Ratio переводит базисные пункты в безразмерную долю: 10bps → 0.001.
func (b Bps) Ratio() decimal.Decimal {
	return decimal.NewFromFloat(float64(b)).Div(decimal.NewFromInt(BpsScale))
}

func (b Bps) Abs() Bps {
	if b < 0 {
		return -b
	}
	return b
}

// RelativeBps возвращает знаковое отклонение to от base в базисных пунктах.
// Результат положителен, когда to выше base.
func RelativeBps(base, to Price) (Bps, error) {
	if !base.IsPositive() {
		return 0, fmt.Errorf("relative bps: base must be positive, got %s", base)
	}
	diff := to.d.Sub(base.d).Div(base.d).Mul(decimal.NewFromInt(BpsScale))
	f, _ := diff.Float64()
	return Bps(f), nil
}
