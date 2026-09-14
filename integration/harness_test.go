// Package integration runs the store port's contract against every backend.
//
// # Why this is a separate module
//
// The library depends on nothing: its go.mod require block is empty, and that is
// a rule rather than a preference. Testing it against real databases needs
// drivers, so the drivers live here, in a module the library never imports.
//
// The effect is that `go test ./...` at the repository root needs no database and
// pulls no driver, while this module can be as thorough as it likes.
//
// # Running it
//
//	cd integration
//	docker compose up -d --wait
//	go test ./...
//
// SQLite needs no container. MySQL and PostgreSQL are skipped unless their DSN
// environment variables are set:
//
//	DISTLEDGER_TEST_MYSQL_DSN
//	DISTLEDGER_TEST_POSTGRES_DSN
//
// # What it proves
//
// The same assertions are run against the in-memory store and against every SQL
// dialect. That is the only way to know the port is genuinely backend-agnostic
// rather than accidentally shaped around whichever implementation was written
// first: a difference between backends shows up here, not in production.
package integration

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/darkinno-tech/distledger"
	"github.com/darkinno-tech/distledger/store/memory"
	"github.com/darkinno-tech/distledger/store/mysql"
	"github.com/darkinno-tech/distledger/store/postgres"
	sqlstore "github.com/darkinno-tech/distledger/store/sql"
	"github.com/darkinno-tech/distledger/store/sqlite"
	_ "github.com/go-sql-driver/mysql"
	_ "github.com/jackc/pgx/v5/stdlib"
	_ "modernc.org/sqlite"
)

// backend is one storage implementation under test.
type backend struct {
	name string
	// open returns a fresh, empty store. It is called once per subtest so that
	// no test can observe another's rows, and so that a failure cannot cascade.
	open func(t *testing.T) distledger.Store
	// reconciles reports whether the store implements Reconciler, and therefore
	// whether SelfCheck checks I1 in the database or row-wise. Tests assert it
	// rather than inferring it, so that a store quietly losing the ability would
	// be noticed instead of silently making the cheap path untested.
	reconciles bool
}

// backends lists every implementation the contract is run against.
//
// Adding a backend is one entry. Everything below then applies to it, which is
// the payoff of keeping the differences inside a Dialect.
func backends(t *testing.T) []backend {
	t.Helper()
	out := []backend{
		{name: "memory", reconciles: false, open: func(t *testing.T) distledger.Store {
			s := memory.New()
			t.Cleanup(func() { _ = s.Close() })
			return s
		}},
		{name: "sqlite", reconciles: true, open: func(t *testing.T) distledger.Store {
			return openSQLite(t)
		}},
	}

	if dsn := os.Getenv("DISTLEDGER_TEST_MYSQL_DSN"); dsn != "" {
		out = append(out, backend{name: "mysql", reconciles: true, open: func(t *testing.T) distledger.Store {
			return openSQL(t, "mysql", dsn, mysql.Dialect{})
		}})
	} else {
		t.Log("DISTLEDGER_TEST_MYSQL_DSN is unset: skipping mysql")
	}

	if dsn := os.Getenv("DISTLEDGER_TEST_POSTGRES_DSN"); dsn != "" {
		out = append(out, backend{name: "postgres", reconciles: true, open: func(t *testing.T) distledger.Store {
			return openSQL(t, "pgx", dsn, postgres.Dialect{})
		}})
	} else {
		t.Log("DISTLEDGER_TEST_POSTGRES_DSN is unset: skipping postgres")
	}
	return out
}

// openSQLite uses a file rather than :memory:.
//
// An in-memory SQLite database belongs to a single connection, and database/sql
// hands out a pool, so a second connection would see an empty database. That
// failure looks like random "no such table" errors under concurrency, which is
// exactly the kind of flakiness worth designing out.
func openSQLite(t *testing.T) distledger.Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ledger.db")
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)&_pragma=foreign_keys(1)"

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	// SQLite serialises writers, so a pool larger than one turns ordinary
	// contention into "database is locked" storms. The store's retry loop covers
	// the rest.
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })

	store, err := sqlstore.Open(db, sqlite.Dialect{})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	migrate(t, store)
	return store
}

func openSQL(t *testing.T, driver, dsn string, d sqlstore.Dialect) distledger.Store {
	t.Helper()
	db, err := sql.Open(driver, dsn)
	if err != nil {
		t.Fatalf("open %s: %v", driver, err)
	}
	t.Cleanup(func() { _ = db.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		t.Fatalf("%s is not reachable (%s): %v", driver, dsn, err)
	}

	// Each run starts from an empty schema. Dropping first means a schema change
	// during development cannot be masked by leftovers from the previous run.
	resetSchema(t, ctx, db, d)

	store, err := sqlstore.Open(db, d)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	migrate(t, store)
	return store
}

func migrate(t *testing.T, s *sqlstore.Store) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	// Running it twice must be a no-op, or every restart would fail on MySQL,
	// which has no IF NOT EXISTS for CREATE INDEX.
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("second migrate must be idempotent: %v", err)
	}
	if issues := s.CheckSchema(ctx); len(issues) > 0 {
		t.Fatalf("schema check after migrate: %v", issues)
	}
}

// resetSchema drops every table the store owns.
func resetSchema(t *testing.T, ctx context.Context, db *sql.DB, d sqlstore.Dialect) {
	t.Helper()
	// Reverse order so that nothing is dropped before something referencing it.
	tables := sqlstore.Schema()
	for i := len(tables) - 1; i >= 0; i-- {
		stmt := "DROP TABLE IF EXISTS " + d.Quote(tables[i].Name)
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("drop %s: %v", tables[i].Name, err)
		}
	}
}

// ── Shared test helpers ─────────────────────────────────────────────────

const testTenant = int64(1)

func agentKey(userID int64) distledger.UserKey {
	return distledger.UserKey{TenantID: testTenant, UserID: userID}
}

func orderKey(orderID string) distledger.OrderKey {
	return distledger.OrderKey{TenantID: testTenant, OrderID: orderID}
}

// seedAgent inserts an agent through the port.
func seedAgent(t *testing.T, ctx context.Context, s distledger.Store, userID, parentID int64) distledger.Agent {
	t.Helper()
	var out distledger.Agent
	if err := s.Update(ctx, func(ctx context.Context, tx distledger.Tx) error {
		var err error
		out, err = tx.PutAgent(ctx, distledger.Agent{
			Key:      agentKey(userID),
			ParentID: parentID,
			Depth:    1,
			Status:   distledger.AgentActive,
			JoinType: distledger.JoinFree,
		})
		return err
	}); err != nil {
		t.Fatalf("seed agent %d: %v", userID, err)
	}
	return out
}

func seedCommission(t *testing.T, ctx context.Context, s distledger.Store, orderID string, agentID int64) distledger.Commission {
	t.Helper()
	var out distledger.Commission
	if err := s.Update(ctx, func(ctx context.Context, tx distledger.Tx) error {
		var err error
		out, err = tx.AppendCommission(ctx, distledger.Commission{
			Key:         orderKey(orderID),
			IdemKey:     "idem-" + orderID,
			BuyerUserID: 9000,
			AgentUserID: agentID,
			Layer:       1,
			BaseAmount:  10000,
			Rate:        500,
			Amount:      500,
			State:       distledger.CommissionPending,
			AccruedAt:   time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		})
		return err
	}); err != nil {
		t.Fatalf("seed commission %s: %v", orderID, err)
	}
	return out
}

func read[T any](t *testing.T, ctx context.Context, s distledger.Store, fn func(context.Context, distledger.Reader) (T, error)) T {
	t.Helper()
	var out T
	if err := s.View(ctx, func(ctx context.Context, r distledger.Reader) error {
		var err error
		out, err = fn(ctx, r)
		return err
	}); err != nil {
		t.Fatalf("view: %v", err)
	}
	return out
}
