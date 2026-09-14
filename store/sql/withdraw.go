package sqlstore

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/darkinno-tech/distledger"
)

// This file holds the withdrawal half of the portable SQL store.
//
// Two things are worth knowing before reading it.
//
// Placeholders are numbered per statement on PostgreSQL, so every query here
// builds its arguments through a single builder. Two builders would produce two
// $1s and a parameter-count error, which is a mistake this codebase has already
// made once in the reconciliation query.
//
// Timestamps arrive from the caller in the transition rather than being read
// from a clock here, so that a ledger's timestamps do not depend on which
// backend was underneath (ADR-023).

func (t *tx) withdrawalCols() string {
	return t.cols("id", "tenant_id", "user_id", "idem_key", "amount", "fee",
		"real_amount", "channel", "account_info", "state", "fail_reason",
		"tax_amount", "invoice_no", "operator", "applied_at", "audited_at",
		"paid_at", "version")
}

func scanWithdrawal(rows *sql.Rows) (distledger.Withdrawal, error) {
	var (
		w                      distledger.Withdrawal
		applied, audited, paid sql.NullInt64
		state                  int64
	)
	err := rows.Scan(
		&w.ID, &w.Key.TenantID, &w.Key.UserID, &w.IdemKey, &w.Amount, &w.Fee,
		&w.RealAmount, &w.Channel, &w.AccountInfo, &state, &w.FailReason,
		&w.TaxAmount, &w.InvoiceNo, &w.Operator, &applied, &audited, &paid,
		&w.Version,
	)
	if err != nil {
		return w, err
	}
	w.State = distledger.WithdrawalState(state)
	w.AppliedAt = nullInstant(applied)
	w.AuditedAt = nullInstant(audited)
	w.PaidAt = nullInstant(paid)
	return w, nil
}

func (t *tx) WithdrawalByIdemKey(
	ctx context.Context, tenantID int64, idemKey string,
) (distledger.Withdrawal, error) {
	b := t.b()
	query := "SELECT " + t.withdrawalCols() + " FROM " + t.table(tableWithdraw) +
		" WHERE " + t.cols("tenant_id") + " = " + b.add(tenantID) +
		" AND " + t.cols("idem_key") + " = " + b.add(idemKey)

	return t.oneWithdrawal(ctx, query, b.vals)
}

func (t *tx) WithdrawalByID(ctx context.Context, id int64) (distledger.Withdrawal, error) {
	b := t.b()
	query := "SELECT " + t.withdrawalCols() + " FROM " + t.table(tableWithdraw) +
		" WHERE " + t.cols("id") + " = " + b.add(id)

	return t.oneWithdrawal(ctx, query, b.vals)
}

// oneWithdrawal runs a query expected to match at most one row, mapping "no
// rows" to ErrNotFound rather than to a zero record.
//
// A zero withdrawal and a missing one would otherwise be indistinguishable, and
// the zero value has state Applied with version 0 - so a caller would see a
// plausible-looking record that does not exist.
func (t *tx) oneWithdrawal(ctx context.Context, query string, argv []any) (distledger.Withdrawal, error) {
	var (
		out distledger.Withdrawal
		got bool
	)
	err := t.queryRows(ctx, query, argv, func(rows *sql.Rows) error {
		w, err := scanWithdrawal(rows)
		if err != nil {
			return err
		}
		out = w
		got = true
		return nil
	})
	if err != nil {
		return out, err
	}
	if !got {
		return out, distledger.ErrNotFound
	}
	return out, nil
}

func (t *tx) WithdrawalsByTenant(
	ctx context.Context, tenantID int64, p distledger.Page,
) ([]distledger.Withdrawal, error) {
	b := t.b()
	query := "SELECT " + t.withdrawalCols() + " FROM " + t.table(tableWithdraw) +
		" WHERE " + t.cols("tenant_id") + " = " + b.add(tenantID) +
		" AND " + t.cols("id") + " > " + b.add(p.AfterID) +
		" ORDER BY " + t.cols("id") +
		" LIMIT " + b.add(int64(p.LimitOrDefault()))

	out := make([]distledger.Withdrawal, 0, p.LimitOrDefault())
	err := t.queryRows(ctx, query, b.vals, func(rows *sql.Rows) error {
		w, err := scanWithdrawal(rows)
		if err != nil {
			return err
		}
		out = append(out, w)
		return nil
	})
	return out, err
}

func (t *tx) CountWithdrawalsByState(
	ctx context.Context, tenantID int64,
) (map[distledger.WithdrawalState]int64, error) {
	b := t.b()
	query := "SELECT " + t.cols("state") + ", COUNT(*) FROM " + t.table(tableWithdraw) +
		" WHERE " + t.cols("tenant_id") + " = " + b.add(tenantID) +
		" GROUP BY " + t.cols("state")

	out := make(map[distledger.WithdrawalState]int64, len(distledger.AllWithdrawalStates()))
	err := t.queryRows(ctx, query, b.vals, func(rows *sql.Rows) error {
		var (
			state int64
			n     int64
		)
		if err := rows.Scan(&state, &n); err != nil {
			return err
		}
		out[distledger.WithdrawalState(state)] = n
		return nil
	})
	return out, err
}

func (t *tx) AppendWithdrawal(
	ctx context.Context, w distledger.Withdrawal,
) (distledger.Withdrawal, error) {
	if err := w.Validate(); err != nil {
		return w, err
	}

	b := t.b()
	query := "INSERT INTO " + t.table(tableWithdraw) + " (" +
		t.cols("tenant_id", "user_id", "idem_key", "amount", "fee", "real_amount",
			"channel", "account_info", "state", "fail_reason", "tax_amount",
			"invoice_no", "operator", "applied_at", "audited_at", "paid_at", "version") +
		") VALUES (" +
		b.add(w.Key.TenantID) + ", " + b.add(w.Key.UserID) + ", " + b.add(w.IdemKey) + ", " +
		b.add(w.Amount) + ", " + b.add(w.Fee) + ", " + b.add(w.RealAmount) + ", " +
		b.add(w.Channel) + ", " +
		// account_info is NOT NULL: the empty string means "no destination
		// recorded yet", which is a value rather than an absence.
		b.add(w.AccountInfo) + ", " +
		b.add(int64(w.State)) + ", " + b.add(w.FailReason) + ", " + b.add(w.TaxAmount) + ", " +
		b.add(w.InvoiceNo) + ", " + b.add(w.Operator) + ", " +
		b.add(instantOrNull(w.AppliedAt)) + ", " + b.add(instantOrNull(w.AuditedAt)) + ", " +
		b.add(instantOrNull(w.PaidAt)) + ", " + b.add(int64(1)) + ")"

	id, inserted, err := t.insertReturningID(ctx, query, b.vals)
	if err != nil {
		return w, fmt.Errorf("sqlstore: insert withdrawal: %w", err)
	}
	if !inserted {
		// The idempotency key already exists: this is a retry, not a failure.
		// The caller reads the original back by its own key.
		return w, distledger.ErrDuplicate
	}
	w.ID = id
	w.Version = 1
	return w, nil
}

func (t *tx) TransitionWithdrawal(
	ctx context.Context,
	id int64,
	from, to distledger.WithdrawalState,
	expectedVersion int64,
	tr distledger.WithdrawalTransition,
) (distledger.Withdrawal, error) {
	if !from.Valid() || !to.Valid() {
		return distledger.Withdrawal{}, &distledger.FieldError{
			Field: "state", Reason: fmt.Sprintf("unknown transition %d -> %d", uint8(from), uint8(to)),
		}
	}

	current, err := t.WithdrawalByID(ctx, id)
	if err != nil {
		return distledger.Withdrawal{}, err
	}

	// The two failure kinds are reported distinctly and in this order, matching
	// the commission path: a stale view of a legal edge is retryable, an illegal
	// edge is a caller bug. Reporting a caller bug as retryable would make the
	// caller retry forever; reporting a race as a caller bug would hide it.
	if current.State != from || current.Version != expectedVersion {
		return distledger.Withdrawal{}, &distledger.ConflictError{
			Kind: "withdrawal", ID: fmt.Sprint(id),
			Expected: expectedVersion, Actual: current.Version,
		}
	}
	if !distledger.CanTransitionWithdrawal(from, to) {
		return distledger.Withdrawal{}, &distledger.TransitionError{
			Kind: "withdrawal", From: from.String(), To: to.String(),
		}
	}

	b := t.b()
	query := "UPDATE " + t.table(tableWithdraw) + " SET " +
		t.cols("state") + " = " + b.add(int64(to)) + ", " +
		t.cols("version") + " = " + t.cols("version") + " + 1"

	// Only the fields the transition actually carries are written, so that an
	// approval does not erase a failure reason recorded by an earlier attempt.
	if tr.FailReason != "" {
		query += ", " + t.cols("fail_reason") + " = " + b.add(tr.FailReason)
	}
	if tr.InvoiceNo != "" {
		query += ", " + t.cols("invoice_no") + " = " + b.add(tr.InvoiceNo)
	}
	if tr.Operator != "" {
		query += ", " + t.cols("operator") + " = " + b.add(tr.Operator)
	}
	if !tr.AuditedAt.IsZero() {
		query += ", " + t.cols("audited_at") + " = " + b.add(instantOrNull(tr.AuditedAt))
	}
	if !tr.PaidAt.IsZero() {
		query += ", " + t.cols("paid_at") + " = " + b.add(instantOrNull(tr.PaidAt))
	}

	query += " WHERE " + t.cols("id") + " = " + b.add(id) +
		" AND " + t.cols("state") + " = " + b.add(int64(from)) +
		" AND " + t.cols("version") + " = " + b.add(expectedVersion)

	res, err := t.updateVersioned(ctx, query, b.vals)
	if err != nil {
		return distledger.Withdrawal{}, err
	}
	if res.affected != 1 {
		// Something moved between the read and the write.
		return distledger.Withdrawal{}, &distledger.ConflictError{
			Kind: "withdrawal", ID: fmt.Sprint(id),
			Expected: expectedVersion, Actual: expectedVersion + 1,
		}
	}
	return t.WithdrawalByID(ctx, id)
}
