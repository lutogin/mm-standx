package account

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nd/mm-standx/internal/domain/market"
	"github.com/nd/mm-standx/internal/domain/trading"
	"github.com/nd/mm-standx/internal/ports"
)

func TestLeverageIsSetAndVerified(t *testing.T) {
	cfg := &fakeConfigurer{leverage: 4}
	log, out := recordingLogger()

	ApplyLeverage(context.Background(), cfg, map[market.Symbol]int{"XAU-USD": 40}, log)

	require.Equal(t, 40, cfg.leverage)
	require.Equal(t, 1, cfg.writes)
	require.Equal(t, 2, cfg.reads, "read, then read again after the write: the result is verified, not assumed")
	require.Contains(t, out.String(), "account leverage changed")
}

// Совпадающее плечо не трогается: лишний вызов на старте — это лишний способ
// сломать то, что уже верно.
func TestMatchingLeverageIsLeftAlone(t *testing.T) {
	cfg := &fakeConfigurer{leverage: 40}
	log, out := recordingLogger()

	ApplyLeverage(context.Background(), cfg, map[market.Symbol]int{"XAU-USD": 40}, log)

	require.Zero(t, cfg.writes)
	require.Contains(t, out.String(), "already matches")
}

// Самый опасный исход: биржа приняла запрос и не применила его. Ответ при этом
// успешный, и без перечитывания расчёт размера исходил бы из плеча, которого
// на счёте нет.
func TestAcceptedButNotAppliedIsReportedAsAnError(t *testing.T) {
	cfg := &fakeConfigurer{leverage: 4, ignoreWrites: true}
	log, out := recordingLogger()

	ApplyLeverage(context.Background(), cfg, map[market.Symbol]int{"XAU-USD": 40}, log)

	require.Equal(t, 4, cfg.leverage)
	require.Contains(t, out.String(), "accepted the leverage and did not apply it")
	require.Contains(t, out.String(), "level=ERROR")
}

// Прежнее плечо — это меньший размер, а не опасность. Падать из-за него значило
// бы менять работающего бота на неработающего.
func TestFailureDoesNotStopTheStart(t *testing.T) {
	cfg := &fakeConfigurer{leverage: 4, writeErr: errors.New("exchange is busy")}
	log, out := recordingLogger()

	require.NotPanics(t, func() {
		ApplyLeverage(context.Background(), cfg, map[market.Symbol]int{"XAU-USD": 40}, log)
	})
	require.Contains(t, out.String(), "size stays limited by the old one")
}

func recordingLogger() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewTextHandler(&buf, nil)), &buf
}

type fakeConfigurer struct {
	leverage     int
	reads        int
	writes       int
	writeErr     error
	ignoreWrites bool
}

func (f *fakeConfigurer) PositionConfig(context.Context, market.Symbol) (trading.PositionConfig, error) {
	f.reads++
	return trading.PositionConfig{Symbol: "XAU-USD", Leverage: f.leverage, MarginMode: trading.Cross}, nil
}

func (f *fakeConfigurer) SetLeverage(_ context.Context, _ market.Symbol, leverage int) error {
	f.writes++
	if f.writeErr != nil {
		return f.writeErr
	}
	if !f.ignoreWrites {
		f.leverage = leverage
	}
	return nil
}

var _ ports.PositionConfigurer = (*fakeConfigurer)(nil)
