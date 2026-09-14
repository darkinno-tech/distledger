// Package mysql provides the MySQL dialect for the portable store in store/sql.
//
// It deliberately does not import a MySQL driver, and it does not open
// connections. Callers already depend on whichever driver they use, so they open
// their own *sql.DB and hand it to the store; this package only describes how
// MySQL differs from the other backends.
//
// Typical wiring, where _ "github.com/go-sql-driver/mysql" is the caller's own
// import:
//
//	db, err := sql.Open("mysql", dsn)
//	if err != nil {
//		return err
//	}
//	store, err := sqlstore.Open(db, mysql.Dialect{})
//
// The import of the driver lives in the caller's module, never in this one. That
// is what keeps the library's own dependency list empty.
package mysql

import (
	"fmt"
	"strings"

	sqlstore "github.com/im10furry/distledger/store/sql"
)

// Dialect implements sqlstore.Dialect for MySQL 5.7 and later, including MariaDB.
type Dialect struct{}

var _ sqlstore.Dialect = Dialect{}

// Name implements sqlstore.Dialect.
func (Dialect) Name() string { return "mysql" }

// Placeholder implements sqlstore.Dialect. MySQL uses "?" for every position.
func (Dialect) Placeholder(int) string { return "?" }

// Quote implements sqlstore.Dialect. MySQL quotes identifiers with backticks,
// which also means a backtick inside a name would have to be doubled. Every
// identifier in the portable schema is a fixed literal, so no escaping path is
// provided: accepting caller-supplied identifiers here would be the start of an
// injection surface.
func (Dialect) Quote(name string) string { return "`" + strings.ReplaceAll(name, "`", "``") + "`" }

// TypeName implements sqlstore.Dialect.
func (Dialect) TypeName(kind sqlstore.ColumnKind, size int) string {
	switch kind {
	case sqlstore.KindInt64:
		return "BIGINT"
	case sqlstore.KindString:
		if size <= 0 {
			size = 255
		}
		return fmt.Sprintf("VARCHAR(%d)", size)
	case sqlstore.KindBytes:
		return "LONGBLOB"
	default:
		// Instants are stored as Unix nanoseconds. BIGINT rather than DATETIME
		// because DATETIME has no time zone, and a ledger that changes meaning
		// with the server's zone setting is not a ledger.
		return "BIGINT"
	}
}

// TableSuffix implements sqlstore.Dialect.
func (Dialect) TableSuffix() string { return " ENGINE=InnoDB DEFAULT CHARSET=utf8mb4" }

// IndexDDL implements sqlstore.Dialect.
//
// MySQL has no CREATE INDEX IF NOT EXISTS, so the migration tolerates the
// duplicate-key-name error instead; see IsAlreadyExists.
func (Dialect) IndexDDL(table, index string, cols []string, _ map[string]sqlstore.ColumnKind, unique bool) string {
	kind := "CREATE INDEX "
	if unique {
		kind = "CREATE UNIQUE INDEX "
	}
	quoted := make([]string, 0, len(cols))
	for _, c := range cols {
		quoted = append(quoted, (Dialect{}).Quote(c))
	}
	return fmt.Sprintf("%s%s ON %s (%s)",
		kind, (Dialect{}).Quote(index), (Dialect{}).Quote(table), strings.Join(quoted, ", "))
}

// AutoIDColumn implements sqlstore.Dialect.
func (Dialect) AutoIDColumn() string { return "BIGINT NOT NULL AUTO_INCREMENT" }

// AutoIDIsPrimary implements sqlstore.Dialect.
//
// MySQL needs the separate table-level PRIMARY KEY clause, because
// AUTO_INCREMENT does not imply a key.
func (Dialect) AutoIDIsPrimary() bool { return false }

// InsertID implements sqlstore.Dialect. MySQL reports generated ids through
// LastInsertId, not through RETURNING.
func (Dialect) InsertID() sqlstore.InsertIDStrategy { return sqlstore.InsertIDLastInsert }

// IsDuplicateKey implements sqlstore.Dialect.
func (Dialect) IsDuplicateKey(err error) bool { return sqlstore.Detect.IsDuplicateKey(err) }

// IsRetryable implements sqlstore.Dialect.
func (Dialect) IsRetryable(err error) bool { return sqlstore.Detect.IsRetryable(err) }

// IsAlreadyExists implements sqlstore.Dialect.
func (Dialect) IsAlreadyExists(err error) bool { return sqlstore.Detect.IsAlreadyExists(err) }
