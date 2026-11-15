package model

import (
	"errors"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const RelayRiskWarningQuota int64 = 1000 * 1000000
const RelayRiskStopQuota int64 = 1500 * 1000000
const relayRiskMax int64 = 9000000000000000

var ErrRelayStopped = errors.New("new generation and top-ups are paused")

type RelayFixedCost struct {
	ID         string  `gorm:"type:varchar(36);primaryKey" json:"id"`
	Month      string  `gorm:"type:varchar(7);not null;index" json:"month"`
	Category   string  `gorm:"type:varchar(64);not null" json:"category"`
	AmountFen  int64   `gorm:"not null" json:"amount_fen"`
	Estimated  bool    `gorm:"not null" json:"estimated"`
	Supersedes *string `gorm:"type:varchar(36);uniqueIndex" json:"supersedes"`
	ReviewerID int     `gorm:"not null" json:"reviewer_id"`
	Reason     string  `gorm:"type:varchar(256);not null" json:"reason"`
	CreatedAt  int64   `gorm:"not null" json:"created_at"`
}

type RelayRiskState struct {
	ID         int    `gorm:"primaryKey;autoIncrement:false" json:"-"`
	Mode       string `gorm:"type:varchar(16);not null" json:"mode"`
	Revision   int64  `gorm:"not null" json:"revision"`
	Reason     string `gorm:"type:varchar(256);not null" json:"reason"`
	ReviewerID int    `gorm:"not null" json:"reviewer_id"`
	UpdatedAt  int64  `gorm:"not null" json:"updated_at"`
}

type RelayRiskEvent struct {
	ID         int64  `gorm:"primaryKey" json:"id"`
	Mode       string `gorm:"type:varchar(16);not null" json:"mode"`
	Revision   int64  `gorm:"not null;uniqueIndex" json:"revision"`
	Reason     string `gorm:"type:varchar(256);not null" json:"reason"`
	ReviewerID int    `gorm:"not null" json:"reviewer_id"`
	CreatedAt  int64  `gorm:"not null" json:"created_at"`
}

func (*RelayFixedCost) BeforeUpdate(*gorm.DB) error { return ErrRelayState }
func (*RelayFixedCost) BeforeDelete(*gorm.DB) error { return ErrRelayState }
func (*RelayRiskEvent) BeforeUpdate(*gorm.DB) error { return ErrRelayState }
func (*RelayRiskEvent) BeforeDelete(*gorm.DB) error { return ErrRelayState }

type RelayRiskReport struct {
	Month          string         `json:"month"`
	Income         int64          `json:"income"`
	Upstream       int64          `json:"upstream"`
	FixedActual    int64          `json:"fixed_actual"`
	FixedEstimated int64          `json:"fixed_estimated"`
	PendingRisk    int64          `json:"pending_risk"`
	Compensation   int64          `json:"compensation"` // audited non-cash credits granted
	Reversed       int64          `json:"reversed"`     // later debits of those credits
	Loss           int64          `json:"loss"`
	MissingCosts   int64          `json:"missing_costs"`
	FixedConfirmed bool           `json:"fixed_confirmed"`
	State          RelayRiskState `json:"state"`
}

// Contemporary Shanghai natural months are UTC+8; no host timezone dependency.
func RelayRiskMonth(now time.Time) string {
	return now.In(time.FixedZone("Asia/Shanghai", 8*3600)).Format("2006-01")
}

func relayRiskMonthBounds(month string) (int64, int64, error) {
	start, err := time.ParseInLocation("2006-01", month, time.FixedZone("Asia/Shanghai", 8*3600))
	if err != nil || start.Format("2006-01") != month || start.Year() < 2020 || start.Year() > 2100 {
		return 0, 0, ErrRelayInput
	}
	return start.Unix(), start.AddDate(0, 1, 0).Unix(), nil
}

func MigrateRelayRisk(db *gorm.DB) error {
	if err := db.AutoMigrate(&RelayFixedCost{}, &RelayRiskState{}, &RelayRiskEvent{}); err != nil {
		return err
	}
	return db.Clauses(clause.OnConflict{DoNothing: true}).Create(&RelayRiskState{ID: 1, Mode: "normal", Reason: "not_evaluated"}).Error
}

func relayRiskAdd(total *int64, value int64) error {
	if value < 0 || value > relayRiskMax || *total < 0 || *total > relayRiskMax-value {
		return ErrRelayInput
	}
	*total += value
	return nil
}

// Called under the global wallet lock; receipts/costs cannot move mid-report.
// Payment principal is intentionally not queried and never counts as revenue.
func relayRiskReport(tx *gorm.DB, month string) (RelayRiskReport, error) {
	r := RelayRiskReport{Month: month}
	start, end, err := relayRiskMonthBounds(month)
	if err != nil {
		return r, err
	}
	if err = tx.First(&r.State, 1).Error; err != nil {
		return r, err
	}
	var fixed []RelayFixedCost
	replaced := tx.Model(&RelayFixedCost{}).Select("supersedes").Where("supersedes IS NOT NULL")
	if err = tx.Where("month = ? AND id NOT IN (?)", month, replaced).Find(&fixed).Error; err != nil {
		return r, err
	}
	r.FixedConfirmed = len(fixed) > 0 // An explicit zero budget is allowed; absent is not zero.
	for _, cost := range fixed {
		if cost.AmountFen < 0 || cost.AmountFen > 1000000000 {
			return r, ErrRelayInput
		}
		total := &r.FixedActual
		if cost.Estimated {
			total = &r.FixedEstimated
		}
		if err = relayRiskAdd(total, cost.AmountFen*10000); err != nil {
			return r, err
		}
	}
	var receipts []RelayWalletReceipt
	err = tx.Where("created_at >= ? AND created_at < ?", start, end).FindInBatches(&receipts, 100, func(batch *gorm.DB, _ int) error {
		for _, receipt := range receipts {
			if err := relayRiskAdd(&r.Income, receipt.Actual); err != nil {
				return err
			}
			var basis RelayWalletCostSnapshot
			var cost RelayWalletCost
			basisErr := tx.Where("request_id = ?", receipt.RequestID).Take(&basis).Error
			costErr := tx.Where("request_id = ?", receipt.RequestID).Take(&cost).Error
			for _, e := range []error{basisErr, costErr} {
				if e != nil && !errors.Is(e, gorm.ErrRecordNotFound) {
					return e
				}
			}
			if basisErr != nil || costErr != nil {
				r.MissingCosts++
				continue
			}
			amount, e := basis.charge(receipt)
			if e != nil || amount != cost.Actual || cost.PriceVersion != receipt.PriceVersion || cost.CreatedAt != receipt.CreatedAt {
				r.MissingCosts++
				continue
			}
			if err := relayRiskAdd(&r.Upstream, amount); err != nil {
				return err
			}
		}
		return nil
	}).Error
	if err != nil {
		return r, err
	}
	// Settlements from legacy integer-only paths have no consumption facts.
	var missing int64
	known := tx.Model(&RelayWalletReceipt{}).Select("request_id")
	if err = tx.Model(&RelayWalletRequest{}).Where("state = ? AND updated_at >= ? AND updated_at < ? AND id NOT IN (?)", "settled", start, end, known).Count(&missing).Error; err != nil {
		return r, err
	}
	r.MissingCosts += missing
	var requests []RelayWalletRequest
	err = tx.Where("state IN ? OR (state = ? AND unknown_at > 0)", []string{"reserved", "submitted", "unknown"}, "released").FindInBatches(&requests, 100, func(batch *gorm.DB, _ int) error {
		for _, request := range requests {
			var basis RelayWalletCostSnapshot
			err := tx.Where("request_id = ?", request.ID).Take(&basis).Error
			if errors.Is(err, gorm.ErrRecordNotFound) {
				r.MissingCosts++
				continue
			}
			if err != nil {
				return err
			}
			bound, err := basis.Bound()
			if err != nil || bound != basis.Reserved || basis.PriceVersion != request.PriceVersion {
				r.MissingCosts++
				continue
			}
			if err := relayRiskAdd(&r.PendingRisk, bound); err != nil {
				return err
			}
		}
		return nil
	}).Error
	if err != nil {
		return r, err
	}
	// Non-cash compensation is a platform loss when granted; consuming it later
	// also counts as income, so the pair nets to zero apart from upstream cost.
	// Only debits of audited credits reverse it; opening history never counted.
	var adjustments []RelayAdjustment
	if err = tx.Select("quota,target").Where("created_at >= ? AND created_at < ?", start, end).Find(&adjustments).Error; err != nil {
		return r, err
	}
	for _, a := range adjustments {
		if a.Quota > 0 {
			err = relayRiskAdd(&r.Compensation, a.Quota)
		} else if strings.HasPrefix(a.Target, "adj:") {
			err = relayRiskAdd(&r.Reversed, -a.Quota)
		}
		if err != nil {
			return r, err
		}
	}
	for _, cost := range []int64{r.Upstream, r.FixedActual, r.FixedEstimated, r.PendingRisk, r.Compensation} {
		if err := relayRiskAdd(&r.Loss, cost); err != nil {
			return r, err
		}
	}
	r.Loss -= r.Income + r.Reversed
	return r, nil
}

func ReadRelayRisk(db *gorm.DB, month string) (RelayRiskReport, error) {
	var report RelayRiskReport
	err := relayWalletTransaction(db, func(tx *gorm.DB) error { var err error; report, err = relayRiskReport(tx, month); return err })
	return report, err
}

func relayRiskTransition(tx *gorm.DB, state *RelayRiskState, mode, reason string, reviewer int) error {
	if state.Revision >= relayRiskMax {
		return ErrRelayState
	}
	state.Mode, state.Reason, state.ReviewerID, state.UpdatedAt = mode, reason, reviewer, time.Now().Unix()
	state.Revision++
	if err := tx.Model(&RelayRiskState{}).Where("id = ?", 1).Updates(map[string]any{"mode": mode, "reason": reason, "reviewer_id": reviewer, "updated_at": state.UpdatedAt, "revision": state.Revision}).Error; err != nil {
		return err
	}
	return tx.Create(&RelayRiskEvent{Mode: mode, Reason: reason, ReviewerID: reviewer, Revision: state.Revision, CreatedAt: state.UpdatedAt}).Error
}

func evaluateRelayRisk(tx *gorm.DB, now time.Time, prospective int64, missing bool) (RelayRiskReport, error) {
	r, err := relayRiskReport(tx, RelayRiskMonth(now))
	if err != nil {
		return r, err
	}
	if prospective < 0 || prospective > relayRiskMax || r.Loss > relayRiskMax-prospective {
		return r, ErrRelayInput
	}
	want, reason := "normal", ""
	if missing || r.MissingCosts > 0 || !r.FixedConfirmed {
		want, reason = "stopped", "accounting_incomplete"
	} else if r.Loss+prospective >= RelayRiskStopQuota {
		want, reason = "stopped", "monthly_loss_limit"
	} else if r.Loss+prospective >= RelayRiskWarningQuota {
		want, reason = "warning", "monthly_loss_warning"
	}
	if r.State.Mode == "normal" && want != "normal" || r.State.Mode == "warning" && want == "stopped" {
		err = relayRiskTransition(tx, &r.State, want, reason, 0)
	}
	return r, err
}

func EvaluateRelayRisk(db *gorm.DB, now time.Time) (RelayRiskReport, error) {
	var r RelayRiskReport
	err := relayWalletTransaction(db, func(tx *gorm.DB) error { var err error; r, err = evaluateRelayRisk(tx, now, 0, false); return err })
	return r, err
}

// Caller commits a stop transition even when it rejects the business request.
func relayRiskAdmission(tx *gorm.DB, prospective int64, missing bool) (bool, error) {
	if !common.GetEnvOrDefaultBool("RELAY_RISK_CONTROL", false) {
		return true, nil
	}
	r, err := evaluateRelayRisk(tx, time.Now(), prospective, missing)
	if err != nil {
		return false, err
	}
	return r.State.Mode == "normal" || r.State.Mode == "warning", nil
}

func relayRiskAdmin(tx *gorm.DB, reviewer int, reason string) error {
	if reviewer <= 0 || strings.TrimSpace(reason) == "" || len(reason) > 256 {
		return ErrRelayInput
	}
	var user User
	if err := tx.Where("id = ? AND role >= ? AND status = ?", reviewer, common.RoleAdminUser, common.UserStatusEnabled).Take(&user).Error; err != nil {
		return ErrRelayInput
	}
	return nil
}

func RecordRelayFixedCost(db *gorm.DB, cost RelayFixedCost) error {
	if !relayRequestPattern.MatchString(cost.ID) || strings.TrimSpace(cost.Category) == "" || len(cost.Category) > 64 || cost.AmountFen < 0 || cost.AmountFen > 1000000000 {
		return ErrRelayInput
	}
	if _, _, err := relayRiskMonthBounds(cost.Month); err != nil {
		return err
	}
	return relayWalletTransaction(db, func(tx *gorm.DB) error {
		if err := relayRiskAdmin(tx, cost.ReviewerID, cost.Reason); err != nil {
			return err
		}
		var existing RelayFixedCost
		err := tx.Where("id = ?", cost.ID).Take(&existing).Error
		if err == nil {
			cost.CreatedAt = existing.CreatedAt
			// Compare nullable IDs by value, not pointer identity.
			a, b := "", ""
			if cost.Supersedes != nil {
				a = *cost.Supersedes
			}
			if existing.Supersedes != nil {
				b = *existing.Supersedes
			}
			cost.Supersedes, existing.Supersedes = nil, nil
			if cost != existing || a != b {
				return ErrRelayConflict
			}
			return nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		var count int64
		if cost.Supersedes == nil {
			if err := tx.Model(&RelayFixedCost{}).Where("month = ? AND category = ?", cost.Month, cost.Category).Count(&count).Error; err != nil {
				return err
			}
			if count != 0 {
				return ErrRelayConflict
			}
		} else {
			var prior RelayFixedCost
			if err := tx.Where("id = ? AND month = ? AND category = ?", *cost.Supersedes, cost.Month, cost.Category).Take(&prior).Error; err != nil {
				return ErrRelayConflict
			}
			if err := tx.Model(&RelayFixedCost{}).Where("supersedes = ?", *cost.Supersedes).Count(&count).Error; err != nil {
				return err
			}
			if count != 0 {
				return ErrRelayConflict
			}
		}
		cost.CreatedAt = time.Now().Unix()
		if err := tx.Create(&cost).Error; err != nil {
			return err
		}
		_, err = evaluateRelayRisk(tx, time.Now(), 0, false)
		return err
	})
}

func ChangeRelayRisk(db *gorm.DB, reviewer int, revision int64, action, reason string) (RelayRiskState, error) {
	var state RelayRiskState
	if action != "maintenance" && action != "resume" {
		return state, ErrRelayInput
	}
	err := relayWalletTransaction(db, func(tx *gorm.DB) error {
		if err := relayRiskAdmin(tx, reviewer, reason); err != nil {
			return err
		}
		if err := tx.First(&state, 1).Error; err != nil {
			return err
		}
		if state.Revision != revision {
			return ErrRelayConflict
		}
		mode := "maintenance"
		if action == "resume" {
			r, err := relayRiskReport(tx, RelayRiskMonth(time.Now()))
			if err != nil {
				return err
			}
			if r.MissingCosts > 0 || !r.FixedConfirmed || r.Loss >= RelayRiskStopQuota {
				return ErrRelayStopped
			}
			mode = "normal"
			if r.Loss >= RelayRiskWarningQuota {
				mode = "warning"
			}
		}
		return relayRiskTransition(tx, &state, mode, reason, reviewer)
	})
	return state, err
}
