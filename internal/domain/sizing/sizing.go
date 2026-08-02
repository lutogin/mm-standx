// Package sizing вычисляет размеры котировок.
//
// Размер не задаётся в конфигурации константой, потому что он связан
// с дистанцией: вес ордера растёт при приближении к mark, а дистанция теперь
// вычисляется по рыночным условиям. Фиксированный размер означал бы, что
// при сжатии дистанции капитал расходуется впустую, а при расширении — что
// котировка внезапно перестаёт помещаться в маржу.
package sizing

import (
	"fmt"

	"github.com/shopspring/decimal"

	"github.com/nd/mm-standx/internal/domain/market"
	"github.com/nd/mm-standx/internal/domain/money"
	"github.com/nd/mm-standx/internal/domain/reward"
)

// Budget — доступный инструменту капитал.
type Budget struct {
	// AllocationUSD — сколько средств выделено этому инструменту конфигурацией.
	AllocationUSD decimal.Decimal
	// AvailableUSD — сколько средств биржа считает свободными прямо сейчас.
	AvailableUSD decimal.Decimal
	// Leverage — плечо позиции.
	Leverage int
	// UtilizationPct — какую долю доступного разрешено занимать.
	//
	// Остаток нужен не про запас, а по необходимости: висящие ордера блокируют
	// маржу, и при полной утилизации закрывающий ордер после залива может
	// не пройти по нехватке средств.
	UtilizationPct float64
}

// Plan — результат расчёта.
type Plan struct {
	// Rungs — размер каждой ступени лесенки, от ближней к mark до дальней.
	Rungs []money.Qty
	// PerSide — суммарный размер стороны.
	PerSide money.Qty
	// Reason описывает, что ограничило размер.
	Reason string
}

// Inputs — всё, что нужно для расчёта.
type Inputs struct {
	Budget    Budget
	Spec      market.SymbolSpec
	Program   reward.Program
	Mark      money.Price
	Distances []money.Bps
}

// Compute распределяет капитал по ступеням лесенки.
//
// Порядок ограничений важен: сначала капитал, затем полезный потолок программы,
// затем лимиты инструмента. Каждое следующее может только уменьшить результат.
func Compute(in Inputs) (Plan, error) {
	if len(in.Distances) == 0 {
		return Plan{}, fmt.Errorf("sizing: no ladder distances given")
	}
	if !in.Mark.IsPositive() {
		return Plan{}, fmt.Errorf("sizing: mark price must be positive, got %s", in.Mark)
	}
	if in.Budget.Leverage < 1 {
		return Plan{}, fmt.Errorf("sizing: leverage must be at least 1, got %d", in.Budget.Leverage)
	}

	capital, reason := usableCapital(in.Budget)
	if !capital.IsPositive() {
		return Plan{}, fmt.Errorf("sizing: no usable capital: %s", reason)
	}

	// Обе стороны требуют маржи, поэтому капитал делится пополам.
	notionalPerSide := capital.
		Mul(decimal.NewFromInt(int64(in.Budget.Leverage))).
		Div(decimal.NewFromInt(2))

	perSide, err := money.QtyForNotional(notionalPerSide, in.Mark)
	if err != nil {
		return Plan{}, fmt.Errorf("sizing: %w", err)
	}

	// Размер сверх программного потолка не засчитывается, поэтому и держать
	// его в стакане незачем: это риск исполнения без встречной награды.
	if useful := usefulCeiling(in); useful.IsPositive() && perSide.Gt(useful) {
		perSide, reason = useful, "capped by the reward ceiling"
	}

	rungs, err := split(perSide, in.Distances, in.Spec)
	if err != nil {
		return Plan{}, err
	}

	total := money.MustParseQty("0")
	for _, r := range rungs {
		total = total.Add(r)
	}
	if len(rungs) < len(in.Distances) {
		reason = fmt.Sprintf("%s; ladder reduced to %d rungs by the minimum order size",
			reason, len(rungs))
	}
	return Plan{Rungs: rungs, PerSide: total, Reason: reason}, nil
}

// usableCapital берёт меньшее из выделенного и фактически доступного.
func usableCapital(b Budget) (decimal.Decimal, string) {
	utilization := decimal.NewFromFloat(b.UtilizationPct / 100)
	headroom := b.AvailableUSD.Mul(utilization)

	if b.AllocationUSD.IsPositive() && b.AllocationUSD.LessThan(headroom) {
		return b.AllocationUSD, "limited by the configured allocation"
	}
	return headroom, "limited by available margin"
}

// usefulCeiling возвращает размер стороны, сверх которого награда не растёт.
//
// Потолок зависит от дистанции: чем ближе котировка, тем больше её вес и тем
// меньше нужно единиц, чтобы упереться в программный предел.
func usefulCeiling(in Inputs) money.Qty {
	if !in.Program.PerSideCap.IsPositive() {
		return money.Qty{}
	}

	// Считаем по ближней ступени: у неё наибольший вес, и именно она первой
	// упирается в потолок.
	nearest := in.Distances[0]
	for _, d := range in.Distances {
		if d < nearest {
			nearest = d
		}
	}
	weight := in.Program.Curve.Weight(nearest)
	if weight <= 0 {
		return money.Qty{}
	}
	return money.NewQty(in.Program.PerSideCap.Div(decimal.NewFromFloat(weight)))
}

// split делит размер стороны между ступенями поровну.
//
// Равное деление выбрано намеренно, хотя перекос в сторону ближних ступеней
// дал бы больше зачёта на тот же капитал: ближние ступени рискованнее, а цена
// исполнения превышает выигрыш в весе.
//
// Если ступень не дотягивает до минимального размера инструмента, число ступеней
// уменьшается. Лучше две полноценные ступени, чем три отвергнутых ордера.
func split(perSide money.Qty, distances []money.Bps, spec market.SymbolSpec) ([]money.Qty, error) {
	for layers := len(distances); layers >= 1; layers-- {
		each := spec.SnapQty(money.NewQty(
			perSide.Decimal().Div(decimal.NewFromInt(int64(layers)))))

		if err := spec.ValidateQty(each); err != nil {
			continue
		}
		rungs := make([]money.Qty, layers)
		for i := range rungs {
			rungs[i] = each
		}
		return rungs, nil
	}

	return nil, fmt.Errorf(
		"sizing: capital allows %s per side, below the %s minimum for %s",
		perSide, spec.MinOrderQty, spec.Symbol)
}
