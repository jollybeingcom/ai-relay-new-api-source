package model

import (
	"time"

	"gorm.io/gorm"
)

// RelayUsageItem is the user-visible projection of one settled receipt.
// Channel, token and price-version identifiers stay server-side.
type RelayUsageItem struct {
	RequestID string `json:"request_id"`
	Model     string `json:"model"`
	Input     int    `json:"input"`
	CacheRead int    `json:"cache_read"`
	Output    int    `json:"output"`
	Actual    int64  `json:"actual"`
	CreatedAt int64  `json:"created_at"`
}

// ListRelayUsage pages a user's receipts from the last `days` days, newest
// first. Receipts are the settlement facts, so this needs no user session.
func ListRelayUsage(db *gorm.DB, userID, page, days int, now time.Time) ([]RelayUsageItem, int64, error) {
	items := []RelayUsageItem{}
	if userID <= 0 || page < 1 || page > 9999 || days < 1 || days > 90 {
		return items, 0, ErrRelayInput
	}
	query := db.Model(&RelayWalletReceipt{}).Where("user_id = ? AND created_at >= ? AND created_at <= ?", userID, now.Unix()-int64(days)*86400, now.Unix())
	var total int64
	if err := query.Session(&gorm.Session{}).Count(&total).Error; err != nil {
		return items, 0, err
	}
	err := query.Session(&gorm.Session{}).Select("request_id,model,input,cache_read,output,actual,created_at").
		Order("created_at DESC").Order("request_id DESC").Offset((page - 1) * 20).Limit(20).Scan(&items).Error
	return items, total, err
}
