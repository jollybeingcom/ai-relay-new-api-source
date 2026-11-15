package model

import "gorm.io/gorm"

// RelayWalletHold summarizes funds the durable relay currently holds for one
// user. Held funds are already deducted from User.Quota; total = quota + held.
type RelayWalletHold struct {
	Held    int64 `json:"held"`
	Active  int64 `json:"active"`
	Unknown int64 `json:"unknown"`
}

func GetRelayWalletHold(db *gorm.DB, userID int) (RelayWalletHold, error) {
	var hold RelayWalletHold
	if userID <= 0 {
		return hold, ErrRelayInput
	}
	err := db.Model(&RelayWalletRequest{}).Where("user_id = ? AND state IN ?", userID, []string{"reserved", "submitted", "unknown"}).
		Select("COALESCE(SUM(reserved), 0) AS held, COUNT(*) AS active, COALESCE(SUM(CASE WHEN state = 'unknown' THEN 1 ELSE 0 END), 0) AS unknown").
		Scan(&hold).Error
	return hold, err
}
