package model

import (
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
)

// RelayAdjustmentMaxFen caps one audited operation at 10,000 CNY. Larger
// corrections must be split into separately reasoned and verified records.
const RelayAdjustmentMaxFen int64 = 1000000

var relayAdjustmentID = regexp.MustCompile(`^AJ[0-9a-f]{30}$`)

// RelayAdjustment is the only supported administrator balance change while
// the relay wallet is authoritative. Credits are NON-CASH funding sources and
// are never refundable through the payment channel. Debits may only consume
// the unspent, unreserved remainder of one earlier non-cash source; paid cash
// can leave a wallet only through the refund flow.
type RelayAdjustment struct {
	ID            string `gorm:"type:varchar(32);primaryKey" json:"id"`
	UserID        int    `gorm:"not null;index" json:"user_id"`
	AmountFen     int64  `gorm:"not null" json:"amount_fen"`
	Quota         int64  `gorm:"not null" json:"quota"`
	Target        string `gorm:"type:varchar(64);not null;default:'';index" json:"target"`
	Reason        string `gorm:"type:varchar(256);not null" json:"reason"`
	Evidence      string `gorm:"type:varchar(64);not null" json:"evidence"`
	ReviewerID    int    `gorm:"not null" json:"reviewer_id"`
	BalanceBefore int64  `gorm:"not null" json:"balance_before"`
	BalanceAfter  int64  `gorm:"not null" json:"balance_after"`
	CreatedAt     int64  `gorm:"not null;index" json:"created_at"`
}

func (*RelayAdjustment) BeforeUpdate(*gorm.DB) error { return ErrRelayState }
func (*RelayAdjustment) BeforeDelete(*gorm.DB) error { return ErrRelayState }

type RelayAdjustmentInput struct {
	ID        string `json:"id"`
	UserID    int    `json:"user_id"`
	AmountFen int64  `json:"amount_fen"`
	Target    string `json:"target"`
	Reason    string `json:"reason"`
	Evidence  string `json:"evidence"`
}

func MigrateRelayAdjustment(db *gorm.DB) error {
	return db.AutoMigrate(&RelayAdjustment{})
}

func relayAdjustmentEntryID(id string) string { return "adj:" + id }

// relayAdjustmentDebits returns successful debits keyed by funding entry ID.
func relayAdjustmentDebits(tx *gorm.DB, userID int) (map[string]int64, error) {
	var debits []RelayAdjustment
	if err := tx.Select("id,target,quota").Where("user_id = ? AND quota < 0", userID).Find(&debits).Error; err != nil {
		return nil, err
	}
	result := make(map[string]int64, len(debits))
	for _, debit := range debits {
		if debit.Target == "" || -debit.Quota <= 0 || result[debit.Target] > common.MaxWalletQuota+debit.Quota {
			return nil, ErrRelayFunding
		}
		result[debit.Target] -= debit.Quota
	}
	return result, nil
}

func validRelayAdjustment(input RelayAdjustmentInput) bool {
	if !relayAdjustmentID.MatchString(input.ID) || input.UserID <= 0 || input.AmountFen == 0 ||
		input.AmountFen > RelayAdjustmentMaxFen || input.AmountFen < -RelayAdjustmentMaxFen ||
		strings.TrimSpace(input.Reason) == "" || len(input.Reason) > 256 || input.Reason != strings.TrimSpace(input.Reason) ||
		!relayPayEvidence.MatchString(input.Evidence) || len(input.Target) > 64 {
		return false
	}
	// A credit creates its own source; a debit must name exactly one source.
	return (input.AmountFen > 0) == (input.Target == "")
}

// ApplyRelayAdjustment changes users.quota, the funding sources and the audit
// row in one wallet transaction. Replaying the same ID with the same intent is
// idempotent; any changed field is a conflict, never a second balance change.
func ApplyRelayAdjustment(db *gorm.DB, reviewerID int, input RelayAdjustmentInput) (RelayAdjustment, error) {
	var saved RelayAdjustment
	if reviewerID <= 0 || !validRelayAdjustment(input) {
		return saved, ErrRelayInput
	}
	err := relayWalletTransaction(db, func(tx *gorm.DB) error {
		if err := relayRiskAdmin(tx, reviewerID, input.Reason); err != nil {
			return ErrRelayQuota
		}
		err := tx.Where("id = ?", input.ID).Take(&saved).Error
		if err == nil {
			if saved.UserID != input.UserID || saved.AmountFen != input.AmountFen || saved.Target != input.Target ||
				saved.Reason != input.Reason || saved.Evidence != input.Evidence || saved.ReviewerID != reviewerID {
				return ErrRelayConflict
			}
			return nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		var user User
		if err := tx.Select("id,quota,role").Where("id = ? AND role = ?", input.UserID, common.RoleCommonUser).Take(&user).Error; err != nil {
			return err
		}
		// Unreconciled history cannot be adjusted; review its funding first.
		allocations, err := relayFundingSnapshot(tx, input.UserID)
		if err != nil {
			return err
		}
		quota := input.AmountFen * 10000
		before := int64(user.Quota)
		if quota > 0 {
			if before > common.MaxWalletQuota-quota {
				return ErrRelayQuota
			}
		} else {
			var target *RelayFundingAllocation
			for i := range allocations {
				if allocations[i].ID == input.Target {
					target = &allocations[i]
				}
			}
			if target == nil {
				return gorm.ErrRecordNotFound
			}
			if target.Kind != "adjustment" {
				return ErrRelayInput
			}
			if target.Available < -quota || before < -quota {
				return ErrRelayQuota
			}
		}
		result := tx.Model(&User{}).Where("id = ? AND quota = ?", input.UserID, user.Quota).Update("quota", gorm.Expr("quota + ?", quota))
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrRelayConflict
		}
		now := time.Now().Unix()
		saved = RelayAdjustment{ID: input.ID, UserID: input.UserID, AmountFen: input.AmountFen, Quota: quota, Target: input.Target,
			Reason: input.Reason, Evidence: input.Evidence, ReviewerID: reviewerID, BalanceBefore: before, BalanceAfter: before + quota, CreatedAt: now}
		if err := tx.Create(&saved).Error; err != nil {
			return err
		}
		if quota > 0 {
			reason := input.Reason
			if len(reason) > 128 {
				reason = reason[:128]
			}
			entry := RelayFundingEntry{ID: relayAdjustmentEntryID(input.ID), UserID: input.UserID, Kind: "adjustment", Quota: quota, OccurredAt: now, Reason: reason}
			if err := tx.Create(&entry).Error; err != nil {
				return err
			}
		}
		// Re-derive allocations: consumption totals must still reconcile exactly.
		if _, err := relayFundingSnapshot(tx, input.UserID); err != nil {
			return err
		}
		// Compensation is a loss; let an enabled stop control react immediately.
		if common.GetEnvOrDefaultBool("RELAY_RISK_CONTROL", false) {
			_, err = evaluateRelayRisk(tx, time.Now(), 0, false)
		}
		return err
	})
	return saved, err
}

// RelayFundingView is the administrator's read-only allocation of one wallet.
type RelayFundingView struct {
	UserID     int                      `json:"user_id"`
	Ready      bool                     `json:"ready"`
	Balance    int64                    `json:"balance"`
	Held       int64                    `json:"held"`
	Sources    []RelayFundingAllocation `json:"sources"`
	Consistent bool                     `json:"consistent"`
}

func ReadRelayFunding(db *gorm.DB, userID int) (RelayFundingView, error) {
	view := RelayFundingView{UserID: userID, Sources: []RelayFundingAllocation{}}
	if userID <= 0 {
		return view, ErrRelayInput
	}
	err := relayWalletTransaction(db, func(tx *gorm.DB) error {
		var user User
		if err := tx.Select("id,quota").Where("id = ? AND role = ?", userID, common.RoleCommonUser).Take(&user).Error; err != nil {
			return err
		}
		view.Balance = int64(user.Quota)
		hold, err := GetRelayWalletHold(tx, userID)
		if err != nil {
			return err
		}
		view.Held = hold.Held
		var ready int64
		if err := tx.Model(&RelayFundingReady{}).Where("user_id = ?", userID).Count(&ready).Error; err != nil {
			return err
		}
		view.Ready = ready == 1
		if !view.Ready {
			return nil
		}
		allocations, err := relayFundingSnapshot(tx, userID)
		if errors.Is(err, ErrRelayFunding) {
			return nil // Report the inconsistency instead of failing the read.
		}
		if err != nil {
			return err
		}
		view.Sources, view.Consistent = allocations, true
		return nil
	})
	return view, err
}

func ListRelayAdjustments(db *gorm.DB, userID, page int) ([]RelayAdjustment, int64, error) {
	items := []RelayAdjustment{}
	if userID < 0 || page < 1 || page > 1000 {
		return items, 0, ErrRelayInput
	}
	query := db.Model(&RelayAdjustment{})
	if userID > 0 {
		query = query.Where("user_id = ?", userID)
	}
	var total int64
	if err := query.Count(&total).Error; err != nil {
		return items, 0, err
	}
	err := query.Order("created_at DESC").Order("id DESC").Offset((page - 1) * 20).Limit(20).Find(&items).Error
	return items, total, err
}
