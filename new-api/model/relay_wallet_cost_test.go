package model

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"gorm.io/gorm/schema"
)

func TestRelayWalletCostMatrix(t *testing.T) {
	for _, engine := range []string{"sqlite", "mysql", "postgres"} {
		t.Run(engine, func(t *testing.T) {
			var driver gorm.Dialector
			switch engine {
			case "sqlite":
				driver = sqlite.Open(filepath.Join(t.TempDir(), "cost.db") + "?_pragma=busy_timeout(10000)")
			case "mysql":
				require.NotEmpty(t, os.Getenv("TEST_MYSQL_DSN"))
				driver = mysql.Open(os.Getenv("TEST_MYSQL_DSN"))
			case "postgres":
				require.NotEmpty(t, os.Getenv("TEST_POSTGRES_DSN"))
				driver = postgres.Open(os.Getenv("TEST_POSTGRES_DSN"))
			}
			config := &gorm.Config{NamingStrategy: schema.NamingStrategy{TablePrefix: "cost_test_"}, Logger: logger.Default.LogMode(logger.Silent)}
			db, err := gorm.Open(driver, config)
			require.NoError(t, err)
			old := common.MainDatabaseType()
			common.SetMainDatabaseType(common.DatabaseType(engine))
			t.Cleanup(func() {
				require.NoError(t, db.Migrator().DropTable(&RelayPolicyVersion{}, &RelayAdjustment{}, &RelayFixedCost{}, &RelayRiskState{}, &RelayRiskEvent{}, &RelayWalletCost{}, &RelayWalletCostSnapshot{}, &RelayRefundEvent{}, &RelayRefund{}, &RelayFundingEntry{}, &RelayFundingReady{}, &RelayPayEvent{}, &RelayPayOrder{}, &RelayWalletLogPending{}, &RelayWalletLogMarker{}, &RelayWalletReceipt{}, &RelayWalletEvent{}, &RelayWalletRequest{}, &RelayWalletLock{}, &Channel{}, &User{}, &Token{}))
				pool, err := db.DB()
				require.NoError(t, err)
				require.NoError(t, pool.Close())
				common.SetMainDatabaseType(old)
			})
			require.NoError(t, db.AutoMigrate(&User{}, &Token{}, &Channel{}))
			require.NoError(t, MigrateRelayWallet(db))
			require.NoError(t, MigrateRelayWallet(db))
			for _, scenario := range []string{"snapshot_replay_restart_settle", "reserve_rollback", "settlement_rollback", "missing_snapshot", "legacy_missing", "zero_cost"} {
				t.Run(scenario, func(t *testing.T) {
					user, token, intent := relayWalletFixture(t, db, 1000, 1000)
					channel := Channel{Type: 1, Name: scenario}
					require.NoError(t, db.Create(&channel).Error)
					basis := RelayWalletCostSnapshot{RequestID: intent.ID, PriceVersion: intent.PriceVersion, ChannelID: channel.Id, Model: "fixture",
						InputRate: "3000000", CacheReadRate: "300000", OutputRate: "9000000", InputLimit: 100, OutputLimit: 20}
					if scenario == "zero_cost" {
						basis.InputRate, basis.CacheReadRate, basis.OutputRate = "0", "0", "0"
					}
					intent.CostBasis = &basis
					if scenario == "legacy_missing" {
						intent.CostBasis = nil
					}
					if scenario == "reserve_rollback" {
						require.NoError(t, db.Create(&basis).Error)
					}
					saved, err := ReserveRelayWallet(db, intent)
					if scenario == "reserve_rollback" {
						require.Error(t, err)
						relayWalletBalances(t, db, user.Id, token.Id, 1000, 1000, 0, 0)
						var count int64
						require.NoError(t, db.Model(&RelayWalletRequest{}).Where("id = ?", intent.ID).Count(&count).Error)
						require.Zero(t, count)
						return
					}
					require.NoError(t, err)
					require.Equal(t, scenario != "legacy_missing", saved.CostTracked)
					if scenario == "snapshot_replay_restart_settle" {
						_, err = ReserveRelayWallet(db, intent)
						require.NoError(t, err)
						basis.InputRate = "4000000"
						_, err = ReserveRelayWallet(db, intent)
						require.ErrorIs(t, err, ErrRelayConflict)
						basis.InputRate = "3000000"
						require.Error(t, db.Model(&RelayWalletCostSnapshot{}).Where("request_id = ?", intent.ID).Update("input_rate", "9").Error)
						require.Error(t, db.Model(&RelayWalletRequest{}).Where("id = ?", intent.ID).Update("cost_tracked", false).Error)
					}
					_, err = AdvanceRelayWallet(db, intent.ID, user.Id, "submitted", 0)
					require.NoError(t, err)
					_, err = AdvanceRelayWallet(db, intent.ID, user.Id, "unknown", 0)
					require.NoError(t, err)
					if saved.CostTracked {
						_, err = AdvanceRelayWallet(db, intent.ID, user.Id, "settled", 400)
						require.ErrorIs(t, err, ErrRelayState, "integer-only settlement cannot bypass cost accounting")
					}
					if scenario == "missing_snapshot" {
						require.NoError(t, db.Exec("DELETE FROM cost_test_relay_wallet_cost_snapshots WHERE request_id = ?", intent.ID).Error)
					}
					if scenario == "settlement_rollback" {
						require.NoError(t, db.Create(&RelayWalletCost{RequestID: intent.ID, PriceVersion: "conflict"}).Error)
					}
					// A fresh database handle represents recovery without in-memory route prices.
					reopened, err := gorm.Open(driver, config)
					require.NoError(t, err)
					pool, err := reopened.DB()
					require.NoError(t, err)
					defer pool.Close()
					receipt := RelayWalletReceipt{RequestID: intent.ID, UserID: user.Id, TokenID: token.Id, ChannelID: channel.Id,
						Model: "fixture", PriceVersion: intent.PriceVersion, Input: 100, CacheRead: 50, Output: 10, Actual: 400}
					err = SettleRelayWalletReceipt(reopened, receipt)
					if scenario == "missing_snapshot" || scenario == "settlement_rollback" {
						require.Error(t, err)
						relayWalletBalances(t, db, user.Id, token.Id, 300, 300, 0, 0)
						var request RelayWalletRequest
						require.NoError(t, db.Where("id = ?", intent.ID).Take(&request).Error)
						require.Equal(t, "unknown", request.State)
						var count int64
						require.NoError(t, db.Model(&RelayWalletReceipt{}).Where("request_id = ?", intent.ID).Count(&count).Error)
						require.Zero(t, count)
						require.NoError(t, db.First(&channel, channel.Id).Error)
						require.Zero(t, channel.UsedQuota)
						return
					}
					require.NoError(t, err)
					require.NoError(t, SettleRelayWalletReceipt(reopened, receipt))
					if scenario == "snapshot_replay_restart_settle" {
						var wg sync.WaitGroup
						errors := make(chan error, 8)
						for i := 0; i < 8; i++ {
							wg.Add(1)
							go func() { defer wg.Done(); errors <- SettleRelayWalletReceipt(reopened, receipt) }()
						}
						wg.Wait()
						close(errors)
						for err := range errors {
							require.NoError(t, err)
						}
					}
					relayWalletBalances(t, db, user.Id, token.Id, 600, 600, 400, 1)
					var costs []RelayWalletCost
					require.NoError(t, db.Where("request_id = ?", intent.ID).Find(&costs).Error)
					if scenario == "legacy_missing" {
						require.Empty(t, costs)
						return
					}
					require.Len(t, costs, 1)
					want := int64(255)
					if scenario == "zero_cost" {
						want = 0
					}
					require.Equal(t, want, costs[0].Actual)
					require.Error(t, db.Model(&costs[0]).Update("actual", 0).Error)
					require.Error(t, db.Delete(&costs[0]).Error)
				})
			}
		})
	}
}

func TestRelayWalletCostArithmetic(t *testing.T) {
	b := RelayWalletCostSnapshot{RequestID: "11111111-1111-4111-8111-111111111111", PriceVersion: "v1", ChannelID: 1, Model: "m", InputRate: "0.5", CacheReadRate: "0.25", OutputRate: "0", InputLimit: 1000000, OutputLimit: 1}
	bound, err := b.Bound()
	require.NoError(t, err)
	require.EqualValues(t, 1, bound)
	b.Reserved = bound
	r := RelayWalletReceipt{RequestID: b.RequestID, PriceVersion: b.PriceVersion, ChannelID: 1, Model: "m", Input: 1000000}
	charge, err := b.charge(r)
	require.NoError(t, err)
	require.EqualValues(t, 1, charge)
	r.Input = 999999
	charge, err = b.charge(r)
	require.NoError(t, err)
	require.Zero(t, charge)
	for _, invalid := range []string{"", "-1", "NaN", "1e6", "0.0000000000001", "9999999999999999"} {
		b.InputRate = invalid
		_, err = b.Bound()
		require.Error(t, err, invalid)
	}
}
