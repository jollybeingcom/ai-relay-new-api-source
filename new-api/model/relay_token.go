package model

import (
	"strings"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
)

// RelayPaidTokenPrefix names the identity service's paid credentials. Their
// remain_quota is a spending cap, not money: funds stay in users.quota.
const RelayPaidTokenPrefix = "relay_paid_"

type RelayTokenCap struct {
	TokenID     int   `json:"token_id"`
	Remain      int64 `json:"remain"`
	Replenished bool  `json:"replenished"`
}

// ReplenishRelayPaidToken restores a paid token's cap once it falls below a
// fifth. It runs under the wallet lock, so an in-flight reservation that has
// already reduced remain_quota is preserved: the cap is raised by a delta,
// never overwritten. Disabled tokens and accounts are left untouched so a
// revocation cannot be undone by maintenance.
func ReplenishRelayPaidToken(db *gorm.DB, userID, tokenID int, capQuota int64) (RelayTokenCap, error) {
	state := RelayTokenCap{TokenID: tokenID}
	if userID <= 0 || tokenID <= 0 || capQuota < 1 || capQuota > 2147483647 {
		return state, ErrRelayInput
	}
	err := relayWalletTransaction(db, func(tx *gorm.DB) error {
		var owner User
		if err := tx.Select("id").Where("id = ? AND role = ? AND status = ?", userID, common.RoleCommonUser, common.UserStatusEnabled).Take(&owner).Error; err != nil {
			return ErrRelayQuota
		}
		var token Token
		if err := lockForUpdate(tx).Where("id = ? AND user_id = ?", tokenID, userID).Take(&token).Error; err != nil {
			return err
		}
		if !strings.HasPrefix(token.Name, RelayPaidTokenPrefix) || token.UnlimitedQuota || token.ExpiredTime != -1 ||
			(token.Status != common.TokenStatusEnabled && token.Status != common.TokenStatusExhausted) || token.RemainQuota < 0 {
			return ErrRelayState
		}
		state.Remain = int64(token.RemainQuota)
		if state.Remain*5 >= capQuota {
			return nil
		}
		delta := capQuota - state.Remain
		result := tx.Model(&Token{}).Where("id = ? AND user_id = ? AND remain_quota = ?", tokenID, userID, token.RemainQuota).
			Updates(map[string]any{"remain_quota": gorm.Expr("remain_quota + ?", delta), "status": common.TokenStatusEnabled})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrRelayConflict
		}
		state.Remain, state.Replenished = capQuota, true
		return nil
	})
	return state, err
}

// RevokeRelayPaidToken disables a paid credential, e.g. after the chat account
// is deleted. Wallet balance, holds and refund obligations stay unchanged;
// requests already submitted still settle by their durable records.
func RevokeRelayPaidToken(db *gorm.DB, userID, tokenID int) error {
	if userID <= 0 || tokenID <= 0 {
		return ErrRelayInput
	}
	return relayWalletTransaction(db, func(tx *gorm.DB) error {
		var token Token
		if err := lockForUpdate(tx).Where("id = ? AND user_id = ?", tokenID, userID).Take(&token).Error; err != nil {
			return err
		}
		if !strings.HasPrefix(token.Name, RelayPaidTokenPrefix) {
			return ErrRelayState
		}
		if token.Status == common.TokenStatusDisabled {
			return nil
		}
		return tx.Model(&Token{}).Where("id = ? AND user_id = ?", tokenID, userID).Update("status", common.TokenStatusDisabled).Error
	})
}
