package router

import (
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/middleware"
	"github.com/stretchr/testify/require"
)

func TestRelayLegacyFundingCoverage(t *testing.T) {
	engine := newAccessTokenScopeTestEngine(t)
	count := 0
	for _, route := range engine.Routes() {
		// Independently discover native user purchase routes; a new channel
		// must be reviewed and added to the guard before this test passes.
		candidate := route.Method == "POST" &&
			(strings.HasPrefix(route.Path, "/api/user/") || strings.HasPrefix(route.Path, "/api/subscription/")) &&
			(strings.HasSuffix(route.Path, "/pay") || strings.HasSuffix(route.Path, "/amount") ||
				route.Path == "/api/user/topup" || route.Path == "/api/user/aff_transfer")
		require.Equal(t, candidate, middleware.RelayLegacyFundingRoute(route.Method, route.Path), route.Method+" "+route.Path)
		if candidate {
			count++
		}
	}
	require.Equal(t, 16, count, "native funding routes changed; audit the new route before updating this inventory")
}
