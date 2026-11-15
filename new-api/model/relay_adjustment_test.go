package model

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"gorm.io/gorm/schema"
)

func TestRelayAdjustmentMatrix(t *testing.T) {
	oldRedis, oldBatch := common.RedisEnabled, common.BatchUpdateEnabled
	common.RedisEnabled, common.BatchUpdateEnabled = false, false
	t.Cleanup(func() { common.RedisEnabled, common.BatchUpdateEnabled = oldRedis, oldBatch })
	for _, dialect := range []string{"sqlite", "mysql", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			var driver gorm.Dialector
			switch dialect {
			case "sqlite":
				driver = sqlite.Open(filepath.Join(t.TempDir(), "adjustment.db") + "?_pragma=busy_timeout(10000)")
			case "mysql":
				require.NotEmpty(t, os.Getenv("TEST_MYSQL_DSN"))
				driver = mysql.Open(os.Getenv("TEST_MYSQL_DSN"))
			case "postgres":
				require.NotEmpty(t, os.Getenv("TEST_POSTGRES_DSN"))
				driver = postgres.Open(os.Getenv("TEST_POSTGRES_DSN"))
			}
			db, err := gorm.Open(driver, &gorm.Config{NamingStrategy: schema.NamingStrategy{TablePrefix: "adjust_test_"}, Logger: logger.Default.LogMode(logger.Silent)})
			require.NoError(t, err)
			pool, err := db.DB()
			require.NoError(t, err)
			pool.SetMaxOpenConns(4)
			previous := common.MainDatabaseType()
			common.SetMainDatabaseType(common.DatabaseType(dialect))
			t.Cleanup(func() {
				require.NoError(t, db.Migrator().DropTable(&RelayPolicyVersion{}, &RelayAdjustment{}, &RelayFixedCost{}, &RelayRiskState{}, &RelayRiskEvent{}, &RelayWalletCost{}, &RelayWalletCostSnapshot{}, &RelayRefundEvent{}, &RelayRefund{}, &RelayFundingEntry{}, &RelayFundingReady{}, &RelayPayEvent{}, &RelayPayOrder{}, &RelayWalletLogPending{}, &RelayWalletLogMarker{}, &RelayWalletReceipt{}, &RelayWalletEvent{}, &RelayWalletRequest{}, &RelayWalletLock{}, &Token{}, &User{}))
				require.NoError(t, pool.Close())
				common.SetMainDatabaseType(previous)
			})
			require.NoError(t, db.AutoMigrate(&User{}, &Token{}))
			require.NoError(t, MigrateRelayWallet(db))
			require.NoError(t, MigrateRelayWallet(db))
			sequence := 0
			newID := func(prefix string) string { sequence++; return fmt.Sprintf("%s%030x", prefix, sequence) }
			newUser := func(role int) User {
				u := User{Username: newID("u"), AffCode: fmt.Sprintf("a%d", sequence), Password: "fixture-only", Role: role, Status: common.UserStatusEnabled}
				require.NoError(t, db.Create(&u).Error)
				return u
			}
			admin := newUser(common.RoleAdminUser)
			evidence := strings.Repeat("e", 64)
			fund := func(u User) RelayPayOrder {
				o, err := CreateRelayPayOrder(db, RelayPayOrder{ID: newID("RP"), UserID: u.Id, AmountFen: 1000, Quota: 10000000, Currency: "CNY", Channel: "mock", MerchantApp: "fixture", ConfigVersion: "fixture", RateVersion: "cny-1e6-v1", ExpiresAt: time.Now().Unix() + 900}, 3)
				require.NoError(t, err)
				o, err = ApplyRelayPayFact(db, RelayPayFact{OrderID: o.ID, Channel: o.Channel, App: o.MerchantApp, TradeNo: "T" + o.ID, AmountFen: o.AmountFen, Currency: "CNY", Status: "paid", Evidence: strings.Repeat("a", 64)})
				require.NoError(t, err)
				return o
			}
			ready := func() (User, RelayPayOrder) {
				u := newUser(common.RoleCommonUser)
				o := fund(u)
				require.NoError(t, InitializeRelayFunding(db, u.Id, 0, "fixture"))
				return u, o
			}
			balance := func(u User) int64 {
				var got User
				require.NoError(t, db.Where("id = ?", u.Id).Take(&got).Error)
				return int64(got.Quota)
			}
			credit := func(u User, fen int64) RelayAdjustmentInput {
				return RelayAdjustmentInput{ID: newID("AJ"), UserID: u.Id, AmountFen: fen, Reason: "service compensation fixture", Evidence: evidence}
			}
			consume := func(u User, quota int64) {
				token := Token{UserId: u.Id, Key: newID("k"), Name: "fixture", Status: common.TokenStatusEnabled, RemainQuota: int(quota), ExpiredTime: -1}
				require.NoError(t, db.Create(&token).Error)
				intent := RelayWalletRequest{ID: fmt.Sprintf("00000000-0000-4000-8000-%012x", sequence), UserID: u.Id, TokenID: token.Id, PayloadHash: strings.Repeat("c", 64), PriceVersion: "fixture", PriceSnapshot: `{"fixture":true}`, Reserved: quota}
				_, err := ReserveRelayWallet(db, intent)
				require.NoError(t, err)
				_, err = AdvanceRelayWallet(db, intent.ID, u.Id, "submitted", 0)
				require.NoError(t, err)
				_, err = AdvanceRelayWallet(db, intent.ID, u.Id, "settled", quota)
				require.NoError(t, err)
			}
			t.Run("credit_replay_conflict_and_not_refundable", func(t *testing.T) {
				u, o := ready()
				input := credit(u, 500)
				for range 2 {
					saved, err := ApplyRelayAdjustment(db, admin.Id, input)
					require.NoError(t, err)
					require.Equal(t, int64(10000000), saved.BalanceBefore)
					require.Equal(t, int64(15000000), saved.BalanceAfter)
				}
				require.Equal(t, int64(15000000), balance(u))
				changed := input
				changed.Reason = "different reason"
				_, err := ApplyRelayAdjustment(db, admin.Id, changed)
				require.ErrorIs(t, err, ErrRelayConflict)
				changed = input
				changed.AmountFen++
				_, err = ApplyRelayAdjustment(db, admin.Id, changed)
				require.ErrorIs(t, err, ErrRelayConflict)
				// Non-cash credit never increases what the payment channel can return.
				view, err := ReadRelayFunding(db, u.Id)
				require.NoError(t, err)
				require.True(t, view.Consistent)
				require.Len(t, view.Sources, 2)
				require.Equal(t, int64(10000000), view.Sources[0].Refundable)
				require.Equal(t, "adjustment", view.Sources[1].Kind)
				require.Zero(t, view.Sources[1].Refundable)
				// FIFO charges the earlier order first: after 7 CNY only 3 CNY cash is refundable.
				consume(u, 7000000)
				_, err = RequestRelayRefund(db, u.Id, newID("RF"), o.ID, 301)
				require.ErrorIs(t, err, ErrRelayQuota)
				_, err = RequestRelayRefund(db, u.Id, newID("RF"), o.ID, 300)
				require.NoError(t, err)
				var count int64
				require.NoError(t, db.Model(&RelayAdjustment{}).Where("id = ?", input.ID).Count(&count).Error)
				require.EqualValues(t, 1, count)
			})
			t.Run("debit_only_unspent_noncash", func(t *testing.T) {
				u, o := ready()
				grant := credit(u, 500)
				_, err := ApplyRelayAdjustment(db, admin.Id, grant)
				require.NoError(t, err)
				// FIFO: the earlier order is consumed before the later credit.
				consume(u, 12000000)
				target := relayAdjustmentEntryID(grant.ID)
				debit := RelayAdjustmentInput{ID: newID("AJ"), UserID: u.Id, AmountFen: -301, Target: target, Reason: "reverse unused credit", Evidence: evidence}
				_, err = ApplyRelayAdjustment(db, admin.Id, debit)
				require.ErrorIs(t, err, ErrRelayQuota)
				debit.ID, debit.Target = newID("AJ"), o.ID
				_, err = ApplyRelayAdjustment(db, admin.Id, debit)
				require.ErrorIs(t, err, ErrRelayInput)
				debit.ID, debit.Target = newID("AJ"), "adj:AJ"+strings.Repeat("0", 30)
				_, err = ApplyRelayAdjustment(db, admin.Id, debit)
				require.ErrorIs(t, err, gorm.ErrRecordNotFound)
				debit.ID, debit.Target, debit.AmountFen = newID("AJ"), target, -300
				saved, err := ApplyRelayAdjustment(db, admin.Id, debit)
				require.NoError(t, err)
				require.Equal(t, int64(-3000000), saved.Quota)
				require.Zero(t, balance(u))
				view, err := ReadRelayFunding(db, u.Id)
				require.NoError(t, err)
				require.True(t, view.Consistent)
				require.Zero(t, view.Sources[1].Available)
				// A credit with a target or a debit without one is malformed.
				bad := credit(u, 100)
				bad.Target = target
				_, err = ApplyRelayAdjustment(db, admin.Id, bad)
				require.ErrorIs(t, err, ErrRelayInput)
			})
			t.Run("unreconciled_admin_and_immutable", func(t *testing.T) {
				u := newUser(common.RoleCommonUser)
				fund(u)
				_, err := ApplyRelayAdjustment(db, admin.Id, credit(u, 100))
				require.ErrorIs(t, err, ErrRelayFunding)
				require.Equal(t, int64(10000000), balance(u))
				require.NoError(t, InitializeRelayFunding(db, u.Id, 0, "fixture"))
				_, err = ApplyRelayAdjustment(db, u.Id, credit(u, 100))
				require.ErrorIs(t, err, ErrRelayQuota)
				_, err = ApplyRelayAdjustment(db, admin.Id, credit(admin, 100))
				require.ErrorIs(t, err, gorm.ErrRecordNotFound)
				big := credit(u, RelayAdjustmentMaxFen+1)
				_, err = ApplyRelayAdjustment(db, admin.Id, big)
				require.ErrorIs(t, err, ErrRelayInput)
				saved, err := ApplyRelayAdjustment(db, admin.Id, credit(u, 100))
				require.NoError(t, err)
				require.Error(t, db.Model(&saved).Update("reason", "rewrite").Error)
				require.Error(t, db.Delete(&saved).Error)
				// An untracked direct balance write is reported, never adjusted over.
				require.NoError(t, db.Model(&User{}).Where("id = ?", u.Id).Update("quota", gorm.Expr("quota + 1")).Error)
				view, err := ReadRelayFunding(db, u.Id)
				require.NoError(t, err)
				require.True(t, view.Ready)
				require.False(t, view.Consistent)
				_, err = ApplyRelayAdjustment(db, admin.Id, credit(u, 100))
				require.ErrorIs(t, err, ErrRelayFunding)
			})
			t.Run("concurrent_replay_and_competing_debits", func(t *testing.T) {
				u, _ := ready()
				grant := credit(u, 500)
				var wg sync.WaitGroup
				errs := make([]error, 10)
				for i := range errs {
					wg.Add(1)
					go func(i int) { defer wg.Done(); _, errs[i] = ApplyRelayAdjustment(db, admin.Id, grant) }(i)
				}
				wg.Wait()
				for _, err := range errs {
					require.NoError(t, err)
				}
				require.Equal(t, int64(15000000), balance(u))
				target := relayAdjustmentEntryID(grant.ID)
				debits := []RelayAdjustmentInput{
					{ID: newID("AJ"), UserID: u.Id, AmountFen: -300, Target: target, Reason: "race a", Evidence: evidence},
					{ID: newID("AJ"), UserID: u.Id, AmountFen: -300, Target: target, Reason: "race b", Evidence: evidence},
				}
				start := make(chan struct{})
				results := make([]error, 2)
				for i := range debits {
					wg.Add(1)
					go func(i int) { defer wg.Done(); <-start; _, results[i] = ApplyRelayAdjustment(db, admin.Id, debits[i]) }(i)
				}
				close(start)
				wg.Wait()
				require.NotEqual(t, results[0] == nil, results[1] == nil)
				for _, err := range results {
					if err != nil {
						require.ErrorIs(t, err, ErrRelayQuota)
					}
				}
				require.Equal(t, int64(12000000), balance(u))
				items, total, err := ListRelayAdjustments(db, u.Id, 1)
				require.NoError(t, err)
				require.EqualValues(t, 2, total)
				require.Len(t, items, 2)
			})
			t.Run("anomaly_report_is_read_only", func(t *testing.T) {
				u, o := ready()
				token := Token{UserId: u.Id, Key: newID("k"), Name: "fixture", Status: common.TokenStatusEnabled, RemainQuota: 1000000, ExpiredTime: -1}
				require.NoError(t, db.Create(&token).Error)
				intent := RelayWalletRequest{ID: fmt.Sprintf("20000000-0000-4000-8000-%012x", sequence), UserID: u.Id, TokenID: token.Id, PayloadHash: strings.Repeat("c", 64), PriceVersion: "fixture", PriceSnapshot: `{"fixture":true}`, Reserved: 1000000}
				_, err := ReserveRelayWallet(db, intent)
				require.NoError(t, err)
				_, err = AdvanceRelayWallet(db, intent.ID, u.Id, "submitted", 0)
				require.NoError(t, err)
				_, err = AdvanceRelayWallet(db, intent.ID, u.Id, "unknown", 0)
				require.NoError(t, err)
				now := time.Now()
				fresh, err := ReadRelayAnomalies(db, now)
				require.NoError(t, err)
				for _, kind := range RelayAnomalyKinds {
					_, ok := fresh.Counts[kind]
					require.True(t, ok, kind)
				}
				// A fresh unknown result is not yet an alert; 11 minutes later it is.
				later, err := ReadRelayAnomalies(db, now.Add(11*time.Minute))
				require.NoError(t, err)
				require.Equal(t, fresh.Counts["request_unknown_alert"]+1, later.Counts["request_unknown_alert"])
				overdue, err := ReadRelayAnomalies(db, now.Add(25*time.Hour))
				require.NoError(t, err)
				require.GreaterOrEqual(t, overdue.Counts["request_unknown_overdue"], int64(1))
				require.NoError(t, db.Exec("UPDATE adjust_test_relay_pay_orders SET credit = ? WHERE id = ?", "attention", o.ID).Error)
				report, err := ReadRelayAnomalies(db, now)
				require.NoError(t, err)
				require.Equal(t, fresh.Counts["payment_attention"]+1, report.Counts["payment_attention"])
				// The earlier untracked direct balance write is reported by user.
				require.GreaterOrEqual(t, report.Counts["funding_inconsistent"], int64(1))
				require.True(t, report.FundingComplete)
				found := false
				for _, item := range report.Items {
					found = found || (item.Kind == "payment_attention" && item.Ref == o.ID && item.UserID == u.Id)
				}
				require.True(t, found)
				var request RelayWalletRequest
				require.NoError(t, db.Where("id = ?", intent.ID).Take(&request).Error)
				require.Equal(t, "unknown", request.State)
				require.Equal(t, int64(9000000), balance(u))
			})
			t.Run("paid_token_cap_replenish", func(t *testing.T) {
				u, _ := ready()
				token := Token{UserId: u.Id, Key: newID("k"), Name: RelayPaidTokenPrefix + strings.Repeat("a", 32), Status: common.TokenStatusEnabled, RemainQuota: 1000000, ExpiredTime: -1}
				require.NoError(t, db.Create(&token).Error)
				remain := func() int {
					var got Token
					require.NoError(t, db.Where("id = ?", token.Id).Take(&got).Error)
					return got.RemainQuota
				}
				// Above a fifth of the cap: no change.
				state, err := ReplenishRelayPaidToken(db, u.Id, token.Id, 5000000)
				require.NoError(t, err)
				require.False(t, state.Replenished)
				require.Equal(t, 1000000, remain())
				// An in-flight hold already lowered remain_quota; replenish adds a delta
				// so settlement's later release still lands on a consistent cap.
				intent := RelayWalletRequest{ID: fmt.Sprintf("30000000-0000-4000-8000-%012x", sequence), UserID: u.Id, TokenID: token.Id, PayloadHash: strings.Repeat("c", 64), PriceVersion: "fixture", PriceSnapshot: `{"fixture":true}`, Reserved: 600000}
				_, err = ReserveRelayWallet(db, intent)
				require.NoError(t, err)
				require.Equal(t, 400000, remain())
				state, err = ReplenishRelayPaidToken(db, u.Id, token.Id, 5000000)
				require.NoError(t, err)
				require.True(t, state.Replenished)
				require.Equal(t, 5000000, remain())
				_, err = AdvanceRelayWallet(db, intent.ID, u.Id, "released", 0)
				require.NoError(t, err)
				require.Equal(t, 5600000, remain())
				require.Equal(t, int64(10000000), balance(u))
				// Wrong owner, non-paid names, disabled tokens or users are refused.
				other, _ := ready()
				_, err = ReplenishRelayPaidToken(db, other.Id, token.Id, 50000000)
				require.ErrorIs(t, err, gorm.ErrRecordNotFound)
				plain := Token{UserId: u.Id, Key: newID("k"), Name: "manual", Status: common.TokenStatusEnabled, RemainQuota: 0, ExpiredTime: -1}
				require.NoError(t, db.Create(&plain).Error)
				_, err = ReplenishRelayPaidToken(db, u.Id, plain.Id, 5000000)
				require.ErrorIs(t, err, ErrRelayState)
				require.NoError(t, db.Model(&Token{}).Where("id = ?", token.Id).Update("status", common.TokenStatusDisabled).Error)
				_, err = ReplenishRelayPaidToken(db, u.Id, token.Id, 50000000)
				require.ErrorIs(t, err, ErrRelayState)
				require.NoError(t, db.Model(&Token{}).Where("id = ?", token.Id).Update("status", common.TokenStatusEnabled).Error)
				require.NoError(t, db.Model(&User{}).Where("id = ?", u.Id).Update("status", common.UserStatusDisabled).Error)
				_, err = ReplenishRelayPaidToken(db, u.Id, token.Id, 50000000)
				require.ErrorIs(t, err, ErrRelayQuota)
				require.Equal(t, 5600000, remain())
				_, err = ReplenishRelayPaidToken(db, u.Id, token.Id, 0)
				require.ErrorIs(t, err, ErrRelayInput)
				// Revocation is idempotent, keeps the balance and blocks new holds.
				require.NoError(t, db.Model(&User{}).Where("id = ?", u.Id).Update("status", common.UserStatusEnabled).Error)
				for range 2 {
					require.NoError(t, RevokeRelayPaidToken(db, u.Id, token.Id))
				}
				require.ErrorIs(t, RevokeRelayPaidToken(db, u.Id, plain.Id), ErrRelayState)
				intent.ID = "40000000" + intent.ID[8:]
				_, err = ReserveRelayWallet(db, intent)
				require.ErrorIs(t, err, ErrRelayQuota)
				_, err = ReplenishRelayPaidToken(db, u.Id, token.Id, 50000000)
				require.ErrorIs(t, err, ErrRelayState)
				require.Equal(t, int64(10000000), balance(u))
			})
			t.Run("compensation_counts_as_loss", func(t *testing.T) {
				month := RelayRiskMonth(time.Now())
				before, err := ReadRelayRisk(db, month)
				require.NoError(t, err)
				u, _ := ready()
				grant := credit(u, 200)
				_, err = ApplyRelayAdjustment(db, admin.Id, grant)
				require.NoError(t, err)
				after, err := ReadRelayRisk(db, month)
				require.NoError(t, err)
				require.Equal(t, before.Compensation+2000000, after.Compensation)
				require.Equal(t, before.Loss+2000000, after.Loss)
				debit := RelayAdjustmentInput{ID: newID("AJ"), UserID: u.Id, AmountFen: -50, Target: relayAdjustmentEntryID(grant.ID), Reason: "partial reversal", Evidence: evidence}
				_, err = ApplyRelayAdjustment(db, admin.Id, debit)
				require.NoError(t, err)
				reversed, err := ReadRelayRisk(db, month)
				require.NoError(t, err)
				require.Equal(t, after.Reversed+500000, reversed.Reversed)
				require.Equal(t, after.Loss-500000, reversed.Loss)
			})
		})
	}
}
