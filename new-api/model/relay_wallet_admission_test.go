package model

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/glebarez/sqlite"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"gorm.io/gorm/schema"
)

func TestRelayWalletAdmissionMatrix(t *testing.T) {
	oldRedis, oldBatch := common.RedisEnabled, common.BatchUpdateEnabled
	common.RedisEnabled, common.BatchUpdateEnabled = false, false
	t.Cleanup(func() { common.RedisEnabled, common.BatchUpdateEnabled = oldRedis, oldBatch })
	for _, dialect := range []string{"sqlite", "mysql", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			var driver gorm.Dialector
			switch dialect {
			case "sqlite":
				driver = sqlite.Open(filepath.Join(t.TempDir(), "admission.db") + "?_pragma=busy_timeout(10000)")
			case "mysql":
				require.NotEmpty(t, os.Getenv("TEST_MYSQL_DSN"))
				driver = mysql.Open(os.Getenv("TEST_MYSQL_DSN"))
			case "postgres":
				require.NotEmpty(t, os.Getenv("TEST_POSTGRES_DSN"))
				driver = postgres.Open(os.Getenv("TEST_POSTGRES_DSN"))
			}
			db, err := gorm.Open(driver, &gorm.Config{NamingStrategy: schema.NamingStrategy{TablePrefix: "admission_test_"}, Logger: logger.Default.LogMode(logger.Silent)})
			require.NoError(t, err)
			sqlDB, err := db.DB()
			require.NoError(t, err)
			sqlDB.SetMaxOpenConns(4)
			previous := common.MainDatabaseType()
			common.SetMainDatabaseType(common.DatabaseType(dialect))
			t.Cleanup(func() {
				require.NoError(t, db.Migrator().DropTable(&RelayPolicyVersion{}, &RelayAdjustment{}, &RelayFixedCost{}, &RelayRiskState{}, &RelayRiskEvent{}, &RelayWalletCost{}, &RelayWalletCostSnapshot{}, &RelayRefundEvent{}, &RelayRefund{}, &RelayFundingEntry{}, &RelayFundingReady{}, &RelayPayEvent{}, &RelayPayOrder{}, &RelayWalletLogPending{}, &RelayWalletLogMarker{}, &RelayWalletReceipt{}, &RelayWalletEvent{}, &RelayWalletRequest{}, &RelayWalletLock{}, &Token{}, &User{}))
				require.NoError(t, sqlDB.Close())
				common.SetMainDatabaseType(previous)
			})
			require.NoError(t, db.AutoMigrate(&User{}, &Token{}))
			require.NoError(t, MigrateRelayWallet(db))
			limits := RelayWalletLimits{PerUser: 1, Global: 5}
			race := func(intents []RelayWalletRequest) []error {
				results := make([]error, len(intents))
				start := make(chan struct{})
				var wg sync.WaitGroup
				for i, intent := range intents {
					wg.Add(1)
					go func(i int, intent RelayWalletRequest) {
						defer wg.Done()
						<-start
						_, results[i] = ReserveRelayWalletWithLimits(db, intent, limits)
					}(i, intent)
				}
				close(start)
				wg.Wait()
				return results
			}
			var winners []RelayWalletRequest
			t.Run("same_user_race_and_idempotency", func(t *testing.T) {
				user, token, first := relayWalletFixture(t, db, 1000, 1000)
				second := first
				second.ID = uuid.NewString()
				intents := []RelayWalletRequest{first, second}
				for i, err := range race(intents) {
					if err == nil {
						winners = append(winners, intents[i])
					} else {
						require.ErrorIs(t, err, ErrRelayUserBusy)
					}
				}
				require.Len(t, winners, 1)
				for _, err := range race([]RelayWalletRequest{winners[0], winners[0]}) {
					require.NoError(t, err)
				}
				var savedUser User
				var savedToken Token
				require.NoError(t, db.First(&savedUser, user.Id).Error)
				require.NoError(t, db.First(&savedToken, token.Id).Error)
				require.EqualValues(t, 1000-first.Reserved, savedUser.Quota)
				require.EqualValues(t, 1000-first.Reserved, savedToken.RemainQuota)
				conflict := winners[0]
				conflict.Reserved++
				_, err := ReserveRelayWalletWithLimits(db, conflict, limits)
				require.ErrorIs(t, err, ErrRelayConflict)
			})
			var rejected RelayWalletRequest
			t.Run("global_race_and_no_rejected_debit", func(t *testing.T) {
				var intents []RelayWalletRequest
				for range 10 {
					_, _, intent := relayWalletFixture(t, db, 1000, 1000)
					intents = append(intents, intent)
				}
				for i, err := range race(intents) {
					if err == nil {
						winners = append(winners, intents[i])
					} else {
						require.ErrorIs(t, err, ErrRelayGlobalBusy)
						rejected = intents[i]
						var user User
						var token Token
						require.NoError(t, db.First(&user, rejected.UserID).Error)
						require.NoError(t, db.First(&token, rejected.TokenID).Error)
						require.Equal(t, 1000, user.Quota)
						require.Equal(t, 1000, token.RemainQuota)
					}
				}
				require.Len(t, winners, 5)
				_, err := ReserveRelayWalletWithLimits(db, winners[0], limits)
				require.NoError(t, err, "full capacity must still permit identical replay")
			})
			t.Run("unknown_holds_slot_release_and_settle_reopen", func(t *testing.T) {
				first := winners[0]
				_, err := AdvanceRelayWallet(db, first.ID, first.UserID, "submitted", 0)
				require.NoError(t, err)
				_, err = AdvanceRelayWallet(db, first.ID, first.UserID, "unknown", 0)
				require.NoError(t, err)
				// Recreated DB session has no process-local admission state.
				reopened := db.Session(&gorm.Session{NewDB: true})
				_, err = ReserveRelayWalletWithLimits(reopened, rejected, limits)
				require.ErrorIs(t, err, ErrRelayGlobalBusy)
				second := winners[1]
				_, err = AdvanceRelayWallet(db, second.ID, second.UserID, "released", 0)
				require.NoError(t, err)
				_, err = ReserveRelayWalletWithLimits(reopened, rejected, limits)
				require.NoError(t, err)
				_, err = AdvanceRelayWallet(db, first.ID, first.UserID, "settled", 1)
				require.NoError(t, err)
				first.ID = uuid.NewString()
				first.Reserved = 1
				_, err = ReserveRelayWalletWithLimits(reopened, first, limits)
				require.NoError(t, err)
			})
			t.Run("invalid_policy_fails_closed", func(t *testing.T) {
				for _, invalid := range []RelayWalletLimits{{}, {PerUser: -1, Global: 5}, {PerUser: 2, Global: 1}, {PerUser: 1, Global: 101}} {
					_, err := ReserveRelayWalletWithLimits(db, rejected, invalid)
					require.True(t, errors.Is(err, ErrRelayInput))
				}
			})
		})
	}
}
