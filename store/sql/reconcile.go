package sqlstore

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/darkinno-tech/distledger"
)

// ReconcileAccounts implements distledger.Reconciler.
//
// # What this replaces
//
// The row-wise conservation check walks every account and, for each one, streams
// every ledger entry of that account back to the caller. At a billion entries
// that is a billion rows over the wire and, at a page size of a thousand, a
// million round trips: hours of work holding a read transaction, in order to
// conclude - in the normal case - that nothing is wrong.
//
// The comparison is a grouped join. Doing it where the data is turns the round
// trips into one query and the rows into only the accounts that fail, which in
// the normal case is none.
//
// # Why the SQL is generated rather than written out
//
// The money buckets are a single definition in the root package, guarded by a
// reflection test, precisely so that adding a field cannot leave it outside the
// invariants. A hand-written query naming frozen, available, withdrawing and
// withdrawn would put a second, unguarded copy of that list in the storage
// layer, and a fifth bucket would then silently stop being reconciled - the
// check would keep passing while covering less.
//
// So every bucket reference below comes from distledger.AllBuckets(), and
// checkBucketColumns refuses to run when a bucket has no column mapping.
func (t *tx) ReconcileAccounts(
	ctx context.Context,
	tenantID int64,
	limit int,
	fn func(distledger.AccountReconciliation) error,
) (int, error) {
	if tenantID < 0 {
		return 0, fmt.Errorf("sqlstore: tenant id must be >= 0, got %d", tenantID)
	}
	if limit <= 0 {
		limit = distledger.DefaultPageLimit
	}

	// Resolve every bucket to its columns once, refusing to build a query when a
	// bucket has no mapping. Doing the lookup here rather than in an accessor
	// makes the precondition structural: the loops below cannot reference a
	// column that was never resolved.
	cols, err := resolveBucketColumns()
	if err != nil {
		return 0, err
	}

	// The aggregate pass reads the (tenant_id, user_id, id) covering index once
	// and yields the per-account sums, the entry count and the id of the last
	// entry. Taking MAX(id) here rather than in a correlated subquery per account
	// is what keeps this to a single pass: the index already covers it.
	// One argument builder for the entire statement. PostgreSQL numbers
	// placeholders per statement, so building the aggregate subquery and the
	// outer query separately would produce two $1s and a parameter count
	// mismatch. The calls below are in the order the placeholders appear in the
	// text: the aggregate's tenant id first, then the outer ones.
	b := t.b()
	aggSel := []string{
		t.qual("l", "user_id"),
		"COUNT(*) AS " + t.table("n"),
		"MAX(" + t.qual("l", "id") + ") AS " + t.table("tail_id"),
	}
	for i, c := range cols {
		aggSel = append(aggSel,
			"SUM("+t.qual("l", c.delta)+") AS "+t.table(aggAlias(i)))
	}
	aggregate := "SELECT " + strings.Join(aggSel, ", ") +
		" FROM " + t.table(tableLedger) + " AS " + t.table("l") +
		" WHERE " + t.qual("l", "tenant_id") + " = " + b.add(tenantID) +
		" GROUP BY " + t.qual("l", "user_id")

	// The outer select hands back the account row, the aggregates, and the last
	// entry's recorded running totals, so the library can run exactly the check
	// it would have run row-wise.
	sel := []string{t.qual("a", "user_id")}
	for _, c := range cols {
		sel = append(sel, t.qual("a", c.value))
	}
	sel = append(sel, t.qual("g", "n"), t.qual("g", "tail_id"))
	for i := range cols {
		sel = append(sel, t.qual("g", aggAlias(i)))
	}
	for _, c := range cols {
		sel = append(sel, t.qual("tl", c.after))
	}

	// Only accounts that can possibly be wrong cross the wire. The judgement
	// still happens in the library, so this is a narrowing rather than a verdict:
	// what it must never do is drop a genuinely broken account.
	var conds []string
	for i, c := range cols {
		// The stored value disagrees with the sum of the deltas. COALESCE covers
		// an account with no ledger entries, for which the sum really is zero.
		conds = append(conds, t.qual("a", c.value)+
			" <> COALESCE("+t.qual("g", aggAlias(i))+", 0)")
	}
	for _, c := range cols {
		// The last entry recorded a different running total than the account
		// holds: one of the two was changed without the other. With no last
		// entry both sides are NULL, SQL skips the comparison, and that is
		// correct - the sum condition above already covers an account with no
		// entries.
		conds = append(conds, t.qual("tl", c.after)+" <> "+t.qual("a", c.value))
	}
	for _, c := range cols {
		// A negative bucket is a violation whatever the sums say.
		conds = append(conds, t.qual("a", c.value)+" < 0")
	}

	query := "SELECT " + strings.Join(sel, ", ") +
		" FROM " + t.table(tableAccount) + " AS " + t.table("a") +
		" LEFT JOIN (" + aggregate + ") AS " + t.table("g") +
		" ON " + t.qual("g", "user_id") + " = " + t.qual("a", "user_id") +
		" LEFT JOIN " + t.table(tableLedger) + " AS " + t.table("tl") +
		" ON " + t.qual("tl", "tenant_id") + " = " + t.qual("a", "tenant_id") +
		" AND " + t.qual("tl", "id") + " = " + t.qual("g", "tail_id") +
		" WHERE " + t.qual("a", "tenant_id") + " = " + b.add(tenantID) +
		" AND (" + strings.Join(conds, " OR ") + ")" +
		" ORDER BY " + t.qual("a", "user_id") +
		" LIMIT " + b.add(int64(limit))

	examined, err := t.countAccounts(ctx, tenantID)
	if err != nil {
		return 0, err
	}

	err = t.queryRows(ctx, query, b.vals, func(rows *sql.Rows) error {
		m, err := scanReconciliation(rows, len(cols))
		if err != nil {
			return err
		}
		return fn(m)
	})
	if err != nil {
		return 0, err
	}
	return examined, nil
}

// countAccounts reports how many accounts the tenant has, so that a check which
// examined every one of them and found nothing wrong is not reported the same way
// as a check that examined nothing.
func (t *tx) countAccounts(ctx context.Context, tenantID int64) (int, error) {
	b := t.b()
	query := "SELECT COUNT(*) FROM " + t.table(tableAccount) +
		" WHERE " + t.cols("tenant_id") + " = " + b.add(tenantID)

	var n int
	err := t.queryRows(ctx, query, b.vals, func(rows *sql.Rows) error {
		return rows.Scan(&n)
	})
	return n, err
}

// aggAlias names the aggregate column for bucket i by position, so that no alias
// has to be derived from a column name and collide with one.
func aggAlias(i int) string { return fmt.Sprintf("s%d", i) }

// bucketColumns maps a bucket to its three storage columns: the account's own
// value, the ledger's delta, and the ledger's recorded running total.
//
// It is deliberately exhaustive over distledger.AllBuckets() and reports failure
// rather than a zero value for anything it does not know, so that a bucket added
// to the domain without a column here stops reconciliation loudly instead of
// being quietly excluded from it.
func bucketColumns(b distledger.Bucket) (bucketCol, bool) {
	switch b {
	case distledger.BucketFrozen:
		return bucketCol{"frozen", "delta_frozen", "after_frozen"}, true
	case distledger.BucketAvailable:
		return bucketCol{"available", "delta_available", "after_available"}, true
	case distledger.BucketWithdrawing:
		return bucketCol{"withdrawing", "delta_withdrawing", "after_withdrawing"}, true
	case distledger.BucketWithdrawn:
		return bucketCol{"withdrawn", "delta_withdrawn", "after_withdrawn"}, true
	default:
		return bucketCol{}, false
	}
}

// bucketCol is one bucket's three storage columns.
type bucketCol struct {
	value string // the account's own balance
	delta string // the ledger's change to it
	after string // the ledger's recorded running total
}

// resolveBucketColumns maps every bucket to its columns, failing rather than
// returning a partial set.
//
// This is what stops a new money field from escaping reconciliation: the domain
// owns the bucket set, so a bucket the storage layer has no column for stops the
// check loudly instead of being quietly left out of it.
func resolveBucketColumns() ([]bucketCol, error) {
	buckets := distledger.AllBuckets()
	out := make([]bucketCol, 0, len(buckets))
	for _, b := range buckets {
		c, ok := bucketColumns(b)
		if !ok {
			return nil, fmt.Errorf(
				"sqlstore: bucket %v has no column mapping; reconciliation would silently skip it",
				b)
		}
		out = append(out, c)
	}
	return out, nil
}

// scanReconciliation reads one candidate row in the order the select list above
// builds: user id, each stored bucket, the entry count, the last entry id, each
// summed bucket, then each recorded running total.
func scanReconciliation(rows *sql.Rows, n int) (distledger.AccountReconciliation, error) {
	var (
		m      distledger.AccountReconciliation
		userID int64
		// Both are nullable. An account with no ledger entries at all matches no
		// row in the aggregate, so its count and tail id arrive as NULL rather
		// than as zero - and such an account is exactly the "balance with no
		// trail" case the check must be able to report.
		entries sql.NullInt64
		tailID  sql.NullInt64
	)

	// The destination list is built in select order explicitly. Deriving it from
	// an offset into one flat slice would turn any change to the select list into
	// a silent misalignment.
	stored := make([]int64, n)
	// Nullable: SUM over no rows is NULL, not zero. An account with no ledger
	// entries is a case the check has to handle, so its sums must read back as
	// zero rather than as a scan error.
	summed := make([]sql.NullInt64, n)
	// Also nullable: the last entry is absent when there is no entry, and the
	// running totals are only meaningful in that case.
	after := make([]sql.NullInt64, n)

	dest := make([]any, 0, 4+3*n)
	dest = append(dest, &userID)
	for i := range stored {
		dest = append(dest, &stored[i])
	}
	dest = append(dest, &entries, &tailID)
	for i := range summed {
		dest = append(dest, &summed[i])
	}
	for i := range after {
		dest = append(dest, &after[i])
	}

	if err := rows.Scan(dest...); err != nil {
		return m, err
	}

	m.Key.UserID = userID
	m.Stored.Key.UserID = userID
	m.Summed.Key.UserID = userID
	// Walked in AllBuckets() order, which is the order the select list and the
	// destination list were both built in.
	for i, b := range distledger.AllBuckets() {
		b.Set(&m.Stored, distledger.Money(stored[i]))
		var sum distledger.Money
		if summed[i].Valid {
			sum = distledger.Money(summed[i].Int64)
		}
		b.Set(&m.Summed, sum)
		var tailAfter distledger.Money
		if after[i].Valid {
			tailAfter = distledger.Money(after[i].Int64)
		}
		b.SetAfter(&m.Tail, tailAfter)
	}
	m.HasTail = entries.Valid && entries.Int64 > 0
	if tailID.Valid {
		m.Tail.ID = tailID.Int64
	}
	return m, nil
}

// ReconcileAccounts implements distledger.Reconciler for the store.
//
// The interface is on the store rather than on the read interface because the
// whole point of it is to run its own aggregation rather than to be driven page
// by page, so it opens its own read transaction.
func (s *Store) ReconcileAccounts(
	ctx context.Context,
	tenantID int64,
	limit int,
	fn func(distledger.AccountReconciliation) error,
) (int, error) {
	var examined int
	err := s.View(ctx, func(ctx context.Context, r distledger.Reader) error {
		t, ok := r.(*tx)
		if !ok {
			return fmt.Errorf("sqlstore: unexpected reader type %T", r)
		}
		var err error
		examined, err = t.ReconcileAccounts(ctx, tenantID, limit, fn)
		return err
	})
	return examined, err
}
