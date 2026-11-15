package controller

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestRelayLegacyQuotaAdjustmentBlocked(t *testing.T) {
	f := newRelayPayControllerFixture(t, "mock")
	user := f.users[0]
	identity := service.AuthIdentity{UserID: user.Id}
	body := fmt.Sprintf(`{"id":%d,"action":"add_quota","mode":"add","value":5000000}`, user.Id)
	quota := func() int {
		var got model.User
		require.NoError(t, model.DB.Where("id = ?", user.Id).Take(&got).Error)
		return got.Quota
	}
	for _, mode := range []string{"add", "subtract", "override"} {
		w := adminUserRequest("POST", "/api/user/manage", strings.Replace(body, `"add"`, `"`+mode+`"`, 1), "", identity, common.RoleRootUser, nil, ManageUser)
		require.Equal(t, 403, w.Code, w.Body.String())
		require.Contains(t, w.Body.String(), "RELAY_LEGACY_ADJUSTMENT_DISABLED")
	}
	require.Zero(t, quota())
	// Without the relay wallet the upstream behavior is unchanged.
	t.Setenv("RELAY_WALLET_SCHEMA", "false")
	w := adminUserRequest("POST", "/api/user/manage", body, "", identity, common.RoleRootUser, nil, ManageUser)
	require.Equal(t, 200, w.Code, w.Body.String())
	require.Equal(t, 5000000, quota())
}

func TestRelayAdjustmentAPI(t *testing.T) {
	f := newRelayPayControllerFixture(t, "mock")
	admin := model.User{Username: "adjust-admin", AffCode: "ajadmin", Role: common.RoleAdminUser, Status: common.UserStatusEnabled}
	require.NoError(t, model.DB.Create(&admin).Error)
	require.NoError(t, model.DB.AutoMigrate(&model.UserSession{}, &model.AuthFlow{}, &model.TwoFA{}, &model.PasskeyCredential{}))
	require.NoError(t, model.DB.Create(&model.TwoFA{UserId: admin.Id, Secret: "JBSWY3DPEHPK3PXP", IsEnabled: true}).Error)
	bundle, err := service.CreateLoginSession(admin.Id, "password", "127.0.0.1", "isolated-adjustment-test")
	require.NoError(t, err)
	identity, err := service.ParseAccessToken(bundle.AccessToken)
	require.NoError(t, err)
	user := f.users[0]
	require.NoError(t, model.InitializeEmptyRelayFunding(model.DB, user.Id))
	quota := func() int {
		var got model.User
		require.NoError(t, model.DB.Where("id = ?", user.Id).Take(&got).Error)
		return got.Quota
	}
	decode := func(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
		var data map[string]any
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &data), w.Body.String())
		return data
	}
	evidence := strings.Repeat("e", 64)
	body := fmt.Sprintf(`{"id":"AJ%s","user_id":%d,"amount_fen":250,"target":"","reason":"outage compensation","evidence":%q}`, strings.Repeat("1", 30), user.Id, evidence)
	t.Run("gate_disabled_and_non_admin", func(t *testing.T) {
		engine := gin.New()
		engine.POST("/as/:id", func(c *gin.Context) { c.Set("id", common.String2Int(c.Param("id"))); c.Next() }, RelayAdjustmentAPIGate, CreateRelayAdjustment)
		for _, test := range []struct {
			enabled string
			id      int
			status  int
		}{{"false", admin.Id, 503}, {"true", user.Id, 403}} {
			t.Setenv("RELAY_ADJUSTMENT_API", test.enabled)
			w := httptest.NewRecorder()
			engine.ServeHTTP(w, httptest.NewRequest("POST", fmt.Sprintf("/as/%d", test.id), strings.NewReader(body)))
			require.Equal(t, test.status, w.Code, w.Body.String())
		}
		require.Zero(t, quota())
	})
	t.Run("proof_is_bound_single_use_and_replay_is_idempotent", func(t *testing.T) {
		w := adminUserRequest("POST", "/adjustments", body, "", identity, common.RoleAdminUser, nil, CreateRelayAdjustment)
		require.Equal(t, 403, w.Code, w.Body.String())
		prepared := adminUserRequest("POST", "/adjustments?prepare=1", body, "", identity, common.RoleAdminUser, nil, CreateRelayAdjustment)
		require.Equal(t, 200, prepared.Code, prepared.Body.String())
		var decoded struct {
			Data struct {
				Verification service.VerificationOperation `json:"verification"`
			} `json:"data"`
		}
		require.NoError(t, json.Unmarshal(prepared.Body.Bytes(), &decoded))
		proof := issueSecurityEnrollmentProof(t, identity, decoded.Data.Verification, service.VerificationMethodTwoFA)
		tampered := strings.Replace(body, `"amount_fen":250`, `"amount_fen":2500`, 1)
		require.Equal(t, 403, adminUserRequest("POST", "/adjustments", tampered, proof, identity, common.RoleAdminUser, nil, CreateRelayAdjustment).Code)
		require.Zero(t, quota())
		w = adminUserRequest("POST", "/adjustments", body, proof, identity, common.RoleAdminUser, nil, CreateRelayAdjustment)
		require.Equal(t, 200, w.Code, w.Body.String())
		require.Equal(t, 2500000, quota())
		require.Equal(t, 403, adminUserRequest("POST", "/adjustments", body, proof, identity, common.RoleAdminUser, nil, CreateRelayAdjustment).Code)
		require.Equal(t, 2500000, quota())
	})
	t.Run("input_validation_list_and_funding_view", func(t *testing.T) {
		for _, bad := range []string{body + `{}`, strings.Replace(body, `"target":""`, `"target":"","extra":1`, 1), strings.Replace(body, `250`, `2.5`, 1)} {
			w := adminUserRequest("POST", "/adjustments", bad, "", identity, common.RoleAdminUser, nil, CreateRelayAdjustment)
			require.Equal(t, 400, w.Code, w.Body.String())
		}
		w := adminUserRequest("GET", fmt.Sprintf("/adjustments?user_id=%d", user.Id), "", "", identity, common.RoleAdminUser, nil, ListRelayAdjustments)
		require.Equal(t, 200, w.Code, w.Body.String())
		data := decode(t, w)["data"].(map[string]any)
		require.Equal(t, float64(1), data["total"])
		for _, query := range []string{"?p=0", "?user_id=01", "?other=1", "?p=1&p=2"} {
			require.Equal(t, 400, adminUserRequest("GET", "/adjustments"+query, "", "", identity, common.RoleAdminUser, nil, ListRelayAdjustments).Code, query)
		}
		params := gin.Params{{Key: "user", Value: fmt.Sprint(user.Id)}}
		w = adminUserRequest("GET", "/funding", "", "", identity, common.RoleAdminUser, params, GetRelayFunding)
		require.Equal(t, 200, w.Code, w.Body.String())
		view := decode(t, w)["data"].(map[string]any)
		require.Equal(t, true, view["consistent"])
		require.Equal(t, float64(2500000), view["balance"])
		sources := view["sources"].([]any)
		require.Len(t, sources, 1)
		require.Equal(t, "adj:AJ"+strings.Repeat("1", 30), sources[0].(map[string]any)["id"])
		require.Equal(t, float64(0), sources[0].(map[string]any)["refundable"])
		params = gin.Params{{Key: "user", Value: "x"}}
		require.Equal(t, 400, adminUserRequest("GET", "/funding", "", "", identity, common.RoleAdminUser, params, GetRelayFunding).Code)
	})
}
