package router

import (
	"github.com/QuantumNous/new-api/middleware"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestRelayRefundScopes(t *testing.T) {
	for route, scope := range map[string]string{
		"POST /api/relay_pay/admin/funding/review":                "billing:write",
		"GET /api/relay_pay/refunds":                              "wallet:read",
		"GET /api/relay_pay/refunds/:id":                          "wallet:read",
		"GET /api/relay_pay/refunds/balance":                      "wallet:read",
		"POST /api/relay_pay/refunds":                             "wallet:write",
		"GET /api/relay_pay/user/:user/refunds":                   "user:read",
		"GET /api/relay_pay/user/:user/refunds/:id":               "user:read",
		"GET /api/relay_pay/user/:user/refunds/balance":           "user:read",
		"POST /api/relay_pay/user/:user/refunds":                  "user:write",
		"POST /api/relay_pay/user/:user/refunds/initialize-empty": "user:write",
		"GET /api/relay_pay/admin/refunds":                        "billing:read",
		"POST /api/relay_pay/admin/refunds/:id/review":            "billing:write",
	} {
		rule, ok := middleware.AccessTokenRouteRule(route)
		require.True(t, ok, route)
		require.Equal(t, "scope", rule.Kind(), route)
		require.Equal(t, scope, rule.Scope(), route)
	}
}
