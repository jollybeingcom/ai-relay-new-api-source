package model

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/glebarez/sqlite"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"gorm.io/gorm/schema"
)

func TestRelayWalletDatabaseMatrix(t *testing.T) {
	for _, dialect := range []string{"sqlite", "mysql", "postgres"} {
		for _, scenario := range []string{"fresh", "upgrade"} {
			t.Run(dialect+"_"+scenario, func(t *testing.T) {
				var driver gorm.Dialector
				switch dialect {
				case "sqlite":
					driver = sqlite.Open(filepath.Join(t.TempDir(), "wallet.db") + "?_pragma=busy_timeout(10000)")
				case "mysql":
					dsn := os.Getenv("TEST_MYSQL_DSN")
					require.NotEmpty(t, dsn, "real MySQL is mandatory")
					driver = mysql.Open(dsn)
				case "postgres":
					dsn := os.Getenv("TEST_POSTGRES_DSN")
					require.NotEmpty(t, dsn, "real PostgreSQL is mandatory")
					driver = postgres.Open(dsn)
				}
				db, err := gorm.Open(driver, &gorm.Config{NamingStrategy: schema.NamingStrategy{TablePrefix: "relay_matrix_"}, Logger: logger.Default.LogMode(logger.Silent)})
				require.NoError(t, err)
				sqlDB, err := db.DB()
				require.NoError(t, err)
				sqlDB.SetMaxOpenConns(4)
				previousType := common.MainDatabaseType()
				common.SetMainDatabaseType(common.DatabaseType(dialect))
				t.Cleanup(func() {
					assert.NoError(t, db.Migrator().DropTable(&RelayPolicyVersion{}, &RelayAdjustment{}, &RelayFixedCost{}, &RelayRiskState{}, &RelayRiskEvent{}, &RelayWalletCost{}, &RelayWalletCostSnapshot{}, &RelayRefundEvent{}, &RelayRefund{}, &RelayFundingEntry{}, &RelayFundingReady{}, &RelayPayEvent{}, &RelayPayOrder{}, &RelayWalletLogPending{}, &RelayWalletLogMarker{}, &Log{}, &RelayWalletReceipt{}, &Channel{}, &RelayWalletEvent{}, &RelayWalletRequest{}, &RelayWalletLock{}, &Token{}, &User{}))
					assert.NoError(t, sqlDB.Close())
					common.SetMainDatabaseType(previousType)
				})
				query := "SELECT version()"
				if dialect == "sqlite" {
					query = "SELECT sqlite_version()"
				}
				var version string
				require.NoError(t, db.Raw(query).Scan(&version).Error)
				t.Logf("database version: %s", version)
				if scenario == "fresh" {
					require.NoError(t, MigrateRelayWallet(db))
				}
				// These are the exact stock release models, before our added migration.
				require.NoError(t, db.AutoMigrate(&User{}, &Token{}))
				legacy, legacyToken, _ := relayWalletFixture(t, db, 12345, 54321)
				require.NoError(t, MigrateRelayWallet(db))
				require.NoError(t, MigrateRelayWallet(db))
				var savedUser User
				var savedToken Token
				require.NoError(t, db.First(&savedUser, legacy.Id).Error)
				require.NoError(t, db.First(&savedToken, legacyToken.Id).Error)
				assert.Equal(t, legacy.Quota, savedUser.Quota)
				assert.Equal(t, legacyToken.Key, savedToken.Key)
				assert.Equal(t, legacyToken.RemainQuota, savedToken.RemainQuota)
				duplicate := User{Username: legacy.Username, Password: "test-only"}
				assert.Error(t, db.Create(&duplicate).Error, "existing username uniqueness survives migration")
				duplicateToken := Token{UserId: legacy.Id, Key: legacyToken.Key}
				assert.Error(t, db.Create(&duplicateToken).Error, "token uniqueness survives migration")

				t.Run("receipt_atomicity_and_replay", func(t *testing.T) {
					require.NoError(t, db.AutoMigrate(&Channel{}))
					channel := Channel{Name: "receipt-fixture", UsedQuota: 10}
					require.NoError(t, db.Create(&channel).Error)
					user, token, intent := relayWalletFixture(t, db, 1000, 1000)
					_, err := ReserveRelayWallet(db, intent)
					require.NoError(t, err)
					_, err = AdvanceRelayWallet(db, intent.ID, user.Id, "submitted", 0)
					require.NoError(t, err)
					receipt := RelayWalletReceipt{RequestID: intent.ID, UserID: user.Id, TokenID: token.Id, ChannelID: channel.Id, Model: "fixture", PriceVersion: intent.PriceVersion, Input: 100, CacheRead: 40, Output: 50, Actual: 170}
					// Receipt insert failure must roll back even a nested wallet settlement.
					require.NoError(t, db.Callback().Create().Before("gorm:create").Register("fixture_receipt_failure", func(tx *gorm.DB) {
						if _, ok := tx.Statement.Dest.(*RelayWalletReceipt); ok {
							tx.AddError(errors.New("fixture receipt failure"))
						}
					}))
					require.Error(t, SettleRelayWalletReceipt(db, receipt))
					require.NoError(t, db.Callback().Create().Remove("fixture_receipt_failure"))
					var stored RelayWalletRequest
					require.NoError(t, db.Where("id = ?", intent.ID).Take(&stored).Error)
					assert.Equal(t, "submitted", stored.State)
					require.NoError(t, db.First(&user, user.Id).Error)
					assert.Equal(t, 300, user.Quota)
					assert.Zero(t, user.RequestCount)
					require.NoError(t, db.First(&channel, channel.Id).Error)
					assert.EqualValues(t, 10, channel.UsedQuota)
					var workers sync.WaitGroup
					results := make(chan error, 2)
					for range 2 {
						workers.Go(func() { results <- SettleRelayWalletReceipt(db, receipt) })
					}
					workers.Wait()
					require.NoError(t, <-results)
					require.NoError(t, <-results)
					require.NoError(t, MigrateRelayWallet(db))
					require.NoError(t, SettleRelayWalletReceipt(db, receipt))
					var count int64
					require.NoError(t, db.Model(&RelayWalletReceipt{}).Where("request_id = ?", intent.ID).Count(&count).Error)
					assert.EqualValues(t, 1, count)
					require.NoError(t, db.First(&user, user.Id).Error)
					require.NoError(t, db.First(&token, token.Id).Error)
					require.NoError(t, db.First(&channel, channel.Id).Error)
					assert.Equal(t, 830, user.Quota)
					assert.Equal(t, 1, user.RequestCount)
					assert.Equal(t, 170, token.UsedQuota)
					assert.EqualValues(t, 180, channel.UsedQuota)
					// A separately opened log connection has its own tables/transaction.
					logDriver := driver
					if dialect == "sqlite" {
						logDriver = sqlite.Open(filepath.Join(t.TempDir(), "logs.db") + "?_pragma=busy_timeout(10000)")
					}
					logs, err := gorm.Open(logDriver, &gorm.Config{NamingStrategy: schema.NamingStrategy{TablePrefix: "relay_logs_"}, Logger: logger.Default.LogMode(logger.Silent)})
					require.NoError(t, err)
					logSQL, err := logs.DB()
					require.NoError(t, err)
					t.Cleanup(func() {
						assert.NoError(t, logs.Migrator().DropTable(&Log{}, &RelayWalletLogMarker{}))
						assert.NoError(t, logSQL.Close())
					})
					require.NoError(t, logs.AutoMigrate(&Log{}))
					require.NoError(t, MigrateRelayWalletLogs(logs))
					require.NoError(t, MigrateRelayWalletLogs(logs))
					require.NoError(t, logs.Callback().Create().Before("gorm:create").Register("fixture_log_failure", func(tx *gorm.DB) {
						if _, ok := tx.Statement.Dest.(*Log); ok {
							tx.AddError(errors.New("fixture log write failure"))
						}
					}))
					require.Error(t, ProjectRelayWalletLog(db, logs, intent.ID))
					require.NoError(t, logs.Model(&RelayWalletLogMarker{}).Where("request_id = ?", intent.ID).Count(&count).Error)
					assert.Zero(t, count, "failed log write also rolls back marker")
					require.NoError(t, logs.Callback().Create().Remove("fixture_log_failure"))
					require.NoError(t, db.Callback().Update().Before("gorm:update").Register("fixture_ack_failure", func(tx *gorm.DB) {
						if _, ok := tx.Statement.Model.(*RelayWalletLogPending); ok {
							tx.AddError(errors.New("fixture ack lost after log commit"))
						}
					}))
					require.Error(t, ProjectRelayWalletLog(db, logs, intent.ID))
					require.NoError(t, db.Callback().Update().Remove("fixture_ack_failure"))
					for range 2 {
						workers.Go(func() { results <- ProjectRelayWalletLog(db, logs, intent.ID) })
					}
					workers.Wait()
					require.NoError(t, <-results)
					require.NoError(t, <-results)
					require.NoError(t, logs.Model(&Log{}).Where("request_id = ?", intent.ID).Count(&count).Error)
					assert.EqualValues(t, 1, count)
					var display Log
					require.NoError(t, logs.Where("request_id = ?", intent.ID).Take(&display).Error)
					assert.Equal(t, user.Id, display.UserId)
					assert.Equal(t, 170, display.Quota)
					assert.Equal(t, 100, display.PromptTokens)
					assert.Contains(t, display.Other, `"cache_tokens":40`)
					require.NoError(t, db.First(&user, user.Id).Error)
					assert.Equal(t, 830, user.Quota, "log replay never charges")
					assert.Equal(t, 1, user.RequestCount)
					delivered, err := SyncRelayWalletLogs(db, logs, 100)
					require.NoError(t, err)
					assert.Zero(t, delivered, "acknowledged receipts leave the pending queue")
					changed := receipt
					changed.CacheRead++
					assert.ErrorIs(t, SettleRelayWalletReceipt(db, changed), ErrRelayConflict)
					assert.Error(t, db.Model(&RelayWalletReceipt{}).Where("request_id = ?", intent.ID).Update("actual", 0).Error)
					assert.Error(t, db.Where("request_id = ?", intent.ID).Delete(&RelayWalletReceipt{}).Error)
					// A zero-charge completion still persists exactly once, including MySQL.
					freeUser, freeToken, freeIntent := relayWalletFixture(t, db, 1000, 1000)
					_, err = ReserveRelayWallet(db, freeIntent)
					require.NoError(t, err)
					_, err = AdvanceRelayWallet(db, freeIntent.ID, freeUser.Id, "submitted", 0)
					require.NoError(t, err)
					free := RelayWalletReceipt{RequestID: freeIntent.ID, UserID: freeUser.Id, TokenID: freeToken.Id, ChannelID: channel.Id, Model: "fixture", PriceVersion: freeIntent.PriceVersion}
					require.NoError(t, SettleRelayWalletReceipt(db, free))
					require.NoError(t, SettleRelayWalletReceipt(db, free))
					require.NoError(t, db.First(&freeUser, freeUser.Id).Error)
					assert.Equal(t, 1000, freeUser.Quota)
					assert.Equal(t, 1, freeUser.RequestCount)
					require.NoError(t, logs.Callback().Create().Before("gorm:create").Register("fixture_log_retry", func(tx *gorm.DB) {
						if _, ok := tx.Statement.Dest.(*Log); ok {
							tx.AddError(errors.New("fixture unavailable log store"))
						}
					}))
					delivered, err = SyncRelayWalletLogs(db, logs, 100)
					require.Error(t, err)
					assert.Zero(t, delivered)
					var queued RelayWalletLogPending
					require.NoError(t, db.Where("request_id = ?", freeIntent.ID).Take(&queued).Error)
					assert.Greater(t, queued.NextAttemptAt, time.Now().Unix())
					require.NoError(t, logs.Callback().Create().Remove("fixture_log_retry"))
					require.NoError(t, db.Model(&RelayWalletLogPending{}).Where("request_id = ?", freeIntent.ID).Update("next_attempt_at", 0).Error)
					delivered, err = SyncRelayWalletLogs(db, logs, 100)
					require.NoError(t, err)
					assert.Equal(t, 1, delivered)
					require.NoError(t, logs.Where("request_id = ?", freeIntent.ID).Delete(&Log{}).Error)
					require.NoError(t, ProjectRelayWalletLog(db, logs, freeIntent.ID))
					require.NoError(t, logs.Model(&Log{}).Where("request_id = ?", freeIntent.ID).Count(&count).Error)
					assert.Zero(t, count, "retained marker must not resurrect expired display logs")
				})

				t.Run("idempotency_and_settlement", func(t *testing.T) {
					user, token, intent := relayWalletFixture(t, db, 1000, 1000)
					first, err := ReserveRelayWallet(db, intent)
					require.NoError(t, err)
					again, err := ReserveRelayWallet(db, intent)
					require.NoError(t, err)
					assert.Equal(t, first, again)
					other := intent
					other.PayloadHash = strings.Repeat("b", 64)
					_, err = ReserveRelayWallet(db, other)
					assert.ErrorIs(t, err, ErrRelayConflict)
					other = intent
					other.PriceSnapshot = `{"input":2}`
					_, err = ReserveRelayWallet(db, other)
					assert.ErrorIs(t, err, ErrRelayConflict)
					changed, err := AdvanceRelayWallet(db, intent.ID, user.Id+1000, "submitted", 0)
					assert.Error(t, err)
					assert.False(t, changed)
					changed, err = AdvanceRelayWallet(db, intent.ID, user.Id, "submitted", 0)
					require.NoError(t, err)
					assert.True(t, changed)
					changed, err = AdvanceRelayWallet(db, intent.ID, user.Id, "submitted", 0)
					require.NoError(t, err)
					assert.False(t, changed, "duplicate submission never grants dispatch")
					_, err = AdvanceRelayWallet(db, intent.ID, user.Id, "settled", 701)
					assert.ErrorIs(t, err, ErrRelayState)
					changed, err = AdvanceRelayWallet(db, intent.ID, user.Id, "settled", 400)
					require.NoError(t, err)
					assert.True(t, changed)
					changed, err = AdvanceRelayWallet(db, intent.ID, user.Id, "settled", 400)
					require.NoError(t, err)
					assert.False(t, changed)
					_, err = AdvanceRelayWallet(db, intent.ID, user.Id, "settled", 401)
					assert.ErrorIs(t, err, ErrRelayConflict)
					_, err = AdvanceRelayWallet(db, intent.ID, user.Id, "released", 0)
					assert.ErrorIs(t, err, ErrRelayState)
					relayWalletBalances(t, db, user.Id, token.Id, 600, 600, 400, 1)
					var events []RelayWalletEvent
					require.NoError(t, db.Where("request_id = ?", intent.ID).Order("id").Find(&events).Error)
					require.Len(t, events, 3)
					assert.Equal(t, []int64{-700, 0, 300}, []int64{events[0].WalletDelta, events[1].WalletDelta, events[2].WalletDelta})
					assert.ErrorIs(t, db.Model(&RelayWalletRequest{}).Where("id = ?", intent.ID).Update("price_snapshot", "{}").Error, ErrRelayConflict)
					assert.ErrorIs(t, db.Model(&RelayWalletEvent{}).Where("request_id = ?", intent.ID).Update("actual", 0).Error, ErrRelayState)
					assert.ErrorIs(t, db.Where("request_id = ?", intent.ID).Delete(&RelayWalletEvent{}).Error, ErrRelayState)
					// Migration with real journal data must preserve intent and terminal state.
					require.NoError(t, MigrateRelayWallet(db))
					again, err = ReserveRelayWallet(db, intent)
					require.NoError(t, err)
					assert.Equal(t, "settled", again.State)
					assert.Equal(t, intent.PriceSnapshot, again.PriceSnapshot)
				})

				t.Run("atomic_failure_and_concurrency", func(t *testing.T) {
					user, token, intent := relayWalletFixture(t, db, 1000, 100)
					_, err := ReserveRelayWallet(db, intent)
					assert.ErrorIs(t, err, ErrRelayQuota)
					relayWalletBalances(t, db, user.Id, token.Id, 1000, 100, 0, 0)
					require.NoError(t, db.Model(&token).Update("remain_quota", 1000).Error)
					other := intent
					other.ID = uuid.NewString()
					start := make(chan struct{})
					results := make(chan error, 2)
					var workers sync.WaitGroup
					for _, request := range []RelayWalletRequest{intent, other} {
						workers.Go(func() { <-start; _, err := ReserveRelayWallet(db, request); results <- err })
					}
					close(start)
					workers.Wait()
					close(results)
					won := 0
					for err := range results {
						if err == nil {
							won++
						} else {
							assert.ErrorIs(t, err, ErrRelayQuota)
						}
					}
					assert.Equal(t, 1, won)
					relayWalletBalances(t, db, user.Id, token.Id, 300, 300, 0, 0)
				})

				t.Run("invalid_intents_and_token_ownership", func(t *testing.T) {
					user, token, intent := relayWalletFixture(t, db, 1000, 1000)
					for _, amount := range []int64{-1, 0, 1 << 31} {
						invalid := intent
						invalid.Reserved = amount
						_, err := ReserveRelayWallet(db, invalid)
						assert.ErrorIs(t, err, ErrRelayInput)
					}
					_, foreign, _ := relayWalletFixture(t, db, 1000, 1000)
					invalid := intent
					invalid.TokenID = foreign.Id
					_, err := ReserveRelayWallet(db, invalid)
					assert.ErrorIs(t, err, ErrRelayQuota)
					for _, changes := range []map[string]any{
						{"status": 2}, {"unlimited_quota": true}, {"expired_time": time.Now().Unix() - 1},
					} {
						require.NoError(t, db.Model(&Token{}).Where("id = ?", token.Id).Updates(changes).Error)
						_, err = ReserveRelayWallet(db, intent)
						assert.ErrorIs(t, err, ErrRelayQuota)
						relayWalletBalances(t, db, user.Id, token.Id, 1000, 1000, 0, 0)
						require.NoError(t, db.Model(&Token{}).Where("id = ?", token.Id).Updates(map[string]any{"status": 1, "unlimited_quota": false, "expired_time": -1}).Error)
					}
					require.NoError(t, db.Model(&User{}).Where("id = ?", user.Id).Update("status", 2).Error)
					_, err = ReserveRelayWallet(db, intent)
					assert.ErrorIs(t, err, ErrRelayQuota)
					relayWalletBalances(t, db, user.Id, token.Id, 1000, 1000, 0, 0)
					require.NoError(t, db.Model(&User{}).Where("id = ?", user.Id).Update("status", 1).Error)
					_, err = ReserveRelayWallet(db, intent)
					require.NoError(t, err)
					require.NoError(t, db.Model(&Token{}).Where("id = ?", token.Id).Update("status", 2).Error)
					_, err = AdvanceRelayWallet(db, intent.ID, user.Id, "submitted", 0)
					assert.ErrorIs(t, err, ErrRelayQuota)
					_, err = AdvanceRelayWallet(db, intent.ID, user.Id, "released", 0)
					require.NoError(t, err)
					relayWalletBalances(t, db, user.Id, token.Id, 1000, 1000, 0, 0)
				})

				t.Run("wallet_overflow_rolls_back", func(t *testing.T) {
					user, token, intent := relayWalletFixture(t, db, common.MaxWalletQuota, 1000)
					_, err := ReserveRelayWallet(db, intent)
					require.NoError(t, err)
					_, err = AdvanceRelayWallet(db, intent.ID, user.Id, "submitted", 0)
					require.NoError(t, err)
					// A concurrent top-up has filled the wallet before the refund delta.
					require.NoError(t, db.Model(&User{}).Where("id = ?", user.Id).Update("quota", common.MaxWalletQuota).Error)
					_, err = AdvanceRelayWallet(db, intent.ID, user.Id, "settled", 400)
					assert.ErrorIs(t, err, ErrRelayQuota)
					relayWalletBalances(t, db, user.Id, token.Id, common.MaxWalletQuota, 300, 0, 0)
					var stored RelayWalletRequest
					require.NoError(t, db.Where("id = ?", intent.ID).Take(&stored).Error)
					assert.Equal(t, "submitted", stored.State)
				})

				t.Run("concurrent_replay_grants_once", func(t *testing.T) {
					user, token, intent := relayWalletFixture(t, db, 1000, 1000)
					start := make(chan struct{})
					results := make(chan error, 2)
					var workers sync.WaitGroup
					for range 2 {
						workers.Go(func() { <-start; _, err := ReserveRelayWallet(db, intent); results <- err })
					}
					close(start)
					workers.Wait()
					close(results)
					for err := range results {
						require.NoError(t, err)
					}
					for _, target := range []string{"submitted", "settled"} {
						start = make(chan struct{})
						outcomes := make(chan bool, 2)
						results = make(chan error, 2)
						actual := int64(0)
						if target == "settled" {
							actual = 400
						}
						for range 2 {
							workers.Go(func() {
								<-start
								changed, err := AdvanceRelayWallet(db, intent.ID, user.Id, target, actual)
								outcomes <- changed
								results <- err
							})
						}
						close(start)
						workers.Wait()
						close(outcomes)
						close(results)
						for err := range results {
							require.NoError(t, err)
						}
						won := 0
						for changed := range outcomes {
							if changed {
								won++
							}
						}
						assert.Equal(t, 1, won)
					}
					relayWalletBalances(t, db, user.Id, token.Id, 600, 600, 400, 1)
				})

				t.Run("audit_failure_rolls_back_and_unknown_holds", func(t *testing.T) {
					user, token, intent := relayWalletFixture(t, db, 1000, 1000)
					_, err := ReserveRelayWallet(db, intent)
					require.NoError(t, err)
					_, err = AdvanceRelayWallet(db, intent.ID, user.Id, "submitted", 0)
					require.NoError(t, err)
					require.NoError(t, db.Callback().Create().Before("gorm:create").Register("relay_test_audit_failure", func(tx *gorm.DB) {
						if tx.Statement.Schema != nil && tx.Statement.Schema.Name == "RelayWalletEvent" {
							tx.AddError(errors.New("audit unavailable"))
						}
					}))
					_, err = AdvanceRelayWallet(db, intent.ID, user.Id, "settled", 700)
					assert.ErrorContains(t, err, "audit unavailable")
					require.NoError(t, db.Callback().Create().Remove("relay_test_audit_failure"))
					relayWalletBalances(t, db, user.Id, token.Id, 300, 300, 0, 0)
					_, err = AdvanceRelayWallet(db, intent.ID, user.Id, "released", 0)
					assert.ErrorIs(t, err, ErrRelayState)
					_, err = AdvanceRelayWallet(db, intent.ID, user.Id, "unknown", 0)
					require.NoError(t, err)
					_, err = AdvanceRelayWallet(db, intent.ID, user.Id, "released", 0)
					assert.ErrorIs(t, err, ErrRelayState)
					require.NoError(t, db.Model(&RelayWalletRequest{}).Where("id = ?", intent.ID).Update("unknown_at", time.Now().Unix()-86401).Error)
					_, err = AdvanceRelayWallet(db, intent.ID, user.Id, "released", 0)
					require.NoError(t, err)
					_, err = AdvanceRelayWallet(db, intent.ID, user.Id, "released", 0)
					require.NoError(t, err)
					_, err = AdvanceRelayWallet(db, intent.ID, user.Id, "settled", 500)
					assert.ErrorIs(t, err, ErrRelayState)
					relayWalletBalances(t, db, user.Id, token.Id, 1000, 1000, 0, 0)
				})

				t.Run("stale_recovery_keeps_unknown_funds", func(t *testing.T) {
					user, token, intent := relayWalletFixture(t, db, 1000, 1000)
					_, err := ReserveRelayWallet(db, intent)
					require.NoError(t, err)
					_, _, submitted := relayWalletFixture(t, db, 1000, 1000)
					_, err = ReserveRelayWallet(db, submitted)
					require.NoError(t, err)
					_, err = AdvanceRelayWallet(db, submitted.ID, submitted.UserID, "submitted", 0)
					require.NoError(t, err)
					require.NoError(t, db.Model(&RelayWalletRequest{}).Where("id IN ?", []string{intent.ID, submitted.ID}).Update("updated_at", time.Now().Unix()-301).Error)
					_, err = RecoverRelayWallet(db, 101)
					assert.ErrorIs(t, err, ErrRelayInput)
					count, err := RecoverRelayWallet(db, 100)
					require.NoError(t, err)
					assert.Equal(t, 2, count)
					count, err = RecoverRelayWallet(db, 100)
					require.NoError(t, err)
					assert.Zero(t, count)
					relayWalletBalances(t, db, user.Id, token.Id, 1000, 1000, 0, 0)
					relayWalletBalances(t, db, submitted.UserID, submitted.TokenID, 300, 300, 0, 0)
					_, err = AdvanceRelayWallet(db, intent.ID, user.Id, "submitted", 0)
					assert.ErrorIs(t, err, ErrRelayState)
				})

				t.Run("zero_delta_and_release_before_send", func(t *testing.T) {
					for _, actual := range []int64{0, 700} {
						user, token, intent := relayWalletFixture(t, db, 1000, 1000)
						_, err := ReserveRelayWallet(db, intent)
						require.NoError(t, err)
						_, err = AdvanceRelayWallet(db, intent.ID, user.Id, "submitted", 0)
						require.NoError(t, err)
						_, err = AdvanceRelayWallet(db, intent.ID, user.Id, "settled", actual)
						require.NoError(t, err)
						relayWalletBalances(t, db, user.Id, token.Id, int(1000-actual), int(1000-actual), int(actual), 1)
					}
					user, token, intent := relayWalletFixture(t, db, 1000, 1000)
					_, err := ReserveRelayWallet(db, intent)
					require.NoError(t, err)
					_, err = AdvanceRelayWallet(db, intent.ID, user.Id, "released", 0)
					require.NoError(t, err)
					_, err = AdvanceRelayWallet(db, intent.ID, user.Id, "submitted", 0)
					assert.ErrorIs(t, err, ErrRelayState)
					relayWalletBalances(t, db, user.Id, token.Id, 1000, 1000, 0, 0)
				})
			})
		}
	}
}

func relayWalletFixture(t *testing.T, db *gorm.DB, wallet, quota int) (User, Token, RelayWalletRequest) {
	t.Helper()
	user := User{Username: "test_" + uuid.NewString()[:12], AffCode: uuid.NewString()[:12], Password: "fixture-only", Quota: wallet, Role: 1, Status: 1}
	require.NoError(t, db.Create(&user).Error)
	token := Token{UserId: user.Id, Key: uuid.NewString(), RemainQuota: quota, Status: 1, ExpiredTime: -1}
	require.NoError(t, db.Create(&token).Error)
	return user, token, RelayWalletRequest{ID: uuid.NewString(), UserID: user.Id, TokenID: token.Id,
		PayloadHash: strings.Repeat("a", 64), PriceVersion: "test-v1", PriceSnapshot: `{"input":1}`, Reserved: 700}
}

func relayWalletBalances(t *testing.T, db *gorm.DB, userID, tokenID, wallet, remain, used, count int) {
	t.Helper()
	var user User
	var token Token
	require.NoError(t, db.First(&user, userID).Error)
	require.NoError(t, db.First(&token, tokenID).Error)
	assert.Equal(t, wallet, user.Quota, fmt.Sprintf("user %d wallet", userID))
	assert.Equal(t, remain, token.RemainQuota)
	assert.Equal(t, used, user.UsedQuota)
	assert.Equal(t, used, token.UsedQuota)
	assert.Equal(t, count, user.RequestCount)
}

// Exercise the release's real startup migrations, not a reduced model list.
func TestRelayWalletStartupMatrix(t *testing.T) {
	for _, dialect := range []string{"sqlite", "mysql", "postgres"} {
		scenarios := []string{"fresh", "upgrade"}
		if dialect != "sqlite" {
			scenarios = append(scenarios, "fresh_logs", "upgrade_logs")
		}
		for _, scenario := range scenarios {
			t.Run(dialect+"_"+scenario, func(t *testing.T) {
				separateLogs := strings.HasSuffix(scenario, "_logs")
				fresh := strings.HasPrefix(scenario, "fresh")
				oldDB, oldLog := DB, LOG_DB
				oldType, oldLogType := common.MainDatabaseType(), common.LogDatabaseType()
				oldMaster, oldSQLite := common.IsMasterNode, common.SQLitePath
				oldDeadline := LegacyAccessTokenRetireAt()
				t.Cleanup(func() {
					DB, LOG_DB = oldDB, oldLog
					common.SetDatabaseTypes(oldType, oldLogType)
					common.IsMasterNode, common.SQLitePath = oldMaster, oldSQLite
					legacyAccessTokenRetireAt.Store(oldDeadline)
					initCol()
				})
				common.IsMasterNode = true
				t.Setenv("LOG_SQL_DSN", "")
				t.Setenv("SKIP_64BIT_QUOTA_SCHEMA_CHECK", "false")
				t.Setenv("SQL_MAX_OPEN_CONNS", "4")
				t.Setenv("SQL_MAX_IDLE_CONNS", "2")
				switch dialect {
				case "sqlite":
					common.SQLitePath = filepath.Join(t.TempDir(), "startup.db") + "?_pragma=busy_timeout(10000)"
					t.Setenv("SQL_DSN", "local")
				case "mysql":
					fixtureDSN := "relay_test:isolated-test-only@tcp(mysql:3306)/relay_test?charset=utf8mb4&parseTime=True&loc=Local"
					require.True(t, os.Getenv("TEST_MYSQL_DSN") == fixtureDSN, "startup tests require the isolated Compose fixture")
					t.Setenv("SQL_DSN", fixtureDSN)
				case "postgres":
					fixtureDSN := "host=postgres user=relay_test password=isolated-test-only dbname=relay_test port=5432 sslmode=disable"
					require.True(t, os.Getenv("TEST_POSTGRES_DSN") == fixtureDSN, "startup tests require the isolated Compose fixture")
					t.Setenv("SQL_DSN", "postgres://relay_test:isolated-test-only@postgres:5432/relay_test?sslmode=disable")
				}
				if separateLogs {
					t.Setenv("LOG_SQL_DSN", strings.Replace(os.Getenv("SQL_DSN"), "/relay_test?", "/relay_logs?", 1))
				}
				initial, _, err := chooseDB("SQL_DSN", false)
				require.NoError(t, err)
				initialSQL, err := initial.DB()
				require.NoError(t, err)
				tables, err := initial.Migrator().GetTables()
				require.NoError(t, initialSQL.Close())
				require.NoError(t, err)
				require.Empty(t, tables, "never migrate or clean an existing nonempty database")
				if separateLogs {
					initialLog, _, err := chooseDB("LOG_SQL_DSN", true)
					require.NoError(t, err)
					tables, err := initialLog.Migrator().GetTables()
					require.NoError(t, err)
					require.Empty(t, tables, "never clean a nonempty log database")
					pool, err := initialLog.DB()
					require.NoError(t, err)
					require.NoError(t, pool.Close())
				}
				DB, LOG_DB = nil, nil
				t.Cleanup(func() {
					if DB == nil {
						return
					}
					if separateLogs && LOG_DB != nil {
						tables, err := LOG_DB.Migrator().GetTables()
						require.NoError(t, err)
						for _, table := range tables {
							assert.NoError(t, LOG_DB.Migrator().DropTable(table))
						}
						pool, err := LOG_DB.DB()
						require.NoError(t, err)
						assert.NoError(t, pool.Close())
					}
					// Only the initially empty, exact test fixture DSN is reachable here.
					tables, err := DB.Migrator().GetTables()
					if assert.NoError(t, err) {
						for _, table := range tables {
							if dialect == "sqlite" && strings.HasPrefix(table, "sqlite_") {
								continue
							}
							assert.NoError(t, DB.Migrator().DropTable(table))
						}
					}
					sqlDB, err := DB.DB()
					if assert.NoError(t, err) {
						assert.NoError(t, sqlDB.Close())
					}
				})
				t.Setenv("RELAY_WALLET_SCHEMA", fmt.Sprint(fresh))
				restartRelayWalletDatabase(t)
				assert.Equal(t, fresh, DB.Migrator().HasTable(&RelayWalletRequest{}))
				user, token, intent := relayWalletFixture(t, DB, 1000, 1000)
				order := TopUp{UserId: user.Id, Amount: 1000, TradeNo: "fixture-" + uuid.NewString(), Status: "success"}
				require.NoError(t, DB.Create(&order).Error)
				entry := Log{UserId: user.Id, Type: LogTypeConsume, Quota: 12, RequestId: "legacy-fixture"}
				require.NoError(t, LOG_DB.Create(&entry).Error)
				deadline := LegacyAccessTokenRetireAt()
				t.Setenv("RELAY_WALLET_SCHEMA", "true")
				restartRelayWalletDatabase(t)
				require.True(t, LOG_DB.Migrator().HasTable(&RelayWalletLogMarker{}))
				relayWalletBalances(t, DB, user.Id, token.Id, 1000, 1000, 0, 0)
				_, err = ReserveRelayWallet(DB, intent)
				require.NoError(t, err)
				_, err = AdvanceRelayWallet(DB, intent.ID, user.Id, "submitted", 0)
				require.NoError(t, err)
				restartRelayWalletDatabase(t)
				assert.Equal(t, deadline, LegacyAccessTokenRetireAt(), "startup must not extend stored deadlines")
				relayWalletBalances(t, DB, user.Id, token.Id, 300, 300, 0, 0)
				changed, err := AdvanceRelayWallet(DB, intent.ID, user.Id, "submitted", 0)
				require.NoError(t, err)
				assert.False(t, changed, "restarting must not grant a second dispatch")
				channel := Channel{Name: "startup-receipt"}
				require.NoError(t, DB.Create(&channel).Error)
				receipt := RelayWalletReceipt{RequestID: intent.ID, UserID: user.Id, TokenID: token.Id, ChannelID: channel.Id, Model: "fixture", PriceVersion: intent.PriceVersion, Input: 10, Output: 10, Actual: 400}
				require.NoError(t, SettleRelayWalletReceipt(DB, receipt))
				require.NoError(t, ProjectRelayWalletLog(DB, LOG_DB, intent.ID))
				// Simulate an older receipt without a queue row. Migration backfills
				// delivery without removing the destination's durable deduplication.
				require.NoError(t, DB.Where("request_id = ?", intent.ID).Delete(&RelayWalletLogPending{}).Error)
				restartRelayWalletDatabase(t)
				delivered, err := SyncRelayWalletLogs(DB, LOG_DB, 100)
				require.NoError(t, err)
				assert.Equal(t, 1, delivered)
				var projectedCount int64
				require.NoError(t, LOG_DB.Model(&Log{}).Where("request_id = ?", intent.ID).Count(&projectedCount).Error)
				assert.EqualValues(t, 1, projectedCount)
				visible, total, err := GetUserLogs(user.Id, LogFilter{RequestId: intent.ID}, 0, 20)
				require.NoError(t, err)
				assert.EqualValues(t, 1, total)
				require.Len(t, visible, 1)
				assert.Equal(t, 400, visible[0].Quota)
				_, otherTotal, err := GetUserLogs(user.Id+1000, LogFilter{RequestId: intent.ID}, 0, 20)
				require.NoError(t, err)
				assert.Zero(t, otherTotal, "stock self-log query must retain owner isolation")
				relayWalletBalances(t, DB, user.Id, token.Id, 600, 600, 400, 1)
				var savedOrder TopUp
				var savedLog Log
				require.NoError(t, DB.First(&savedOrder, order.Id).Error)
				require.NoError(t, LOG_DB.First(&savedLog, entry.Id).Error)
				assert.Equal(t, order.TradeNo, savedOrder.TradeNo)
				assert.Equal(t, order.Amount, savedOrder.Amount)
				assert.Equal(t, entry.Quota, savedLog.Quota)
				assert.Equal(t, entry.RequestId, savedLog.RequestId)
				duplicate := User{Username: user.Username, AffCode: uuid.NewString()[:12], Password: "fixture-only"}
				assert.Error(t, DB.Create(&duplicate).Error)
				duplicateToken := Token{UserId: user.Id, Key: token.Key}
				assert.Error(t, DB.Create(&duplicateToken).Error)
				duplicateOrder := TopUp{UserId: user.Id, TradeNo: order.TradeNo}
				assert.Error(t, DB.Create(&duplicateOrder).Error)
			})
		}
	}
}

func restartRelayWalletDatabase(t *testing.T) {
	t.Helper()
	if LOG_DB != nil && LOG_DB != DB {
		pool, err := LOG_DB.DB()
		require.NoError(t, err)
		require.NoError(t, pool.Close())
	}
	if DB != nil {
		sqlDB, err := DB.DB()
		require.NoError(t, err)
		require.NoError(t, sqlDB.Close())
	}
	require.NoError(t, InitDB())
	require.NoError(t, InitLogDB())
}
