package distledger_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/im10furry/distledger"
)

// This file covers defensive branches on the refund and risk-control paths that
// the behavior-driven tests reach only indirectly. An uncovered guard is
// indistinguishable from a guard that does not work, so each is exercised at
// least once.

// TestRiskControlRejectsBadIdentifiers covers the scoping guard on the operator
// API. Commission ids are global, so the tenant check is the only thing standing
// between an operator endpoint and a cross-tenant write.
//
// The validation lives inside the transaction callback rather than in the
// facade, so this test needs a real store: a stub that skips the callback would
// report success for every input and prove nothing.
func TestRiskControlRejectsBadIdentifiers(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, distledger.Rules{Levels: 1, RateBP: []distledger.Rate{1000}})

	cases := map[string]struct {
		tenantID int64
		id       int64
		want     error
	}{
		"negative tenant":    {tenantID: -1, id: 1, want: distledger.ErrInvalidArgument},
		"zero commission":    {tenantID: 0, id: 0, want: distledger.ErrInvalidArgument},
		"missing commission": {tenantID: 0, id: 9999, want: distledger.ErrNotFound},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := f.led.FreezeCommission(ctx, tc.tenantID, tc.id, "x"); !errors.Is(err, tc.want) {
				t.Errorf("FreezeCommission = %v, want %v", err, tc.want)
			}
			if _, err := f.led.UnfreezeCommission(ctx, tc.tenantID, tc.id, "x"); !errors.Is(err, tc.want) {
				t.Errorf("UnfreezeCommission = %v, want %v", err, tc.want)
			}
			if _, err := f.led.VoidCommission(ctx, tc.tenantID, tc.id, "x"); !errors.Is(err, tc.want) {
				t.Errorf("VoidCommission = %v, want %v", err, tc.want)
			}
		})
	}
}

// TestRiskControlRejectsOverLongReason pins the bound on the reason string,
// which is copied into the ledger remark.
func TestRiskControlRejectsOverLongReason(t *testing.T) {
	f := newFixture(t, distledger.Rules{Levels: 1, RateBP: []distledger.Rate{1000}})
	f.mustAgent(3001, 0, distledger.AgentActive)
	f.mustBuyer(4001, 3001)
	f.mustPay("ORD-1", 4001, 100000)

	commissions, err := f.led.CommissionsByOrder(context.Background(), tenant, "ORD-1")
	if err != nil {
		t.Fatal(err)
	}
	long := strings.Repeat("x", 200)
	if _, err := f.led.FreezeCommission(context.Background(), tenant, commissions[0].ID, long); !errors.Is(err, distledger.ErrInvalidArgument) {
		t.Fatalf("over-long reason = %v, want ErrInvalidArgument", err)
	}
}

// TestClawbackSkipsPaidOutCommissions pins the branch that becomes reachable
// once withdrawals exist. Money that has left the platform cannot be clawed back
// without a debt policy, so the commission is reported as skipped rather than
// silently ignored or, worse, pushed into a negative balance.
func TestClawbackSkipsPaidOutCommissions(t *testing.T) {
	f := newFixture(t, distledger.Rules{Levels: 1, RateBP: []distledger.Rate{1000}, FreezeDays: 0})
	f.mustAgent(3001, 0, distledger.AgentActive)
	f.mustBuyer(4001, 3001)
	f.mustPay("ORD-1", 4001, 100000)

	commissions, err := f.led.CommissionsByOrder(context.Background(), tenant, "ORD-1")
	if err != nil {
		t.Fatal(err)
	}
	id := commissions[0].ID

	// Drive the commission to Withdrawn through the store: no public API reaches
	// that state until withdrawals land in v0.5, but the clawback must already
	// handle it rather than corrupt an account.
	ctx := context.Background()
	if err := f.store.Update(ctx, func(ctx context.Context, tx distledger.Tx) error {
		cur, err := tx.Commission(ctx, id)
		if err != nil {
			return err
		}
		cur, err = tx.TransitionCommission(ctx, id, distledger.CommissionPending, distledger.CommissionSettled, cur.Version)
		if err != nil {
			return err
		}
		_, err = tx.TransitionCommission(ctx, id, distledger.CommissionSettled, distledger.CommissionWithdrawn, cur.Version)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	res := f.refund(distledger.OrderRefundedEvent{OrderID: "ORD-1", IsFull: true, IdemKey: idem()})
	if res.ReversedAmount != 0 {
		t.Fatalf("clawed back %s from a commission that was already paid out", res.ReversedAmount)
	}
	var sawWithdrawn bool
	for _, s := range res.Skipped {
		if s.Code == distledger.SkipAlreadyWithdrawn {
			sawWithdrawn = true
		}
	}
	if !sawWithdrawn {
		t.Fatalf("expected distledger.SkipAlreadyWithdrawn, got %+v", res.Skipped)
	}
}

// TestRiskControlStateGuards covers the transition guards on the operator API.
func TestRiskControlStateGuards(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, distledger.Rules{Levels: 1, RateBP: []distledger.Rate{1000}})
	f.mustAgent(3001, 0, distledger.AgentActive)
	f.mustBuyer(4001, 3001)
	f.mustPay("ORD-1", 4001, 100000)

	commissions, err := f.led.CommissionsByOrder(ctx, tenant, "ORD-1")
	if err != nil {
		t.Fatal(err)
	}
	id := commissions[0].ID

	// Unfreezing something that is not frozen is a conflict, not a silent no-op.
	if _, err := f.led.UnfreezeCommission(ctx, tenant, id, "not frozen"); !errors.Is(err, distledger.ErrIllegalTransition) {
		t.Fatalf("unfreeze of a pending commission = %v, want distledger.ErrIllegalTransition", err)
	}

	// A settled commission is not voidable: it must go through the refund path
	// so the money is accounted for as a clawback, not as a never-earned accrual.
	if err := f.store.Update(ctx, func(ctx context.Context, tx distledger.Tx) error {
		cur, err := tx.Commission(ctx, id)
		if err != nil {
			return err
		}
		_, err = tx.TransitionCommission(ctx, id, distledger.CommissionPending, distledger.CommissionSettled, cur.Version)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.led.VoidCommission(ctx, tenant, id, "settled"); !errors.Is(err, distledger.ErrIllegalTransition) {
		t.Fatalf("void of a settled commission = %v, want distledger.ErrIllegalTransition", err)
	}

	// Freezing a settled commission is likewise rejected.
	if _, err := f.led.FreezeCommission(ctx, tenant, id, "settled"); !errors.Is(err, distledger.ErrIllegalTransition) {
		t.Fatalf("freeze of a settled commission = %v, want distledger.ErrIllegalTransition", err)
	}
}

// TestVoidOnAlreadyReversedCommissionIsANoOp covers the branch where a forfeiture
// has nothing left to take.
func TestVoidOnAlreadyReversedCommissionIsANoOp(t *testing.T) {
	f := newFixture(t, distledger.Rules{Levels: 1, RateBP: []distledger.Rate{1000}})
	f.mustAgent(3001, 0, distledger.AgentActive)
	f.mustBuyer(4001, 3001)
	f.mustPay("ORD-1", 4001, 100000)

	commissions, err := f.led.CommissionsByOrder(context.Background(), tenant, "ORD-1")
	if err != nil {
		t.Fatal(err)
	}
	id := commissions[0].ID

	if _, err := f.led.VoidCommission(context.Background(), tenant, id, "first"); err != nil {
		t.Fatal(err)
	}
	// Second attempt: the commission is already terminal, so this is refused by
	// the state guard rather than writing a zero-amount record.
	if _, err := f.led.VoidCommission(context.Background(), tenant, id, "second"); !errors.Is(err, distledger.ErrIllegalTransition) {
		t.Fatalf("second void = %v, want distledger.ErrIllegalTransition", err)
	}
	if rep := f.selfCheck(); !rep.OK {
		t.Fatalf("invariants violated: %+v", rep.Invariants)
	}
}

// TestClawbackFailsLoudlyWhenTheBucketIsShort pins the safety net for v0.5.
//
// With no withdrawals yet, the money for a settled commission is always still in
// the bucket, so this path is unreachable through the public API. It exists for
// the day a commission can be paid out before its order is refunded - and the
// behavior that matters is that it fails loudly instead of pushing the bucket
// negative, which would silently invalidate invariant I1.
func TestClawbackFailsLoudlyWhenTheBucketIsShort(t *testing.T) {
	f := newFixture(t, distledger.Rules{
		Levels: 1, RateBP: []distledger.Rate{1000}, FreezeDays: 0,
	})
	f.mustAgent(3001, 0, distledger.AgentActive)
	f.mustBuyer(4001, 3001)
	f.mustPay("ORD-1", 4001, 100000)
	f.receive("ORD-1", f.clock.Now())
	f.maintain()

	ctx := context.Background()
	key := distledger.UserKey{TenantID: tenant, UserID: 3001}
	if err := f.store.Update(ctx, func(ctx context.Context, tx distledger.Tx) error {
		acct, err := tx.Account(ctx, key)
		if err != nil {
			return err
		}
		acct.Key = key
		acct.Available = 0
		_, err = tx.PutAccount(ctx, acct)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	_, err := f.led.OnOrderRefunded(ctx, distledger.OrderRefundedEvent{
		TenantID: tenant, OrderID: "ORD-1", IsFull: true,
		IdemKey: idem(), RefundedAt: f.clock.Now(),
	})
	if !errors.Is(err, distledger.ErrInsufficientBalance) {
		t.Fatalf("clawback with an empty bucket = %v, want ErrInsufficientBalance", err)
	}

	// The failed refund must have rolled back completely: no voucher, no
	// reversal record, no negative balance.
	if bal := f.balance(3001); bal.Available < 0 {
		t.Fatalf("available went negative: %s", bal.Available)
	}
	refunds, err := f.led.CommissionsByOrder(ctx, tenant, "ORD-1")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range refunds {
		if c.ReverseOf != 0 {
			t.Fatalf("a reversal record survived the rolled-back refund: %+v", c)
		}
	}
}
