package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestRelayLegacyFundingAuth(t *testing.T) {
	setupDashboardAuthMiddlewareTest(t)
	gin.SetMode(gin.TestMode)
	createMiddlewarePATUser(t, "funding-user", "funding-user-token")
	admin := createMiddlewarePATUser(t, "funding-admin", "funding-admin-token")
	require.NoError(t, model.DB.Model(admin).Update("role", common.RoleAdminUser).Error)
	paths := []string{
		"/api/user/topup", "/api/user/pay", "/api/user/amount", "/api/user/stripe/pay",
		"/api/user/stripe/amount", "/api/user/creem/pay", "/api/user/waffo/pay",
		"/api/user/waffo/amount", "/api/user/waffo-pancake/pay", "/api/user/waffo-pancake/amount",
		"/api/user/aff_transfer", "/api/subscription/balance/pay", "/api/subscription/epay/pay",
		"/api/subscription/stripe/pay", "/api/subscription/creem/pay", "/api/subscription/waffo-pancake/pay",
	}
	for _, enabled := range []string{"true", "false"} {
		t.Setenv("RELAY_WALLET_BILLING", enabled)
		for _, path := range paths {
			for _, token := range []string{"funding-user-token", "funding-admin-token"} {
				t.Run(enabled+path+token, func(t *testing.T) {
					engine := gin.New()
					called := false
					engine.POST(path, UserAuth(), func(c *gin.Context) { called = true; c.Status(204) })
					r := httptest.NewRequest(http.MethodPost, path+"?ignored=true", nil)
					r.Header.Set("Authorization", "Bearer "+token)
					w := httptest.NewRecorder()
					engine.ServeHTTP(w, r)
					blocked := enabled == "true" && token == "funding-user-token"
					if blocked {
						require.Equal(t, 403, w.Code, w.Body.String())
						require.Contains(t, w.Body.String(), "RELAY_LEGACY_FUNDING_DISABLED")
					} else {
						require.Equal(t, 204, w.Code, w.Body.String())
					}
					require.Equal(t, !blocked, called)
				})
			}
		}
	}
	for _, route := range []struct{ method, path string }{
		{"GET", "/api/user/topup/self"}, {"GET", "/api/user/topup/info"},
		{"POST", "/api/user/epay/notify"}, {"POST", "/api/subscription/epay/notify"},
		{"POST", "/api/relay_pay/orders"}, {"POST", "/api/relay_pay/refunds"},
		{"POST", "/api/relay_pay/notify/alipay"}, {"POST", "/api/user/topup/complete"},
	} {
		require.False(t, RelayLegacyFundingRoute(route.method, route.path), route.path)
	}
}
