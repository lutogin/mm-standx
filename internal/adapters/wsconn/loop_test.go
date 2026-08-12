package wsconn

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Признак необязательности объявляет сам обработчик: цикл соединения ничего
// не знает о том, чей это канал, и знать не должен.
func TestOptionalIsTakenFromTheHandler(t *testing.T) {
	require.False(t, optional(plainHandler{}), "a handler that says nothing is required")
	require.False(t, optional(declaredHandler{optional: false}),
		"a handler may declare itself required explicitly")
	require.True(t, optional(declaredHandler{optional: true}))
}

type plainHandler struct{}

func (plainHandler) OnConnect(context.Context, Sender) error      { return nil }
func (plainHandler) OnMessage(context.Context, []byte, time.Time) {}
func (plainHandler) OnDisconnect(error)                           {}

type declaredHandler struct {
	plainHandler
	optional bool
}

func (h declaredHandler) Optional() bool { return h.optional }

var (
	_ Handler  = plainHandler{}
	_ Optional = declaredHandler{}
)
