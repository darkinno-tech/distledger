package sqlstore

import (
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"
)

// Driver-shaped error doubles.
//
// These mimic the exported surface of real drivers without importing them, which
// is exactly the contract errorCode depends on. If the structural detection ever
// stops recognising one of these shapes, these tests fail.

// mysqlErr mirrors go-sql-driver/mysql's *MySQLError: a numeric Number field.
type mysqlErr struct {
	Number  uint16
	Message string
}

func (e *mysqlErr) Error() string { return fmt.Sprintf("Error %d: %s", e.Number, e.Message) }

// pgErr mirrors pgx's *pgconn.PgError, which exposes SQLState as a method.
type pgErr struct {
	Code    string
	Message string
}

func (e *pgErr) Error() string    { return e.Message + " (SQLSTATE " + e.Code + ")" }
func (e *pgErr) SQLState() string { return e.Code }

// sqliteErr mirrors modernc.org/sqlite's error, which carries a numeric Code.
type sqliteErr struct {
	Code uint32
	Msg  string
}

func (e *sqliteErr) Error() string { return fmt.Sprintf("%s (%d)", e.Msg, e.Code) }

func TestErrorCodeReadsDriverShapes(t *testing.T) {
	tests := []struct {
		name    string
		err     error
		wantNum uint32
		wantStr string
		wantOK  bool
	}{
		{"nil", nil, 0, "", false},
		{"plain error", errors.New("something went wrong"), 0, "", false},
		{"mysql number field", &mysqlErr{Number: 1062}, 1062, "", true},
		{"postgres sqlstate method", &pgErr{Code: "23505"}, 0, "23505", true},
		{"sqlite code field", &sqliteErr{Code: 19}, 19, "", true},
		{"wrapped mysql error", fmt.Errorf("insert commission: %w", &mysqlErr{Number: 1213}), 1213, "", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			num, str, ok := errorCode(tc.err)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if ok && (num != tc.wantNum || str != tc.wantStr) {
				t.Fatalf("got (%d, %q), want (%d, %q)", num, str, tc.wantNum, tc.wantStr)
			}
		})
	}
}

func TestDetectDuplicateKey(t *testing.T) {
	duplicates := []error{
		&mysqlErr{Number: 1062, Message: "Duplicate entry 'x' for key 'idem'"},
		&pgErr{Code: "23505", Message: "duplicate key value violates unique constraint"},
		&sqliteErr{Code: 19, Msg: "constraint failed"},
		errors.New("Error 1062: Duplicate entry"),
		errors.New("UNIQUE constraint failed: dist_commission.idem_key"),
		errors.New("cannot insert duplicate key row in object"),
	}
	for _, err := range duplicates {
		if !Detect.IsDuplicateKey(err) {
			t.Errorf("not detected as duplicate: %v", err)
		}
	}

	// A recognised code that is NOT a duplicate must not be mistaken for one,
	// even when the message happens to contain a suggestive word.
	notDuplicates := []error{
		nil,
		errors.New("connection refused"),
		&mysqlErr{Number: 1213, Message: "Deadlock found"},
		&pgErr{Code: "40001", Message: "could not serialize access"},
	}
	for _, err := range notDuplicates {
		if Detect.IsDuplicateKey(err) {
			t.Errorf("wrongly detected as duplicate: %v", err)
		}
	}
}

func TestDetectRetryable(t *testing.T) {
	retryable := []error{
		&mysqlErr{Number: 1213, Message: "Deadlock found when trying to get lock"},
		&mysqlErr{Number: 1205, Message: "Lock wait timeout exceeded"},
		&pgErr{Code: "40001", Message: "could not serialize access due to concurrent update"},
		&pgErr{Code: "40P01", Message: "deadlock detected"},
		&sqliteErr{Code: 5, Msg: "database is locked"},
		errors.New("database is locked"),
	}
	for _, err := range retryable {
		if !Detect.IsRetryable(err) {
			t.Errorf("not detected as retryable: %v", err)
		}
	}

	notRetryable := []error{
		nil,
		&mysqlErr{Number: 1062, Message: "Duplicate entry"},
		errors.New("syntax error"),
	}
	for _, err := range notRetryable {
		if Detect.IsRetryable(err) {
			t.Errorf("wrongly detected as retryable: %v", err)
		}
	}
}

func TestDetectAlreadyExists(t *testing.T) {
	existing := []error{
		&mysqlErr{Number: 1050, Message: "Table 'dist_agent' already exists"},
		&mysqlErr{Number: 1061, Message: "Duplicate key name 'dist_agent_idx_0'"},
		&pgErr{Code: "42P07", Message: "relation already exists"},
		errors.New("index dist_ledger_idx_0 already exists"),
	}
	for _, err := range existing {
		if !Detect.IsAlreadyExists(err) {
			t.Errorf("not detected as already existing: %v", err)
		}
	}
	for _, err := range []error{nil, errors.New("syntax error")} {
		if Detect.IsAlreadyExists(err) {
			t.Errorf("wrongly detected as already existing: %v", err)
		}
	}
}

func TestFallbackClassifierSatisfiesDialectContracts(t *testing.T) {
	// The classifier is what every dialect delegates to, so its answers must
	// agree with the package-level Detect value.
	var c Classifier = FallbackClassifier{}

	duplicateEntry := &mysqlErr{Number: 1062, Message: "Duplicate entry"}
	if !c.IsDuplicateKey(duplicateEntry) || c.IsRetryable(duplicateEntry) {
		t.Fatal("FallbackClassifier disagrees with Detect on a duplicate entry")
	}
	// A duplicate *entry* is a row conflict, not a schema object that already
	// exists, so it must not be reported as the latter. Conflating the two would
	// make the migration swallow a real constraint violation.
	if (FallbackClassifier{}).IsAlreadyExists(duplicateEntry) {
		t.Fatal("1062 is a duplicate row, not an existing schema object")
	}
	// 1061 is the duplicate *index name* code, which is what a re-run migration
	// produces and what must be tolerated.
	if !(FallbackClassifier{}).IsAlreadyExists(&mysqlErr{Number: 1061}) {
		t.Fatal("1061 should be classified as already existing")
	}
}

func TestArgsPlaceholdersTrackDialect(t *testing.T) {
	tests := []struct {
		name string
		d    Dialect
		want []string
	}{
		{"question marks", questionDialect{}, []string{"?", "?", "?"}},
		{"numbered", numberedDialect{}, []string{"$1", "$2", "$3"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := newArgs(tc.d)
			got := []string{}
			for i := 0; i < 3; i++ {
				got = append(got, a.add(i))
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Fatalf("placeholder %d = %q, want %q", i, got[i], tc.want[i])
				}
			}
			if len(a.vals) != 3 {
				t.Fatalf("bound %d values, want 3", len(a.vals))
			}
			// The bound values must be in call order: a placeholder that does not
			// line up with its value is how a statement ends up writing the wrong
			// column.
			for i, v := range a.vals {
				if v != i {
					t.Fatalf("value %d = %v, want %d", i, v, i)
				}
			}
		})
	}
}

func TestArgsInts(t *testing.T) {
	a := newArgs(questionDialect{})
	got := a.ints([]int64{10, 20})
	if len(got) != 2 || got[0] != "?" || got[1] != "?" {
		t.Fatalf("ints = %v", got)
	}
	if len(a.vals) != 2 || a.vals[0] != int64(10) || a.vals[1] != int64(20) {
		t.Fatalf("vals = %v", a.vals)
	}
	// An empty slice must not append a placeholder.
	if got := a.ints(nil); len(got) != 0 {
		t.Fatalf("ints(nil) = %v, want empty", got)
	}
}

func TestInstantEncodingUsesNullForZero(t *testing.T) {
	if got := instantOrNull(time.Time{}); got != nil {
		t.Fatalf("zero time encoded as %v, want nil", got)
	}

	at := time.Date(2026, 3, 4, 5, 6, 7, 123456789, time.UTC)
	got := instantOrNull(at)
	if got == nil {
		t.Fatal("a real instant encoded as nil")
	}
	if got != at.UnixNano() {
		t.Fatalf("encoded %v, want %d", got, at.UnixNano())
	}

	// Round trip through the nullable column representation.
	back := nullInstant(sql.NullInt64{Int64: got.(int64), Valid: true})
	if !back.Equal(at) {
		t.Fatalf("round trip produced %s, want %s", back, at)
	}

	if got := nullInstant(sql.NullInt64{}); !got.IsZero() {
		t.Fatalf("NULL decoded as %s, want the zero time", got)
	}

	// A non-UTC location must survive as the same instant.
	inZone := at.In(time.FixedZone("CST", 8*3600))
	if !nullInstant(sql.NullInt64{Int64: instantOrNull(inZone).(int64), Valid: true}).Equal(at) {
		t.Fatal("encoding is not location independent")
	}
}

func TestNullableStringDistinguishesEmptyFromAbsent(t *testing.T) {
	if got := nullableString(""); got != nil {
		t.Fatalf("empty string encoded as %v, want nil", got)
	}
	if got := nullableString("x"); got != "x" {
		t.Fatalf("encoded %q, want \"x\"", got)
	}
	if got := stringOrEmpty(sql.NullString{Valid: false}); got != "" {
		t.Fatalf("NULL decoded as %q", got)
	}
	if got := stringOrEmpty(sql.NullString{String: "x", Valid: true}); got != "x" {
		t.Fatalf("decoded %q, want \"x\"", got)
	}
}

func TestParseUintColumnAcceptsDriverVariations(t *testing.T) {
	tests := []struct {
		in      any
		want    uint64
		wantErr bool
	}{
		{nil, 0, false},
		{int64(42), 42, false},
		{uint64(42), 42, false},
		{[]byte("42"), 42, false},
		{"42", 42, false},
		{[]byte("not a number"), 0, true},
		{3.5, 0, true},
	}
	for _, tc := range tests {
		got, err := parseUintColumn(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseUintColumn(%#v) should have failed", tc.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseUintColumn(%#v) = %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("parseUintColumn(%#v) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// questionDialect and numberedDialect are minimal dialects for exercising the
// portable helpers without pulling in a real backend. They implement only what
// the helpers touch and panic on the rest, so a helper that starts relying on
// something unexpected fails loudly here.
type questionDialect struct{ Dialect }

func (questionDialect) Placeholder(int) string { return "?" }

type numberedDialect struct{ Dialect }

func (numberedDialect) Placeholder(n int) string { return "$" + string(rune('0'+n)) }
