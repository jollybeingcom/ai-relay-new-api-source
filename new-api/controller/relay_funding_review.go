package controller

import (
	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
)

// Admin-only maintenance endpoint, separate from the public refund gate so
// reconciliation can finish before refund applications are enabled.
func ReviewRelayFunding(c *gin.Context) {
	if !common.GetEnvOrDefaultBool("RELAY_WALLET_SCHEMA", false) || !common.GetEnvOrDefaultBool("RELAY_FUNDING_REVIEW", false) {
		relayPayJSON(c, 503, nil, "funding review unavailable")
		return
	}
	if !relayRefundAdmin(c) {
		return
	}
	var body struct {
		model.RelayFundingReview
		PlanHash        string `json:"plan_hash"`
		HistoryVerified bool   `json:"history_verified"`
	}
	if relayRefundDecode(c, &body) != nil || !body.HistoryVerified {
		relayRefundError(c, model.ErrRelayInput)
		return
	}
	// Preview rolls back; applying a reviewed plan requires a bound second factor.
	if body.PlanHash != "" {
		if !relayFinanceProof(c, "funding_review", body) {
			return
		}
	} else if len(c.Request.URL.Query()) != 0 {
		relayRefundError(c, model.ErrRelayInput)
		return
	}
	plan, err := model.ReviewRelayFunding(model.DB, c.GetInt("id"), body.RelayFundingReview, body.PlanHash)
	if err != nil {
		relayRefundError(c, err)
		return
	}
	relayPayJSON(c, 200, plan, "")
}
