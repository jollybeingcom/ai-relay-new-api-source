package service

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"gorm.io/gorm/schema"
)

func TestRelayRefundAlipay(t *testing.T) {
	keys := newRelayPayKeys(t)
	trade := "T12345"
	order := model.RelayPayOrder{ID: "RP" + strings.Repeat("a", 30), UserID: 1, AmountFen: 1000, Channel: "alipay", MerchantApp: "2021000000000001", ConfigVersion: "fixture-v1", TradeNo: &trade}
	refund := model.RelayRefund{ID: "RF" + strings.Repeat("b", 30), OrderID: order.ID, UserID: 1, AmountFen: 300, Quota: 3000000}
	base := `"trade_no":"T12345","out_trade_no":"` + order.ID + `","out_request_no":"` + refund.ID + `"`
	success := `{"code":"10000",` + base + `,"refund_fee":"3.00","refund_amount":"3.00","refund_status":"REFUND_SUCCESS","fund_change":"Y"}`
	failure := `{"code":"40004","sub_code":"ACQ.REFUND_AMT_NOT_EQUAL_TOTAL"}`
	cases := []struct {
		name                               string
		query                              bool
		payload, state                     string
		invalid, wrongKey, tamper, timeout bool
	}{
		{name: "direct_success_requires_query", payload: success, state: "unknown"},
		{name: "query_success", query: true, payload: success, state: "succeeded"},
		{name: "verified_initial_failure", payload: failure, state: "failed_released"},
		{name: "failure_for_other_order", payload: `{"code":"40004","sub_code":"ACQ.REFUND_AMT_NOT_EQUAL_TOTAL","out_trade_no":"OTHER"}`, invalid: true},
		{name: "query_failure_is_not_release", query: true, payload: failure, state: "unknown"},
		{name: "unknown_error", payload: `{"code":"40004","sub_code":"ACQ.SYSTEM_ERROR"}`, state: "unknown"},
		{name: "trade_not_found", payload: `{"code":"40004","sub_code":"ACQ.TRADE_NOT_EXIST"}`, state: "unknown"},
		{name: "missing_status", query: true, payload: strings.Replace(success, `"REFUND_SUCCESS"`, `""`, 1), state: "unknown"},
		{name: "missing_refund_id", query: true, payload: strings.Replace(success, refund.ID, "", 1), state: "unknown"},
		{name: "wrong_refund_id", query: true, payload: strings.Replace(success, refund.ID, "RF"+strings.Repeat("c", 30), 1), invalid: true},
		{name: "wrong_order", query: true, payload: strings.Replace(success, order.ID, "RP"+strings.Repeat("c", 30), 1), invalid: true},
		{name: "wrong_trade", query: true, payload: strings.Replace(success, trade, "OTHER", 1), invalid: true},
		{name: "signed_wrong_amount", query: true, payload: strings.ReplaceAll(success, "3.00", "4.00"), invalid: true},
		{name: "forged_success", query: true, payload: success, wrongKey: true, invalid: true},
		{name: "forged_failure", payload: failure, wrongKey: true, invalid: true},
		{name: "tampered_amount", query: true, payload: success, tamper: true, invalid: true},
		{name: "duplicate_signed_field", query: true, payload: strings.Replace(success, `"refund_amount":"3.00"`, `"refund_amount":"3.00","refund_amount":"3.00"`, 1), invalid: true},
		{name: "timeout", timeout: true, invalid: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			transport := relayPayRoundTrip(func(req *http.Request) (*http.Response, error) {
				calls++
				raw, err := io.ReadAll(req.Body)
				require.NoError(t, err)
				values, err := url.ParseQuery(string(raw))
				require.NoError(t, err)
				method := "alipay.trade.refund"
				root := "alipay_trade_refund_response"
				if tc.query {
					method = "alipay.trade.fastpay.refund.query"
					root = "alipay_trade_fastpay_refund_query_response"
				}
				require.Equal(t, method, values.Get("method"))
				var biz map[string]string
				require.NoError(t, json.Unmarshal([]byte(values.Get("biz_content")), &biz))
				require.Equal(t, refund.ID, biz["out_request_no"])
				require.Equal(t, order.ID, biz["out_trade_no"])
				require.Equal(t, trade, biz["trade_no"])
				if !tc.query {
					require.Equal(t, "3.00", biz["refund_amount"])
				}
				sig, err := base64.StdEncoding.DecodeString(values.Get("sign"))
				require.NoError(t, err)
				digest := sha256.Sum256([]byte(relayPayCanonicalForm(values, true)))
				require.NoError(t, rsa.VerifyPKCS1v15(&keys.app.PublicKey, crypto.SHA256, digest[:], sig))
				if tc.timeout {
					return nil, context.DeadlineExceeded
				}
				key := keys.alipay
				if tc.wrongKey {
					key = keys.app
				}
				body := `{"` + root + `":` + tc.payload + `,"sign":"` + relaySignJSON(t, key, tc.payload) + `"}`
				if tc.tamper {
					body = strings.ReplaceAll(body, "3.00", "4.00")
				}
				return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, nil
			})
			channel := relayPayAlipayFixture(t, keys, transport)
			var fact model.RelayRefundFact
			var err error
			if tc.query {
				fact, err = channel.QueryRefund(context.Background(), order, refund)
			} else {
				fact, err = channel.Refund(context.Background(), order, refund)
			}
			require.Equal(t, 1, calls)
			if tc.invalid {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.state, fact.Status)
			require.Equal(t, refund.ID, fact.RefundID)
			require.Len(t, fact.Evidence, 64)
		})
	}
}

func TestRelayRefundRecoveryMatrix(t *testing.T) {
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
			db, err := gorm.Open(driver, &gorm.Config{NamingStrategy: schema.NamingStrategy{TablePrefix: "refund_recovery_"}, Logger: logger.Default.LogMode(logger.Silent)})
			require.NoError(t, err)
			pool, err := db.DB()
			require.NoError(t, err)
			pool.SetMaxOpenConns(4)
			previous := common.MainDatabaseType()
			common.SetMainDatabaseType(common.DatabaseType(dialect))
			t.Cleanup(func() {
				require.NoError(t, db.Migrator().DropTable(&model.RelayPolicyVersion{}, &model.RelayAdjustment{}, &model.RelayFixedCost{}, &model.RelayRiskState{}, &model.RelayRiskEvent{}, &model.RelayWalletCost{}, &model.RelayWalletCostSnapshot{}, &model.RelayRefundEvent{}, &model.RelayRefund{}, &model.RelayFundingEntry{}, &model.RelayFundingReady{}, &model.RelayPayEvent{}, &model.RelayPayOrder{}, &model.RelayWalletLogPending{}, &model.RelayWalletLogMarker{}, &model.RelayWalletReceipt{}, &model.RelayWalletEvent{}, &model.RelayWalletRequest{}, &model.RelayWalletLock{}, &model.User{}))
				require.NoError(t, pool.Close())
				common.SetMainDatabaseType(previous)
			})
			require.NoError(t, db.AutoMigrate(&model.User{}))
			require.NoError(t, model.MigrateRelayWallet(db))
			admin := model.User{Username: "admin", AffCode: "admin", Role: common.RoleAdminUser, Status: common.UserStatusEnabled}
			require.NoError(t, db.Create(&admin).Error)
			sequence := 0
			setup := func() (model.User, model.RelayRefund) {
				sequence++
				u := model.User{Username: fmt.Sprint("user", sequence), AffCode: fmt.Sprint("user", sequence), Role: common.RoleCommonUser, Status: common.UserStatusEnabled}
				require.NoError(t, db.Create(&u).Error)
				o, err := model.CreateRelayPayOrder(db, model.RelayPayOrder{ID: fmt.Sprintf("RP%030x", sequence), UserID: u.Id, AmountFen: 1000, Quota: 10000000, Currency: "CNY", Channel: "mock", MerchantApp: "mock-local", ConfigVersion: "mock-v1", RateVersion: "cny-1e6-v1", ExpiresAt: time.Now().Unix() + 900}, 3)
				require.NoError(t, err)
				_, err = model.ApplyRelayPayFact(db, model.RelayPayFact{OrderID: o.ID, Channel: o.Channel, App: o.MerchantApp, TradeNo: "T" + o.ID, AmountFen: 1000, Currency: "CNY", Status: "paid", Evidence: strings.Repeat("a", 64)})
				require.NoError(t, err)
				require.NoError(t, model.InitializeRelayFunding(db, u.Id, 0, "fixture"))
				r, err := model.RequestRelayRefund(db, u.Id, fmt.Sprintf("RF%030x", sequence), o.ID, 1000)
				require.NoError(t, err)
				require.NoError(t, model.ReviewRelayRefund(db, r.ID, admin.Id, "fixture", true))
				return u, r
			}
			get := func(r model.RelayRefund) model.RelayRefund {
				var got model.RelayRefund
				require.NoError(t, db.Where("id = ?", r.ID).Take(&got).Error)
				return got
			}
			balance := func(u model.User) int {
				var got model.User
				require.NoError(t, db.Where("id = ?", u.Id).Take(&got).Error)
				return got.Quota
			}
			ctx := context.Background()
			t.Run("timeout_reopen_query_only", func(t *testing.T) {
				u, r := setup()
				t.Setenv("RELAY_REFUND_MOCK_RESULT", "timeout")
				t.Setenv("RELAY_REFUND_MOCK_QUERY_RESULT", "success")
				_, err := syncRelayRefunds(ctx, db, relayPayMock{}, 50)
				require.Error(t, err)
				require.Equal(t, "unknown", get(r).State)
				require.Zero(t, balance(u))
				n, err := syncRelayRefunds(ctx, db, relayPayMock{}, 50)
				require.NoError(t, err)
				require.Zero(t, n)
				require.NoError(t, pool.Close())
				db, err = gorm.Open(driver, &gorm.Config{NamingStrategy: schema.NamingStrategy{TablePrefix: "refund_recovery_"}, Logger: logger.Default.LogMode(logger.Silent)})
				require.NoError(t, err)
				pool, err = db.DB()
				require.NoError(t, err)
				pool.SetMaxOpenConns(4)
				require.NoError(t, db.Model(&model.RelayRefund{}).Where("id = ?", r.ID).Updates(map[string]any{"next_query_at": 0}).Error)
				_, err = syncRelayRefunds(ctx, db, relayPayMock{}, 50)
				require.NoError(t, err)
				require.Equal(t, "succeeded", get(r).State)
				require.Zero(t, balance(u))
			})
			t.Run("failure_releases_once", func(t *testing.T) {
				u, r := setup()
				t.Setenv("RELAY_REFUND_MOCK_RESULT", "failure")
				_, err := syncRelayRefunds(ctx, db, relayPayMock{}, 50)
				require.NoError(t, err)
				require.Equal(t, "failed_released", get(r).State)
				require.Equal(t, 10000000, balance(u))
				_, err = syncRelayRefunds(ctx, db, relayPayMock{}, 50)
				require.NoError(t, err)
				require.Equal(t, 10000000, balance(u))
			})
			t.Run("crash_backoff_review_and_concurrent_claim", func(t *testing.T) {
				u, r := setup()
				ok, err := model.ClaimRelayRefund(db, r.ID)
				require.NoError(t, err)
				require.True(t, ok)
				t.Setenv("RELAY_REFUND_MOCK_QUERY_RESULT", "failure")
				require.NoError(t, db.Model(&model.RelayRefund{}).Where("id = ?", r.ID).Updates(map[string]any{"next_query_at": 0, "submitted_at": time.Now().Unix() - 90000, "query_attempts": 6}).Error)
				var wg sync.WaitGroup
				errs := make([]error, 8)
				wins := make([]bool, 8)
				for i := range errs {
					wg.Add(1)
					go func(i int) { defer wg.Done(); wins[i], errs[i] = model.ClaimRelayRefundQuery(db, r.ID) }(i)
				}
				wg.Wait()
				count := 0
				for i, err := range errs {
					require.NoError(t, err)
					if wins[i] {
						count++
					}
				}
				require.Equal(t, 1, count)
				current := get(r)
				require.True(t, current.NeedsReview)
				require.InDelta(t, time.Now().Unix()+3600, current.NextQueryAt, 2)
				require.Zero(t, balance(u))
				require.NoError(t, db.Model(&model.RelayRefund{}).Where("id = ?", r.ID).Updates(map[string]any{"next_query_at": 0}).Error)
				_, err = syncRelayRefunds(ctx, db, relayPayMock{}, 50)
				require.NoError(t, err)
				require.Equal(t, "unknown", get(r).State)
				require.Zero(t, balance(u))
			})
			t.Run("merchant_mismatch_is_deferred_without_dispatch", func(t *testing.T) {
				u, r := setup()
				t.Setenv("RELAY_REFUND_MOCK_RESULT", "success")
				wrong := relayRefundWrongMerchant{}
				_, err := syncRelayRefunds(ctx, db, wrong, 50)
				require.ErrorIs(t, err, model.ErrRelayPayMismatch)
				current := get(r)
				require.Equal(t, "approved_frozen", current.State)
				require.True(t, current.NeedsReview)
				require.InDelta(t, time.Now().Unix()+3600, current.NextQueryAt, 2)
				require.Zero(t, balance(u))
			})
			t.Run("cancel_and_invalid_limit", func(t *testing.T) {
				_, r := setup()
				cancelCtx, cancel := context.WithCancel(ctx)
				cancel()
				_, err := syncRelayRefunds(cancelCtx, db, relayPayMock{}, 50)
				require.ErrorIs(t, err, context.Canceled)
				require.Equal(t, "approved_frozen", get(r).State)
				_, err = syncRelayRefunds(ctx, db, relayPayMock{}, 51)
				require.ErrorIs(t, err, model.ErrRelayInput)
			})
		})
	}
}

type relayRefundWrongMerchant struct{ relayPayMock }

func (relayRefundWrongMerchant) App() string { return "wrong-merchant" }
