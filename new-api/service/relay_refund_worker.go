package service

import (
	"context"
	"errors"
	"time"

	"github.com/QuantumNous/new-api/model"
	"gorm.io/gorm"
)

func SyncRelayRefunds(ctx context.Context, limit int) (int, error) {
	channel, err := CurrentRelayPayChannel()
	if err != nil {
		return 0, err
	}
	return syncRelayRefunds(ctx, model.DB, channel, limit)
}

func syncRelayRefunds(ctx context.Context, db *gorm.DB, channel RelayPayChannel, limit int) (int, error) {
	refunds, err := model.DueRelayRefunds(db, limit)
	if err != nil {
		return 0, err
	}
	changed := 0
	var failures []error
	for _, refund := range refunds {
		if err := ctx.Err(); err != nil {
			return changed, errors.Join(append(failures, err)...)
		}
		var order model.RelayPayOrder
		if err := db.Where("id = ?", refund.OrderID).Take(&order).Error; err != nil {
			failures = append(failures, err)
			failures = append(failures, model.DeferRelayRefundReview(db, refund.ID))
			continue
		}
		if !refundBound(order, refund, channel) {
			failures = append(failures, model.ErrRelayPayMismatch)
			failures = append(failures, model.DeferRelayRefundReview(db, refund.ID))
			continue // No dispatch with a different merchant/version; frozen for review.
		}
		first := refund.State == "approved_frozen"
		var claimed bool
		if first {
			claimed, err = model.ClaimRelayRefund(db, refund.ID)
		} else {
			claimed, err = model.ClaimRelayRefundQuery(db, refund.ID)
		}
		if err != nil {
			failures = append(failures, err)
			continue
		}
		if !claimed {
			continue
		}
		callCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		var fact model.RelayRefundFact
		if first {
			fact, err = channel.Refund(callCtx, order, refund)
		} else {
			fact, err = channel.QueryRefund(callCtx, order, refund)
		}
		cancel()
		if err != nil {
			// Do not persist SDK errors: they may contain signed requests or PII.
			failures = append(failures, errors.New("refund channel result unavailable or unverified"))
			fact = refundFact(order, refund, "unknown", "unverified-or-unavailable:"+refund.ID)
		}
		if applyErr := model.ApplyRelayRefundFact(db, fact); applyErr != nil {
			failures = append(failures, applyErr)
			continue
		}
		changed++
	}
	return changed, errors.Join(failures...)
}
