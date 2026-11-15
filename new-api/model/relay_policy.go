package model

import (
	"regexp"
	"strings"
	"time"

	"gorm.io/gorm"
)

const (
	RelayPolicyPrice  = "price"
	RelayPolicyLimits = "limits"
	// A version must be published at least this long before it takes effect, so
	// every process has loaded it (cache refresh is seconds) before the switch.
	RelayPolicyLeadSeconds    = 60
	RelayPolicyHorizonSeconds = 30 * 24 * 3600
	relayPolicyMaxRows        = 1000
)

// RelayPolicyVersion is the append-only publication log of paid-chat prices and
// of the global generation limit. A row is never changed or removed: a mistake
// is corrected by publishing the next version. The unique (kind, scope,
// previous) index makes the history of one scope a single chain, so two
// operators who both saw the same current version cannot both publish.
type RelayPolicyVersion struct {
	ID          int64  `json:"id" gorm:"primaryKey;autoIncrement"`
	Kind        string `json:"kind" gorm:"type:varchar(16);not null;uniqueIndex:idx_relay_policy_chain,priority:1"`
	Scope       string `json:"scope" gorm:"type:varchar(160);not null;uniqueIndex:idx_relay_policy_chain,priority:2"`
	Previous    string `json:"previous" gorm:"type:varchar(100);not null;uniqueIndex:idx_relay_policy_chain,priority:3"`
	Version     string `json:"version" gorm:"type:varchar(100);not null;uniqueIndex:idx_relay_policy_version"`
	Payload     string `json:"-" gorm:"type:text;not null"`
	Digest      string `json:"digest" gorm:"type:char(64);not null"`
	OperatorID  int    `json:"operator_id" gorm:"not null"`
	Reason      string `json:"reason" gorm:"type:varchar(256);not null"`
	EffectiveAt int64  `json:"effective_at" gorm:"not null"`
	Created     int64  `json:"created_at" gorm:"not null"`
}

func (*RelayPolicyVersion) BeforeUpdate(*gorm.DB) error { return ErrRelayState }
func (*RelayPolicyVersion) BeforeDelete(*gorm.DB) error { return ErrRelayState }

var relayPolicyVersionPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,99}$`)

func MigrateRelayPolicy(db *gorm.DB) error {
	return db.AutoMigrate(&RelayPolicyVersion{})
}

// PublishRelayPolicy appends one version. base is the version in force while
// the scope has no row yet (the price-file version or the built-in limits).
// The caller has already validated the payload for its kind.
func PublishRelayPolicy(db *gorm.DB, row RelayPolicyVersion, base string, now time.Time) (RelayPolicyVersion, error) {
	row.Reason = strings.TrimSpace(row.Reason)
	if (row.Kind != RelayPolicyPrice && row.Kind != RelayPolicyLimits) || row.Scope == "" || len(row.Scope) > 160 ||
		!relayPolicyVersionPattern.MatchString(row.Version) || !relayPolicyVersionPattern.MatchString(row.Previous) ||
		!relayPolicyVersionPattern.MatchString(base) || row.Version == row.Previous || row.Version == base ||
		row.Payload == "" || len(row.Payload) > 8192 || !relayHashPattern.MatchString(row.Digest) || row.OperatorID <= 0 ||
		row.Reason == "" || len(row.Reason) > 256 ||
		row.EffectiveAt < now.Unix()+RelayPolicyLeadSeconds || row.EffectiveAt > now.Unix()+RelayPolicyHorizonSeconds {
		return RelayPolicyVersion{}, ErrRelayInput
	}
	row.ID, row.Created = 0, now.Unix()
	err := relayWalletTransaction(db, func(tx *gorm.DB) error {
		var total, same int64
		if err := tx.Model(&RelayPolicyVersion{}).Count(&total).Error; err != nil {
			return err
		}
		if total >= relayPolicyMaxRows {
			return ErrRelayState
		}
		if err := tx.Model(&RelayPolicyVersion{}).Where("version = ?", row.Version).Count(&same).Error; err != nil {
			return err
		}
		if same != 0 {
			return ErrRelayConflict
		}
		var chain []RelayPolicyVersion
		if err := tx.Where("kind = ? AND scope = ?", row.Kind, row.Scope).Order("id DESC").Limit(1).Find(&chain).Error; err != nil {
			return err
		}
		current := base
		if len(chain) == 1 {
			current = chain[0].Version
			// Effective times only move forward, so "newest row already in
			// force" is unambiguous and a later row never predates an earlier.
			if row.EffectiveAt <= chain[0].EffectiveAt {
				return ErrRelayConflict
			}
		}
		if row.Previous != current {
			return ErrRelayConflict
		}
		return tx.Create(&row).Error
	})
	if err != nil {
		return RelayPolicyVersion{}, err
	}
	return row, nil
}

// ListRelayPolicy returns the whole log in publication order.
func ListRelayPolicy(db *gorm.DB) ([]RelayPolicyVersion, error) {
	rows := []RelayPolicyVersion{}
	err := db.Order("id").Limit(relayPolicyMaxRows).Find(&rows).Error
	return rows, err
}
