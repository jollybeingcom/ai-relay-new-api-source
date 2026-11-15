package controller

import (
	"strconv"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
)

// Audited adjustments are disabled by default and separate from the refund
// and risk gates, so finance can enable each capability independently.
func RelayAdjustmentAPIGate(c *gin.Context) {
	if !common.GetEnvOrDefaultBool("RELAY_WALLET_SCHEMA", false) || !common.GetEnvOrDefaultBool("RELAY_ADJUSTMENT_API", false) {
		relayPayJSON(c, 503, nil, "adjustments unavailable")
		c.Abort()
		return
	}
	if !relayRefundAdmin(c) {
		c.Abort()
		return
	}
	c.Next()
}

func CreateRelayAdjustment(c *gin.Context) {
	var input model.RelayAdjustmentInput
	if relayRefundDecode(c, &input) != nil {
		relayRefundError(c, model.ErrRelayInput)
		return
	}
	// The proof binds user, signed amount, target, reason and evidence hash.
	if !relayFinanceProof(c, "adjustment", input) {
		return
	}
	saved, err := model.ApplyRelayAdjustment(model.DB, c.GetInt("id"), input)
	if err != nil {
		relayRefundError(c, err)
		return
	}
	relayPayJSON(c, 200, saved, "")
}

func relayAdjustmentQuery(c *gin.Context, allowed ...string) (map[string]int, bool) {
	values := map[string]int{}
	for key, list := range c.Request.URL.Query() {
		ok := false
		for _, name := range allowed {
			ok = ok || key == name
		}
		if !ok || len(list) != 1 {
			return nil, false
		}
		value, err := strconv.Atoi(list[0])
		if err != nil || value < 1 || strconv.Itoa(value) != list[0] {
			return nil, false
		}
		values[key] = value
	}
	return values, true
}

func ListRelayAdjustments(c *gin.Context) {
	query, ok := relayAdjustmentQuery(c, "user_id", "p")
	if !ok {
		relayRefundError(c, model.ErrRelayInput)
		return
	}
	page := max(query["p"], 1)
	items, total, err := model.ListRelayAdjustments(model.DB, query["user_id"], page)
	if err != nil {
		relayRefundError(c, err)
		return
	}
	relayPayJSON(c, 200, gin.H{"items": items, "total": total, "page": page, "page_size": 20}, "")
}

func GetRelayFunding(c *gin.Context) {
	userID, err := strconv.Atoi(c.Param("user"))
	if err != nil || userID <= 0 || strconv.Itoa(userID) != c.Param("user") || len(c.Request.URL.Query()) != 0 {
		relayRefundError(c, model.ErrRelayInput)
		return
	}
	view, err := model.ReadRelayFunding(model.DB, userID)
	if err != nil {
		relayRefundError(c, err)
		return
	}
	relayPayJSON(c, 200, view, "")
}
