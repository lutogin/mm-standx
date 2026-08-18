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
	// RefillNotBefore — самый ранний момент, когда опустевшую ступень
	// разрешено занять снова.
	//
	// СТУПЕНЬ ПУСТА НЕ ПРОСТО ТАК. Ветка Empty в planOne десять месяцев
	// заполняла её безусловно, потому что «пусто» выглядит как состояние,
	// а не как событие: у Live есть MinRest, у Empty не было ничего. Разница
	// между «ступень свободна» и «мы с этой ступени только что убежали»
	// в фазе не выражена, и заполнение возвращало ордер точно туда, откуда
	// его секунду назад сняли по опасности — по цене, посчитанной от такта
	// mark, который движение уже обогнало.
	//
	// Замер 18 августа 2026, 13:30 UTC. Danger снял все три бидовые ступени
	// в 13:30:17.250 с причиной "price approaching". planOne вернул их
	// в 13:30:19.201, 19.799 и 21.319 с причиной "filling an empty rung",
	// все три — по одной цене 4390.5 после зажима post-only буфером. В
	// 13:30:22.486-24.307 их снесло целиком: шесть исполнений за 1.8 секунды,
	// 5.51 USD убытка и обвал присутствия, стоивший часу 1.18 Maker Hour
	// (StandX заплатил 0.82 вместо достижимых 2.00).
	//
	// Поле ставится только там, где ступень освободила защита, и переживает
	// переход в Empty: сброс структуры слота в OnOrderUpdate сохраняет его
	// намеренно, иначе подтверждение отмены стирало бы ровно тот запрет,
	// ради которого он и ставился.
	RefillNotBefore time.Time
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
	// RefillLockout — сколько ступень остаётся пустой после того, как её
	// освободила защита. См. Slot.RefillNotBefore.
	//
	// Цена запрета считается, а не назначается. Защитных снятий 18 августа
	// было 1098 ("price approaching" 1084, "inside pull threshold" 14), каждое
	// запирает ОДНУ ступень: 1098 x запрет ступенно-секунд против 6 ступеней
	// x 54000 секунд работы, то есть при трёх секундах теряется 1% времени
	// ступени. Против 1.18 Maker Hour, которые один незапертый возврат стоил
	// в тот же день.
	//
	// Ноль возвращает прежнее безусловное заполнение.
	RefillLockout time.Duration
}

// PullAll возвращает команды на снятие всей стороны.
//
// Аварийный путь обходит и очередь round-robin, и минимальное время жизни:
// когда рынок подошёл вплотную, присутствие в стакане перестаёт быть ценностью.
//
// Запрет на возврат ставится здесь, а не в момент подтверждения отмены, потому
// что решение об опасности принято именно здесь и только здесь известно. Ставится
// он и на ступени в фазе Placing: команда размещения в полёте — это ордер,
// который вот-вот окажется в стакане по цене, посчитанной до опасности.
func (l *Ladder) PullAll(now time.Time, lockout time.Duration, reason string) []Action {
	var out []Action
	for i := range l.Slots {
		if l.Slots[i].Phase == Live || l.Slots[i].Phase == Placing {
			l.holdRefill(i, now, lockout)
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

// holdRefill запирает ступень от повторного заполнения.
//
// Запрет только продлевается: два защитных снятия подряд не имеют права
// сократить срок, назначенный первым.
func (l *Ladder) holdRefill(slot int, now time.Time, lockout time.Duration) {
	if lockout <= 0 {
		return
	}
	if until := now.Add(lockout); until.After(l.Slots[slot].RefillNotBefore) {
		l.Slots[slot].RefillNotBefore = until
	}
}

// Classifier выносит решение по живой ступени.
//
// Принимает ступень целиком, а не дистанцию, потому что решение зависит
// от двух опорных цен сразу: официальный mark определяет попадание в полосу
// награды, оценочный — близость опасности (PLAN.md §4.7). Сводить это
// к одному числу — значит потерять одну из двух ролей.
type Classifier func(Slot) policy.Decision

// Danger returns commands for pulling the side when at least one live rung is
// classified as requiring cancel-first safety handling.
//
// Проверка вынесена из Plan отдельным методом, потому что вызывается раньше
// ограничения темпа перестановок: кулдаун существует, чтобы не жечь бюджет
// запросов, и задерживать им аварийный съём — значит применять экономию там,
// где она стоит залива.
//
// Panic is deliberately side-wide. Moving one rung at a time is an uptime
// optimization, but once the market is within one measured jump of any rung,
// the remaining rungs are exposed to the same move. Waiting for the pull
// threshold means waiting until the market has already reached the quote.
func (l *Ladder) Danger(now time.Time, classify Classifier, lockout time.Duration) []Action {
	for i := range l.Slots {
		if l.Slots[i].Phase != Live {
			continue
		}
		d := classify(l.Slots[i])
		if d.Action != policy.Hold &&
			d.Urgency == policy.UrgencySafety &&
			d.Sequence == policy.CancelFirst {
			return l.PullAll(now, lockout, d.Reason)
		}
	}
	return nil
}

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
	if danger := l.Danger(now, classify, rules.RefillLockout); len(danger) > 0 {
		return danger
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

		// Ступени сверх текущей геометрии не существует: лесенка сужена, чтобы
		// поместиться в полосу награды. Пустая остаётся пустой, живая снимается —
		// иначе она стоит по дистанции, которую расчёт уже не считает своей.
		if i >= len(desired) {
			if slot.Phase != Live {
				continue
			}
			l.cursor = (i + 1) % n
			return []Action{{
				Kind:    Cancel,
				Slot:    i,
				ClOrdID: slot.ClOrdID,
				Urgency: policy.UrgencyReward,
				Reason:  "ladder narrowed to fit the reward band",
			}}
		}
		target := desired[i]

		switch slot.Phase {
		case Empty:
			// Ступень освободила защита, и срок запрета ещё не вышел.
			// Курсор не двигаем: очередь round-robin принадлежит сделанному
			// действию, а запрет — это отказ действовать. Следующая ступень
			// в том же проходе рассматривается как обычно, поэтому запрет
			// на одну ступень не останавливает сторону целиком.
			if now.Before(slot.RefillNotBefore) {
				continue
			}
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
				// CancelFirst выбирается только когда рынок идёт на нас
				// (policy.Thresholds.Classify), то есть это то же событие,
				// что и в PullAll, только по одной ступени. Запрет обязан
				// стоять здесь тоже, иначе round-robin вернёт ордер туда же
				// на следующем проходе.
				if decision.Urgency == policy.UrgencySafety {
					l.holdRefill(i, now, rules.RefillLockout)
				}
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
			// Запрет переживает сброс слота намеренно. Он ставится в момент
			// решения об опасности, а подтверждение отмены приходит после —
			// и, обнуляя структуру целиком, стирало бы ровно тот запрет,
			// ради которого решение принималось.
			//
			// Своего запрета этот путь не назначает, и для исполнения тоже:
			// после залива сторона снимается целиком до закрытия позиции
			// (PLAN.md §4.9, инвариант 4), а идёт это снятие через PullAll,
			// который запрет уже поставил. Назначать здесь второй значило бы
			// продлевать его на срок, никем не решённый.
			//
			// Отмена и отказ биржи запрет тем более не назначают: рядовая
			// перестановка по награде и отказ «balance not enough» опасностью
			// не являются, и платить за них присутствием не за что.
			hold := l.Slots[i].RefillNotBefore
			l.Slots[i] = Slot{Index: i, Phase: Empty, Since: now, RefillNotBefore: hold}
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
