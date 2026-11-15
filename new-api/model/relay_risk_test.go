package model

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

func TestRelayRiskMatrix(t *testing.T) {
	t.Setenv("RELAY_RISK_CONTROL", "true")
	for _, engine := range []string{"sqlite", "mysql", "postgres"} {
		t.Run(engine, func(t *testing.T) {
			for _, scenario := range []string{"income_cost_month", "fixed_history_threshold_resume", "maintenance_callbacks_refunds", "prospective_stop_commits", "missing_accounting", "unknown_release_risk", "restart_persists"} {
				t.Run(scenario, func(t *testing.T) {
					var driver gorm.Dialector
					switch engine {
					case "sqlite":
						driver = sqlite.Open(filepath.Join(t.TempDir(), "risk.db") + "?_pragma=busy_timeout(10000)")
					case "mysql":
						require.NotEmpty(t, os.Getenv("TEST_MYSQL_DSN"))
						driver = mysql.Open(os.Getenv("TEST_MYSQL_DSN"))
					case "postgres":
						require.NotEmpty(t, os.Getenv("TEST_POSTGRES_DSN"))
						driver = postgres.Open(os.Getenv("TEST_POSTGRES_DSN"))
					}
					config := &gorm.Config{NamingStrategy: schema.NamingStrategy{TablePrefix: "risk_test_"}, Logger: logger.Default.LogMode(logger.Silent)}
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
					admin := User{Username: "risk-admin", AffCode: "risk-admin", Password: "fixture", Role: common.RoleAdminUser, Status: common.UserStatusEnabled}
					require.NoError(t, db.Create(&admin).Error)
					month := RelayRiskMonth(time.Now())
					fixed := RelayFixedCost{ID: uuid.NewString(), Month: month, Category: "hosting", AmountFen: 0, Estimated: true, ReviewerID: admin.Id, Reason: "explicit fixture budget"}
					if scenario != "missing_accounting" {
						require.NoError(t, RecordRelayFixedCost(db, fixed))
					}
					user, token, intent := relayWalletFixture(t, db, 1000000, 1000000)
					channel := Channel{Type: 1, Name: "risk-fixture"}
					require.NoError(t, db.Create(&channel).Error)
					intent.CostBasis = &RelayWalletCostSnapshot{ChannelID: channel.Id, Model: "fixture", InputRate: "3000000", CacheReadRate: "300000", OutputRate: "9000000", InputLimit: 100, OutputLimit: 20}
					read := func() RelayRiskReport { r, err := ReadRelayRisk(db, month); require.NoError(t, err); return r }
					settle := func() {
						_, err := ReserveRelayWallet(db, intent)
						require.NoError(t, err)
						_, err = AdvanceRelayWallet(db, intent.ID, user.Id, "submitted", 0)
						require.NoError(t, err)
						require.NoError(t, SettleRelayWalletReceipt(db, RelayWalletReceipt{RequestID: intent.ID, UserID: user.Id, TokenID: token.Id, ChannelID: channel.Id, Model: "fixture", PriceVersion: intent.PriceVersion, Input: 100, CacheRead: 50, Output: 10, Actual: 400}))
					}
					replace := func(fen int64, estimated bool) {
						prior := fixed.ID
						fixed.ID = uuid.NewString()
						fixed.Supersedes = &prior
						fixed.AmountFen = fen
						fixed.Estimated = estimated
						require.NoError(t, RecordRelayFixedCost(db, fixed))
					}
					switch scenario {
					case "income_cost_month":
						settle()
						r := read()
						require.EqualValues(t, 400, r.Income)
						require.EqualValues(t, 255, r.Upstream)
						require.EqualValues(t, -145, r.Loss)
						require.Zero(t, r.MissingCosts)
						start, _, err := relayRiskMonthBounds(month)
						require.NoError(t, err)
						require.NoError(t, db.Exec("UPDATE risk_test_relay_wallet_receipts SET created_at = ? WHERE request_id = ?", start-1, intent.ID).Error)
						require.NoError(t, db.Exec("UPDATE risk_test_relay_wallet_costs SET created_at = ? WHERE request_id = ?", start-1, intent.ID).Error)
						r = read()
						require.Zero(t, r.Income)
						require.Zero(t, r.Upstream)
						r, err = ReadRelayRisk(db, RelayRiskMonth(time.Unix(start-1, 0)))
						require.NoError(t, err)
						require.EqualValues(t, 400, r.Income)
						require.EqualValues(t, 255, r.Upstream)
					case "fixed_history_threshold_resume":
						require.NoError(t, RecordRelayFixedCost(db, fixed))
						replace(100000, true)
						r := read()
						require.Equal(t, "warning", r.State.Mode)
						require.EqualValues(t, RelayRiskWarningQuota, r.FixedEstimated)
						replace(150000, false)
						r = read()
						require.Equal(t, "stopped", r.State.Mode)
						require.Zero(t, r.FixedEstimated)
						require.EqualValues(t, RelayRiskStopQuota, r.FixedActual)
						_, err := ChangeRelayRisk(db, admin.Id, r.State.Revision, "resume", "still unsafe")
						require.ErrorIs(t, err, ErrRelayStopped)
						replace(0, false)
						r = read()
						require.Equal(t, "stopped", r.State.Mode)
						_, err = ChangeRelayRisk(db, admin.Id, r.State.Revision, "resume", " ")
						require.Error(t, err)
						_, err = ChangeRelayRisk(db, user.Id, r.State.Revision, "resume", "not admin")
						require.Error(t, err)
						_, err = ChangeRelayRisk(db, admin.Id, r.State.Revision-1, "resume", "stale")
						require.ErrorIs(t, err, ErrRelayConflict)
						state, err := ChangeRelayRisk(db, admin.Id, r.State.Revision, "resume", "corrected actual cost verified")
						require.NoError(t, err)
						require.Equal(t, "normal", state.Mode)
						require.Error(t, db.Model(&fixed).Update("amount_fen", 1).Error)
					case "maintenance_callbacks_refunds":
						order, err := CreateRelayPayOrder(db, RelayPayOrder{ID: "RP" + strings.Repeat("1", 30), UserID: user.Id, AmountFen: 1000, Quota: 10000000, Currency: "CNY", Channel: "mock", MerchantApp: "fixture", ConfigVersion: "v1", RateVersion: "cny-1e6-v1", ExpiresAt: time.Now().Unix() + 900}, 3)
						require.NoError(t, err)
						state, err := ChangeRelayRisk(db, admin.Id, read().State.Revision, "maintenance", "planned work")
						require.NoError(t, err)
						_, err = ReserveRelayWallet(db, intent)
						require.ErrorIs(t, err, ErrRelayStopped)
						blocked := order
						blocked.ID = "RP" + strings.Repeat("2", 30)
						_, err = CreateRelayPayOrder(db, blocked, 3)
						require.ErrorIs(t, err, ErrRelayStopped)
						order, err = ApplyRelayPayFact(db, RelayPayFact{OrderID: order.ID, Channel: order.Channel, App: order.MerchantApp, TradeNo: "trade1", AmountFen: 1000, Currency: "CNY", Status: "paid", Evidence: strings.Repeat("a", 64)})
						require.NoError(t, err)
						require.NoError(t, InitializeRelayFunding(db, user.Id, 1000000, "verified fixture opening"))
						refund, err := RequestRelayRefund(db, user.Id, "RF"+strings.Repeat("1", 30), order.ID, 1)
						require.NoError(t, err)
						require.NoError(t, ReviewRelayRefund(db, refund.ID, admin.Id, "approved fixture", true))
						_, err = ClaimRelayRefund(db, refund.ID)
						require.NoError(t, err)
						require.NoError(t, ApplyRelayRefundFact(db, RelayRefundFact{RefundID: refund.ID, OrderID: order.ID, Channel: order.Channel, App: order.MerchantApp, TradeNo: *order.TradeNo, AmountFen: 1, Status: "succeeded", Evidence: strings.Repeat("b", 64)}))
						r := read()
						require.Zero(t, r.Income, "top-up principal is not income")
						require.Equal(t, state.Mode, r.State.Mode)
					case "prospective_stop_commits":
						replace(149999, false)
						intent.CostBasis.InputRate = "100000000"
						_, err := ReserveRelayWallet(db, intent)
						require.ErrorIs(t, err, ErrRelayStopped)
						r := read()
						require.Equal(t, "stopped", r.State.Mode)
						require.Equal(t, "monthly_loss_limit", r.State.Reason)
						relayWalletBalances(t, db, user.Id, token.Id, 1000000, 1000000, 0, 0)
					case "missing_accounting":
						_, err := ReserveRelayWallet(db, intent)
						require.ErrorIs(t, err, ErrRelayStopped)
						require.Equal(t, "accounting_incomplete", read().State.Reason)
						require.NoError(t, RecordRelayFixedCost(db, fixed))
						_, err = ChangeRelayRisk(db, admin.Id, read().State.Revision, "resume", "budget confirmed")
						require.NoError(t, err)
						t.Setenv("RELAY_RISK_CONTROL", "false")
						intent.CostBasis = nil
						_, err = ReserveRelayWallet(db, intent)
						require.NoError(t, err)
						_, err = AdvanceRelayWallet(db, intent.ID, user.Id, "submitted", 0)
						require.NoError(t, err)
						_, err = AdvanceRelayWallet(db, intent.ID, user.Id, "settled", 100)
						require.NoError(t, err)
						t.Setenv("RELAY_RISK_CONTROL", "true")
						r, err := EvaluateRelayRisk(db, time.Now())
						require.NoError(t, err)
						require.EqualValues(t, 1, r.MissingCosts)
						require.Equal(t, "stopped", r.State.Mode)
					case "unknown_release_risk":
						_, err := ReserveRelayWallet(db, intent)
						require.NoError(t, err)
						_, err = AdvanceRelayWallet(db, intent.ID, user.Id, "submitted", 0)
						require.NoError(t, err)
						_, err = AdvanceRelayWallet(db, intent.ID, user.Id, "unknown", 0)
						require.NoError(t, err)
						require.EqualValues(t, 480, read().PendingRisk)
						require.NoError(t, db.Model(&RelayWalletRequest{}).Where("id = ?", intent.ID).Update("unknown_at", time.Now().Unix()-90000).Error)
						_, err = AdvanceRelayWallet(db, intent.ID, user.Id, "released", 0)
						require.NoError(t, err)
						require.EqualValues(t, 480, read().PendingRisk, "release is not evidence of zero upstream cost")
					case "restart_persists":
						replace(150000, false)
						reopened, err := gorm.Open(driver, config)
						require.NoError(t, err)
						pool, err := reopened.DB()
						require.NoError(t, err)
						defer pool.Close()
						require.NoError(t, MigrateRelayWallet(reopened))
						r, err := EvaluateRelayRisk(reopened, time.Now().AddDate(0, 1, 0))
						require.NoError(t, err)
						require.Equal(t, "stopped", r.State.Mode)
					}
					var events []RelayRiskEvent
					require.NoError(t, db.Order("revision").Find(&events).Error)
					for i, event := range events {
						require.EqualValues(t, i+1, event.Revision, fmt.Sprint(events))
					}
				})
			}
		})
	}
}

func TestRelayRiskMonthBoundary(t *testing.T) {
	require.Equal(t, "2026-10", RelayRiskMonth(time.Date(2026, 10, 31, 15, 59, 59, 0, time.UTC)))
	require.Equal(t, "2026-11", RelayRiskMonth(time.Date(2026, 10, 31, 16, 0, 0, 0, time.UTC)))
	_, _, err := relayRiskMonthBounds("2026-1")
	require.Error(t, err)
}
