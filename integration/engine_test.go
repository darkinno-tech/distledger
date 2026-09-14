package integration

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/darkinno-tech/distledger"
)

// The engine suite.
//
// The contract suite above checks the port method by method. This one builds a
// real Ledger on top of each backend and runs the whole flow through it, ending
// with a self check. That is the only way to know the engine works on a real
// database rather than only through the in-memory store it was developed
// against: accrual, freeze, settlement, clawback and reconciliation all have to
// agree about what a transaction is.

const engineTenant = int64(1)

func newEngine(t *testing.T, b backend) (*distledger.Ledger, *distledger.ManualClock) {
	t.Helper()
	clock := distledger.NewManualClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	led, err := distledger.New(distledger.Config{
		Store: b.open(t),
		Clock: clock,
		Rules: distledger.Rules{
			Levels:     3,
			RateBP:     []distledger.Rate{500, 200, 100},
			FreezeDays: 7,
		},
	})
	if err != nil {
		t.Fatalf("new ledger: %v", err)
	}
	t.Cleanup(func() { _ = led.Close() })
	return led, clock
}

// TestEngineEndToEnd runs the full lifecycle on each backend.
func TestEngineEndToEnd(t *testing.T) {
	for _, b := range backends(t) {
		t.Run(b.name, func(t *testing.T) {
			ctx := context.Background()
			led, clock := newEngine(t, b)

			// A three-level chain: 3001 <- 3002 <- 3003.
			bind := func(userID, parentID int64) {
				t.Helper()
				if _, err := led.BindAgent(ctx, distledger.BindAgentRequest{
					TenantID: engineTenant, UserID: userID, ParentID: parentID,
					Status: distledger.AgentActive,
				}); err != nil {
					t.Fatalf("BindAgent(%d): %v", userID, err)
				}
			}
			bind(3001, 0)
			bind(3002, 3001)
			bind(3003, 3002)
			if _, err := led.BindBuyer(ctx, distledger.BindBuyerRequest{
				TenantID: engineTenant, BuyerUserID: 4001, AgentUserID: 3003,
				Source: distledger.SourceLink,
			}); err != nil {
				t.Fatalf("BindBuyer: %v", err)
			}

			// 1000.00 paid: 50.00 / 20.00 / 10.00 across the three levels.
			paidAt := clock.Now()
			res, err := led.OnOrderPaid(ctx, distledger.OrderPaidEvent{
				TenantID: engineTenant, OrderID: "ORD-1", BuyerUserID: 4001,
				PaidAmount: 100000, PaidAt: paidAt,
			})
			if err != nil {
				t.Fatalf("OnOrderPaid: %v", err)
			}
			if len(res.Commissions) != 3 {
				t.Fatalf("got %d commissions, want 3", len(res.Commissions))
			}
			want := map[int64]distledger.Money{3003: 5000, 3002: 2000, 3001: 1000}
			for _, c := range res.Commissions {
				if want[c.AgentUserID] != c.Amount {
					t.Errorf("agent %d got %s, want %s", c.AgentUserID, c.Amount, want[c.AgentUserID])
				}
			}

			// Redelivery must be free.
			again, err := led.OnOrderPaid(ctx, distledger.OrderPaidEvent{
				TenantID: engineTenant, OrderID: "ORD-1", BuyerUserID: 4001,
				PaidAmount: 100000, PaidAt: paidAt,
			})
			if err != nil {
				t.Fatalf("redelivery: %v", err)
			}
			if !again.Replayed || len(again.Commissions) != 0 {
				t.Fatalf("redelivery produced %d commissions (replayed=%v)", len(again.Commissions), again.Replayed)
			}

			// Receipt, then past the freeze window.
			if _, err := led.OnOrderReceived(ctx, distledger.OrderReceivedEvent{
				TenantID: engineTenant, OrderID: "ORD-1", ReceivedAt: clock.Now(),
			}); err != nil {
				t.Fatalf("OnOrderReceived: %v", err)
			}
			clock.Advance(8 * 24 * time.Hour)
			m, err := led.Maintain(ctx)
			if err != nil {
				t.Fatalf("Maintain: %v", err)
			}
			if m.SettledCount != 3 {
				t.Fatalf("settled %d, want 3", m.SettledCount)
			}
			if m.SettledAmount != 8000 {
				t.Fatalf("settled %s, want 80.00", m.SettledAmount)
			}

			for userID, amount := range want {
				bal, err := led.Balance(ctx, engineTenant, userID)
				if err != nil {
					t.Fatalf("Balance(%d): %v", userID, err)
				}
				if bal.Frozen != 0 || bal.Available != amount {
					t.Errorf("agent %d: frozen=%s available=%s, want 0/%s",
						userID, bal.Frozen, bal.Available, amount)
				}
			}

			// A partial refund of 40% claws back 40% of every level.
			refund, err := led.OnOrderRefunded(ctx, distledger.OrderRefundedEvent{
				TenantID: engineTenant, OrderID: "ORD-1",
				Amount: 40000, IdemKey: "refund-1", RefundedAt: clock.Now(),
			})
			if err != nil {
				t.Fatalf("OnOrderRefunded: %v", err)
			}
			if refund.ReversedAmount != 3200 {
				t.Fatalf("reversed %s, want 32.00", refund.ReversedAmount)
			}
			// The same refund redelivered is a no-op.
			replay, err := led.OnOrderRefunded(ctx, distledger.OrderRefundedEvent{
				TenantID: engineTenant, OrderID: "ORD-1",
				Amount: 40000, IdemKey: "refund-1", RefundedAt: clock.Now(),
			})
			if err != nil {
				t.Fatalf("refund redelivery: %v", err)
			}
			if !replay.AlreadyReversed {
				t.Fatalf("refund redelivery reversed another %s", replay.ReversedAmount)
			}

			// A full refund clears the rest.
			if _, err := led.OnOrderRefunded(ctx, distledger.OrderRefundedEvent{
				TenantID: engineTenant, OrderID: "ORD-1",
				IsFull: true, IdemKey: "refund-2", RefundedAt: clock.Now(),
			}); err != nil {
				t.Fatalf("full refund: %v", err)
			}
			for userID := range want {
				bal, err := led.Balance(ctx, engineTenant, userID)
				if err != nil {
					t.Fatal(err)
				}
				if bal.Available != 0 || bal.Frozen != 0 {
					t.Errorf("agent %d still holds frozen=%s available=%s after a full refund",
						userID, bal.Frozen, bal.Available)
				}
				if bal.TotalEarned != bal.TotalReversed {
					t.Errorf("agent %d gross %s != reversed %s", userID, bal.TotalEarned, bal.TotalReversed)
				}
			}

			assertSelfCheckOK(t, ctx, led, engineTenant)
		})
	}
}

// TestEngineRiskControl exercises the states that only the operator API reaches.
func TestEngineRiskControl(t *testing.T) {
	for _, b := range backends(t) {
		t.Run(b.name, func(t *testing.T) {
			ctx := context.Background()
			led, clock := newEngine(t, b)

			if _, err := led.BindAgent(ctx, distledger.BindAgentRequest{
				TenantID: engineTenant, UserID: 3001, Status: distledger.AgentActive,
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := led.BindBuyer(ctx, distledger.BindBuyerRequest{
				TenantID: engineTenant, BuyerUserID: 4001, AgentUserID: 3001,
				Source: distledger.SourceLink,
			}); err != nil {
				t.Fatal(err)
			}
			res, err := led.OnOrderPaid(ctx, distledger.OrderPaidEvent{
				TenantID: engineTenant, OrderID: "ORD-1", BuyerUserID: 4001,
				PaidAmount: 100000, PaidAt: clock.Now(),
			})
			if err != nil {
				t.Fatal(err)
			}
			id := res.Commissions[0].ID

			// Freeze, then confirm the frozen commission does not settle.
			if _, err := led.FreezeCommission(ctx, engineTenant, id, "suspected fraud"); err != nil {
				t.Fatalf("FreezeCommission: %v", err)
			}
			if _, err := led.OnOrderReceived(ctx, distledger.OrderReceivedEvent{
				TenantID: engineTenant, OrderID: "ORD-1", ReceivedAt: clock.Now(),
			}); err != nil {
				t.Fatal(err)
			}
			clock.Advance(30 * 24 * time.Hour)
			if m, err := led.Maintain(ctx); err != nil {
				t.Fatal(err)
			} else if m.SettledCount != 0 {
				t.Fatalf("a frozen commission settled (%d)", m.SettledCount)
			}

			// Unfreeze: it must settle on the next tick without anyone
			// re-delivering the receipt.
			if _, err := led.UnfreezeCommission(ctx, engineTenant, id, "appeal accepted"); err != nil {
				t.Fatalf("UnfreezeCommission: %v", err)
			}
			if m, err := led.Maintain(ctx); err != nil {
				t.Fatal(err)
			} else if m.SettledCount != 1 {
				t.Fatalf("unfrozen commission did not settle (%d)", m.SettledCount)
			}

			assertSelfCheckOK(t, ctx, led, engineTenant)
		})
	}
}

// TestEngineSelfCheckCatchesCorruption confirms the reconciliation actually
// reads the database rather than trusting it.
//
// The corruption is injected through the port, since the engine has no way to
// produce it: an account whose balance has no ledger entry behind it.
func TestEngineSelfCheckCatchesCorruption(t *testing.T) {
	for _, b := range backends(t) {
		t.Run(b.name, func(t *testing.T) {
			ctx := context.Background()
			led, clock := newEngine(t, b)

			if _, err := led.BindAgent(ctx, distledger.BindAgentRequest{
				TenantID: engineTenant, UserID: 3001, Status: distledger.AgentActive,
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := led.BindBuyer(ctx, distledger.BindBuyerRequest{
				TenantID: engineTenant, BuyerUserID: 4001, AgentUserID: 3001,
				Source: distledger.SourceLink,
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := led.OnOrderPaid(ctx, distledger.OrderPaidEvent{
				TenantID: engineTenant, OrderID: "ORD-1", BuyerUserID: 4001,
				PaidAmount: 100000, PaidAt: clock.Now(),
			}); err != nil {
				t.Fatal(err)
			}
			assertSelfCheckOK(t, ctx, led, engineTenant)

			// Write an account with a balance but no ledger entry behind it.
			store := b.open(t)
			if err := store.Update(ctx, func(ctx context.Context, tx distledger.Tx) error {
				_, err := tx.PutAccount(ctx, distledger.Account{
					Key:       distledger.UserKey{TenantID: engineTenant, UserID: 7777},
					Available: 123456,
				})
				return err
			}); err != nil {
				t.Fatalf("inject corruption: %v", err)
			}
			led2, _ := newEngineOn(t, store, clock)
			rep, err := led2.SelfCheck(ctx, engineTenant)
			if err != nil {
				t.Fatalf("SelfCheck: %v", err)
			}
			if rep.OK {
				t.Fatal("an account with a balance and no ledger entries was not reported")
			}
		})
	}
}

// retryingStore simulates the retry semantics the store contract permits, on top
// of a real database.
//
// The engine's safety depends on a retry rolling the failed attempt back first
// (ADR-030). The in-memory store never conflicts, so that contract had never been
// exercised against a backend that actually retries - this is that exercise.
type retryingStore struct {
	inner    distledger.Store
	attempts atomic.Int64
}

func (s *retryingStore) View(ctx context.Context, fn func(context.Context, distledger.Reader) error) error {
	return s.inner.View(ctx, fn)
}

func (s *retryingStore) Update(ctx context.Context, fn func(context.Context, distledger.Tx) error) error {
	if s.attempts.Add(1) == 1 {
		// Run the work, then force a rollback, exactly as a serialization
		// failure would.
		_ = s.inner.Update(ctx, func(ctx context.Context, tx distledger.Tx) error {
			if err := fn(ctx, tx); err != nil {
				return err
			}
			return errors.New("simulated conflict")
		})
	}
	return s.inner.Update(ctx, fn)
}

func (s *retryingStore) Close() error { return s.inner.Close() }

func TestEngineSurvivesTransactionRetry(t *testing.T) {
	for _, b := range backends(t) {
		t.Run(b.name, func(t *testing.T) {
			ctx := context.Background()
			clock := distledger.NewManualClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
			store := &retryingStore{inner: b.open(t)}
			led, err := distledger.New(distledger.Config{
				Store: store,
				Clock: clock,
				Rules: distledger.Rules{Levels: 2, RateBP: []distledger.Rate{500, 200}, FreezeDays: 7},
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = led.Close() })

			if _, err := led.BindAgent(ctx, distledger.BindAgentRequest{
				TenantID: engineTenant, UserID: 3001, Status: distledger.AgentActive,
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := led.BindAgent(ctx, distledger.BindAgentRequest{
				TenantID: engineTenant, UserID: 3002, ParentID: 3001, Status: distledger.AgentActive,
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := led.BindBuyer(ctx, distledger.BindBuyerRequest{
				TenantID: engineTenant, BuyerUserID: 4001, AgentUserID: 3002,
				Source: distledger.SourceLink,
			}); err != nil {
				t.Fatal(err)
			}

			res, err := led.OnOrderPaid(ctx, distledger.OrderPaidEvent{
				TenantID: engineTenant, OrderID: "ORD-1", BuyerUserID: 4001,
				PaidAmount: 100000, PaidAt: clock.Now(),
			})
			if err != nil {
				t.Fatalf("OnOrderPaid under retry: %v", err)
			}
			// A retried transaction must not double the accrual: the first
			// attempt's writes have to disappear completely.
			if len(res.Commissions) != 2 {
				t.Fatalf("got %d commissions, want 2", len(res.Commissions))
			}
			total, err := res.AccruedAmount()
			if err != nil {
				t.Fatal(err)
			}
			if total != 7000 {
				t.Fatalf("accrued %s, want 70.00", total)
			}

			for userID, want := range map[int64]distledger.Money{3002: 5000, 3001: 2000} {
				bal, err := led.Balance(ctx, engineTenant, userID)
				if err != nil {
					t.Fatal(err)
				}
				if bal.Frozen != want {
					t.Fatalf("agent %d frozen = %s, want %s", userID, bal.Frozen, want)
				}
			}
			assertSelfCheckOK(t, ctx, led, engineTenant)
		})
	}
}

// TestEngineConcurrentAccrualIsIdempotent runs the same payload from many
// goroutines at once.
//
// This is the case a real database makes interesting: the in-memory store
// serialises everything behind one lock, whereas here the duplicate insert races
// a unique index and only one of them may win.
func TestEngineConcurrentAccrualIsIdempotent(t *testing.T) {
	for _, b := range backends(t) {
		t.Run(b.name, func(t *testing.T) {
			ctx := context.Background()
			led, clock := newEngine(t, b)

			if _, err := led.BindAgent(ctx, distledger.BindAgentRequest{
				TenantID: engineTenant, UserID: 3001, Status: distledger.AgentActive,
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := led.BindBuyer(ctx, distledger.BindBuyerRequest{
				TenantID: engineTenant, BuyerUserID: 4001, AgentUserID: 3001,
				Source: distledger.SourceLink,
			}); err != nil {
				t.Fatal(err)
			}

			const workers = 12
			paidAt := clock.Now()
			var (
				created int64
				done    = make(chan struct{}, workers)
			)
			for i := 0; i < workers; i++ {
				go func() {
					defer func() { done <- struct{}{} }()
					res, err := led.OnOrderPaid(ctx, distledger.OrderPaidEvent{
						TenantID: engineTenant, OrderID: "ORD-1", BuyerUserID: 4001,
						PaidAmount: 100000, PaidAt: paidAt,
					})
					if err != nil {
						// A conflict is a legitimate outcome here; the engine
						// surfaces it rather than swallowing it.
						return
					}
					atomic.AddInt64(&created, int64(len(res.Commissions)))
				}()
			}
			for i := 0; i < workers; i++ {
				<-done
			}

			if created != 1 {
				t.Fatalf("%d workers created %d commissions, want exactly 1", workers, created)
			}
			bal, err := led.Balance(ctx, engineTenant, 3001)
			if err != nil {
				t.Fatal(err)
			}
			if bal.Frozen != 5000 {
				t.Fatalf("frozen = %s, want 50.00 exactly once", bal.Frozen)
			}
			assertSelfCheckOK(t, ctx, led, engineTenant)
		})
	}
}

func newEngineOn(t *testing.T, s distledger.Store, clock distledger.Clock) (*distledger.Ledger, error) {
	t.Helper()
	led, err := distledger.New(distledger.Config{
		Store: s,
		Clock: clock,
		Rules: distledger.Rules{Levels: 3, RateBP: []distledger.Rate{500, 200, 100}, FreezeDays: 7},
	})
	if err != nil {
		return nil, err
	}
	t.Cleanup(func() { _ = led.Close() })
	return led, nil
}

func assertSelfCheckOK(t *testing.T, ctx context.Context, led *distledger.Ledger, tenantID int64) {
	t.Helper()
	rep, err := led.SelfCheck(ctx, tenantID)
	if err != nil {
		t.Fatalf("SelfCheck: %v", err)
	}
	if !rep.OK {
		for _, inv := range rep.Invariants {
			if !inv.OK {
				t.Errorf("invariant %q failed: %v", inv.Name, inv.Violations)
			}
		}
		t.Fatalf("self check failed (store=%s)", rep.StoreKind)
	}
	// A pass that checked nothing proves nothing.
	for _, inv := range rep.Invariants {
		if inv.Checked == 0 {
			t.Errorf("invariant %q checked nothing", inv.Name)
		}
	}
}

var _ = fmt.Sprintf

// TestEngineConcurrentOrdersToOneAgent is the reason account movements became
// atomic increments.
//
// Every order in this test is attributed to the SAME agent, so they all contend
// on one account row. Under the read-modify-write form that preceded the delta
// API, each transaction read the same balances and all but one lost the version
// check, so the caller saw conflicts and had to retry whole events.
//
// With an increment the database serialises them on the row lock: every order
// succeeds, nothing conflicts, and the final balance is exactly the sum. The
// assertion is therefore not just "no error" but "the arithmetic is exact",
// because a lost update would show up as a shortfall rather than as a failure.
func TestEngineConcurrentOrdersToOneAgent(t *testing.T) {
	for _, b := range backends(t) {
		t.Run(b.name, func(t *testing.T) {
			ctx := context.Background()
			led, clock := newEngine(t, b)

			if _, err := led.BindAgent(ctx, distledger.BindAgentRequest{
				TenantID: engineTenant, UserID: 3001, Status: distledger.AgentActive,
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := led.BindBuyer(ctx, distledger.BindBuyerRequest{
				TenantID: engineTenant, BuyerUserID: 4001, AgentUserID: 3001,
				Source: distledger.SourceLink,
			}); err != nil {
				t.Fatal(err)
			}

			const orders = 12
			paidAt := clock.Now()
			var (
				accrued int64
				failed  int64
			)
			var (
				mu       sync.Mutex
				firstErr error
			)
			done := make(chan struct{}, orders)
			for i := 0; i < orders; i++ {
				go func(n int) {
					defer func() { done <- struct{}{} }()
					res, err := led.OnOrderPaid(ctx, distledger.OrderPaidEvent{
						TenantID:    engineTenant,
						OrderID:     fmt.Sprintf("ORD-%02d", n),
						BuyerUserID: 4001,
						PaidAmount:  10000, // 5% at level 1 -> 500 per order
						PaidAt:      paidAt,
					})
					if err != nil {
						atomic.AddInt64(&failed, 1)
						mu.Lock()
						if firstErr == nil {
							firstErr = err
						}
						mu.Unlock()
						return
					}
					atomic.AddInt64(&accrued, int64(len(res.Commissions)))
				}(i)
			}
			for i := 0; i < orders; i++ {
				<-done
			}

			if failed != 0 {
				mu.Lock()
				reason := firstErr
				mu.Unlock()
				t.Fatalf("%d of %d orders failed; concurrent writers must not conflict; first error: %v",
					failed, orders, reason)
			}
			if accrued != orders {
				t.Fatalf("%d commissions accrued, want %d", accrued, orders)
			}

			bal, err := led.Balance(ctx, engineTenant, 3001)
			if err != nil {
				t.Fatal(err)
			}
			want := distledger.Money(orders * 500)
			if bal.Frozen != want {
				t.Fatalf("frozen = %s, want %s: a concurrent increment was lost",
					bal.Frozen, want)
			}
			assertSelfCheckOK(t, ctx, led, engineTenant)
		})
	}
}

// newEngineAndStore is newEngine plus the store, for tests that need to reach
// behind the engine to construct a state the engine would never produce.
func newEngineAndStore(t *testing.T, b backend) (*distledger.Ledger, distledger.Store, *distledger.ManualClock) {
	t.Helper()
	store := b.open(t)
	clock := distledger.NewManualClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	led, err := distledger.New(distledger.Config{
		Store: store,
		Clock: clock,
		Rules: distledger.Rules{
			Levels:     3,
			RateBP:     []distledger.Rate{500, 200, 100},
			FreezeDays: 7,
		},
	})
	if err != nil {
		t.Fatalf("new ledger: %v", err)
	}
	t.Cleanup(func() { _ = led.Close() })
	return led, store, clock
}

// TestSelfCheckReconciliationAgreesWithTheRowWiseCheck is the test that makes the
// set-based reconciliation trustworthy.
//
// A reconciling store is only worth having if it agrees with the row-wise check
// about what is broken. Checking it on a healthy ledger would prove only that it
// produces no false positives, which a query returning nothing unconditionally
// would pass too.
//
// So the same corruption is injected on every backend, and each must report the
// same invariant failure. The corruption is written through the store rather than
// the engine, because the engine would never produce it: that is the point of the
// invariant. The account row is changed and the ledger is left alone.
func TestSelfCheckReconciliationAgreesWithTheRowWiseCheck(t *testing.T) {
	for _, b := range backends(t) {
		t.Run(b.name, func(t *testing.T) {
			ctx := context.Background()
			led, store, clock := newEngineAndStore(t, b)

			if _, err := led.BindAgent(ctx, distledger.BindAgentRequest{
				TenantID: engineTenant, UserID: 3001, Status: distledger.AgentActive,
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := led.BindBuyer(ctx, distledger.BindBuyerRequest{
				TenantID: engineTenant, BuyerUserID: 4001, AgentUserID: 3001,
				Source: distledger.SourceLink,
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := led.OnOrderPaid(ctx, distledger.OrderPaidEvent{
				TenantID: engineTenant, OrderID: "ORD-1", BuyerUserID: 4001,
				PaidAmount: 100000, PaidAt: clock.Now(),
			}); err != nil {
				t.Fatal(err)
			}

			// A healthy ledger must reconcile cleanly, and the check must
			// actually have inspected something.
			before, err := led.SelfCheck(ctx, engineTenant)
			if err != nil {
				t.Fatal(err)
			}
			if !before.OK {
				t.Fatalf("a healthy ledger must pass self-check: %+v", before.Invariants)
			}
			if before.Reconciled != b.reconciles {
				t.Fatalf("Reconciled = %v, want %v: the %s store %s take the set-based path",
					before.Reconciled, b.reconciles, b.name,
					map[bool]string{true: "must", false: "must not"}[b.reconciles])
			}
			i1, ok := findInvariant(before, "I1")
			if !ok {
				t.Fatalf("no I1 result in %+v", before.Invariants)
			}
			if i1.Checked == 0 {
				t.Fatal("I1 checked nothing, so passing means nothing")
			}

			// Change the account row without writing a ledger entry: "someone
			// edited a balance and forgot the trail".
			key := distledger.UserKey{TenantID: engineTenant, UserID: 3001}
			if err := store.Update(ctx, func(ctx context.Context, tx distledger.Tx) error {
				acct, err := tx.Account(ctx, key)
				if err != nil {
					return err
				}
				acct.Key = key
				acct.Available += 12345
				_, err = tx.PutAccount(ctx, acct)
				return err
			}); err != nil {
				t.Fatal(err)
			}

			after, err := led.SelfCheck(ctx, engineTenant)
			if err != nil {
				t.Fatal(err)
			}
			if after.OK {
				t.Fatal("self-check passed after the account row was changed behind the ledger's back")
			}
			i1, ok = findInvariant(after, "I1")
			if !ok {
				t.Fatalf("no I1 result in %+v", after.Invariants)
			}
			if i1.OK {
				t.Fatalf("I1 passed despite the corruption: %+v", i1)
			}
			if len(i1.Violations) == 0 {
				t.Fatalf("I1 failed but recorded no violation, so it cannot be acted on: %+v", i1)
			}
			// The violation must name the account, or an operator cannot find it.
			if !strings.Contains(i1.Violations[0], "u3001") {
				t.Fatalf("violation %q does not name the offending account", i1.Violations[0])
			}
		})
	}
}

// findInvariant returns the result whose name starts with prefix.
func findInvariant(rep distledger.Report, prefix string) (distledger.InvariantResult, bool) {
	for _, inv := range rep.Invariants {
		if strings.HasPrefix(inv.Name, prefix) {
			return inv, true
		}
	}
	return distledger.InvariantResult{}, false
}
