package wsapi

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nd/mm-standx/internal/domain/trading"
	"github.com/nd/mm-standx/internal/events"
	"github.com/nd/mm-standx/internal/ports"
)

type pipelineSender struct {
	frames []request
	onSend func()
}

func (s *pipelineSender) Send(context.Context, []byte) error { return nil }

func (s *pipelineSender) SendJSON(_ context.Context, value any) error {
	s.frames = append(s.frames, value.(request))
	if s.onSend != nil {
		s.onSend()
	}
	return nil
}

type permitAll struct{}

func (permitAll) Acquire(context.Context, ports.Priority) error { return nil }
func (permitAll) Available() float64                            { return 100 }

type fixedSigner struct{}

func (fixedSigner) Sign([]byte) (ports.SignatureHeaders, error) {
	return ports.SignatureHeaders{Version: "v1", RequestID: "signed", Timestamp: 1, Signature: "sig"}, nil
}

type fixedClock struct{ now time.Time }

func (c fixedClock) Now() time.Time                           { return c.now }
func (fixedClock) After(time.Duration) <-chan time.Time       { return make(chan time.Time) }
func (fixedClock) NewTicker(time.Duration) ports.Ticker       { return inertTicker{} }
func (fixedClock) Sleep(context.Context, time.Duration) error { return nil }

type inertTicker struct{}

func (inertTicker) C() <-chan time.Time { return make(chan time.Time) }
func (inertTicker) Stop()               {}

// The fallback batch must put every cancel frame on the wire before waiting
// for the first ACK. Responding only after the third send makes a sequential
// implementation fail this test by context deadline.
func TestCancelBatchPipelinesFramesBeforeWaitingForAcknowledgements(t *testing.T) {
	clk := fixedClock{now: time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)}
	sender := &pipelineSender{}
	client := &Client{
		sender:     sender,
		sessionID:  "session",
		signer:     fixedSigner{},
		limiter:    permitAll{},
		clock:      clk,
		bus:        events.New(nil),
		ackTimeout: 2 * time.Second,
		log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		pending:    make(map[string]*pending),
	}

	responded := false
	sender.onSend = func() {
		if responded || len(sender.frames) != 3 {
			return
		}
		responded = true
		for _, frame := range sender.frames {
			payload, err := json.Marshal(response{Code: 0, Status: "accepted", RequestID: frame.RequestID})
			require.NoError(t, err)
			client.OnMessage(context.Background(), payload, clk.now)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := client.CancelBatch(ctx,
		[]trading.ClOrdID{"cancel-1", "cancel-2", "cancel-3"}, ports.PrioritySafety)

	require.NoError(t, err)
	require.True(t, responded)
	require.Len(t, sender.frames, 3)
}

// A normal WS command returns after the frame is written. Its ACK and final
// outcome are delivered asynchronously, so waiting here would make the actor
// blind to market data during the acknowledgement timeout.
func TestSingleCancelDoesNotWaitForAcknowledgement(t *testing.T) {
	clk := fixedClock{now: time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)}
	sender := &pipelineSender{}
	client := &Client{
		sender:     sender,
		sessionID:  "session",
		signer:     fixedSigner{},
		limiter:    permitAll{},
		clock:      clk,
		bus:        events.New(nil),
		ackTimeout: 2 * time.Second,
		log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		pending:    make(map[string]*pending),
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	require.NoError(t, client.Cancel(ctx, "cancel-1", ports.PrioritySafety))
	require.Len(t, sender.frames, 1)
	require.NoError(t, ctx.Err(), "the command must return before its ACK timeout")
}
