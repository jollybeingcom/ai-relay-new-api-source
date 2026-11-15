package model

import (
	"errors"
	"sort"
	"time"

	"gorm.io/gorm"
)

// Thresholds follow the requirement: unknown usage alerts after 10 minutes
// and must be handled within 24 hours. Others are operational defaults.
const (
	relayAnomalyUnknownAlert = int64(600)
	relayAnomalyStuck        = int64(3600)
	relayAnomalyDay          = int64(86400)
	relayAnomalySamples      = 10
	relayAnomalyFundingScan  = 500
)

type RelayAnomalyItem struct {
	Kind   string `json:"kind"`
	Ref    string `json:"ref"`
	UserID int    `json:"user_id"`
	Since  int64  `json:"since"`
	Amount int64  `json:"amount"`
}

type RelayAnomalyReport struct {
	CheckedAt       int64              `json:"checked_at"`
	Counts          map[string]int64   `json:"counts"`
	Items           []RelayAnomalyItem `json:"items"`
	RiskMode        string             `json:"risk_mode"`
	MissingCosts    int64              `json:"missing_costs"`
	FundingChecked  int                `json:"funding_checked"`
	FundingComplete bool               `json:"funding_complete"`
}

// Kinds, in display order. Every kind is always present in Counts.
var RelayAnomalyKinds = []string{"payment_attention", "payment_overdue", "request_unknown_alert", "request_unknown_overdue",
	"request_stuck", "request_released_unknown", "refund_review", "refund_waiting", "log_delivery_delayed", "funding_inconsistent"}

// ReadRelayAnomalies is read-only. It reports facts for a person to handle and
// never repairs balances, releases holds or retries channel calls itself.
func ReadRelayAnomalies(db *gorm.DB, now time.Time) (RelayAnomalyReport, error) {
	at := now.Unix()
	r := RelayAnomalyReport{CheckedAt: at, Counts: map[string]int64{}, Items: []RelayAnomalyItem{}}
	for _, kind := range RelayAnomalyKinds {
		r.Counts[kind] = 0
	}
	err := relayWalletTransaction(db, func(tx *gorm.DB) error {
		collect := func(kind string, query *gorm.DB, rows func() ([]RelayAnomalyItem, error)) error {
			var count int64
			if err := query.Session(&gorm.Session{}).Count(&count).Error; err != nil {
				return err
			}
			r.Counts[kind] = count
			if count == 0 {
				return nil
			}
			items, err := rows()
			if err != nil {
				return err
			}
			for i := range items {
				items[i].Kind = kind
			}
			r.Items = append(r.Items, items...)
			return nil
		}
		orders := func(query *gorm.DB, since func(RelayPayOrder) int64) func() ([]RelayAnomalyItem, error) {
			return func() ([]RelayAnomalyItem, error) {
				var list []RelayPayOrder
				if err := query.Session(&gorm.Session{}).Select("id,user_id,quota,created_at,expires_at,updated_at").Order("id").Limit(relayAnomalySamples).Find(&list).Error; err != nil {
					return nil, err
				}
				items := make([]RelayAnomalyItem, 0, len(list))
				for _, o := range list {
					items = append(items, RelayAnomalyItem{Ref: o.ID, UserID: o.UserID, Since: since(o), Amount: o.Quota})
				}
				return items, nil
			}
		}
		requests := func(query *gorm.DB, since func(RelayWalletRequest) int64) func() ([]RelayAnomalyItem, error) {
			return func() ([]RelayAnomalyItem, error) {
				var list []RelayWalletRequest
				if err := query.Session(&gorm.Session{}).Select("id,user_id,reserved,created_at,updated_at,unknown_at").Order("updated_at").Order("id").Limit(relayAnomalySamples).Find(&list).Error; err != nil {
					return nil, err
				}
				items := make([]RelayAnomalyItem, 0, len(list))
				for _, q := range list {
					items = append(items, RelayAnomalyItem{Ref: q.ID, UserID: q.UserID, Since: since(q), Amount: q.Reserved})
				}
				return items, nil
			}
		}
		refunds := func(query *gorm.DB) func() ([]RelayAnomalyItem, error) {
			return func() ([]RelayAnomalyItem, error) {
				var list []RelayRefund
				if err := query.Session(&gorm.Session{}).Select("id,user_id,quota,created_at").Order("created_at").Order("id").Limit(relayAnomalySamples).Find(&list).Error; err != nil {
					return nil, err
				}
				items := make([]RelayAnomalyItem, 0, len(list))
				for _, f := range list {
					items = append(items, RelayAnomalyItem{Ref: f.ID, UserID: f.UserID, Since: f.CreatedAt, Amount: f.Quota})
				}
				return items, nil
			}
		}
		attention := tx.Model(&RelayPayOrder{}).Where("credit = ?", "attention")
		if err := collect("payment_attention", attention, orders(attention, func(o RelayPayOrder) int64 { return o.UpdatedAt })); err != nil {
			return err
		}
		overdue := tx.Model(&RelayPayOrder{}).Where("state = ? AND expires_at < ?", "pending", at-relayAnomalyStuck)
		if err := collect("payment_overdue", overdue, orders(overdue, func(o RelayPayOrder) int64 { return o.ExpiresAt })); err != nil {
			return err
		}
		unknownSince := func(q RelayWalletRequest) int64 { return q.UnknownAt }
		alert := tx.Model(&RelayWalletRequest{}).Where("state = ? AND unknown_at < ? AND unknown_at >= ?", "unknown", at-relayAnomalyUnknownAlert, at-relayAnomalyDay)
		if err := collect("request_unknown_alert", alert, requests(alert, unknownSince)); err != nil {
			return err
		}
		late := tx.Model(&RelayWalletRequest{}).Where("state = ? AND unknown_at < ?", "unknown", at-relayAnomalyDay)
		if err := collect("request_unknown_overdue", late, requests(late, unknownSince)); err != nil {
			return err
		}
		stuck := tx.Model(&RelayWalletRequest{}).Where("state IN ? AND updated_at < ?", []string{"reserved", "submitted"}, at-relayAnomalyStuck)
		if err := collect("request_stuck", stuck, requests(stuck, func(q RelayWalletRequest) int64 { return q.UpdatedAt })); err != nil {
			return err
		}
		// Released after an unknown result: possible upstream cost, platform loss.
		released := tx.Model(&RelayWalletRequest{}).Where("state = ? AND unknown_at > 0 AND updated_at >= ?", "released", at-30*relayAnomalyDay)
		if err := collect("request_released_unknown", released, requests(released, func(q RelayWalletRequest) int64 { return q.UpdatedAt })); err != nil {
			return err
		}
		review := tx.Model(&RelayRefund{}).Where("needs_review = ? OR state = ?", true, "unknown")
		if err := collect("refund_review", review, refunds(review)); err != nil {
			return err
		}
		waiting := tx.Model(&RelayRefund{}).Where("state = ? AND created_at < ?", "requested", at-relayAnomalyDay)
		if err := collect("refund_waiting", waiting, refunds(waiting)); err != nil {
			return err
		}
		delayed := tx.Model(&RelayWalletLogPending{}).Where("delivered_at = 0 AND next_attempt_at < ?", at-relayAnomalyUnknownAlert)
		if err := collect("log_delivery_delayed", delayed, func() ([]RelayAnomalyItem, error) {
			var list []RelayWalletLogPending
			if err := delayed.Session(&gorm.Session{}).Order("next_attempt_at").Limit(relayAnomalySamples).Find(&list).Error; err != nil {
				return nil, err
			}
			items := make([]RelayAnomalyItem, 0, len(list))
			for _, p := range list {
				items = append(items, RelayAnomalyItem{Ref: p.RequestID, Since: p.NextAttemptAt})
			}
			return items, nil
		}); err != nil {
			return err
		}
		var ready []RelayFundingReady
		if err := tx.Select("user_id,created_at").Order("user_id").Limit(relayAnomalyFundingScan + 1).Find(&ready).Error; err != nil {
			return err
		}
		r.FundingComplete = len(ready) <= relayAnomalyFundingScan
		if !r.FundingComplete {
			ready = ready[:relayAnomalyFundingScan]
		}
		for _, marker := range ready {
			r.FundingChecked++
			_, err := relayFundingSnapshot(tx, marker.UserID)
			if errors.Is(err, ErrRelayFunding) {
				r.Counts["funding_inconsistent"]++
				if r.Counts["funding_inconsistent"] <= relayAnomalySamples {
					r.Items = append(r.Items, RelayAnomalyItem{Kind: "funding_inconsistent", Ref: "user", UserID: marker.UserID, Since: marker.CreatedAt})
				}
				continue
			}
			if err != nil {
				return err
			}
		}
		report, err := relayRiskReport(tx, RelayRiskMonth(now))
		if err != nil {
			return err
		}
		r.RiskMode, r.MissingCosts = report.State.Mode, report.MissingCosts
		return nil
	})
	sort.SliceStable(r.Items, func(i, j int) bool { return r.Items[i].Since < r.Items[j].Since })
	return r, err
}
