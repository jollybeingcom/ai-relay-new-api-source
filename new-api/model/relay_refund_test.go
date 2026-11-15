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

func TestRelayRefundMatrix(t *testing.T) {
	oldRedis, oldBatch := common.RedisEnabled, common.BatchUpdateEnabled
	common.RedisEnabled, common.BatchUpdateEnabled = false, false
	t.Cleanup(func() { common.RedisEnabled, common.BatchUpdateEnabled = oldRedis, oldBatch })
	for _, dialect := range []string{"sqlite", "mysql", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			var driver gorm.Dialector
			switch dialect {
			case "sqlite":
				driver = sqlite.Open(filepath.Join(t.TempDir(), "refund.db") + "?_pragma=busy_timeout(10000)")
			case "mysql":
				require.NotEmpty(t, os.Getenv("TEST_MYSQL_DSN"))
				driver = mysql.Open(os.Getenv("TEST_MYSQL_DSN"))
			case "postgres":
				require.NotEmpty(t, os.Getenv("TEST_POSTGRES_DSN"))
				driver = postgres.Open(os.Getenv("TEST_POSTGRES_DSN"))
			}
			db, err := gorm.Open(driver, &gorm.Config{NamingStrategy: schema.NamingStrategy{TablePrefix: "refund_test_"}, Logger: logger.Default.LogMode(logger.Silent)})
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
				name := newID("u")
				u := User{Username: name, AffCode: fmt.Sprintf("r%d", sequence), Password: "fixture-only", Role: role, Status: common.UserStatusEnabled}
				require.NoError(t, db.Create(&u).Error)
				return u
			}
			admin := newUser(common.RoleAdminUser)
			fund := func(u User) RelayPayOrder {
				o, err := CreateRelayPayOrder(db, RelayPayOrder{ID: newID("RP"), UserID: u.Id, AmountFen: 1000, Quota: 10000000, Currency: "CNY", Channel: "mock", MerchantApp: "fixture", ConfigVersion: "fixture", RateVersion: "cny-1e6-v1", ExpiresAt: time.Now().Unix() + 900}, 3)
				require.NoError(t, err)
				o, err = ApplyRelayPayFact(db, RelayPayFact{OrderID: o.ID, Channel: o.Channel, App: o.MerchantApp, TradeNo: "T" + o.ID, AmountFen: o.AmountFen, Currency: "CNY", Status: "paid", Evidence: strings.Repeat("a", 64)})
				require.NoError(t, err)
				return o
			}
			balance := func(u User) int {
				var got User
				require.NoError(t, db.Where("id = ?", u.Id).Take(&got).Error)
				return got.Quota
			}
			fact := func(o RelayPayOrder, r RelayRefund, state string) RelayRefundFact {
				return RelayRefundFact{RefundID: r.ID, OrderID: o.ID, Channel: o.Channel, App: o.MerchantApp, TradeNo: *o.TradeNo, AmountFen: r.AmountFen, Status: state, Evidence: strings.Repeat("b", 64)}
			}
			request := func(u User, o RelayPayOrder, fen int64) RelayRefund {
				r, err := RequestRelayRefund(db, u.Id, newID("RF"), o.ID, fen)
				require.NoError(t, err)
				return r
			}
			approve := func(r RelayRefund) {
				require.NoError(t, ReviewRelayRefund(db, r.ID, admin.Id, "reviewed fixture", true))
			}
			claim := func(r RelayRefund) {
				ok, err := ClaimRelayRefund(db, r.ID)
				require.NoError(t, err)
				require.True(t, ok)
			}
			t.Run("audited_funding_preview_stale_apply_and_replay", func(t *testing.T) {
				u := newUser(common.RoleCommonUser)
				fund(u)
				require.NoError(t, db.Model(&User{}).Where("id = ?", u.Id).Update("quota", 12000000).Error)
				input := RelayFundingReview{UserID: u.Id, OpeningQuota: 2000000, Reason: "verified adjustment predates first payment", Evidence: strings.Repeat("c", 64)}
				_, err := ReviewRelayFunding(db, u.Id, input, "")
				require.Error(t, err)
				bad := input
				bad.OpeningQuota = 1000000
				_, err = ReviewRelayFunding(db, admin.Id, bad, "")
				require.ErrorIs(t, err, ErrRelayFunding)
				plan, err := ReviewRelayFunding(db, admin.Id, input, "")
				require.NoError(t, err)
				require.False(t, plan.Applied)
				require.Equal(t, int64(12000000), plan.Available)
				var count int64
				require.NoError(t, db.Model(&RelayFundingReady{}).Where("user_id = ?", u.Id).Count(&count).Error)
				require.Zero(t, count)
				require.NoError(t, db.Model(&RelayFundingEntry{}).Where("user_id = ? AND kind = ?", u.Id, "adjustment").Count(&count).Error)
				require.Zero(t, count)
				fund(u)
				_, err = ReviewRelayFunding(db, admin.Id, input, plan.PlanHash)
				require.ErrorIs(t, err, ErrRelayConflict)
				plan, err = ReviewRelayFunding(db, admin.Id, input, "")
				require.NoError(t, err)
				before := balance(u)
				for range 2 {
					applied, err := ReviewRelayFunding(db, admin.Id, input, plan.PlanHash)
					require.NoError(t, err)
					require.True(t, applied.Applied)
				}
				require.Equal(t, before, balance(u))
				var marker RelayFundingReady
				require.NoError(t, db.Where("user_id = ?", u.Id).Take(&marker).Error)
				require.Equal(t, admin.Id, marker.ReviewerID)
				require.Equal(t, input.Evidence, marker.Evidence)
				require.Equal(t, plan.PlanHash, marker.PlanHash)
				bad = input
				bad.OpeningQuota++
				_, err = ReviewRelayFunding(db, admin.Id, bad, plan.PlanHash)
				require.ErrorIs(t, err, ErrRelayConflict)
				require.Error(t, db.Model(&RelayFundingReady{}).Where("user_id = ?", u.Id).Update("reason", "changed").Error)
			})
			t.Run("partial_duplicate_unknown_success", func(t *testing.T) {
				u := newUser(common.RoleCommonUser)
				o := fund(u)
				require.NoError(t, InitializeRelayFunding(db, u.Id, 0, "fixture opening"))
				r := request(u, o, 300)
				same, err := RequestRelayRefund(db, u.Id, r.ID, o.ID, 300)
				require.NoError(t, err)
				require.Equal(t, r.ID, same.ID)
				_, err = RequestRelayRefund(db, u.Id, newID("RF"), o.ID, 100)
				require.ErrorIs(t, err, ErrRelayConflict)
				approve(r)
				approve(r)
				require.Equal(t, 7000000, balance(u))
				claim(r)
				ok, err := ClaimRelayRefund(db, r.ID)
				require.NoError(t, err)
				require.False(t, ok)
				require.NoError(t, ApplyRelayRefundFact(db, fact(o, r, "unknown")))
				require.NoError(t, ApplyRelayRefundFact(db, fact(o, r, "unknown")))
				require.NoError(t, pool.Close())
				db, err = gorm.Open(driver, &gorm.Config{NamingStrategy: schema.NamingStrategy{TablePrefix: "refund_test_"}, Logger: logger.Default.LogMode(logger.Silent)})
				require.NoError(t, err)
				pool, err = db.DB()
				require.NoError(t, err)
				pool.SetMaxOpenConns(4)
				require.NoError(t, MigrateRelayWallet(db))
				require.Equal(t, 7000000, balance(u))
				require.NoError(t, ApplyRelayRefundFact(db, fact(o, r, "succeeded")))
				require.NoError(t, ApplyRelayRefundFact(db, fact(o, r, "succeeded")))
				require.ErrorIs(t, ApplyRelayRefundFact(db, fact(o, r, "failed_released")), ErrRelayState)
				_, err = RequestRelayRefund(db, u.Id, newID("RF"), o.ID, 701)
				require.ErrorIs(t, err, ErrRelayQuota)
				r2 := request(u, o, 700)
				approve(r2)
				claim(r2)
				require.NoError(t, ApplyRelayRefundFact(db, fact(o, r2, "succeeded")))
				require.Zero(t, balance(u))
				var events int64
				require.NoError(t, db.Model(&RelayRefundEvent{}).Where("refund_id = ?", r.ID).Count(&events).Error)
				require.EqualValues(t, 5, events)
			})
			t.Run("explicit_failure_releases_once", func(t *testing.T) {
				u := newUser(common.RoleCommonUser)
				o := fund(u)
				require.NoError(t, InitializeRelayFunding(db, u.Id, 0, "fixture"))
				r := request(u, o, 1000)
				approve(r)
				claim(r)
				require.NoError(t, ApplyRelayRefundFact(db, fact(o, r, "failed_released")))
				require.NoError(t, ApplyRelayRefundFact(db, fact(o, r, "failed_released")))
				require.Equal(t, 10000000, balance(u))
			})
			t.Run("ownership_review_and_snapshot", func(t *testing.T) {
				u := newUser(common.RoleCommonUser)
				other := newUser(common.RoleCommonUser)
				o := fund(u)
				require.NoError(t, InitializeRelayFunding(db, u.Id, 0, "fixture"))
				_, err := RequestRelayRefund(db, other.Id, newID("RF"), o.ID, 100)
				require.Error(t, err)
				r := request(u, o, 100)
				_, err = GetRelayRefund(db, other.Id, r.ID)
				require.ErrorIs(t, err, gorm.ErrRecordNotFound)
				require.Error(t, ReviewRelayRefund(db, r.ID, other.Id, "not admin", true))
				require.Error(t, ReviewRelayRefund(db, r.ID, admin.Id, "", true))
				require.Error(t, db.Model(&RelayRefund{}).Where("id = ?", r.ID).Updates(map[string]any{"quota": 1}).Error)
				approve(r)
				claim(r)
				f := fact(o, r, "succeeded")
				f.AmountFen++
				require.ErrorIs(t, ApplyRelayRefundFact(db, f), ErrRelayPayMismatch)
				require.Equal(t, 9000000, balance(u))
			})
			t.Run("opening_adjustment_and_untracked_changes", func(t *testing.T) {
				u := newUser(common.RoleCommonUser)
				require.NoError(t, db.Model(&User{}).Where("id = ?", u.Id).Update("quota", 1000000).Error)
				o := fund(u)
				require.ErrorIs(t, InitializeRelayFunding(db, u.Id, 0, "wrong"), ErrRelayFunding)
				require.NoError(t, InitializeRelayFunding(db, u.Id, 1000000, "audited test adjustment"))
				require.ErrorIs(t, InitializeRelayFunding(db, u.Id, 1000000, "repeat"), ErrRelayConflict)
				r := request(u, o, 1000)
				approve(r)
				claim(r)
				require.NoError(t, ApplyRelayRefundFact(db, fact(o, r, "succeeded")))
				require.Equal(t, 1000000, balance(u))
				_, err := RequestRelayRefund(db, u.Id, newID("RF"), o.ID, 1)
				require.ErrorIs(t, err, ErrRelayQuota)
				o2 := fund(u)
				require.NoError(t, db.Model(&User{}).Where("id = ?", u.Id).Update("quota", gorm.Expr("quota - 1")).Error)
				_, err = RequestRelayRefund(db, u.Id, newID("RF"), o2.ID, 1)
				require.ErrorIs(t, err, ErrRelayFunding)
			})
			t.Run("FIFO_after_settlement_and_pending_reservation", func(t *testing.T) {
				u := newUser(common.RoleCommonUser)
				first, second := fund(u), fund(u)
				require.NoError(t, InitializeRelayFunding(db, u.Id, 0, "fixture"))
				token := Token{UserId: u.Id, Key: newID("k"), Name: "fixture", Status: common.TokenStatusEnabled, RemainQuota: 20000000, ExpiredTime: -1}
				require.NoError(t, db.Create(&token).Error)
				intent := RelayWalletRequest{ID: fmt.Sprintf("00000000-0000-4000-8000-%012x", sequence), UserID: u.Id, TokenID: token.Id, PayloadHash: strings.Repeat("c", 64), PriceVersion: "fixture", PriceSnapshot: `{"fixture":true}`, Reserved: 12000000}
				_, err := ReserveRelayWallet(db, intent)
				require.NoError(t, err)
				_, err = AdvanceRelayWallet(db, intent.ID, u.Id, "submitted", 0)
				require.NoError(t, err)
				_, err = AdvanceRelayWallet(db, intent.ID, u.Id, "settled", 12000000)
				require.NoError(t, err)
				intent.ID = "10000000" + intent.ID[8:]
				intent.Reserved = 2000000
				_, err = ReserveRelayWallet(db, intent)
				require.NoError(t, err)
				_, err = RequestRelayRefund(db, u.Id, newID("RF"), first.ID, 1)
				require.ErrorIs(t, err, ErrRelayQuota)
				_, err = RequestRelayRefund(db, u.Id, newID("RF"), second.ID, 601)
				require.ErrorIs(t, err, ErrRelayQuota)
				r := request(u, second, 600)
				approve(r)
				claim(r)
				require.NoError(t, ApplyRelayRefundFact(db, fact(second, r, "succeeded")))
				require.Zero(t, balance(u))
			})
			t.Run("concurrent_approvals_freeze_once_and_rejection", func(t *testing.T) {
				u := newUser(common.RoleCommonUser)
				o := fund(u)
				require.NoError(t, InitializeRelayFunding(db, u.Id, 0, "fixture"))
				r := request(u, o, 100)
				require.NoError(t, ReviewRelayRefund(db, r.ID, admin.Id, "rejected fixture", false))
				require.NoError(t, ReviewRelayRefund(db, r.ID, admin.Id, "replay", false))
				require.Equal(t, 10000000, balance(u))
				r = request(u, o, 1000)
				var wg sync.WaitGroup
				errs := make([]error, 12)
				for i := range errs {
					wg.Add(1)
					go func(i int) { defer wg.Done(); errs[i] = ReviewRelayRefund(db, r.ID, admin.Id, "concurrent", true) }(i)
				}
				wg.Wait()
				for _, err := range errs {
					require.NoError(t, err)
				}
				require.Zero(t, balance(u))
				var count int64
				require.NoError(t, db.Model(&RelayRefundEvent{}).Where("refund_id = ? AND kind = ?", r.ID, "approved_frozen").Count(&count).Error)
				require.EqualValues(t, 1, count)
			})
			t.Run("reserve_races_refund_without_double_spend", func(t *testing.T) {
				u := newUser(common.RoleCommonUser)
				o := fund(u)
				require.NoError(t, InitializeRelayFunding(db, u.Id, 0, "fixture"))
				r := request(u, o, 1000)
				token := Token{UserId: u.Id, Key: newID("k"), Name: "fixture", Status: common.TokenStatusEnabled, RemainQuota: 10000000, ExpiredTime: -1}
				require.NoError(t, db.Create(&token).Error)
				intent := RelayWalletRequest{ID: fmt.Sprintf("00000000-0000-4000-8000-%012x", sequence), UserID: u.Id, TokenID: token.Id, PayloadHash: strings.Repeat("c", 64), PriceVersion: "fixture", PriceSnapshot: `{"fixture":true}`, Reserved: 10000000}
				var errs [2]error
				var wg sync.WaitGroup
				start := make(chan struct{})
				wg.Add(2)
				go func() { defer wg.Done(); <-start; errs[0] = ReviewRelayRefund(db, r.ID, admin.Id, "race", true) }()
				go func() { defer wg.Done(); <-start; _, errs[1] = ReserveRelayWallet(db, intent) }()
				close(start)
				wg.Wait()
				require.NotEqual(t, errs[0] == nil, errs[1] == nil)
				require.Zero(t, balance(u))
				if errs[0] != nil {
					require.ErrorIs(t, errs[0], ErrRelayQuota)
				}
				if errs[1] != nil {
					require.ErrorIs(t, errs[1], ErrRelayQuota)
				}
			})
		})
	}
}
