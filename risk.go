package distledger

import (
	"context"
	"errors"
	"time"
)

// Reasons attached to a clawback. They end up in the ledger remark so that a
// reviewer can tell a refund-driven reversal from a risk-control forfeiture
// without joining back to the commission records.
const (
	reasonRefund = "refund"
	reasonVoid   = "void"
)

// maxReasonLen bounds the operator-supplied reason string.
//
// It is bounded because the reason is copied into the ledger remark: an
// unbounded string would let a caller inflate the ledger at will.
const maxReasonLen = 128

// FreezeCommission moves a pending commission into risk-control freeze.
//
// # What freezing means
//
// The commission keeps its money in the account's Frozen bucket — nothing moves
// on the books — but it stops being eligible for settlement. A frozen
// commission can later be released back to Pending with UnfreezeCommission, or
// forfeited with VoidCommission, or reversed outright if the order is refunded.
//
// This is the entry point that makes the Frozen state reachable. Without it the
// state existed only in the transition table, which is exactly the kind of
// "declared but unreachable" surface this project refuses to ship.
func (l *Ledger) FreezeCommission(ctx context.Context, tenantID, commissionID int64, reason string) (Commission, error) {
	if err := validateReason(reason); err != nil {
		return Commission{}, err
	}
	var out Commission
	err := l.store.Update(ctx, func(ctx context.Context, tx Tx) error {
		cur, err := l.loadScopedCommission(ctx, tx, tenantID, commissionID)
		if err != nil {
			return err
		}
		if cur.State != CommissionPending {
			return &TransitionError{
				Kind: "commission freeze", From: cur.State.String(), To: CommissionFrozen.String(),
			}
		}
		updated, err := tx.TransitionCommission(ctx, cur.ID, CommissionPending, CommissionFrozen, cur.Version)
		if err != nil {
			return err
		}
		out = updated
		return nil
	})
	if err != nil {
		return Commission{}, err
	}
	l.log.InfoContext(ctx, "commission frozen",
		"commission_id", out.ID, "agent_user_id", out.AgentUserID,
		"amount", out.Amount.String(), "reason", reason)
	return out, nil
}

// UnfreezeCommission releases a frozen commission back to Pending.
//
// It is first-wins in the sense that it only acts on a commission that is
// currently frozen; anything else is a conflict, not a silent success.
func (l *Ledger) UnfreezeCommission(ctx context.Context, tenantID, commissionID int64, reason string) (Commission, error) {
	if err := validateReason(reason); err != nil {
		return Commission{}, err
	}
	var out Commission
	err := l.store.Update(ctx, func(ctx context.Context, tx Tx) error {
		cur, err := l.loadScopedCommission(ctx, tx, tenantID, commissionID)
		if err != nil {
			return err
		}
		if cur.State != CommissionFrozen {
			return &TransitionError{
				Kind: "commission unfreeze", From: cur.State.String(), To: CommissionPending.String(),
			}
		}
		updated, err := tx.TransitionCommission(ctx, cur.ID, CommissionFrozen, CommissionPending, cur.Version)
		if err != nil {
			return err
		}
		out = updated
		return nil
	})
	if err != nil {
		return Commission{}, err
	}
	l.log.InfoContext(ctx, "commission unfrozen",
		"commission_id", out.ID, "agent_user_id", out.AgentUserID, "reason", reason)
	return out, nil
}

// VoidCommission forfeits a commission: the outstanding amount is clawed back
// from the account and the commission lands in the terminal Void state.
//
// # How this differs from a refund reversal
//
// The money movement is identical — the outstanding amount leaves the Frozen
// bucket and the agent's TotalReversed grows — but the reason differs and the
// terminal state is Void rather than Reversed. Accounting therefore stays
// reconciled while reporting can still tell "the customer got their money back"
// apart from "the platform decided this commission should never have existed".
//
// Void is allowed from Pending and from Frozen. A settled commission is voided
// through the ordinary refund path instead, because money that has already
// reached the withdrawable bucket must be accounted for as a clawback, not as a
// never-earned accrual.
func (l *Ledger) VoidCommission(ctx context.Context, tenantID, commissionID int64, reason string) (Commission, error) {
	if err := validateReason(reason); err != nil {
		return Commission{}, err
	}
	at := l.clock.Now()
	var out Commission
	err := l.store.Update(ctx, func(ctx context.Context, tx Tx) error {
		cur, err := l.loadScopedCommission(ctx, tx, tenantID, commissionID)
		if err != nil {
			return err
		}
		switch cur.State {
		case CommissionPending, CommissionFrozen:
		default:
			return &TransitionError{
				Kind: "commission void", From: cur.State.String(), To: CommissionVoid.String(),
			}
		}
		if cur.OutstandingAmount() <= 0 {
			// Nothing left to forfeit: report the current record instead of
			// writing a zero-amount reversal record.
			out = cur
			return nil
		}

		delta, target, skip := l.reversalDelta(cur, 0, 1, CommissionVoid)
		if skip != nil {
			out = cur
			return nil
		}
		reversal, err := l.clawBack(ctx, tx, cur, delta, target, CommissionVoid, reasonVoid, at)
		if err != nil {
			if errors.Is(err, ErrDuplicate) {
				out = cur
				return nil
			}
			return err
		}
		_ = reversal
		out, err = tx.Commission(ctx, cur.ID)
		return err
	})
	if err != nil {
		return Commission{}, err
	}
	l.log.WarnContext(ctx, "commission voided",
		"commission_id", out.ID, "agent_user_id", out.AgentUserID,
		"reversed_amount", out.ReversedAmount.String(), "reason", reason)
	return out, nil
}

// loadScopedCommission loads a commission and verifies it belongs to the given
// tenant.
//
// The tenant check is what keeps an operator API from becoming a cross-tenant
// read: commission IDs are global, so an ID alone is not an authorisation.
func (l *Ledger) loadScopedCommission(ctx context.Context, tx Tx, tenantID, commissionID int64) (Commission, error) {
	if tenantID < 0 {
		return Commission{}, fieldErrf("tenant_id", "must be >= 0, got %d", tenantID)
	}
	if commissionID <= 0 {
		return Commission{}, fieldErrf("commission_id", "must be > 0, got %d", commissionID)
	}
	cur, err := tx.Commission(ctx, commissionID)
	if err != nil {
		return Commission{}, err
	}
	if cur.Key.TenantID != tenantID {
		// Deliberately indistinguishable from "not found": revealing that an
		// ID exists in another tenant is itself a leak.
		return Commission{}, ErrNotFound
	}
	return cur, nil
}

func validateReason(reason string) error {
	if len(reason) > maxReasonLen {
		return fieldErrf("reason", "must be at most %d bytes, got %d", maxReasonLen, len(reason))
	}
	return nil
}

var _ = time.Time{}
