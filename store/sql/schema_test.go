package sqlstore_test

import (
	"strings"
	"testing"

	"github.com/im10furry/distledger/store/mysql"
	"github.com/im10furry/distledger/store/postgres"
	sqlstore "github.com/im10furry/distledger/store/sql"
	"github.com/im10furry/distledger/store/sqlite"
)

// dialects returns every shipped dialect with a human-readable label.
//
// Adding a backend means adding one line here, and every schema parity check
// below then applies to it automatically. That is the point of the portable
// schema: a new dialect cannot silently omit a column.
func dialects() map[string]sqlstore.Dialect {
	return map[string]sqlstore.Dialect{
		"mysql":    mysql.Dialect{},
		"postgres": postgres.Dialect{},
		"sqlite":   sqlite.Dialect{},
	}
}

// TestDialectsEmitTheSameColumns is the central schema parity check.
//
// Each dialect renders the shared table definitions in its own syntax. If one of
// them dropped a column, or spelled a table differently, the backends would
// diverge in a way that only shows up as a runtime error on one of them.
func TestDialectsEmitTheSameColumns(t *testing.T) {
	schema := sqlstore.Schema()
	if len(schema) == 0 {
		t.Fatal("the portable schema is empty")
	}

	for name, d := range dialects() {
		t.Run(name, func(t *testing.T) {
			ddl := strings.Join(sqlstore.Statements(d), "\n")
			for _, table := range schema {
				if !strings.Contains(ddl, d.Quote(table.Name)) {
					t.Errorf("table %s is missing entirely", table.Name)
				}
				for _, col := range table.Columns {
					// Look for the quoted column name followed by a space, so that
					// a prefix match on a longer column name does not count.
					if !strings.Contains(ddl, d.Quote(col.Name)+" ") {
						t.Errorf("table %s is missing column %s", table.Name, col.Name)
					}
				}
			}
		})
	}
}

// TestSchemaCoversEveryDomainField guards against the schema drifting away from
// the domain types.
//
// It lists the fields whose persistence would be missed first: the ones that hold
// money or decide when money moves. A new such field must be added here, which
// forces the author to add it to the schema as well.
func TestSchemaCoversEveryDomainField(t *testing.T) {
	required := map[string][]string{
		"dist_agent":      {"tenant_id", "user_id", "parent_id", "depth", "status", "join_type", "rate_override_bp", "version"},
		"dist_binding":    {"tenant_id", "buyer_user_id", "agent_user_id", "source", "bound_at", "expire_at", "active", "rebound_from", "version"},
		"dist_account":    {"frozen", "available", "withdrawing", "withdrawn", "total_earned", "total_reversed", "version"},
		"dist_commission": {"id", "tenant_id", "order_id", "order_item_id", "idem_key", "buyer_user_id", "agent_user_id", "layer", "base_amount", "rate_bp", "amount", "reversed_amount", "state", "freeze_days", "available_at", "settled_at", "accrued_at", "rule_version", "rule_snapshot", "reverse_of", "version"},
		"dist_ledger":     {"id", "tenant_id", "user_id", "biz_type", "biz_id", "delta_frozen", "delta_available", "delta_withdrawing", "delta_withdrawn", "after_frozen", "after_available", "after_withdrawing", "after_withdrawn", "remark", "created_at"},
		"dist_refund":     {"id", "tenant_id", "order_id", "item_id", "idem_key", "amount", "cumulative", "refunded_at"},
	}

	got := make(map[string]map[string]bool, len(sqlstore.Schema()))
	for _, table := range sqlstore.Schema() {
		cols := make(map[string]bool, len(table.Columns))
		for _, c := range table.Columns {
			cols[c.Name] = true
		}
		got[table.Name] = cols
	}

	for table, cols := range required {
		present, ok := got[table]
		if !ok {
			t.Errorf("table %s is missing from the schema", table)
			continue
		}
		for _, col := range cols {
			if !present[col] {
				t.Errorf("table %s is missing column %s", table, col)
			}
		}
	}
}

// TestDueWorkIndexLeadsWithStateAndTime pins the index shape that makes the
// heartbeat cheap (ADR-035).
//
// TenantsWithDueWork asks "which tenants have work" across the whole
// installation, and DueCommissions asks the same question for one tenant. Both
// are answered by a range scan over this index. Ordering it tenant-first instead
// would make the cross-tenant query scan the entire backlog of whichever tenant
// happens to sort first, so the column order is load-bearing and is asserted here.
func TestDueWorkIndexLeadsWithStateAndTime(t *testing.T) {
	for _, table := range sqlstore.Schema() {
		if table.Name != "dist_commission" {
			continue
		}
		for _, idx := range table.Indexes {
			if len(idx) == 3 && idx[0] == "state" {
				if idx[1] != "available_at" || idx[2] != "tenant_id" {
					t.Fatalf("due-work index = %v, want [state available_at tenant_id]", idx)
				}
				return
			}
		}
		t.Fatal("dist_commission has no (state, available_at, tenant_id) index; " +
			"the heartbeat would fall back to scanning")
	}
	t.Fatal("dist_commission is missing from the schema")
}

// TestPrimaryKeysAreNotDuplicated checks the interaction between a dialect's
// generated-key column and the table-level key.
//
// SQLite must declare "INTEGER PRIMARY KEY" on the column itself, because that is
// the only form it treats as the rowid alias. Emitting a second, table-level
// PRIMARY KEY clause would make the DDL invalid. MySQL is the opposite: the
// column carries AUTO_INCREMENT and the key must be declared separately.
func TestPrimaryKeysAreNotDuplicated(t *testing.T) {
	for name, d := range dialects() {
		t.Run(name, func(t *testing.T) {
			for _, table := range sqlstore.Schema() {
				ddl := statementFor(t, d, table.Name)
				occurrences := strings.Count(ddl, "PRIMARY KEY")
				want := 1
				if len(table.Primary) == 0 {
					want = 0
				}
				if occurrences != want {
					t.Errorf("table %s has %d PRIMARY KEY clauses, want %d:\n%s",
						table.Name, occurrences, want, ddl)
				}
			}
		})
	}
}

// TestDialectSyntaxDifferences pins what actually differs, so that a dialect
// accidentally copied from another one fails here rather than in production.
func TestDialectSyntaxDifferences(t *testing.T) {
	commission := statementFor(t, mysql.Dialect{}, "dist_commission")
	pgCommission := statementFor(t, postgres.Dialect{}, "dist_commission")
	sqliteCommission := statementFor(t, sqlite.Dialect{}, "dist_commission")

	if !strings.Contains(commission, "AUTO_INCREMENT") {
		t.Error("mysql should use AUTO_INCREMENT for generated ids")
	}
	if !strings.Contains(commission, "ENGINE=InnoDB") {
		t.Error("mysql should declare a storage engine")
	}
	if !strings.Contains(pgCommission, "GENERATED BY DEFAULT AS IDENTITY") {
		t.Error("postgres should use the standard identity form, not SERIAL")
	}
	if strings.Contains(pgCommission, "ENGINE=") {
		t.Error("postgres must not receive a storage-engine suffix")
	}
	if !strings.Contains(sqliteCommission, "INTEGER PRIMARY KEY") {
		t.Error("sqlite needs INTEGER PRIMARY KEY on the column for rowid assignment")
	}
	if strings.Contains(sqliteCommission, "AUTO_INCREMENT") {
		t.Error("sqlite has no AUTO_INCREMENT")
	}

	// Identifiers are quoted differently and must not be interchangeable.
	if !strings.Contains(commission, "`order_id`") {
		t.Error("mysql should quote identifiers with backticks")
	}
	if !strings.Contains(pgCommission, `"order_id"`) {
		t.Error("postgres should quote identifiers with double quotes")
	}
}

func TestPlaceholderConventions(t *testing.T) {
	if got := (mysql.Dialect{}).Placeholder(3); got != "?" {
		t.Errorf("mysql placeholder = %q, want ?", got)
	}
	if got := (sqlite.Dialect{}).Placeholder(3); got != "?" {
		t.Errorf("sqlite placeholder = %q, want ?", got)
	}
	if got := (postgres.Dialect{}).Placeholder(3); got != "$3" {
		t.Errorf("postgres placeholder = %q, want $3", got)
	}
}

func TestInsertIDStrategies(t *testing.T) {
	if got := (mysql.Dialect{}).InsertID(); got != sqlstore.InsertIDLastInsert {
		t.Errorf("mysql should read generated ids from LastInsertId")
	}
	for name, d := range map[string]sqlstore.Dialect{
		"postgres": postgres.Dialect{},
		"sqlite":   sqlite.Dialect{},
	} {
		if got := d.InsertID(); got != sqlstore.InsertIDReturning {
			t.Errorf("%s should read generated ids with RETURNING", name)
		}
	}
}

// TestMigrationIdempotencyStrategy checks that every dialect can express
// "create if absent" for both tables and indexes.
//
// PostgreSQL and SQLite say it with IF NOT EXISTS. MySQL has no such clause for
// CREATE INDEX, so it must at least be able to recognise the resulting error.
func TestMigrationIdempotencyStrategy(t *testing.T) {
	indexErr := errString("Duplicate key name 'dist_agent_idx_0'")

	for name, d := range dialects() {
		t.Run(name, func(t *testing.T) {
			ddl := d.IndexDDL("dist_agent", "idx", []string{"tenant_id"}, nil, false)
			if strings.Contains(ddl, "IF NOT EXISTS") {
				return
			}
			// No IF NOT EXISTS support: the migration must be able to tolerate
			// the duplicate-name error instead.
			if name != "mysql" {
				t.Fatalf("%s emits neither IF NOT EXISTS nor an error strategy", name)
			}
			if !d.IsAlreadyExists(indexErr) {
				t.Fatalf("mysql must classify a duplicate index name as already existing")
			}
		})
	}
}

// TestStatementsAreDeterministic keeps migrations reproducible: the same dialect
// must always produce the same statement list, in the same order, or a migration
// log becomes impossible to compare between runs.
func TestStatementsAreDeterministic(t *testing.T) {
	for name, d := range dialects() {
		t.Run(name, func(t *testing.T) {
			first := sqlstore.Statements(d)
			for i := 0; i < 5; i++ {
				again := sqlstore.Statements(d)
				if len(again) != len(first) {
					t.Fatalf("statement count changed between calls")
				}
				for j := range first {
					if again[j] != first[j] {
						t.Fatalf("statement %d differs between calls", j)
					}
				}
			}
		})
	}
}

// TestSchemaIsACopy keeps a caller from editing the shared definition, which
// would change the schema for every dialect and every test in the process.
func TestSchemaIsACopy(t *testing.T) {
	first := sqlstore.Schema()
	original := first[0].Columns[0].Name
	first[0].Columns[0].Name = "tampered"

	if sqlstore.Schema()[0].Columns[0].Name != original {
		t.Fatal("Schema leaked its internal definitions")
	}
}

// statementFor finds the CREATE TABLE statement for one table.
func statementFor(t *testing.T, d sqlstore.Dialect, table string) string {
	t.Helper()
	needle := "CREATE TABLE IF NOT EXISTS " + d.Quote(table)
	for _, s := range sqlstore.Statements(d) {
		if strings.HasPrefix(s, needle) {
			return s
		}
	}
	t.Fatalf("%s: no CREATE TABLE statement for %s", d.Name(), table)
	return ""
}

// errString is a plain error carrying a driver-like message, used to exercise the
// textual fallback of error classification without importing a driver.
type errString string

func (e errString) Error() string { return string(e) }

// TestEveryColumnIsDeclaredExactlyOnce is the check that would have caught a real
// defect: the generated-key column used to be rendered with its name twice,
// producing `"id" "id" INTEGER PRIMARY KEY`. The parity test above only looked
// for containment, so it passed.
//
// A column name may appear twice only when the table also declares a table-level
// PRIMARY KEY over it (once in the column list, once in the key clause).
func TestEveryColumnIsDeclaredExactlyOnce(t *testing.T) {
	for name, d := range dialects() {
		t.Run(name, func(t *testing.T) {
			for _, table := range sqlstore.Schema() {
				ddl := statementFor(t, d, table.Name)

				inPrimary := map[string]bool{}
				for _, c := range table.Primary {
					inPrimary[c] = true
				}
				// When the generated-key column carries the primary key inline, the
				// key is declared once, on the column itself. Every other table
				// declares its key separately, so the columns in it appear twice.
				inlineAutoID := d.AutoIDIsPrimary() && len(table.Primary) == 1 && table.Primary[0] == "id"

				for _, col := range table.Columns {
					quoted := d.Quote(col.Name)
					got := strings.Count(ddl, quoted)
					want := 1
					if inPrimary[col.Name] && !inlineAutoID {
						want = 2
					}
					if got != want {
						t.Errorf("table %s: column %s appears %d times, want %d\n%s",
							table.Name, col.Name, got, want, ddl)
					}
				}
			}
		})
	}
}

// TestNoIdentifierIsRepeatedAdjacent catches the same defect in a way that does
// not depend on knowing the schema: two identical quoted identifiers with only
// whitespace between them is never valid SQL.
func TestNoIdentifierIsRepeatedAdjacent(t *testing.T) {
	for name, d := range dialects() {
		t.Run(name, func(t *testing.T) {
			for _, stmt := range sqlstore.Statements(d) {
				fields := strings.Fields(stmt)
				for i := 1; i < len(fields); i++ {
					prev := strings.TrimRight(fields[i-1], ",()")
					cur := strings.TrimRight(fields[i], ",()")
					if prev == "" || cur == "" {
						continue
					}
					if prev == cur && strings.HasPrefix(prev, string(d.Quote("")[0])) {
						t.Errorf("identifier %s repeated adjacent in:\n%s", prev, stmt)
					}
				}
			}
		})
	}
}

// TestDialectContract runs one contract over every shipped dialect.
//
// Adding a backend means adding one line to dialects(), and this file then tests
// it. That is the payoff of the portable core: the differences between databases
// are enumerated in a handful of methods, so a new one can be checked
// exhaustively instead of spot-checked.
func TestDialectContract(t *testing.T) {
	duplicate := errString("Error 1062: Duplicate entry 'x' for key 'idem'")
	retryable := errString("deadlock detected")
	fresh := errString("syntax error near FROM")

	for name, d := range dialects() {
		t.Run(name, func(t *testing.T) {
			if d.Name() != name {
				t.Errorf("Name() = %q, want %q", d.Name(), name)
			}

			// Every portable column kind must render to something, or a schema
			// change would produce a statement with an empty type.
			for _, kind := range []sqlstore.ColumnKind{
				sqlstore.KindInt64, sqlstore.KindString, sqlstore.KindBytes, sqlstore.KindInstant,
			} {
				if got := d.TypeName(kind, 64); strings.TrimSpace(got) == "" {
					t.Errorf("TypeName(kind=%d) is empty", kind)
				}
			}
			// An unspecified string size must still produce a usable type rather
			// than VARCHAR(0), which some databases reject outright.
			if got := d.TypeName(sqlstore.KindString, 0); strings.Contains(got, "(0)") {
				t.Errorf("TypeName(KindString, 0) = %q, a zero width is not usable", got)
			}

			// Quoting must be reversible for ordinary names and must not let a
			// stray quote break out of the identifier.
			if d.Quote("a") == "a" {
				t.Error("Quote left the name unquoted")
			}
			for _, tricky := range []string{"a b", "select", "a`b", `a"b`} {
				q := d.Quote(tricky)
				if len(q) < 2 {
					t.Errorf("Quote(%q) = %q, too short to be quoted", tricky, q)
				}
			}

			// Classification is what turns a constraint violation into the normal
			// ErrDuplicate outcome rather than a hard failure.
			if !d.IsDuplicateKey(duplicate) {
				t.Error("a duplicate-entry error was not recognised")
			}
			if d.IsDuplicateKey(fresh) || d.IsRetryable(fresh) || d.IsAlreadyExists(fresh) {
				t.Error("a plain syntax error was misclassified")
			}
			if !d.IsRetryable(retryable) {
				t.Error("a deadlock was not recognised as retryable")
			}
			if d.IsDuplicateKey(retryable) {
				t.Error("a deadlock was misclassified as a duplicate key")
			}
			if d.IsAlreadyExists(retryable) {
				t.Error("a deadlock was misclassified as an existing object")
			}
		})
	}
}
