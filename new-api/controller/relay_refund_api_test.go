package controller

import (
	"context"
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

func TestRelayRefundAPI(t *testing.T) {
	f := newRelayPayControllerFixture(t, "mock")
	t.Setenv("RELAY_REFUND_API", "true")
	admin := model.User{Username: "refund-admin", AffCode: "rfadmin", Role: common.RoleAdminUser, Status: common.UserStatusEnabled}
	require.NoError(t, model.DB.Create(&admin).Error)
	require.NoError(t, model.DB.AutoMigrate(&model.UserSession{}, &model.AuthFlow{}, &model.TwoFA{}, &model.PasskeyCredential{}))
	require.NoError(t, model.DB.Create(&model.TwoFA{UserId: admin.Id, Secret: "JBSWY3DPEHPK3PXP", IsEnabled: true}).Error)
	bundle, err := service.CreateLoginSession(admin.Id, "password", "127.0.0.1", "isolated-refund-test")
	require.NoError(t, err)
	identity, err := service.ParseAccessToken(bundle.AccessToken)
	require.NoError(t, err)
	verified := func(path, body string) (int, map[string]any) {
		t.Helper()
		handler := ReviewRelayFunding
		var params gin.Params
		if strings.HasPrefix(path, "/admin/review/") {
			handler = ReviewRelayRefund
			params = gin.Params{{Key: "id", Value: strings.TrimPrefix(path, "/admin/review/")}}
		}
		prepared := adminUserRequest("POST", path+"?prepare=1", body, "", identity, common.RoleAdminUser, params, handler)
		require.Equal(t, 200, prepared.Code, prepared.Body.String())
		var decoded struct {
			Data struct {
				Verification service.VerificationOperation `json:"verification"`
			} `json:"data"`
		}
		require.NoError(t, json.Unmarshal(prepared.Body.Bytes(), &decoded))
		proof := issueSecurityEnrollmentProof(t, identity, decoded.Data.Verification, service.VerificationMethodTwoFA)
		// The same proof cannot authorize a different object or plan.
		wrong := body
		if len(params) > 0 {
			other := gin.Params{{Key: "id", Value: "RF" + strings.Repeat("f", 30)}}
			require.Equal(t, 403, adminUserRequest("POST", path, wrong, proof, identity, common.RoleAdminUser, other, handler).Code)
		} else {
			wrong = strings.Replace(body, `"plan_hash":"`, `"plan_hash":"0`, 1)
			require.NotEqual(t, body, wrong)
			require.Equal(t, 403, adminUserRequest("POST", path, wrong, proof, identity, common.RoleAdminUser, params, handler).Code)
		}
		response := adminUserRequest("POST", path, body, proof, identity, common.RoleAdminUser, params, handler)
		require.Equal(t, 403, adminUserRequest("POST", path, body, proof, identity, common.RoleAdminUser, params, handler).Code)
		var data map[string]any
		require.NoError(t, json.Unmarshal(response.Body.Bytes(), &data))
		return response.Code, data
	}
	engine := gin.New()
	for i, u := range f.users {
		id := u.Id
		group := engine.Group(fmt.Sprintf("/u%d", i), func(c *gin.Context) { c.Set("id", id); c.Next() }, RelayRefundAPIGate)
		group.POST("/refunds", CreateRelayRefund)
		group.GET("/refunds", ListRelayRefunds)
		group.GET("/refunds/balance", GetRelayRefundBalance)
		group.GET("/refunds/:id", GetRelayRefund)
		// Intentionally bypass upstream auth here to verify DB-role defence.
		group.POST("/review/:id", ReviewRelayRefund)
		group.GET("/admin", ListAdminRelayRefunds)
	}
	ag := engine.Group("/admin", func(c *gin.Context) { c.Set("id", admin.Id); c.Next() }, RelayRefundAPIGate)
	ag.GET("/refunds", ListAdminRelayRefunds)
	ag.POST("/funding", ReviewRelayFunding)
	ag.POST("/review/:id", ReviewRelayRefund)
	server := httptest.NewServer(engine)
	defer server.Close()
	f.gateway = server.URL
	channel, err := service.CurrentRelayPayChannel()
	require.NoError(t, err)
	order, err := service.NewRelayPayOrder(f.users[0].Id, 10, channel)
	require.NoError(t, err)
	order, err = service.MockRelayPaySuccess(order)
	require.NoError(t, err)
	t.Run("fresh_initialization_replays_and_history_is_not_guessed", func(t *testing.T) {
		require.NoError(t, model.InitializeEmptyRelayFunding(model.DB, f.users[1].Id))
		require.NoError(t, model.InitializeEmptyRelayFunding(model.DB, f.users[1].Id))
		require.ErrorIs(t, model.InitializeEmptyRelayFunding(model.DB, f.users[0].Id), model.ErrRelayFunding)
	})
	rid := "RF" + strings.Repeat("1", 30)
	body := fmt.Sprintf(`{"id":%q,"order_id":%q,"amount_fen":300}`, rid, order.ID)
	t.Run("unreconciled_funds_rejected", func(t *testing.T) {
		status, data := f.call(t, "GET", "/u0/refunds/balance", "")
		require.Equal(t, 200, status, data)
		require.Equal(t, false, data["data"].(map[string]any)["balance"].(map[string]any)["ready"])
		status, _ = f.call(t, "POST", "/u0/refunds", body)
		require.Equal(t, 409, status)
	})
	require.NoError(t, model.InitializeRelayFunding(model.DB, f.users[0].Id, 0, "reviewed test history"))
	t.Run("owner_scope_input_and_replay", func(t *testing.T) {
		status, _ := f.call(t, "POST", "/u1/refunds", body)
		require.Equal(t, 404, status)
		for _, bad := range []string{body + `{}`, strings.Replace(body, `300`, `3.1`, 1), strings.Replace(body, `300`, `-1`, 1), strings.Replace(body, `300`, `300,"user_id":1`, 1)} {
			status, _ = f.call(t, "POST", "/u0/refunds", bad)
			require.Equal(t, 400, status)
		}
		for range 2 {
			status, _ = f.call(t, "POST", "/u0/refunds", body)
			require.Equal(t, 200, status)
		}
		status, _ = f.call(t, "GET", "/u1/refunds/"+rid, "")
		require.Equal(t, 404, status)
		status, data := f.call(t, "GET", "/u1/refunds", "")
		require.Equal(t, 200, status)
		require.Equal(t, float64(0), data["data"].(map[string]any)["total"])
		status, _ = f.call(t, "GET", "/u0/refunds?p=0", "")
		require.Equal(t, 400, status)
	})
	t.Run("admin_review_and_separate_frozen_balance", func(t *testing.T) {
		review := `{"decision":"approve","reason":"private internal note"}`
		status, _ := f.call(t, "POST", "/u0/review/"+rid, review)
		require.Equal(t, 403, status)
		status, _ = f.call(t, "GET", "/u0/admin", "")
		require.Equal(t, 403, status)
		status, _ = f.call(t, "POST", "/admin/review/"+rid, `{"reason":"no decision"}`)
		require.Equal(t, 400, status)
		for range 2 {
			status, _ = f.call(t, "POST", "/admin/review/"+rid, review)
			require.Equal(t, 403, status)
			status, _ = verified("/admin/review/"+rid, review)
			require.Equal(t, 200, status)
		}
		require.Equal(t, 7000000, f.quota(t, 0))
		status, data := f.call(t, "GET", "/u0/refunds/balance", "")
		require.Equal(t, 200, status, data)
		b := data["data"].(map[string]any)["balance"].(map[string]any)
		require.Equal(t, float64(7000000), b["available"])
		require.Equal(t, float64(3000000), b["refund_held"])
		require.Equal(t, float64(0), b["generation_held"])
		status, data = f.call(t, "GET", "/u0/refunds/"+rid, "")
		require.Equal(t, 200, status)
		raw, _ := json.Marshal(data)
		require.NotContains(t, string(raw), "private internal note")
		require.NotContains(t, string(raw), "reviewer_id")
		status, data = f.call(t, "GET", "/admin/refunds", "")
		require.Equal(t, 200, status)
		raw, _ = json.Marshal(data)
		require.Contains(t, string(raw), "private internal note")
	})
	t.Run("approved_HTTP_request_is_completed_by_mock_worker_once", func(t *testing.T) {
		t.Setenv("RELAY_REFUND_MOCK_RESULT", "success")
		changed, err := service.SyncRelayRefunds(context.Background(), 50)
		require.NoError(t, err)
		require.Equal(t, 1, changed)
		changed, err = service.SyncRelayRefunds(context.Background(), 50)
		require.NoError(t, err)
		require.Zero(t, changed)
		status, data := f.call(t, "GET", "/u0/refunds/"+rid, "")
		require.Equal(t, 200, status)
		require.Equal(t, "succeeded", data["data"].(map[string]any)["state"])
		require.Equal(t, 7000000, f.quota(t, 0))
		status, data = f.call(t, "GET", "/u0/refunds/balance", "")
		require.Equal(t, 200, status)
		require.Equal(t, float64(0), data["data"].(map[string]any)["balance"].(map[string]any)["refund_held"])
	})
	t.Run("funding_preview_gate_and_application", func(t *testing.T) {
		u := model.User{Username: "history-user", AffCode: "huser", Role: common.RoleCommonUser, Status: common.UserStatusEnabled}
		require.NoError(t, model.DB.Create(&u).Error)
		body := map[string]any{"user_id": u.Id, "opening_quota": 0, "reason": "verified empty history", "evidence": strings.Repeat("d", 64), "history_verified": true}
		raw, _ := json.Marshal(body)
		status, _ := f.call(t, "POST", "/admin/funding", string(raw))
		require.Equal(t, 503, status)
		t.Setenv("RELAY_FUNDING_REVIEW", "true")
		status, data := f.call(t, "POST", "/admin/funding", string(raw))
		require.Equal(t, 200, status, data)
		body["plan_hash"] = data["data"].(map[string]any)["plan_hash"]
		raw, _ = json.Marshal(body)
		status, data = f.call(t, "POST", "/admin/funding", string(raw))
		require.Equal(t, 403, status)
		status, data = verified("/admin/funding", string(raw))
		require.Equal(t, 200, status, data)
		require.Equal(t, true, data["data"].(map[string]any)["applied"])
	})
	t.Run("disabled_admin_and_api_gate", func(t *testing.T) {
		require.NoError(t, model.DB.Model(&model.User{}).Where("id = ?", admin.Id).Update("status", common.UserStatusDisabled).Error)
		status, _ := f.call(t, "GET", "/admin/refunds", "")
		require.Equal(t, 403, status)
		t.Setenv("RELAY_REFUND_API", "false")
		status, _ = f.call(t, "GET", "/u0/refunds", "")
		require.Equal(t, 503, status)
	})
}
