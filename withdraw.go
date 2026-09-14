package distledger

import (
	"fmt"
	"time"
)

// This file holds the second — and last — hard-coded state machine in the
// kernel.
//
// Why it is not pluggable, for the same reason as the commission machine: the
// state machine decides where money is allowed to go. Everything else about
// withdrawals is configurable (the minimum, the fee, whether a payout is
// attempted at all); which transitions exist is not.

// WithdrawalState describes the lifecycle state of one withdrawal.
type WithdrawalState uint8

const (
	// WithdrawalApplied means the agent has asked for the money and it has been
	// moved out of the withdrawable balance into the reserved one.
	//
	// The money moves at *application*, not at approval, which is the whole
	// point of a reserved bucket: without it an agent could request a
	// withdrawal and spend the same balance again before anyone reviewed it.
	WithdrawalApplied WithdrawalState = iota
	// WithdrawalApproved means a human or a policy cleared it for payout. The
	// money has not moved again; it was already reserved.
	WithdrawalApproved
	// WithdrawalRejected means the request was refused and the reserved money
	// was returned to the withdrawable balance.
	WithdrawalRejected
	// WithdrawalPayFailed means the payout was attempted and did not succeed.
	// The money stays reserved, so a retry pays the same amount.
	WithdrawalPayFailed
	// WithdrawalPaid means the money left. This is the only state in which the
	// account's withdrawn bucket grows, and the only one from which the money
	// cannot be returned.
	WithdrawalPaid
)

// Valid reports whether the state is a known value.
func (s WithdrawalState) Valid() bool { return s <= WithdrawalPaid }

// String returns the state name.
func (s WithdrawalState) String() string {
	switch s {
	case WithdrawalApplied:
		return "applied"
	case WithdrawalApproved:
		return "approved"
	case WithdrawalRejected:
		return "rejected"
	case WithdrawalPayFailed:
		return "pay_failed"
	case WithdrawalPaid:
		return "paid"
	default:
		return fmt.Sprintf("WithdrawalState(%d)", uint8(s))
	}
}

// Terminal reports whether the state is final.
func (s WithdrawalState) Terminal() bool {
	switch s {
	case WithdrawalRejected, WithdrawalPaid:
		return true
	default:
		return false
	}
}

// Reserved reports whether the withdrawal still holds money in the account's
// withdrawing bucket.
//
// This is the property that matters to every caller, because it decides whether
// the money is still recoverable: a reserved withdrawal can be rejected and the
// money returned, a paid one cannot.
func (s WithdrawalState) Reserved() bool {
	switch s {
	case WithdrawalApplied, WithdrawalApproved, WithdrawalPayFailed:
		return true
	default:
		return false
	}
}

// withdrawalTransitions is the table of legal transitions for a withdrawal.
//
// Like the commission table, it is an explicit enumeration rather than an
// if/else chain so that a test can walk every combination and check every entry.
var withdrawalTransitions = map[WithdrawalState][]WithdrawalState{
	// Applied: a review either clears it for payout or refuses it.
	WithdrawalApplied: {
		WithdrawalApproved,
		WithdrawalRejected,
	},
	// Approved: the payout either succeeds or does not. A failure keeps the
	// money reserved so that a retry pays the same amount rather than
	// re-reserving it.
	WithdrawalApproved: {
		WithdrawalPaid,
		WithdrawalPayFailed,
	},
	// PayFailed -> Paid is the retry.
	//
	// PayFailed -> Rejected is also here, and it is not decoration: without it
	// the money would be stuck in the reserved bucket forever once a payout
	// failed and could not be retried. The same reasoning as
	// Frozen -> Reversed for commissions: every intermediate state must have a
	// way out that returns the money, or the library itself creates the
	// "money that can never move again" problem it exists to prevent.
	WithdrawalPayFailed: {
		WithdrawalPaid,
		WithdrawalRejected,
	},
	// Paid and Rejected are final. Paid is final because the money has left and
	// recovering it is not a state transition but a money movement, handled by
	// the reversal path and the debt policy (see ADR-042).
	//
	// Rejected is final because the money went back to available, where it is
	// indistinguishable from any other available money - there is nothing left
	// to transition.
	WithdrawalPaid:     nil,
	WithdrawalRejected: nil,
}

// allWithdrawalStates lists every state in declaration order, for exhaustive
// tests and iteration.
var allWithdrawalStates = []WithdrawalState{
	WithdrawalApplied,
	WithdrawalApproved,
	WithdrawalRejected,
	WithdrawalPayFailed,
	WithdrawalPaid,
}

// AllWithdrawalStates returns every withdrawal state.
//
// The result is a copy: callers must not be able to redefine the state set.
func AllWithdrawalStates() []WithdrawalState {
	out := make([]WithdrawalState, len(allWithdrawalStates))
	copy(out, allWithdrawalStates)
	return out
}

// CanTransitionWithdrawal reports whether a withdrawal may move between two
// states.
//
// A transition to the same state is not legal: it would be a no-op write that
// bumps the version, which makes concurrent retries look like progress.
func CanTransitionWithdrawal(from, to WithdrawalState) bool {
	if !from.Valid() || !to.Valid() {
		return false
	}
	for _, allowed := range withdrawalTransitions[from] {
		if allowed == to {
			return true
		}
	}
	return false
}

// Withdrawal is one request to pay money out of an account.
//
// # Mutability contract
//
// Financial fields never change after creation:
//
//	Key / IdemKey / Amount / Fee / RealAmount / AppliedAt
//
// Lifecycle fields change as the state advances, and every change carries a
// Version for CAS:
//
//	State / FailReason / Channel / InvoiceNo / Operator / AuditedAt / PaidAt /
//	Version
//
// Unlike a commission, a withdrawal is **not append-only in the same way**: it
// is a request whose outcome is recorded, not a record of money that moved. The
// money movements are the ledger entries, and those are append-only.
type Withdrawal struct {
	ID int64
	// Key is the account the money comes from.
	Key UserKey
	// IdemKey is the caller's identifier for this request, unique within a
	// tenant. A retry with the same key returns the same withdrawal rather than
	// reserving the money twice (see ADR-043).
	IdemKey string
	// Amount is the gross amount taken from the withdrawable balance. This is
	// the figure the account moves by, fee included, because the fee leaves the
	// agent's account whether or not the agent receives it.
	Amount Money
	// Fee is the platform's cut, defaulting to zero.
	//
	// It is recorded for reporting rather than moved anywhere: the fee is
	// revenue belonging outside this ledger, so the agent's account loses
	// Amount and the ledger stays conserved. Paying out RealAmount while moving
	// Amount is exactly right, and moving anything else would break I1.
	Fee Money
	// RealAmount is what the destination actually receives: Amount - Fee.
	RealAmount Money
	// Channel names the payout route, for example "manual" or "mock".
	Channel string
	// AccountInfo is where the money goes, masked by the caller. The library
	// never sees an unmasked account number, and stores what it is given.
	AccountInfo string
	State       WithdrawalState
	// FailReason is why a payout did not succeed, kept for the retry decision.
	FailReason string
	// TaxAmount is reserved for withholding tax. It is not implemented, and it
	// is not configurable, so it never holds a non-zero value.
	//
	// It exists because the alternative was a shell method that accepted a
	// value and ignored it, which this project does not ship (ADR-022). A field
	// that is always zero is honest about being unimplemented; a setter that
	// silently drops its argument is not.
	TaxAmount Money
	// InvoiceNo is the payout provider's reference, recorded once the payout
	// succeeds.
	InvoiceNo string
	// Operator records who approved or paid it, for audit.
	Operator string
	// AppliedAt is when the agent asked. Processing time comes from the Clock,
	// never from the caller (ADR-023).
	AppliedAt time.Time
	// AuditedAt is when the request was approved or rejected.
	AuditedAt time.Time
	// PaidAt is when the money left. Required to be non-zero in the Paid state
	// (ADR-018).
	PaidAt  time.Time
	Version int64
}

// Validate checks that a withdrawal is internally consistent before it is
// written.
func (w Withdrawal) Validate() error {
	if err := w.Key.Validate(); err != nil {
		return err
	}
	if w.Amount <= 0 {
		return fieldErrf("amount", "must be positive, got %s", w.Amount)
	}
	if w.Fee < 0 {
		return fieldErrf("fee", "must not be negative, got %s", w.Fee)
	}
	if w.Fee >= w.Amount {
		// A fee that swallows the whole amount means the agent pays to
		// withdraw. That is a configuration error rather than a payout.
		return fieldErrf("fee", "must be less than the amount, got %s of %s", w.Fee, w.Amount)
	}
	if w.RealAmount != w.Amount-w.Fee {
		return fieldErrf("real_amount",
			"must equal amount minus fee (%s), got %s", w.Amount-w.Fee, w.RealAmount)
	}
	if !w.State.Valid() {
		return fieldErrf("state", "unknown withdrawal state %d", uint8(w.State))
	}
	if w.State == WithdrawalPaid && w.PaidAt.IsZero() {
		return fieldErrf("paid_at", "a paid withdrawal must record when it was paid")
	}
	if w.State != WithdrawalPaid && !w.PaidAt.IsZero() {
		return fieldErrf("paid_at",
			"only a paid withdrawal may record a payout time, state is %s", w.State)
	}
	if w.IdemKey == "" {
		return fieldErrf("idem_key", "must not be empty")
	}
	return nil
}

// Outstanding returns the money this withdrawal still holds in the reserved
// bucket: the full amount while it is reserved, nothing once it is paid or
// rejected.
func (w Withdrawal) Outstanding() Money {
	if w.State.Reserved() {
		return w.Amount
	}
	return 0
}
