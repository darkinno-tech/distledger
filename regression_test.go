package distledger_test

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync/atomic"
	"testing"
	"time"

	"github.com/im10furry/distledger"
	"github.com/im10furry/distledger/store/memory"
)

// This file holds regression tests for defects found during an adversarial
// review of v0.1. Each test names the behaviour it pins down, because the value
// of these tests is entirely in preventing a recurrence.

// TestItemCountIsBounded pins the work bound on accrual.
//
// Accrual runs inside a transaction that holds the global write lock, so an
// unbounded Items list lets one request stall every tenant's bookkeeping. The
// limit is about predictable tail latency, not about business rules.
func TestItemCountIsBounded(t *testing.T) {
	f := newFixture(t, distledger.Rules{Levels: 1, RateBP: []distledger.Rate{1000}})

	items := make([]distledger.OrderItem, distledger.MaxOrderItems+1)
	for i := range items {
		items[i] = distledger.OrderItem{ItemID: fmt.Sprintf("SKU-%d", i), Amount: 1}
	}
	_, err := f.led.OnOrderPaid(context.Background(), distledger.OrderPaidEvent{
		TenantID: tenant, OrderID: "ORD-1", BuyerUserID: 4001,
		PaidAmount: distledger.Money(len(items)), PaidAt: f.clock.Now(),
		Items: items,
	})
	if !errors.Is(err, distledger.ErrInvalidArgument) {
		t.Fatalf("expected ErrInvalidArgument for too many items, got %v", err)
	}
}

// TestZeroAmountItemsAreRejected pins the validation that closed a cheap way to
// make the engine do unbounded work: a long list of zero-amount lines passes the
// "items sum <= paid amount" check while still costing a full accrual pass each.
func TestZeroAmountItemsAreRejected(t *testing.T) {
	f := newFixture(t, distledger.Rules{Levels: 1, RateBP: []distledger.Rate{1000}})
	_, err := f.led.OnOrderPaid(context.Background(), distledger.OrderPaidEvent{
		TenantID: tenant, OrderID: "ORD-1", BuyerUserID: 4001,
		PaidAmount: 1000, PaidAt: f.clock.Now(),
		Items: []distledger.OrderItem{{ItemID: "SKU-A", Amount: 0}},
	})
	if !errors.Is(err, distledger.ErrInvalidArgument) {
		t.Fatalf("expected ErrInvalidArgument for a zero-amount item, got %v", err)
	}
}

// TestReplayWithDifferentBuyerIsRejected pins the contradiction check.
//
// The order-level short circuit keys on OrderID alone, so without this check a
// second event naming a different buyer would be reported as a successful
// replay and the genuine discrepancy would be swallowed.
func TestReplayWithDifferentBuyerIsRejected(t *testing.T) {
	f := newFixture(t, twoLevelRules())
	f.mustAgent(3001, 0, distledger.AgentActive)
	f.mustBuyer(4001, 3001)
	f.mustPay("ORD-1", 4001, 100000)

	_, err := f.led.OnOrderPaid(context.Background(), distledger.OrderPaidEvent{
		TenantID: tenant, OrderID: "ORD-1", BuyerUserID: 4002,
		PaidAmount: 100000, PaidAt: f.clock.Now(),
	})
	if !errors.Is(err, distledger.ErrInvalidArgument) {
		t.Fatalf("expected ErrInvalidArgument for a buyer mismatch, got %v", err)
	}
}

// TestSelfPurchaseIsNotEligible pins a default that invariant I1 can never
// catch: the books balance perfectly while a agent pays themselves a
// commission on their own order.
func TestSelfPurchaseIsNotEligible(t *testing.T) {
	f := newFixture(t, distledger.Rules{Levels: 1, RateBP: []distledger.Rate{1000}})
	f.mustAgent(4001, 0, distledger.AgentActive)

	// BindBuyer already refuses to bind a buyer to themselves, so that gate is
	// exercised separately below. This test reaches past it to prove the second
	// layer - the eligibility hook - also holds, which is what protects an
	// integration that supplies its own EligibilityChecker.
	ctx := context.Background()
	if _, err := f.led.BindBuyer(ctx, distledger.BindBuyerRequest{
		TenantID: tenant, BuyerUserID: 4001, AgentUserID: 4001, Source: distledger.SourceLink,
	}); !errors.Is(err, distledger.ErrInvalidArgument) {
		t.Fatalf("BindBuyer should refuse self-binding, got %v", err)
	}
	if err := f.store.Update(ctx, func(ctx context.Context, tx distledger.Tx) error {
		_, err := tx.PutBinding(ctx, distledger.Binding{
			Buyer:       distledger.UserKey{TenantID: tenant, UserID: 4001},
			AgentUserID: 4001,
			Source:      distledger.SourceLink,
			BoundAt:     f.clock.Now(),
			Active:      true,
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}

	res := f.mustPay("ORD-1", 4001, 100000)
	if res.Attributed != true {
		t.Fatal("the order is still attributed; only eligibility should fail")
	}
	if len(res.Commissions) != 0 {
		t.Fatalf("self-purchase produced %d commissions", len(res.Commissions))
	}
	if len(res.Skipped) == 0 || res.Skipped[0].Code != distledger.SkipIneligible {
		t.Fatalf("expected SkipIneligible, got %+v", res.Skipped)
	}
}

// TestQuarantineDoesNotBlockHealthySettlement pins the blast radius of a single
// corrupt record. Aborting the batch would let one bad row freeze every
// agent's payout in the tenant.
func TestQuarantineDoesNotBlockHealthySettlement(t *testing.T) {
	f := newFixture(t, distledger.Rules{
		Levels: 1, RateBP: []distledger.Rate{1000}, FreezeDays: 0,
	})
	f.mustAgent(3001, 0, distledger.AgentActive)
	f.mustBuyer(4001, 3001)
	f.mustPay("ORD-1", 4001, 100000)
	f.receive("ORD-1", f.clock.Now())

	// Inject a pending commission with no outstanding amount: unreachable
	// through the public API, so it stands in for a corrupted row.
	ctx := context.Background()
	if err := f.store.Update(ctx, func(ctx context.Context, tx distledger.Tx) error {
		_, err := tx.AppendCommission(ctx, distledger.Commission{
			Key:         distledger.OrderKey{TenantID: tenant, OrderID: "ORD-BAD"},
			IdemKey:     "corrupt-row",
			AgentUserID: 3001,
			Layer:       1,
			BaseAmount:  0,
			Amount:      0,
			State:       distledger.CommissionPending,
			AvailableAt: f.clock.Now().Add(-time.Hour),
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}

	m := f.maintain()
	if m.SettledCount != 1 {
		t.Fatalf("settled %d commissions, want the healthy one to go through", m.SettledCount)
	}
	if len(m.Quarantined) != 1 {
		t.Fatalf("quarantined %v, want exactly one record reported", m.Quarantined)
	}

	// Quarantine is reported, and the corrupt row is visible to the self check.
	if rep := f.selfCheck(); rep.OK {
		t.Fatal("a corrupt row must make the self check fail")
	}
}

// TestMaintainIsolatedPerTenant pins that one tenant's failure does not stop
// the others from settling.
func TestMaintainIsolatedPerTenant(t *testing.T) {
	clock := distledger.NewManualClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	store := memory.New()
	t.Cleanup(func() { _ = store.Close() })
	led := newLedgerOn(t, store, clock, distledger.Rules{
		Levels: 1, RateBP: []distledger.Rate{1000}, FreezeDays: 0,
	})
	defer led.Close()

	ctx := context.Background()
	for _, tn := range []int64{1, 2, 3} {
		if _, err := led.BindAgent(ctx, distledger.BindAgentRequest{
			TenantID: tn, UserID: 3001, Status: distledger.AgentActive,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := led.BindBuyer(ctx, distledger.BindBuyerRequest{
			TenantID: tn, BuyerUserID: 4001, AgentUserID: 3001, Source: distledger.SourceLink,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := led.OnOrderPaid(ctx, distledger.OrderPaidEvent{
			TenantID: tn, OrderID: "ORD-1", BuyerUserID: 4001,
			PaidAmount: 100000, PaidAt: clock.Now(),
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := led.OnOrderReceived(ctx, distledger.OrderReceivedEvent{
			TenantID: tn, OrderID: "ORD-1", ReceivedAt: clock.Now(),
		}); err != nil {
			t.Fatal(err)
		}
	}

	m, err := led.Maintain(ctx)
	if err != nil {
		t.Fatalf("Maintain: %v", err)
	}
	if m.SettledCount != 3 {
		t.Fatalf("settled %d, want 3 (one per tenant)", m.SettledCount)
	}
}

// TestMinInt64StringIsReadable pins the diagnostic path. A corrupted balance is
// exactly when String() gets called, so it must not be the thing that produces
// nonsense.
func TestMinInt64StringIsReadable(t *testing.T) {
	got := distledger.Money(math.MinInt64).String()
	if got != "-92233720368547758.08" {
		t.Fatalf("Money(MinInt64).String() = %q", got)
	}
}

// retryingStore simulates the retry semantics the Store contract allows: the
// update function runs once, its writes are rolled back, and then it runs again.
//
// It exists because that contract was documented but never exercised, which
// meant any engine state leaking across attempts would have gone unnoticed
// until a real database adapter started retrying.
type retryingStore struct {
	inner    distledger.Store
	attempts int32
}

var errSimulatedRetry = errors.New("simulated conflict")

func (s *retryingStore) View(ctx context.Context, fn func(context.Context, distledger.Reader) error) error {
	return s.inner.View(ctx, fn)
}

func (s *retryingStore) Update(ctx context.Context, fn func(context.Context, distledger.Tx) error) error {
	if atomic.AddInt32(&s.attempts, 1) == 1 {
		// First attempt: let the engine do its work, then force a rollback.
		_ = s.inner.Update(ctx, func(ctx context.Context, tx distledger.Tx) error {
			if err := fn(ctx, tx); err != nil {
				return err
			}
			return errSimulatedRetry
		})
	}
	return s.inner.Update(ctx, fn)
}

func (s *retryingStore) Close() error { return s.inner.Close() }

// TestEngineIsSafeUnderTransactionRetry pins the retry contract.
//
// Accrual accumulates into local variables that the store's retry loop cannot
// see, so anything not reset on entry would be counted twice.
func TestEngineIsSafeUnderTransactionRetry(t *testing.T) {
	clock := distledger.NewManualClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	inner := memory.New()
	t.Cleanup(func() { _ = inner.Close() })
	store := &retryingStore{inner: inner}

	led, err := distledger.New(distledger.Config{
		Store: store,
		Clock: clock,
		Rules: distledger.Rules{Levels: 2, RateBP: []distledger.Rate{500, 200}, FreezeDays: 7},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer led.Close()

	ctx := context.Background()
	if _, err := led.BindAgent(ctx, distledger.BindAgentRequest{
		TenantID: tenant, UserID: 3001, Status: distledger.AgentActive,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := led.BindAgent(ctx, distledger.BindAgentRequest{
		TenantID: tenant, UserID: 3002, ParentID: 3001, Status: distledger.AgentActive,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := led.BindBuyer(ctx, distledger.BindBuyerRequest{
		TenantID: tenant, BuyerUserID: 4001, AgentUserID: 3002, Source: distledger.SourceLink,
	}); err != nil {
		t.Fatal(err)
	}

	res, err := led.OnOrderPaid(ctx, distledger.OrderPaidEvent{
		TenantID: tenant, OrderID: "ORD-1", BuyerUserID: 4001,
		PaidAmount: 100000, PaidAt: clock.Now(),
	})
	if err != nil {
		t.Fatalf("OnOrderPaid under retry: %v", err)
	}
	if len(res.Commissions) != 2 {
		t.Fatalf("got %d commissions, want 2 (a retry must not double them)", len(res.Commissions))
	}
	total, err := res.AccruedAmount()
	if err != nil {
		t.Fatal(err)
	}
	if total != 7000 {
		t.Fatalf("accrued %s, want 70.00", total)
	}

	for userID, want := range map[int64]distledger.Money{3002: 5000, 3001: 2000} {
		if bal := f2Balance(t, led, userID); bal.Frozen != want {
			t.Fatalf("agent %d frozen = %s, want %s", userID, bal.Frozen, want)
		}
	}
	if rep, err := led.SelfCheck(ctx, tenant); err != nil || !rep.OK {
		t.Fatalf("invariants violated under retry: %+v, %v", rep.Invariants, err)
	}
}

func f2Balance(t *testing.T, led *distledger.Ledger, userID int64) distledger.Account {
	t.Helper()
	bal, err := led.Balance(context.Background(), tenant, userID)
	if err != nil {
		t.Fatal(err)
	}
	return bal
}

// TestSelfCheckDetectsTamperedTotals pins the check that was missing entirely in
// v0.1: TotalEarned and TotalReversed live outside the four money buckets, so
// invariant I1 never looked at them.
func TestSelfCheckDetectsTamperedTotals(t *testing.T) {
	f := newFixture(t, twoLevelRules())
	f.healthy()
	ctx := context.Background()
	key := distledger.UserKey{TenantID: tenant, UserID: 3001}

	if err := f.store.Update(ctx, func(ctx context.Context, tx distledger.Tx) error {
		acct, err := tx.Account(ctx, key)
		if err != nil {
			return err
		}
		acct.Key = key
		acct.TotalEarned = distledger.Money(-99999999)
		_, err = tx.PutAccount(ctx, acct)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	rep := f.selfCheck()
	if rep.OK {
		t.Fatal("a tampered TotalEarned must be reported")
	}
	if !hasViolation(rep, "I3") {
		t.Fatalf("I3 did not report the tampered total: %+v", rep.Invariants)
	}
}

// TestSelfCheckDetectsReversalAccumulatorDrift pins the reconciliation between
// the reversal accumulator and its detail records.
func TestSelfCheckDetectsReversalAccumulatorDrift(t *testing.T) {
	f := newFixture(t, twoLevelRules())
	f.seededOrders("ORD-1")
	f.refund(distledger.OrderRefundedEvent{OrderID: "ORD-1", Amount: 50000, IdemKey: idem()})
	ctx := context.Background()

	commissions, err := f.led.CommissionsByOrder(ctx, tenant, "ORD-1")
	if err != nil {
		t.Fatal(err)
	}
	var accrualID int64
	for _, c := range commissions {
		if c.Amount > 0 {
			accrualID = c.ID
			break
		}
	}

	// Push the accumulator past what the detail records justify.
	if err := f.store.Update(ctx, func(ctx context.Context, tx distledger.Tx) error {
		cur, err := tx.Commission(ctx, accrualID)
		if err != nil {
			return err
		}
		_, err = tx.SetCommissionReversedAmount(ctx, cur.ID, cur.Amount, cur.Version)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	rep := f.selfCheck()
	if rep.OK {
		t.Fatal("accumulator drift must be reported")
	}
	if !hasViolation(rep, "I3") {
		t.Fatalf("I3 did not report the drift: %+v", rep.Invariants)
	}
}

// TestSetCommissionReversedAmountRejectsOutOfRange pins the store-level bound.
func TestSetCommissionReversedAmountRejectsOutOfRange(t *testing.T) {
	f := newFixture(t, distledger.Rules{Levels: 1, RateBP: []distledger.Rate{1000}})
	f.mustAgent(3001, 0, distledger.AgentActive)
	f.mustBuyer(4001, 3001)
	f.mustPay("ORD-1", 4001, 100000)

	commissions, err := f.led.CommissionsByOrder(context.Background(), tenant, "ORD-1")
	if err != nil {
		t.Fatal(err)
	}
	id := commissions[0].ID

	err = f.store.Update(context.Background(), func(ctx context.Context, tx distledger.Tx) error {
		_, err := tx.SetCommissionReversedAmount(ctx, id, commissions[0].Amount+1, commissions[0].Version)
		return err
	})
	if !errors.Is(err, distledger.ErrInvalidArgument) {
		t.Fatalf("expected ErrInvalidArgument, got %v", err)
	}
	err = f.store.Update(context.Background(), func(ctx context.Context, tx distledger.Tx) error {
		_, err := tx.SetCommissionReversedAmount(ctx, id, -1, commissions[0].Version)
		return err
	})
	if !errors.Is(err, distledger.ErrInvalidArgument) {
		t.Fatalf("expected ErrInvalidArgument for a negative accumulator, got %v", err)
	}
}
