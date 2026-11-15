package middleware

import (
	"net/http"

	"github.com/QuantumNous/new-api/common"
	"github.com/gin-gonic/gin"
)

// RelayLegacyFundingRoute covers native purchase and wallet credit entry points.
// Callbacks and read-only history remain available for existing obligations.
func RelayLegacyFundingRoute(method, path string) bool {
	if method != http.MethodPost {
		return false
	}
	switch path {
	case "/api/user/topup", "/api/user/pay", "/api/user/amount",
		"/api/user/stripe/pay", "/api/user/stripe/amount",
		"/api/user/creem/pay", "/api/user/waffo/pay", "/api/user/waffo/amount",
		"/api/user/waffo-pancake/pay", "/api/user/waffo-pancake/amount",
		"/api/user/aff_transfer",
		"/api/subscription/balance/pay", "/api/subscription/epay/pay",
		"/api/subscription/stripe/pay", "/api/subscription/creem/pay",
		"/api/subscription/waffo-pancake/pay":
		return true
	}
	return false
}

func allowRelayLegacyFunding(c *gin.Context, role int) bool {
	if common.GetEnvOrDefaultBool("RELAY_WALLET_BILLING", false) && role < common.RoleAdminUser &&
		RelayLegacyFundingRoute(c.Request.Method, c.FullPath()) {
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
			"success": false, "code": "RELAY_LEGACY_FUNDING_DISABLED",
			"message": "Please use the relay account top-up page.",
		})
		return false
	}
	return true
}
