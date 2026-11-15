package controller

import (
	"github.com/QuantumNous/new-api/model"
	"github.com/stretchr/testify/require"
	"testing"
	"time"
)

func TestRelayRefundWorkerGate(t *testing.T) {
	h := relayRefundQueryHandler{}
	t.Setenv("RELAY_PAY_CHANNEL", "mock")
	t.Setenv("RELAY_REFUND_WORKER", "false")
	t.Setenv("RELAY_WALLET_SCHEMA", "true")
	require.False(t, h.Enabled())
	t.Setenv("RELAY_REFUND_WORKER", "true")
	require.True(t, h.Enabled())
	t.Setenv("RELAY_WALLET_SCHEMA", "false")
	require.False(t, h.Enabled())
	require.Equal(t, model.SystemTaskTypeRelayRefundQuery, h.Type())
	require.Equal(t, 30*time.Second, h.Interval())
}
