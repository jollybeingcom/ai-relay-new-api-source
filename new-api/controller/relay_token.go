package controller

import (
	"strconv"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
)

// ReplenishRelayPaidToken is called by the identity service's administrator
// credential. It only raises a paid token's spending cap; it never adds money.
func ReplenishRelayPaidToken(c *gin.Context) {
	if !common.GetEnvOrDefaultBool("RELAY_WALLET_SCHEMA", false) {
		relayPayJSON(c, 503, nil, "relay wallet unavailable")
		return
	}
	userID, userErr := strconv.Atoi(c.Param("id"))
	tokenID, tokenErr := strconv.Atoi(c.Param("token"))
	var body struct {
		Cap int64 `json:"cap"`
	}
	if userErr != nil || tokenErr != nil || strconv.Itoa(userID) != c.Param("id") || strconv.Itoa(tokenID) != c.Param("token") ||
		len(c.Request.URL.Query()) != 0 || relayRefundDecode(c, &body) != nil {
		relayRefundError(c, model.ErrRelayInput)
		return
	}
	state, err := model.ReplenishRelayPaidToken(model.DB, userID, tokenID, body.Cap)
	if err != nil {
		relayRefundError(c, err)
		return
	}
	relayPayJSON(c, 200, state, "")
}

func RevokeRelayPaidToken(c *gin.Context) {
	if !common.GetEnvOrDefaultBool("RELAY_WALLET_SCHEMA", false) {
		relayPayJSON(c, 503, nil, "relay wallet unavailable")
		return
	}
	userID, userErr := strconv.Atoi(c.Param("id"))
	tokenID, tokenErr := strconv.Atoi(c.Param("token"))
	var body struct{}
	if userErr != nil || tokenErr != nil || strconv.Itoa(userID) != c.Param("id") || strconv.Itoa(tokenID) != c.Param("token") ||
		len(c.Request.URL.Query()) != 0 || relayRefundDecode(c, &body) != nil {
		relayRefundError(c, model.ErrRelayInput)
		return
	}
	if err := model.RevokeRelayPaidToken(model.DB, userID, tokenID); err != nil {
		relayRefundError(c, err)
		return
	}
	relayPayJSON(c, 200, gin.H{"revoked": true}, "")
}
