package controller

import (
	"strconv"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
)

// GetRelayWalletUserUsage is admin-only (identity's user:read credential).
// It replaces a per-query user login, which hit New API's per-IP login limit.
func GetRelayWalletUserUsage(c *gin.Context) {
	if !common.GetEnvOrDefaultBool("RELAY_WALLET_SCHEMA", false) {
		relayPayJSON(c, 503, nil, "relay wallet unavailable")
		return
	}
	id, err := strconv.Atoi(c.Param("id"))
	page := 1
	query := c.Request.URL.Query()
	for key, values := range query {
		if key != "p" || len(values) != 1 {
			err = model.ErrRelayInput
		}
	}
	if raw := query.Get("p"); err == nil && raw != "" {
		page, err = strconv.Atoi(raw)
		if err == nil && strconv.Itoa(page) != raw {
			err = model.ErrRelayInput
		}
	}
	if err != nil || id <= 0 || strconv.Itoa(id) != c.Param("id") {
		relayRefundError(c, model.ErrRelayInput)
		return
	}
	items, total, err := model.ListRelayUsage(model.DB, id, page, 30, time.Now())
	if err != nil {
		relayRefundError(c, err)
		return
	}
	relayPayJSON(c, 200, gin.H{"items": items, "total": total, "page": page, "page_size": 20, "days": 30}, "")
}
