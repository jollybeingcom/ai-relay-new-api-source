package service

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
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

func TestRelayTextPriceBounds(t *testing.T) {
	t.Run("raw_upstream_usage", func(t *testing.T) {
		good := `{"prompt_tokens":100,"completion_tokens":50,"total_tokens":150,"prompt_cache_hit_tokens":40,"prompt_cache_miss_tokens":60,"prompt_tokens_details":{"cached_tokens":40},"completion_tokens_details":{"reasoning_tokens":30}}`
		facts, err := ParseRelayTextUsage([]byte(good), "fixture", true)
		require.NoError(t, err)
		assert.Equal(t, 40, *facts.CacheRead)
		assert.Equal(t, 50, *facts.Output, "reasoning is already included, not added again")
		for _, raw := range []string{
			`{}`, `null`, `[]`,
			strings.Replace(good, `"prompt_tokens":100`, `"prompt_tokens":null`, 1),
			strings.Replace(good, `"completion_tokens":50`, `"completion_tokens":-1`, 1),
			strings.Replace(good, `"completion_tokens":50`, `"completion_tokens":1.5`, 1),
			strings.Replace(good, `"completion_tokens":50`, `"completion_tokens":18446744073709551615`, 1),
			strings.Replace(good, `"cached_tokens":40`, `"cached_tokens":41`, 1),
			strings.Replace(good, `"reasoning_tokens":30`, `"reasoning_tokens":51`, 1),
			strings.Replace(good, `"prompt_cache_miss_tokens":60`, `"prompt_cache_miss_tokens":59`, 1),
			strings.Replace(good, `"cached_tokens":40`, `"audio_tokens":40`, 1),
			strings.Replace(good, `"prompt_cache_hit_tokens":40,`, ``, 1),
		} {
			// A single valid cache source is enough; the final case preserves details.
			_, err := ParseRelayTextUsage([]byte(raw), "fixture", true)
			if raw == strings.Replace(good, `"prompt_cache_hit_tokens":40,`, ``, 1) {
				require.NoError(t, err)
			} else {
				require.Error(t, err, raw)
			}
		}
		_, err = ParseRelayTextUsage([]byte(good), "fixture", false)
		require.Error(t, err)
	})
	base := RelayTextPrice{Schema: "text-v1", Version: "fixture-v1", Model: "fixture", Unit: "new-api-quota", InputRate: "0.5", CacheReadRate: "0.25", OutputRate: "1", ContextTokens: 2000000, InputLimit: 1000000, OutputLimit: 1000000}
	reserve, err := base.Reservation()
	require.NoError(t, err)
	assert.Equal(t, 2, reserve, "sum 1.5 then ceil once")
	input, cache, output, total := 1000000, 0, 1000000, 2000000
	usage := RelayTextUsage{Model: "fixture", Complete: true, Input: &input, CacheRead: &cache, Output: &output, Total: &total}
	charge, err := base.Charge(usage)
	require.NoError(t, err)
	assert.Equal(t, 2, charge, "round whole request once")
	for _, scenario := range []string{"negative_rate", "exponent", "nan", "huge_rate", "context", "negative_limit", "overflow", "free", "cache_more_expensive", "missing", "incomplete", "wrong_model", "negative_usage", "cache_overlap", "total_mismatch", "explicit_zero"} {
		t.Run(scenario, func(t *testing.T) {
			price, facts := base, usage
			switch scenario {
			case "negative_rate":
				price.InputRate = "-1"
			case "exponent":
				price.InputRate = "1e1000000000"
			case "nan":
				price.InputRate = "NaN"
			case "huge_rate":
				price.InputRate = strings.Repeat("9", 500)
			case "context":
				price.ContextTokens = 1
			case "negative_limit":
				price.InputLimit = -1
			case "overflow":
				price.OutputRate = "9999999999999999"
			case "free":
				price.InputRate, price.CacheReadRate, price.OutputRate = "0", "0", "0"
			case "cache_more_expensive":
				price.CacheReadRate = "4"
				value, err := price.Reservation()
				require.NoError(t, err)
				assert.Equal(t, 5, value)
				return
			case "missing":
				facts.Input = nil
			case "incomplete":
				facts.Complete = false
			case "wrong_model":
				facts.Model = "another-model"
			case "negative_usage":
				n := -1
				facts.Output = &n
			case "cache_overlap":
				n := 1000001
				facts.CacheRead = &n
			case "total_mismatch":
				n := 1
				facts.Total = &n
			case "explicit_zero":
				zero := 0
				facts.Input, facts.CacheRead, facts.Output, facts.Total = &zero, &zero, &zero, &zero
				value, err := price.Charge(facts)
				require.NoError(t, err)
				assert.Zero(t, value)
				return
			}
			if price != base {
				_, err := price.Reservation()
				require.Error(t, err)
			} else {
				_, err := price.Charge(facts)
				require.ErrorIs(t, err, ErrRelayUsageUnknown)
			}
		})
	}
}

func TestRelayWalletSessionMatrix(t *testing.T) {
	for _, dialect := range []string{"sqlite", "mysql", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			var driver gorm.Dialector
			switch dialect {
			case "sqlite":
				driver = sqlite.Open(filepath.Join(t.TempDir(), "session.db") + "?_pragma=busy_timeout(10000)")
			case "mysql":
				dsn := "relay_test:isolated-test-only@tcp(mysql:3306)/relay_test?charset=utf8mb4&parseTime=True&loc=Local"
				require.True(t, os.Getenv("TEST_MYSQL_DSN") == dsn, "isolated fixture required")
				driver = mysql.Open(dsn)
			case "postgres":
				dsn := "host=postgres user=relay_test password=isolated-test-only dbname=relay_test port=5432 sslmode=disable"
				require.True(t, os.Getenv("TEST_POSTGRES_DSN") == dsn, "isolated fixture required")
				driver = postgres.Open(dsn)
			}
			db, err := gorm.Open(driver, &gorm.Config{NamingStrategy: schema.NamingStrategy{TablePrefix: "session_test_"}, Logger: logger.Default.LogMode(logger.Silent)})
			require.NoError(t, err)
			sqlDB, err := db.DB()
			require.NoError(t, err)
			sqlDB.SetMaxOpenConns(4)
			previous := common.MainDatabaseType()
			common.SetMainDatabaseType(common.DatabaseType(dialect))
			t.Cleanup(func() {
				assert.NoError(t, db.Migrator().DropTable(&model.RelayPolicyVersion{}, &model.RelayAdjustment{}, &model.RelayFixedCost{}, &model.RelayRiskState{}, &model.RelayRiskEvent{}, &model.RelayWalletCost{}, &model.RelayWalletCostSnapshot{}, &model.RelayRefundEvent{}, &model.RelayRefund{}, &model.RelayFundingEntry{}, &model.RelayFundingReady{}, &model.RelayPayEvent{}, &model.RelayPayOrder{}, &model.RelayWalletLogPending{}, &model.RelayWalletLogMarker{}, &model.RelayWalletReceipt{}, &model.Channel{}, &model.RelayWalletEvent{}, &model.RelayWalletRequest{}, &model.RelayWalletLock{}, &model.Token{}, &model.User{}))
				assert.NoError(t, sqlDB.Close())
				common.SetMainDatabaseType(previous)
			})
			require.NoError(t, db.AutoMigrate(&model.User{}, &model.Token{}, &model.Channel{}))
			require.NoError(t, model.MigrateRelayWallet(db))
			query := "SELECT version()"
			if dialect == "sqlite" {
				query = "SELECT sqlite_version()"
			}
			var version string
			require.NoError(t, db.Raw(query).Scan(&version).Error)
			t.Logf("database version: %s", version)
			for _, scenario := range []string{"before_send", "after_send", "settled", "cancel_race", "priced", "usage_missing", "usage_exceeds_bound"} {
				t.Run(scenario, func(t *testing.T) {
					user := model.User{Username: uuid.NewString()[:16], AffCode: uuid.NewString()[:12], Password: "fixture-only", Quota: 1000, Status: 1, Role: 1}
					require.NoError(t, db.Create(&user).Error)
					token := model.Token{UserId: user.Id, Key: uuid.NewString(), RemainQuota: 1000, Status: 1, ExpiredTime: -1}
					require.NoError(t, db.Create(&token).Error)
					intent := model.RelayWalletRequest{ID: uuid.NewString(), UserID: user.Id, TokenID: token.Id, PayloadHash: strings.Repeat("a", 64), PriceVersion: "fixture-v1", PriceSnapshot: `{"input":1}`, Reserved: 700}
					priced := scenario == "priced" || strings.HasPrefix(scenario, "usage_")
					var session *RelayWalletSession
					if priced {
						channel := model.Channel{Name: "price-fixture"}
						require.NoError(t, db.Create(&channel).Error)
						price := RelayTextPrice{Schema: "text-v1", Version: "fixture-v1", Model: "fixture", Unit: "new-api-quota", InputRate: "1000000", CacheReadRate: "250000", OutputRate: "2000000", ContextTokens: 500, InputLimit: 300, OutputLimit: 200}
						price.ChannelID = channel.Id
						session, err = NewPricedRelayWalletSession(db, intent, price)
						require.NoError(t, err)
						require.ErrorIs(t, session.SubmitForChannel(channel.Id+1000), model.ErrRelayConflict)
						require.ErrorIs(t, session.Settle(170), ErrRelayUsageUnknown)
						require.NoError(t, db.Where("id = ?", intent.ID).Take(&intent).Error)
						price.OutputRate = "9000000"
						_, changedErr := NewPricedRelayWalletSession(db, intent, price)
						require.ErrorIs(t, changedErr, model.ErrRelayConflict, "a changed price must not replace an in-flight snapshot")
					} else {
						session, err = NewRelayWalletSession(db, intent)
					}
					require.NoError(t, err)
					assert.Equal(t, 700, session.GetPreConsumedQuota())
					assert.Error(t, session.ReserveBeforeSubmit(701))
					assert.Error(t, session.Reserve(-1))
					require.NoError(t, session.ReserveBeforeSubmit(700))
					wantState, wantQuota, wantUsed := "released", 1000, 0
					switch scenario {
					case "priced", "usage_missing", "usage_exceeds_bound":
						require.NoError(t, session.Submit())
						session, err = NewRelayWalletSession(db, intent)
						require.NoError(t, err)
						input, cache, output, total := 100, 40, 50, 150
						usage := RelayTextUsage{Model: "fixture", Complete: true, Input: &input, CacheRead: &cache, Output: &output, Total: &total}
						if scenario == "priced" {
							require.NoError(t, session.SettleRawTextUsage([]byte(`{"prompt_tokens":100,"completion_tokens":50,"total_tokens":150,"prompt_cache_hit_tokens":40}`), "fixture", true))
							require.NoError(t, session.SettleTextUsage(usage))
							wantState, wantQuota, wantUsed = "settled", 830, 170
						} else {
							if scenario == "usage_missing" {
								usage.CacheRead = nil
							} else {
								output, total = 201, 301
							}
							if scenario == "usage_missing" {
								require.ErrorIs(t, session.SettleRawTextUsage([]byte(`{"prompt_tokens":100}`), "fixture", true), ErrRelayUsageUnknown)
							} else {
								require.ErrorIs(t, session.SettleTextUsage(usage), ErrRelayUsageUnknown)
							}
							wantState, wantQuota = "unknown", 300
						}
					case "before_send":
						assert.True(t, session.NeedsRefund())
						session.Refund(nil)
						assert.Error(t, session.Submit())
					case "after_send":
						require.NoError(t, session.Submit())
						assert.ErrorIs(t, session.Submit(), ErrRelayAlreadyDispatched)
						assert.Error(t, session.ReserveBeforeSubmit(700))
						require.NoError(t, session.Fail())
						wantState, wantQuota = "unknown", 300
					case "settled":
						require.NoError(t, session.Submit())
						require.NoError(t, session.Settle(700))
						require.NoError(t, session.Fail())
						wantState, wantQuota, wantUsed = "settled", 300, 700
					case "cancel_race":
						start := make(chan struct{})
						dispatch := make(chan error, 1)
						cancel := make(chan error, 1)
						var workers sync.WaitGroup
						workers.Go(func() { <-start; dispatch <- session.Submit() })
						workers.Go(func() { <-start; cancel <- session.Fail() })
						close(start)
						workers.Wait()
						require.NoError(t, <-cancel)
						if err := <-dispatch; err == nil {
							wantState, wantQuota = "unknown", 300
						} else {
							assert.ErrorIs(t, err, model.ErrRelayState)
						}
					}
					// A new in-memory session must obey the stored state after failure/success.
					recreated, err := NewRelayWalletSession(db, intent)
					require.NoError(t, err)
					assert.Error(t, recreated.Submit())
					recreated.Refund(nil)
					assert.False(t, recreated.NeedsRefund())
					if scenario == "settled" {
						require.NoError(t, recreated.Settle(700))
					}
					var request model.RelayWalletRequest
					require.NoError(t, db.Where("id = ?", intent.ID).Take(&request).Error)
					assert.Equal(t, wantState, request.State)
					require.NoError(t, db.First(&user, user.Id).Error)
					require.NoError(t, db.First(&token, token.Id).Error)
					assert.Equal(t, wantQuota, user.Quota)
					assert.Equal(t, wantQuota, token.RemainQuota)
					assert.Equal(t, wantUsed, user.UsedQuota)
					assert.Equal(t, wantUsed, token.UsedQuota)
					if scenario == "priced" {
						var receipt model.RelayWalletReceipt
						require.NoError(t, db.Where("request_id = ?", intent.ID).Take(&receipt).Error)
						assert.EqualValues(t, 170, receipt.Actual)
						assert.Equal(t, 40, receipt.CacheRead)
						var channel model.Channel
						require.NoError(t, db.First(&channel, receipt.ChannelID).Error)
						assert.EqualValues(t, 170, channel.UsedQuota)
					}
				})
			}
		})
	}
}
