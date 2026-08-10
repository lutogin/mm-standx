// Package policy содержит правила котирования: где стоять, когда переставлять
// и в каком порядке это делать.
//
// Пакет чистый и детерминированный. Решение принимается по дистанции ордера
// от mark price и ни от чего больше, поэтому его можно прогнать на миллионах
// синтетических последовательностей за секунды.
package policy

import (
	"fmt"

	"github.com/nd/mm-standx/internal/domain/money"
)

// Action — что делать с котировкой.
type Action uint8

const (
	// Hold — ордер в мёртвой зоне, трогать его не нужно.
	//
	// Это самое частое решение и главный источник экономии бюджета запросов:
	// без мёртвой зоны бот дрожал бы на рыночном шуме.
	Hold Action = iota
	// Requote — переставить ордер на целевую дистанцию.
	Requote
	// Pull — снять сторону целиком и уйти в паузу.
	Pull
)

func (a Action) String() string {
	switch a {
	case Hold:
		return "hold"
	case Requote:
		return "requote"
	default:
		return "pull"
	}
}

// Sequence — порядок операций при перестановке.
//
// В API StandX нет изменения ордера, поэтому перестановка всегда состоит
// из отмены и размещения. Порядок решает, чем мы жертвуем: присутствием
// в стакане или безопасностью.
type Sequence uint8

const (
	// PlaceFirst сначала выставляет новый ордер, затем снимает старый.
	// Сторона ни на мгновение не пустеет ценой кратковременной двойной маржи.
	PlaceFirst Sequence = iota
	// CancelFirst сначала снимает старый ордер. Оставляет сторону пустой
	// примерно на 400 мс (PLAN.md §2.4), зато исключает исполнение по цене,
	// к которой уже подошёл рынок.
	CancelFirst
)

func (s Sequence) String() string {
	if s == PlaceFirst {
		return "place-first"
	}
	return "cancel-first"
}

// Urgency — что движет решением.
//
// Слой приложения отображает это в приоритет шейпера запросов: команды
// безопасности обязаны иметь возможность вытеснить команды заработка.
type Urgency uint8

const (
	// UrgencyNone — действий не требуется.
	UrgencyNone Urgency = iota
	// UrgencyReward — награда теряется, но денег это не стоит.
	UrgencyReward
	// UrgencySafety — растёт риск исполнения.
	UrgencySafety
)

func (u Urgency) String() string {
	switch u {
	case UrgencyNone:
		return "none"
	case UrgencyReward:
		return "reward"
	default:
		return "safety"
	}
}

// Decision — результат классификации одной котировки.
type Decision struct {
	Action   Action
	Sequence Sequence
	Urgency  Urgency
	// Reason описывает зону человекочитаемо и попадает в логи и метрики.
	Reason string
}

// Thresholds задаёт зоны гистерезиса. Значения обязаны идти строго
// по возрастанию: Pull < Panic < MinKeep < Target < MaxKeep ≤ Band.
type Thresholds struct {
	PullBps    money.Bps
	PanicBps   money.Bps
	MinKeepBps money.Bps
	TargetBps  money.Bps
	MaxKeepBps money.Bps
	BandBps    money.Bps
}

// Validate проверяет порядок порогов.
func (t Thresholds) Validate() error {
	ordered := []struct {
		name string
		val  money.Bps
	}{
		{"pull", t.PullBps},
		{"panic", t.PanicBps},
		{"minKeep", t.MinKeepBps},
		{"target", t.TargetBps},
	}
	if t.PullBps < 0 {
		return fmt.Errorf("policy: pull threshold must not be negative, got %s", t.PullBps)
	}
	for i := 1; i < len(ordered); i++ {
		if ordered[i].val <= ordered[i-1].val {
			return fmt.Errorf("policy: require %s < %s, got %s >= %s",
				ordered[i-1].name, ordered[i].name, ordered[i-1].val, ordered[i].val)
		}
	}
	if t.MaxKeepBps > t.BandBps {
		return fmt.Errorf("policy: maxKeep (%s) must not exceed the reward band (%s)", t.MaxKeepBps, t.BandBps)
	}
	// Мёртвая зона над целью вправе схлопнуться, но только об край полосы.
	//
	// Требуемая дистанция может лечь ровно на край: полспреда плюс скачки плюс
	// пол складываются в 9.5 при полосе 9.5, и тогда MaxKeep, обрезанный
	// полосой, совпадает с целью. Геометрия при этом остаётся осмысленной —
	// над краем нет ничего, во что можно было бы сдвинуться, а ордер за краем
	// не стоит ничего и обязан переставляться немедленно. Требовать строгого
	// неравенства здесь значило бы объявить невалидной единственную котировку,
	// которую рынок в этот момент допускает.
	//
	// Найдено property-тестом 10 августа при halfSpread 0.5, jump 4.0 и поле 1.0
	// со множителем 2.0.
	if t.MaxKeepBps < t.TargetBps || (t.MaxKeepBps == t.TargetBps && t.MaxKeepBps != t.BandBps) {
		return fmt.Errorf("policy: require target < maxKeep away from the band edge, got %s >= %s",
			t.TargetBps, t.MaxKeepBps)
	}
	return nil
}

// Classify решает судьбу котировки по её текущей дистанции от mark price.
//
// Асимметрия порядка операций — суть правила. Когда рынок идёт на нас,
// дистанция сжимается, и мы сначала убираем ордер: короткая дыра в котировках
// дешевле залива. Когда ордер выпадает наружу, заливом не пахнет, и мы сначала
// выставляем новый, чтобы сторона не пустела.
func (t Thresholds) Classify(d money.Bps) Decision {
	switch {
	case d < t.PullBps:
		// Рынок вплотную. Ордер уже почти маркетабелен, и переставлять его
		// бессмысленно: следующий тик снова застанет нас на опасной дистанции.
		return Decision{Action: Pull, Sequence: CancelFirst, Urgency: UrgencySafety, Reason: "inside pull threshold"}

	case d < t.PanicBps:
		return Decision{Action: Requote, Sequence: CancelFirst, Urgency: UrgencySafety, Reason: "price approaching"}

	case d < t.MinKeepBps:
		// Ещё не опасно, но заметно ближе желаемого. Присутствие важнее,
		// поэтому порядок обычный.
		return Decision{Action: Requote, Sequence: PlaceFirst, Urgency: UrgencySafety, Reason: "closer than target range"}

	case d <= t.MaxKeepBps:
		return Decision{Action: Hold, Sequence: PlaceFirst, Urgency: UrgencyNone, Reason: "within dead band"}

	case d <= t.BandBps:
		return Decision{Action: Requote, Sequence: PlaceFirst, Urgency: UrgencyReward, Reason: "drifted outward"}

	default:
		// За полосой вес равен нулю: ордер стоит в стакане и не приносит ничего.
		return Decision{Action: Requote, Sequence: PlaceFirst, Urgency: UrgencyReward, Reason: "outside reward band"}
	}
}

// Ladder описывает набор ступеней на одной стороне.
//
// Лесенка существует потому, что перестановка занимает около 400 мс, в течение
// которых сторона с единственным ордером пуста, а зачёт равен нулю. Переставляя
// ступени по одной, мы теряем долю размера вместо всего присутствия.
type Ladder struct {
	Layers    int
	SpreadBps money.Bps
}

// HalfWidth — расстояние от центра лесенки до крайней ступени.
func (l Ladder) HalfWidth() money.Bps {
	if l.Layers <= 1 {
		return 0
	}
	return money.Bps(float64(l.Layers-1)) * l.SpreadBps / 2
}

// Distances возвращает дистанции ступеней, центрированные вокруг target,
// от ближней к mark до дальней.
func (l Ladder) Distances(target money.Bps) []money.Bps {
	if l.Layers <= 1 {
		return []money.Bps{target}
	}

	half := l.HalfWidth()
	out := make([]money.Bps, l.Layers)
	for i := range out {
		out[i] = target - half + money.Bps(float64(i))*l.SpreadBps
	}
	return out
}

// Anchor превращает дистанцию, вычисленную для одного ордера, в центр лесенки.
//
// Appetite.Target отвечает на вопрос «где безопасно стоять ордеру», и ответ
// относится к одному ордеру. Ступени же расставляются вокруг этой дистанции,
// и ближняя оказывается на половину ширины лесенки ближе к рынку, чем позволяет
// мера риска. Рынок достаёт первой именно её, поэтому требование риска обязано
// относиться к ней, а центр — отодвигаться наружу.
//
// Depth и Margin остаются от исходной дистанции намеренно: они описывают ту
// ступень, до которой рынку идти меньше всего, то есть связывающую.
//
// Дальняя ступень при этом обязана уместиться в полосу награды. Ордер за её
// краем не приносит ничего и попадает в зону перестановки сразу после
// размещения — бот переставлял бы его бесконечно, платя запросами за ноль.
func (l Ladder) Anchor(q Quote, b Bounds) Quote {
	if !q.Feasible {
		return q
	}
	width := l.HalfWidth()
	if width <= 0 {
		return q
	}

	outer := q.Distance + 2*width
	if outer > b.MaxBps {
		return Quote{
			Feasible: false,
			Reason: fmt.Sprintf(
				"the ladder does not fit: the near rung needs %s and the outer one would reach %s past the band at %s",
				q.Distance, outer, b.MaxBps),
		}
	}

	q.Distance += width
	return q
}

// Fit возвращает самую широкую лесенку, помещающуюся в полосу на дистанции,
// которую требует риск, вместе с её заякоренной котировкой.
//
// Существует потому, что прежний ответ на «лесенка не помещается» был отказом
// котировать вовсе. Это всё-или-ничего там, где есть очевидное промежуточное
// решение: три ступени занимают (layers-1) × spread = 0.8 bps из бюджета
// дистанции, одна не занимает ничего. Когда спред расходится и требуемая
// дистанция подходит к краю полосы, лесенка перестаёт помещаться раньше, чем
// сама котировка, — и актор уходил из стакана, имея законное место для одного
// ордера. 10 августа это дало 30 отказов подряд при спреде золота 4.4-5.1 bps.
//
// Сужение предпочтительнее отказа по прямому счёту: три ступени существуют
// ради присутствия во время перестановки (PresenceLoss), а отказ обнуляет
// присутствие целиком. Диверсификации риска они не дают — 6 августа все три
// ушли одним свипом за 51 мс.
func (l Ladder) Fit(q Quote, b Bounds) (Ladder, Quote) {
	for layers := l.Layers; layers > 1; layers-- {
		candidate := Ladder{Layers: layers, SpreadBps: l.SpreadBps}
		if anchored := candidate.Anchor(q, b); anchored.Feasible {
			return candidate, anchored
		}
	}
	// Одна ступень не занимает ширины вовсе: если не помещается и она, места
	// нет ни для какой котировки, и Anchor вернёт исходный отказ.
	single := Ladder{Layers: 1, SpreadBps: l.SpreadBps}
	return single, single.Anchor(q, b)
}

// Fits сообщает, помещается ли лесенка целиком в удерживаемый диапазон.
//
// Ступень за пределами диапазона попадала бы в зону перестановки сразу после
// размещения, и бот переставлял бы её бесконечно.
func (l Ladder) Fits(t Thresholds) bool {
	for _, d := range l.Distances(t.TargetBps) {
		if d < t.MinKeepBps || d > t.MaxKeepBps {
			return false
		}
	}
	return true
}

// PresenceLoss оценивает долю размера стороны, теряемую на время перестановки
// одной ступени. При единственной ступени сторона пустеет полностью.
func (l Ladder) PresenceLoss() float64 {
	if l.Layers <= 1 {
		return 1
	}
	return 1 / float64(l.Layers)
}
