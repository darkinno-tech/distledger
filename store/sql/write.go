package sqlstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/im10furry/distledger"
)

// This file holds the write half of the port.
//
// # One write pattern, repeated
//
// Every Put* method follows the same shape:
//
//	UPDATE ... SET ..., version = version + 1 WHERE <key> AND version = ?
//	if no row changed:
//	    SELECT the current version
//	    row missing  -> insert it, and a duplicate key means somebody raced us
//	    version differs -> conflict
//
// Two details make it work. First, bumping version unconditionally guarantees
// the statement always changes the row, so "zero rows affected" can only mean
// "no such row or wrong version" - MySQL reports zero affected rows for an
// update that changes nothing, which would otherwise be indistinguishable.
//
// Second, the follow-up SELECT runs inside the same transaction, so the answer
// it gives cannot go stale before the caller retries.

// writeResult reports what a conditional update did.
type writeResult struct {
	affected int64
}

// updateVersioned runs a version-checked UPDATE and reports how many rows it hit.
func (t *tx) updateVersioned(ctx context.Context, query string, argv []any) (writeResult, error) {
	res, err := t.sqlTx.ExecContext(ctx, query, argv...)
	if err != nil {
		return writeResult{}, fmt.Errorf("sqlstore: update: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return writeResult{}, fmt.Errorf("sqlstore: rows affected: %w", err)
	}
	return writeResult{affected: n}, nil
}

// insertReturningID inserts a row, reporting the assigned id and whether the row
// was actually written.
//
// Three combinations of dialect behaviour, all of them real:
//
//   - PostgreSQL pairs RETURNING with ON CONFLICT DO NOTHING, so a collision
//     yields no row and surfaces as sql.ErrNoRows.
//   - SQLite has RETURNING but does not need the clause: a duplicate raises an
//     error, and its transaction survives that.
//   - MySQL has no RETURNING at all, so the id comes from LastInsertId and a
//     duplicate raises an error like SQLite's.
func (t *tx) insertReturningID(ctx context.Context, query string, argv []any) (int64, bool, error) {
	query += t.s.dialect.InsertConflictClause()

	if t.s.dialect.InsertID() == InsertIDReturning {
		var id int64
		err := t.sqlTx.QueryRowContext(ctx, query+" RETURNING "+t.s.dialect.Quote("id"), argv...).Scan(&id)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			// The conflict clause absorbed the row.
			return 0, false, nil
		case err != nil:
			if t.s.classifier.IsDuplicateKey(err) {
				return 0, false, nil
			}
			return 0, false, fmt.Errorf("sqlstore: insert: %w", err)
		}
		return id, true, nil
	}

	res, err := t.sqlTx.ExecContext(ctx, query, argv...)
	if err != nil {
		if t.s.classifier.IsDuplicateKey(err) {
			return 0, false, nil
		}
		return 0, false, fmt.Errorf("sqlstore: insert: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, false, fmt.Errorf("sqlstore: rows affected: %w", err)
	}
	if n == 0 {
		return 0, false, nil
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, false, fmt.Errorf("sqlstore: last insert id: %w", err)
	}
	return id, true, nil
}

// execInsert runs an INSERT with no generated id to read back.
//
// Only the generated-key tables use this; the colliding inserts are the ones that
// need the id, and they go through insertReturningID.
func (t *tx) execInsert(ctx context.Context, query string, argv []any) (bool, error) {
	query += t.s.dialect.InsertConflictClause()
	res, err := t.sqlTx.ExecContext(ctx, query, argv...)
	if err != nil {
		if t.s.classifier.IsDuplicateKey(err) {
			return false, nil
		}
		return false, fmt.Errorf("sqlstore: insert: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("sqlstore: rows affected: %w", err)
	}
	return n > 0, nil
}

// conflict builds the error for a version mismatch, in the same shape the
// in-memory store produces so callers cannot tell the backends apart.
func conflict(kind string, id any, expected, actual int64) error {
	return &distledger.ConflictError{Kind: kind, ID: id, Expected: expected, Actual: actual}
}

// ── Agent ───────────────────────────────────────────────────────────────

func (t *tx) PutAgent(ctx context.Context, a distledger.Agent) (distledger.Agent, error) {
	if err := a.Key.Validate(); err != nil {
		return a, err
	}
	if a.ParentID < 0 {
		return a, &distledger.FieldError{Field: "parent_id", Reason: "must be >= 0"}
	}
	if a.ParentID != 0 && a.ParentID == a.Key.UserID {
		return a, &distledger.FieldError{Field: "parent_id", Reason: "must not equal user_id"}
	}
	if !a.Status.Valid() {
		return a, &distledger.FieldError{Field: "status", Reason: "unknown agent status"}
	}
	if !a.JoinType.Valid() {
		return a, &distledger.FieldError{Field: "join_type", Reason: "unknown join type"}
	}

	var rateOverride any
	if a.RateOverrideBP != nil {
		rateOverride = int64(*a.RateOverrideBP)
	}

	b := t.b()
	update := "UPDATE " + t.table("dist_agent") + " SET " +
		t.cols("parent_id") + " = " + b.add(a.ParentID) + ", " +
		t.cols("depth") + " = " + b.add(int64(a.Depth)) + ", " +
		t.cols("status") + " = " + b.add(int64(a.Status)) + ", " +
		t.cols("join_type") + " = " + b.add(int64(a.JoinType)) + ", " +
		t.cols("rate_override_bp") + " = " + b.add(rateOverride) + ", " +
		t.cols("created_at") + " = " + b.add(instantOrNull(a.CreatedAt)) + ", " +
		t.cols("updated_at") + " = " + b.add(instantOrNull(a.UpdatedAt)) + ", " +
		t.cols("version") + " = " + t.cols("version") + " + 1" +
		" WHERE " + t.cols("tenant_id") + " = " + b.add(a.Key.TenantID) +
		" AND " + t.cols("user_id") + " = " + b.add(a.Key.UserID) +
		" AND " + t.cols("version") + " = " + b.add(a.Version)

	res, err := t.updateVersioned(ctx, update, b.vals)
	if err != nil {
		return a, err
	}
	if res.affected == 1 {
		a.Version++
		return a, nil
	}

	cur, err := t.Agent(ctx, a.Key)
	if err == nil {
		// The row is there with a different version.
		return a, conflict("agent", a.Key.String(), a.Version, cur.Version)
	}
	if !errors.Is(err, distledger.ErrNotFound) {
		return a, err
	}
	if a.Version != 0 {
		// The caller believed it was updating something that is not there.
		return a, conflict("agent", a.Key.String(), a.Version, 0)
	}

	ins := t.b()
	insert := "INSERT INTO " + t.table("dist_agent") + " (" + t.agentCols() + ") VALUES (" +
		ins.add(a.Key.TenantID) + ", " + ins.add(a.Key.UserID) + ", " + ins.add(a.ParentID) + ", " +
		ins.add(int64(a.Depth)) + ", " + ins.add(int64(a.Status)) + ", " + ins.add(int64(a.JoinType)) + ", " +
		ins.add(rateOverride) + ", " + ins.add(instantOrNull(a.CreatedAt)) + ", " +
		ins.add(instantOrNull(a.UpdatedAt)) + ", " + ins.add(int64(1)) + ")"
	inserted, err := t.execInsert(ctx, insert, ins.vals)
	if err != nil {
		return a, fmt.Errorf("sqlstore: insert agent: %w", err)
	}
	if !inserted {
		// Another transaction created it between our UPDATE and INSERT.
		return a, conflict("agent", a.Key.String(), 0, 0)
	}
	a.Version = 1
	return a, nil
}

// ── Binding ─────────────────────────────────────────────────────────────

func (t *tx) PutBinding(ctx context.Context, bnd distledger.Binding) (distledger.Binding, error) {
	if err := bnd.Buyer.Validate(); err != nil {
		return bnd, err
	}
	if bnd.AgentUserID <= 0 {
		return bnd, &distledger.FieldError{Field: "agent_user_id", Reason: "must be > 0"}
	}
	if !bnd.Source.Valid() {
		return bnd, &distledger.FieldError{Field: "source", Reason: "unknown bind source"}
	}

	b := t.b()
	update := "UPDATE " + t.table("dist_binding") + " SET " +
		t.cols("agent_user_id") + " = " + b.add(bnd.AgentUserID) + ", " +
		t.cols("source") + " = " + b.add(int64(bnd.Source)) + ", " +
		t.cols("source_ref") + " = " + b.add(nullableString(bnd.SourceRef)) + ", " +
		t.cols("bound_at") + " = " + b.add(instantOrNull(bnd.BoundAt)) + ", " +
		t.cols("expire_at") + " = " + b.add(instantOrNull(bnd.ExpireAt)) + ", " +
		t.cols("active") + " = " + b.add(boolInt(bnd.Active)) + ", " +
		t.cols("rebound_from") + " = " + b.add(bnd.ReboundFrom) + ", " +
		t.cols("version") + " = " + t.cols("version") + " + 1" +
		" WHERE " + t.cols("tenant_id") + " = " + b.add(bnd.Buyer.TenantID) +
		" AND " + t.cols("buyer_user_id") + " = " + b.add(bnd.Buyer.UserID) +
		" AND " + t.cols("version") + " = " + b.add(bnd.Version)

	res, err := t.updateVersioned(ctx, update, b.vals)
	if err != nil {
		return bnd, err
	}
	if res.affected == 1 {
		bnd.Version++
		return bnd, nil
	}

	cur, err := t.Binding(ctx, bnd.Buyer)
	if err == nil {
		return bnd, conflict("binding", bnd.Buyer.String(), bnd.Version, cur.Version)
	}
	if !errors.Is(err, distledger.ErrNotFound) {
		return bnd, err
	}
	if bnd.Version != 0 {
		return bnd, conflict("binding", bnd.Buyer.String(), bnd.Version, 0)
	}

	ins := t.b()
	insert := "INSERT INTO " + t.table("dist_binding") + " (" + t.bindingCols() + ") VALUES (" +
		ins.add(bnd.Buyer.TenantID) + ", " + ins.add(bnd.Buyer.UserID) + ", " +
		ins.add(bnd.AgentUserID) + ", " + ins.add(int64(bnd.Source)) + ", " +
		ins.add(nullableString(bnd.SourceRef)) + ", " + ins.add(instantOrNull(bnd.BoundAt)) + ", " +
		ins.add(instantOrNull(bnd.ExpireAt)) + ", " + ins.add(boolInt(bnd.Active)) + ", " +
		ins.add(bnd.ReboundFrom) + ", " + ins.add(int64(1)) + ")"
	inserted, err := t.execInsert(ctx, insert, ins.vals)
	if err != nil {
		return bnd, fmt.Errorf("sqlstore: insert binding: %w", err)
	}
	if !inserted {
		return bnd, conflict("binding", bnd.Buyer.String(), 0, 0)
	}
	bnd.Version = 1
	return bnd, nil
}

// ── Account ─────────────────────────────────────────────────────────────

func (t *tx) PutAccount(ctx context.Context, a distledger.Account) (distledger.Account, error) {
	if err := a.Key.Validate(); err != nil {
		return a, err
	}
	// Negative buckets are rejected before the statement is built, so a caller
	// cannot push one into the table and leave reconciliation to find it.
	if !a.Settleable() {
		for _, bkt := range distledger.AllBuckets() {
			if v := bkt.Value(a); v < 0 {
				return a, &distledger.FieldError{
					Field: "account", Reason: string(bkt) + " bucket is negative: " + v.String(),
				}
			}
		}
	}

	b := t.b()
	update := "UPDATE " + t.table("dist_account") + " SET " +
		t.cols("frozen") + " = " + b.add(a.Frozen) + ", " +
		t.cols("available") + " = " + b.add(a.Available) + ", " +
		t.cols("withdrawing") + " = " + b.add(a.Withdrawing) + ", " +
		t.cols("withdrawn") + " = " + b.add(a.Withdrawn) + ", " +
		t.cols("total_earned") + " = " + b.add(a.TotalEarned) + ", " +
		t.cols("total_reversed") + " = " + b.add(a.TotalReversed) + ", " +
		t.cols("updated_at") + " = " + b.add(instantOrNull(a.UpdatedAt)) + ", " +
		t.cols("version") + " = " + t.cols("version") + " + 1" +
		" WHERE " + t.cols("tenant_id") + " = " + b.add(a.Key.TenantID) +
		" AND " + t.cols("user_id") + " = " + b.add(a.Key.UserID) +
		" AND " + t.cols("version") + " = " + b.add(a.Version)

	res, err := t.updateVersioned(ctx, update, b.vals)
	if err != nil {
		return a, err
	}
	if res.affected == 1 {
		a.Version++
		return a, nil
	}

	// Account is the one entity whose absence is not an error, so the check is
	// "did anything come back" rather than catching ErrNotFound.
	cur, found, err := t.lookupAccount(ctx, a.Key)
	if err != nil {
		return a, err
	}
	if found {
		return a, conflict("account", a.Key.String(), a.Version, cur.Version)
	}
	if a.Version != 0 {
		return a, conflict("account", a.Key.String(), a.Version, 0)
	}

	ins := t.b()
	insert := "INSERT INTO " + t.table("dist_account") + " (" + t.accountCols() + ") VALUES (" +
		ins.add(a.Key.TenantID) + ", " + ins.add(a.Key.UserID) + ", " +
		ins.add(a.Frozen) + ", " + ins.add(a.Available) + ", " +
		ins.add(a.Withdrawing) + ", " + ins.add(a.Withdrawn) + ", " +
		ins.add(a.TotalEarned) + ", " + ins.add(a.TotalReversed) + ", " +
		ins.add(int64(1)) + ", " + ins.add(instantOrNull(a.UpdatedAt)) + ")"
	inserted, err := t.execInsert(ctx, insert, ins.vals)
	if err != nil {
		return a, fmt.Errorf("sqlstore: insert account: %w", err)
	}
	if !inserted {
		return a, conflict("account", a.Key.String(), 0, 0)
	}
	a.Version = 1
	return a, nil
}

// lookupAccount distinguishes "no account" from "account with zero balances".
//
// Account() alone cannot: the port says a missing account reads as the zero
// account, which is exactly right for callers and exactly wrong for a
// read-modify-write that needs to know whether to insert.
func (t *tx) lookupAccount(ctx context.Context, key distledger.UserKey) (distledger.Account, bool, error) {
	b := t.b()
	query := "SELECT " + t.accountCols() + " FROM " + t.table("dist_account") +
		" WHERE " + t.cols("tenant_id") + " = " + b.add(key.TenantID) +
		" AND " + t.cols("user_id") + " = " + b.add(key.UserID)

	var (
		out   distledger.Account
		found bool
	)
	err := t.queryOne(ctx, query, b.vals, func(rows *sql.Rows) error {
		a, err := scanAccount(rows)
		if err != nil {
			return err
		}
		out, found = a, true
		return nil
	})
	return out, found, err
}

// IncrementAccount applies a delta in one statement.
//
// # Why one statement and not read-modify-write
//
// Two orders attributed to the same popular agent arrive together. With a
// read-modify-write they both read the same balances, both compute new ones, and
// one of them loses the version check and has to retry the whole event. With an
// increment the database serialises them on the row lock and neither has to know
// the other exists.
//
// # The guard travels with the statement
//
// RequireNonNegative is expressed as additional predicates on the same UPDATE, so
// a change that would push a bucket below zero simply matches no row. Checking in
// Go and then writing would leave a window for another writer between the two.
func (t *tx) IncrementAccount(ctx context.Context, delta distledger.AccountDelta) (distledger.Account, error) {
	if err := delta.Key.Validate(); err != nil {
		return distledger.Account{}, err
	}
	if delta.IsZero() {
		acct, _, err := t.lookupAccount(ctx, delta.Key)
		return acct, err
	}

	// The update is attempted first, because a missing account is the rarer case
	// and a mismatch tells us which case we are in.
	b := t.b()
	update := "UPDATE " + t.table("dist_account") + " SET " +
		t.cols("frozen") + " = " + t.cols("frozen") + " + " + b.add(delta.Frozen) + ", " +
		t.cols("available") + " = " + t.cols("available") + " + " + b.add(delta.Available) + ", " +
		t.cols("withdrawing") + " = " + t.cols("withdrawing") + " + " + b.add(delta.Withdrawing) + ", " +
		t.cols("withdrawn") + " = " + t.cols("withdrawn") + " + " + b.add(delta.Withdrawn) + ", " +
		t.cols("total_earned") + " = " + t.cols("total_earned") + " + " + b.add(delta.TotalEarned) + ", " +
		t.cols("total_reversed") + " = " + t.cols("total_reversed") + " + " + b.add(delta.TotalReversed) + ", " +
		t.cols("version") + " = " + t.cols("version") + " + 1" +
		" WHERE " + t.cols("tenant_id") + " = " + b.add(delta.Key.TenantID) +
		" AND " + t.cols("user_id") + " = " + b.add(delta.Key.UserID)
	if delta.RequireNonNegative {
		// Each bucket's post-state must be non-negative. Writing it as a
		// predicate is what makes the guard part of the same atomic statement.
		update += " AND " + t.cols("frozen") + " + " + b.add(delta.Frozen) + " >= 0" +
			" AND " + t.cols("available") + " + " + b.add(delta.Available) + " >= 0" +
			" AND " + t.cols("withdrawing") + " + " + b.add(delta.Withdrawing) + " >= 0" +
			" AND " + t.cols("withdrawn") + " + " + b.add(delta.Withdrawn) + " >= 0"
	}

	res, err := t.updateVersioned(ctx, update, b.vals)
	if err != nil {
		return distledger.Account{}, err
	}
	if res.affected == 1 {
		acct, _, err := t.lookupAccount(ctx, delta.Key)
		return acct, err
	}

	_, found, err := t.lookupAccount(ctx, delta.Key)
	if err != nil {
		return distledger.Account{}, err
	}
	if found {
		// The row exists, so the guard is what rejected the update.
		return distledger.Account{}, fmt.Errorf(
			"%w: applying %s would leave a bucket of account %s negative",
			distledger.ErrInsufficientBalance, delta.Key, delta.Key)
	}

	// No account yet: create it from the delta itself.
	seed, err := delta.Apply(distledger.Account{Key: delta.Key})
	if err != nil {
		return distledger.Account{}, err
	}
	if delta.RequireNonNegative {
		if bad := seed.NegativeBuckets(); len(bad) > 0 {
			return distledger.Account{}, fmt.Errorf(
				"%w: applying %s would leave the %s bucket negative",
				distledger.ErrInsufficientBalance, delta.Key, bad[0])
		}
	}

	ins := t.b()
	insert := "INSERT INTO " + t.table("dist_account") + " (" + t.accountCols() + ") VALUES (" +
		ins.add(seed.Key.TenantID) + ", " + ins.add(seed.Key.UserID) + ", " +
		ins.add(seed.Frozen) + ", " + ins.add(seed.Available) + ", " +
		ins.add(seed.Withdrawing) + ", " + ins.add(seed.Withdrawn) + ", " +
		ins.add(seed.TotalEarned) + ", " + ins.add(seed.TotalReversed) + ", " +
		ins.add(int64(1)) + ", " + ins.add(instantOrNull(time.Now().UTC())) + ")"
	inserted, err := t.execInsert(ctx, insert, ins.vals)
	if err != nil {
		return distledger.Account{}, fmt.Errorf("sqlstore: insert account: %w", err)
	}
	if !inserted {
		// Another transaction created it between the UPDATE and the INSERT, so
		// this delta has not been applied yet. Reporting a conflict lets the
		// caller retry, which the port's retry contract permits.
		return distledger.Account{}, &distledger.ConflictError{
			Kind: "account", ID: delta.Key.String(), Expected: 0, Actual: 0,
		}
	}
	seed.Version = 1
	return seed, nil
}

// ── Commission ──────────────────────────────────────────────────────────

func (t *tx) AppendCommission(ctx context.Context, c distledger.Commission) (distledger.Commission, error) {
	if err := validateCommissionForWrite(c); err != nil {
		return c, err
	}

	b := t.b()
	query := "INSERT INTO " + t.table("dist_commission") + " (" + t.commissionCols() + ") VALUES (" +
		b.add(c.Key.TenantID) + ", " + b.add(c.Key.OrderID) + ", " +
		// order_item_id is NOT NULL: the empty string means "order-level
		// accrual", which is a value, not an absence.
		b.add(c.OrderItemID) + ", " + b.add(c.IdemKey) + ", " +
		b.add(c.BuyerUserID) + ", " + b.add(c.AgentUserID) + ", " + b.add(int64(c.Layer)) + ", " +
		b.add(c.BaseAmount) + ", " + b.add(int64(c.Rate)) + ", " + b.add(c.Amount) + ", " +
		b.add(c.ReversedAmount) + ", " + b.add(int64(c.State)) + ", " + b.add(int64(c.FreezeDays)) + ", " +
		b.add(instantOrNull(c.AvailableAt)) + ", " + b.add(instantOrNull(c.SettledAt)) + ", " +
		b.add(instantOrNull(c.AccruedAt)) + ", " + b.add(c.RuleVersion) + ", " +
		b.add(nullableString(c.RuleSnapshot)) + ", " + b.add(c.ReverseOf) + ", " + b.add(int64(1)) + ")"

	// The generated id column is not in the value list above, so the column list
	// has to drop it before the statement is sent.
	query = strings.Replace(query, t.cols("id")+", ", "", 1)

	id, inserted, err := t.insertReturningID(ctx, query, b.vals)
	if err != nil {
		return c, fmt.Errorf("sqlstore: insert commission: %w", err)
	}
	if !inserted {
		return c, distledger.ErrDuplicate
	}
	c.ID = id
	c.Version = 1
	return c, nil
}

// validateCommissionForWrite mirrors the in-memory store's checks, so a value
// that one backend refuses is refused by all of them.
func validateCommissionForWrite(c distledger.Commission) error {
	if err := c.Key.Validate(); err != nil {
		return err
	}
	if c.IdemKey == "" {
		return &distledger.FieldError{Field: "idem_key", Reason: "must not be empty"}
	}
	if c.AgentUserID <= 0 {
		return &distledger.FieldError{Field: "agent_user_id", Reason: "must be > 0"}
	}
	if c.Layer < 1 || c.Layer > distledger.MaxLevels {
		return &distledger.FieldError{Field: "layer", Reason: "outside the allowed range"}
	}
	if !c.State.Valid() {
		return &distledger.FieldError{Field: "state", Reason: "unknown commission state"}
	}
	if c.ReversedAmount < 0 || c.ReversedAmount > c.MaxReversible() {
		return &distledger.FieldError{Field: "reversed_amount", Reason: "outside [0, amount]"}
	}
	if c.ReverseOf < 0 {
		return &distledger.FieldError{Field: "reverse_of", Reason: "must be >= 0"}
	}
	switch {
	case c.Amount == 0:
		return &distledger.FieldError{Field: "amount", Reason: "a zero-amount commission carries no meaning"}
	case c.Amount < 0 && c.ReverseOf <= 0:
		return &distledger.FieldError{Field: "reverse_of", Reason: "a negative commission must reference its accrual"}
	case c.Amount > 0 && c.ReverseOf != 0:
		return &distledger.FieldError{Field: "reverse_of", Reason: "an accrual must not reference another commission"}
	}
	return nil
}

func (t *tx) SetCommissionAvailableAt(ctx context.Context, id int64, at time.Time, expectedVersion int64) (distledger.Commission, error) {
	b := t.b()
	update := "UPDATE " + t.table("dist_commission") + " SET " +
		t.cols("available_at") + " = " + b.add(instantOrNull(at)) + ", " +
		t.cols("version") + " = " + t.cols("version") + " + 1" +
		" WHERE " + t.cols("id") + " = " + b.add(id) +
		// First writer wins, expressed in the WHERE clause so two concurrent
		// deliveries cannot both succeed: the second sees available_at already
		// set and changes nothing.
		" AND " + t.cols("available_at") + " IS NULL" +
		" AND " + t.cols("version") + " = " + b.add(expectedVersion)

	res, err := t.updateVersioned(ctx, update, b.vals)
	if err != nil {
		return distledger.Commission{}, err
	}
	if res.affected == 1 {
		return t.Commission(ctx, id)
	}

	cur, err := t.Commission(ctx, id)
	if err != nil {
		return distledger.Commission{}, err
	}
	if !cur.AvailableAt.IsZero() {
		// Already settled in time by an earlier delivery: a no-op, not a failure.
		return cur, nil
	}
	return cur, conflict("commission", id, expectedVersion, cur.Version)
}

func (t *tx) SetCommissionReversedAmount(ctx context.Context, id int64, reversed distledger.Money, expectedVersion int64) (distledger.Commission, error) {
	if reversed < 0 {
		return distledger.Commission{}, &distledger.FieldError{
			Field: "reversed_amount", Reason: "must not be negative",
		}
	}
	b := t.b()
	update := "UPDATE " + t.table("dist_commission") + " SET " +
		t.cols("reversed_amount") + " = " + b.add(reversed) + ", " +
		t.cols("version") + " = " + t.cols("version") + " + 1" +
		" WHERE " + t.cols("id") + " = " + b.add(id) +
		" AND " + t.cols("version") + " = " + b.add(expectedVersion) +
		// The bound is enforced in the statement, because the limit depends on
		// the row's own amount and comparing the NEW value against that amount is
		// the only form that stays correct under concurrency.
		" AND " + t.cols("amount") + " >= " + b.add(reversed)

	res, err := t.updateVersioned(ctx, update, b.vals)
	if err != nil {
		return distledger.Commission{}, err
	}
	if res.affected == 1 {
		return t.Commission(ctx, id)
	}

	cur, err := t.Commission(ctx, id)
	if err != nil {
		return distledger.Commission{}, err
	}
	if reversed < 0 || reversed > cur.MaxReversible() {
		return cur, &distledger.FieldError{Field: "reversed_amount", Reason: "outside [0, amount]"}
	}
	if cur.ReversedAmount == reversed {
		return cur, nil
	}
	return cur, conflict("commission", id, expectedVersion, cur.Version)
}

func (t *tx) TransitionCommission(ctx context.Context, id int64, from, to distledger.CommissionState, expectedVersion int64) (distledger.Commission, error) {
	b := t.b()
	update := "UPDATE " + t.table("dist_commission") + " SET " +
		t.cols("state") + " = " + b.add(int64(to)) + ", " +
		t.cols("settled_at") + " = " + t.cols("settled_at") + ", " +
		t.cols("version") + " = " + t.cols("version") + " + 1" +
		" WHERE " + t.cols("id") + " = " + b.add(id) +
		" AND " + t.cols("state") + " = " + b.add(int64(from)) +
		" AND " + t.cols("version") + " = " + b.add(expectedVersion)

	res, err := t.updateVersioned(ctx, update, b.vals)
	if err != nil {
		return distledger.Commission{}, err
	}
	if res.affected == 1 {
		// Legality is checked after the row matched, not before, so that a state
		// or version mismatch still reports a conflict rather than an illegal
		// transition. The two must stay distinguishable: only one is retryable.
		//
		// Returning the error rolls the transaction back, so the row the UPDATE
		// just touched is restored.
		if err := distledger.ValidateCommissionTransition(from, to); err != nil {
			return distledger.Commission{}, err
		}
	}
	cur, err := t.Commission(ctx, id)
	if err != nil {
		return distledger.Commission{}, err
	}
	if res.affected == 1 {
		// A transition into a settled state records when it happened, which the
		// port's Commission.SettledAt documents as the settlement time.
		if to == distledger.CommissionSettled && cur.SettledAt.IsZero() {
			sb := t.b()
			stamp := "UPDATE " + t.table("dist_commission") + " SET " +
				t.cols("settled_at") + " = " + sb.add(instantOrNull(time.Now().UTC())) +
				" WHERE " + t.cols("id") + " = " + sb.add(id) +
				" AND " + t.cols("settled_at") + " IS NULL"
			if _, err := t.updateVersioned(ctx, stamp, sb.vals); err != nil {
				return distledger.Commission{}, err
			}
			return t.Commission(ctx, id)
		}
		return cur, nil
	}

	if cur.State != from {
		// Somebody else advanced it: retryable, and distinguishable from an
		// illegal transition, which is what lets the engine skip rather than
		// abort.
		return cur, &distledger.ConflictError{
			Kind: "commission state", ID: id, Expected: int64(from), Actual: int64(cur.State),
		}
	}
	return cur, conflict("commission", id, expectedVersion, cur.Version)
}

// ── Refund ──────────────────────────────────────────────────────────────

func (t *tx) AppendRefund(ctx context.Context, r distledger.Refund) (distledger.Refund, error) {
	if err := r.Key.Validate(); err != nil {
		return r, err
	}
	if r.IdemKey == "" {
		return r, &distledger.FieldError{Field: "idem_key", Reason: "must not be empty"}
	}
	if r.Amount <= 0 {
		return r, &distledger.FieldError{Field: "amount", Reason: "must be > 0"}
	}
	if r.Cumulative < r.Amount {
		return r, &distledger.FieldError{Field: "cumulative", Reason: "must be at least the instalment"}
	}

	b := t.b()
	query := "INSERT INTO " + t.table("dist_refund") + " (" +
		t.cols("tenant_id", "order_id", "item_id", "idem_key", "amount", "cumulative", "refunded_at") +
		") VALUES (" +
		b.add(r.Key.TenantID) + ", " + b.add(r.Key.OrderID) + ", " +
		// item_id is NOT NULL for the same reason as order_item_id.
		b.add(r.ItemID) + ", " + b.add(r.IdemKey) + ", " +
		b.add(r.Amount) + ", " + b.add(r.Cumulative) + ", " +
		b.add(instantOrNull(r.RefundedAt)) + ")"

	id, inserted, err := t.insertReturningID(ctx, query, b.vals)
	if err != nil {
		return r, fmt.Errorf("sqlstore: insert refund: %w", err)
	}
	if !inserted {
		return r, distledger.ErrDuplicate
	}
	r.ID = id
	return r, nil
}

// ── Ledger ──────────────────────────────────────────────────────────────

func (t *tx) AppendLedger(ctx context.Context, e distledger.LedgerEntry) (distledger.LedgerEntry, error) {
	if err := e.Key.Validate(); err != nil {
		return e, err
	}
	if !e.BizType.Valid() {
		return e, &distledger.FieldError{Field: "biz_type", Reason: "unknown ledger biz type"}
	}
	if len(e.Remark) > distledger.MaxRemarkLen {
		return e, &distledger.FieldError{Field: "remark", Reason: "too long"}
	}

	b := t.b()
	query := "INSERT INTO " + t.table("dist_ledger") + " (" +
		t.cols("tenant_id", "user_id", "biz_type", "biz_id",
			"delta_frozen", "delta_available", "delta_withdrawing", "delta_withdrawn",
			"after_frozen", "after_available", "after_withdrawing", "after_withdrawn",
			"remark", "created_at") +
		") VALUES (" +
		b.add(e.Key.TenantID) + ", " + b.add(e.Key.UserID) + ", " + b.add(int64(e.BizType)) + ", " +
		b.add(e.BizID) + ", " + b.add(e.DeltaFrozen) + ", " + b.add(e.DeltaAvailable) + ", " +
		b.add(e.DeltaWithdrawing) + ", " + b.add(e.DeltaWithdrawn) + ", " +
		b.add(e.AfterFrozen) + ", " + b.add(e.AfterAvailable) + ", " +
		b.add(e.AfterWithdrawing) + ", " + b.add(e.AfterWithdrawn) + ", " +
		b.add(nullableString(e.Remark)) + ", " + b.add(instantOrNull(e.CreatedAt)) + ")"

	id, inserted, err := t.insertReturningID(ctx, query, b.vals)
	if err != nil {
		return e, fmt.Errorf("sqlstore: insert ledger: %w", err)
	}
	if !inserted {
		// A ledger entry has no unique constraint to collide with, so this is
		// unreachable; reporting it beats writing an entry with id 0.
		return e, fmt.Errorf("sqlstore: ledger entry was not inserted")
	}
	e.ID = id
	return e, nil
}

func boolInt(v bool) int64 {
	if v {
		return 1
	}
	return 0
}
