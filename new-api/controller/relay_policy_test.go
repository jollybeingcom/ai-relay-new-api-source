package controller

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestRelayPolicyAPI(t *testing.T) {
	operator, identity, _ := setupAdminUserTest(t)
	require.NoError(t, model.MigrateRelayWallet(model.DB))
	service.ResetRelayPolicyCache()
	t.Cleanup(service.ResetRelayPolicyCache)
	file := service.RelayWalletRoute{RelayTextPrice: service.RelayTextPrice{ChannelID: 7, Schema: "text-v1", Version: "file-v1", Model: "vendor/chat", Unit: "new-api-quota",
		InputRate: "1000000", CacheReadRate: "250000", OutputRate: "2000000", ContextTokens: 1000, InputLimit: 300, OutputLimit: 200},
		RequestModel: "chat", OutputCap: "max_tokens_total", Thinking: "none",
		UpstreamInputRate: "600000", UpstreamCacheReadRate: "150000", UpstreamOutputRate: "1300000"}
	raw, err := common.Marshal(map[string]any{"routes": []service.RelayWalletRoute{file}})
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "prices.json")
	require.NoError(t, os.WriteFile(path, raw, 0o600))
	t.Setenv("RELAY_WALLET_PRICE_FILE", path)
	t.Setenv("RELAY_WALLET_BILLING", "true")
	t.Setenv("RELAY_WALLET_SCHEMA", "true")
	t.Setenv("RELAY_POLICY_API", "true")
	t.Setenv("RELAY_POLICY_VERSIONS", "true")
	guard := func(handler gin.HandlerFunc) gin.HandlerFunc {
		return func(c *gin.Context) {
			RelayPolicyAPIGate(c)
			if !c.IsAborted() {
				handler(c)
			}
		}
	}
	call := func(method, path, body, proof string, handler gin.HandlerFunc) *httptest.ResponseRecorder {
		return adminUserRequest(method, path, body, proof, identity, common.RoleRootUser, nil, guard(handler))
	}
	rows := func() []model.RelayPolicyVersion {
		items, err := model.ListRelayPolicy(model.DB)
		require.NoError(t, err)
		return items
	}
	next := file
	next.Version, next.OutputRate = "v2", "3000000"
	input := service.RelayPricePublication{ExpectedVersion: "file-v1", EffectiveAt: time.Now().Unix() + 600, Reason: "verified supplier price change", Route: next}
	encoded, err := common.Marshal(input)
	require.NoError(t, err)
	body := string(encoded)
	op, err := service.RelayFinanceOperation("price_publish", input)
	require.NoError(t, err)

	t.Run("switches_and_role", func(t *testing.T) {
		for _, name := range []string{"RELAY_POLICY_API", "RELAY_POLICY_VERSIONS", "RELAY_WALLET_SCHEMA"} {
			t.Setenv(name, "false")
			require.Equal(t, 503, call("GET", "/policy", "", "", GetRelayPolicy).Code, name)
			require.Equal(t, 503, call("POST", "/policy/price", body, "", PublishRelayPolicyPrice).Code, name)
			t.Setenv(name, "true")
		}
		require.NoError(t, model.DB.Model(operator).Update("role", common.RoleCommonUser).Error)
		require.Equal(t, 403, call("GET", "/policy", "", "", GetRelayPolicy).Code)
		require.Equal(t, 403, call("POST", "/policy/price", body, "", PublishRelayPolicyPrice).Code)
		require.NoError(t, model.DB.Model(operator).Update("role", common.RoleRootUser).Error)
		require.Equal(t, 400, call("GET", "/policy?user=1", "", "", GetRelayPolicy).Code)
		view := call("GET", "/policy", "", "", GetRelayPolicy)
		require.Equal(t, 200, view.Code)
		require.Equal(t, "no-store", view.Header().Get("Cache-Control"))
		require.Contains(t, view.Body.String(), `"source":"file"`)
		require.Contains(t, view.Body.String(), `"upstream_input_rate":"600000"`)
		// Without a second factor nothing is published, also not with a password.
		require.Equal(t, 403, call("POST", "/policy/price", body, "", PublishRelayPolicyPrice).Code)
		require.Empty(t, rows())
	})
	require.NoError(t, model.DB.Create(&model.TwoFA{UserId: operator.Id, Secret: "JBSWY3DPEHPK3PXP", IsEnabled: true}).Error)

	t.Run("price_bound_to_second_factor", func(t *testing.T) {
		require.Equal(t, 200, call("POST", "/policy/price?prepare=1", body, "", PublishRelayPolicyPrice).Code)
		require.Empty(t, rows(), "preparing writes nothing")
		require.Equal(t, 400, call("POST", "/policy/price?force=1", body, "", PublishRelayPolicyPrice).Code)
		passwordProof := issueSecurityEnrollmentProof(t, identity, op, service.VerificationMethodPassword)
		require.Equal(t, 403, call("POST", "/policy/price", body, passwordProof, PublishRelayPolicyPrice).Code)
		proof := issueSecurityEnrollmentProof(t, identity, op, service.VerificationMethodTwoFA)
		cheaper := strings.Replace(body, `"output_rate":"3000000"`, `"output_rate":"2500000"`, 1)
		require.NotEqual(t, body, cheaper)
		require.Equal(t, 403, call("POST", "/policy/price", cheaper, proof, PublishRelayPolicyPrice).Code, "the proof covers the exact rates")
		later := strings.Replace(body, `"reason":"verified supplier price change"`, `"reason":"another reason"`, 1)
		require.Equal(t, 403, call("POST", "/policy/price", later, proof, PublishRelayPolicyPrice).Code)
		require.Equal(t, 400, call("POST", "/policy/price", strings.TrimSuffix(body, "}")+`,"operator_id":999}`, proof, PublishRelayPolicyPrice).Code)
		require.Equal(t, 400, call("POST", "/policy/price", body+body, proof, PublishRelayPolicyPrice).Code)
		require.Empty(t, rows())
		require.Equal(t, 200, call("POST", "/policy/price", body, proof, PublishRelayPolicyPrice).Code)
		require.Equal(t, 403, call("POST", "/policy/price", body, proof, PublishRelayPolicyPrice).Code, "a proof is single use")
		saved := rows()
		require.Len(t, saved, 1)
		require.Equal(t, operator.Id, saved[0].OperatorID, "the operator comes from the session, never the body")
		require.Equal(t, "v2", saved[0].Version)
		require.Equal(t, "file-v1", saved[0].Previous)
		require.Equal(t, input.EffectiveAt, saved[0].EffectiveAt)
		// The same expected version again, freshly verified: the chain refuses it.
		again := input
		again.Route.Version = "v2-b"
		againBody, err := common.Marshal(again)
		require.NoError(t, err)
		againOp, err := service.RelayFinanceOperation("price_publish", again)
		require.NoError(t, err)
		require.Equal(t, 409, call("POST", "/policy/price", string(againBody), issueSecurityEnrollmentProof(t, identity, againOp, service.VerificationMethodTwoFA), PublishRelayPolicyPrice).Code)
		below := input
		below.ExpectedVersion, below.Route.Version, below.Route.OutputRate, below.EffectiveAt = "v2", "v3", "1000000", input.EffectiveAt+60
		belowBody, err := common.Marshal(below)
		require.NoError(t, err)
		belowOp, err := service.RelayFinanceOperation("price_publish", below)
		require.NoError(t, err)
		require.Equal(t, 400, call("POST", "/policy/price", string(belowBody), issueSecurityEnrollmentProof(t, identity, belowOp, service.VerificationMethodTwoFA), PublishRelayPolicyPrice).Code, "selling below cost")
		require.Len(t, rows(), 1)
		view := call("GET", "/policy", "", "", GetRelayPolicy)
		require.Equal(t, 200, view.Code)
		require.Contains(t, view.Body.String(), `"pending":[{"version":"v2"`)
	})

	t.Run("limits_bound_to_second_factor", func(t *testing.T) {
		limits := service.RelayLimitsPublication{ExpectedVersion: service.RelayPolicyBaseLimits, Version: "limits-v2", EffectiveAt: time.Now().Unix() + 600, Reason: "capacity verified on the target host", Global: 8}
		limitsBody, err := common.Marshal(limits)
		require.NoError(t, err)
		limitsOp, err := service.RelayFinanceOperation("limits_publish", limits)
		require.NoError(t, err)
		require.Equal(t, 403, call("POST", "/policy/limits", string(limitsBody), "", PublishRelayPolicyLimits).Code)
		// A proof for a price publication cannot authorize a limit change.
		require.Equal(t, 403, call("POST", "/policy/limits", string(limitsBody), issueSecurityEnrollmentProof(t, identity, op, service.VerificationMethodTwoFA), PublishRelayPolicyLimits).Code)
		proof := issueSecurityEnrollmentProof(t, identity, limitsOp, service.VerificationMethodTwoFA)
		require.Equal(t, 403, call("POST", "/policy/limits", strings.Replace(string(limitsBody), `"global":8`, `"global":20`, 1), proof, PublishRelayPolicyLimits).Code)
		require.Equal(t, 200, call("POST", "/policy/limits", string(limitsBody), proof, PublishRelayPolicyLimits).Code)
		saved := rows()
		require.Len(t, saved, 2)
		require.Equal(t, model.RelayPolicyLimits, saved[1].Kind)
		require.Equal(t, operator.Id, saved[1].OperatorID)
		over := limits
		over.ExpectedVersion, over.Version, over.Global, over.EffectiveAt = "limits-v2", "limits-v3", 21, limits.EffectiveAt+60
		overBody, err := common.Marshal(over)
		require.NoError(t, err)
		overOp, err := service.RelayFinanceOperation("limits_publish", over)
		require.NoError(t, err)
		require.Equal(t, 400, call("POST", "/policy/limits", string(overBody), issueSecurityEnrollmentProof(t, identity, overOp, service.VerificationMethodTwoFA), PublishRelayPolicyLimits).Code)
		require.Len(t, rows(), 2)
	})
}
