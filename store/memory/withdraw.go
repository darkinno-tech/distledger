package memory

import (
	"context"
	"strconv"

	"github.com/darkinno-tech/distledger"
)

// This file holds the withdrawal half of the in-memory store.
//
// It follows the same two rules as the rest of this package: every mutation is
// recorded in the undo log before it happens, so a rolled-back transaction
// leaves nothing behind, and every read validates what it returns rather than
// trusting a derived index.

// ── Reads ───────────────────────────────────────────────────────────────

// WithdrawalByIdemKey implements distledger.Reader.
func (r *reader) WithdrawalByIdemKey(
	ctx context.Context, tenantID int64, idemKey string,
) (distledger.Withdrawal, error) {
	if err := ctx.Err(); err != nil {
		return distledger.Withdrawal{}, err
	}
	if idemKey == "" {
		return distledger.Withdrawal{}, fieldErr("idem_key", "must not be empty")
	}
	id, ok := r.d.withdrawIdem[idemRef{tenantID: tenantID, key: idemKey}]
	if !ok {
		return distledger.Withdrawal{}, distledger.ErrNotFound
	}
	w, ok := r.d.withdrawals[id]
	if !ok {
		return distledger.Withdrawal{}, distledger.ErrNotFound
	}
	return w, nil
}

// WithdrawalByID implements distledger.Reader.
func (r *reader) WithdrawalByID(ctx context.Context, id int64) (distledger.Withdrawal, error) {
	if err := ctx.Err(); err != nil {
		return distledger.Withdrawal{}, err
	}
	w, ok := r.d.withdrawals[id]
	if !ok {
		return distledger.Withdrawal{}, distledger.ErrNotFound
	}
	return w, nil
}

// WithdrawalsByTenant implements distledger.Reader.
//
// The scan walks the global ascending id index and filters by tenant, which
// keeps the page order stable and keyset pagination correct without a second
// index per tenant. Withdrawals are far rarer than ledger entries, so the
// filtering costs little; if that stops being true, this is the place to add a
// per-tenant index the way commissions have one.
func (r *reader) WithdrawalsByTenant(
	ctx context.Context, tenantID int64, p distledger.Page,
) ([]distledger.Withdrawal, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if tenantID < 0 {
		return nil, fieldErrf("tenant_id", "must be >= 0, got %d", tenantID)
	}
	limit := p.LimitOrDefault()

	out := make([]distledger.Withdrawal, 0, limit)
	for _, id := range r.d.withdrawalIDs {
		if id <= p.AfterID {
			continue
		}
		w, ok := r.d.withdrawals[id]
		if !ok || w.Key.TenantID != tenantID {
			continue
		}
		out = append(out, w)
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

// CountWithdrawalsByState implements distledger.Reader.
func (r *reader) CountWithdrawalsByState(
	ctx context.Context, tenantID int64,
) (map[distledger.WithdrawalState]int64, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	out := make(map[distledger.WithdrawalState]int64, len(distledger.AllWithdrawalStates()))
	for _, id := range r.d.withdrawalIDs {
		w, ok := r.d.withdrawals[id]
		if !ok || w.Key.TenantID != tenantID {
			continue
		}
		out[w.State]++
	}
	return out, nil
}

// ── Writes ──────────────────────────────────────────────────────────────

// AppendWithdrawal implements distledger.Tx.
func (t *tx) AppendWithdrawal(ctx context.Context, w distledger.Withdrawal) (distledger.Withdrawal, error) {
	if err := ctx.Err(); err != nil {
		return w, err
	}
	if err := w.Validate(); err != nil {
		return w, err
	}

	d := t.reader.d
	ref := idemRef{tenantID: w.Key.TenantID, key: w.IdemKey}
	if _, dup := d.withdrawIdem[ref]; dup {
		// Idempotency hit. The caller should treat this as "already requested"
		// and read the existing record back by its own key, rather than as a
		// failure - a retried request must not reserve the money twice.
		return w, distledger.ErrDuplicate
	}

	d.nextID++
	w.ID = d.nextID

	saveWithdrawIdem(t.u, d, ref)
	d.withdrawIdem[ref] = w.ID
	saveWithdrawal(t.u, d, w.ID)
	d.withdrawals[w.ID] = w
	d.withdrawalIDs = append(d.withdrawalIDs, w.ID)
	return w, nil
}

// TransitionWithdrawal implements distledger.Tx.
func (t *tx) TransitionWithdrawal(
	ctx context.Context,
	id int64,
	from, to distledger.WithdrawalState,
	expectedVersion int64,
	tr distledger.WithdrawalTransition,
) (distledger.Withdrawal, error) {
	if err := ctx.Err(); err != nil {
		return distledger.Withdrawal{}, err
	}
	if !from.Valid() || !to.Valid() {
		return distledger.Withdrawal{}, fieldErrf("state",
			"unknown withdrawal state transition %d -> %d", uint8(from), uint8(to))
	}

	d := t.reader.d
	w, ok := d.withdrawals[id]
	if !ok {
		return distledger.Withdrawal{}, distledger.ErrNotFound
	}

	// Legality is checked **after** the row is found and before anything is
	// written, so that the two failure kinds stay distinguishable: a version or
	// state mismatch is a retryable race (ErrConflict), an illegal edge is a
	// caller bug (ErrIllegalTransition). Reporting the wrong one either makes a
	// caller retry forever or hides a logic error behind a retry.
	if w.State != from {
		return distledger.Withdrawal{}, &distledger.ConflictError{
			Kind: "withdrawal", ID: strconv.FormatInt(id, 10),
			Expected: int64(from), Actual: int64(w.State),
		}
	}
	if w.Version != expectedVersion {
		return distledger.Withdrawal{}, &distledger.ConflictError{
			Kind: "withdrawal", ID: strconv.FormatInt(id, 10),
			Expected: expectedVersion, Actual: w.Version,
		}
	}
	if !distledger.CanTransitionWithdrawal(from, to) {
		return distledger.Withdrawal{}, &distledger.TransitionError{
			Kind: "withdrawal", From: from.String(), To: to.String(),
		}
	}

	saveWithdrawal(t.u, d, id)
	w.State = to
	w.Version++
	if tr.FailReason != "" {
		w.FailReason = tr.FailReason
	}
	if tr.InvoiceNo != "" {
		w.InvoiceNo = tr.InvoiceNo
	}
	if tr.Operator != "" {
		w.Operator = tr.Operator
	}
	if !tr.AuditedAt.IsZero() {
		w.AuditedAt = tr.AuditedAt
	}
	if !tr.PaidAt.IsZero() {
		w.PaidAt = tr.PaidAt
	}
	d.withdrawals[id] = w
	return w, nil
}
