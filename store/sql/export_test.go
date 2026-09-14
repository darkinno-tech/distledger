package sqlstore

import "github.com/darkinno-tech/distledger"

// Test-only views of unexported details.
//
// These live in a _test.go file so they are never part of the library's API. They
// exist because the properties worth pinning here are about how names are
// derived, and a derivation is only testable if the test can call it with inputs
// the current schema does not happen to contain.

// IndexNameForTest exposes the index-name derivation so a test can assert that a
// name depends on the columns and on nothing else.
func IndexNameForTest(table, kind string, cols []string) string {
	return indexName(table, kind, cols)
}

// MaxIdentifierLenForTest exposes the identifier budget.
const MaxIdentifierLenForTest = maxIdentifierLen

// BucketColForTest is one bucket's three storage columns.
type BucketColForTest struct {
	Value string
	Delta string
	After string
}

// BucketColumnsForTest exposes the bucket-to-column mapping so a test can assert
// that every bucket the domain declares is reachable by the reconciliation
// query, and that the columns it names exist in the schema.
func BucketColumnsForTest(b distledger.Bucket) (BucketColForTest, bool) {
	c, ok := bucketColumns(b)
	return BucketColForTest{Value: c.value, Delta: c.delta, After: c.after}, ok
}

// ResolveBucketColumnsForTest exposes the resolved table over every bucket.
func ResolveBucketColumnsForTest() ([]BucketColForTest, error) {
	cols, err := resolveBucketColumns()
	if err != nil {
		return nil, err
	}
	out := make([]BucketColForTest, 0, len(cols))
	for _, c := range cols {
		out = append(out, BucketColForTest{Value: c.value, Delta: c.delta, After: c.after})
	}
	return out, nil
}
