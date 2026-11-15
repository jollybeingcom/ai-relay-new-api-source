package controller

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/glebarez/sqlite"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestRelayWalletMaintenanceMatrix(t *testing.T) {
	oldRedis, oldBatch := common.RedisEnabled, common.BatchUpdateEnabled
	common.RedisEnabled, common.BatchUpdateEnabled = false, false
	t.Cleanup(func() { common.RedisEnabled, common.BatchUpdateEnabled = oldRedis, oldBatch })
	for _, dialect := range []string{"sqlite", "mysql", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			var driver gorm.Dialector
			switch dialect {
			case "sqlite":
				driver = sqlite.Open(filepath.Join(t.TempDir(), "worker.db") + "?_pragma=busy_timeout(10000)")
			case "mysql":
				require.NotEmpty(t, os.Getenv("TEST_MYSQL_DSN"))
				driver = mysql.Open(os.Getenv("TEST_MYSQL_DSN"))
			case "postgres":
				require.NotEmpty(t, os.Getenv("TEST_POSTGRES_DSN"))
				driver = postgres.Open(os.Getenv("TEST_POSTGRES_DSN"))
			}
			// The upstream lease predicates use literal table names. This entire
			// database is disposable, so use the real schema rather than prefixes.
			db, err := gorm.Open(driver, &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
			require.NoError(t, err)
			sqlDB, err := db.DB()
			require.NoError(t, err)
			sqlDB.SetMaxOpenConns(4)
			oldDB, oldLog, oldType := model.DB, model.LOG_DB, common.MainDatabaseType()
			model.DB, model.LOG_DB = db, db
			common.SetMainDatabaseType(common.DatabaseType(dialect))
			t.Cleanup(func() {
				require.NoError(t, db.Migrator().DropTable(&model.RelayPolicyVersion{}, &model.RelayAdjustment{}, &model.RelayFixedCost{}, &model.RelayRiskState{}, &model.RelayRiskEvent{}, &model.RelayWalletCost{}, &model.RelayWalletCostSnapshot{}, &model.SystemTask{}, &model.SystemTaskLock{}, &model.RelayRefundEvent{}, &model.RelayRefund{}, &model.RelayFundingEntry{}, &model.RelayFundingReady{}, &model.RelayPayEvent{}, &model.RelayPayOrder{}, &model.RelayWalletLogPending{}, &model.RelayWalletLogMarker{}, &model.Log{}, &model.RelayWalletReceipt{}, &model.RelayWalletEvent{}, &model.RelayWalletRequest{}, &model.RelayWalletLock{}, &model.User{}, &model.Token{}, &model.Channel{}))
				model.DB, model.LOG_DB = oldDB, oldLog
				common.SetMainDatabaseType(oldType)
				require.NoError(t, sqlDB.Close())
			})
			require.NoError(t, db.AutoMigrate(&model.SystemTask{}, &model.SystemTaskLock{}, &model.User{}, &model.Token{}, &model.Channel{}, &model.Log{}))
			require.NoError(t, model.MigrateRelayWallet(db))
			t.Setenv("RELAY_WALLET_SCHEMA", "true")
			t.Setenv("RELAY_WALLET_WORKER", "true")
			handler := relayWalletMaintenanceHandler{}
			fixture := func(t *testing.T) model.RelayWalletRequest {
				id := uuid.NewString()
				user := model.User{Username: id, AffCode: id[:8], Role: common.RoleCommonUser, Status: common.UserStatusEnabled, Quota: 1000}
				require.NoError(t, db.Create(&user).Error)
				token := model.Token{UserId: user.Id, Key: id, Status: common.TokenStatusEnabled, RemainQuota: 1000, ExpiredTime: -1}
				require.NoError(t, db.Create(&token).Error)
				intent := model.RelayWalletRequest{ID: id, UserID: user.Id, TokenID: token.Id, PayloadHash: strings.Repeat("a", 64), PriceVersion: "worker-fixture", PriceSnapshot: `{"fixture":true}`, Reserved: 100}
				_, err := model.ReserveRelayWallet(db, intent)
				require.NoError(t, err)
				return intent
			}
			create := func(t *testing.T) *model.SystemTask {
				task, err := model.CreateSystemTask(handler.Type(), nil, nil)
				require.NoError(t, err)
				return task
			}
			run := func(t *testing.T, ctx context.Context) *model.SystemTask {
				task := create(t)
				claimed, ok, err := model.ClaimSystemTask(task.ID, handler.Type(), "runner", time.Now().Unix()+60)
				require.NoError(t, err)
				require.True(t, ok)
				handler.Run(ctx, claimed, "runner")
				stored, err := model.GetSystemTaskByTaskID(task.TaskID)
				require.NoError(t, err)
				return stored
			}
			state := func(t *testing.T, intent model.RelayWalletRequest, expected string, quota int) {
				var request model.RelayWalletRequest
				var user model.User
				require.NoError(t, db.Where("id = ?", intent.ID).Take(&request).Error)
				require.NoError(t, db.First(&user, intent.UserID).Error)
				require.Equal(t, expected, request.State)
				require.Equal(t, quota, user.Quota)
			}
			t.Run("disabled_and_cancelled_pending", func(t *testing.T) {
				intent := fixture(t)
				require.NoError(t, db.Model(&model.RelayWalletRequest{}).Where("id = ?", intent.ID).Update("updated_at", time.Now().Unix()-301).Error)
				for _, key := range []string{"RELAY_WALLET_SCHEMA", "RELAY_WALLET_WORKER"} {
					t.Setenv(key, "false")
					require.False(t, handler.Enabled())
					require.Equal(t, model.SystemTaskStatusFailed, run(t, context.Background()).Status)
					state(t, intent, "reserved", 900)
					t.Setenv(key, "true")
				}
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				require.Equal(t, model.SystemTaskStatusFailed, run(t, ctx).Status)
				state(t, intent, "reserved", 900)
				require.Equal(t, model.SystemTaskStatusSucceeded, run(t, context.Background()).Status)
				state(t, intent, "released", 1000)
			})
			t.Run("restart_concurrent_claim_and_replay", func(t *testing.T) {
				intent := fixture(t)
				_, err := model.AdvanceRelayWallet(db, intent.ID, intent.UserID, "submitted", 0)
				require.NoError(t, err)
				require.NoError(t, db.Model(&model.RelayWalletRequest{}).Where("id = ?", intent.ID).Update("updated_at", time.Now().Unix()-301).Error)
				task := create(t)
				_, ok, err := model.ClaimSystemTask(task.ID, handler.Type(), "crashed", time.Now().Unix()+60)
				require.NoError(t, err)
				require.True(t, ok)
				require.NoError(t, db.Model(&model.SystemTaskLock{}).Where("task_id = ?", task.TaskID).Update("locked_until", time.Now().Unix()-1).Error)
				require.NoError(t, model.ExpireStaleSystemTaskLocks(time.Now().Unix()))
				task = create(t)
				var wg sync.WaitGroup
				wins := make(chan *model.SystemTask, 2)
				errs := make(chan error, 2)
				for i := 0; i < 2; i++ {
					wg.Add(1)
					go func() {
						defer wg.Done()
						claimed, ok, err := model.ClaimSystemTask(task.ID, handler.Type(), "restart", time.Now().Unix()+60)
						errs <- err
						if ok {
							wins <- claimed
						}
					}()
				}
				wg.Wait()
				require.NoError(t, <-errs)
				require.NoError(t, <-errs)
				require.Len(t, wins, 1)
				handler.Run(context.Background(), <-wins, "restart")
				state(t, intent, "unknown", 900)
				require.Equal(t, model.SystemTaskStatusSucceeded, run(t, context.Background()).Status)
				state(t, intent, "unknown", 900)
			})
			t.Run("log_failure_backoff_and_replay", func(t *testing.T) {
				intent := fixture(t)
				_, err := model.AdvanceRelayWallet(db, intent.ID, intent.UserID, "submitted", 0)
				require.NoError(t, err)
				channel := model.Channel{Name: "worker-fixture"}
				require.NoError(t, db.Create(&channel).Error)
				require.NoError(t, model.SettleRelayWalletReceipt(db, model.RelayWalletReceipt{RequestID: intent.ID, UserID: intent.UserID, TokenID: intent.TokenID, ChannelID: channel.Id, Model: "fixture", PriceVersion: intent.PriceVersion, Actual: 20, Input: 1, Output: 1}))
				require.NoError(t, db.Callback().Create().Before("gorm:create").Register("worker_log_failure", func(tx *gorm.DB) {
					if tx.Statement.Schema != nil && tx.Statement.Schema.Name == "Log" {
						tx.AddError(errors.New("log unavailable"))
					}
				}))
				require.Equal(t, model.SystemTaskStatusFailed, run(t, context.Background()).Status)
				require.NoError(t, db.Callback().Create().Remove("worker_log_failure"))
				var pending model.RelayWalletLogPending
				require.NoError(t, db.Where("request_id = ?", intent.ID).Take(&pending).Error)
				require.Greater(t, pending.NextAttemptAt, time.Now().Unix())
				require.Zero(t, pending.DeliveredAt)
				require.Equal(t, model.SystemTaskStatusSucceeded, run(t, context.Background()).Status)
				var count int64
				require.NoError(t, db.Model(&model.Log{}).Count(&count).Error)
				require.Zero(t, count)
				require.NoError(t, db.Model(&model.RelayWalletLogPending{}).Where("request_id = ?", intent.ID).Update("next_attempt_at", 0).Error)
				require.Equal(t, model.SystemTaskStatusSucceeded, run(t, context.Background()).Status)
				// Simulate losing source acknowledgement after destination commit.
				require.NoError(t, db.Model(&model.RelayWalletLogPending{}).Where("request_id = ?", intent.ID).Update("delivered_at", 0).Error)
				require.Equal(t, model.SystemTaskStatusSucceeded, run(t, context.Background()).Status)
				require.NoError(t, db.Model(&model.Log{}).Count(&count).Error)
				require.EqualValues(t, 1, count)
				state(t, intent, "settled", 980)
			})
		})
	}
}
