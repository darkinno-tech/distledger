// Package sqlite provides the SQLite dialect for the portable store in
// store/sql.
//
// Like the other dialect packages it imports no driver and opens no connections.
// Callers open their own *sql.DB with whichever driver they already depend on —
// modernc.org/sqlite (pure Go, no cgo) or github.com/mattn/go-sqlite3 — and hand
// it to the store.
//
// Typical wiring, where _ "modernc.org/sqlite" is the caller's own import:
//
//	db, err := sql.Open("sqlite", "file:ledger.db?_pragma=journal_mode(WAL)")
//	if err != nil {
//		return err
//	}
//	store, err := sqlstore.Open(db, sqlite.Dialect{})
//
// # Deployment note
//
// SQLite serialises writers. The store therefore relies on the retry loop for
// contended writers rather than on row locks: "database is locked" is classified
// as retryable, and a writer that loses the race rolls back and tries again. For
// anything beyond a single node, use MySQL or PostgreSQL.
package sqlite

import (
	"database/sql"
	"fmt"
	"strings"

	sqlstore "github.com/darkinno-tech/distledger/store/sql"
)

// Dialect implements sqlstore.Dialect for SQLite 3.35 and later, which is the
// version that introduced RETURNING.
type Dialect struct{}

var _ sqlstore.Dialect = Dialect{}

// Name implements sqlstore.Dialect.
func (Dialect) Name() string { return "sqlite" }

// Placeholder implements sqlstore.Dialect. SQLite uses "?" for every position.
func (Dialect) Placeholder(int) string { return "?" }

// Quote implements sqlstore.Dialect.
func (Dialect) Quote(name string) string { return `"` + strings.ReplaceAll(name, `"`, `""`) + `"` }

// TypeName implements sqlstore.Dialect.
func (Dialect) TypeName(kind sqlstore.ColumnKind, size int) string {
	switch kind {
	case sqlstore.KindInt64, sqlstore.KindInstant:
		// SQLite has dynamic typing and a single integer affinity, so BIGINT and
		// the instant representation collapse to the same declaration.
		return "INTEGER"
	case sqlstore.KindString:
		// VARCHAR(n) is accepted for compatibility but not enforced; the root
		// package enforces the limits before a value ever reaches the store.
		if size <= 0 {
			size = 255
		}
		return fmt.Sprintf("VARCHAR(%d)", size)
	default:
		return "BLOB"
	}
}

// TableSuffix implements sqlstore.Dialect.
func (Dialect) TableSuffix() string { return "" }

// IndexDDL implements sqlstore.Dialect. SQLite understands IF NOT EXISTS.
func (Dialect) IndexDDL(table, index string, cols []string, _ map[string]sqlstore.ColumnKind, unique bool) string {
	kind := "CREATE INDEX IF NOT EXISTS "
	if unique {
		kind = "CREATE UNIQUE INDEX IF NOT EXISTS "
	}
	quoted := make([]string, 0, len(cols))
	for _, c := range cols {
		quoted = append(quoted, (Dialect{}).Quote(c))
	}
	return fmt.Sprintf("%s%s ON %s (%s)",
		kind, (Dialect{}).Quote(index), (Dialect{}).Quote(table), strings.Join(quoted, ", "))
}

// AutoIDColumn implements sqlstore.Dialect.
//
// The column must be declared "INTEGER PRIMARY KEY" for SQLite to treat it as the
// rowid alias and assign values, which is why the primary key is inlined here and
// AutoIDIsPrimary reports true. Declaring BIGINT instead would produce a column
// that does not auto-assign, and inserts would fail on a NULL key.
//
// AUTOINCREMENT is deliberately omitted: it adds a sqlite_sequence table and
// forbids id reuse, and this library does not require either. Rowids may be
// recycled after a delete, but nothing here deletes.
func (Dialect) AutoIDColumn() string { return "INTEGER PRIMARY KEY" }

// AutoIDIsPrimary implements sqlstore.Dialect.
func (Dialect) AutoIDIsPrimary() bool { return true }

// InsertID implements sqlstore.Dialect.
func (Dialect) InsertID() sqlstore.InsertIDStrategy { return sqlstore.InsertIDReturning }

// InsertConflictClause implements sqlstore.Dialect.
//
// SQLite aborts the failing statement, not the transaction, so the error can be
// classified and no clause is needed.
func (Dialect) InsertConflictClause() string { return "" }

// IsDuplicateKey implements sqlstore.Dialect.
func (Dialect) IsDuplicateKey(err error) bool { return sqlstore.Detect.IsDuplicateKey(err) }

// IsRetryable implements sqlstore.Dialect.
//
// "database is locked" and SQLITE_BUSY are the normal outcome of two writers
// contending, not a failure worth surfacing to the caller.
func (Dialect) IsRetryable(err error) bool { return sqlstore.Detect.IsRetryable(err) }

// IsAlreadyExists implements sqlstore.Dialect.
func (Dialect) IsAlreadyExists(err error) bool { return sqlstore.Detect.IsAlreadyExists(err) }

// Open wraps an already-open database handle with this dialect.
//
// It is a thin convenience over sqlstore.Open, and it exists so a caller does not
// have to name the dialect type explicitly. The handle is still the caller's: the
// library opens no connections and closes none.
func Open(db *sql.DB, opts ...sqlstore.Option) (*sqlstore.Store, error) {
	return sqlstore.Open(db, Dialect{}, opts...)
}
