package controller

import (
	"net/http"
	"strconv"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
)

// GetRelayWalletPrices publishes the immutable paid-chat price table in CNY.
// It never exposes channel IDs or upstream cost; disabled billing lists nothing.
func GetRelayWalletPrices(c *gin.Context) {
	prices, err := service.PublishedRelayWalletPrices()
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"success": false, "message": "price table unavailable"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": gin.H{"enabled": service.RelayWalletBillingEnabled(),
		"quota_per_cny": service.RelayQuotaPerCNY, "prices": prices}})
}

// GetRelayWalletUserHolds is admin-only; the account page shows the owner's
// own hold through the identity service, never another user's.
func GetRelayWalletUserHolds(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil || id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "invalid user id"})
		return
	}
	if !common.GetEnvOrDefaultBool("RELAY_WALLET_SCHEMA", false) {
		c.JSON(http.StatusOK, gin.H{"success": true, "data": model.RelayWalletHold{}})
		return
	}
	hold, err := model.GetRelayWalletHold(model.DB, id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"success": false, "message": "hold query failed"})
		return
	}
	var refundHeld int64
	if err := model.DB.Model(&model.RelayRefund{}).Where("user_id = ?", id).Select("COALESCE(SUM(frozen), 0)").Scan(&refundHeld).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"success": false, "message": "refund hold query failed"})
		return
	}
	// Keep generation holds unchanged for the funding allocator. The account
	// projection adds refund holds even when new refund applications are closed.
	c.JSON(http.StatusOK, gin.H{"success": true, "data": gin.H{"held": hold.Held, "active": hold.Active, "unknown": hold.Unknown, "refund_held": refundHeld}})
}
