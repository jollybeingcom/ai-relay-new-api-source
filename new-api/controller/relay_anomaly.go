package controller

import (
	"time"

	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
)

// GetRelayAnomalies is the administrator's read-only exception list. It never
// includes chat content, credentials or merchant secrets.
func GetRelayAnomalies(c *gin.Context) {
	if len(c.Request.URL.Query()) != 0 {
		relayRiskError(c, model.ErrRelayInput)
		return
	}
	report, err := model.ReadRelayAnomalies(model.DB, time.Now())
	if err != nil {
		relayRiskError(c, err)
		return
	}
	relayPayJSON(c, 200, report, "")
}
