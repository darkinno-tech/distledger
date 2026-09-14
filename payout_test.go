package distledger_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/darkinno-tech/distledger"
	"github.com/darkinno-tech/distledger/store/memory"
)

// withdrawalFixture is a settled, withdrawable balance for agent 3001.
//
// It reaches that state through the ordinary engine path - pay, receive,
// advance past the freeze window, settle - rather than by writing an account
// directly, because a withdrawal test that starts from a hand-made balance would
// not notice settlement and withdrawal disagreeing about which bucket the money
// is in.
func withdrawalFixture(t *testing.T, rules distledger.Rules) *fixture {
	t.Helper()
	f := newFixture(t, rules)
	f.mustAgent(3001, 0, distledger.AgentActive)
	f.mustBuyer(4001, 3001)
	f.mustPay("ORD-W1", 4001, 100000) // 5% -> 5000
	f.receive("ORD-W1", f.clock.Now())
	f.clock.Advance(8 * 24 * 60 * 60 * 1e9) // past the 7-day freeze
	f.maintain()

	bal, err := f.led.Balance(context.Background(), tenant, 3001)
	if err != nil {
		t.Fatal(err)
	}
	if bal.Available <= 0 {
		t.Fatalf("fixture produced no withdrawable balance: %+v", bal)
	}
	return f
}

func withdrawalRules() distledger.Rules {
	r := twoLevelRules()
	r.MinWithdraw = 1000
	return r
}

// moneyOf strips the bookkeeping fields, so that a test about money is not
// derailed by a version bump. Comparing whole accounts made two correct
// behaviors look like failures: a rejection and a give-up both leave the
// balances exactly as they were, and both change the version.
func moneyOf(a distledger.Account) distledger.Account {
	return distledger.Account{
		Key:           a.Key,
		Frozen:        a.Frozen,
		Available:     a.Available,
		Withdrawing:   a.Withdrawing,
		Withdrawn:     a.Withdrawn,
		TotalEarned:   a.TotalEarned,
		TotalReversed: a.TotalReversed,
	}
}

func balance(t *testing.T, f *fixture, userID int64) distledger.Account {
	t.Helper()
	b, err := f.led.Balance(context.Background(), tenant, userID)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestWithdrawRequestReservesTheMoney pins the core property of a withdrawal:
// the money leaves the withdrawable bucket immediately, so it cannot be spent
// twice while the request waits for review.
func TestWithdrawRequestReservesTheMoney(t *testing.T) {
	ctx := context.Background()
	f := withdrawalFixture(t, withdrawalRules())
	before := balance(t, f, 3001)

	w, existed, err := f.led.RequestWithdraw(ctx, distledger.WithdrawRequest{
		TenantID: tenant, UserID: 3001, Amount: 3000, IdemKey: "wd-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if existed {
		t.Fatal("the first request must not report an existing withdrawal")
	}
	if w.State != distledger.WithdrawalApplied {
		t.Errorf("state = %s, want applied", w.State)
	}
	if w.RealAmount != w.Amount-w.Fee {
		t.Errorf("real amount %s must be amount %s minus fee %s", w.RealAmount, w.Amount, w.Fee)
	}

	after := balance(t, f, 3001)
	if after.Available != before.Available-3000 {
		t.Errorf("available = %s, want %s", after.Available, before.Available-3000)
	}
	if after.Withdrawing != before.Withdrawing+3000 {
		t.Errorf("withdrawing = %s, want %s", after.Withdrawing, before.Withdrawing+3000)
	}
	if after.Withdrawn != before.Withdrawn {
		t.Errorf("nothing has been paid out yet, withdrawn = %s", after.Withdrawn)
	}

	// The reservation must be on the ledger, or reconciliation cannot see it.
	entries := ledgerFor(t, f, 3001)
	var holds int
	for _, e := range entries {
		if e.BizType == distledger.LedgerWithdrawHold {
			holds++
			if e.DeltaAvailable != -3000 || e.DeltaWithdrawing != 3000 {
				t.Errorf("hold entry moved the wrong buckets: %+v", e)
			}
		}
	}
	if holds != 1 {
		t.Errorf("got %d withdraw-hold entries, want 1", holds)
	}
}

// TestWithdrawRequestIsIdempotent is the property protecting the agent's money:
// a retried request must not reserve the balance twice.
func TestWithdrawRequestIsIdempotent(t *testing.T) {
	ctx := context.Background()
	f := withdrawalFixture(t, withdrawalRules())
	before := balance(t, f, 3001)

	first, _, err := f.led.RequestWithdraw(ctx, distledger.WithdrawRequest{
		TenantID: tenant, UserID: 3001, Amount: 3000, IdemKey: "same",
	})
	if err != nil {
		t.Fatal(err)
	}
	second, existed, err := f.led.RequestWithdraw(ctx, distledger.WithdrawRequest{
		TenantID: tenant, UserID: 3001, Amount: 3000, IdemKey: "same",
	})
	if err != nil {
		t.Fatalf("a retry must not fail: %v", err)
	}
	if !existed {
		t.Error("the second call must report an existing withdrawal")
	}
	if second.ID != first.ID {
		t.Errorf("a retry returned a different withdrawal: %d then %d", first.ID, second.ID)
	}
	if got := balance(t, f, 3001).Withdrawing; got != before.Withdrawing+3000 {
		t.Errorf("withdrawing = %s, want %s: the retry reserved the money twice", got, before.Withdrawing+3000)
	}

	// And the caller can find it by its own key afterwards.
	byKey, err := f.led.WithdrawalByIdemKey(ctx, tenant, "same")
	if err != nil {
		t.Fatal(err)
	}
	if byKey.ID != first.ID {
		t.Errorf("lookup by caller key returned %d, want %d", byKey.ID, first.ID)
	}
}

func TestWithdrawRequestRejectsBadInput(t *testing.T) {
	ctx := context.Background()
	f := withdrawalFixture(t, withdrawalRules())

	for _, tc := range []struct {
		name  string
		req   distledger.WithdrawRequest
		field string
	}{
		{"below minimum", distledger.WithdrawRequest{
			TenantID: tenant, UserID: 3001, Amount: 999, IdemKey: "k1"}, "amount"},
		{"zero amount", distledger.WithdrawRequest{
			TenantID: tenant, UserID: 3001, Amount: 0, IdemKey: "k2"}, "amount"},
		{"empty idem key", distledger.WithdrawRequest{
			TenantID: tenant, UserID: 3001, Amount: 3000}, "idem_key"},
		{"unknown user", distledger.WithdrawRequest{
			TenantID: tenant, UserID: 0, Amount: 3000, IdemKey: "k3"}, "user_id"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := f.led.RequestWithdraw(ctx, tc.req)
			if err == nil {
				t.Fatal("must be rejected")
			}
			var fe *distledger.FieldError
			if !errors.As(err, &fe) {
				t.Fatalf("want a FieldError, got %T: %v", err, err)
			}
			if fe.Field != tc.field {
				t.Errorf("field = %q, want %q", fe.Field, tc.field)
			}
		})
	}
}

// TestWithdrawRequestBeyondBalanceLeavesNothingBehind checks that a failed
// request is fully rolled back: a withdrawal record with no money behind it
// would be worse than no record, because an operator would see a queue entry for
// money that was never reserved.
func TestWithdrawRequestBeyondBalanceLeavesNothingBehind(t *testing.T) {
	ctx := context.Background()
	f := withdrawalFixture(t, withdrawalRules())
	before := balance(t, f, 3001)

	_, _, err := f.led.RequestWithdraw(ctx, distledger.WithdrawRequest{
		TenantID: tenant, UserID: 3001, Amount: before.Available + 1, IdemKey: "too-much",
	})
	if !errors.Is(err, distledger.ErrInsufficientBalance) {
		t.Fatalf("want ErrInsufficientBalance, got %v", err)
	}

	// Nothing moved.
	after := balance(t, f, 3001)
	if after != before {
		t.Errorf("a refused request changed the account:\n before %+v\n  after %+v", before, after)
	}
	// No record survived.
	if _, err := f.led.WithdrawalByIdemKey(ctx, tenant, "too-much"); !errors.Is(err, distledger.ErrNotFound) {
		t.Errorf("a refused request left a withdrawal behind: %v", err)
	}
	list, err := f.led.Withdrawals(ctx, tenant, distledger.Page{})
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Errorf("got %d withdrawals, want 0", len(list))
	}
	// The key is reusable: a rollback that leaked it would make the caller's
	// corrected retry fail as a duplicate.
	smaller := before.Available - 1000
	if smaller < 1000 {
		smaller = 1000
	}
	if _, _, err := f.led.RequestWithdraw(ctx, distledger.WithdrawRequest{
		TenantID: tenant, UserID: 3001, Amount: smaller, IdemKey: "too-much",
	}); err != nil {
		t.Errorf("the key must be usable after a refused attempt: %v", err)
	}
}

// TestWithdrawFeeKeepsTheLedgerConserved is the check that the fee is recorded
// rather than routed: the account loses the gross, and I1 still holds.
func TestWithdrawFeeKeepsTheLedgerConserved(t *testing.T) {
	ctx := context.Background()
	rules := withdrawalRules()
	rules.FeeRateBP = 200 // 2%
	f := withdrawalFixture(t, rules)
	before := balance(t, f, 3001)

	w, _, err := f.led.RequestWithdraw(ctx, distledger.WithdrawRequest{
		TenantID: tenant, UserID: 3001, Amount: 3000, IdemKey: "fee-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if w.Fee != 60 {
		t.Fatalf("fee = %s, want 60 (2%% of 3000)", w.Fee)
	}
	if w.RealAmount != 2940 {
		t.Fatalf("real amount = %s, want 2940", w.RealAmount)
	}

	if _, err := f.led.ApproveWithdraw(ctx, distledger.WithdrawDecision{
		TenantID: tenant, WithdrawalID: w.ID, Operator: "alice",
	}); err != nil {
		t.Fatal(err)
	}
	paid, err := f.led.MarkWithdrawPaid(ctx, distledger.PayoutOutcome{
		TenantID: tenant, WithdrawalID: w.ID, Operator: "alice", InvoiceNo: "BANK-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if paid.State != distledger.WithdrawalPaid {
		t.Fatalf("state = %s, want paid", paid.State)
	}

	after := balance(t, f, 3001)
	// The gross leaves the account; the fee is revenue outside this ledger.
	if after.Withdrawn != before.Withdrawn+3000 {
		t.Errorf("withdrawn = %s, want %s (the gross, not the net)",
			after.Withdrawn, before.Withdrawn+3000)
	}
	if after.Available != before.Available-3000 {
		t.Errorf("available = %s, want %s", after.Available, before.Available-3000)
	}
	if rep := f.selfCheck(); !rep.OK {
		t.Errorf("a withdrawal with a fee broke the invariants: %+v", rep.Invariants)
	}
}

func TestWithdrawRejectReturnsTheMoney(t *testing.T) {
	ctx := context.Background()
	f := withdrawalFixture(t, withdrawalRules())
	before := balance(t, f, 3001)

	w, _, err := f.led.RequestWithdraw(ctx, distledger.WithdrawRequest{
		TenantID: tenant, UserID: 3001, Amount: 3000, IdemKey: "rej-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	rejected, err := f.led.RejectWithdraw(ctx, distledger.WithdrawDecision{
		TenantID: tenant, WithdrawalID: w.ID, Operator: "alice", Reason: "account name mismatch",
	})
	if err != nil {
		t.Fatal(err)
	}
	if rejected.State != distledger.WithdrawalRejected {
		t.Fatalf("state = %s, want rejected", rejected.State)
	}
	if got := balance(t, f, 3001); moneyOf(got) != moneyOf(before) {
		t.Errorf("a rejection must return the money exactly:\n before %+v\n  after %+v", before, got)
	}
	if rep := f.selfCheck(); !rep.OK {
		t.Errorf("a rejected withdrawal broke the invariants: %+v", rep.Invariants)
	}

	// Rejecting a rejection is a caller error, not a retry.
	if _, err := f.led.RejectWithdraw(ctx, distledger.WithdrawDecision{
		TenantID: tenant, WithdrawalID: w.ID, Reason: "again",
	}); !errors.Is(err, distledger.ErrIllegalTransition) {
		t.Errorf("rejecting a rejected withdrawal: got %v, want ErrIllegalTransition", err)
	}
}

func TestWithdrawRejectRequiresAReason(t *testing.T) {
	ctx := context.Background()
	f := withdrawalFixture(t, withdrawalRules())
	w, _, err := f.led.RequestWithdraw(ctx, distledger.WithdrawRequest{
		TenantID: tenant, UserID: 3001, Amount: 3000, IdemKey: "rej-2",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.led.RejectWithdraw(ctx, distledger.WithdrawDecision{
		TenantID: tenant, WithdrawalID: w.ID, Operator: "alice",
	}); err == nil {
		t.Fatal("a rejection with no reason must be refused: an agent asking why would get nothing")
	}
}

// TestWithdrawPayViaChannel covers the automatic path, including the retry after
// a failed attempt.
func TestWithdrawPayViaChannel(t *testing.T) {
	ctx := context.Background()
	ch := &distledger.MockChannel{}
	f := newPayoutFixture(t, withdrawalRules(), ch)

	w, _, err := f.led.RequestWithdraw(ctx, distledger.WithdrawRequest{
		TenantID: tenant, UserID: 3001, Amount: 3000, IdemKey: "pay-1",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Paying before approval is refused: the review gate is not optional.
	if _, err := f.led.PayWithdraw(ctx, distledger.PayoutOutcome{
		TenantID: tenant, WithdrawalID: w.ID,
	}); !errors.Is(err, distledger.ErrIllegalTransition) {
		t.Fatalf("paying an unapproved withdrawal: got %v, want ErrIllegalTransition", err)
	}
	if len(ch.Calls) != 0 {
		t.Fatal("the channel must not be called for an unapproved withdrawal")
	}

	if _, err := f.led.ApproveWithdraw(ctx, distledger.WithdrawDecision{
		TenantID: tenant, WithdrawalID: w.ID, Operator: "alice",
	}); err != nil {
		t.Fatal(err)
	}
	paid, err := f.led.PayWithdraw(ctx, distledger.PayoutOutcome{
		TenantID: tenant, WithdrawalID: w.ID, Operator: "alice",
	})
	if err != nil {
		t.Fatal(err)
	}
	if paid.State != distledger.WithdrawalPaid {
		t.Fatalf("state = %s, want paid", paid.State)
	}
	if paid.InvoiceNo == "" {
		t.Error("a successful payout must record the channel's reference")
	}
	if len(ch.Calls) != 1 {
		t.Fatalf("channel called %d times, want 1", len(ch.Calls))
	}
	// The channel's idempotency key must be the withdrawal's own, so that a
	// retry after a crash pays once.
	if ch.Calls[0].IdemKey != paid.IdemKey {
		t.Errorf("channel key %q != withdrawal key %q: a retry could pay twice",
			ch.Calls[0].IdemKey, paid.IdemKey)
	}
	// The channel is paid the net amount, not the gross.
	if ch.Calls[0].Amount != paid.RealAmount {
		t.Errorf("channel amount %s, want the real amount %s", ch.Calls[0].Amount, paid.RealAmount)
	}
	if rep := f.selfCheck(); !rep.OK {
		t.Errorf("a paid withdrawal broke the invariants: %+v", rep.Invariants)
	}
}

// TestWithdrawPayFailureKeepsTheMoneyReserved covers the path where a wrong
// answer strands the agent's money.
func TestWithdrawPayFailureKeepsTheMoneyReserved(t *testing.T) {
	ctx := context.Background()
	ch := &distledger.MockChannel{Fail: true}
	f := newPayoutFixture(t, withdrawalRules(), ch)

	w, _, err := f.led.RequestWithdraw(ctx, distledger.WithdrawRequest{
		TenantID: tenant, UserID: 3001, Amount: 3000, IdemKey: "fail-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.led.ApproveWithdraw(ctx, distledger.WithdrawDecision{
		TenantID: tenant, WithdrawalID: w.ID, Operator: "alice",
	}); err != nil {
		t.Fatal(err)
	}

	failed, err := f.led.PayWithdraw(ctx, distledger.PayoutOutcome{
		TenantID: tenant, WithdrawalID: w.ID, Operator: "alice",
	})
	if err != nil {
		t.Fatalf("a channel-reported failure is an outcome, not an error: %v", err)
	}
	if failed.State != distledger.WithdrawalPayFailed {
		t.Fatalf("state = %s, want pay_failed", failed.State)
	}
	if failed.FailReason == "" {
		t.Error("a failure must record why, or a retry decision is guesswork")
	}
	// The money stays reserved, so a retry pays the same amount rather than
	// reserving a second time.
	bal := balance(t, f, 3001)
	if bal.Withdrawing != 3000 {
		t.Errorf("withdrawing = %s, want 3000: a failed payout must keep the reservation", bal.Withdrawing)
	}
	if bal.Withdrawn != 0 {
		t.Errorf("withdrawn = %s, want 0: nothing was paid", bal.Withdrawn)
	}

	// Retrying succeeds and does not double-reserve.
	ch.Fail = false
	paid, err := f.led.PayWithdraw(ctx, distledger.PayoutOutcome{
		TenantID: tenant, WithdrawalID: w.ID, Operator: "alice",
	})
	if err != nil {
		t.Fatal(err)
	}
	if paid.State != distledger.WithdrawalPaid {
		t.Fatalf("after retry state = %s, want paid", paid.State)
	}
	after := balance(t, f, 3001)
	if after.Withdrawing != 0 || after.Withdrawn != 3000 {
		t.Errorf("after a successful retry: withdrawing %s, withdrawn %s; want 0 and 3000",
			after.Withdrawing, after.Withdrawn)
	}
	if rep := f.selfCheck(); !rep.OK {
		t.Errorf("a retried payout broke the invariants: %+v", rep.Invariants)
	}
}

// TestWithdrawPayGivingUpReturnsTheMoney covers the edge the PRD's diagram
// omits: if a payout cannot be retried, the money must come back rather than
// being stranded in the reserved bucket forever.
func TestWithdrawPayGivingUpReturnsTheMoney(t *testing.T) {
	ctx := context.Background()
	ch := &distledger.MockChannel{Fail: true}
	f := newPayoutFixture(t, withdrawalRules(), ch)
	before := balance(t, f, 3001)

	w, _, err := f.led.RequestWithdraw(ctx, distledger.WithdrawRequest{
		TenantID: tenant, UserID: 3001, Amount: 3000, IdemKey: "give-up",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.led.ApproveWithdraw(ctx, distledger.WithdrawDecision{
		TenantID: tenant, WithdrawalID: w.ID, Operator: "alice",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.led.PayWithdraw(ctx, distledger.PayoutOutcome{
		TenantID: tenant, WithdrawalID: w.ID, Operator: "alice",
	}); err != nil {
		t.Fatal(err)
	}
	// The operator gives up instead of retrying.
	if _, err := f.led.RejectWithdraw(ctx, distledger.WithdrawDecision{
		TenantID: tenant, WithdrawalID: w.ID, Operator: "alice",
		Reason: "destination account closed",
	}); err != nil {
		t.Fatalf("giving up after a failed payout must be possible: %v", err)
	}
	if got := balance(t, f, 3001); moneyOf(got) != moneyOf(before) {
		t.Errorf("giving up must return the money exactly:\n before %+v\n  after %+v", before, got)
	}
	if rep := f.selfCheck(); !rep.OK {
		t.Errorf("giving up broke the invariants: %+v", rep.Invariants)
	}
}

func TestWithdrawPayWithoutAChannelSaysSo(t *testing.T) {
	ctx := context.Background()
	f := withdrawalFixture(t, withdrawalRules())
	w, _, err := f.led.RequestWithdraw(ctx, distledger.WithdrawRequest{
		TenantID: tenant, UserID: 3001, Amount: 3000, IdemKey: "no-channel",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.led.ApproveWithdraw(ctx, distledger.WithdrawDecision{
		TenantID: tenant, WithdrawalID: w.ID, Operator: "alice",
	}); err != nil {
		t.Fatal(err)
	}
	_, err = f.led.PayWithdraw(ctx, distledger.PayoutOutcome{
		TenantID: tenant, WithdrawalID: w.ID,
	})
	if !errors.Is(err, distledger.ErrNoPayoutChannel) {
		t.Fatalf("got %v, want ErrNoPayoutChannel: the fix is configuration, not a retry", err)
	}
	// The manual path still works, which is the point of the default.
	if _, err := f.led.MarkWithdrawPaid(ctx, distledger.PayoutOutcome{
		TenantID: tenant, WithdrawalID: w.ID, Operator: "alice", InvoiceNo: "BANK-9",
	}); err != nil {
		t.Fatalf("manual confirmation must work with no channel: %v", err)
	}
}

// TestWithdrawTenantIsolation checks that an id from another tenant is not a
// handle. Commission and withdrawal ids are global, so an id alone must never
// authorise anything.
func TestWithdrawTenantIsolation(t *testing.T) {
	ctx := context.Background()
	f := withdrawalFixture(t, withdrawalRules())
	w, _, err := f.led.RequestWithdraw(ctx, distledger.WithdrawRequest{
		TenantID: tenant, UserID: 3001, Amount: 3000, IdemKey: "iso-1",
	})
	if err != nil {
		t.Fatal(err)
	}

	other := tenant + 7
	if _, err := f.led.Withdrawal(ctx, other, w.ID); !errors.Is(err, distledger.ErrNotFound) {
		t.Errorf("cross-tenant read: got %v, want ErrNotFound", err)
	}
	if _, err := f.led.ApproveWithdraw(ctx, distledger.WithdrawDecision{
		TenantID: other, WithdrawalID: w.ID, Operator: "mallory",
	}); !errors.Is(err, distledger.ErrNotFound) {
		t.Errorf("cross-tenant approve: got %v, want ErrNotFound", err)
	}
	if _, err := f.led.RejectWithdraw(ctx, distledger.WithdrawDecision{
		TenantID: other, WithdrawalID: w.ID, Reason: "mine now",
	}); !errors.Is(err, distledger.ErrNotFound) {
		t.Errorf("cross-tenant reject: got %v, want ErrNotFound", err)
	}
	// And the withdrawal is untouched.
	got, err := f.led.Withdrawal(ctx, tenant, w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != distledger.WithdrawalApplied {
		t.Errorf("state = %s, want applied", got.State)
	}
}

// TestWithdrawMarkPaidIsIdempotent covers the manual confirmation being
// retried, which happens whenever an operator reloads a page.
func TestWithdrawMarkPaidIsIdempotent(t *testing.T) {
	ctx := context.Background()
	f := withdrawalFixture(t, withdrawalRules())
	w, _, err := f.led.RequestWithdraw(ctx, distledger.WithdrawRequest{
		TenantID: tenant, UserID: 3001, Amount: 3000, IdemKey: "manual-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.led.ApproveWithdraw(ctx, distledger.WithdrawDecision{
		TenantID: tenant, WithdrawalID: w.ID, Operator: "alice",
	}); err != nil {
		t.Fatal(err)
	}
	first, err := f.led.MarkWithdrawPaid(ctx, distledger.PayoutOutcome{
		TenantID: tenant, WithdrawalID: w.ID, Operator: "alice", InvoiceNo: "BANK-7",
	})
	if err != nil {
		t.Fatal(err)
	}
	after := balance(t, f, 3001)

	second, err := f.led.MarkWithdrawPaid(ctx, distledger.PayoutOutcome{
		TenantID: tenant, WithdrawalID: w.ID, Operator: "alice", InvoiceNo: "BANK-7",
	})
	if err != nil {
		t.Fatalf("a repeated confirmation must not fail: %v", err)
	}
	if second.ID != first.ID || second.State != distledger.WithdrawalPaid {
		t.Errorf("repeated confirmation changed the record: %+v", second)
	}
	if got := balance(t, f, 3001); moneyOf(got) != moneyOf(after) {
		t.Errorf("a repeated confirmation moved money again:\n before %+v\n  after %+v", after, got)
	}
}

func ledgerFor(t *testing.T, f *fixture, userID int64) []distledger.LedgerEntry {
	t.Helper()
	var out []distledger.LedgerEntry
	if err := f.store.View(context.Background(), func(ctx context.Context, r distledger.Reader) error {
		var err error
		out, err = r.LedgerEntries(ctx, distledger.UserKey{TenantID: tenant, UserID: userID},
			distledger.LedgerQuery{Page: distledger.Page{Limit: distledger.MaxPageLimit}})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

// newPayoutFixture is withdrawalFixture with a payout channel installed.
//
// The channel has to be given at construction because the Ledger holds only
// immutable configuration, which is also why it is safe to share across
// goroutines.
func newPayoutFixture(t *testing.T, rules distledger.Rules, ch distledger.PayoutChannel) *fixture {
	t.Helper()
	f := newFixtureWithChannel(t, rules, ch)
	f.mustAgent(3001, 0, distledger.AgentActive)
	f.mustBuyer(4001, 3001)
	f.mustPay("ORD-W1", 4001, 100000)
	f.receive("ORD-W1", f.clock.Now())
	f.clock.Advance(8 * 24 * 60 * 60 * 1e9)
	f.maintain()
	return f
}

// newFixtureWithChannel builds a fixture whose ledger has a payout channel.
//
// The channel is given at construction because the Ledger holds only immutable
// configuration, which is also why it is safe to share across goroutines.
func newFixtureWithChannel(
	t *testing.T, rules distledger.Rules, ch distledger.PayoutChannel,
) *fixture {
	t.Helper()
	clock := distledger.NewManualClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	store := memory.New()
	led, err := distledger.New(distledger.Config{
		Store:  store,
		Clock:  clock,
		Rules:  rules,
		Payout: ch,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = led.Close() })
	return &fixture{t: t, led: led, clock: clock, store: store}
}

// TestWithdrawalHistoryIsPaginatedAndCounted checks the reads an operator queue
// depends on.
func TestWithdrawalHistoryIsPaginatedAndCounted(t *testing.T) {
	ctx := context.Background()
	f := withdrawalFixture(t, withdrawalRules())
	before := balance(t, f, 3001)

	// Three requests of 1000 each, within the available balance.
	for i, key := range []string{"h1", "h2", "h3"} {
		if _, _, err := f.led.RequestWithdraw(ctx, distledger.WithdrawRequest{
			TenantID: tenant, UserID: 3001, Amount: 1000, IdemKey: key,
		}); err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
	}
	if got := balance(t, f, 3001).Withdrawing; got != before.Withdrawing+3000 {
		t.Errorf("withdrawing = %s, want %s", got, before.Withdrawing+3000)
	}

	page1, err := f.led.Withdrawals(ctx, tenant, distledger.Page{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(page1) != 2 {
		t.Fatalf("page 1 has %d rows, want 2", len(page1))
	}
	page2, err := f.led.Withdrawals(ctx, tenant, distledger.Page{
		AfterID: page1[len(page1)-1].ID, Limit: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(page2) != 1 {
		t.Fatalf("page 2 has %d rows, want 1", len(page2))
	}
	if page2[0].ID <= page1[len(page1)-1].ID {
		t.Error("keyset pagination did not advance")
	}
}

// TestSelfCheckReportsAWithdrawalMismatch is the test that makes I4 worth
// having: a withdrawal and its ledger entries must agree, and a corrupt pair
// must be found rather than passed.
func TestSelfCheckReportsAWithdrawalMismatch(t *testing.T) {
	ctx := context.Background()
	f := withdrawalFixture(t, withdrawalRules())
	w, _, err := f.led.RequestWithdraw(ctx, distledger.WithdrawRequest{
		TenantID: tenant, UserID: 3001, Amount: 3000, IdemKey: "i4-1",
	})
	if err != nil {
		t.Fatal(err)
	}

	if rep := f.selfCheck(); !rep.OK {
		t.Fatalf("a healthy withdrawal must pass: %+v", rep.Invariants)
	}

	// Corrupt it through a *legal* edge, so that what I4 catches is the missing
	// money rather than a rejected transition: approve it, then record the
	// payout without the money movement that is supposed to accompany it. This
	// is the shape a half-applied payout leaves behind, and it is exactly the
	// case a state-machine check cannot see.
	if _, err := f.led.ApproveWithdraw(ctx, distledger.WithdrawDecision{
		TenantID: tenant, WithdrawalID: w.ID, Operator: "alice",
	}); err != nil {
		t.Fatal(err)
	}
	approved, err := f.led.Withdrawal(ctx, tenant, w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.Update(ctx, func(ctx context.Context, tx distledger.Tx) error {
		_, err := tx.TransitionWithdrawal(ctx, w.ID,
			distledger.WithdrawalApproved, distledger.WithdrawalPaid, approved.Version,
			distledger.WithdrawalTransition{PaidAt: f.clock.Now(), InvoiceNo: "GHOST-1"})
		return err
	}); err != nil {
		t.Fatal(err)
	}

	rep := f.selfCheck()
	if rep.OK {
		t.Fatal("self-check passed although a withdrawal claims a payout with no money behind it")
	}
	var found bool
	for _, inv := range rep.Invariants {
		if strings.HasPrefix(inv.Name, "I4") {
			found = true
			if inv.OK {
				t.Errorf("I4 passed on a corrupted withdrawal: %+v", inv)
			}
			if len(inv.Violations) == 0 {
				t.Errorf("I4 failed with nothing recorded: %+v", inv)
			}
		}
	}
	if !found {
		t.Fatalf("no I4 result in %+v", rep.Invariants)
	}
}
