package controller

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestRelayRiskAPI(t *testing.T) {
	operator, identity, target := setupAdminUserTest(t)
	require.NoError(t, model.MigrateRelayWallet(model.DB))
	t.Setenv("RELAY_WALLET_SCHEMA", "true")
	t.Setenv("RELAY_RISK_API", "true")
	guard := func(handler gin.HandlerFunc) gin.HandlerFunc {
		return func(c *gin.Context) {
			RelayRiskAPIGate(c)
			if !c.IsAborted() {
				handler(c)
			}
		}
	}
	request := func(method, path, body, proof string, handler gin.HandlerFunc) int {
		r := adminUserRequest(method, path, body, proof, identity, common.RoleRootUser, nil, guard(handler))
		return r.Code
	}
	input := relayRiskControlInput{Revision: 0, Action: "maintenance", Reason: "verified planned work"}
	body, err := json.Marshal(input)
	require.NoError(t, err)
	op, err := service.RelayFinanceOperation("risk_control", input)
	require.NoError(t, err)
	t.Run("gate_role_and_no_password_fallback", func(t *testing.T) {
		_, err := service.GetVerificationRequirements(identity, service.VerificationScopeRelayFinance)
		require.ErrorIs(t, err, service.ErrVerificationForbidden)
		require.Equal(t, 403, request("POST", "/risk/control", string(body), "", ControlRelayRisk))
		t.Setenv("RELAY_RISK_API", "false")
		require.Equal(t, 503, request("GET", "/risk", "", "", GetRelayRisk))
		t.Setenv("RELAY_RISK_API", "true")
		require.NoError(t, model.DB.Model(operator).Update("role", common.RoleCommonUser).Error)
		require.Equal(t, 403, request("GET", "/risk", "", "", GetRelayRisk))
		require.NoError(t, model.DB.Model(operator).Update("role", common.RoleRootUser).Error)
	})
	require.NoError(t, model.DB.Create(&model.TwoFA{UserId: operator.Id, Secret: "JBSWY3DPEHPK3PXP", IsEnabled: true}).Error)
	t.Run("scope_factor_and_body_binding", func(t *testing.T) {
		requirements, err := service.GetVerificationRequirements(identity, service.VerificationScopeRelayFinance)
		require.NoError(t, err)
		require.Equal(t, []service.VerificationMethodOption{{Method: service.VerificationMethodTwoFA, Available: true}}, requirements.Methods)
		require.Equal(t, 200, request("POST", "/risk/control?prepare=1", string(body), "", ControlRelayRisk))
		passwordProof := issueSecurityEnrollmentProof(t, identity, op, service.VerificationMethodPassword)
		require.Equal(t, 403, request("POST", "/risk/control", string(body), passwordProof, ControlRelayRisk))
		proof := issueSecurityEnrollmentProof(t, identity, op, service.VerificationMethodTwoFA)
		require.Equal(t, 403, request("POST", "/risk/control", strings.Replace(string(body), "verified planned work", "different reason", 1), proof, ControlRelayRisk))
		require.Equal(t, 200, request("POST", "/risk/control", string(body), proof, ControlRelayRisk))
		require.Equal(t, 403, request("POST", "/risk/control", string(body), proof, ControlRelayRisk))
		var state model.RelayRiskState
		require.NoError(t, model.DB.First(&state, 1).Error)
		require.Equal(t, "maintenance", state.Mode)
	})
	t.Run("fixed_cost_identity_and_strict_input", func(t *testing.T) {
		estimated := false
		input := relayFixedCostInput{ID: uuid.NewString(), Month: model.RelayRiskMonth(time.Now()), Category: "hosting", AmountFen: 0, Estimated: &estimated, Reason: "explicit local test budget"}
		encoded, err := common.Marshal(input)
		require.NoError(t, err)
		op, err := service.RelayFinanceOperation("fixed_cost", input)
		require.NoError(t, err)
		proof := issueSecurityEnrollmentProof(t, identity, op, service.VerificationMethodTwoFA)
		require.Equal(t, 400, request("POST", "/risk/fixed-costs", strings.TrimSuffix(string(encoded), "}")+`,"reviewer_id":999}`, proof, CreateRelayFixedCost))
		require.Equal(t, 200, request("POST", "/risk/fixed-costs", string(encoded), proof, CreateRelayFixedCost))
		var cost model.RelayFixedCost
		require.NoError(t, model.DB.Where("id = ?", input.ID).Take(&cost).Error)
		require.Equal(t, operator.Id, cost.ReviewerID)
		require.NotEqual(t, target.Id, cost.ReviewerID)
		require.Equal(t, 400, request("GET", "/risk?month=invalid", "", "", GetRelayRisk))
		require.Equal(t, 200, request("GET", "/risk", "", "", GetRelayRisk))
		require.Equal(t, 400, request("GET", "/risk?unexpected=1", "", "", GetRelayRisk))
		require.Equal(t, 200, request("GET", "/risk/fixed-costs?p=1", "", "", ListRelayFixedCosts))
		require.Equal(t, 400, request("GET", "/risk/fixed-costs?p=0", "", "", ListRelayFixedCosts))
	})
}

func TestRelayRiskWorkerGate(t *testing.T) {
	t.Setenv("RELAY_WALLET_SCHEMA", "true")
	t.Setenv("RELAY_RISK_CONTROL", "false")
	require.False(t, (relayRiskEvaluateHandler{}).Enabled())
	t.Setenv("RELAY_RISK_CONTROL", "true")
	require.True(t, (relayRiskEvaluateHandler{}).Enabled())
	t.Setenv("RELAY_WALLET_SCHEMA", "false")
	require.False(t, (relayRiskEvaluateHandler{}).Enabled())
	require.Equal(t, time.Minute, (relayRiskEvaluateHandler{}).Interval())
}
