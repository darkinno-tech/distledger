package distledger

import (
	"errors"
	"testing"
)

// TestCommissionTransitionMatrix exercises all 6x6 combinations.
//
// The expectations are **restated independently** here rather than read from
// the table under test: if that table were edited by mistake, a test that read
// it would agree with the mistake, and verification against the same source
// verifies nothing.
func TestCommissionTransitionMatrix(t *testing.T) {
	want := map[CommissionState]map[CommissionState]bool{
		CommissionPending: {
			CommissionSettled:  true,
			CommissionReversed: true,
			CommissionFrozen:   true,
			CommissionVoid:     true,
		},
		CommissionSettled: {
			// No "withdrawn" edge: a withdrawal is an amount against the
			// account, not a commission state (ADR-044).
			CommissionReversed: true,
		},
		CommissionFrozen: {
			CommissionPending: true,
			CommissionVoid:    true,
			// A refunded order must be able to claw back a risk-frozen
			// commission: its money is still sitting in the frozen bucket, and
			// without this edge the funds would be stranded in a state that is
			// neither payable nor recoverable.
			CommissionReversed: true,
		},
		CommissionReversed: {},
		CommissionVoid:     {},
	}

	states := AllCommissionStates()
	if len(states) != len(want) {
		t.Fatalf("state count mismatch: %d states but %d expected groups", len(states), len(want))
	}

	for _, from := range states {
		for _, to := range states {
			expected := want[from][to]
			got := CanTransitionCommission(from, to)
			if got != expected {
				t.Errorf("CanTransitionCommission(%s, %s) = %v, want %v",
					from, to, got, expected)
			}
		}
	}
}

// TestSelfTransitionAlwaysRejected calls out one easily overlooked rule:
// a state advance must be a real advance. If Pending → Pending were legal,
// a duplicate delivery could pass as a successful transition and post twice.
func TestSelfTransitionAlwaysRejected(t *testing.T) {
	for _, s := range AllCommissionStates() {
		if CanTransitionCommission(s, s) {
			t.Errorf("%s -> %s must not be a legal transition", s, s)
		}
	}
}

func TestTerminalStates(t *testing.T) {
	terminal := map[CommissionState]bool{
		CommissionReversed: true,
		CommissionVoid:     true,
	}
	for _, s := range AllCommissionStates() {
		if s.Terminal() != terminal[s] {
			t.Errorf("%s.Terminal() = %v, want %v", s, s.Terminal(), terminal[s])
		}
		for _, to := range AllCommissionStates() {
			if s.Terminal() && CanTransitionCommission(s, to) {
				t.Errorf("terminal state %s must not transition to %s", s, to)
			}
		}
	}
}

func TestValidateCommissionTransitionError(t *testing.T) {
	err := ValidateCommissionTransition(CommissionVoid, CommissionPending)
	if !errors.Is(err, ErrIllegalTransition) {
		t.Fatalf("expected ErrIllegalTransition, got %v", err)
	}
	var te *TransitionError
	if !errors.As(err, &te) {
		t.Fatalf("expected *TransitionError, got %T", err)
	}
	if te.From != "void" || te.To != "pending" {
		t.Fatalf("unexpected transition error payload: %+v", te)
	}

	if err := ValidateCommissionTransition(CommissionPending, CommissionSettled); err != nil {
		t.Fatalf("legal transition reported an error: %v", err)
	}

	if err := ValidateCommissionTransition(CommissionState(99), CommissionPending); err == nil {
		t.Fatal("unknown source state must be rejected")
	}
	if err := ValidateCommissionTransition(CommissionPending, CommissionState(99)); err == nil {
		t.Fatal("unknown target state must be rejected")
	}
}

// TestAllCommissionStatesReturnsCopy stops a caller from corrupting the state
// machine by mutating the returned slice.
func TestAllCommissionStatesReturnsCopy(t *testing.T) {
	a := AllCommissionStates()
	a[0] = CommissionState(200)
	b := AllCommissionStates()
	if b[0] == CommissionState(200) {
		t.Fatal("AllCommissionStates leaked its internal slice")
	}
}
