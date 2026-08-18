package quotes

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nd/mm-standx/internal/domain/market"
	"github.com/nd/mm-standx/internal/domain/money"
	"github.com/nd/mm-standx/internal/domain/trading"
)

const testLockout = 3 * time.Second

func guardedRules() Rules {
	r := rules()
	r.RefillLockout = testLockout
	return r
}

// vacateBySafety walks the whole side through the danger path exactly as the
// engine does: plan sees danger, the cancels go out, the exchange confirms.
func vacateBySafety(t *testing.T, l *Ladder, at time.Time, r Rules) {
	t.Helper()

	// 2.0 bps sits below the panic threshold and above the pull threshold,
	// which is the "price approaching" branch — the same decision that emptied
	// the book on 18 August 2026 at 13:30:17.
	actions := l.Plan(at, desired(), atDistance(2.0), r)
	require.Len(t, actions, 3, "danger pulls the whole side at once")

	for _, a := range actions {
		require.Equal(t, Cancel, a.Kind)
		l.OnCancelSent(a.Slot, at)
	}
	for _, a := range actions {
		_, matched := l.OnOrderUpdate(
			trading.Order{ClOrdID: a.ClOrdID, Status: trading.StatusCanceled}, at)
		require.True(t, matched)
	}
	for i := range l.Slots {
		require.Equal(t, Empty, l.Slots[i].Phase)
	}
}

// Воспроизведение залива 18 августа 2026, 13:30 UTC.
//
// Защита сняла всю сторону в 13:30:17.250, заполнение пустых ступеней вернуло
// её обратно через 1.95 секунды, и в 13:30:22.486 стакан снёс все три ордера.
// Запрет обязан пережить и подтверждение отмены, и успокоившийся классификатор:
// спокойным рынок выглядит именно потому, что мы из него вышли.
func TestSafetyPullLocksRungsAgainstImmediateRefill(t *testing.T) {
	l := fullLadder(t)
	r := guardedRules()

	vacateBySafety(t, l, epoch, r)

	// 1.95 секунды — ровно тот интервал, на котором бот вернул ордера в бою.
	require.Nil(t, l.Plan(epoch.Add(1950*time.Millisecond), desired(), atDistance(7.5), r),
		"the rung the market just chased us off must not be refilled 1.95s later")

	// Полутора секунд сравнимого оператора здесь тоже не хватило бы.
	require.Nil(t, l.Plan(epoch.Add(2*time.Second), desired(), atDistance(7.5), r),
		"a 1.5s lockout would not have covered the measured 1.95s")

	actions := l.Plan(epoch.Add(testLockout+time.Millisecond), desired(), atDistance(7.5), r)
	require.Len(t, actions, 1, "once the lockout expires the side refills as usual")
	require.Equal(t, Place, actions[0].Kind)
}

// Запрет ставится в момент решения об опасности, а подтверждение отмены
// приходит позже и обнуляет структуру слота. Если оно сотрёт запрет, вся
// защита сведётся к нулю — именно этот порядок событий и происходит в бою.
func TestRefillLockoutSurvivesCancelConfirmation(t *testing.T) {
	l := fullLadder(t)
	r := guardedRules()

	actions := l.Plan(epoch, desired(), atDistance(2.0), r)
	require.Len(t, actions, 3)
	for _, a := range actions {
		require.False(t, l.Slots[a.Slot].RefillNotBefore.IsZero(),
			"the hold is stamped when danger is decided, not when the cancel lands")
	}

	for _, a := range actions {
		l.OnCancelSent(a.Slot, epoch)
		_, matched := l.OnOrderUpdate(
			trading.Order{ClOrdID: a.ClOrdID, Status: trading.StatusCanceled}, epoch.Add(400*time.Millisecond))
		require.True(t, matched)
		require.Equal(t, epoch.Add(testLockout), l.Slots[a.Slot].RefillNotBefore,
			"confirming the cancel must not erase the hold it was ordered under")
	}
}

// Рядовая перестановка по награде присутствием не платит.
//
// Ступень, выпавшая за полосу, снимается ради зачёта, а не от опасности:
// запирать её значит терять вес там, где риска нет. Таких снятий 18 августа
// было 2752 против 1098 защитных — перепутать эти два случая дороже, чем
// не иметь запрета вовсе.
func TestRewardRequoteLeavesTheRungFree(t *testing.T) {
	l := fullLadder(t)
	r := guardedRules()

	// 9.6 bps — за MaxKeepBps, но внутри полосы: "drifted outward", PlaceFirst.
	actions := l.Plan(epoch.Add(r.MinRest+time.Second), desired(), atDistance(9.6), r)
	require.Len(t, actions, 1)
	require.Equal(t, Cancel, actions[0].Kind, "no free rung, so the slot is freed first")

	for i := range l.Slots {
		require.True(t, l.Slots[i].RefillNotBefore.IsZero(),
			"a reward-urgency requote is not danger and must not lock the rung")
	}
}

// Ноль возвращает прежнее поведение целиком. Аварийный выключатель обязан
// быть проверен, иначе он не выключатель, а обещание.
func TestZeroLockoutRestoresImmediateRefill(t *testing.T) {
	l := fullLadder(t)
	r := rules() // RefillLockout == 0

	vacateBySafety(t, l, epoch, r)

	actions := l.Plan(epoch, desired(), atDistance(7.5), r)
	require.Len(t, actions, 1, "with the lockout disabled the empty rung is filled at once")
	require.Equal(t, Place, actions[0].Kind)
	require.Equal(t, "filling an empty rung", actions[0].Reason)
}

// Запрет на одну ступень не имеет права останавливать сторону целиком.
//
// Сторона считается по слабейшей из двух, поэтому пустая сторона обнуляет
// зачёт. Запрет, распространившийся с одной ступени на всю лесенку, лечил бы
// залив ценой того самого часа, ради которого всё и делается.
func TestLockoutOnOneRungDoesNotStallTheOthers(t *testing.T) {
	l := New(market.Buy, 3)
	r := guardedRules()

	// Только средняя ступень живёт и только её сгоняет рынок.
	place(t, l, 1, trading.ClOrdID("b"), epoch)
	l.holdRefill(1, epoch, testLockout)
	l.Slots[1] = Slot{Index: 1, Phase: Empty, Since: epoch, RefillNotBefore: l.Slots[1].RefillNotBefore}

	seen := map[int]bool{}
	for at := epoch; at.Before(epoch.Add(2 * time.Second)); at = at.Add(300 * time.Millisecond) {
		actions := l.Plan(at, desired(), atDistance(7.5), r)
		if len(actions) == 0 {
			continue
		}
		require.Equal(t, Place, actions[0].Kind)
		seen[actions[0].Slot] = true
		l.OnPlaceSent(actions[0].Slot, trading.ClOrdID("x"), money.MustParsePrice("100"), money.MustParseQty("1"), at)
		_, matched := l.OnOrderUpdate(trading.Order{ClOrdID: "x", Status: trading.StatusOpen}, at)
		require.True(t, matched)
	}

	require.True(t, seen[0], "an unlocked rung must still be filled")
	require.True(t, seen[2], "an unlocked rung must still be filled")
	require.False(t, seen[1], "the locked rung stays empty until its hold expires")
}
