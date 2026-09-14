package distledger

// This file holds the only hard-coded state transition definition in the
// kernel.
//
// Why the state machine is not pluggable: the state machine decides how money
// is allowed to flow. If a user could replace it, the library could not be
// accountable for any invariant and would lose its reason to exist (see
// ADR-002). The only thing users can change is the numbers — rates, level
// counts, thresholds — and those live in rule.go.

// commissionTransitions is the table of legal transitions for a commission.
//
// The table uses explicit enumeration rather than an if/else chain, which makes
// it exhaustively testable: transition_test.go walks all 6×6 combinations and
// checks every entry of this table.
var commissionTransitions = map[CommissionState][]CommissionState{
	// Pending: can settle, be reversed by a refund, be frozen by risk control,
	// or be voided.
	CommissionPending: {
		CommissionSettled,
		CommissionReversed,
		CommissionFrozen,
		CommissionVoid,
	},
	// Settled: can be withdrawn or reversed by a refund.
	CommissionSettled: {
		CommissionWithdrawn,
		CommissionReversed,
	},
	// Frozen by risk control: a successful appeal returns it to pending, a
	// failed appeal voids it, and an order refund reverses it directly.
	//
	// Frozen -> Reversed is mandatory: the money of a risk-frozen commission
	// still sits in the "pending settlement" bucket, so a refund has to be able
	// to take it back, otherwise the money would be stuck forever in an
	// intermediate state that neither pays out nor returns it.
	CommissionFrozen: {
		CommissionPending,
		CommissionVoid,
		CommissionReversed,
	},
	// The following are final states and never transition.
	//
	// CommissionWithdrawn is final because the money has already left the
	// account; clawing it back is a money movement (through the Reverse policy
	// and the negative-balance policy), not a state transition (delivered in
	// v0.3).
	CommissionWithdrawn: nil,
	CommissionReversed:  nil,
	CommissionVoid:      nil,
}

// allCommissionStates lists every state in declaration order, for exhaustive
// tests and iteration.
var allCommissionStates = []CommissionState{
	CommissionPending,
	CommissionSettled,
	CommissionWithdrawn,
	CommissionReversed,
	CommissionFrozen,
	CommissionVoid,
}

// AllCommissionStates returns a copy of all commission states.
//
// It returns a copy rather than the internal slice: a caller that took the
// internal slice and modified it would break the state machine's single source
// of truth.
func AllCommissionStates() []CommissionState {
	out := make([]CommissionState, len(allCommissionStates))
	copy(out, allCommissionStates)
	return out
}

// CanTransitionCommission reports whether from → to is a legal transition.
//
// A transition from a state to itself is always false: state advancement must
// be real advancement, otherwise a duplicate delivery would be mistaken for a
// successful transition and booked twice.
func CanTransitionCommission(from, to CommissionState) bool {
	if !from.Valid() || !to.Valid() {
		return false
	}
	for _, allowed := range commissionTransitions[from] {
		if allowed == to {
			return true
		}
	}
	return false
}

// ValidateCommissionTransition returns *TransitionError on an illegal
// transition.
func ValidateCommissionTransition(from, to CommissionState) error {
	if !from.Valid() {
		return fieldErrf("state", "unknown commission state %d", uint8(from))
	}
	if !to.Valid() {
		return fieldErrf("state", "unknown commission state %d", uint8(to))
	}
	if !CanTransitionCommission(from, to) {
		return &TransitionError{Kind: "commission", From: from.String(), To: to.String()}
	}
	return nil
}
