package distledger_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/darkinno-tech/distledger"
)

// cashOutFixture settles a commission and then withdraws the whole balance, so
// that a refund has nothing left to claw back.
//
// This is the state withdrawals make reachable for the first time. Before them,
// a refund always found the money still sitting in a bucket, and the
// insufficient-balance path in the clawback was documented as a safety net that
// nothing could reach. Now it is ordinary.
func cashOutFixture(t *testing.T, rules distledger.Rules, ch distledger.PayoutChannel) (*fixture, distledger.Money) {
	t.Helper()
	ctx := context.Background()
	var f *fixture
	if ch != nil {
		f = newPayoutFixture(t, rules, ch)
	} else {
		f = withdrawalFixture(t, rules)
	}

	bal := balance(t, f, 3001)
	amount := bal.Available
	if amount <= 0 {
		t.Fatalf("fixture produced nothing to withdraw: %+v", bal)
	}

	w, _, err := f.led.RequestWithdraw(ctx, distledger.WithdrawRequest{
		TenantID: tenant, UserID: 3001, Amount: amount, IdemKey: "cash-out",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.led.ApproveWithdraw(ctx, distledger.WithdrawDecision{
		TenantID: tenant, WithdrawalID: w.ID, Operator: "alice",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.led.MarkWithdrawPaid(ctx, distledger.PayoutOutcome{
		TenantID: tenant, WithdrawalID: w.ID, Operator: "alice", InvoiceNo: "BANK-1",
	}); err != nil {
		t.Fatal(err)
	}

	after := balance(t, f, 3001)
	if after.Available != 0 {
		t.Fatalf("the fixture was supposed to leave nothing withdrawable, has %s", after.Available)
	}
	return f, amount
}

func refundOrder(t *testing.T, f *fixture, orderID, idem string) error {
	t.Helper()
	_, err := f.led.OnOrderRefunded(context.Background(), distledger.OrderRefundedEvent{
		TenantID: tenant, OrderID: orderID, IsFull: true,
		IdemKey: idem, RefundedAt: f.clock.Now(),
	})
	return err
}

// refundWholeOrder is refundOrder for the order the cash-out fixture uses.
func refundWholeOrder(t *testing.T, f *fixture) error {
	t.Helper()
	return refundOrder(t, f, "ORD-W1", "refund-1")
}

// TestRefundAfterPayoutIsRefusedByDefault is the conservative half of the debt
// policy.
//
// The refusal has to be loud and specific, because the alternative - quietly
// doing something plausible - is how a ledger ends up wrong without anybody
// noticing. The error names the shortfall, and the books stay consistent: a
// reversal that cannot be completed is not recorded, rather than half recorded.
func TestRefundAfterPayoutIsRefusedByDefault(t *testing.T) {
	rules := withdrawalRules() // AllowNegative defaults to false
	f, amount := cashOutFixture(t, rules, nil)

	if rep := f.selfCheck(); !rep.OK {
		t.Fatalf("the fixture must start consistent: %+v", rep.Invariants)
	}

	err := refundWholeOrder(t, f)
	if err == nil {
		t.Fatal("refunding money that has been paid out must not silently succeed")
	}
	if !errors.Is(err, distledger.ErrInsufficientBalance) {
		t.Fatalf("want ErrInsufficientBalance, got %v", err)
	}
	// The message has to be actionable: an operator needs to know how much is
	// missing and which knob turns the debt on.
	msg := err.Error()
	if !strings.Contains(msg, amount.String()) {
		t.Errorf("the error should name the shortfall %s: %s", amount, msg)
	}
	if !strings.Contains(msg, "AllowNegative") {
		t.Errorf("the error should name the setting that would permit the debt: %s", msg)
	}

	// Nothing was half-applied.
	if rep := f.selfCheck(); !rep.OK {
		t.Errorf("a refused reversal left the books inconsistent: %+v", rep.Invariants)
	}
	bal := balance(t, f, 3001)
	if bal.Available != 0 || bal.Withdrawn != amount {
		t.Errorf("a refused reversal moved money: %+v", bal)
	}
}

// TestRefundAfterPayoutWithDebtAllowed checks the opt-in half: the debt is
// represented, the books stay complete, and SelfCheck reports it without
// failing.
//
// The "without failing" part is what makes the setting usable. If a permitted
// debt counted as an invariant violation, every tenant that used it would see a
// permanent red light, and a real violation would be indistinguishable from the
// expected one.
func TestRefundAfterPayoutWithDebtAllowed(t *testing.T) {
	rules := withdrawalRules()
	rules.AllowNegative = true
	f, amount := cashOutFixture(t, rules, nil)

	if err := refundWholeOrder(t, f); err != nil {
		t.Fatalf("with AllowNegative the reversal must go through: %v", err)
	}

	bal := balance(t, f, 3001)
	if bal.Available != -amount {
		t.Fatalf("available = %s, want %s (the debt)", bal.Available, -amount)
	}
	if bal.Withdrawn != amount {
		t.Errorf("withdrawn = %s, want %s: the payout still happened", bal.Withdrawn, amount)
	}

	rep := f.selfCheck()
	if !rep.OK {
		t.Fatalf("a permitted debt must not fail the invariants: %+v", rep.Invariants)
	}
	// But it must be visible. A negative balance nobody is told about is its own
	// kind of bug.
	var noted bool
	for _, inv := range rep.Invariants {
		for _, n := range inv.Notes {
			if strings.Contains(n, "debt") && strings.Contains(n, amount.String()) {
				noted = true
			}
		}
	}
	if !noted {
		t.Errorf("a permitted debt must be reported as a note: %+v", rep.Invariants)
	}

	// A debt only in the withdrawable bucket: the frozen bucket going negative
	// would mean a commission was clawed back twice.
	var i1 distledger.InvariantResult
	for _, inv := range rep.Invariants {
		if strings.HasPrefix(inv.Name, "I1") {
			i1 = inv
		}
	}
	if len(i1.Violations) != 0 {
		t.Errorf("a permitted debt is not a violation, but I1 reported: %v", i1.Violations)
	}
}

// TestDebtIsRepaidByLaterEarnings is why representing the debt is worth it: the
// account recovers on its own instead of needing an out-of-band correction.
func TestDebtIsRepaidByLaterEarnings(t *testing.T) {
	rules := withdrawalRules()
	rules.AllowNegative = true
	f, amount := cashOutFixture(t, rules, nil)

	if err := refundWholeOrder(t, f); err != nil {
		t.Fatal(err)
	}
	if got := balance(t, f, 3001).Available; got != -amount {
		t.Fatalf("available = %s, want %s", got, -amount)
	}

	// A second order earns more than the debt and settles.
	f.mustPay("ORD-W2", 4001, 200000) // 5% -> 10000, twice the debt
	f.receive("ORD-W2", f.clock.Now())
	f.clock.Advance(8 * 24 * 60 * 60 * 1e9)
	f.maintain()

	bal := balance(t, f, 3001)
	if bal.Available != 10000-amount {
		t.Fatalf("available = %s, want %s: later earnings should have repaid the %s debt",
			bal.Available, 10000-amount, amount)
	}
	if bal.Available <= 0 {
		t.Fatalf("the debt should be repaid and then some, available = %s", bal.Available)
	}
	if rep := f.selfCheck(); !rep.OK {
		t.Errorf("repaying a debt broke the invariants: %+v", rep.Invariants)
	}
}

// TestWithdrawalIsRefusedWhileInDebt checks that the debt is not withdrawable,
// which sounds obvious and is worth pinning: the debt is a negative balance, so
// the ordinary balance guard already refuses it, and that is exactly the
// property to keep.
func TestWithdrawalIsRefusedWhileInDebt(t *testing.T) {
	ctx := context.Background()
	rules := withdrawalRules()
	rules.AllowNegative = true
	f, _ := cashOutFixture(t, rules, nil)
	if err := refundWholeOrder(t, f); err != nil {
		t.Fatal(err)
	}

	_, _, err := f.led.RequestWithdraw(ctx, distledger.WithdrawRequest{
		TenantID: tenant, UserID: 3001, Amount: 1000, IdemKey: "while-in-debt",
	})
	if !errors.Is(err, distledger.ErrInsufficientBalance) {
		t.Fatalf("want ErrInsufficientBalance, got %v", err)
	}
}

// TestDebtIsNotAllowedInTheFrozenBucket is the boundary of the rule.
//
// AllowNegative permits a debt in the withdrawable bucket because that is what
// "the agent owes the platform" looks like. A frozen bucket below zero would
// mean a commission was clawed back twice - a defect, not a debt - and letting
// the rule cover it would hide the defect behind a setting.
func TestDebtIsNotAllowedInTheFrozenBucket(t *testing.T) {
	ctx := context.Background()
	rules := withdrawalRules()
	rules.AllowNegative = true
	f := newFixture(t, rules)
	f.mustAgent(3001, 0, distledger.AgentActive)
	f.mustBuyer(4001, 3001)

	f.mustPay("ORD-F1", 4001, 100000) // accrues into Frozen, never settled
	f.receive("ORD-F1", f.clock.Now())

	// Corrupt the frozen bucket to zero without touching the ledger, so the
	// reversal below must reach below zero to proceed.
	acct := balance(t, f, 3001)
	acct.Frozen = 0
	acct.Available = 0
	acct.TotalEarned = 0
	if err := f.store.Update(ctx, func(ctx context.Context, tx distledger.Tx) error {
		_, err := tx.PutAccount(ctx, acct)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	err := refundOrder(t, f, "ORD-F1", "refund-f1")
	if err == nil {
		t.Fatal("a frozen bucket must not be allowed to go negative, even with AllowNegative")
	}
	if !errors.Is(err, distledger.ErrInsufficientBalance) {
		t.Fatalf("want ErrInsufficientBalance, got %v", err)
	}
}

// TestWithdrawThenRefundKeepsTheLedgerConserved is the end-to-end shape: the
// payout, the debt, and the recovery all have to leave a ledger that still adds
// up to the account.
func TestWithdrawThenRefundKeepsTheLedgerConserved(t *testing.T) {
	rules := withdrawalRules()
	rules.AllowNegative = true
	f, amount := cashOutFixture(t, rules, nil)

	if err := refundWholeOrder(t, f); err != nil {
		t.Fatal(err)
	}
	f.mustPay("ORD-W3", 4001, 200000) // 5% -> 10000, more than the debt
	f.receive("ORD-W3", f.clock.Now())
	f.clock.Advance(8 * 24 * 60 * 60 * 1e9)
	f.maintain()

	bal := balance(t, f, 3001)
	if bal.Available != 10000-amount {
		t.Errorf("available = %s, want %s (the new commission minus the debt)",
			bal.Available, 10000-amount)
	}
	if bal.Withdrawn != amount {
		t.Errorf("withdrawn = %s, want %s", bal.Withdrawn, amount)
	}

	// Writing the ledger out and re-summing it is the check that matters: I1
	// recomputes the same sums, so this asserts the account agrees with its own
	// history after a payout, a debt and a recovery.
	var sum int64
	for _, e := range ledgerFor(t, f, 3001) {
		sum += int64(e.DeltaAvailable)
	}
	if distledger.Money(sum) != bal.Available {
		t.Errorf("ledger deltas sum to %s but the account holds %s", distledger.Money(sum), bal.Available)
	}
	if rep := f.selfCheck(); !rep.OK {
		t.Errorf("the full cycle broke the invariants: %+v", rep.Invariants)
	}
}
