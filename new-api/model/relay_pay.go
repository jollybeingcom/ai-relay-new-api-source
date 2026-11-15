package model

import (
	"errors"
	"regexp"
	"time"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Top-up orders credit the existing New API wallet (User.Quota); there is no
// second balance. Payment state and credit state are recorded separately.
var (
	ErrRelayPayMismatch = errors.New("payment fact does not match the order snapshot")
	ErrRelayPayLimit    = errors.New("too many unpaid top-up orders")
	relayPayOrderID     = regexp.MustCompile(`^RP[0-9a-f]{30}$`)
	relayPayTradeNo     = regexp.MustCompile(`^[0-9A-Za-z_-]{1,64}$`)
)

type RelayPayOrder struct {
	ID            string  `gorm:"type:varchar(32);primaryKey"`
	UserID        int     `gorm:"not null;index"`
	AmountFen     int64   `gorm:"not null"`
	Quota         int64   `gorm:"not null"`
	Currency      string  `gorm:"type:varchar(3);not null"`
	Channel       string  `gorm:"type:varchar(16);not null;uniqueIndex:relay_pay_trade,priority:1"`
	MerchantApp   string  `gorm:"type:varchar(64);not null"`
	ConfigVersion string  `gorm:"type:varchar(64);not null"`
	RateVersion   string  `gorm:"type:varchar(32);not null"`
	State         string  `gorm:"type:varchar(16);not null;index"` // pending, paid, closed
	Credit        string  `gorm:"type:varchar(16);not null"`       // none, credited, attention
	TradeNo       *string `gorm:"type:varchar(64);uniqueIndex:relay_pay_trade,priority:2"`
	CreatedAt     int64   `gorm:"not null"`
	ExpiresAt     int64   `gorm:"not null"`
	PaidAt        int64   `gorm:"not null"`
	UpdatedAt     int64   `gorm:"not null"`
	NextQueryAt   int64   `gorm:"not null;index"`
	QueryAttempts int     `gorm:"not null"`
}

// Kinds are unique per order, so a credit can be recorded only once.
type RelayPayEvent struct {
	ID         int64  `gorm:"primaryKey"`
	OrderID    string `gorm:"type:varchar(32);not null;uniqueIndex:relay_pay_event,priority:1"`
	Kind       string `gorm:"type:varchar(16);not null;uniqueIndex:relay_pay_event,priority:2"`
	QuotaDelta int64  `gorm:"not null"`
	Evidence   string `gorm:"type:varchar(64);not null"`
	CreatedAt  int64  `gorm:"not null"`
}

func (order *RelayPayOrder) BeforeUpdate(tx *gorm.DB) error {
	fields, ok := tx.Statement.Dest.(map[string]any)
	if !ok {
		return ErrRelayInput
	}
	for field := range fields {
		switch field {
		case "state", "credit", "trade_no", "paid_at", "updated_at", "next_query_at", "query_attempts":
		default:
			return ErrRelayConflict // The amount, user and merchant snapshot never change.
		}
	}
	return nil
}

func (*RelayPayOrder) BeforeDelete(*gorm.DB) error { return ErrRelayState }
func (*RelayPayEvent) BeforeUpdate(*gorm.DB) error { return ErrRelayState }
func (*RelayPayEvent) BeforeDelete(*gorm.DB) error { return ErrRelayState }

func MigrateRelayPay(db *gorm.DB) error {
	return db.AutoMigrate(&RelayPayOrder{}, &RelayPayEvent{})
}

// CreateRelayPayOrder stores a server-built snapshot. Callers pass trusted
// values only: the amount tier, quota rate and merchant come from server config.
func CreateRelayPayOrder(db *gorm.DB, order RelayPayOrder, maxPending int) (RelayPayOrder, error) {
	now := time.Now().Unix()
	if !relayPayOrderID.MatchString(order.ID) || order.UserID <= 0 || order.AmountFen <= 0 || order.AmountFen > 10000000 ||
		order.Quota <= 0 || order.Quota > common.MaxWalletQuota || order.Currency != "CNY" || order.Channel == "" || len(order.Channel) > 16 ||
		order.MerchantApp == "" || len(order.MerchantApp) > 64 || order.ConfigVersion == "" || len(order.ConfigVersion) > 64 ||
		order.RateVersion == "" || len(order.RateVersion) > 32 || order.ExpiresAt <= now || maxPending < 1 {
		return RelayPayOrder{}, ErrRelayInput
	}
	order.State, order.Credit, order.TradeNo = "pending", "none", nil
	order.CreatedAt, order.UpdatedAt, order.PaidAt, order.NextQueryAt, order.QueryAttempts = now, now, 0, now+60, 0
	denied := false
	err := relayWalletTransaction(db, func(tx *gorm.DB) error {
		var owner User
		if err := tx.Where("id = ? AND role = ? AND status = ?", order.UserID, common.RoleCommonUser, common.UserStatusEnabled).Take(&owner).Error; err != nil {
			return ErrRelayQuota
		}
		var pending int64
		if err := tx.Model(&RelayPayOrder{}).Where("user_id = ? AND state = ? AND expires_at > ?", order.UserID, "pending", now).Count(&pending).Error; err != nil {
			return err
		}
		if pending >= int64(maxPending) {
			return ErrRelayPayLimit
		}
		allowed, err := relayRiskAdmission(tx, 0, false)
		if err != nil {
			return err
		}
		if !allowed {
			denied = true
			return nil
		}
		if err := tx.Create(&order).Error; err != nil {
			return err
		}
		return tx.Create(&RelayPayEvent{OrderID: order.ID, Kind: "created", Evidence: "", CreatedAt: now}).Error
	})
	if err != nil {
		return RelayPayOrder{}, err
	}
	if denied {
		return RelayPayOrder{}, ErrRelayStopped
	}
	return order, nil
}

// RelayPayFact is a verified channel result: a signed notification or a signed
// query response. Unverified browser returns must never construct one.
type RelayPayFact struct {
	OrderID   string
	Channel   string
	App       string
	TradeNo   string
	AmountFen int64
	Currency  string
	Status    string // paid, closed, pending
	Evidence  string // SHA-256 of the verified payload, for audit
}

var relayPayEvidence = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ApplyRelayPayFact is shared by notifications and queries. The order lock and
// unique trade number make concurrent duplicates credit the wallet only once.
// A late payment after local closure is still recorded and credited.
func ApplyRelayPayFact(db *gorm.DB, fact RelayPayFact) (RelayPayOrder, error) {
	if !relayPayOrderID.MatchString(fact.OrderID) || !relayPayEvidence.MatchString(fact.Evidence) ||
		fact.Status != "paid" && fact.Status != "closed" && fact.Status != "pending" {
		return RelayPayOrder{}, ErrRelayInput
	}
	if fact.Status == "paid" && !relayPayTradeNo.MatchString(fact.TradeNo) {
		return RelayPayOrder{}, ErrRelayInput
	}
	var order RelayPayOrder
	err := relayWalletTransaction(db, func(tx *gorm.DB) error {
		if err := lockForUpdate(tx).Where("id = ?", fact.OrderID).Take(&order).Error; err != nil {
			return err
		}
		if fact.Channel != order.Channel || fact.App != order.MerchantApp {
			return ErrRelayPayMismatch
		}
		now := time.Now().Unix()
		switch fact.Status {
		case "pending":
			return nil
		case "closed":
			if order.State != "pending" {
				return nil // A paid order is never reopened or closed by a stale query.
			}
			order.State, order.UpdatedAt = "closed", now
			if err := tx.Model(&RelayPayOrder{}).Where("id = ?", order.ID).Updates(map[string]any{"state": "closed", "updated_at": now}).Error; err != nil {
				return err
			}
			return tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&RelayPayEvent{OrderID: order.ID, Kind: "closed", Evidence: fact.Evidence, CreatedAt: now}).Error
		}
		if fact.Currency != order.Currency || fact.AmountFen != order.AmountFen {
			return ErrRelayPayMismatch
		}
		if order.State == "paid" {
			if order.TradeNo == nil || *order.TradeNo != fact.TradeNo {
				return ErrRelayConflict
			}
			return nil // Duplicate notification or query: already credited once.
		}
		credit := "attention" // Paid money for an unavailable account is kept for refund review.
		result := tx.Model(&User{}).Where("id = ? AND role = ? AND status = ? AND quota >= 0 AND quota <= ?",
			order.UserID, common.RoleCommonUser, common.UserStatusEnabled, common.MaxWalletQuota-order.Quota).
			Update("quota", gorm.Expr("quota + ?", order.Quota))
		if result.Error != nil {
			return result.Error
		}
		delta := int64(0)
		if result.RowsAffected == 1 {
			credit, delta = "credited", order.Quota
		}
		trade := fact.TradeNo
		if err := tx.Model(&RelayPayOrder{}).Where("id = ? AND state <> ?", order.ID, "paid").Updates(map[string]any{
			"state": "paid", "credit": credit, "trade_no": trade, "paid_at": now, "updated_at": now}).Error; err != nil {
			return err // Includes a trade number already bound to another order.
		}
		order.State, order.Credit, order.TradeNo, order.PaidAt, order.UpdatedAt = "paid", credit, &trade, now, now
		if credit == "credited" {
			if err := tx.Create(&RelayFundingEntry{ID: order.ID, UserID: order.UserID, Kind: "order", Quota: order.Quota, OccurredAt: now, Reason: "payment-credit"}).Error; err != nil {
				return err
			}
		}
		if err := tx.Create(&RelayPayEvent{OrderID: order.ID, Kind: "paid", Evidence: fact.Evidence, CreatedAt: now}).Error; err != nil {
			return err
		}
		return tx.Create(&RelayPayEvent{OrderID: order.ID, Kind: credit, QuotaDelta: delta, Evidence: fact.Evidence, CreatedAt: now}).Error
	})
	if err != nil {
		return RelayPayOrder{}, err
	}
	return order, nil
}

// DueRelayPayOrders lists pending orders for bounded background queries.
func DueRelayPayOrders(db *gorm.DB, limit int) ([]RelayPayOrder, error) {
	var orders []RelayPayOrder
	err := db.Where("state = ? AND next_query_at <= ?", "pending", time.Now().Unix()).Order("next_query_at").Order("id").Limit(limit).Find(&orders).Error
	return orders, err
}

// ScheduleRelayPayQuery backs off exponentially: 1, 2, 4 ... minutes, capped
// at one hour, so a provider is never polled in a tight loop.
func ScheduleRelayPayQuery(db *gorm.DB, order RelayPayOrder) error {
	delay := int64(60) << min(order.QueryAttempts, 6)
	return db.Model(&RelayPayOrder{}).Where("id = ? AND state = ?", order.ID, "pending").Updates(map[string]any{
		"next_query_at": time.Now().Unix() + delay, "query_attempts": order.QueryAttempts + 1, "updated_at": time.Now().Unix()}).Error
}

func GetRelayPayOrder(db *gorm.DB, userID int, id string) (RelayPayOrder, error) {
	var order RelayPayOrder
	if userID <= 0 || !relayPayOrderID.MatchString(id) {
		return order, ErrRelayInput
	}
	err := db.Where("id = ? AND user_id = ?", id, userID).Take(&order).Error
	return order, err
}

func ListRelayPayOrders(db *gorm.DB, userID, page int) ([]RelayPayOrder, int64, error) {
	var orders []RelayPayOrder
	var total int64
	if userID <= 0 || page < 1 || page > 1000 {
		return nil, 0, ErrRelayInput
	}
	query := db.Model(&RelayPayOrder{}).Where("user_id = ?", userID)
	if err := query.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	err := query.Order("created_at DESC").Order("id DESC").Offset((page - 1) * 20).Limit(20).Find(&orders).Error
	return orders, total, err
}
