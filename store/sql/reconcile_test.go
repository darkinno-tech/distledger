package sqlstore_test

import (
	"testing"

	"github.com/darkinno-tech/distledger"
	sqlstore "github.com/darkinno-tech/distledger/store/sql"
)

// TestEveryBucketHasReconcilableColumns is the guard that keeps a new money
// field from escaping reconciliation.
//
// The bucket set belongs to the domain, and a reflection test there forces every
// money field of an Account to be classified as a bucket. That test cannot see
// the storage layer, so it cannot notice that the reconciliation query has no
// column for a bucket it just declared. This closes that gap from the other side:
// the SQL store is asked about every bucket, and about the columns it names.
func TestEveryBucketHasReconcilableColumns(t *testing.T) {
	cols, err := sqlstore.ResolveBucketColumnsForTest()
	if err != nil {
		t.Fatalf("every bucket must resolve to columns, or reconciliation silently skips it: %v", err)
	}

	buckets := distledger.AllBuckets()
	if len(cols) != len(buckets) {
		t.Fatalf("resolved %d buckets, the domain declares %d", len(cols), len(buckets))
	}

	// Collect the columns each table actually declares, so a mapping that names a
	// column the schema does not have is caught here rather than as a SQL error
	// on a live database.
	declared := map[string]map[string]bool{}
	for _, table := range sqlstore.Schema() {
		set := make(map[string]bool, len(table.Columns))
		for _, c := range table.Columns {
			set[c.Name] = true
		}
		declared[table.Name] = set
	}

	account := declared["dist_account"]
	ledger := declared["dist_ledger"]
	if account == nil || ledger == nil {
		t.Fatal("dist_account and dist_ledger must both be in the schema")
	}

	seen := map[string]string{}
	for i, b := range buckets {
		c := cols[i]

		if !account[c.Value] {
			t.Errorf("bucket %v maps to dist_account.%s, which the schema does not declare", b, c.Value)
		}
		if !ledger[c.Delta] {
			t.Errorf("bucket %v maps to dist_ledger.%s, which the schema does not declare", b, c.Delta)
		}
		if !ledger[c.After] {
			t.Errorf("bucket %v maps to dist_ledger.%s, which the schema does not declare", b, c.After)
		}

		// Two buckets sharing a column would silently reconcile one of them
		// twice and the other never.
		for _, name := range []string{c.Value, c.Delta, c.After} {
			if prev, dup := seen[name]; dup {
				t.Errorf("column %s is claimed by both bucket %v and bucket %v", name, prev, b)
			}
			seen[name] = b.String()
		}
	}
}

// TestNoBucketIsSilentlyUnmapped pins the failure direction: an unknown bucket
// must be an error, not a zero value that produces a query comparing nothing.
func TestNoBucketIsSilentlyUnmapped(t *testing.T) {
	if _, ok := sqlstore.BucketColumnsForTest(distledger.Bucket(200)); ok {
		t.Fatal("an unknown bucket must not resolve to columns")
	}
	for _, b := range distledger.AllBuckets() {
		if _, ok := sqlstore.BucketColumnsForTest(b); !ok {
			t.Fatalf("bucket %v must resolve", b)
		}
	}
}
