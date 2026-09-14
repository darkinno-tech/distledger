package sqlstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/im10furry/distledger"
)

// Store is the portable SQL implementation of distledger.Store.
//
// It is built once and shared: it holds no per-call state, so every method is
// safe to call concurrently. Concurrency is confined to database/sql's
// connection pool and to the transactions opened per call.
type Store struct {
	db         *sql.DB
	dialect    Dialect
	classifier Classifier
	maxRetries int
	retryWait  time.Duration
	log        *slog.Logger
}

var _ distledger.Store = (*Store)(nil)
var _ distledger.ReportedStore = (*Store)(nil)
var _ distledger.SchemaChecker = (*Store)(nil)

// Option adjusts a Store at construction time.
type Option func(*Store)

// WithClassifier replaces the built-in structural error classification.
//
// Supply one when your driver's errors expose neither a numeric code field nor
// an SQLState method. Embed FallbackClassifier to keep the built-in behaviour
// and add to it.
func WithClassifier(c Classifier) Option {
	return func(s *Store) {
		if c != nil {
			s.classifier = c
		}
	}
}

// WithMaxRetries sets how many times a transaction is retried after a retryable
// failure. The default is 5.
//
// Retrying is only ever done after the failed attempt has been rolled back, so
// the update function sees the pre-attempt state again. That contract is what
// makes the engine's transactions safe to re-run; see distledger.Store.
func WithMaxRetries(n int) Option {
	return func(s *Store) {
		if n >= 0 {
			s.maxRetries = n
		}
	}
}

// WithRetryBackoff sets the base delay between retries. The default is 2ms, and
// the delay doubles on each attempt up to 200ms.
func WithRetryBackoff(d time.Duration) Option {
	return func(s *Store) {
		if d > 0 {
			s.retryWait = d
		}
	}
}

// WithLogger sets the logger used for retry and migration reporting.
func WithLogger(l *slog.Logger) Option {
	return func(s *Store) {
		if l != nil {
			s.log = l
		}
	}
}

// Open returns a Store over an already-open database handle.
//
// The library imports no database driver, so the caller opens the handle with
// whichever driver they already depend on:
//
//	db, err := sql.Open("mysql", dsn)
//	store, err := sqlstore.Open(db, mysql.Dialect{})
//
// Callers who use store/mysql, store/postgres or store/sqlite can reach the same
// thing through that package's Open, which simply passes its own Dialect.
func Open(db *sql.DB, d Dialect, opts ...Option) (*Store, error) {
	if db == nil {
		return nil, errors.New("sqlstore: nil *sql.DB")
	}
	if d == nil {
		return nil, errors.New("sqlstore: nil Dialect")
	}
	s := &Store{
		db:         db,
		dialect:    d,
		classifier: FallbackClassifier{},
		maxRetries: 5,
		retryWait:  2 * time.Millisecond,
		log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	for _, opt := range opts {
		opt(s)
	}
	return s, nil
}

// StoreKind implements distledger.ReportedStore.
func (s *Store) StoreKind() string { return s.dialect.Name() }

// Close is a no-op.
//
// The Store does not own the *sql.DB it was handed: the caller opened it and the
// caller closes it. Closing a borrowed handle would be a surprise, and the
// library's own tests rely on being able to inspect the pool afterwards.
func (s *Store) Close() error { return nil }

// DB exposes the underlying handle, for callers who need to run their own
// queries alongside the store.
func (s *Store) DB() *sql.DB { return s.db }

// Dialect exposes the dialect the Store was built with.
func (s *Store) Dialect() Dialect { return s.dialect }

// View runs fn in a read-only transaction.
func (s *Store) View(ctx context.Context, fn func(ctx context.Context, r distledger.Reader) error) error {
	return s.runTx(ctx, true, func(ctx context.Context, t *tx) error {
		return fn(ctx, t)
	})
}

// Update runs fn in a read-write transaction, retrying retryable failures.
//
// # The retry contract
//
// A retry always rolls the failed attempt back first, so fn is invoked again
// against the state as it was before that attempt. Weakening this - retrying
// without a rollback - would let the engine's second attempt see its own
// half-applied work and mistake it for pre-existing state. The engine depends on
// the stronger form: settling a commission moves its state and then debits the
// account, and running that again is only safe because the first attempt left
// nothing behind (see ADR-030).
//
// Only failures the dialect classifies as retryable are retried. A duplicate key
// or a version conflict is a business outcome, not a transient fault, and is
// returned to the caller unchanged.
func (s *Store) Update(ctx context.Context, fn func(ctx context.Context, tx distledger.Tx) error) error {
	var lastErr error
	for attempt := 0; attempt <= s.maxRetries; attempt++ {
		if attempt > 0 {
			if err := s.waitBeforeRetry(ctx, attempt); err != nil {
				return err
			}
		}
		err := s.runTx(ctx, false, func(ctx context.Context, t *tx) error {
			return fn(ctx, t)
		})
		if err == nil {
			return nil
		}
		if !s.classifier.IsRetryable(err) {
			return err
		}
		lastErr = err
		s.log.WarnContext(ctx, "retrying transaction",
			"dialect", s.dialect.Name(), "attempt", attempt+1, "error", err)
	}
	return fmt.Errorf("sqlstore: transaction failed after %d retries: %w", s.maxRetries, lastErr)
}

func (s *Store) waitBeforeRetry(ctx context.Context, attempt int) error {
	d := s.retryWait << (attempt - 1)
	if d > 200*time.Millisecond || d <= 0 {
		d = 200 * time.Millisecond
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// runTx opens a transaction, runs fn, and commits only if fn succeeded.
//
// The rollback is deferred unconditionally: commit failure and early return both
// have to release the connection, and a leaked transaction holds locks until the
// driver's timeout, which under load becomes a self-inflicted outage.
func (s *Store) runTx(ctx context.Context, readOnly bool, fn func(context.Context, *tx) error) error {
	sqlTx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: readOnly})
	if err != nil {
		return fmt.Errorf("sqlstore: begin: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			// Rolling back a committed transaction returns ErrTxDone, which is
			// expected and ignored.
			_ = sqlTx.Rollback()
		}
	}()

	t := &tx{s: s, sqlTx: sqlTx}
	if err := fn(ctx, t); err != nil {
		return err
	}
	if err := sqlTx.Commit(); err != nil {
		return fmt.Errorf("sqlstore: commit: %w", err)
	}
	committed = true
	return nil
}

// tx is the transaction-scoped handle implementing distledger.Tx.
type tx struct {
	s     *Store
	sqlTx *sql.Tx
}

var _ distledger.Tx = (*tx)(nil)

// b starts an argument builder bound to the store's dialect.
func (t *tx) b() *args { return newArgs(t.s.dialect) }

// cols renders a quoted, comma-separated column list.
func (t *tx) cols(names ...string) string {
	quoted := make([]string, 0, len(names))
	for _, n := range names {
		quoted = append(quoted, t.s.dialect.Quote(n))
	}
	return strings.Join(quoted, ", ")
}

// table renders a quoted table name.
func (t *tx) table(name string) string { return t.s.dialect.Quote(name) }

// queryRows runs a query and hands each row to scan, closing the rows.
func (t *tx) queryRows(ctx context.Context, query string, argv []any, scan func(*sql.Rows) error) error {
	rows, err := t.sqlTx.QueryContext(ctx, query, argv...)
	if err != nil {
		return fmt.Errorf("sqlstore: query: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		if err := scan(rows); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("sqlstore: iterate: %w", err)
	}
	return nil
}

// queryOne runs a query expected to return at most one row.
//
// sql.ErrNoRows is passed through untouched so callers can map it to whatever
// the port says about that entity, which differs: a missing account is a zero
// account, a missing commission is ErrNotFound.
func (t *tx) queryOne(ctx context.Context, query string, argv []any, scan func(*sql.Rows) error) error {
	return t.queryRows(ctx, query, argv, scan)
}

// ── Column lists ────────────────────────────────────────────────────────
//
// One list per table, used by every statement that reads it. Keeping them in one
// place means a new column reaches every query at once instead of only the one
// the author happened to be editing.

func (t *tx) agentCols() string {
	return t.cols("tenant_id", "user_id", "parent_id", "depth", "status", "join_type",
		"rate_override_bp", "created_at", "updated_at", "version")
}

func (t *tx) bindingCols() string {
	return t.cols("tenant_id", "buyer_user_id", "agent_user_id", "source", "source_ref",
		"bound_at", "expire_at", "active", "rebound_from", "version")
}

func (t *tx) accountCols() string {
	return t.cols("tenant_id", "user_id", "frozen", "available", "withdrawing", "withdrawn",
		"total_earned", "total_reversed", "version", "updated_at")
}

func (t *tx) commissionCols() string {
	return t.cols("id", "tenant_id", "order_id", "order_item_id", "idem_key",
		"buyer_user_id", "agent_user_id", "layer", "base_amount", "rate_bp", "amount",
		"reversed_amount", "state", "freeze_days", "available_at", "settled_at", "accrued_at",
		"rule_version", "rule_snapshot", "reverse_of", "version")
}

func (t *tx) ledgerCols() string {
	return t.cols("id", "tenant_id", "user_id", "biz_type", "biz_id",
		"delta_frozen", "delta_available", "delta_withdrawing", "delta_withdrawn",
		"after_frozen", "after_available", "after_withdrawing", "after_withdrawn",
		"remark", "created_at")
}

func (t *tx) refundCols() string {
	return t.cols("id", "tenant_id", "order_id", "item_id", "idem_key",
		"amount", "cumulative", "refunded_at")
}

// ── Scanning ────────────────────────────────────────────────────────────

func scanAgent(rows *sql.Rows) (distledger.Agent, error) {
	var (
		a            distledger.Agent
		status       int64
		joinType     int64
		rateOverride sql.NullInt64
		createdAt    sql.NullInt64
		updatedAt    sql.NullInt64
	)
	err := rows.Scan(&a.Key.TenantID, &a.Key.UserID, &a.ParentID, &a.Depth,
		&status, &joinType, &rateOverride, &createdAt, &updatedAt, &a.Version)
	if err != nil {
		return a, err
	}
	a.Status = distledger.AgentStatus(status)
	a.JoinType = distledger.JoinType(joinType)
	if rateOverride.Valid {
		rate := distledger.Rate(rateOverride.Int64)
		a.RateOverrideBP = &rate
	}
	a.CreatedAt = nullInstant(createdAt)
	a.UpdatedAt = nullInstant(updatedAt)
	return a, nil
}

func scanBinding(rows *sql.Rows) (distledger.Binding, error) {
	var (
		b        distledger.Binding
		source   int64
		sourceRe sql.NullString
		boundAt  sql.NullInt64
		expireAt sql.NullInt64
		active   int64
	)
	err := rows.Scan(&b.Buyer.TenantID, &b.Buyer.UserID, &b.AgentUserID, &source, &sourceRe,
		&boundAt, &expireAt, &active, &b.ReboundFrom, &b.Version)
	if err != nil {
		return b, err
	}
	b.Source = distledger.BindSource(source)
	b.SourceRef = stringOrEmpty(sourceRe)
	b.BoundAt = nullInstant(boundAt)
	b.ExpireAt = nullInstant(expireAt)
	b.Active = active != 0
	return b, nil
}

func scanAccount(rows *sql.Rows) (distledger.Account, error) {
	var (
		a         distledger.Account
		updatedAt sql.NullInt64
	)
	err := rows.Scan(&a.Key.TenantID, &a.Key.UserID, &a.Frozen, &a.Available,
		&a.Withdrawing, &a.Withdrawn, &a.TotalEarned, &a.TotalReversed, &a.Version, &updatedAt)
	if err != nil {
		return a, err
	}
	a.UpdatedAt = nullInstant(updatedAt)
	return a, nil
}

func scanCommission(rows *sql.Rows) (distledger.Commission, error) {
	var (
		c            distledger.Commission
		rateBP       int64
		itemID       sql.NullString
		availableAt  sql.NullInt64
		settledAt    sql.NullInt64
		accruedAt    sql.NullInt64
		ruleSnapshot sql.NullString
		state        int64
	)
	err := rows.Scan(&c.ID, &c.Key.TenantID, &c.Key.OrderID, &itemID, &c.IdemKey,
		&c.BuyerUserID, &c.AgentUserID, &c.Layer, &c.BaseAmount, &rateBP, &c.Amount,
		&c.ReversedAmount, &state, &c.FreezeDays, &availableAt, &settledAt, &accruedAt,
		&c.RuleVersion, &ruleSnapshot, &c.ReverseOf, &c.Version)
	if err != nil {
		return c, err
	}
	c.OrderItemID = stringOrEmpty(itemID)
	c.Rate = distledger.Rate(rateBP)
	c.State = distledger.CommissionState(state)
	c.AvailableAt = nullInstant(availableAt)
	c.SettledAt = nullInstant(settledAt)
	c.AccruedAt = nullInstant(accruedAt)
	c.RuleSnapshot = stringOrEmpty(ruleSnapshot)
	return c, nil
}

func scanLedger(rows *sql.Rows) (distledger.LedgerEntry, error) {
	var (
		e         distledger.LedgerEntry
		bizType   int64
		bizID     sql.NullString
		remark    sql.NullString
		createdAt sql.NullInt64
	)
	err := rows.Scan(&e.ID, &e.Key.TenantID, &e.Key.UserID, &bizType, &bizID,
		&e.DeltaFrozen, &e.DeltaAvailable, &e.DeltaWithdrawing, &e.DeltaWithdrawn,
		&e.AfterFrozen, &e.AfterAvailable, &e.AfterWithdrawing, &e.AfterWithdrawn,
		&remark, &createdAt)
	if err != nil {
		return e, err
	}
	e.BizType = distledger.LedgerBizType(bizType)
	e.BizID = stringOrEmpty(bizID)
	e.Remark = stringOrEmpty(remark)
	e.CreatedAt = nullInstant(createdAt)
	return e, nil
}

func scanRefund(rows *sql.Rows) (distledger.Refund, error) {
	var (
		r          distledger.Refund
		itemID     sql.NullString
		refundedAt sql.NullInt64
	)
	err := rows.Scan(&r.ID, &r.Key.TenantID, &r.Key.OrderID, &itemID, &r.IdemKey,
		&r.Amount, &r.Cumulative, &refundedAt)
	if err != nil {
		return r, err
	}
	r.ItemID = stringOrEmpty(itemID)
	r.RefundedAt = nullInstant(refundedAt)
	return r, nil
}

// ── Reader ──────────────────────────────────────────────────────────────

func (t *tx) Agent(ctx context.Context, key distledger.UserKey) (distledger.Agent, error) {
	b := t.b()
	query := "SELECT " + t.agentCols() + " FROM " + t.table("dist_agent") +
		" WHERE " + t.cols("tenant_id") + " = " + b.add(key.TenantID) +
		" AND " + t.cols("user_id") + " = " + b.add(key.UserID)

	var (
		out   distledger.Agent
		found bool
	)
	err := t.queryOne(ctx, query, b.vals, func(rows *sql.Rows) error {
		a, err := scanAgent(rows)
		if err != nil {
			return err
		}
		out, found = a, true
		return nil
	})
	if err != nil {
		return distledger.Agent{}, err
	}
	if !found {
		return distledger.Agent{}, distledger.ErrNotFound
	}
	return out, nil
}

func (t *tx) Binding(ctx context.Context, buyer distledger.UserKey) (distledger.Binding, error) {
	b := t.b()
	query := "SELECT " + t.bindingCols() + " FROM " + t.table("dist_binding") +
		" WHERE " + t.cols("tenant_id") + " = " + b.add(buyer.TenantID) +
		" AND " + t.cols("buyer_user_id") + " = " + b.add(buyer.UserID)

	var (
		out   distledger.Binding
		found bool
	)
	err := t.queryOne(ctx, query, b.vals, func(rows *sql.Rows) error {
		v, err := scanBinding(rows)
		if err != nil {
			return err
		}
		out, found = v, true
		return nil
	})
	if err != nil {
		return distledger.Binding{}, err
	}
	if !found {
		return distledger.Binding{}, distledger.ErrNotFound
	}
	return out, nil
}

// Account returns the account, or the zero account when there is none.
//
// The port is explicit that a missing account is not an error: a user who has
// never moved money simply has the zero account, and making callers distinguish
// the two would only add a branch to every one of them.
func (t *tx) Account(ctx context.Context, key distledger.UserKey) (distledger.Account, error) {
	b := t.b()
	query := "SELECT " + t.accountCols() + " FROM " + t.table("dist_account") +
		" WHERE " + t.cols("tenant_id") + " = " + b.add(key.TenantID) +
		" AND " + t.cols("user_id") + " = " + b.add(key.UserID)

	var out distledger.Account
	err := t.queryOne(ctx, query, b.vals, func(rows *sql.Rows) error {
		a, err := scanAccount(rows)
		if err != nil {
			return err
		}
		out = a
		return nil
	})
	if err != nil {
		return distledger.Account{}, err
	}
	return out, nil
}

func (t *tx) Commission(ctx context.Context, id int64) (distledger.Commission, error) {
	b := t.b()
	query := "SELECT " + t.commissionCols() + " FROM " + t.table("dist_commission") +
		" WHERE " + t.cols("id") + " = " + b.add(id)

	var (
		out   distledger.Commission
		found bool
	)
	err := t.queryOne(ctx, query, b.vals, func(rows *sql.Rows) error {
		c, err := scanCommission(rows)
		if err != nil {
			return err
		}
		out, found = c, true
		return nil
	})
	if err != nil {
		return distledger.Commission{}, err
	}
	if !found {
		return distledger.Commission{}, distledger.ErrNotFound
	}
	return out, nil
}

func (t *tx) CommissionsByOrder(ctx context.Context, key distledger.OrderKey) ([]distledger.Commission, error) {
	b := t.b()
	query := "SELECT " + t.commissionCols() + " FROM " + t.table("dist_commission") +
		" WHERE " + t.cols("tenant_id") + " = " + b.add(key.TenantID) +
		" AND " + t.cols("order_id") + " = " + b.add(key.OrderID) +
		" ORDER BY " + t.cols("id")

	out := make([]distledger.Commission, 0, 4)
	err := t.queryRows(ctx, query, b.vals, func(rows *sql.Rows) error {
		c, err := scanCommission(rows)
		if err != nil {
			return err
		}
		out = append(out, c)
		return nil
	})
	return out, err
}

func (t *tx) RefundsByOrder(ctx context.Context, key distledger.OrderKey) ([]distledger.Refund, error) {
	b := t.b()
	query := "SELECT " + t.refundCols() + " FROM " + t.table("dist_refund") +
		" WHERE " + t.cols("tenant_id") + " = " + b.add(key.TenantID) +
		" AND " + t.cols("order_id") + " = " + b.add(key.OrderID) +
		" ORDER BY " + t.cols("id")

	out := make([]distledger.Refund, 0, 2)
	err := t.queryRows(ctx, query, b.vals, func(rows *sql.Rows) error {
		r, err := scanRefund(rows)
		if err != nil {
			return err
		}
		out = append(out, r)
		return nil
	})
	return out, err
}

func (t *tx) CommissionsByAgent(ctx context.Context, key distledger.UserKey, q distledger.CommissionQuery) ([]distledger.Commission, error) {
	b := t.b()
	query := "SELECT " + t.commissionCols() + " FROM " + t.table("dist_commission") +
		" WHERE " + t.cols("tenant_id") + " = " + b.add(key.TenantID) +
		" AND " + t.cols("agent_user_id") + " = " + b.add(key.UserID) +
		" AND " + t.cols("id") + " > " + b.add(q.Page.AfterID)
	if len(q.States) > 0 {
		placeholders := make([]string, 0, len(q.States))
		for _, st := range q.States {
			placeholders = append(placeholders, b.add(int64(st)))
		}
		query += " AND " + t.cols("state") + " IN (" + strings.Join(placeholders, ", ") + ")"
	}
	query += " ORDER BY " + t.cols("id") + " LIMIT " + b.add(int64(q.Page.LimitOrDefault()))

	out := make([]distledger.Commission, 0, 16)
	err := t.queryRows(ctx, query, b.vals, func(rows *sql.Rows) error {
		c, err := scanCommission(rows)
		if err != nil {
			return err
		}
		out = append(out, c)
		return nil
	})
	return out, err
}

func (t *tx) CommissionsByTenant(ctx context.Context, tenantID int64, p distledger.Page) ([]distledger.Commission, error) {
	b := t.b()
	query := "SELECT " + t.commissionCols() + " FROM " + t.table("dist_commission") +
		" WHERE " + t.cols("tenant_id") + " = " + b.add(tenantID) +
		" AND " + t.cols("id") + " > " + b.add(p.AfterID) +
		" ORDER BY " + t.cols("id") + " LIMIT " + b.add(int64(p.LimitOrDefault()))

	out := make([]distledger.Commission, 0, 16)
	err := t.queryRows(ctx, query, b.vals, func(rows *sql.Rows) error {
		c, err := scanCommission(rows)
		if err != nil {
			return err
		}
		out = append(out, c)
		return nil
	})
	return out, err
}

// DueCommissions returns pending commissions whose settlement time has arrived.
//
// The condition spells out IS NOT NULL rather than relying on a sentinel value:
// an absent settlement date is NULL, so the predicate matches the index
// (state, available_at, tenant_id) directly and the range scan stays tight.
func (t *tx) DueCommissions(ctx context.Context, tenantID int64, dueAt time.Time, limit int) ([]distledger.Commission, error) {
	if limit <= 0 {
		limit = distledger.DefaultPageLimit
	}
	b := t.b()
	query := "SELECT " + t.commissionCols() + " FROM " + t.table("dist_commission") +
		" WHERE " + t.cols("tenant_id") + " = " + b.add(tenantID) +
		" AND " + t.cols("state") + " = " + b.add(int64(distledger.CommissionPending)) +
		" AND " + t.cols("available_at") + " IS NOT NULL" +
		" AND " + t.cols("available_at") + " <= " + b.add(instantOrNull(dueAt)) +
		" ORDER BY " + t.cols("available_at") + ", " + t.cols("id") +
		" LIMIT " + b.add(int64(limit))

	out := make([]distledger.Commission, 0, limit)
	err := t.queryRows(ctx, query, b.vals, func(rows *sql.Rows) error {
		c, err := scanCommission(rows)
		if err != nil {
			return err
		}
		out = append(out, c)
		return nil
	})
	return out, err
}

// TenantsWithDueWork answers the heartbeat's question in one indexed query.
//
// It is the SQL equivalent of the in-memory index walk: a range scan over
// (state, available_at, tenant_id) that stops as soon as it has seen limit
// distinct tenants. Its cost is proportional to the tenants that have work, not
// to the tenants that exist, and not to the tenants that do not.
func (t *tx) TenantsWithDueWork(ctx context.Context, dueAt time.Time, limit int) ([]int64, error) {
	if limit <= 0 {
		limit = distledger.DefaultPageLimit
	}
	b := t.b()
	query := "SELECT DISTINCT " + t.cols("tenant_id") + " FROM " + t.table("dist_commission") +
		" WHERE " + t.cols("state") + " = " + b.add(int64(distledger.CommissionPending)) +
		" AND " + t.cols("available_at") + " IS NOT NULL" +
		" AND " + t.cols("available_at") + " <= " + b.add(instantOrNull(dueAt)) +
		" ORDER BY " + t.cols("tenant_id") +
		" LIMIT " + b.add(int64(limit))

	out := make([]int64, 0, 8)
	err := t.queryRows(ctx, query, b.vals, func(rows *sql.Rows) error {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return err
		}
		out = append(out, id)
		return nil
	})
	return out, err
}

func (t *tx) LedgerEntries(ctx context.Context, key distledger.UserKey, q distledger.LedgerQuery) ([]distledger.LedgerEntry, error) {
	b := t.b()
	query := "SELECT " + t.ledgerCols() + " FROM " + t.table("dist_ledger") +
		" WHERE " + t.cols("tenant_id") + " = " + b.add(key.TenantID) +
		" AND " + t.cols("user_id") + " = " + b.add(key.UserID) +
		" AND " + t.cols("id") + " > " + b.add(q.Page.AfterID)
	if q.BizType != 0 {
		query += " AND " + t.cols("biz_type") + " = " + b.add(int64(q.BizType))
	}
	query += " ORDER BY " + t.cols("id") + " LIMIT " + b.add(int64(q.Page.LimitOrDefault()))

	out := make([]distledger.LedgerEntry, 0, 16)
	err := t.queryRows(ctx, query, b.vals, func(rows *sql.Rows) error {
		e, err := scanLedger(rows)
		if err != nil {
			return err
		}
		out = append(out, e)
		return nil
	})
	return out, err
}

func (t *tx) LedgerByTenant(ctx context.Context, tenantID int64, p distledger.Page) ([]distledger.LedgerEntry, error) {
	b := t.b()
	query := "SELECT " + t.ledgerCols() + " FROM " + t.table("dist_ledger") +
		" WHERE " + t.cols("tenant_id") + " = " + b.add(tenantID) +
		" AND " + t.cols("id") + " > " + b.add(p.AfterID) +
		" ORDER BY " + t.cols("id") + " LIMIT " + b.add(int64(p.LimitOrDefault()))

	out := make([]distledger.LedgerEntry, 0, 16)
	err := t.queryRows(ctx, query, b.vals, func(rows *sql.Rows) error {
		e, err := scanLedger(rows)
		if err != nil {
			return err
		}
		out = append(out, e)
		return nil
	})
	return out, err
}

func (t *tx) AccountsByTenant(ctx context.Context, tenantID int64, p distledger.AccountPage) ([]distledger.Account, error) {
	b := t.b()
	query := "SELECT " + t.accountCols() + " FROM " + t.table("dist_account") +
		" WHERE " + t.cols("tenant_id") + " = " + b.add(tenantID) +
		" AND " + t.cols("user_id") + " > " + b.add(p.AfterUserID) +
		" ORDER BY " + t.cols("user_id") + " LIMIT " + b.add(int64(p.LimitOrDefault()))

	out := make([]distledger.Account, 0, 16)
	err := t.queryRows(ctx, query, b.vals, func(rows *sql.Rows) error {
		a, err := scanAccount(rows)
		if err != nil {
			return err
		}
		out = append(out, a)
		return nil
	})
	return out, err
}

func (t *tx) CountCommissionsByState(ctx context.Context, tenantID int64) (map[distledger.CommissionState]int64, error) {
	b := t.b()
	query := "SELECT " + t.cols("state") + ", COUNT(*) FROM " + t.table("dist_commission") +
		" WHERE " + t.cols("tenant_id") + " = " + b.add(tenantID) +
		" GROUP BY " + t.cols("state")

	out := make(map[distledger.CommissionState]int64, len(distledger.AllCommissionStates()))
	err := t.queryRows(ctx, query, b.vals, func(rows *sql.Rows) error {
		var (
			state int64
			n     int64
		)
		if err := rows.Scan(&state, &n); err != nil {
			return err
		}
		out[distledger.CommissionState(state)] = n
		return nil
	})
	return out, err
}
