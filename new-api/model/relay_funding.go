package model

import (
	"errors"
	"math"
	"sort"
)

// This calculator does not maintain a balance. Its caller must read all inputs
// from one relayWalletTransaction, then freeze against users.quota in that same
// transaction. Never use a browser-supplied snapshot to authorize a refund.
var ErrRelayFunding = errors.New("funding snapshot is inconsistent; manual reconciliation required")

type RelayFundingSource struct {
	ID       string `json:"id"`
	Kind     string `json:"kind"` // order or adjustment; adjustments cannot be refunded
	Quota    int64  `json:"quota"`
	PaidAt   int64  `json:"paid_at"`
	Refunded int64  `json:"refunded"` // successful refunds against this source
	Frozen   int64  `json:"frozen"`   // approved/processing/unknown refunds against this source
	Debited  int64  `json:"debited"`  // audited debits; only non-cash adjustments
}

type RelayFundingAllocation struct {
	RelayFundingSource
	Consumed   int64 `json:"consumed"`
	Reserved   int64 `json:"reserved"`
	Available  int64 `json:"available"`
	Refundable int64 `json:"refundable"`
}

// AllocateRelayFunding assigns settled consumption and then active generation
// reservations FIFO. Refunds stay bound to their original source. Removing the
// per-source refunded/frozen amounts BEFORE allocating prevents consumption
// from being counted against money already returned to the customer.
//
// available is users.quota; reserved is the sum of reserved amounts on active
// reserved/submitted/unknown requests. All values are integer quota, not fen.
// Missing historical sources must be reconciled explicitly, never silently
// synthesized as paid cash. A migration may add audited non-cash adjustments.
func AllocateRelayFunding(sources []RelayFundingSource, available, reserved int64) ([]RelayFundingAllocation, error) {
	if available < 0 || reserved < 0 {
		return nil, ErrRelayFunding
	}
	ordered := append([]RelayFundingSource(nil), sources...)
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].PaidAt != ordered[j].PaidAt {
			return ordered[i].PaidAt < ordered[j].PaidAt
		}
		return ordered[i].ID < ordered[j].ID
	})
	seen := make(map[string]bool, len(ordered))
	var remaining int64
	for _, source := range ordered {
		if source.ID == "" || seen[source.ID] || source.PaidAt < 0 || source.Quota <= 0 ||
			source.Refunded < 0 || source.Frozen < 0 || source.Debited < 0 || source.Refunded > source.Quota ||
			source.Frozen > source.Quota-source.Refunded || source.Debited > source.Quota-source.Refunded-source.Frozen ||
			(source.Kind != "order" && source.Kind != "adjustment") ||
			(source.Kind == "order" && source.Debited != 0) ||
			(source.Kind == "adjustment" && (source.Refunded != 0 || source.Frozen != 0)) {
			return nil, ErrRelayFunding
		}
		seen[source.ID] = true
		net := source.Quota - source.Refunded - source.Frozen - source.Debited
		if net > math.MaxInt64-remaining {
			return nil, ErrRelayFunding
		}
		remaining += net
	}
	// Subtract rather than add to avoid integer overflow on malformed snapshots.
	if available > remaining || reserved > remaining-available {
		return nil, ErrRelayFunding
	}
	consumed := remaining - available - reserved
	allocations := make([]RelayFundingAllocation, 0, len(ordered))
	for _, source := range ordered {
		net := source.Quota - source.Refunded - source.Frozen - source.Debited
		allocation := RelayFundingAllocation{RelayFundingSource: source}
		allocation.Consumed = min(consumed, net)
		consumed -= allocation.Consumed
		net -= allocation.Consumed
		allocation.Reserved = min(reserved, net)
		reserved -= allocation.Reserved
		allocation.Available = net - allocation.Reserved
		if source.Kind == "order" {
			allocation.Refundable = allocation.Available
		}
		allocations = append(allocations, allocation)
	}
	return allocations, nil
}
