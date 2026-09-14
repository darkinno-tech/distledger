package sqlstore

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// Migrate creates the schema, tolerating what is already there.
//
// It is safe to run on every start: CREATE TABLE IF NOT EXISTS covers the tables
// portably, and index creation is tolerated through the dialect's IsAlreadyExists.
// That fallback exists for MySQL, which has no IF NOT EXISTS on CREATE INDEX.
//
// # What this deliberately is not
//
// It is not a migration framework. There is no version table and no down path,
// because a ledger's schema cannot be rolled back without deciding what happens
// to the money already recorded under it. Adding a column later is a migration a
// human writes and reviews; this function only brings an empty database up to
// the current shape.
func Migrate(ctx context.Context, db *sql.DB, d Dialect) error {
	if db == nil {
		return fmt.Errorf("sqlstore: migrate: nil *sql.DB")
	}
	if d == nil {
		return fmt.Errorf("sqlstore: migrate: nil Dialect")
	}
	for _, stmt := range Statements(d) {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			if d.IsAlreadyExists(err) {
				continue
			}
			return fmt.Errorf("sqlstore: migrate %s: %w", summarise(stmt), err)
		}
	}
	return nil
}

// summarise renders just enough of a statement to identify it in an error,
// without dumping an entire CREATE TABLE into the log line.
func summarise(stmt string) string {
	const limit = 60
	stmt = strings.Join(strings.Fields(stmt), " ")
	if len(stmt) <= limit {
		return stmt
	}
	return stmt[:limit] + "..."
}

// Migrate implements the same thing as a method, for callers who already hold a
// Store.
func (s *Store) Migrate(ctx context.Context) error {
	return Migrate(ctx, s.db, s.dialect)
}

// CheckSchema implements distledger.SchemaChecker.
//
// It probes each table with a query that returns no rows, rather than reading
// information_schema. A probe is portable across every backend and it checks the
// thing that actually matters: that the table exists and the columns the store
// selects are all present. A missing column fails the probe, which is precisely
// the case a "table exists" check would miss.
func (s *Store) CheckSchema(ctx context.Context) []string {
	var issues []string
	t := &tx{s: s}
	for _, table := range Schema() {
		if missing := s.missingColumns(ctx, table); len(missing) > 0 {
			issues = append(issues, fmt.Sprintf("table %s is missing: %s",
				table.Name, strings.Join(missing, ", ")))
			continue
		}
		// The probe doubles as the existence check.
		probe := "SELECT " + columnList(s.dialect, table) + " FROM " +
			s.dialect.Quote(table.Name) + " WHERE 1 = 0"
		if _, err := s.db.ExecContext(ctx, probe); err != nil {
			issues = append(issues, fmt.Sprintf("table %s is not usable: %v", table.Name, err))
		}
	}
	_ = t
	return issues
}

// missingColumns reports columns the schema declares but the database cannot
// answer for. It is only reachable when the database is older than the schema,
// which is exactly the case worth naming rather than reporting as a vague probe
// failure.
func (s *Store) missingColumns(ctx context.Context, table TableDef) []string {
	probe := "SELECT " + strings.Join(quotedColumns(s.dialect, table), ", ") +
		" FROM " + s.dialect.Quote(table.Name) + " WHERE 1 = 0"
	err := func() error {
		rows, err := s.db.QueryContext(ctx, probe)
		if err != nil {
			return err
		}
		return rows.Close()
	}()
	if err == nil {
		return nil
	}
	// Narrow the failure to individual columns so the message is actionable.
	var missing []string
	for _, col := range table.Columns {
		one := "SELECT " + s.dialect.Quote(col.Name) + " FROM " +
			s.dialect.Quote(table.Name) + " WHERE 1 = 0"
		if err := func() error {
			rows, qerr := s.db.QueryContext(ctx, one)
			if qerr != nil {
				return qerr
			}
			return rows.Close()
		}(); err != nil {
			missing = append(missing, col.Name)
		}
	}
	if len(missing) == 0 {
		// The table itself is what is missing.
		return []string{"the table itself"}
	}
	return missing
}

func quotedColumns(d Dialect, table TableDef) []string {
	out := make([]string, 0, len(table.Columns))
	for _, c := range table.Columns {
		out = append(out, d.Quote(c.Name))
	}
	return out
}

func columnList(d Dialect, table TableDef) string {
	return strings.Join(quotedColumns(d, table), ", ")
}
