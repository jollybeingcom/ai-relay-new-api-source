package model

import (
	"regexp"

	"github.com/QuantumNous/new-api/common"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

// Provider prices are private accounting facts, separate from text-v1 sale
// snapshots and public consumption logs. Neither record may be rewritten.
type RelayWalletCostSnapshot struct {
	RequestID     string `gorm:"type:varchar(36);primaryKey"`
	PriceVersion  string `gorm:"type:varchar(100);not null"`
	ChannelID     int    `gorm:"not null"`
	Model         string `gorm:"type:varchar(200);not null"`
	InputRate     string `gorm:"type:varchar(32);not null"`
	CacheReadRate string `gorm:"type:varchar(32);not null"`
	OutputRate    string `gorm:"type:varchar(32);not null"`
	InputLimit    int    `gorm:"not null"`
	OutputLimit   int    `gorm:"not null"`
	Reserved      int64  `gorm:"not null"`
}

type RelayWalletCost struct {
	RequestID    string `gorm:"type:varchar(36);primaryKey"`
	PriceVersion string `gorm:"type:varchar(100);not null"`
	Actual       int64  `gorm:"not null"`
	CreatedAt    int64  `gorm:"not null;index"`
}

func (*RelayWalletCostSnapshot) BeforeUpdate(*gorm.DB) error { return ErrRelayState }
func (*RelayWalletCostSnapshot) BeforeDelete(*gorm.DB) error { return ErrRelayState }
func (*RelayWalletCost) BeforeUpdate(*gorm.DB) error         { return ErrRelayState }
func (*RelayWalletCost) BeforeDelete(*gorm.DB) error         { return ErrRelayState }

var relayCostRatePattern = regexp.MustCompile(`^(0|[1-9][0-9]{0,15})(\.[0-9]{1,12})?$`)

func (basis RelayWalletCostSnapshot) rates() ([3]decimal.Decimal, error) {
	var rates [3]decimal.Decimal
	if !relayRequestPattern.MatchString(basis.RequestID) || len(basis.PriceVersion) == 0 || len(basis.PriceVersion) > 100 ||
		basis.ChannelID <= 0 || len(basis.Model) == 0 || len(basis.Model) > 200 || basis.InputLimit <= 0 || basis.InputLimit > common.MaxQuota ||
		basis.OutputLimit <= 0 || basis.OutputLimit > common.MaxQuota-basis.InputLimit {
		return rates, ErrRelayInput
	}
	for i, raw := range []string{basis.InputRate, basis.CacheReadRate, basis.OutputRate} {
		if !relayCostRatePattern.MatchString(raw) {
			return rates, ErrRelayInput
		}
		value, err := decimal.NewFromString(raw)
		if err != nil {
			return rates, ErrRelayInput
		}
		rates[i] = value
	}
	return rates, nil
}

func (basis RelayWalletCostSnapshot) Bound() (int64, error) {
	rates, err := basis.rates()
	if err != nil {
		return 0, err
	}
	amount := decimal.Max(rates[0], rates[1]).Mul(decimal.NewFromInt(int64(basis.InputLimit))).
		Add(rates[2].Mul(decimal.NewFromInt(int64(basis.OutputLimit)))).Shift(-6).Ceil()
	quota, err := common.QuotaFromDecimalStrict(amount)
	return int64(quota), err
}

func (basis RelayWalletCostSnapshot) charge(receipt RelayWalletReceipt) (int64, error) {
	rates, err := basis.rates()
	if err != nil {
		return 0, err
	}
	if basis.RequestID != receipt.RequestID || basis.PriceVersion != receipt.PriceVersion || basis.ChannelID != receipt.ChannelID || basis.Model != receipt.Model ||
		receipt.Input < 0 || receipt.Input > basis.InputLimit || receipt.CacheRead < 0 || receipt.CacheRead > receipt.Input || receipt.Output < 0 || receipt.Output > basis.OutputLimit {
		return 0, ErrRelayConflict
	}
	amount := rates[0].Mul(decimal.NewFromInt(int64(receipt.Input - receipt.CacheRead))).
		Add(rates[1].Mul(decimal.NewFromInt(int64(receipt.CacheRead)))).
		Add(rates[2].Mul(decimal.NewFromInt(int64(receipt.Output)))).Shift(-6).Round(0)
	quota, err := common.QuotaFromDecimalStrict(amount)
	if err != nil {
		return 0, err
	}
	bound, err := basis.Bound()
	if err != nil || bound != basis.Reserved || int64(quota) > bound {
		return 0, ErrRelayConflict
	}
	return int64(quota), nil
}

// Called inside the same wallet transaction as the immutable receipt. Missing
// legacy costs remain missing, never invented as zero or priced at today's rate.
func settleRelayWalletCost(tx *gorm.DB, request RelayWalletRequest, receipt RelayWalletReceipt, replay bool) error {
	if !request.CostTracked {
		return nil
	}
	var basis RelayWalletCostSnapshot
	if err := tx.Where("request_id = ?", request.ID).Take(&basis).Error; err != nil {
		return err
	}
	amount, err := basis.charge(receipt)
	if err != nil {
		return err
	}
	cost := RelayWalletCost{RequestID: request.ID, PriceVersion: request.PriceVersion, Actual: amount, CreatedAt: receipt.CreatedAt}
	if replay {
		var saved RelayWalletCost
		if err := tx.Where("request_id = ?", request.ID).Take(&saved).Error; err != nil {
			return err
		}
		if cost != saved {
			return ErrRelayConflict
		}
		return nil
	}
	return tx.Create(&cost).Error
}
