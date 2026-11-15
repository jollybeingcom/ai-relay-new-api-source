package model

import (
	"errors"
	"regexp"
	"strconv"
	"time"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
)

type RelayFundingEntry struct {
	ID         string `gorm:"type:varchar(64);primaryKey"`
	UserID     int    `gorm:"not null;index"`
	Kind       string `gorm:"type:varchar(16);not null"`
	Quota      int64  `gorm:"not null"`
	OccurredAt int64  `gorm:"not null"`
	Reason     string `gorm:"type:varchar(128);not null"`
}

func (*RelayFundingEntry) BeforeUpdate(*gorm.DB) error { return ErrRelayState }
func (*RelayFundingEntry) BeforeDelete(*gorm.DB) error { return ErrRelayState }

// Sources must be explicitly reconciled before enabling refunds for a user.
// This marker prevents inferring cash from a missing history on later requests.
type RelayFundingReady struct {
	UserID     int    `gorm:"primaryKey;autoIncrement:false"`
	CreatedAt  int64  `gorm:"not null"`
	ReviewerID int    `gorm:"not null;default:0"`
	Reason     string `gorm:"type:varchar(128);not null;default:''"`
	Evidence   string `gorm:"type:varchar(64);not null;default:''"`
	PlanHash   string `gorm:"type:varchar(64);not null;default:''"`
}

func (*RelayFundingReady) BeforeUpdate(*gorm.DB) error { return ErrRelayState }
func (*RelayFundingReady) BeforeDelete(*gorm.DB) error { return ErrRelayState }

func GetRelayRefund(db *gorm.DB, userID int, id string) (RelayRefund, error) {
	var refund RelayRefund
	if userID <= 0 || !relayRefundID.MatchString(id) {
		return refund, ErrRelayInput
	}
	err := db.Where("id = ? AND user_id = ?", id, userID).Take(&refund).Error
	return refund, err
}

type RelayRefund struct {
	ID            string `gorm:"type:varchar(32);primaryKey"`
	OrderID       string `gorm:"type:varchar(32);not null;index"`
	UserID        int    `gorm:"not null;index"`
	AmountFen     int64  `gorm:"not null"`
	Quota         int64  `gorm:"not null"`
	State         string `gorm:"type:varchar(24);not null;index"`
	Frozen        int64  `gorm:"not null"`
	ReviewerID    int    `gorm:"not null"`
	Reason        string `gorm:"type:varchar(256);not null"`
	CreatedAt     int64  `gorm:"not null"`
	UpdatedAt     int64  `gorm:"not null"`
	SubmittedAt   int64  `gorm:"not null;default:0"`
	NextQueryAt   int64  `gorm:"not null;default:0;index"`
	QueryAttempts int    `gorm:"not null;default:0"`
	NeedsReview   bool   `gorm:"not null;default:false"`
}

type RelayRefundEvent struct {
	ID         int64  `gorm:"primaryKey"`
	RefundID   string `gorm:"type:varchar(32);not null;uniqueIndex:relay_refund_event,priority:1"`
	Kind       string `gorm:"type:varchar(24);not null;uniqueIndex:relay_refund_event,priority:2"`
	QuotaDelta int64  `gorm:"not null"`
	Evidence   string `gorm:"type:varchar(64);not null"`
	CreatedAt  int64  `gorm:"not null"`
}

func (r *RelayRefund) BeforeUpdate(tx *gorm.DB) error {
	fields, ok := tx.Statement.Dest.(map[string]any)
	if !ok {
		return ErrRelayInput
	}
	for field := range fields {
		switch field {
		case "state", "frozen", "reviewer_id", "reason", "updated_at", "submitted_at", "next_query_at", "query_attempts", "needs_review":
		default:
			return ErrRelayConflict
		}
	}
	return nil
}
func (*RelayRefund) BeforeDelete(*gorm.DB) error      { return ErrRelayState }
func (*RelayRefundEvent) BeforeUpdate(*gorm.DB) error { return ErrRelayState }
func (*RelayRefundEvent) BeforeDelete(*gorm.DB) error { return ErrRelayState }

var relayRefundID = regexp.MustCompile(`^RF[0-9a-f]{30}$`)
var relayRefundActive = []string{"requested", "approved_frozen", "processing", "unknown"}

func MigrateRelayRefund(db *gorm.DB) error {
	if err := db.AutoMigrate(&RelayFundingEntry{}, &RelayFundingReady{}, &RelayRefund{}, &RelayRefundEvent{}); err != nil {
		return err
	}
	return MigrateRelayAdjustment(db)
}

// InitializeRelayFunding imports an audited opening adjustment and credited
// order snapshots. It never changes users.quota. The caller must supply the
// verified historical NON-CASH funding, including already consumed adjustments.
// An automatic guess from today's balance would lose history and is forbidden.
func InitializeRelayFunding(db *gorm.DB, userID int, openingAdjustment int64, reason string) error {
	return relayWalletTransaction(db, func(tx *gorm.DB) error {
		return initializeRelayFunding(tx, userID, openingAdjustment, reason, &RelayFundingReady{UserID: userID, CreatedAt: time.Now().Unix(), Reason: reason})
	})
}

func initializeRelayFunding(tx *gorm.DB, userID int, openingAdjustment int64, reason string, marker *RelayFundingReady) error {
	if userID <= 0 || openingAdjustment < 0 || openingAdjustment > common.MaxWalletQuota || reason == "" || len(reason) > 128 {
		return ErrRelayInput
	}
	var ready RelayFundingReady
	err := tx.Where("user_id = ?", userID).Take(&ready).Error
	if err == nil {
		return ErrRelayConflict
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return err
	}
	var user User
	if err := tx.Where("id = ? AND role = ?", userID, common.RoleCommonUser).Take(&user).Error; err != nil {
		return err
	}
	var orders []RelayPayOrder
	if err := tx.Where("user_id = ? AND credit = ?", userID, "credited").Find(&orders).Error; err != nil {
		return err
	}
	for _, order := range orders {
		entry := RelayFundingEntry{ID: order.ID, UserID: userID, Kind: "order", Quota: order.Quota, OccurredAt: order.PaidAt, Reason: "credited-order-backfill"}
		var existing RelayFundingEntry
		err := tx.Where("id = ?", order.ID).Take(&existing).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			if err := tx.Create(&entry).Error; err != nil {
				return err
			}
		} else if err != nil {
			return err
		} else if existing.UserID != userID || existing.Kind != "order" || existing.Quota != order.Quota || existing.OccurredAt != order.PaidAt {
			return ErrRelayFunding
		}
	}
	if openingAdjustment > 0 {
		// User-scoped opening keys cannot collide with RP order identifiers.
		entry := RelayFundingEntry{ID: "opening:" + strconv.Itoa(userID), UserID: userID, Kind: "adjustment", Quota: openingAdjustment, OccurredAt: 0, Reason: reason}
		if len(entry.ID) > 64 {
			return ErrRelayInput
		}
		if err := tx.Create(&entry).Error; err != nil {
			return err
		}
	}
	if err := tx.Create(marker).Error; err != nil {
		return err
	}
	allocations, err := relayFundingSnapshot(tx, userID)
	if err != nil {
		return err
	}
	var consumed int64
	for _, a := range allocations {
		consumed += a.Consumed
	}
	var actual int64
	if err := tx.Model(&RelayWalletRequest{}).Where("user_id = ? AND state = ?", userID, "settled").Select("COALESCE(SUM(actual), 0)").Scan(&actual).Error; err != nil {
		return err
	}
	if consumed != actual {
		return ErrRelayFunding
	}
	return nil
}

func relayFundingSnapshot(tx *gorm.DB, userID int) ([]RelayFundingAllocation, error) {
	var ready RelayFundingReady
	if err := tx.Where("user_id = ?", userID).Take(&ready).Error; err != nil {
		return nil, ErrRelayFunding
	}
	var user User
	if err := tx.Where("id = ?", userID).Take(&user).Error; err != nil {
		return nil, err
	}
	var entries []RelayFundingEntry
	if err := tx.Where("user_id = ?", userID).Find(&entries).Error; err != nil {
		return nil, err
	}
	var refunds []RelayRefund
	if err := tx.Where("user_id = ?", userID).Find(&refunds).Error; err != nil {
		return nil, err
	}
	debits, err := relayAdjustmentDebits(tx, userID)
	if err != nil {
		return nil, err
	}
	sources := make([]RelayFundingSource, 0, len(entries))
	for _, entry := range entries {
		source := RelayFundingSource{ID: entry.ID, Kind: entry.Kind, Quota: entry.Quota, PaidAt: entry.OccurredAt, Debited: debits[entry.ID]}
		delete(debits, entry.ID)
		for _, refund := range refunds {
			if refund.OrderID != entry.ID {
				continue
			}
			if refund.State == "succeeded" {
				source.Refunded += refund.Quota
			}
			source.Frozen += refund.Frozen
			if source.Refunded < 0 || source.Frozen < 0 || source.Refunded > source.Quota || source.Frozen > source.Quota-source.Refunded {
				return nil, ErrRelayFunding
			}
		}
		sources = append(sources, source)
	}
	if len(debits) != 0 {
		return nil, ErrRelayFunding // A debit must name one of this user's sources.
	}
	hold, err := GetRelayWalletHold(tx, userID)
	if err != nil {
		return nil, err
	}
	allocations, err := AllocateRelayFunding(sources, int64(user.Quota), hold.Held)
	if err != nil {
		return nil, err
	}
	var consumed, actual int64
	for _, a := range allocations {
		consumed += a.Consumed
	}
	if err := tx.Model(&RelayWalletRequest{}).Where("user_id = ? AND state = ?", userID, "settled").Select("COALESCE(SUM(actual), 0)").Scan(&actual).Error; err != nil {
		return nil, err
	}
	if consumed != actual {
		return nil, ErrRelayFunding
	}
	return allocations, nil
}

func refundAvailable(tx *gorm.DB, userID int, orderID string) (int64, error) {
	allocations, err := relayFundingSnapshot(tx, userID)
	if err != nil {
		return 0, err
	}
	for _, a := range allocations {
		if a.ID == orderID {
			return a.Refundable, nil
		}
	}
	return 0, ErrRelayFunding
}

func RequestRelayRefund(db *gorm.DB, userID int, id, orderID string, amountFen int64) (RelayRefund, error) {
	var refund RelayRefund
	if userID <= 0 || !relayRefundID.MatchString(id) || !relayPayOrderID.MatchString(orderID) || amountFen <= 0 || amountFen > common.MaxWalletQuota/10000 {
		return refund, ErrRelayInput
	}
	err := relayWalletTransaction(db, func(tx *gorm.DB) error {
		var owner User
		if err := tx.Where("id = ? AND role = ? AND status = ?", userID, common.RoleCommonUser, common.UserStatusEnabled).Take(&owner).Error; err != nil {
			return ErrRelayQuota
		}
		var order RelayPayOrder
		if err := lockForUpdate(tx).Where("id = ? AND user_id = ? AND credit = ?", orderID, userID, "credited").Take(&order).Error; err != nil {
			return err
		}
		err := lockForUpdate(tx).Where("id = ?", id).Take(&refund).Error
		if err == nil {
			if refund.UserID != userID || refund.OrderID != orderID || refund.AmountFen != amountFen {
				return ErrRelayConflict
			}
			return nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if order.RateVersion != "cny-1e6-v1" || order.Quota != order.AmountFen*10000 || amountFen > order.AmountFen {
			return ErrRelayInput
		}
		var active int64
		if err := tx.Model(&RelayRefund{}).Where("order_id = ? AND state IN ?", orderID, relayRefundActive).Count(&active).Error; err != nil {
			return err
		}
		if active != 0 {
			return ErrRelayConflict
		}
		available, err := refundAvailable(tx, userID, orderID)
		if err != nil {
			return err
		}
		if amountFen*10000 > available {
			return ErrRelayQuota
		}
		now := time.Now().Unix()
		refund = RelayRefund{ID: id, OrderID: orderID, UserID: userID, AmountFen: amountFen, Quota: amountFen * 10000, State: "requested", CreatedAt: now, UpdatedAt: now}
		if err := tx.Create(&refund).Error; err != nil {
			return err
		}
		return tx.Create(&RelayRefundEvent{RefundID: id, Kind: "requested", CreatedAt: now}).Error
	})
	return refund, err
}

// ReviewRelayRefund requires server-authenticated administrator identity.
// Approval and request reservations share the same wallet lock.
func ReviewRelayRefund(db *gorm.DB, id string, reviewerID int, reason string, approve bool) error {
	if !relayRefundID.MatchString(id) || reviewerID <= 0 || reason == "" || len(reason) > 256 {
		return ErrRelayInput
	}
	return relayWalletTransaction(db, func(tx *gorm.DB) error {
		var admin User
		if err := tx.Where("id = ? AND role >= ? AND status = ?", reviewerID, common.RoleAdminUser, common.UserStatusEnabled).Take(&admin).Error; err != nil {
			return ErrRelayQuota
		}
		var refund RelayRefund
		if err := lockForUpdate(tx).Where("id = ?", id).Take(&refund).Error; err != nil {
			return err
		}
		if refund.State != "requested" {
			if approve && (refund.State == "approved_frozen" || refund.State == "processing" || refund.State == "unknown" || refund.State == "succeeded") || !approve && refund.State == "rejected" {
				return nil
			}
			return ErrRelayState
		}
		state, frozen := "rejected", int64(0)
		if approve {
			available, err := refundAvailable(tx, refund.UserID, refund.OrderID)
			if err != nil {
				return err
			}
			if available < refund.Quota {
				return ErrRelayQuota
			}
			result := tx.Model(&User{}).Where("id = ? AND quota >= ?", refund.UserID, refund.Quota).Update("quota", gorm.Expr("quota - ?", refund.Quota))
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected != 1 {
				return ErrRelayQuota
			}
			state, frozen = "approved_frozen", refund.Quota
		}
		now := time.Now().Unix()
		if err := tx.Model(&RelayRefund{}).Where("id = ?", id).Updates(map[string]any{"state": state, "frozen": frozen, "reviewer_id": reviewerID, "reason": reason, "updated_at": now}).Error; err != nil {
			return err
		}
		return tx.Create(&RelayRefundEvent{RefundID: id, Kind: state, QuotaDelta: -frozen, CreatedAt: now}).Error
	})
}

// ClaimRelayRefund commits processing before external I/O. A crash after this
// point must use QueryRefund; a second worker must not send another Refund.
func ClaimRelayRefund(db *gorm.DB, id string) (bool, error) {
	claimed := false
	err := relayWalletTransaction(db, func(tx *gorm.DB) error {
		now := time.Now().Unix()
		result := tx.Model(&RelayRefund{}).Where("id = ? AND state = ?", id, "approved_frozen").Updates(map[string]any{"state": "processing", "updated_at": now, "submitted_at": now, "next_query_at": now + 60})
		if result.Error != nil {
			return result.Error
		}
		claimed = result.RowsAffected == 1
		if !claimed {
			return nil
		}
		return tx.Create(&RelayRefundEvent{RefundID: id, Kind: "processing", CreatedAt: time.Now().Unix()}).Error
	})
	return claimed && err == nil, err
}

func DueRelayRefunds(db *gorm.DB, limit int) ([]RelayRefund, error) {
	if limit < 1 || limit > 50 {
		return nil, ErrRelayInput
	}
	var refunds []RelayRefund
	err := db.Where("state IN ? AND next_query_at <= ?", []string{"approved_frozen", "processing", "unknown"}, time.Now().Unix()).Order("next_query_at").Order("id").Limit(limit).Find(&refunds).Error
	return refunds, err
}

func DeferRelayRefundReview(db *gorm.DB, id string) error {
	return relayWalletTransaction(db, func(tx *gorm.DB) error {
		return tx.Model(&RelayRefund{}).Where("id = ? AND state IN ?", id, []string{"approved_frozen", "processing", "unknown"}).Updates(map[string]any{"needs_review": true, "next_query_at": time.Now().Unix() + 3600}).Error
	})
}

// Persist backoff BEFORE external I/O; concurrent workers cannot query the
// same due refund. A crash consumes the slot but cannot clear the frozen money.
func ClaimRelayRefundQuery(db *gorm.DB, id string) (bool, error) {
	claimed := false
	err := relayWalletTransaction(db, func(tx *gorm.DB) error {
		var refund RelayRefund
		if err := lockForUpdate(tx).Where("id = ?", id).Take(&refund).Error; err != nil {
			return err
		}
		now := time.Now().Unix()
		if (refund.State != "processing" && refund.State != "unknown") || refund.NextQueryAt > now {
			return nil
		}
		if refund.QueryAttempts < 0 {
			return ErrRelayState
		}
		delay := min(int64(60)<<min(refund.QueryAttempts, 6), int64(3600))
		review := refund.NeedsReview || (refund.SubmittedAt > 0 && refund.SubmittedAt <= now-86400)
		if err := tx.Model(&RelayRefund{}).Where("id = ?", id).Updates(map[string]any{"next_query_at": now + delay, "query_attempts": min(refund.QueryAttempts+1, 1000), "needs_review": review}).Error; err != nil {
			return err
		}
		claimed = true
		return nil
	})
	return claimed && err == nil, err
}

type RelayRefundFact struct {
	RefundID, OrderID, Channel, App, TradeNo, Status, Evidence string
	AmountFen                                                  int64
}

// Only verified signed channel results may enter this function. Transport
// failures must be represented as unknown by the trusted service adapter.
func ApplyRelayRefundFact(db *gorm.DB, fact RelayRefundFact) error {
	if !relayRefundID.MatchString(fact.RefundID) || !relayPayEvidence.MatchString(fact.Evidence) || (fact.Status != "succeeded" && fact.Status != "failed_released" && fact.Status != "unknown") {
		return ErrRelayInput
	}
	return relayWalletTransaction(db, func(tx *gorm.DB) error {
		var order RelayPayOrder
		if err := lockForUpdate(tx).Where("id = ?", fact.OrderID).Take(&order).Error; err != nil {
			return err
		}
		var refund RelayRefund
		if err := lockForUpdate(tx).Where("id = ? AND order_id = ?", fact.RefundID, fact.OrderID).Take(&refund).Error; err != nil {
			return err
		}
		if order.TradeNo == nil || *order.TradeNo != fact.TradeNo || order.Channel != fact.Channel || order.MerchantApp != fact.App || refund.AmountFen != fact.AmountFen {
			return ErrRelayPayMismatch
		}
		if refund.State == fact.Status {
			return nil
		}
		if refund.State != "processing" && refund.State != "unknown" {
			return ErrRelayState
		}
		if refund.Frozen != refund.Quota {
			return ErrRelayFunding
		}
		frozen, delta := refund.Frozen, int64(0)
		if fact.Status == "failed_released" {
			result := tx.Model(&User{}).Where("id = ? AND quota >= 0 AND quota <= ?", refund.UserID, common.MaxWalletQuota-refund.Quota).Update("quota", gorm.Expr("quota + ?", refund.Quota))
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected != 1 {
				return ErrRelayQuota
			}
			delta = refund.Quota
		}
		if fact.Status != "unknown" {
			frozen = 0
		}
		now := time.Now().Unix()
		if err := tx.Model(&RelayRefund{}).Where("id = ?", refund.ID).Updates(map[string]any{"state": fact.Status, "frozen": frozen, "updated_at": now}).Error; err != nil {
			return err
		}
		return tx.Create(&RelayRefundEvent{RefundID: refund.ID, Kind: fact.Status, QuotaDelta: delta, Evidence: fact.Evidence, CreatedAt: now}).Error
	})
}
