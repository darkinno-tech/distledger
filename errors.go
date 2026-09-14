package distledger

import (
	"errors"
	"fmt"
)

// Sentinel errors. Callers must always test them with errors.Is rather than
// comparing error strings.
//
// Design principle: an error carries only input supplied by the caller and never
// leaks internal implementation details or data belonging to other tenants or
// users (see PRD §15, security considerations).
var (
	// ErrInvalidArgument reports an invalid argument from the caller, such as a
	// missing required field or an out-of-range value.
	ErrInvalidArgument = errors.New("distledger: invalid argument")

	// ErrNotFound reports that the requested object does not exist.
	ErrNotFound = errors.New("distledger: not found")

	// ErrDuplicate reports an idempotency key conflict: the same ledger action has
	// already been posted.
	//
	// This is not a failure state — it is the sign that the idempotency guarantee
	// is working. Entry points such as OnOrderPaid turn a duplicate delivery into
	// a normal return of "success with no side effects".
	ErrDuplicate = errors.New("distledger: duplicate idempotency key")

	// ErrNoPayoutChannel reports that automatic payout was attempted without a
	// channel configured.
	//
	// It is its own error rather than a generic refusal because the fix is a
	// configuration step, not a retry: a caller that retried would wait forever.
	ErrNoPayoutChannel = errors.New("distledger: no payout channel configured")

	// ErrIllegalTransition reports an illegal state transition.
	//
	// The library never guesses at intent: an illegal transition is always an error,
	// with no best-effort correction.
	ErrIllegalTransition = errors.New("distledger: illegal state transition")

	// ErrConflict reports an optimistic locking version conflict: the object was
	// modified concurrently after it was read.
	ErrConflict = errors.New("distledger: concurrent modification")

	// ErrCycle reports that a parent-child relation forms a cycle.
	ErrCycle = errors.New("distledger: relation cycle detected")

	// ErrDepthExceeded reports that the hierarchy depth exceeds the configured limit.
	ErrDepthExceeded = errors.New("distledger: relation depth exceeded")

	// ErrParentAlreadySet reports that the agent's parent is already set and differs
	// from the one in this request.
	//
	// This guard is deliberate: silently reassigning a parent would quietly rewrite
	// the ownership of the entire downstream tree.
	ErrParentAlreadySet = errors.New("distledger: parent already set")

	// ErrInvalidConfig reports an inconsistent configuration.
	ErrInvalidConfig = errors.New("distledger: invalid config")

	// ErrInsufficientBalance reports that a funds bucket does not hold enough balance
	// to complete a reversal.
	//
	// It fires when a reversal needs money the account no longer holds, which is
	// what happens once a commission can be paid out before its order is refunded.
	// Pushing the bucket negative would break I1, so the answer is the debt policy:
	// either refuse and name the shortfall, or record the debt deliberately
	// (ADR-042). Either way it is never silent.
	ErrInsufficientBalance = errors.New("distledger: insufficient balance for reversal")

	// ErrOverflow reports an amount arithmetic overflow. Seeing it means the input
	// scale is beyond the designed bounds.
	ErrOverflow = errors.New("distledger: amount overflow")

	// ErrClosed reports that the Ledger has been closed.
	ErrClosed = errors.New("distledger: ledger closed")

	// ErrBindingLocked reports that the buyer is already bound to another agent and
	// that the binding is still in effect at attribution time.
	//
	// It exists to prevent poaching: without this gate, anyone could see an order
	// and then bind themselves as the buyer's promoter. When BindBuyer returns
	// this error it **also returns the existing binding**, which the caller can
	// use directly.
	ErrBindingLocked = errors.New("distledger: buyer already bound to another agent")
)

// FieldError describes a field validation failure and wraps ErrInvalidArgument.
type FieldError struct {
	Field  string
	Reason string
}

func (e *FieldError) Error() string {
	return fmt.Sprintf("distledger: invalid %s: %s", e.Field, e.Reason)
}

// Unwrap makes errors.Is(err, ErrInvalidArgument) true.
func (e *FieldError) Unwrap() error { return ErrInvalidArgument }

func fieldErr(field, reason string) error {
	return &FieldError{Field: field, Reason: reason}
}

func fieldErrf(field, format string, args ...any) error {
	return &FieldError{Field: field, Reason: fmt.Sprintf(format, args...)}
}

// TransitionError describes an illegal state transition and wraps
// ErrIllegalTransition.
type TransitionError struct {
	Kind string
	From string
	To   string
}

func (e *TransitionError) Error() string {
	return fmt.Sprintf("distledger: illegal %s transition: %s -> %s", e.Kind, e.From, e.To)
}

// Unwrap makes errors.Is(err, ErrIllegalTransition) true.
func (e *TransitionError) Unwrap() error { return ErrIllegalTransition }

// ConflictError describes an optimistic locking conflict and wraps ErrConflict.
type ConflictError struct {
	Kind     string
	ID       any
	Expected int64
	Actual   int64
}

func (e *ConflictError) Error() string {
	return fmt.Sprintf("distledger: %s %v modified concurrently (expected version %d, actual %d)",
		e.Kind, e.ID, e.Expected, e.Actual)
}

// Unwrap makes errors.Is(err, ErrConflict) true.
func (e *ConflictError) Unwrap() error { return ErrConflict }
