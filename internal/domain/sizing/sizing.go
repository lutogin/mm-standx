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
//
// Единственный источник — состояние счёта. Настроенной аллокации здесь больше
// нет намеренно: счёт целиком отдан этой стратегии, и число в файле, которое
// обязано следовать за балансом, всегда от него отстаёт. 15 августа так и было
// — 5800 при балансе 11091, то есть половина денег простаивала, и увидеть это
// можно было только по строке size_limit в сводке.
//
// Если инструментов когда-нибудь станет больше одного, аллокация вернётся —
// но уже как доля, а не как сумма в долларах.
type Budget struct {
	// EquityUSD — весь капитал счёта, включая уже заблокированное.
	//
	// Именно equity, а не available. Свободный остаток уже уменьшен тем, что
	// мы сами и заблокировали, поэтому бюджет от него — петля: каждый
	// выставленный доллар сокращает разрешённое. Она сходится примерно
	// на 40% капитала и не переваливает половины ни при каком значении
	// UtilizationPct, сколько его ни поднимай.
	EquityUSD decimal.Decimal
	// SpendableUSD — сколько биржа готова заблокировать под новыми висящими
	// ордерами прямо сейчас: свободный остаток плюс уже заблокированное
	// нашими собственными ордерами.
	//
	// Это жёсткий потолок над бюджетом от equity, и он существует потому,
	// что бюджет считается по InitialMarginPct — величине ИЗМЕРЕННОЙ,
	// а не известной. Замер боевого счёта 28 августа 2026, 13:10 UTC: при
	// equity 20596 и утилизации 85% бюджет вышел 17507 USD маржи, что при
	// измеренной ставке 2.667% дало 656400 USD нотионала. Биржа списала
	// 18948, то есть действующая ставка была 2.887% — оценка ошиблась
	// на 7.6%, бот занял 92% капитала вместо 85%, и на третью ступень
	// не хватило. Дальше пять минут отказов «balance not enough»: 550 штук,
	// по 1.8 в секунду.
	//
	// Петли, из-за которой размер считается от equity, здесь нет, и это
	// главное свойство суммы. Выставленный доллар уходит из Available
	// и ровно в тот же миг появляется в Locked; сумма не меняется. Убывает
	// она только от того, что действительно уходит со счёта, — от маржи под
	// открытой позицией и от убытка.
	//
	// Ноль отключает потолок: снимок баланса может ещё не прийти, и в этом
	// случае решает один бюджет от equity, как решал раньше.
	SpendableUSD decimal.Decimal
	// InitialMarginPct — доля нотионала, которую биржа блокирует под висящим
	// ордером, в процентах.
	//
	// Не плечо. Плечо, положенное в ордер, биржа для блокировки игнорирует
	// полностью: проба 12 августа дала одинаковые 10.542% при запрошенных
	// 4, 8 и 20 (PLAN.md §12.5). Доля берётся от цены ордера, линейна
	// по размеру и считается по обеим сторонам без взаимозачёта.
	InitialMarginPct float64
	// UtilizationPct — какую долю капитала разрешено держать под маржой.
	//
	// Остаток нужен не про запас, а по необходимости: после залива закрывающий
	// ордер требует маржи под позицию, и при полной утилизации он может
	// не пройти.
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
	if in.Budget.InitialMarginPct <= 0 || in.Budget.InitialMarginPct > 100 {
		return Plan{}, fmt.Errorf(
			"sizing: initial margin must be within (0, 100]%%, got %.4f", in.Budget.InitialMarginPct)
	}

	capital, reason := usableCapital(in.Budget)
	if !capital.IsPositive() {
		return Plan{}, fmt.Errorf("sizing: no usable capital: %s", reason)
	}

	// Бюджет — это маржа, а не нотионал: сколько капитала разрешено держать
	// заблокированным. Нотионал получается делением на долю блокировки,
	// и обе стороны требуют её независимо, без взаимозачёта, — отсюда пополам.
	rate := decimal.NewFromFloat(in.Budget.InitialMarginPct / 100)
	notionalPerSide := capital.Div(rate).Div(decimal.NewFromInt(2))

	perSide, err := money.QtyForNotional(notionalPerSide, in.Mark)
	if err != nil {
		return Plan{}, fmt.Errorf("sizing: %w", err)
	}

	rungs, capped, err := allocate(perSide, in)
	if err != nil {
		return Plan{}, err
	}
	if capped {
		reason = "capped by the reward ceiling"
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

// usableCapital — разрешённая к блокировке доля капитала счёта.
//
// Два ограничения, и меньшее побеждает. Бюджет от equity — это НАМЕРЕНИЕ:
// какую долю счёта мы согласны держать под маржой. Сумма, которую сообщила
// биржа, — это ФАКТ: сколько она согласна заблокировать. Намерение считается
// по измеренной доле блокировки и потому может промахнуться; факт промахнуться
// не может, потому что ничего не оценивает.
func usableCapital(b Budget) (decimal.Decimal, string) {
	utilization := decimal.NewFromFloat(b.UtilizationPct / 100)
	capital := b.EquityUSD.Mul(utilization)
	if b.SpendableUSD.IsPositive() && b.SpendableUSD.LessThan(capital) {
		return b.SpendableUSD, "limited by the free balance the venue reports"
	}
	return capital, "limited by the margin budget"
}

// usefulCeiling возвращает размер стороны, сверх которого награда не растёт.
//
// Потолок зависит от дистанции: чем ближе котировка, тем больше её вес и тем
// меньше нужно единиц, чтобы упереться в программный предел.
//
// THE DIVISOR IS THE AVERAGE RUNG WEIGHT, NOT THE NEAREST ONE. The programme
// scores the sum over every rung at its own weight, while the size is split
// evenly, so what the side actually reaches is `perSide x mean(weight)`.
// Dividing the cap by the nearest weight therefore aims at a target the ladder
// cannot hit: with rungs at 7.0 / 7.4 / 7.8 bps the mean weight is 0.816
// against 0.880 at the near rung, and the side settles 7% under its own
// ceiling.
//
// Замер 24 августа 2026 (PLAN.md §0), отчёт Maker Yield History по XAU-USD
// за часы 06:00-14:00 UTC: P70 = 54.3…59.3 при потолке 60, то есть
// MakerHours 1.81…1.98 из достижимых 2.00. Предсказание прежней формулы —
// 60 / 0.880 x 0.816 = 55.6 — лежит внутри измеренного интервала, и недобор
// до потолка объясняется целиком ею, а не аптаймом и не заливами.
//
// Расчёт обязан идти по тем ступеням, которые действительно будут выставлены:
// лесенка, суженная минимальным размером ордера, оставляет ближние ступени
// с более высокими весами, и средний вес по исходному списку дал бы перелёт
// над потолком вместо недолёта. Поэтому потолок считается внутри allocate,
// где число ступеней уже известно.
func usefulCeiling(p reward.Program, distances []money.Bps) money.Qty {
	if !p.PerSideCap.IsPositive() || len(distances) == 0 {
		return money.Qty{}
	}

	total := 0.0
	for _, d := range distances {
		total += p.Curve.Weight(d)
	}
	if total <= 0 {
		return money.Qty{}
	}
	average := total / float64(len(distances))
	return money.NewQty(p.PerSideCap.Div(decimal.NewFromFloat(average)))
}

// allocate делит размер стороны между ступенями поровну, применяя программный
// потолок к тому числу ступеней, которое действительно уместилось.
//
// Равное деление выбрано намеренно, хотя перекос в сторону ближних ступеней
// дал бы больше зачёта на тот же капитал: ближние ступени рискованнее, а цена
// исполнения превышает выигрыш в весе.
//
// Если ступень не дотягивает до минимального размера инструмента, число ступеней
// уменьшается. Лучше две полноценные ступени, чем три отвергнутых ордера.
//
// The ceiling lives inside this loop rather than before it because the two
// decisions are coupled: fewer rungs means a higher mean weight, which means a
// lower ceiling. Capping first and shrinking afterwards would leave the side
// above the cap — with weights 0.96 / 0.80 / 0.64, a ladder cut to two rungs
// would reach 1.10x the cap, and the excess earns nothing while carrying the
// full risk of a fill.
func allocate(perSide money.Qty, in Inputs) ([]money.Qty, bool, error) {
	for layers := len(in.Distances); layers >= 1; layers-- {
		// Размер сверх программного потолка не засчитывается, поэтому и держать
		// его в стакане незачем: это риск исполнения без встречной награды.
		size, capped := perSide, false
		if useful := usefulCeiling(in.Program, in.Distances[:layers]); useful.IsPositive() && size.Gt(useful) {
			size, capped = useful, true
		}

		each := in.Spec.SnapQty(money.NewQty(
			size.Decimal().Div(decimal.NewFromInt(int64(layers)))))

		if err := in.Spec.ValidateQty(each); err != nil {
			continue
		}
		rungs := make([]money.Qty, layers)
		for i := range rungs {
			rungs[i] = each
		}
		return rungs, capped, nil
	}

	return nil, false, fmt.Errorf(
		"sizing: capital allows %s per side, below the %s minimum for %s",
		perSide, in.Spec.MinOrderQty, in.Spec.Symbol)
}
