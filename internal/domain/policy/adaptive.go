package policy

import (
	"fmt"

	"github.com/nd/mm-standx/internal/domain/money"
)

// Conditions — измеренное состояние рынка, от которого зависит риск исполнения.
//
// Обе величины наблюдаемы напрямую и не требуют модели: половина спреда берётся
// из стакана, характерный скачок — из истории обновлений mark price.
type Conditions struct {
	// HalfSpreadBps — расстояние от mark до вершины стакана.
	//
	// Именно на эту величину котировка «утоплена» относительно того, что видит
	// рынок: дистанция от mark сама по себе ничего не защищает.
	HalfSpreadBps money.Bps

	// JumpBps — характерный скачок mark price за один такт.
	//
	// Такт составляет три секунды и движение внутри него для нас невидимо
	// (PLAN.md §1.4), поэтому мерой риска служит именно скачок за такт,
	// а не сглаженная волатильность. Значение должно быть уже поправлено
	// на зрелость оценки, см. volatility.Estimate.EffectiveJump.
	JumpBps money.Bps

	// Ready сообщает, что оценка риска опирается на достаточные данные.
	//
	// Проверка живёт здесь, а не в вызывающем коде, намеренно: котирование
	// без представления о волатильности — это ставка вслепую, и запретить её
	// нужно в том же месте, где вычисляется дистанция, чтобы забыть проверку
	// было негде.
	Ready bool
}

// Depth возвращает глубину котировки: сколько рынку нужно пройти от вершины
// стакана, чтобы достать ордер на дистанции d.
func (c Conditions) Depth(d money.Bps) money.Bps { return d - c.HalfSpreadBps }

// Appetite описывает готовность рисковать ради награды.
//
// Асимметрия здесь намеренная и заложена в саму постановку: исполнение стоит
// определённых денег (тейкерская комиссия, половина спреда, неблагоприятный
// отбор), тогда как награда за близость к mark лишь вероятна и её ставка
// не публикуется. Отсюда правило: отступить дешевле, чем поймать залив.
type Appetite struct {
	// JumpMultiple — сколько характерных скачков должно умещаться в глубину.
	// Единица означает готовность к типичному движению, три — запас на выброс.
	JumpMultiple float64

	// FloorMarginBps — абсолютный запас поверх расчётного.
	// Страхует от ситуации, когда рынок замер и измеренный скачок близок к нулю.
	FloorMarginBps money.Bps

	// DeadBandJumps — ширина мёртвой зоны в характерных скачках.
	// Слишком узкая зона заставляет переставлять ордер на обычном шуме.
	DeadBandJumps float64

	// MinDeadBandBps — нижняя граница ширины мёртвой зоны.
	MinDeadBandBps money.Bps

	// ReentryDeadBandBps — насколько требуемая дистанция обязана не дотягивать
	// до края полосы, чтобы вернуться в рынок после отказа.
	//
	// Мёртвая зона для решения «котировать или нет». Порог осуществимости был
	// единственным решением в контуре без неё, и рынок стоял ровно на нём:
	// требуемая дистанция включает половину спреда, а та ходит от 0.12 до 3.5
	// bps внутри часа. Каждый заход хвоста за край снимал лесенку, каждый
	// возврат выставлял её заново — 564 съёма за час при аптайме 99%.
	//
	// Ноль отключает гистерезис.
	ReentryDeadBandBps money.Bps
}

// Bounds — жёсткие границы дистанции, не зависящие от рынка.
type Bounds struct {
	// MinBps — ближе не подходим ни при каких условиях.
	MinBps money.Bps
	// MaxBps — дальше награда обнуляется, это край полосы программы.
	MaxBps money.Bps
}

// Quote — рекомендация по геометрии котировки.
type Quote struct {
	// Distance — целевая дистанция от mark price.
	Distance money.Bps
	// Depth — фактическая глубина в стакане на этой дистанции.
	Depth money.Bps
	// Margin — запас глубины сверх требуемого риском.
	Margin money.Bps
	// Feasible сообщает, существует ли безопасная дистанция внутри полосы.
	Feasible bool
	// Reason описывает исход человекочитаемо.
	Reason string
}

// Target выбирает ближайшую дистанцию, при которой глубина покрывает
// измеренный риск.
//
// Ближе — значит больше веса и больше награды, поэтому подходим настолько,
// насколько позволяют условия, но ни на базисный пункт ближе. Когда условия
// ухудшаются, дистанция уезжает наружу сама, без отдельного правила
// «расширяться при волатильности».
//
// Если требуемая дистанция не помещается в полосу, безопасной котировки
// не существует, и правильный ответ — не котировать вовсе. Ступенчатый зачёт
// по аптайму делает такой отказ дешёвым: до тридцати процентов часа можно
// простоять без котировок, ничего не потеряв.
func (a Appetite) Target(c Conditions, b Bounds) Quote {
	if !c.Ready {
		return Quote{
			Feasible: false,
			Reason:   "risk estimate is not yet based on enough observations",
		}
	}

	required := c.HalfSpreadBps +
		money.Bps(a.JumpMultiple)*c.JumpBps +
		a.FloorMarginBps

	switch {
	case required > b.MaxBps:
		return Quote{
			Feasible: false,
			Reason: fmt.Sprintf("risk demands %s but the reward band ends at %s",
				required, b.MaxBps),
		}
	case required < b.MinBps:
		// Рынок спокоен настолько, что упираемся в собственный нижний предел.
		// Ближе не идём: предел существует именно для таких моментов, когда
		// измеренный риск обманчиво мал.
		required = b.MinBps
	}

	return Quote{
		Distance: required,
		Depth:    c.Depth(required),
		Margin:   c.Depth(required) - money.Bps(a.JumpMultiple)*c.JumpBps,
		Feasible: true,
		Reason:   "risk-adjusted distance",
	}
}

// Thresholds строит зоны гистерезиса вокруг целевой дистанции.
//
// Ширина мёртвой зоны тоже зависит от рынка: на спокойном рынке она узкая
// и котировка стоит близко к оптимуму, на подвижном — широкая, чтобы обычное
// колебание не вызывало перестановку. Фиксированная ширина означала бы либо
// лишние запросы в шторм, либо потерянную награду в штиль.
//
// Пороги опасности привязаны к скачку, а не к доле дистанции: опасность
// определяется тем, может ли рынок дойти до ордера за один такт.
func (a Appetite) Thresholds(q Quote, c Conditions, b Bounds) Thresholds {
	deadBand := money.Bps(a.DeadBandJumps) * c.JumpBps
	if deadBand < a.MinDeadBandBps {
		deadBand = a.MinDeadBandBps
	}

	t := Thresholds{
		TargetBps:  q.Distance,
		MinKeepBps: q.Distance - deadBand,
		MaxKeepBps: q.Distance + deadBand,
		BandBps:    b.MaxBps,
	}

	// Ордер опасен, когда до него остаётся меньше одного такта движения,
	// и безнадёжен, когда рынок уже практически на нём.
	t.PanicBps = c.HalfSpreadBps + c.JumpBps
	t.PullBps = c.HalfSpreadBps

	// Границы могли пересечься на спокойном рынке, где скачок близок к нулю,
	// либо на широком спреде. Восстанавливаем строгий порядок, раздвигая
	// пороги вниз: сдвигать их вверх значило бы приближать ордер к рынку.
	const minSeparation money.Bps = 0.1
	if t.MaxKeepBps > b.MaxBps {
		t.MaxKeepBps = b.MaxBps
	}
	if t.MinKeepBps >= t.TargetBps {
		t.MinKeepBps = t.TargetBps - minSeparation
	}
	if t.PanicBps >= t.MinKeepBps {
		t.PanicBps = t.MinKeepBps - minSeparation
	}
	if t.PullBps >= t.PanicBps {
		t.PullBps = t.PanicBps - minSeparation
	}
	if t.PullBps < 0 {
		t.PullBps = 0
	}
	return t
}
