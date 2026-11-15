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

func TestRelayPolicyMatrix(t *testing.T) {
	for _, engine := range []string{"sqlite", "mysql", "postgres"} {
		t.Run(engine, func(t *testing.T) {
			var driver gorm.Dialector
			switch engine {
			case "sqlite":
				driver = sqlite.Open(filepath.Join(t.TempDir(), "policy.db") + "?_pragma=busy_timeout(10000)")
			case "mysql":
				require.NotEmpty(t, os.Getenv("TEST_MYSQL_DSN"))
				driver = mysql.Open(os.Getenv("TEST_MYSQL_DSN"))
			case "postgres":
				require.NotEmpty(t, os.Getenv("TEST_POSTGRES_DSN"))
				driver = postgres.Open(os.Getenv("TEST_POSTGRES_DSN"))
			}
			db, err := gorm.Open(driver, &gorm.Config{NamingStrategy: schema.NamingStrategy{TablePrefix: "policy_test_"}, Logger: logger.Default.LogMode(logger.Silent)})
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
			require.NoError(t, MigrateRelayWallet(db), "migration is repeatable")
			now := time.Now()
			row := func(scope, previous, version string, after int64) RelayPolicyVersion {
				return RelayPolicyVersion{Kind: RelayPolicyPrice, Scope: scope, Previous: previous, Version: version, Payload: `{"fixture":true}`,
					Digest: strings.Repeat("b", 64), OperatorID: 1, Reason: " verified price change ", EffectiveAt: now.Unix() + after}
			}
			count := func() int64 {
				var total int64
				require.NoError(t, db.Model(&RelayPolicyVersion{}).Count(&total).Error)
				return total
			}

			t.Run("invalid_input_writes_nothing", func(t *testing.T) {
				cases := map[string]func(*RelayPolicyVersion){
					"too_soon":        func(r *RelayPolicyVersion) { r.EffectiveAt = now.Unix() + RelayPolicyLeadSeconds - 1 },
					"past":            func(r *RelayPolicyVersion) { r.EffectiveAt = now.Unix() - 1 },
					"too_far":         func(r *RelayPolicyVersion) { r.EffectiveAt = now.Unix() + RelayPolicyHorizonSeconds + 1 },
					"no_reason":       func(r *RelayPolicyVersion) { r.Reason = "   " },
					"long_reason":     func(r *RelayPolicyVersion) { r.Reason = strings.Repeat("r", 257) },
					"bad_version":     func(r *RelayPolicyVersion) { r.Version = "v 2" },
					"same_as_prev":    func(r *RelayPolicyVersion) { r.Version = r.Previous },
					"no_previous":     func(r *RelayPolicyVersion) { r.Previous = "" },
					"bad_kind":        func(r *RelayPolicyVersion) { r.Kind = "other" },
					"no_scope":        func(r *RelayPolicyVersion) { r.Scope = "" },
					"no_payload":      func(r *RelayPolicyVersion) { r.Payload = "" },
					"bad_digest":      func(r *RelayPolicyVersion) { r.Digest = "abc" },
					"no_operator":     func(r *RelayPolicyVersion) { r.OperatorID = 0 },
					"huge_payload":    func(r *RelayPolicyVersion) { r.Payload = strings.Repeat("x", 8193) },
					"version_is_base": func(r *RelayPolicyVersion) { r.Version = "file-v1" },
				}
				for name, change := range cases {
					candidate := row("1/chat", "file-v1", "v2", 120)
					change(&candidate)
					_, err := PublishRelayPolicy(db, candidate, "file-v1", now)
					require.ErrorIs(t, err, ErrRelayInput, name)
				}
				require.Zero(t, count())
			})

			t.Run("single_chain_and_forward_time", func(t *testing.T) {
				_, err := PublishRelayPolicy(db, row("1/chat", "not-current", "v2", 120), "file-v1", now)
				require.ErrorIs(t, err, ErrRelayConflict, "expected version must be the one in force")
				saved, err := PublishRelayPolicy(db, row("1/chat", "file-v1", "v2", 120), "file-v1", now)
				require.NoError(t, err)
				require.Equal(t, "verified price change", saved.Reason)
				require.Equal(t, now.Unix(), saved.Created)
				require.NotZero(t, saved.ID)
				_, err = PublishRelayPolicy(db, row("1/chat", "file-v1", "v3", 180), "file-v1", now)
				require.ErrorIs(t, err, ErrRelayConflict, "the base is no longer current")
				_, err = PublishRelayPolicy(db, row("1/chat", "v2", "v3", 120), "file-v1", now)
				require.ErrorIs(t, err, ErrRelayConflict, "effective time must move forward")
				_, err = PublishRelayPolicy(db, row("1/chat", "v2", "v3", 60), "file-v1", now)
				require.ErrorIs(t, err, ErrRelayConflict)
				_, err = PublishRelayPolicy(db, row("1/chat", "v2", "v3", 180), "file-v1", now)
				require.NoError(t, err)
				_, err = PublishRelayPolicy(db, row("2/other", "file-v9", "v3", 120), "file-v9", now)
				require.ErrorIs(t, err, ErrRelayConflict, "a version name is unique across scopes")
				_, err = PublishRelayPolicy(db, row("2/other", "file-v9", "other-v2", 120), "file-v9", now)
				require.NoError(t, err, "scopes are independent chains")
				limits := row("global", "builtin-1-5", "limits-v2", 120)
				limits.Kind = RelayPolicyLimits
				_, err = PublishRelayPolicy(db, limits, "builtin-1-5", now)
				require.NoError(t, err)
				rows, err := ListRelayPolicy(db)
				require.NoError(t, err)
				versions := []string{}
				for _, item := range rows {
					versions = append(versions, item.Version)
				}
				require.Equal(t, []string{"v2", "v3", "other-v2", "limits-v2"}, versions)
			})

			t.Run("append_only", func(t *testing.T) {
				var first RelayPolicyVersion
				require.NoError(t, db.Where("version = ?", "v2").Take(&first).Error)
				before := first
				require.ErrorIs(t, db.Model(&first).Update("reason", "rewritten").Error, ErrRelayState)
				require.ErrorIs(t, db.Model(&first).Update("effective_at", 1).Error, ErrRelayState)
				require.ErrorIs(t, db.Delete(&first).Error, ErrRelayState)
				var after RelayPolicyVersion
				require.NoError(t, db.Where("version = ?", "v2").Take(&after).Error)
				require.Equal(t, before, after)
			})

			t.Run("concurrent_publishers_one_wins", func(t *testing.T) {
				before := count()
				var wait sync.WaitGroup
				results := make([]error, 8)
				for i := range results {
					wait.Add(1)
					go func(i int) {
						defer wait.Done()
						_, results[i] = PublishRelayPolicy(db, row("3/race", "file-race", fmt.Sprintf("race-v%d", i), 120), "file-race", now)
					}(i)
				}
				wait.Wait()
				succeeded := 0
				for _, err := range results {
					if err == nil {
						succeeded++
					}
				}
				require.Equal(t, 1, succeeded, "%v", results)
				require.Equal(t, before+1, count())
			})
		})
	}
}
