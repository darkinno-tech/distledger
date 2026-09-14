package sqlstore

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
