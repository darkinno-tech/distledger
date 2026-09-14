package distledger_test

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/im10furry/distledger"
	"github.com/im10furry/distledger/store/memory"
)

// This file pins the COST of the heartbeat, not just its result.
//
// Correctness tests cannot see the difference between "asks the store which
// tenants have work" and "asks for every tenant and opens a transaction for
// each", because both settle the same commissions. The difference only shows up
// at scale, which is the worst place to discover it. So these tests count store
// round trips directly.

// countingStore records how many transactions and reads a caller performs.
type countingStore struct {
	inner distledger.Store

	views   atomic.Int64
	updates atomic.Int64
}

func (s *countingStore) View(ctx context.Context, fn func(context.Context, distledger.Reader) error) error {
	s.views.Add(1)
	return s.inner.View(ctx, fn)
}

func (s *countingStore) Update(ctx context.Context, fn func(context.Context, distledger.Tx) error) error {
	s.updates.Add(1)
	return s.inner.Update(ctx, fn)
}

func (s *countingStore) Close() error { return s.inner.Close() }

func (s *countingStore) reset() {
	s.views.Store(0)
	s.updates.Store(0)
}

func newCountingLedger(t *testing.T, tenants int) (*distledger.Ledger, *countingStore, *distledger.ManualClock) {
	t.Helper()
	clock := distledger.NewManualClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	store := &countingStore{inner: memory.New()}
	led, err := distledger.New(distledger.Config{
		Store: store,
		Clock: clock,
		Rules: distledger.Rules{Levels: 1, RateBP: []distledger.Rate{1000}, FreezeDays: 7},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = led.Close() })

	ctx := context.Background()
	for tn := 0; tn < tenants; tn++ {
		tenantID := int64(tn)
		if _, err := led.BindAgent(ctx, distledger.BindAgentRequest{
			TenantID: tenantID, UserID: 3001, Status: distledger.AgentActive,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := led.BindBuyer(ctx, distledger.BindBuyerRequest{
			TenantID: tenantID, BuyerUserID: 4001, AgentUserID: 3001, Source: distledger.SourceLink,
		}); err != nil {
			t.Fatal(err)
		}
	}
	return led, store, clock
}

// TestMaintainCostIsIndependentOfIdleTenants is the reason the port asks for work
// rather than for tenants (ADR-035).
//
// A heartbeat that opens one transaction per tenant costs O(tenants) per tick
// whether or not any work exists. At a thousand tenants that is a thousand
// transactions a minute of pure overhead; at tens of thousands it dominates. The
// assertion here is on the operation count, because the settlement result is
// identical either way.
func TestMaintainCostIsIndependentOfIdleTenants(t *testing.T) {
	for _, tenants := range []int{1, 50, 500} {
		t.Run(fmt.Sprintf("%d tenants", tenants), func(t *testing.T) {
			led, store, _ := newCountingLedger(t, tenants)
			ctx := context.Background()

			store.reset()
			res, err := led.Maintain(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if res.SettledCount != 0 {
				t.Fatalf("settled %d with nothing due", res.SettledCount)
			}

			// One read to discover there is no work, and no transaction at all.
			if got := store.views.Load(); got != 1 {
				t.Errorf("%d tenants, nothing due: %d reads, want exactly 1", tenants, got)
			}
			if got := store.updates.Load(); got != 0 {
				t.Errorf("%d tenants, nothing due: %d transactions, want 0", tenants, got)
			}
		})
	}
}

// TestMaintainCostScalesWithWorkNotWithTenants is the other half: when work does
// exist, the cost must follow the work.
func TestMaintainCostScalesWithWorkNotWithTenants(t *testing.T) {
	led, store, clock := newCountingLedger(t, 200)
	ctx := context.Background()
	start := clock.Now()

	// Give exactly three tenants work.
	for _, tenantID := range []int64{7, 42, 199} {
		if _, err := led.OnOrderPaid(ctx, distledger.OrderPaidEvent{
			TenantID: tenantID, OrderID: "ORD-1", BuyerUserID: 4001,
			PaidAmount: 10000, PaidAt: start,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := led.OnOrderReceived(ctx, distledger.OrderReceivedEvent{
			TenantID: tenantID, OrderID: "ORD-1",
			ReceivedAt: start,
		}); err != nil {
			t.Fatal(err)
		}
	}
	clock.Advance(30 * 24 * time.Hour)

	store.reset()
	res, err := led.Maintain(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.SettledCount != 3 {
		t.Fatalf("settled %d, want 3", res.SettledCount)
	}

	// A handful of operations for three tenants out of two hundred, rather than
	// one per tenant.
	if got := store.updates.Load(); got > 6 {
		t.Errorf("%d transactions for 3 tenants out of 200, want a handful", got)
	}
	if got := store.views.Load(); got > 4 {
		t.Errorf("%d reads for 3 tenants out of 200, want a handful", got)
	}
}
