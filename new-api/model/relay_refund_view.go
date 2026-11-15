package model

import (
	"errors"
	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
	"time"
)

// Only an empty, unused account may be initialized without historical review.
// A zero balance alone is insufficient: old spending must not be forgotten.
func InitializeEmptyRelayFunding(db *gorm.DB, userID int) error {
	if userID <= 0 {
		return ErrRelayInput
	}
	return relayWalletTransaction(db, func(tx *gorm.DB) error {
		var user User
		if err := tx.Where("id = ? AND role = ? AND status = ?", userID, common.RoleCommonUser, common.UserStatusEnabled).Take(&user).Error; err != nil {
			return err
		}
		var ready RelayFundingReady
		err := tx.Where("user_id = ?", userID).Take(&ready).Error
		if err == nil {
			return nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if user.Quota != 0 || user.UsedQuota != 0 || user.RequestCount != 0 {
			return ErrRelayFunding
		}
		for _, entity := range []any{&RelayPayOrder{}, &RelayWalletRequest{}, &RelayFundingEntry{}, &RelayRefund{}} {
			var count int64
			if err := tx.Model(entity).Where("user_id = ?", userID).Count(&count).Error; err != nil {
				return err
			}
			if count != 0 {
				return ErrRelayFunding
			}
		}
		return tx.Create(&RelayFundingReady{UserID: userID, CreatedAt: time.Now().Unix()}).Error
	})
}

// Refund holds are separate from generation reservations: the FIFO allocator
// already subtracts them from their original source.
type RelayRefundBalance struct {
	Ready          bool  `json:"ready"`
	Available      int64 `json:"available"`
	GenerationHeld int64 `json:"generation_held"`
	RefundHeld     int64 `json:"refund_held"`
}

type RelayRefundableOrder struct {
	ID            string `json:"id"`
	RefundableFen int64  `json:"refundable_fen"`
}

func ListRelayRefunds(db *gorm.DB, userID, page int) ([]RelayRefund, int64, error) {
	if userID < 0 || page < 1 || page > 1000 {
		return nil, 0, ErrRelayInput
	}
	query := db.Model(&RelayRefund{})
	// Zero is reserved for an authenticated billing administrator.
	if userID > 0 {
		query = query.Where("user_id = ?", userID)
	}
	var total int64
	if err := query.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	items := []RelayRefund{}
	err := query.Order("created_at DESC").Order("id DESC").Offset((page - 1) * 20).Limit(20).Find(&items).Error
	return items, total, err
}

func GetRelayRefundBalance(db *gorm.DB, userID, page int) (RelayRefundBalance, []RelayRefundableOrder, int64, error) {
	var balance RelayRefundBalance
	items := []RelayRefundableOrder{}
	var total int64
	if userID <= 0 || page < 1 || page > 1000 {
		return balance, items, 0, ErrRelayInput
	}
	err := relayWalletTransaction(db, func(tx *gorm.DB) error {
		var user User
		if err := tx.Where("id = ? AND role = ?", userID, common.RoleCommonUser).Take(&user).Error; err != nil {
			return err
		}
		balance.Available = int64(user.Quota)
		hold, err := GetRelayWalletHold(tx, userID)
		if err != nil {
			return err
		}
		balance.GenerationHeld = hold.Held
		if err := tx.Model(&RelayRefund{}).Where("user_id = ?", userID).Select("COALESCE(SUM(frozen), 0)").Scan(&balance.RefundHeld).Error; err != nil {
			return err
		}
		var ready RelayFundingReady
		err = tx.Where("user_id = ?", userID).Take(&ready).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		allocations, err := relayFundingSnapshot(tx, userID)
		if err != nil {
			return err
		}
		balance.Ready = user.Status == common.UserStatusEnabled
		orders, count, err := ListRelayPayOrders(tx, userID, page)
		if err != nil {
			return err
		}
		total = count
		for _, order := range orders {
			item := RelayRefundableOrder{ID: order.ID}
			var active int64
			if err := tx.Model(&RelayRefund{}).Where("order_id = ? AND state IN ?", order.ID, relayRefundActive).Count(&active).Error; err != nil {
				return err
			}
			if balance.Ready && active == 0 && order.Credit == "credited" && order.RateVersion == "cny-1e6-v1" {
				for _, a := range allocations {
					if a.ID == order.ID {
						item.RefundableFen = a.Refundable / 10000
					}
				}
			}
			items = append(items, item)
		}
		return nil
	})
	return balance, items, total, err
}
