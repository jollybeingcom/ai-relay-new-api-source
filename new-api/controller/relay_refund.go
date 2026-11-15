package controller

import (
	"context"
	"errors"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
)

type relayRefundQueryHandler struct{}

func (relayRefundQueryHandler) Type() string { return model.SystemTaskTypeRelayRefundQuery }
func (relayRefundQueryHandler) Enabled() bool {
	if !common.GetEnvOrDefaultBool("RELAY_WALLET_SCHEMA", false) || !common.GetEnvOrDefaultBool("RELAY_REFUND_WORKER", false) {
		return false
	}
	_, err := service.CurrentRelayPayChannel()
	return err == nil
}
func (relayRefundQueryHandler) Interval() time.Duration { return 30 * time.Second }
func (relayRefundQueryHandler) NewPayload() any         { return nil }
func (relayRefundQueryHandler) Run(ctx context.Context, task *model.SystemTask, runnerID string) {
	if !(relayRefundQueryHandler{}).Enabled() {
		finishSystemTaskHandler(task, runnerID, model.SystemTaskStatusFailed, nil, errors.New("refund worker disabled"))
		return
	}
	changed, err := service.SyncRelayRefunds(ctx, 50)
	status := model.SystemTaskStatusSucceeded
	if err != nil {
		status = model.SystemTaskStatusFailed
	}
	finishSystemTaskHandler(task, runnerID, status, map[string]int{"changed": changed}, err)
}

var _ service.ScheduledSystemTaskHandler = relayRefundQueryHandler{}
