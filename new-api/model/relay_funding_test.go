package model

import (
	"math"
	"reflect"
	"testing"
)

func TestRelayFundingAllocation(t *testing.T) {
	order := func(id string, amount, at int64) RelayFundingSource {
		return RelayFundingSource{ID: id, Kind: "order", Quota: amount, PaidAt: at}
	}
	t.Run("FIFO_and_active_reservations", func(t *testing.T) {
		sources := []RelayFundingSource{order("B", 300, 2), order("A", 100, 1)}
		original := append([]RelayFundingSource(nil), sources...)
		got, err := AllocateRelayFunding(sources, 230, 50)
		if err != nil || len(got) != 2 {
			t.Fatalf("allocation: %v %v", got, err)
		}
		if got[0].Consumed != 100 || got[0].Refundable != 0 || got[1].Consumed != 20 || got[1].Reserved != 50 || got[1].Refundable != 230 {
			t.Fatalf("unexpected FIFO: %+v", got)
		}
		if !reflect.DeepEqual(sources, original) {
			t.Fatal("caller snapshot was mutated")
		}
	})
	t.Run("same_time_order_ID_and_non_cash", func(t *testing.T) {
		sources := []RelayFundingSource{order("B", 100, 1), {ID: "C", Kind: "adjustment", Quota: 100, PaidAt: 2}, order("A", 100, 1)}
		got, err := AllocateRelayFunding(sources, 150, 0)
		if err != nil || len(got) != 3 {
			t.Fatalf("allocation: %v %v", got, err)
		}
		if got[0].ID != "A" || got[0].Consumed != 100 || got[1].Refundable != 50 || got[2].Available != 100 || got[2].Refundable != 0 {
			t.Fatalf("unexpected cash attribution: %+v", got)
		}
	})
	t.Run("refunds_are_not_consumption", func(t *testing.T) {
		a := order("A", 100, 1)
		a.Refunded, a.Frozen = 40, 30
		got, err := AllocateRelayFunding([]RelayFundingSource{a, order("B", 100, 2)}, 80, 20)
		if err != nil || len(got) != 2 {
			t.Fatalf("allocation: %v %v", got, err)
		}
		if got[0].Consumed != 30 || got[0].Refundable != 0 || got[1].Consumed != 0 || got[1].Reserved != 20 || got[1].Refundable != 80 {
			t.Fatalf("refund counted twice: %+v", got)
		}
	})
	t.Run("adjustment_before_payment", func(t *testing.T) {
		got, err := AllocateRelayFunding([]RelayFundingSource{{ID: "adjust", Kind: "adjustment", Quota: 100, PaidAt: 0}, order("A", 100, 1)}, 120, 0)
		if err != nil || len(got) != 2 || got[0].Consumed != 80 || got[0].Refundable != 0 || got[1].Refundable != 100 {
			t.Fatalf("adjustment attribution: %+v %v", got, err)
		}
	})
	t.Run("invalid_snapshots_fail_closed", func(t *testing.T) {
		cases := []struct {
			sources             []RelayFundingSource
			available, reserved int64
		}{
			{nil, 1, 0}, {nil, -1, 0}, {nil, 0, -1},
			{[]RelayFundingSource{order("A", 10, 1)}, 10, 1},
			{[]RelayFundingSource{order("A", 10, 1), order("A", 10, 2)}, 0, 0},
			{[]RelayFundingSource{order("", 10, 1)}, 0, 0},
			{[]RelayFundingSource{order("A", 0, 1)}, 0, 0},
			{[]RelayFundingSource{order("A", 10, -1)}, 0, 0},
			{[]RelayFundingSource{{ID: "A", Kind: "gift", Quota: 10}}, 0, 0},
			{[]RelayFundingSource{{ID: "A", Kind: "order", Quota: 10, Refunded: 11}}, 0, 0},
			{[]RelayFundingSource{{ID: "A", Kind: "order", Quota: 10, Refunded: 5, Frozen: 6}}, 0, 0},
			{[]RelayFundingSource{{ID: "A", Kind: "order", Quota: 10, Frozen: -1}}, 0, 0},
			{[]RelayFundingSource{{ID: "A", Kind: "order", Quota: 10, Refunded: -1}}, 0, 0},
			{[]RelayFundingSource{{ID: "A", Kind: "adjustment", Quota: 10, Frozen: 1}}, 0, 0},
			{[]RelayFundingSource{{ID: "A", Kind: "adjustment", Quota: 10, Refunded: 1}}, 0, 0},
			{[]RelayFundingSource{order("A", math.MaxInt64, 1), order("B", 1, 2)}, 0, 0},
			{[]RelayFundingSource{order("A", math.MaxInt64, 1)}, math.MaxInt64, 1},
		}
		for i, tc := range cases {
			if _, err := AllocateRelayFunding(tc.sources, tc.available, tc.reserved); err != ErrRelayFunding {
				t.Fatalf("case %d: wanted reconciliation error, got %v", i, err)
			}
		}
	})
	t.Run("conservation_exhaustive_small_balances", func(t *testing.T) {
		for refunded := int64(0); refunded <= 10; refunded++ {
			for frozen := int64(0); frozen <= 10-refunded; frozen++ {
				net := 20 - refunded - frozen
				for available := int64(0); available <= net; available++ {
					for reserved := int64(0); reserved <= net-available; reserved++ {
						a := order("A", 10, 1)
						a.Refunded, a.Frozen = refunded, frozen
						got, err := AllocateRelayFunding([]RelayFundingSource{a, order("B", 10, 2)}, available, reserved)
						if err != nil {
							t.Fatal(err)
						}
						var totalAvailable, totalReserved, totalConsumed int64
						for _, allocation := range got {
							if allocation.Consumed+allocation.Reserved+allocation.Available+allocation.Refunded+allocation.Frozen != allocation.Quota || allocation.Refundable < 0 || allocation.Refundable > allocation.Available {
								t.Fatalf("source conservation failed: %+v", allocation)
							}
							totalAvailable += allocation.Available
							totalReserved += allocation.Reserved
							totalConsumed += allocation.Consumed
						}
						if totalAvailable != available || totalReserved != reserved || totalConsumed != net-available-reserved {
							t.Fatalf("wallet conservation failed: %+v", got)
						}
					}
				}
			}
		}
	})
}
