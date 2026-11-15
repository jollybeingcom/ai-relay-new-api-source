package service

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
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
	"gorm.io/gorm/schema"
)

// Published versions replace a file route from their effective time; requests
// already reserved keep the snapshot they started with.
func TestRelayPolicyVersions(t *testing.T) {
	for _, dialect := range []string{"sqlite", "mysql", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			var driver gorm.Dialector
			switch dialect {
			case "sqlite":
				driver = sqlite.Open(filepath.Join(t.TempDir(), "policy.db") + "?_pragma=busy_timeout(10000)")
			case "mysql":
				require.NotEmpty(t, os.Getenv("TEST_MYSQL_DSN"))
				driver = mysql.Open(os.Getenv("TEST_MYSQL_DSN"))
			case "postgres":
				require.NotEmpty(t, os.Getenv("TEST_POSTGRES_DSN"))
				driver = postgres.Open(os.Getenv("TEST_POSTGRES_DSN"))
			}
			db, err := gorm.Open(driver, &gorm.Config{NamingStrategy: schema.NamingStrategy{TablePrefix: "policy_svc_"}, Logger: logger.Default.LogMode(logger.Silent)})
			require.NoError(t, err)
			pool, err := db.DB()
			require.NoError(t, err)
			pool.SetMaxOpenConns(4)
			previousType, previousDB := common.MainDatabaseType(), model.DB
			common.SetMainDatabaseType(common.DatabaseType(dialect))
			model.DB = db
			ResetRelayPolicyCache()
			t.Cleanup(func() {
				require.NoError(t, db.Migrator().DropTable(&model.RelayPolicyVersion{}, &model.RelayAdjustment{}, &model.RelayFixedCost{}, &model.RelayRiskState{}, &model.RelayRiskEvent{}, &model.RelayWalletCost{}, &model.RelayWalletCostSnapshot{}, &model.RelayRefundEvent{}, &model.RelayRefund{}, &model.RelayFundingEntry{}, &model.RelayFundingReady{}, &model.RelayPayEvent{}, &model.RelayPayOrder{}, &model.RelayWalletLogPending{}, &model.RelayWalletLogMarker{}, &model.RelayWalletReceipt{}, &model.Channel{}, &model.RelayWalletEvent{}, &model.RelayWalletRequest{}, &model.RelayWalletLock{}, &model.Token{}, &model.User{}))
				require.NoError(t, pool.Close())
				model.DB = previousDB
				common.SetMainDatabaseType(previousType)
				ResetRelayPolicyCache()
			})
			require.NoError(t, db.AutoMigrate(&model.User{}, &model.Token{}, &model.Channel{}))
			require.NoError(t, model.MigrateRelayWallet(db))
			channel := model.Channel{Name: "policy-fixture"}
			require.NoError(t, db.Create(&channel).Error)

			file := relayWalletChatRoute{RelayTextPrice: RelayTextPrice{ChannelID: channel.Id, Schema: "text-v1", Version: "file-v1", Model: "vendor/chat", Unit: "new-api-quota",
				InputRate: "1000000", CacheReadRate: "250000", OutputRate: "2000000", ContextTokens: 1000, InputLimit: 300, OutputLimit: 200},
				RequestModel: "chat", OutputCap: "max_tokens_total", Thinking: "none",
				UpstreamInputRate: "600000", UpstreamCacheReadRate: "150000", UpstreamOutputRate: "1300000"}
			raw, err := common.Marshal(map[string]any{"routes": []relayWalletChatRoute{file}})
			require.NoError(t, err)
			path := filepath.Join(t.TempDir(), "prices.json")
			require.NoError(t, os.WriteFile(path, raw, 0o600))
			t.Setenv("RELAY_WALLET_PRICE_FILE", path)
			t.Setenv("RELAY_WALLET_BILLING", "true")
			key := relayWalletRouteKey(channel.Id, "chat")
			scope := relayPolicyPriceScope(channel.Id, "chat")
			active := func() relayWalletChatRoute {
				routes, err := activeRelayWalletRoutes()
				require.NoError(t, err)
				return routes[key]
			}
			changed := func(version string, edit func(*relayWalletChatRoute)) relayWalletChatRoute {
				route := file
				route.Version = version
				if edit != nil {
					edit(&route)
				}
				return route
			}
			// A row that is already in force cannot be produced through the
			// publication API (60 s lead); tests insert it as the API would store it.
			inForce := func(kind, rowScope, previous, version string, payload any, secondsAgo int64) {
				encoded, err := common.Marshal(payload)
				require.NoError(t, err)
				digest := sha256.Sum256(encoded)
				require.NoError(t, db.Create(&model.RelayPolicyVersion{Kind: kind, Scope: rowScope, Previous: previous, Version: version, Payload: string(encoded),
					Digest: hex.EncodeToString(digest[:]), OperatorID: 1, Reason: "fixture", EffectiveAt: time.Now().Unix() - secondsAgo, Created: time.Now().Unix() - secondsAgo - 60}).Error)
				ResetRelayPolicyCache()
			}
			reserve := func(route relayWalletChatRoute) (*RelayWalletSession, model.RelayWalletRequest) {
				user := model.User{Username: uuid.NewString()[:16], AffCode: uuid.NewString()[:12], Password: "fixture-only", Quota: 100000, Status: 1, Role: 1}
				require.NoError(t, db.Create(&user).Error)
				token := model.Token{UserId: user.Id, Key: uuid.NewString(), RemainQuota: 100000, Status: 1, ExpiredTime: -1}
				require.NoError(t, db.Create(&token).Error)
				intent := model.RelayWalletRequest{ID: uuid.NewString(), UserID: user.Id, TokenID: token.Id, PayloadHash: strings.Repeat("a", 64),
					CostBasis: &model.RelayWalletCostSnapshot{ChannelID: route.ChannelID, Model: route.Model, InputRate: route.UpstreamInputRate,
						CacheReadRate: route.UpstreamCacheReadRate, OutputRate: route.UpstreamOutputRate, InputLimit: route.InputLimit, OutputLimit: route.OutputLimit}}
				session, err := NewPricedRelayWalletSession(db, intent, route.RelayTextPrice)
				require.NoError(t, err)
				return session, intent
			}
			input, cache, output, total := 100, 40, 50, 150
			usage := RelayTextUsage{Model: "vendor/chat", Complete: true, Input: &input, CacheRead: &cache, Output: &output, Total: &total}
			receipt := func(id string) model.RelayWalletReceipt {
				var saved model.RelayWalletReceipt
				require.NoError(t, db.Where("request_id = ?", id).Take(&saved).Error)
				return saved
			}

			t.Run("disabled_reads_only_the_file", func(t *testing.T) {
				t.Setenv("RELAY_POLICY_VERSIONS", "false")
				require.Equal(t, file, active())
				limits, err := ActiveRelayWalletLimits()
				require.NoError(t, err)
				require.Equal(t, model.RelayWalletLimits{PerUser: 1, Global: 5}, limits)
				_, err = PublishRelayPrice(1, RelayPricePublication{ExpectedVersion: "file-v1", EffectiveAt: time.Now().Unix() + 120, Reason: "closed", Route: changed("v2", nil)})
				require.ErrorIs(t, err, ErrRelayPolicyDisabled)
				_, err = PublishRelayLimits(1, RelayLimitsPublication{ExpectedVersion: RelayPolicyBaseLimits, Version: "limits-v2", EffectiveAt: time.Now().Unix() + 120, Reason: "closed", Global: 3})
				require.ErrorIs(t, err, ErrRelayPolicyDisabled)
				_, err = ReadRelayPolicy()
				require.ErrorIs(t, err, ErrRelayPolicyDisabled)
			})
			t.Setenv("RELAY_POLICY_VERSIONS", "true")

			t.Run("publication_rules", func(t *testing.T) {
				soon := time.Now().Unix() + 120
				publish := func(expected string, route relayWalletChatRoute) error {
					_, err := PublishRelayPrice(1, RelayPricePublication{ExpectedVersion: expected, EffectiveAt: soon, Reason: "verified supplier change", Route: route})
					return err
				}
				require.ErrorIs(t, publish("file-v1", changed("v2", func(r *relayWalletChatRoute) { r.OutputRate = "1000000" })), model.ErrRelayInput, "sale below cost")
				require.ErrorIs(t, publish("file-v1", changed("v2", func(r *relayWalletChatRoute) { r.UpstreamInputRate = "" })), model.ErrRelayInput, "cost is required")
				require.ErrorIs(t, publish("file-v1", changed("v2", func(r *relayWalletChatRoute) { r.Model = "vendor/other" })), model.ErrRelayInput, "provider model is not republishable")
				require.ErrorIs(t, publish("file-v1", changed("v2", func(r *relayWalletChatRoute) { r.Thinking = "disabled" })), model.ErrRelayInput)
				require.ErrorIs(t, publish("file-v1", changed("v2", func(r *relayWalletChatRoute) { r.ContextTokens = 2000 })), model.ErrRelayInput)
				require.ErrorIs(t, publish("file-v1", changed("v2", func(r *relayWalletChatRoute) { r.RequestModel = "unknown" })), model.ErrRelayInput, "only existing routes")
				require.ErrorIs(t, publish("file-v1", changed("v2", func(r *relayWalletChatRoute) { r.ChannelID = channel.Id + 99 })), model.ErrRelayInput)
				require.ErrorIs(t, publish("file-v1", changed("v2", func(r *relayWalletChatRoute) { r.OutputLimit = 701 })), model.ErrRelayInput, "limits stay inside the context")
				require.ErrorIs(t, publish("file-v1", changed("file-v1", nil)), model.ErrRelayConflict, "file versions cannot be reused")
				require.ErrorIs(t, publish("stale", changed("v2", nil)), model.ErrRelayConflict)
				_, err := PublishRelayPrice(1, RelayPricePublication{ExpectedVersion: "file-v1", EffectiveAt: time.Now().Unix() + 30, Reason: "too soon", Route: changed("v2", nil)})
				require.ErrorIs(t, err, model.ErrRelayInput)
				var rows int64
				require.NoError(t, db.Model(&model.RelayPolicyVersion{}).Count(&rows).Error)
				require.Zero(t, rows, "a refused publication writes nothing")
				require.Equal(t, file, active())
			})

			t.Run("in_flight_keeps_snapshot_new_requests_switch", func(t *testing.T) {
				first, firstIntent := reserve(active())
				require.NoError(t, first.Submit())
				v2 := changed("v2", func(r *relayWalletChatRoute) {
					r.InputRate, r.CacheReadRate, r.OutputRate = "2000000", "500000", "4000000"
					r.InputLimit, r.OutputLimit = 200, 100
				})
				// Published for later: nothing changes yet.
				_, err := PublishRelayPrice(7, RelayPricePublication{ExpectedVersion: "file-v1", EffectiveAt: time.Now().Unix() + 3600, Reason: "pending change", Route: changed("v2-pending", nil)})
				require.NoError(t, err)
				require.Equal(t, file, active(), "a pending version does not price requests")
				prices, err := PublishedRelayWalletPrices()
				require.NoError(t, err)
				require.Equal(t, "file-v1", prices[0].Version)
				// Another scope chain is used so the pending row above stays valid.
				require.NoError(t, db.Exec("DELETE FROM "+db.NamingStrategy.TableName("relay_policy_versions")).Error)
				inForce(model.RelayPolicyPrice, scope, "file-v1", "v2", v2, 5)
				require.Equal(t, v2, active(), "the effective version prices new requests without a restart")
				prices, err = PublishedRelayWalletPrices()
				require.NoError(t, err)
				require.Equal(t, "v2", prices[0].Version)
				require.Equal(t, 200, prices[0].InputLimit)

				second, secondIntent := reserve(active())
				require.Equal(t, 800, second.GetPreConsumedQuota(), "(2*200 + 4*100) under v2")
				require.Equal(t, 700, first.GetPreConsumedQuota(), "(1*300 + 2*200) under the file price")
				require.NoError(t, second.Submit())
				require.NoError(t, first.SettleTextUsage(usage))
				require.NoError(t, second.SettleTextUsage(usage))
				old, fresh := receipt(firstIntent.ID), receipt(secondIntent.ID)
				require.Equal(t, "file-v1", old.PriceVersion)
				require.EqualValues(t, 170, old.Actual, "60 + 40*0.25 + 50*2 at the price in force when it started")
				require.Equal(t, "v2", fresh.PriceVersion)
				require.EqualValues(t, 340, fresh.Actual)
				var costs []model.RelayWalletCostSnapshot
				require.NoError(t, db.Where("request_id IN ?", []string{firstIntent.ID, secondIntent.ID}).Order("price_version").Find(&costs).Error)
				require.Len(t, costs, 2)
				require.Equal(t, "file-v1", costs[0].PriceVersion)
				require.Equal(t, "v2", costs[1].PriceVersion)
			})

			t.Run("administrator_view", func(t *testing.T) {
				_, err := PublishRelayPrice(7, RelayPricePublication{ExpectedVersion: "v2", EffectiveAt: time.Now().Unix() + 600, Reason: "next change", Route: changed("v3", nil)})
				require.NoError(t, err)
				view, err := ReadRelayPolicy()
				require.NoError(t, err)
				require.Len(t, view.Routes, 1)
				require.Equal(t, "published", view.Routes[0].Active.Source)
				require.Equal(t, "v2", view.Routes[0].Active.Version)
				require.NotZero(t, view.Routes[0].Active.EffectiveAt)
				require.Equal(t, "600000", view.Routes[0].Active.Route.UpstreamInputRate, "loaded cost is visible to the administrator")
				require.Len(t, view.Routes[0].Pending, 1)
				require.Equal(t, "v3", view.Routes[0].Pending[0].Version)
				require.Equal(t, "v3", view.History[0].Version, "newest first")
				require.Equal(t, 7, view.History[0].OperatorID)
				require.Equal(t, "next change", view.History[0].Reason)
				require.Equal(t, RelayPolicyLimitVersion{Version: RelayPolicyBaseLimits, Source: "builtin", Global: 5}, view.Limits)
				encoded, err := common.Marshal(view.History)
				require.NoError(t, err)
				require.NotContains(t, string(encoded), "upstream_input_rate", "history lists facts, payloads stay out")
			})

			t.Run("limits", func(t *testing.T) {
				for _, global := range []int{0, -1, 21} {
					_, err := PublishRelayLimits(1, RelayLimitsPublication{ExpectedVersion: RelayPolicyBaseLimits, Version: "limits-bad", EffectiveAt: time.Now().Unix() + 120, Reason: "out of range", Global: global})
					require.ErrorIs(t, err, model.ErrRelayInput)
				}
				_, err := PublishRelayLimits(1, RelayLimitsPublication{ExpectedVersion: "stale", Version: "limits-v2", EffectiveAt: time.Now().Unix() + 120, Reason: "stale", Global: 3})
				require.ErrorIs(t, err, model.ErrRelayConflict)
				_, err = PublishRelayLimits(1, RelayLimitsPublication{ExpectedVersion: RelayPolicyBaseLimits, Version: "limits-pending", EffectiveAt: time.Now().Unix() + 120, Reason: "planned", Global: 3})
				require.NoError(t, err)
				limits, err := ActiveRelayWalletLimits()
				require.NoError(t, err)
				require.Equal(t, 5, limits.Global, "pending limit is not in force")
				require.NoError(t, db.Exec("DELETE FROM "+db.NamingStrategy.TableName("relay_policy_versions")+" WHERE kind = ?", model.RelayPolicyLimits).Error)
				inForce(model.RelayPolicyLimits, relayPolicyLimitsScope, RelayPolicyBaseLimits, "limits-v2", relayPolicyLimits{Global: 2}, 5)
				limits, err = ActiveRelayWalletLimits()
				require.NoError(t, err)
				require.Equal(t, model.RelayWalletLimits{PerUser: 1, Global: 2}, limits)
				// Two held generations fill the published site-wide limit.
				one, _ := reserve(active())
				two, _ := reserve(active())
				user := model.User{Username: uuid.NewString()[:16], AffCode: uuid.NewString()[:12], Password: "fixture-only", Quota: 100000, Status: 1, Role: 1}
				require.NoError(t, db.Create(&user).Error)
				token := model.Token{UserId: user.Id, Key: uuid.NewString(), RemainQuota: 100000, Status: 1, ExpiredTime: -1}
				require.NoError(t, db.Create(&token).Error)
				_, err = NewPricedRelayWalletSession(db, model.RelayWalletRequest{ID: uuid.NewString(), UserID: user.Id, TokenID: token.Id, PayloadHash: strings.Repeat("a", 64)}, active().RelayTextPrice)
				require.ErrorIs(t, err, model.ErrRelayGlobalBusy)
				require.NoError(t, one.Submit())
				require.NoError(t, two.Submit())
				require.NoError(t, one.SettleTextUsage(usage))
				require.NoError(t, two.SettleTextUsage(usage))
			})

			t.Run("invalid_stored_version_fails_closed", func(t *testing.T) {
				broken := changed("v9", func(r *relayWalletChatRoute) { r.OutputRate = "1" })
				inForce(model.RelayPolicyPrice, scope, "v3", "v9", broken, 1)
				_, err := activeRelayWalletRoutes()
				require.Error(t, err, "a stored version below cost must stop pricing, not be used")
				_, err = PublishedRelayWalletPrices()
				require.Error(t, err)
				_, err = ReadRelayPolicy()
				require.Error(t, err)
				require.NoError(t, db.Exec("DELETE FROM "+db.NamingStrategy.TableName("relay_policy_versions")+" WHERE version = ?", "v9").Error)
				inForce(model.RelayPolicyLimits, relayPolicyLimitsScope, "limits-v2", "limits-v9", relayPolicyLimits{Global: 500}, 1)
				_, err = ActiveRelayWalletLimits()
				require.Error(t, err)
			})
		})
	}
}
