// Package quotes хранит состояние лесенки котировок одной стороны и решает,
// какую ступень трогать следующей.
//
// Пакет чистый: время приходит параметром, идентификаторы ордеров — извне.
// Здесь сосредоточен инвариант, нарушение которого стоит присутствия в стакане:
// в штатном режиме переставляется ровно одна ступень за раз.
package quotes

import (
	"time"

	"github.com/nd/mm-standx/internal/domain/market"
	"github.com/nd/mm-standx/internal/domain/money"
	"github.com/nd/mm-standx/internal/domain/policy"
	"github.com/nd/mm-standx/internal/domain/trading"
)

// Phase — стадия жизни ступени.
type Phase uint8

const (
	// Empty — ступень свободна.
	Empty Phase = iota
	// Placing — команда размещения отправлена, подтверждения нет.
	Placing
	// Live — ордер стоит в стакане.
	Live
	// Cancelling — команда отмены отправлена, подтверждения нет.
	Cancelling
)

func (p Phase) String() string {
	switch p {
	case Empty:
		return "empty"
	case Placing:
		return "placing"
	case Live:
		return "live"
	default:
		return "cancelling"
	}
}

// InFlight сообщает, что ступень ждёт ответа биржи.
func (p Phase) InFlight() bool { return p == Placing || p == Cancelling }

// Slot — одна ступень лесенки.
type Slot struct {
	Index   int
	Phase   Phase
	ClOrdID trading.ClOrdID
	Price   money.Price
	Qty     money.Qty
	// Since — момент входа в текущую фазу. Нужен и для таймаутов подтверждения,
	// и для правила минимального времени жизни ордера.
	Since time.Time
}

// ActionKind — что делать со ступенью.
type ActionKind uint8

const (
	// Place — выставить новый ордер.
	Place ActionKind = iota
	// Cancel — снять существующий.
	Cancel
)

func (k ActionKind) String() string {
	if k == Place {
		return "place"
	}
	return "cancel"
}

// Action — команда по одной ступени.
type Action struct {
	Kind     ActionKind
	Slot     int
	Distance money.Bps
	ClOrdID  trading.ClOrdID
	Urgency  policy.Urgency
	Reason   string
}

// Ladder — состояние одной стороны.
type Ladder struct {
	Side  market.Side
	Slots []Slot
	// cursor задаёт очередь round-robin, чтобы ступени переставлялись
	// по очереди, а не одна и та же снова и снова.
	cursor int
}

// New создаёт лесенку с заданным числом пустых ступеней.
func New(side market.Side, layers int) *Ladder {
	slots := make([]Slot, layers)
	for i := range slots {
		slots[i] = Slot{Index: i}
	}
	return &Ladder{Side: side, Slots: slots}
}

// InFlight возвращает число ступеней, ожидающих ответа биржи.
func (l *Ladder) InFlight() int {
	n := 0
	for _, s := range l.Slots {
		if s.Phase.InFlight() {
			n++
		}
	}
	return n
}

// LiveCount возвращает число ступеней, стоящих в стакане.
func (l *Ladder) LiveCount() int {
	n := 0
	for _, s := range l.Slots {
		if s.Phase == Live {
			n++
		}
	}
	return n
}

// Populated сообщает, присутствует ли сторона в стакане.
//
// Пустая сторона обнуляет зачёт целиком: программа считает слабейшую сторону,
// а не сумму.
func (l *Ladder) Populated() bool { return l.LiveCount() > 0 }

// Rules — ограничения на темп перестановки.
type Rules struct {
	// MinRest — минимальное время жизни ордера. Заодно закрывает требование
	// программы о трёх секундах для начисления Maker Points.
	MinRest time.Duration
	// AckTimeout — сколько ждать подтверждения, прежде чем считать команду потерянной.
	AckTimeout time.Duration
}

// PullAll возвращает команды на снятие всей стороны.
//
// Аварийный путь обходит и очередь round-robin, и минимальное время жизни:
// когда рынок подошёл вплотную, присутствие в стакане перестаёт быть ценностью.
func (l *Ladder) PullAll(reason string) []Action {
	var out []Action
	for i := range l.Slots {
		if l.Slots[i].Phase == Live || l.Slots[i].Phase == Placing {
			out = append(out, Action{
				Kind:    Cancel,
				Slot:    i,
				ClOrdID: l.Slots[i].ClOrdID,
				Urgency: policy.UrgencySafety,
				Reason:  reason,
			})
		}
	}
	return out
}

// Classifier выносит решение по живой ступени.
//
// Принимает ступень целиком, а не дистанцию, потому что решение зависит
// от двух опорных цен сразу: официальный mark определяет попадание в полосу
// награды, оценочный — близость опасности (PLAN.md §4.7). Сводить это
// к одному числу — значит потерять одну из двух ролей.
type Classifier func(Slot) policy.Decision

// Plan решает, что делать со стороной прямо сейчас.
//
// desired задаёт целевые дистанции ступеней. Возвращается не более одной
// команды в штатном режиме — кроме случая опасности, где снимается всё сразу.
func (l *Ladder) Plan(
	now time.Time,
	desired []money.Bps,
	classify Classifier,
	rules Rules,
) []Action {
	// Опасность рассматривается первой и по всем ступеням сразу: пока мы
	// аккуратно переставляем одну, остальные стоят под тем же рынком.
	for i := range l.Slots {
		if l.Slots[i].Phase != Live {
			continue
		}
		if d := classify(l.Slots[i]); d.Action == policy.Pull {
			return l.PullAll(d.Reason)
		}
	}

	// Потерянные команды: биржа не ответила за отведённое время. Ступень
	// освобождается, а расхождение с действительностью устраняет сверка.
	for i := range l.Slots {
		if l.Slots[i].Phase.InFlight() && now.Sub(l.Slots[i].Since) > rules.AckTimeout {
			l.Slots[i].Phase = Empty
			l.Slots[i].ClOrdID = ""
			l.Slots[i].Since = now
		}
	}

	// Штатный режим: одна команда в полёте на сторону. Иначе round-robin
	// вырождается в одновременную перестановку всей лесенки.
	if l.InFlight() > 0 {
		return nil
	}

	return l.planOne(now, desired, classify, rules)
}

// planOne выбирает одну ступень, начиная с позиции курсора.
func (l *Ladder) planOne(
	now time.Time,
	desired []money.Bps,
	classify Classifier,
	rules Rules,
) []Action {
	n := len(l.Slots)
	for offset := range n {
		i := (l.cursor + offset) % n
		slot := l.Slots[i]

		target := desired[min(i, len(desired)-1)]

		switch slot.Phase {
		case Empty:
			l.cursor = (i + 1) % n
			return []Action{{
				Kind:     Place,
				Slot:     i,
				Distance: target,
				Urgency:  policy.UrgencyReward,
				Reason:   "filling an empty rung",
			}}

		case Live:
			decision := classify(slot)
			if decision.Action != policy.Requote {
				continue
			}
			// Свежий ордер не трогаем, пока опасность не подтвердилась:
			// перестановка стоит запроса и разрыва присутствия, а программа
			// не засчитывает ордера моложе трёх секунд.
			if decision.Urgency != policy.UrgencySafety && now.Sub(slot.Since) < rules.MinRest {
				continue
			}

			l.cursor = (i + 1) % n
			if decision.Sequence == policy.CancelFirst {
				return []Action{{
					Kind:    Cancel,
					Slot:    i,
					ClOrdID: slot.ClOrdID,
					Urgency: decision.Urgency,
					Reason:  decision.Reason,
				}}
			}
			// PlaceFirst: новый ордер выставляется на свободную ступень, если
			// она есть, иначе приходится сначала освободить текущую.
			if free, ok := l.freeSlot(); ok {
				return []Action{{
					Kind:     Place,
					Slot:     free,
					Distance: target,
					Urgency:  decision.Urgency,
					Reason:   decision.Reason,
				}}
			}
			return []Action{{
				Kind:    Cancel,
				Slot:    i,
				ClOrdID: slot.ClOrdID,
				Urgency: decision.Urgency,
				Reason:  decision.Reason + " (no free rung)",
			}}
		}
	}
	return nil
}

func (l *Ladder) freeSlot() (int, bool) {
	for i, s := range l.Slots {
		if s.Phase == Empty {
			return i, true
		}
	}
	return 0, false
}

// --- Переходы состояний ---

// OnPlaceSent отмечает отправленную команду размещения.
func (l *Ladder) OnPlaceSent(slot int, id trading.ClOrdID, price money.Price, qty money.Qty, now time.Time) {
	l.Slots[slot] = Slot{
		Index:   slot,
		Phase:   Placing,
		ClOrdID: id,
		Price:   price,
		Qty:     qty,
		Since:   now,
	}
}

// OnCancelSent отмечает отправленную команду отмены.
func (l *Ladder) OnCancelSent(slot int, now time.Time) {
	if l.Slots[slot].Phase == Live || l.Slots[slot].Phase == Placing {
		l.Slots[slot].Phase = Cancelling
		l.Slots[slot].Since = now
	}
}

// OnOrderUpdate применяет сообщение биржи о судьбе ордера.
//
// Сопоставление идёт по клиентскому идентификатору: он известен до отправки
// и переживает и ретраи, и переподключения.
func (l *Ladder) OnOrderUpdate(o trading.Order, now time.Time) (slot int, matched bool) {
	for i := range l.Slots {
		if l.Slots[i].ClOrdID != o.ClOrdID || o.ClOrdID == "" {
			continue
		}

		switch {
		case o.Status.IsTerminal():
			l.Slots[i] = Slot{Index: i, Phase: Empty, Since: now}
		case o.Status == trading.StatusOpen && l.Slots[i].Phase == Placing:
			l.Slots[i].Phase = Live
			l.Slots[i].Since = now
		}
		return i, true
	}
	return 0, false
}

// Orders возвращает клиентские идентификаторы всех неоконченных ордеров.
func (l *Ladder) Orders() []trading.ClOrdID {
	var out []trading.ClOrdID
	for _, s := range l.Slots {
		if s.ClOrdID != "" && s.Phase != Empty {
			out = append(out, s.ClOrdID)
		}
	}
	return out
}
