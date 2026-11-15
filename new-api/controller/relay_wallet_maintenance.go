package controller

import (
	"context"
	"errors"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
)

// Relay wallet maintenance reuses New API's persistent, lease-deduplicated
// system task runner. Both switches are opt-in so installing the code cannot
// alter schemas or start accounting work by itself.
type relayWalletMaintenanceHandler struct{}

func (relayWalletMaintenanceHandler) Type() string { return model.SystemTaskTypeRelayWalletMaintenance }

func (relayWalletMaintenanceHandler) Enabled() bool {
	return common.GetEnvOrDefaultBool("RELAY_WALLET_SCHEMA", false) &&
		common.GetEnvOrDefaultBool("RELAY_WALLET_WORKER", false)
}

func (relayWalletMaintenanceHandler) Interval() time.Duration { return 15 * time.Second }

func (relayWalletMaintenanceHandler) NewPayload() any { return nil }

func (relayWalletMaintenanceHandler) Run(ctx context.Context, task *model.SystemTask, runnerID string) {
	// Pending tasks survive restarts; the scheduler's Enabled check does not
	// prevent the runner from claiming a task created before switches were disabled.
	if !(relayWalletMaintenanceHandler{}).Enabled() {
		finishSystemTaskHandler(task, runnerID, model.SystemTaskStatusFailed, nil, errors.New("relay wallet maintenance disabled"))
		return
	}
	if ctx.Err() != nil {
		finishSystemTaskHandler(task, runnerID, model.SystemTaskStatusFailed, nil, ctx.Err())
		return
	}
	recovered, recoveryErr := model.RecoverRelayWallet(model.DB.WithContext(ctx), 100)
	if ctx.Err() != nil {
		finishSystemTaskHandler(task, runnerID, model.SystemTaskStatusFailed, nil, ctx.Err())
		return
	}
	delivered, deliveryErr := model.SyncRelayWalletLogs(model.DB.WithContext(ctx), model.LOG_DB.WithContext(ctx), 100)
	result := map[string]int{"recovered": recovered, "logs_delivered": delivered}
	runErr := errors.Join(recoveryErr, deliveryErr, ctx.Err())
	status := model.SystemTaskStatusSucceeded
	if runErr != nil {
		status = model.SystemTaskStatusFailed
	}
	finishSystemTaskHandler(task, runnerID, status, result, runErr)
}

var _ service.ScheduledSystemTaskHandler = relayWalletMaintenanceHandler{}
