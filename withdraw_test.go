package distledger

import (
	"errors"
	"testing"
	"time"
)

// TestWithdrawalTransitionMatrix exercises all 5x5 combinations.
//
// The expectations are **restated independently** here rather than read from
// the table under test. A test that derived its expectations from
// withdrawalTransitions would agree with any mistake made in it, which is
// exactly the mistake worth catching: this table is the only thing deciding
// where an agent's money is allowed to go.
func TestWithdrawalTransitionMatrix(t *testing.T) {
	legal := map[WithdrawalState][]WithdrawalState{
		WithdrawalApplied:   {WithdrawalApproved, WithdrawalRejected},
		WithdrawalApproved:  {WithdrawalPaid, WithdrawalPayFailed},
		WithdrawalPayFailed: {WithdrawalPaid, WithdrawalRejected},
		WithdrawalPaid:      {},
		WithdrawalRejected:  {},
	}

	states := AllWithdrawalStates()
	if len(states) != 5 {
		t.Fatalf("got %d withdrawal states, want 5", len(states))
	}

	for _, from := range states {
		for _, to := range states {
			want := false
			for _, allowed := range legal[from] {
				if allowed == to {
					want = true
				}
			}
			if got := CanTransitionWithdrawal(from, to); got != want {
				t.Errorf("CanTransitionWithdrawal(%s, %s) = %v, want %v",
					from, to, got, want)
			}
		}
	}
}

// TestWithdrawalMoneyIsAlwaysRecoverable is the property that matters more than
// any individual edge.
//
// A withdrawal holds the agent's money in the reserved bucket for as long as it
// is not finished. If a state could hold money with no way out, the library
// would have created the "money that can never move again" problem it exists to
// prevent — which is precisely why PayFailed -> Rejected exists even though the
// PRD's diagram omits it.
func TestWithdrawalMoneyIsAlwaysRecoverable(t *testing.T) {
	for _, s := range AllWithdrawalStates() {
		if !s.Reserved() {
			continue
		}
		// A reserved state must have at least one outgoing transition, and at
		// least one of those must end the reservation.
		out := withdrawalTransitions[s]
		if len(out) == 0 {
			t.Errorf("%s reserves money but has no transition out of it", s)
			continue
		}
		var escapes bool
		for _, to := range out {
			if !to.Reserved() {
				escapes = true
			}
		}
		if !escapes {
			t.Errorf("%s reserves money and cannot reach a state that releases it", s)
		}
	}

	// The converse: a state that does not reserve money must not be reachable
	// from a reserved one by an edge that keeps the money reserved, or the
	// reservation bookkeeping would disagree with the account.
	for _, s := range AllWithdrawalStates() {
		if s.Reserved() {
			continue
		}
		for _, to := range withdrawalTransitions[s] {
			t.Errorf("%s is terminal for money purposes but transitions to %s", s, to)
		}
	}
}

// TestWithdrawalReservedMatchesTheAccountBuckets pins Reserved against the
// bucket each state is supposed to hold money in.
func TestWithdrawalReservedMatchesTheAccountBuckets(t *testing.T) {
	// The states in which the account's withdrawing bucket holds this money.
	want := map[WithdrawalState]bool{
		WithdrawalApplied:   true,
		WithdrawalApproved:  true,
		WithdrawalPayFailed: true,
		WithdrawalRejected:  false, // returned to available
		WithdrawalPaid:      false, // moved to withdrawn
	}
	for s, w := range want {
		if got := s.Reserved(); got != w {
			t.Errorf("%s.Reserved() = %v, want %v", s, got, w)
		}
	}
}

func TestWithdrawalStateStringsAndValidity(t *testing.T) {
	for _, s := range AllWithdrawalStates() {
		if !s.Valid() {
			t.Errorf("%s must be valid", s)
		}
		if s.String() == "" {
			t.Errorf("%s has an empty name", s)
		}
	}

	if WithdrawalState(200).Valid() {
		t.Error("an unknown state must not be valid")
	}
	if got := WithdrawalState(200).String(); got != "WithdrawalState(200)" {
		t.Errorf("unknown state should include its numeric value, got %q", got)
	}
	// Same state is not a transition: it would be a no-op write that bumps the
	// version, making a concurrent retry look like progress.
	for _, s := range AllWithdrawalStates() {
		if CanTransitionWithdrawal(s, s) {
			t.Errorf("%s -> %s must not be legal", s, s)
		}
	}
	if CanTransitionWithdrawal(WithdrawalState(200), WithdrawalApplied) {
		t.Error("a transition from an unknown state must not be legal")
	}
}

func TestWithdrawalTerminalAndOutstanding(t *testing.T) {
	paid := Withdrawal{Amount: 1000, Fee: 0, RealAmount: 1000, State: WithdrawalPaid,
		IdemKey: "k", PaidAt: time.Unix(1, 0)}
	if paid.Outstanding() != 0 {
		t.Errorf("a paid withdrawal holds nothing, got %s", paid.Outstanding())
	}
	rejected := Withdrawal{Amount: 1000, RealAmount: 1000, State: WithdrawalRejected, IdemKey: "k"}
	if rejected.Outstanding() != 0 {
		t.Errorf("a rejected withdrawal holds nothing, got %s", rejected.Outstanding())
	}
	applied := Withdrawal{Amount: 1000, RealAmount: 1000, State: WithdrawalApplied, IdemKey: "k"}
	if applied.Outstanding() != 1000 {
		t.Errorf("an applied withdrawal holds its full amount, got %s", applied.Outstanding())
	}
	if !paid.State.Terminal() || !rejected.State.Terminal() {
		t.Error("paid and rejected must be terminal")
	}
	for _, s := range []WithdrawalState{WithdrawalApplied, WithdrawalApproved, WithdrawalPayFailed} {
		if s.Terminal() {
			t.Errorf("%s must not be terminal", s)
		}
	}
}

func TestWithdrawalValidate(t *testing.T) {
	valid := Withdrawal{
		Key: UserKey{TenantID: 1, UserID: 2}, IdemKey: "k",
		Amount: 1000, Fee: 100, RealAmount: 900, State: WithdrawalApplied,
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("a well-formed withdrawal must validate: %v", err)
	}

	for _, tc := range []struct {
		name  string
		mut   func(*Withdrawal)
		field string
	}{
		{"zero amount", func(w *Withdrawal) { w.Amount = 0; w.RealAmount = -w.Fee }, "amount"},
		{"negative amount", func(w *Withdrawal) { w.Amount = -1; w.RealAmount = -101 }, "amount"},
		{"negative fee", func(w *Withdrawal) { w.Fee = -1; w.RealAmount = 1001 }, "fee"},
		{"empty idem key", func(w *Withdrawal) { w.IdemKey = "" }, "idem_key"},
		{"unknown state", func(w *Withdrawal) { w.State = WithdrawalState(200) }, "state"},
		{"real amount disagrees", func(w *Withdrawal) { w.RealAmount = 1 }, "real_amount"},
		{"tenant negative", func(w *Withdrawal) { w.Key.TenantID = -1 }, "tenant_id"},
		{"user zero", func(w *Withdrawal) { w.Key.UserID = 0 }, "user_id"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := valid
			tc.mut(&w)
			err := w.Validate()
			if err == nil {
				t.Fatalf("must be rejected")
			}
			var fe *FieldError
			if !errors.As(err, &fe) {
				t.Fatalf("want a FieldError, got %T: %v", err, err)
			}
			if fe.Field != tc.field {
				t.Errorf("field = %q, want %q", fe.Field, tc.field)
			}
		})
	}
}

// TestWithdrawalFeeCannotSwallowTheAmount guards a configuration error that
// would otherwise look like a payout.
func TestWithdrawalFeeCannotSwallowTheAmount(t *testing.T) {
	w := Withdrawal{
		Key: UserKey{TenantID: 1, UserID: 2}, IdemKey: "k",
		Amount: 100, Fee: 100, RealAmount: 0, State: WithdrawalApplied,
	}
	if err := w.Validate(); err == nil {
		t.Fatal("a fee equal to the amount must be rejected")
	}
	// The realistic case: a fee larger than the amount.
	w = Withdrawal{
		Key: UserKey{TenantID: 1, UserID: 2}, IdemKey: "k",
		Amount: 100, Fee: 250, RealAmount: -150, State: WithdrawalApplied,
	}
	if err := w.Validate(); err == nil {
		t.Fatal("a fee larger than the amount must be rejected")
	}
}

// TestWithdrawalPaidRequiresPaidAt pins ADR-018 for the withdrawal path: a
// payout that does not record when it happened is not auditable, and the two
// directions of the rule both matter.
func TestWithdrawalPaidRequiresPaidAt(t *testing.T) {
	w := Withdrawal{
		Key: UserKey{TenantID: 1, UserID: 2}, IdemKey: "k",
		Amount: 1000, RealAmount: 1000, State: WithdrawalPaid,
	}
	if err := w.Validate(); err == nil {
		t.Fatal("a paid withdrawal with no payout time must be rejected")
	}
	w.PaidAt = time.Unix(1, 0)
	if err := w.Validate(); err != nil {
		t.Fatalf("a paid withdrawal with a payout time must validate: %v", err)
	}
	// The other direction: a withdrawal that has not been paid must not claim a
	// payout time, or an operator reading the row would believe money left.
	w.State = WithdrawalApplied
	if err := w.Validate(); err == nil {
		t.Fatal("an unpaid withdrawal must not record a payout time")
	}
}
