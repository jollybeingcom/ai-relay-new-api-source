package model

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
	"strings"
	"time"
)

// OpeningQuota is reviewed NON-CASH funding that predates all imported orders.
// Evidence is a hash of the operator's historical evidence file, not a balance guess.
type RelayFundingReview struct {
	UserID       int    `json:"user_id"`
	OpeningQuota int64  `json:"opening_quota"`
	Reason       string `json:"reason"`
	Evidence     string `json:"evidence"`
}

type RelayFundingPlan struct {
	PlanHash     string `json:"plan_hash"`
	UserID       int    `json:"user_id"`
	ReviewerID   int    `json:"reviewer_id"`
	Available    int64  `json:"available"`
	Reserved     int64  `json:"reserved"`
	Consumed     int64  `json:"consumed"`
	CashQuota    int64  `json:"cash_quota"`
	OpeningQuota int64  `json:"opening_quota"`
	Applied      bool   `json:"applied"`
}

var relayFundingPreview = errors.New("rollback funding preview")

// ReviewRelayFunding previews by executing the real import and rolling it back.
// Apply compares the original snapshot hash under the same wallet lock before
// importing. Any intervening payment/reservation/settlement invalidates the plan.
func ReviewRelayFunding(db *gorm.DB, reviewerID int, input RelayFundingReview, expectedHash string) (RelayFundingPlan, error) {
	var plan RelayFundingPlan
	if reviewerID <= 0 || input.UserID <= 0 || input.OpeningQuota < 0 || input.OpeningQuota > common.MaxWalletQuota ||
		strings.TrimSpace(input.Reason) == "" || len(input.Reason) > 128 || !relayPayEvidence.MatchString(input.Evidence) ||
		(expectedHash != "" && !relayPayEvidence.MatchString(expectedHash)) {
		return plan, ErrRelayInput
	}
	err := relayWalletTransaction(db, func(tx *gorm.DB) error {
		var admin User
		if err := tx.Select("id").Where("id = ? AND role >= ? AND status = ?", reviewerID, common.RoleAdminUser, common.UserStatusEnabled).Take(&admin).Error; err != nil {
			return ErrRelayQuota
		}
		var ready RelayFundingReady
		err := tx.Where("user_id = ?", input.UserID).Take(&ready).Error
		if err == nil {
			if expectedHash != "" && ready.PlanHash == expectedHash && ready.ReviewerID == reviewerID && ready.Evidence == input.Evidence && ready.Reason == input.Reason {
				// Bind replay to the opening amount as well, even after later consumption.
				var entry RelayFundingEntry
				e := tx.Where("user_id = ? AND kind = ? AND occurred_at = ?", input.UserID, "adjustment", 0).Take(&entry).Error
				if (input.OpeningQuota == 0 && errors.Is(e, gorm.ErrRecordNotFound)) || (e == nil && entry.Quota == input.OpeningQuota) {
					plan = RelayFundingPlan{PlanHash: expectedHash, UserID: input.UserID, ReviewerID: reviewerID, Applied: true, OpeningQuota: input.OpeningQuota}
					return nil
				}
			}
			return ErrRelayConflict
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		// Select only financial fields; do not read passwords or channel secrets.
		var user struct {
			ID           int
			Quota        int64
			UsedQuota    int64
			RequestCount int
			Role         int
			Status       int
		}
		if err := tx.Model(&User{}).Select("id,quota,used_quota,request_count,role,status").Where("id = ? AND role = ?", input.UserID, common.RoleCommonUser).Take(&user).Error; err != nil {
			return err
		}
		var orders []RelayPayOrder
		if err := tx.Select("id,user_id,amount_fen,quota,paid_at,credit,rate_version,state").Where("user_id = ?", input.UserID).Order("id").Find(&orders).Error; err != nil {
			return err
		}
		var requests []RelayWalletRequest
		if err := tx.Select("id,user_id,state,reserved,actual").Where("user_id = ?", input.UserID).Order("id").Find(&requests).Error; err != nil {
			return err
		}
		var entries []RelayFundingEntry
		if err := tx.Where("user_id = ?", input.UserID).Order("id").Find(&entries).Error; err != nil {
			return err
		}
		var refunds int64
		if err := tx.Model(&RelayRefund{}).Where("user_id = ?", input.UserID).Count(&refunds).Error; err != nil {
			return err
		}
		if refunds != 0 {
			return ErrRelayFunding
		}
		raw, err := json.Marshal([]any{reviewerID, input, user, orders, requests, entries})
		if err != nil {
			return err
		}
		digest := sha256.Sum256(raw)
		plan = RelayFundingPlan{PlanHash: hex.EncodeToString(digest[:]), UserID: input.UserID, ReviewerID: reviewerID, Available: user.Quota, OpeningQuota: input.OpeningQuota}
		if expectedHash != "" && expectedHash != plan.PlanHash {
			return ErrRelayConflict
		}
		marker := &RelayFundingReady{UserID: input.UserID, CreatedAt: time.Now().Unix(), ReviewerID: reviewerID, Reason: input.Reason, Evidence: input.Evidence, PlanHash: plan.PlanHash}
		if err := initializeRelayFunding(tx, input.UserID, input.OpeningQuota, input.Reason, marker); err != nil {
			return err
		}
		allocations, err := relayFundingSnapshot(tx, input.UserID)
		if err != nil {
			return err
		}
		for _, a := range allocations {
			plan.Reserved += a.Reserved
			plan.Consumed += a.Consumed
			if a.Kind == "order" {
				plan.CashQuota += a.Quota
			}
		}
		if expectedHash == "" {
			return relayFundingPreview
		}
		plan.Applied = true
		return nil
	})
	if errors.Is(err, relayFundingPreview) {
		err = nil
	}
	return plan, err
}
