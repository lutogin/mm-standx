// Package uptime ведёт учёт присутствия в стакане и планирует поведение
// внутри календарного часа.
//
// Учёт устроен так же, как у биржи: выборка величины min(bid, ask) с постоянным
// шагом. Совпадение метода принципиально — расхождение нашей оценки с отчётом
// StandX означает ошибку в модели, и обнаружить её нужно раньше, чем она станет
// расхождением в деньгах.
package uptime

import (
	"time"

	"github.com/shopspring/decimal"

	"github.com/nd/mm-standx/internal/domain/reward"
)

// Mode — режим поведения на остаток часа.
type Mode uint8

const (
	// Earning — обычная работа: котируем ради зачёта.
	Earning Mode = iota
	// Banked — порог уже взят, дальнейший аптайм ничего не добавит.
	//
	// Зачёт ступенчатый: сто процентов присутствия дают ровно столько же,
	// сколько семьдесят. Набрав запас, разумно отойти дальше от рынка —
	// награда та же, риск меньше.
	Banked
	// Lost — час недостижим математически. Продолжать значит платить
	// за исполнения, не получая взамен ничего.
	Lost
)

func (m Mode) String() string {
	switch m {
	case Earning:
		return "earning"
	case Banked:
		return "banked"
	default:
		return "lost"
	}
}

// Tracker накапливает выборку за текущий час.
type Tracker struct {
	program reward.Program
	// targetPct — доля часа, которую считаем целью. Выше порога лучшего
	// тира, чтобы дрожание вокруг границы не стоило половины зачёта.
	targetPct float64

	hour    time.Time
	samples []decimal.Decimal
	nonZero int
}

// New создаёт учёт для инструмента.
func New(program reward.Program, targetPct float64) *Tracker {
	return &Tracker{program: program, targetPct: targetPct}
}

// Sample фиксирует очередное наблюдение. Нулевое значение означает отсутствие
// квалифицирующих котировок и учитывается наравне с остальными: именно нули
// и делают метрику зависимой от присутствия.
func (t *Tracker) Sample(now time.Time, value decimal.Decimal) {
	hour := now.UTC().Truncate(time.Hour)
	if !hour.Equal(t.hour) {
		t.hour = hour
		t.samples = t.samples[:0]
		t.nonZero = 0
	}

	t.samples = append(t.samples, value)
	if value.IsPositive() {
		t.nonZero++
	}
}

// Snapshot — состояние часа.
type Snapshot struct {
	Mode Mode
	// QualifiedPct — доля наблюдений с живыми котировками.
	QualifiedPct float64
	// ElapsedPct — какая часть часа прошла.
	ElapsedPct float64
	// BestPossiblePct — предел присутствия, если котировать без перерыва
	// до конца часа.
	BestPossiblePct float64
	// DowntimeBudget — сколько ещё можно простоять, не потеряв цель.
	DowntimeBudget time.Duration
	// MakerHours — оценка начисления по накопленной выборке.
	MakerHours float64
	// Multiplier is the accrual factor in force for this hour. Below one it
	// means the reference market is shut and the hour is worth a fraction of
	// a normal one, while the fill risk stays exactly the same.
	Multiplier float64
	// Reason описывает выбранный режим.
	Reason string
}

// Assess оценивает час и выбирает режим на остаток.
//
// Здесь и живёт основная альфа стратегии. Ступенчатый зачёт превращает час
// в задачу с тремя исходами, и в двух из трёх правильное действие —
// уменьшить риск, а не наращивать присутствие.
func (t *Tracker) Assess(now time.Time) Snapshot {
	elapsed := now.UTC().Sub(now.UTC().Truncate(time.Hour))
	elapsedPct := float64(elapsed) / float64(time.Hour) * 100

	s := Snapshot{
		ElapsedPct: elapsedPct,
		MakerHours: t.program.MakerHours(t.samples, now),
		Multiplier: t.program.Multiplier(now),
	}
	if len(t.samples) == 0 {
		s.Reason = "no observations yet"
		s.DowntimeBudget = time.Hour
		return s
	}

	s.QualifiedPct = float64(t.nonZero) / float64(len(t.samples)) * 100
	// Доля часа, набранная к текущему моменту, и потолок при безупречной
	// работе до конца часа.
	bankedOfHour := s.QualifiedPct * elapsedPct / 100
	s.BestPossiblePct = bankedOfHour + (100 - elapsedPct)

	lowest := t.lowestThreshold()
	switch {
	case bankedOfHour >= t.targetPct:
		s.Mode = Banked
		s.Reason = "target already secured, further uptime adds nothing"

	case s.BestPossiblePct < lowest:
		// Даже безупречная работа до конца часа не дотянет до нижнего тира.
		s.Mode = Lost
		s.Reason = "hour is unreachable, further risk buys nothing"

	default:
		s.Mode = Earning
		s.Reason = "accumulating uptime"
	}

	// Бюджет простоя: сколько времени можно не котировать, оставаясь у цели.
	if slack := s.BestPossiblePct - t.targetPct; slack > 0 {
		s.DowntimeBudget = time.Duration(slack / 100 * float64(time.Hour))
	}
	return s
}

func (t *Tracker) lowestThreshold() float64 {
	lowest := 100.0
	for _, tier := range t.program.Tiers {
		if tier.ThresholdPct < lowest {
			lowest = tier.ThresholdPct
		}
	}
	return lowest
}

// Samples возвращает число наблюдений в текущем часе.
func (t *Tracker) Samples() int { return len(t.samples) }
