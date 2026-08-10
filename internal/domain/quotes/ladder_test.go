package quotes

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nd/mm-standx/internal/domain/market"
	"github.com/nd/mm-standx/internal/domain/money"
	"github.com/nd/mm-standx/internal/domain/policy"
	"github.com/nd/mm-standx/internal/domain/trading"
)

var epoch = time.Unix(1_754_897_028, 0).UTC()

func rules() Rules { return Rules{MinRest: 3 * time.Second, AckTimeout: 2 * time.Second} }

func thresholds() policy.Thresholds {
	return policy.Thresholds{
		PullBps: 1.0, PanicBps: 2.5, MinKeepBps: 5.0,
		TargetBps: 7.5, MaxKeepBps: 9.2, BandBps: 10.0,
	}
}

func desired() []money.Bps { return []money.Bps{6.5, 7.5, 8.5} }

// atDistance builds a classifier that sees every rung at a fixed distance.
func atDistance(d money.Bps) Classifier {
	th := thresholds()
	return func(Slot) policy.Decision { return th.Classify(d) }
}

// place walks a rung through the full lifecycle to Live.
func place(t *testing.T, l *Ladder, slot int, id trading.ClOrdID, at time.Time) {
	t.Helper()
	l.OnPlaceSent(slot, id, money.MustParsePrice("100"), money.MustParseQty("1"), at)
	_, matched := l.OnOrderUpdate(trading.Order{ClOrdID: id, Status: trading.StatusOpen}, at)
	require.True(t, matched)
	require.Equal(t, Live, l.Slots[slot].Phase)
}

func fullLadder(t *testing.T) *Ladder {
	t.Helper()
	l := New(market.Buy, 3)
	for i := range 3 {
		place(t, l, i, trading.ClOrdID(string(rune('a'+i))), epoch)
	}
	return l
}

func TestEmptyLadderFillsRungsOneAtATime(t *testing.T) {
	l := New(market.Buy, 3)

	actions := l.Plan(epoch, desired(), atDistance(7.5), rules())
	require.Len(t, actions, 1, "only one command may be in flight per side")
	require.Equal(t, Place, actions[0].Kind)
	require.Equal(t, money.Bps(6.5), actions[0].Distance)
}

// This is the invariant the whole ladder exists to protect: while one rung is
// being moved the others keep the side populated, so the reward sample never
// collapses to zero.
func TestOnlyOneCommandInFlightPerSide(t *testing.T) {
	l := New(market.Buy, 3)

	actions := l.Plan(epoch, desired(), atDistance(7.5), rules())
	require.Len(t, actions, 1)
	l.OnPlaceSent(actions[0].Slot, "a", money.MustParsePrice("100"), money.MustParseQty("1"), epoch)

	require.Empty(t, l.Plan(epoch, desired(), atDistance(7.5), rules()),
		"a second command must wait for the first to settle")
}

func TestQuietLadderProducesNoCommands(t *testing.T) {
	l := fullLadder(t)

	later := epoch.Add(time.Minute)
	require.Empty(t, l.Plan(later, desired(), atDistance(7.5), rules()),
		"rungs inside the dead band must be left alone")
	require.True(t, l.Populated())
}

// Danger is evaluated across every rung before anything else: while we politely
// requote one, the others rest under the same market.
func TestDangerPullsTheEntireSide(t *testing.T) {
	l := fullLadder(t)

	actions := l.Plan(epoch.Add(time.Minute), desired(), atDistance(0.5), rules())
	require.Len(t, actions, 3, "the whole side must come down at once")
	for _, a := range actions {
		require.Equal(t, Cancel, a.Kind)
		require.Equal(t, policy.UrgencySafety, a.Urgency)
	}
}

// Panic starts one observed jump before the market reaches the quote. The
// entire side must leave there, not one rung at a time after the requote
// throttle, because a single sweep reaches every rung in the ladder.
func TestPanicPullsTheEntireSideBeforeTheMarketReachesTheQuote(t *testing.T) {
	l := fullLadder(t)

	actions := l.Plan(epoch.Add(time.Second), desired(), atDistance(2.0), rules())
	require.Len(t, actions, 3, "panic must pull the whole side before the quote is touched")
	for _, a := range actions {
		require.Equal(t, Cancel, a.Kind)
		require.Equal(t, policy.UrgencySafety, a.Urgency)
		require.Equal(t, "price approaching", a.Reason)
	}
}

// A pull must ignore both the round-robin pacing and the minimum rest time:
// once the market is on top of the quote, presence stops being valuable.
func TestPullIgnoresMinimumRestTime(t *testing.T) {
	l := fullLadder(t)

	actions := l.Plan(epoch, desired(), atDistance(0.5), rules())
	require.Len(t, actions, 3, "a fresh order must still be pulled when in danger")
}

func TestFreshOrdersAreLeftAloneUnlessThreatened(t *testing.T) {
	l := fullLadder(t)

	// Drifted out of the band: pure reward loss, so the rest rule applies.
	require.Empty(t, l.Plan(epoch.Add(time.Second), desired(), atDistance(12), rules()),
		"an order younger than minRest must not be requoted for reward alone")

	require.NotEmpty(t, l.Plan(epoch.Add(4*time.Second), desired(), atDistance(12), rules()),
		"after minRest the same drift must trigger a requote")
}

// Approaching price outranks the rest rule and round-robin pacing: the cost of
// waiting is a fill across the same sweep.
func TestSafetyPullBypassesMinimumRest(t *testing.T) {
	l := fullLadder(t)

	actions := l.Plan(epoch.Add(time.Second), desired(), atDistance(2.0), rules())
	require.Len(t, actions, 3)
	for _, a := range actions {
		require.Equal(t, Cancel, a.Kind, "approaching price must cancel first")
		require.Equal(t, policy.UrgencySafety, a.Urgency)
	}
}

// Outward drift carries no execution risk, so the replacement goes up before
// the stale order comes down and the side never thins out.
func TestOutwardDriftPlacesBeforeCancelling(t *testing.T) {
	l := New(market.Buy, 3)
	place(t, l, 0, "a", epoch)
	place(t, l, 1, "b", epoch)
	// Rung 2 stays empty, giving the replacement somewhere to go.

	actions := l.Plan(epoch.Add(4*time.Second), desired(), atDistance(12), rules())
	require.Len(t, actions, 1)
	require.Equal(t, Place, actions[0].Kind, "a free rung must be used before cancelling")
	require.Equal(t, 2, actions[0].Slot)
}

// With every rung occupied there is nowhere to place first, so the ladder falls
// back to freeing a rung. The remaining rungs keep the side alive meanwhile.
func TestFullLadderCancelsWhenNoRungIsFree(t *testing.T) {
	l := fullLadder(t)

	actions := l.Plan(epoch.Add(4*time.Second), desired(), atDistance(12), rules())
	require.Len(t, actions, 1)
	require.Equal(t, Cancel, actions[0].Kind)
	require.Contains(t, actions[0].Reason, "no free rung")

	// Plan only states the intent; the caller applies it once the command is
	// actually sent.
	l.OnCancelSent(actions[0].Slot, epoch.Add(4*time.Second))
	require.Equal(t, 2, l.LiveCount(), "the other rungs must stay in the book")
}

// Without a cursor the ladder would keep choosing the same rung and the others
// would never be refreshed.
func TestCursorRotatesAcrossRungs(t *testing.T) {
	l := New(market.Buy, 3)

	seen := map[int]bool{}
	for range 3 {
		actions := l.Plan(epoch, desired(), atDistance(7.5), rules())
		require.Len(t, actions, 1)
		seen[actions[0].Slot] = true

		l.OnPlaceSent(actions[0].Slot, trading.ClOrdID(string(rune('a'+actions[0].Slot))),
			money.MustParsePrice("100"), money.MustParseQty("1"), epoch)
		l.OnOrderUpdate(trading.Order{
			ClOrdID: trading.ClOrdID(string(rune('a' + actions[0].Slot))),
			Status:  trading.StatusOpen,
		}, epoch)
	}
	require.Len(t, seen, 3, "every rung must get its turn")
}

// A command the exchange never answered would otherwise block the side forever.
func TestLostCommandFreesTheRung(t *testing.T) {
	l := New(market.Buy, 3)
	l.OnPlaceSent(0, "a", money.MustParsePrice("100"), money.MustParseQty("1"), epoch)

	require.Empty(t, l.Plan(epoch.Add(time.Second), desired(), atDistance(7.5), rules()),
		"within the ack timeout the side stays blocked")

	actions := l.Plan(epoch.Add(3*time.Second), desired(), atDistance(7.5), rules())
	require.NotEmpty(t, actions, "after the ack timeout the rung must be reusable")
	require.Equal(t, Empty, l.Slots[0].Phase)
}

func TestTerminalStatusEmptiesTheRung(t *testing.T) {
	for _, status := range []trading.OrderStatus{
		trading.StatusFilled, trading.StatusCanceled, trading.StatusRejected,
	} {
		l := fullLadder(t)
		_, matched := l.OnOrderUpdate(trading.Order{ClOrdID: "a", Status: status}, epoch)

		require.True(t, matched)
		require.Equal(t, Empty, l.Slots[0].Phase, "status %s must free the rung", status)
		require.Equal(t, 2, l.LiveCount())
	}
}

func TestUnknownOrderIsIgnored(t *testing.T) {
	l := fullLadder(t)

	_, matched := l.OnOrderUpdate(trading.Order{ClOrdID: "unknown", Status: trading.StatusFilled}, epoch)
	require.False(t, matched)
	require.Equal(t, 3, l.LiveCount())
}

func TestOrdersListsUnfinishedIdentifiers(t *testing.T) {
	l := fullLadder(t)
	require.ElementsMatch(t, []trading.ClOrdID{"a", "b", "c"}, l.Orders())

	l.OnOrderUpdate(trading.Order{ClOrdID: "b", Status: trading.StatusCanceled}, epoch)
	require.ElementsMatch(t, []trading.ClOrdID{"a", "c"}, l.Orders())
}

// Сужение лесенки обязано снимать ступени, которых в новой геометрии нет:
// иначе ордер остаётся стоять по дистанции, которую расчёт уже не считает своей.
func TestNarrowedLadderCancelsTheRungsItNoLongerHas(t *testing.T) {
	l := New(market.Buy, 3)
	now := time.Now().UTC()
	rules := Rules{MinRest: 0, AckTimeout: 2 * time.Second}

	for i, id := range []trading.ClOrdID{"a", "b", "c"} {
		l.OnPlaceSent(i, id, money.MustParsePrice("4300"), money.MustParseQty("0.01"), now)
		l.OnOrderUpdate(trading.Order{ClOrdID: id, Status: trading.StatusOpen}, now)
	}

	hold := func(Slot) policy.Decision {
		return policy.Decision{Action: policy.Hold, Reason: "within dead band"}
	}

	// Лесенка сузилась до одной ступени: две оставшиеся обязаны уйти.
	var cancelled []trading.ClOrdID
	for range 4 {
		actions := l.Plan(now, []money.Bps{7.0}, hold, rules)
		if len(actions) == 0 {
			break
		}
		for _, a := range actions {
			require.Equal(t, Cancel, a.Kind)
			require.Contains(t, a.Reason, "narrowed")
			cancelled = append(cancelled, a.ClOrdID)
			// Одна команда в полёте на сторону: пока биржа не подтвердила
			// снятие, следующая ступень не планируется.
			l.OnCancelSent(a.Slot, now)
			l.OnOrderUpdate(trading.Order{ClOrdID: a.ClOrdID, Status: trading.StatusCanceled}, now)
		}
	}
	require.ElementsMatch(t, []trading.ClOrdID{"b", "c"}, cancelled,
		"slot 0 stays: it is the only one the narrowed geometry still has")
}
