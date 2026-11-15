package model

// This opt-in ledger is not wired into the live relay until dispatch, pricing,
// counters and consume-log integration have passed end-to-end verification.
import (
	"errors"
	"math"
	"regexp"
	"time"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var (
	ErrRelayConflict    = errors.New("relay billing request conflicts with stored intent")
	ErrRelayState       = errors.New("relay billing transition is not allowed")
	ErrRelayQuota       = errors.New("relay wallet or token unavailable")
	ErrRelayInput       = errors.New("invalid relay billing intent")
	ErrRelayUserBusy    = errors.New("relay user generation limit reached")
	ErrRelayGlobalBusy  = errors.New("relay global generation limit reached")
	relayRequestPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	relayHashPattern    = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

type RelayWalletRequest struct {
	ID            string                   `gorm:"type:varchar(36);primaryKey"`
	UserID        int                      `gorm:"not null;index"`
	TokenID       int                      `gorm:"not null"`
	PayloadHash   string                   `gorm:"type:varchar(64);not null"`
	PriceVersion  string                   `gorm:"type:varchar(100);not null"`
	PriceSnapshot string                   `gorm:"type:text;not null"`
	CostTracked   bool                     `gorm:"not null;default:false"`
	CostBasis     *RelayWalletCostSnapshot `gorm:"-"`
	Reserved      int64                    `gorm:"not null"`
	Actual        int64                    `gorm:"not null"`
	State         string                   `gorm:"type:varchar(16);not null;index"`
	CreatedAt     int64                    `gorm:"not null"`
	UpdatedAt     int64                    `gorm:"not null;index"`
	UnknownAt     int64                    `gorm:"not null"`
}

type RelayWalletEvent struct {
	ID          int64  `gorm:"primaryKey"`
	RequestID   string `gorm:"type:varchar(36);not null;uniqueIndex:relay_event_transition,priority:1"`
	State       string `gorm:"type:varchar(16);not null;uniqueIndex:relay_event_transition,priority:2"`
	WalletDelta int64  `gorm:"not null"`
	Actual      int64  `gorm:"not null"`
	Reason      string `gorm:"type:varchar(64);not null"`
	CreatedAt   int64  `gorm:"not null"`
}

// A write before any reads serializes this small single-instance ledger even on
// SQLite. The bit flip avoids MySQL's unchanged-row semantics and integer growth.
type RelayWalletLock struct {
	ID    int `gorm:"primaryKey;autoIncrement:false"`
	Epoch int `gorm:"not null"`
}

func (request *RelayWalletRequest) BeforeUpdate(tx *gorm.DB) error {
	fields, ok := tx.Statement.Dest.(map[string]any)
	if !ok {
		return ErrRelayInput
	}
	for field := range fields {
		switch field {
		case "state", "actual", "updated_at", "unknown_at":
		default:
			return ErrRelayConflict
		}
	}
	return nil
}

func (request *RelayWalletRequest) BeforeDelete(*gorm.DB) error { return ErrRelayState }
func (event *RelayWalletEvent) BeforeUpdate(*gorm.DB) error     { return ErrRelayState }
func (event *RelayWalletEvent) BeforeDelete(*gorm.DB) error     { return ErrRelayState }

func MigrateRelayWallet(db *gorm.DB) error {
	if err := db.AutoMigrate(&RelayWalletRequest{}, &RelayWalletEvent{}, &RelayWalletLock{}, &RelayWalletReceipt{}, &RelayWalletLogPending{}, &RelayWalletCostSnapshot{}, &RelayWalletCost{}); err != nil {
		return err
	}
	// Backfill receipts written before the delivery queue existed. Conflict
	// handling preserves completed delivery state across repeated migrations.
	var receipts []RelayWalletReceipt
	if err := db.FindInBatches(&receipts, 100, func(tx *gorm.DB, _ int) error {
		for _, receipt := range receipts {
			if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&RelayWalletLogPending{RequestID: receipt.RequestID}).Error; err != nil {
				return err
			}
		}
		return nil
	}).Error; err != nil {
		return err
	}
	if err := MigrateRelayWalletLogs(db); err != nil {
		return err
	}
	if err := MigrateRelayPay(db); err != nil {
		return err
	}
	if err := MigrateRelayRefund(db); err != nil {
		return err
	}
	if err := MigrateRelayRisk(db); err != nil {
		return err
	}
	if err := MigrateRelayPolicy(db); err != nil {
		return err
	}
	return db.Clauses(clause.OnConflict{DoNothing: true}).Create(&RelayWalletLock{ID: 1}).Error
}

func relayWalletTransaction(db *gorm.DB, action func(*gorm.DB) error) error {
	if common.RedisEnabled || common.BatchUpdateEnabled {
		return errors.New("relay wallet requires synchronous database accounting without Redis")
	}
	return db.Transaction(func(tx *gorm.DB) error {
		result := tx.Model(&RelayWalletLock{}).Where("id = ?", 1).Update("epoch", gorm.Expr("1 - epoch"))
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrRelayState
		}
		return action(tx)
	})
}

// ReserveRelayWallet binds a durable business key to its exact immutable intent.
// It debits the existing New API user and finite token, never a second wallet.
func ReserveRelayWallet(db *gorm.DB, intent RelayWalletRequest) (RelayWalletRequest, error) {
	saved, _, err := reserveRelayWallet(db, intent, nil)
	return saved, err
}

// Limits are trusted server policy, never client request fields. Admission uses
// the same transaction/lock as funds: a crash cannot lose an in-memory permit.
type RelayWalletLimits struct {
	PerUser int
	Global  int
}

func ReserveRelayWalletWithLimits(db *gorm.DB, intent RelayWalletRequest, limits RelayWalletLimits) (RelayWalletRequest, error) {
	saved, _, err := ReserveRelayWalletCreated(db, intent, limits)
	return saved, err
}

// ReserveRelayWalletCreated also reports whether this call created the intent.
// A replay of an existing business key must not act for the original attempt.
func ReserveRelayWalletCreated(db *gorm.DB, intent RelayWalletRequest, limits RelayWalletLimits) (RelayWalletRequest, bool, error) {
	if limits.PerUser < 1 || limits.Global < limits.PerUser || limits.Global > 100 {
		return RelayWalletRequest{}, false, ErrRelayInput
	}
	return reserveRelayWallet(db, intent, &limits)
}

func reserveRelayWallet(db *gorm.DB, intent RelayWalletRequest, limits *RelayWalletLimits) (RelayWalletRequest, bool, error) {
	var basis RelayWalletCostSnapshot
	if intent.CostBasis != nil {
		basis = *intent.CostBasis
		basis.RequestID, basis.PriceVersion = intent.ID, intent.PriceVersion
		bound, err := basis.Bound()
		if err != nil {
			return RelayWalletRequest{}, false, err
		}
		basis.Reserved = bound
	}
	var snapshot map[string]any
	if !relayRequestPattern.MatchString(intent.ID) || intent.UserID <= 0 || intent.TokenID <= 0 ||
		!relayHashPattern.MatchString(intent.PayloadHash) || len(intent.PriceVersion) == 0 || len(intent.PriceVersion) > 100 ||
		len(intent.PriceSnapshot) > 16384 || common.UnmarshalJsonStr(intent.PriceSnapshot, &snapshot) != nil || len(snapshot) == 0 ||
		intent.Reserved <= 0 || intent.Reserved > math.MaxInt32 {
		return RelayWalletRequest{}, false, ErrRelayInput
	}
	var saved RelayWalletRequest
	created := false
	denied := false
	err := relayWalletTransaction(db, func(tx *gorm.DB) error {
		err := lockForUpdate(tx).Where("id = ?", intent.ID).Take(&saved).Error
		if err == nil {
			if saved.CostTracked != (intent.CostBasis != nil) {
				return ErrRelayConflict
			}
			if saved.CostTracked {
				var stored RelayWalletCostSnapshot
				if err := tx.Where("request_id = ?", intent.ID).Take(&stored).Error; err != nil {
					return err
				}
				if stored != basis {
					return ErrRelayConflict
				}
			}
			if saved.UserID != intent.UserID || saved.TokenID != intent.TokenID || saved.PayloadHash != intent.PayloadHash ||
				saved.PriceVersion != intent.PriceVersion || saved.PriceSnapshot != intent.PriceSnapshot || saved.Reserved != intent.Reserved {
				return ErrRelayConflict
			}
			return nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		allowed, err := relayRiskAdmission(tx, basis.Reserved, intent.CostBasis == nil)
		if err != nil {
			return err
		}
		if !allowed {
			denied = true
			return nil
		}
		if limits != nil {
			// An unknown upstream may still be generating. Retain its slot until
			// authoritative settlement/recovery, rather than admit overlapping work.
			states := []string{"reserved", "submitted", "unknown"}
			var active int64
			if err := tx.Model(&RelayWalletRequest{}).Where("user_id = ? AND state IN ?", intent.UserID, states).Count(&active).Error; err != nil {
				return err
			}
			if active >= int64(limits.PerUser) {
				return ErrRelayUserBusy
			}
			if err := tx.Model(&RelayWalletRequest{}).Where("state IN ?", states).Count(&active).Error; err != nil {
				return err
			}
			if active >= int64(limits.Global) {
				return ErrRelayGlobalBusy
			}
		}
		now := time.Now().Unix()
		result := tx.Model(&User{}).Where("id = ? AND role = ? AND status = ? AND quota >= ? AND quota <= ?",
			intent.UserID, common.RoleCommonUser, common.UserStatusEnabled, intent.Reserved, common.MaxWalletQuota).
			Update("quota", gorm.Expr("quota - ?", intent.Reserved))
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrRelayQuota
		}
		result = tx.Model(&Token{}).Where("id = ? AND user_id = ? AND status = ? AND unlimited_quota = ? AND remain_quota >= ? AND remain_quota <= ? AND (expired_time = -1 OR expired_time > ?)",
			intent.TokenID, intent.UserID, common.TokenStatusEnabled, false, intent.Reserved, common.MaxWalletQuota, now).
			Update("remain_quota", gorm.Expr("remain_quota - ?", intent.Reserved))
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrRelayQuota
		}
		saved = RelayWalletRequest{ID: intent.ID, UserID: intent.UserID, TokenID: intent.TokenID,
			CostTracked: intent.CostBasis != nil,
			PayloadHash: intent.PayloadHash, PriceVersion: intent.PriceVersion, PriceSnapshot: intent.PriceSnapshot,
			Reserved: intent.Reserved, State: "reserved", CreatedAt: now, UpdatedAt: now}
		if err := tx.Create(&saved).Error; err != nil {
			return err
		}
		if saved.CostTracked {
			if err := tx.Create(&basis).Error; err != nil {
				return err
			}
		}
		created = true
		return tx.Create(&RelayWalletEvent{RequestID: saved.ID, State: saved.State, WalletDelta: -saved.Reserved, CreatedAt: now}).Error
	})
	if err != nil {
		return RelayWalletRequest{}, false, err
	}
	if denied {
		return RelayWalletRequest{}, false, ErrRelayStopped
	}
	return saved, created, nil
}

// AdvanceRelayWallet returns changed=true only to the winner of a transition.
// Only that winner may dispatch when target=submitted; replay must not send.
// The caller is internal and must use the authenticated owner, never a client ID.
func AdvanceRelayWallet(db *gorm.DB, id string, userID int, target string, actual int64) (bool, error) {
	return advanceRelayWallet(db, id, userID, target, actual, false)
}

func advanceRelayWallet(db *gorm.DB, id string, userID int, target string, actual int64, withReceipt bool) (bool, error) {
	if !relayRequestPattern.MatchString(id) || userID <= 0 || actual < 0 || actual > math.MaxInt32 {
		return false, ErrRelayInput
	}
	if target != "submitted" && target != "unknown" && target != "settled" && target != "released" {
		return false, ErrRelayInput
	}
	if target != "settled" && actual != 0 {
		return false, ErrRelayInput
	}
	changed := false
	err := relayWalletTransaction(db, func(tx *gorm.DB) error {
		var request RelayWalletRequest
		if err := lockForUpdate(tx).Where("id = ? AND user_id = ?", id, userID).Take(&request).Error; err != nil {
			return err
		}
		if request.State == target {
			if request.CostTracked && target == "settled" && !withReceipt {
				return ErrRelayState
			}
			if target == "settled" && request.Actual != actual {
				return ErrRelayConflict
			}
			return nil
		}
		now := time.Now().Unix()
		delta := int64(0)
		reason := ""
		updates := map[string]any{"state": target, "updated_at": now}
		switch target {
		case "submitted":
			if request.State != "reserved" {
				return ErrRelayState
			}
			var owner User
			if err := lockForUpdate(tx).Where("id = ? AND role = ? AND status = ?", userID, common.RoleCommonUser, common.UserStatusEnabled).Take(&owner).Error; err != nil {
				return ErrRelayQuota
			}
			var token Token
			if err := lockForUpdate(tx).Where("id = ? AND user_id = ? AND status = ? AND (expired_time = -1 OR expired_time > ?)", request.TokenID, userID, common.TokenStatusEnabled, now).Take(&token).Error; err != nil {
				return ErrRelayQuota
			}
		case "unknown":
			if request.State != "submitted" {
				return ErrRelayState
			}
			updates["unknown_at"] = now
		case "settled":
			if request.CostTracked && !withReceipt {
				return ErrRelayState
			}
			if (request.State != "submitted" && request.State != "unknown") || actual > request.Reserved {
				return ErrRelayState
			}
			delta = request.Reserved - actual
			updates["actual"] = actual
		case "released":
			if request.State != "reserved" && !(request.State == "unknown" && request.UnknownAt > 0 && now-request.UnknownAt >= 86400) {
				return ErrRelayState
			}
			delta = request.Reserved
			reason = "not_submitted"
			if request.State == "unknown" {
				reason = "unknown_after_24h_platform_cost"
			}
		}
		if target == "settled" || target == "released" {
			userUpdates := map[string]any{"quota": gorm.Expr("quota + ?", delta)}
			query := tx.Unscoped().Model(&User{}).Where("id = ? AND quota >= 0 AND quota <= ?", userID, common.MaxWalletQuota-delta)
			if target == "settled" {
				query = query.Where("used_quota >= 0 AND used_quota <= ? AND request_count >= 0 AND request_count < ?", common.MaxWalletQuota-actual, common.MaxWalletQuota)
				userUpdates["used_quota"] = gorm.Expr("used_quota + ?", actual)
				userUpdates["request_count"] = gorm.Expr("request_count + 1")
			}
			result := query.Updates(userUpdates)
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected != 1 {
				return ErrRelayQuota
			}
			result = tx.Unscoped().Model(&Token{}).Where("id = ? AND user_id = ? AND remain_quota >= 0 AND remain_quota <= ? AND used_quota >= 0 AND used_quota <= ?", request.TokenID, userID, common.MaxWalletQuota-delta, common.MaxWalletQuota-actual).
				Updates(map[string]any{"remain_quota": gorm.Expr("remain_quota + ?", delta), "used_quota": gorm.Expr("used_quota + ?", actual), "accessed_time": now})
			if result.Error != nil {
				return result.Error
			}
			// Reserved is strictly positive: either delta or actual changes a column.
			if result.RowsAffected != 1 {
				return ErrRelayQuota
			}
		}
		if err := tx.Model(&RelayWalletRequest{}).Where("id = ?", id).Updates(updates).Error; err != nil {
			return err
		}
		if err := tx.Create(&RelayWalletEvent{RequestID: id, State: target, WalletDelta: delta, Actual: actual, Reason: reason, CreatedAt: now}).Error; err != nil {
			return err
		}
		changed = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return changed, nil
}

// RecoverRelayWallet never sends upstream requests. Stale submitted requests
// become unknown; their funds remain held for a full day from that transition.
func RecoverRelayWallet(db *gorm.DB, limit int) (int, error) {
	if limit < 1 || limit > 100 {
		return 0, ErrRelayInput
	}
	now := time.Now().Unix()
	var requests []RelayWalletRequest
	err := db.Where("(state IN ? AND updated_at <= ?) OR (state = ? AND unknown_at > 0 AND unknown_at <= ?)",
		[]string{"reserved", "submitted"}, now-300, "unknown", now-86400).
		Order("updated_at").Order("id").Limit(limit).Find(&requests).Error
	if err != nil {
		return 0, err
	}
	changed := 0
	for _, request := range requests {
		target := "released"
		if request.State == "submitted" {
			target = "unknown"
		}
		advanced, err := AdvanceRelayWallet(db, request.ID, request.UserID, target, 0)
		if errors.Is(err, ErrRelayState) {
			continue
		} // A live worker won the transition.
		if err != nil {
			return changed, err
		}
		if advanced {
			changed++
		}
	}
	return changed, nil
}
