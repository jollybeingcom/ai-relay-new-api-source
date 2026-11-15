package model

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"time"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Durable consumption facts in the primary database. A separate log database
// cannot participate in this wallet transaction; projection must use this
// request key idempotently, without ever replaying a charge.
type RelayWalletReceipt struct {
	RequestID    string `gorm:"type:varchar(36);primaryKey"`
	UserID       int    `gorm:"not null;index"`
	TokenID      int    `gorm:"not null"`
	ChannelID    int    `gorm:"not null;index"`
	Model        string `gorm:"type:varchar(200);not null"`
	PriceVersion string `gorm:"type:varchar(100);not null"`
	Input        int    `gorm:"not null"`
	CacheRead    int    `gorm:"not null"`
	Output       int    `gorm:"not null"`
	Actual       int64  `gorm:"not null"`
	CreatedAt    int64  `gorm:"not null"`
}

func (*RelayWalletReceipt) BeforeUpdate(*gorm.DB) error { return ErrRelayState }
func (*RelayWalletReceipt) BeforeDelete(*gorm.DB) error { return ErrRelayState }

// Only trusted normalized usage may reach this boundary. Financial effects,
// channel statistics and receipt either commit together or all roll back.
func SettleRelayWalletReceipt(db *gorm.DB, receipt RelayWalletReceipt) error {
	if !relayRequestPattern.MatchString(receipt.RequestID) || receipt.UserID <= 0 || receipt.TokenID <= 0 || receipt.ChannelID <= 0 ||
		len(receipt.Model) == 0 || len(receipt.Model) > 200 || len(receipt.PriceVersion) == 0 || len(receipt.PriceVersion) > 100 ||
		receipt.Input < 0 || receipt.Input > common.MaxQuota || receipt.CacheRead < 0 || receipt.CacheRead > receipt.Input ||
		receipt.Output < 0 || receipt.Output > common.MaxQuota-receipt.Input || receipt.Actual < 0 || receipt.Actual > common.MaxQuota {
		return ErrRelayInput
	}
	return relayWalletTransaction(db, func(tx *gorm.DB) error {
		var request RelayWalletRequest
		if err := lockForUpdate(tx).Where("id = ? AND user_id = ?", receipt.RequestID, receipt.UserID).Take(&request).Error; err != nil {
			return err
		}
		if request.TokenID != receipt.TokenID || request.PriceVersion != receipt.PriceVersion {
			return ErrRelayConflict
		}
		var saved RelayWalletReceipt
		err := tx.Where("request_id = ?", receipt.RequestID).Take(&saved).Error
		if err == nil {
			receipt.CreatedAt = saved.CreatedAt
			if saved != receipt || request.State != "settled" || request.Actual != receipt.Actual {
				return ErrRelayConflict
			}
			return settleRelayWalletCost(tx, request, receipt, true)
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if request.State == "settled" {
			return ErrRelayConflict
		} // Never invent facts for an older settlement.
		changed, err := advanceRelayWallet(tx, receipt.RequestID, receipt.UserID, "settled", receipt.Actual, true)
		if err != nil {
			return err
		}
		if !changed {
			return ErrRelayState
		}
		// Lock/read even for zero charge: MySQL may report zero changed rows for
		// +0, and zero-cost completions still require a valid channel and receipt.
		var channel Channel
		if err := lockForUpdate(tx).Where("id = ?", receipt.ChannelID).Take(&channel).Error; err != nil {
			return err
		}
		if channel.UsedQuota < 0 || int64(channel.UsedQuota) > common.MaxWalletQuota-receipt.Actual {
			return ErrRelayQuota
		}
		if receipt.Actual > 0 {
			if err := tx.Model(&Channel{}).Where("id = ?", receipt.ChannelID).Update("used_quota", gorm.Expr("used_quota + ?", receipt.Actual)).Error; err != nil {
				return err
			}
		}
		receipt.CreatedAt = time.Now().Unix()
		if err := settleRelayWalletCost(tx, request, receipt, false); err != nil {
			return err
		}
		if err := tx.Create(&receipt).Error; err != nil {
			return err
		}
		return tx.Create(&RelayWalletLogPending{RequestID: receipt.RequestID}).Error
	})
}

type RelayWalletLogPending struct {
	RequestID     string `gorm:"type:varchar(36);primaryKey"`
	DeliveredAt   int64  `gorm:"not null;index:relay_log_pending,priority:1"`
	NextAttemptAt int64  `gorm:"not null;index:relay_log_pending,priority:2"`
}

// Stored in the destination log database, in the SAME transaction as Log.
// Keep markers even when display logs expire, so replay cannot resurrect them.
type RelayWalletLogMarker struct {
	RequestID   string `gorm:"type:varchar(36);primaryKey"`
	ReceiptHash string `gorm:"type:varchar(64);not null"`
}

func MigrateRelayWalletLogs(db *gorm.DB) error {
	if db == nil {
		return ErrRelayInput
	}
	switch db.Dialector.Name() {
	case "sqlite", "mysql", "postgres":
		return db.AutoMigrate(&RelayWalletLogMarker{})
	default:
		return errors.New("relay wallet log projection requires a transactional SQLite, MySQL or PostgreSQL database")
	}
}

// Safe across independent source/destination databases. If the process dies
// after the log commit but before acknowledging the source, replay finds the
// marker and only acknowledges; it never creates another log or debits funds.
func ProjectRelayWalletLog(source, destination *gorm.DB, requestID string) error {
	if source == nil || destination == nil || !relayRequestPattern.MatchString(requestID) {
		return ErrRelayInput
	}
	switch destination.Dialector.Name() {
	case "sqlite", "mysql", "postgres":
	default:
		return ErrRelayInput
	}
	var receipt RelayWalletReceipt
	if err := source.Where("request_id = ?", requestID).Take(&receipt).Error; err != nil {
		return err
	}
	encoded, err := common.Marshal(receipt)
	if err != nil {
		return err
	}
	marker := RelayWalletLogMarker{RequestID: requestID, ReceiptHash: fmt.Sprintf("%x", sha256.Sum256(encoded))}
	err = destination.Transaction(func(tx *gorm.DB) error {
		result := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&marker)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			var existing RelayWalletLogMarker
			if err := tx.Where("request_id = ?", requestID).Take(&existing).Error; err != nil {
				return err
			}
			if existing.ReceiptHash != marker.ReceiptHash {
				return ErrRelayConflict
			}
			return nil
		}
		other := NewLogOther()
		other.SetPublic("cache_tokens", receipt.CacheRead)
		other.SetPublic("price_version", receipt.PriceVersion)
		other.SetPublic("billing_source", "relay_wallet")
		// Actual was validated at settlement; do not silently truncate corruption.
		if receipt.Actual < 0 || receipt.Actual > common.MaxQuota {
			return ErrRelayInput
		}
		entry := Log{UserId: receipt.UserID, TokenId: receipt.TokenID, ChannelId: receipt.ChannelID,
			RequestId: requestID, CreatedAt: receipt.CreatedAt, Type: LogTypeConsume, ModelName: receipt.Model,
			Quota: int(receipt.Actual), PromptTokens: receipt.Input, CompletionTokens: receipt.Output, Other: other.JSONString()}
		return tx.Create(&entry).Error
	})
	if err != nil {
		return err
	}
	return source.Model(&RelayWalletLogPending{}).Where("request_id = ? AND delivered_at = 0", requestID).
		Update("delivered_at", time.Now().Unix()).Error
}

// A bounded worker pass. A failed item is delayed so it cannot starve the queue.
// Scheduling is separate from charging; this path never calls settlement.
func SyncRelayWalletLogs(source, destination *gorm.DB, limit int) (int, error) {
	if limit < 1 || limit > 100 {
		return 0, ErrRelayInput
	}
	var pending []RelayWalletLogPending
	if err := source.Where("delivered_at = 0 AND next_attempt_at <= ?", time.Now().Unix()).Order("request_id").Limit(limit).Find(&pending).Error; err != nil {
		return 0, err
	}
	delivered := 0
	var failures []error
	for _, item := range pending {
		if err := ProjectRelayWalletLog(source, destination, item.RequestID); err != nil {
			failures = append(failures, err)
			if err := source.Model(&RelayWalletLogPending{}).Where("request_id = ? AND delivered_at = 0", item.RequestID).
				Update("next_attempt_at", time.Now().Unix()+30).Error; err != nil {
				failures = append(failures, err)
			}
			continue
		}
		delivered++
	}
	return delivered, errors.Join(failures...)
}
