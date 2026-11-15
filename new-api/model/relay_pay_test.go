package model

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"gorm.io/gorm/schema"
)

func relayPayEvidenceFor(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func TestRelayPayMatrix(t *testing.T) {
	oldRedis, oldBatch := common.RedisEnabled, common.BatchUpdateEnabled
	common.RedisEnabled, common.BatchUpdateEnabled = false, false
	t.Cleanup(func() { common.RedisEnabled, common.BatchUpdateEnabled = oldRedis, oldBatch })
	for _, dialect := range []string{"sqlite", "mysql", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			var driver gorm.Dialector
			switch dialect {
			case "sqlite":
				driver = sqlite.Open(filepath.Join(t.TempDir(), "pay.db") + "?_pragma=busy_timeout(10000)")
			case "mysql":
				require.NotEmpty(t, os.Getenv("TEST_MYSQL_DSN"))
				driver = mysql.Open(os.Getenv("TEST_MYSQL_DSN"))
			case "postgres":
				require.NotEmpty(t, os.Getenv("TEST_POSTGRES_DSN"))
				driver = postgres.Open(os.Getenv("TEST_POSTGRES_DSN"))
			}
			db, err := gorm.Open(driver, &gorm.Config{NamingStrategy: schema.NamingStrategy{TablePrefix: "pay_test_"}, Logger: logger.Default.LogMode(logger.Silent)})
			require.NoError(t, err)
			sqlDB, err := db.DB()
			require.NoError(t, err)
			sqlDB.SetMaxOpenConns(4)
			previous := common.MainDatabaseType()
			common.SetMainDatabaseType(common.DatabaseType(dialect))
			t.Cleanup(func() {
				require.NoError(t, db.Migrator().DropTable(&RelayPolicyVersion{}, &RelayAdjustment{}, &RelayFixedCost{}, &RelayRiskState{}, &RelayRiskEvent{}, &RelayWalletCost{}, &RelayWalletCostSnapshot{}, &RelayRefundEvent{}, &RelayRefund{}, &RelayFundingEntry{}, &RelayFundingReady{}, &RelayPayEvent{}, &RelayPayOrder{}, &RelayWalletLogPending{}, &RelayWalletLogMarker{},
					&RelayWalletReceipt{}, &RelayWalletEvent{}, &RelayWalletRequest{}, &RelayWalletLock{}, &Token{}, &User{}))
				require.NoError(t, sqlDB.Close())
				common.SetMainDatabaseType(previous)
			})
			require.NoError(t, db.AutoMigrate(&User{}, &Token{}))
			require.NoError(t, MigrateRelayWallet(db))
			require.NoError(t, MigrateRelayWallet(db), "repeat migration is harmless")
			sequence := 0
			newUser := func(t *testing.T, status int) User {
				sequence++
				user := User{Username: fmt.Sprintf("pay%d", sequence), AffCode: fmt.Sprintf("pay%d", sequence), Password: "fixture-only",
					Role: common.RoleCommonUser, Status: status, Quota: 0}
				require.NoError(t, db.Create(&user).Error)
				return user
			}
			newOrder := func(t *testing.T, user User, yuan int64) RelayPayOrder {
				sequence++
				order, err := CreateRelayPayOrder(db, RelayPayOrder{ID: fmt.Sprintf("RP%030x", sequence), UserID: user.Id, AmountFen: yuan * 100,
					Quota: yuan * 1000000, Currency: "CNY", Channel: "alipay", MerchantApp: "2021000000000001", ConfigVersion: "fixture-v1",
					RateVersion: "cny-1e6-v1", ExpiresAt: time.Now().Add(15 * time.Minute).Unix()}, 3)
				require.NoError(t, err)
				return order
			}
			paid := func(order RelayPayOrder, trade string) RelayPayFact {
				return RelayPayFact{OrderID: order.ID, Channel: "alipay", App: "2021000000000001", TradeNo: trade, AmountFen: order.AmountFen,
					Currency: "CNY", Status: "paid", Evidence: relayPayEvidenceFor(trade)}
			}
			quota := func(t *testing.T, user User) int {
				var current User
				require.NoError(t, db.First(&current, user.Id).Error)
				return current.Quota
			}

			t.Run("duplicate_and_concurrent_success_credits_once", func(t *testing.T) {
				user := newUser(t, common.UserStatusEnabled)
				order := newOrder(t, user, 30)
				var wg sync.WaitGroup
				errs := make([]error, 20)
				start := make(chan struct{})
				for i := range errs {
					wg.Add(1)
					go func(i int) { defer wg.Done(); <-start; _, errs[i] = ApplyRelayPayFact(db, paid(order, "T"+order.ID)) }(i)
				}
				close(start)
				wg.Wait()
				for _, err := range errs {
					require.NoError(t, err)
				}
				assert.Equal(t, 30000000, quota(t, user))
				var events []RelayPayEvent
				require.NoError(t, db.Where("order_id = ?", order.ID).Order("id").Find(&events).Error)
				require.Len(t, events, 3)
				assert.Equal(t, []string{"created", "paid", "credited"}, []string{events[0].Kind, events[1].Kind, events[2].Kind})
				assert.EqualValues(t, 30000000, events[2].QuotaDelta)
				_, err := ApplyRelayPayFact(db, paid(order, "TOTHER"))
				assert.ErrorIs(t, err, ErrRelayConflict, "a second trade number for a paid order is refused")
			})

			t.Run("mismatched_facts_never_credit", func(t *testing.T) {
				user := newUser(t, common.UserStatusEnabled)
				order := newOrder(t, user, 10)
				for _, change := range []func(*RelayPayFact){
					func(f *RelayPayFact) { f.AmountFen = 1 },
					func(f *RelayPayFact) { f.AmountFen = 100000 },
					func(f *RelayPayFact) { f.Currency = "USD" },
					func(f *RelayPayFact) { f.App = "2021000000000002" },
					func(f *RelayPayFact) { f.Channel = "mock" },
				} {
					fact := paid(order, "TMISMATCH")
					change(&fact)
					_, err := ApplyRelayPayFact(db, fact)
					assert.ErrorIs(t, err, ErrRelayPayMismatch)
				}
				for _, change := range []func(*RelayPayFact){
					func(f *RelayPayFact) { f.Evidence = "x" }, func(f *RelayPayFact) { f.TradeNo = "" },
					func(f *RelayPayFact) { f.Status = "refunded" }, func(f *RelayPayFact) { f.OrderID = "RP" },
				} {
					fact := paid(order, "TINVALID")
					change(&fact)
					_, err := ApplyRelayPayFact(db, fact)
					assert.ErrorIs(t, err, ErrRelayInput)
				}
				assert.Zero(t, quota(t, user))
				var current RelayPayOrder
				require.NoError(t, db.Where("id = ?", order.ID).Take(&current).Error)
				assert.Equal(t, "pending", current.State)
			})

			t.Run("one_trade_number_cannot_pay_two_orders", func(t *testing.T) {
				user := newUser(t, common.UserStatusEnabled)
				first, second := newOrder(t, user, 10), newOrder(t, user, 10)
				_, err := ApplyRelayPayFact(db, paid(first, "TSHARED"))
				require.NoError(t, err)
				_, err = ApplyRelayPayFact(db, paid(second, "TSHARED"))
				require.Error(t, err)
				assert.Equal(t, 10000000, quota(t, user), "the rolled-back credit is not applied")
			})

			t.Run("late_payment_after_close_is_credited", func(t *testing.T) {
				user := newUser(t, common.UserStatusEnabled)
				order := newOrder(t, user, 50)
				closed := RelayPayFact{OrderID: order.ID, Channel: "alipay", App: "2021000000000001", Status: "closed", Evidence: relayPayEvidenceFor("closed")}
				_, err := ApplyRelayPayFact(db, closed)
				require.NoError(t, err)
				applied, err := ApplyRelayPayFact(db, paid(order, "TLATE"))
				require.NoError(t, err)
				assert.Equal(t, "paid", applied.State)
				assert.Equal(t, 50000000, quota(t, user))
				_, err = ApplyRelayPayFact(db, closed)
				require.NoError(t, err)
				require.NoError(t, db.Where("id = ?", order.ID).Take(&applied).Error)
				assert.Equal(t, "paid", applied.State, "a stale close never reopens a paid order")
			})

			t.Run("unavailable_account_keeps_payment_for_review", func(t *testing.T) {
				user := newUser(t, common.UserStatusEnabled)
				order := newOrder(t, user, 10)
				require.NoError(t, db.Model(&User{}).Where("id = ?", user.Id).Update("status", common.UserStatusDisabled).Error)
				applied, err := ApplyRelayPayFact(db, paid(order, "TREVIEW"))
				require.NoError(t, err)
				assert.Equal(t, "paid", applied.State)
				assert.Equal(t, "attention", applied.Credit)
				assert.Zero(t, quota(t, user))
			})

			t.Run("creation_rules_and_immutable_snapshot", func(t *testing.T) {
				user := newUser(t, common.UserStatusEnabled)
				for range 3 {
					newOrder(t, user, 10)
				}
				_, err := CreateRelayPayOrder(db, RelayPayOrder{ID: "RP" + fmt.Sprintf("%030x", 999999), UserID: user.Id, AmountFen: 1000, Quota: 10000000,
					Currency: "CNY", Channel: "alipay", MerchantApp: "app", ConfigVersion: "v1", RateVersion: "r", ExpiresAt: time.Now().Add(time.Minute).Unix()}, 3)
				assert.ErrorIs(t, err, ErrRelayPayLimit)
				disabled := newUser(t, common.UserStatusDisabled)
				_, err = CreateRelayPayOrder(db, RelayPayOrder{ID: "RP" + fmt.Sprintf("%030x", 999998), UserID: disabled.Id, AmountFen: 1000, Quota: 10000000,
					Currency: "CNY", Channel: "alipay", MerchantApp: "app", ConfigVersion: "v1", RateVersion: "r", ExpiresAt: time.Now().Add(time.Minute).Unix()}, 3)
				assert.ErrorIs(t, err, ErrRelayQuota)
				other := newUser(t, common.UserStatusEnabled)
				order := newOrder(t, other, 100)
				assert.Error(t, db.Model(&RelayPayOrder{}).Where("id = ?", order.ID).Updates(map[string]any{"amount_fen": 1}).Error)
				assert.Error(t, db.Model(&RelayPayOrder{}).Where("id = ?", order.ID).Updates(map[string]any{"user_id": user.Id}).Error)
				assert.Error(t, db.Delete(&order).Error)
				_, err = GetRelayPayOrder(db, user.Id, order.ID)
				assert.True(t, errors.Is(err, gorm.ErrRecordNotFound), "another user's order is invisible")
				orders, total, err := ListRelayPayOrders(db, other.Id, 1)
				require.NoError(t, err)
				assert.EqualValues(t, 1, total)
				assert.Equal(t, order.ID, orders[0].ID)
			})

			t.Run("bounded_query_backoff", func(t *testing.T) {
				user := newUser(t, common.UserStatusEnabled)
				order := newOrder(t, user, 10)
				require.NoError(t, db.Model(&RelayPayOrder{}).Where("id = ?", order.ID).Updates(map[string]any{"next_query_at": 0}).Error)
				due, err := DueRelayPayOrders(db, 100)
				require.NoError(t, err)
				found := false
				for _, item := range due {
					found = found || item.ID == order.ID
				}
				assert.True(t, found)
				require.NoError(t, db.Where("id = ?", order.ID).Take(&order).Error)
				require.NoError(t, ScheduleRelayPayQuery(db, order))
				require.NoError(t, db.Where("id = ?", order.ID).Take(&order).Error)
				assert.Equal(t, 1, order.QueryAttempts)
				assert.Greater(t, order.NextQueryAt, time.Now().Unix()+30)
			})
		})
	}
}
