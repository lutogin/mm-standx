package engine

import (
	"bytes"
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"

	"github.com/nd/mm-standx/internal/domain/market"
	"github.com/nd/mm-standx/internal/domain/trading"
	"github.com/nd/mm-standx/internal/events"
)

// Каждый выход из evaluate обязан назвать себя.
//
// Тесты этого файла закрывают отказ, стоивший пяти заходов на диагностику: бот
// работал около часа, ловил первые заливы и замолкал навсегда. В логе не
// оставалось ни строки, метрика показывала ноль — а ноль одинаково означает
// «воздерживаюсь по делу» и «сломан», поэтому отличить одно от другого можно
// было только по косвенным уликам.

// An actor that stopped quoting has to say so on every tick. Silence is the one
// state an operator cannot act on.
func TestHaltedActorKeepsExplainingItself(t *testing.T) {
	ctx := context.Background()
	e, _, _ := dangerEngine(t)

	var buf bytes.Buffer
	e.deps.Log = slog.New(slog.NewTextHandler(&buf, nil))

	e.halted = true
	e.haltReason = "daily loss 12.00 USD reached the 10.00 limit"
	e.evaluate(ctx, true)

	require.Contains(t, buf.String(), "engine status")
	require.Contains(t, buf.String(), "daily loss 12.00 USD")
}

// Остановка была защёлкой в одну сторону: поднять её было нечем, и переживший
// смену суток UTC процесс продолжал молчать с уже пустым основанием.
func TestClearedKillSwitchLetsTheActorQuoteAgain(t *testing.T) {
	ctx := context.Background()
	e, gw, clk := dangerEngine(t)
	e.available = decimal.NewFromInt(1000)

	e.handle(ctx, events.KillSwitchTripped{Reason: "daily loss", At: clk.now})
	e.evaluate(ctx, true)
	require.Empty(t, gw.placed, "a halted actor places nothing")

	e.handle(ctx, events.KillSwitchCleared{At: clk.now})

	require.False(t, e.halted)
	require.NotEmpty(t, gw.placed, "clearing the switch has to bring the actor back")
}

// Пауза держится сроком, а не флагом: сообщение о снятии может не прийти,
// и тогда актор молчал бы до конца запуска.
func TestRiskPauseIsHonouredAndExpiresWithoutAnEvent(t *testing.T) {
	ctx := context.Background()
	e, gw, clk := dangerEngine(t)
	e.available = decimal.NewFromInt(1000)

	liveRung(e, market.Buy, "bid-1", "4273", clk.now)
	liveRung(e, market.Sell, "ask-1", "4279", clk.now)

	until := clk.now.Add(10 * time.Minute)
	e.handle(ctx, events.QuotingPaused{
		Until:  until,
		Reason: "6 fills in the last hour reached the 6 limit",
		At:     clk.now,
	})

	require.ElementsMatch(t, []trading.ClOrdID{"bid-1", "ask-1"}, gw.cancelled,
		"both sides come off when the guard pauses quoting")

	gw.placed = nil
	e.evaluate(ctx, true)
	require.Empty(t, gw.placed, "nothing goes up while the pause is in force")

	clk.now = until.Add(time.Second)
	primeVolatility(t, e, clk)
	e.evaluate(ctx, true)
	require.NotEmpty(t, gw.placed, "the deadline has to release the actor on its own")
}

// The pause is reported for the same reason the halt is: an operator reading
// the log must never have to infer why the book is empty.
func TestRiskPauseIsReportedInTheStatusLog(t *testing.T) {
	ctx := context.Background()
	e, _, clk := dangerEngine(t)

	var buf bytes.Buffer
	e.deps.Log = slog.New(slog.NewTextHandler(&buf, nil))

	e.riskPauseUntil = clk.now.Add(10 * time.Minute)
	e.riskPauseReason = "6 fills in the last hour reached the 6 limit"
	e.evaluate(ctx, true)

	require.Contains(t, buf.String(), "engine status")
	require.Contains(t, buf.String(), "6 fills in the last hour")
}

// Пауза после залива длится секунды, но и она обязана быть видна: раньше это
// был молчаливый return, неотличимый от зависшего актора.
func TestPostFillCooldownIsReportedInTheStatusLog(t *testing.T) {
	ctx := context.Background()
	e, _, clk := dangerEngine(t)

	var buf bytes.Buffer
	e.deps.Log = slog.New(slog.NewTextHandler(&buf, nil))

	e.cooldownUntil = clk.now.Add(3 * time.Second)
	e.evaluate(ctx, true)

	require.Contains(t, buf.String(), "engine status")
	require.Contains(t, buf.String(), "post-fill cooldown")
}

// Открытая экспозиция снимает котировки — и объясняет это. Молчаливый выход
// здесь был самым коварным: залипший pendingFill выглядел как мёртвый процесс.
func TestOpenExposureIsReportedInTheStatusLog(t *testing.T) {
	ctx := context.Background()
	e, _, clk := dangerEngine(t)

	var buf bytes.Buffer
	e.deps.Log = slog.New(slog.NewTextHandler(&buf, nil))

	e.flattenRetryAt = clk.now.Add(2 * time.Second)
	e.evaluate(ctx, true)

	require.Contains(t, buf.String(), "engine status")
	require.Contains(t, buf.String(), "no quoting while exposure is open")
}

// Причина, названная ровным тоном INFO раз в пятнадцать секунд, теряется
// в потоке. Затянувшееся молчание обязано подниматься в тревогу вместе с полным
// снимком управляющего состояния — иначе следующий разбор снова начинается
// с восстановления невидимых полей по косвенным уликам.
func TestProlongedSilenceEscalatesWithTheFullState(t *testing.T) {
	ctx := context.Background()
	e, _, clk := dangerEngine(t)

	var buf bytes.Buffer
	e.deps.Log = slog.New(slog.NewTextHandler(&buf, nil))

	e.halted = true
	e.haltReason = "daily loss"
	e.evaluate(ctx, true)
	require.NotContains(t, buf.String(), "no quoting for a long stretch",
		"a short abstention is not an incident")

	clk.advance(stallThreshold + time.Second)
	e.evaluate(ctx, true)

	out := buf.String()
	require.Contains(t, out, "no quoting for a long stretch")
	// Именно те поля, отсутствие которых стоило разбора: они не видны ни
	// в метриках, ни в обычной сводке.
	for _, field := range []string{
		"halted=true", "pending_fill=", "flatten_retry_at=", "cooldown_until=",
		"risk_pause_until=", "position=", "book_readable=", "estimate_ready=",
	} {
		require.Contains(t, out, field)
	}

	// Повтор прорежен: тревога не должна вытеснять из лога всё остальное.
	buf.Reset()
	clk.advance(time.Minute)
	e.evaluate(ctx, true)
	require.NotContains(t, buf.String(), "no quoting for a long stretch")
}

// Возобновление котирования снимает счётчик: иначе первая же пауза за смену
// объявляла бы инцидентом любую следующую.
func TestResumedQuotingClearsTheStallCounter(t *testing.T) {
	ctx := context.Background()
	e, _, clk := dangerEngine(t)
	e.available = decimal.NewFromInt(1000)

	e.halted = true
	e.evaluate(ctx, true)
	require.False(t, e.blockedSince.IsZero())

	e.halted = false
	clk.advance(time.Second)
	e.evaluate(ctx, true)
	require.True(t, e.blockedSince.IsZero(), "a quoting actor is not stalled")
}
