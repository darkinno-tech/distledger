// Package sqlstore is the portable SQL core behind the distledger store port.
//
// # Why one core and not one store per database
//
// The port has a couple of dozen methods, and every one of them involves
// transactions, optimistic locking and idempotency conflicts. Writing that logic
// once per database would mean several copies of the accounting rules that drift
// apart, and a fix applied to only one of them raises no error (see ADR-033).
//
// Instead, statement building, row scanning, transaction and retry handling,
// version checks and duplicate-key handling live here, once. Each database gets a
// package that supplies a Dialect and nothing else.
//
// # No driver, ever
//
// This package imports database/sql and nothing else. The library ships no
// database driver: callers open their own *sql.DB with whichever driver they
// already depend on and hand it over. That is what keeps the library's go.mod
// require block empty, and it is a hard rule rather than a preference.
//
// # Reading driver errors without importing drivers
//
// Classifying a duplicate-key violation or a deadlock normally means importing
// the driver's error type. Doing so here would drag that driver into every
// caller's module graph, which is exactly what the no-driver rule forbids.
//
// So detection is structural: a driver error is inspected for the exported shape
// it presents (a Number or Code field, an SQLState or Number method) rather than
// for its concrete type. See errorCode. Callers whose driver exposes neither can
// implement Classifier and pass it in Config.
package sqlstore

import (
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"time"
)

// ColumnKind is the portable type of a column. Dialects translate it into
// whatever their database calls that type.
//
// The portable schema is written in these kinds rather than in SQL types so that
// every dialect is guaranteed to produce the same columns. A dialect that omits a
// column would otherwise be a silent schema divergence between backends.
type ColumnKind uint8

const (
	// KindInt64 is a 64-bit signed integer. Money, rates, ids and counters.
	KindInt64 ColumnKind = iota
	// KindString is a variable-length string of a bounded size.
	KindString
	// KindBytes is a BLOB/BYTEA column.
	KindBytes
	// KindInstant is a point in time, stored as an integer count of nanoseconds
	// since the Unix epoch. See encodeInstant for why it is not a native
	// timestamp type.
	KindInstant
)

// Dialect describes everything that genuinely differs between SQL databases.
//
// It is deliberately small. Anything that can be expressed in portable SQL does
// not belong here, because every method added to this interface is a method three
// packages have to implement and could get subtly wrong.
type Dialect interface {
	// Name identifies the dialect, for example "mysql". It appears in error
	// messages and in SelfCheck output.
	Name() string

	// Placeholder returns the bind marker for the n-th parameter, counting from
	// one. MySQL and SQLite use "?" for every position; PostgreSQL uses "$1",
	// "$2" and so on.
	Placeholder(n int) string

	// Quote renders an identifier, for example a table or column name.
	Quote(name string) string

	// TypeName renders a portable column kind as this database's type. size is
	// the declared length for KindString and is ignored otherwise.
	TypeName(kind ColumnKind, size int) string

	// TableSuffix is appended after the closing parenthesis of CREATE TABLE, for
	// engines that need to be told which storage engine or charset to use.
	TableSuffix() string

	// IndexDDL renders a CREATE INDEX statement. colTypes gives each indexed
	// column's kind so that dialects which need a prefix length on indexed
	// strings can ask for one.
	IndexDDL(table, index string, cols []string, colTypes map[string]ColumnKind, unique bool) string

	// IsDuplicateKey reports whether err is a unique-constraint violation.
	//
	// The store relies on it to turn a duplicate idempotency key into
	// ErrDuplicate, which is a normal outcome rather than a failure.
	IsDuplicateKey(err error) bool

	// IsRetryable reports whether err is a transient failure that may be retried
	// after the transaction has been rolled back: a deadlock, a lock timeout, a
	// serialization failure.
	IsRetryable(err error) bool

	// AutoIDColumn renders the type and constraints of a database-generated 64-bit
	// primary key column, for example "BIGINT NOT NULL AUTO_INCREMENT".
	//
	// It must NOT include the column name: the caller writes the quoted name, and
	// returning it here as well produced a statement with the name twice.
	//
	// Letting the database allocate ids is the portable choice: the alternatives
	// are a shared sequence row, which serializes every insert, or ids generated
	// in the store, which need a scheme the library has no business inventing.
	AutoIDColumn() string

	// AutoIDIsPrimary reports whether AutoIDColumn already declares the primary
	// key, in which case the table-level PRIMARY KEY clause must be omitted.
	//
	// This is a genuine dialect difference rather than a convenience: SQLite only
	// treats a column as the rowid alias, and therefore only auto-assigns it, when
	// "INTEGER PRIMARY KEY" is part of the column definition itself.
	AutoIDIsPrimary() bool

	// InsertID reports how the dialect returns the id of a freshly inserted row.
	InsertID() InsertIDStrategy

	// InsertConflictClause returns the clause that turns a unique-constraint
	// collision into a no-op insert, or "" when the dialect reports the collision
	// as an error instead.
	//
	// This is not a stylistic difference. PostgreSQL aborts the ENTIRE
	// transaction when any statement fails, so there the "insert and catch the
	// duplicate error" pattern is unusable: the transaction is already poisoned
	// and the eventual COMMIT turns into a rollback. The collision has to be
	// absorbed by the statement itself. MySQL and SQLite leave the transaction
	// usable after a constraint error, so they can raise and be classified.
	//
	// PostgreSQL: " ON CONFLICT DO NOTHING". MySQL and SQLite: "".
	InsertConflictClause() string

	// IsAlreadyExists reports whether err means the object being created is
	// already there.
	//
	// Migrations have to be re-runnable. CREATE TABLE IF NOT EXISTS covers the
	// tables portably, but MySQL has no IF NOT EXISTS for CREATE INDEX, so the
	// migration tolerates the "duplicate key name" error instead.
	IsAlreadyExists(err error) bool
}

// InsertIDStrategy says how a dialect reports a generated id.
type InsertIDStrategy uint8

const (
	// InsertIDLastInsert reads it from the driver's LastInsertId, which is what
	// MySQL offers.
	InsertIDLastInsert InsertIDStrategy = iota
	// InsertIDReturning appends "RETURNING <column>" to the INSERT, which is what
	// PostgreSQL and SQLite offer.
	InsertIDReturning
)

// Classifier lets a caller supply database-specific error classification.
//
// It exists for drivers whose errors expose neither a numeric nor a string code,
// which the structural detection in Detect cannot read. Implementations may embed
// FallbackClassifier to keep the built-in behaviour and add to it.
type Classifier interface {
	IsDuplicateKey(err error) bool
	IsRetryable(err error) bool
}

// FallbackClassifier performs the structural detection described in the package
// documentation. Embed it in a custom Classifier to extend rather than replace it.
type FallbackClassifier struct{}

// IsDuplicateKey implements Classifier.
func (FallbackClassifier) IsDuplicateKey(err error) bool { return Detect.duplicateKey(err) }

// IsRetryable implements Classifier.
func (FallbackClassifier) IsRetryable(err error) bool { return Detect.retryable(err) }

// IsAlreadyExists reports whether err means the object already exists.
func (FallbackClassifier) IsAlreadyExists(err error) bool { return alreadyExists(err) }

func alreadyExists(err error) bool {
	num, str, ok := errorCode(err)
	if ok {
		switch {
		case num == 1050, num == 1061: // MySQL: table exists, duplicate key name
			return true
		case str == "42P07", str == "42710": // PostgreSQL: duplicate table/object
			return true
		}
	}
	return containsPhrase(err, []string{"already exists", "duplicate key name"})
}

// Detect is the built-in structural classifier.
var Detect FallbackClassifier

// errorCode extracts a numeric code and a string code from an error, if either is
// present.
//
// It looks for the shapes database drivers actually use: a numeric Number field
// (go-sql-driver/mysql), a numeric Code field (lib/pq, SQLite), an SQLState
// method (pgx, lib/pq) or a Number method. Neither the drivers nor their packages
// are imported: matching on shape is what keeps a caller's driver out of this
// module's dependency graph.
func errorCode(err error) (num uint32, str string, ok bool) {
	if err == nil {
		return 0, "", false
	}

	type numberMethod interface{ Number() uint16 }
	type codeMethod interface{ SQLState() string }

	var nm numberMethod
	if errors.As(err, &nm) {
		return uint32(nm.Number()), "", true
	}
	var cm codeMethod
	if errors.As(err, &cm) {
		if s := cm.SQLState(); s != "" {
			return 0, s, true
		}
	}

	// Field access needs reflection: a driver's error type carries the code in an
	// exported field, and matching a field without importing the type is not
	// something the language offers. This runs only on the error path.
	//
	// errors.As cannot help here, because it matches methods and not fields, so
	// the wrap chain is walked explicitly.
	for e := err; e != nil; e = errors.Unwrap(e) {
		if num, str, ok := errorFields(e); ok {
			return num, str, true
		}
	}
	return 0, "", false
}

// errorFields looks for a numeric or textual code on the error value itself.
func errorFields(err error) (num uint32, str string, ok bool) {
	v := reflect.ValueOf(err)
	for v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return 0, "", false
		}
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return 0, "", false
	}
	for _, name := range []string{"Number", "Code"} {
		f := v.FieldByName(name)
		if !f.IsValid() || !f.CanInterface() {
			continue
		}
		switch f.Kind() {
		case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
			return uint32(f.Uint()), "", true
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			if f.Int() >= 0 {
				return uint32(f.Int()), "", true
			}
		case reflect.String:
			if s := f.String(); s != "" {
				return 0, s, true
			}
		}
	}
	return 0, "", false
}

// messages that are matched when a driver exposes no code at all.
//
// String matching is the last resort, and it is deliberately confined to a small
// list of phrases that have been stable across driver versions for years. A
// driver that changes its wording makes detection fail closed: an unrecognised
// duplicate becomes a hard error rather than a silent double booking, which is
// the safe direction.
var (
	duplicatePhrases = []string{
		"duplicate entry",                 // MySQL
		"duplicate key value",             // PostgreSQL
		"duplicate key",                   // SQLite, SQL Server
		"unique constraint",               // SQLite, PostgreSQL
		"unique violation",                // generic
		"cannot insert duplicate key row", // SQL Server
	}
	retryablePhrases = []string{
		"deadlock",
		"lock wait timeout",
		"could not serialize",
		"serialization failure",
		"database is locked", // SQLite
		"try restarting transaction",
	}
)

func containsPhrase(err error, phrases []string) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, p := range phrases {
		if strings.Contains(msg, p) {
			return true
		}
	}
	return false
}

func (FallbackClassifier) duplicateKey(err error) bool {
	if _, _, ok := errorCode(err); ok {
		if numericDuplicate(err) {
			return true
		}
	}
	return containsPhrase(err, duplicatePhrases)
}

func (FallbackClassifier) retryable(err error) bool {
	return numericRetryable(err) || containsPhrase(err, retryablePhrases)
}

// numericDuplicate recognises the duplicate-key codes of the databases this
// library ships a dialect for.
func numericDuplicate(err error) bool {
	num, str, ok := errorCode(err)
	if !ok {
		return false
	}
	switch {
	case num == 1062: // MySQL ER_DUP_ENTRY
		return true
	case num == 2601 || num == 2627: // SQL Server unique index / constraint
		return true
	case num == 19: // SQLite SQLITE_CONSTRAINT
		return true
	case str == "23505": // PostgreSQL unique_violation
		return true
	case str == "23000": // MySQL SQLSTATE for integrity constraint violation
		return true
	default:
		return false
	}
}

// numericRetryable recognises the transient failures worth retrying.
func numericRetryable(err error) bool {
	num, str, ok := errorCode(err)
	if !ok {
		return false
	}
	switch {
	case num == 1213, num == 1205: // MySQL deadlock, lock wait timeout
		return true
	case num == 5, num == 6: // SQLite SQLITE_BUSY, SQLITE_LOCKED
		return true
	case str == "40001", str == "40P01": // serialization_failure, deadlock_detected
		return true
	default:
		return false
	}
}

// args accumulates bind parameters and renders their placeholders.
//
// Callers must not build a statement by formatting values into it. Keeping the
// two together means the placeholder count and the argument count cannot drift,
// which is the usual cause of a statement that binds the wrong column.
type args struct {
	dialect Dialect
	vals    []any
}

func newArgs(d Dialect) *args { return &args{dialect: d} }

// add appends a value and returns the placeholder that binds it.
func (a *args) add(v any) string {
	a.vals = append(a.vals, v)
	return a.dialect.Placeholder(len(a.vals))
}

// ints appends a slice of int64 values and returns their placeholders.
func (a *args) ints(vs []int64) []string {
	out := make([]string, 0, len(vs))
	for _, v := range vs {
		out = append(out, a.add(v))
	}
	return out
}

// Instants are stored as NULL when absent rather than as a sentinel integer.
//
// A zero time.Time means "this has not happened yet": a commission whose buyer
// has not confirmed receipt has no settlement date, a binding may never expire.
// Mapping that to a magic number would put a value into the same column that
// also has to hold real timestamps, and the query that finds due commissions
// would then have to know the magic number. NULL says it directly, and SQL
// indexes it well.
// instantOrNull encodes an instant for storage, mapping the zero time to NULL.
func instantOrNull(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC().UnixNano()
}

// nullInstant decodes an instant column, mapping NULL back to the zero time.
func nullInstant(v sql.NullInt64) time.Time {
	if !v.Valid {
		return time.Time{}
	}
	return time.Unix(0, v.Int64).UTC()
}

// nullableString returns nil for the empty string so that optional text columns
// stay NULL rather than becoming empty strings, which are indistinguishable from
// "explicitly set to empty" in later queries.
func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// stringOrEmpty decodes a nullable text column.
func stringOrEmpty(v sql.NullString) string {
	if !v.Valid {
		return ""
	}
	return v.String
}

// parseUintColumn reads a value that a driver may hand back as either an integer
// or a string.
//
// MySQL returns integers for integer columns, but some drivers and some column
// affinities return decimal text. Parsing both keeps the scanner independent of
// that choice.
func parseUintColumn(v any) (uint64, error) {
	switch t := v.(type) {
	case nil:
		return 0, nil
	case int64:
		return uint64(t), nil
	case uint64:
		return t, nil
	case []byte:
		return strconv.ParseUint(string(t), 10, 64)
	case string:
		return strconv.ParseUint(t, 10, 64)
	default:
		return 0, fmt.Errorf("sqlstore: cannot read %T as an integer", v)
	}
}
