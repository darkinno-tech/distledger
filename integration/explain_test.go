package integration

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/darkinno-tech/distledger/store/mysql"
	"github.com/darkinno-tech/distledger/store/postgres"
	sqlstore "github.com/darkinno-tech/distledger/store/sql"
)

// Query-plan probes for the heartbeat.
//
// The per-minute cost of Maintain is the one thing in this library that grows on
// its own, so it is measured rather than reasoned about. Skipped unless asked
// for, because it writes tens of thousands of synthetic rows:
//
//	DISTLEDGER_EXPLAIN=1 DISTLEDGER_TEST_MYSQL_DSN=... go test -run Explain -v
//
// # What these probes are for
//
// They answer one question: when the heartbeat asks for the next batch of due
// work, does the backend walk the whole table or a thin slice of it? A plan that
// says "covering index range scan" and reports a row count near the limit is the
// answer we want; a plan that sorts a row count equal to the table is not,
// whatever its wall-clock time happens to be on a small data set.
//
// # Keeping the probe honest
//
// Three things have to hold, or the numbers describe the seed instead of the
// query.
//
//   - The queries must mirror the store's statements. They are written out
//     because EXPLAIN needs SQL text, so if the store's shape changes these must
//     change with them. The form DueWork replaced is kept alongside, so the
//     improvement is visible in the output rather than only in a commit message.
//
//   - available_at must be spread out. With one shared timestamp InnoDB reports
//     cardinality 1 for the column, the optimizer loses any reason to prefer the
//     ordered index, and the plan degenerates for reasons that have nothing to
//     do with the query.
//
//   - The due bound must vary, because its selectivity is the whole story. A
//     sweep where nothing is due, one where a thin slice is due, and one where
//     the entire table is due are three different questions with three different
//     right answers.
func TestExplainHeartbeatPlans(t *testing.T) {
	if os.Getenv("DISTLEDGER_EXPLAIN") != "1" {
		t.Skip("set DISTLEDGER_EXPLAIN=1 to run the query-plan probes")
	}

	for _, target := range []struct {
		name   string
		env    string
		driver string
		dial   sqlstore.Dialect
	}{
		{"mysql", "DISTLEDGER_TEST_MYSQL_DSN", "mysql", mysql.Dialect{}},
		{"postgres", "DISTLEDGER_TEST_POSTGRES_DSN", "pgx", postgres.Dialect{}},
	} {
		t.Run(target.name, func(t *testing.T) {
			dsn := os.Getenv(target.env)
			if dsn == "" {
				t.Skipf("%s is unset", target.env)
			}
			db, err := sql.Open(target.driver, dsn)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()

			ctx := context.Background()
			resetSchema(t, ctx, db, target.dial)
			store, err := sqlstore.Open(db, target.dial)
			if err != nil {
				t.Fatal(err)
			}
			migrate(t, store)

			bounds := seedProbeData(t, ctx, db, target.dial)
			t.Logf("seeded %d pending commissions across %d tenants, available_at spread one minute apart",
				probeTenants*probePerTenant, probeTenants)

			q := target.dial.Quote
			queries := []struct {
				label string
				sql   string
			}{
				{"DueWork, nothing due (the empty tick)", dueWork(q, bounds.none, maintainLimit)},
				{"DueWork, one hour due", dueWork(q, bounds.hour, maintainLimit)},
				{"DueWork, one day due (full batch)", dueWork(q, bounds.day, maintainLimit)},
				{"DueWork, whole table due (backlog)", dueWork(q, bounds.all, maintainLimit)},
				{
					"replaced: tenant discovery",
					"SELECT DISTINCT " + q("tenant_id") + " FROM " + q("dist_commission") +
						" WHERE " + q("state") + " = 0" +
						" AND " + q("available_at") + " IS NOT NULL" +
						" AND " + q("available_at") + " <= " + fmt.Sprint(bounds.all) +
						" ORDER BY " + q("tenant_id") + " LIMIT 500",
				},
			}

			for _, item := range queries {
				t.Logf("\n=== %s ===\n%s", item.label, explain(t, ctx, db, target.dial, item.sql))
				start := time.Now()
				rows, err := db.QueryContext(ctx, item.sql)
				if err != nil {
					t.Fatalf("%s: %v", item.label, err)
				}
				n := 0
				for rows.Next() {
					n++
				}
				rows.Close()
				t.Logf("%-40s %5d rows in %s", item.label, n, time.Since(start).Round(time.Microsecond))
			}
		})
	}
}

// maintainLimit mirrors the store's own batch size, so the probe asks for exactly
// as much as Maintain would.
const maintainLimit = 2000

// dueWork renders the statement DueWork issues, with the bound as a literal
// because EXPLAIN cannot see bound parameters.
func dueWork(q func(string) string, dueAt int64, limit int) string {
	return "SELECT " + q("id") + " FROM " + q("dist_commission") +
		" WHERE " + q("state") + " = 0" +
		" AND " + q("available_at") + " IS NOT NULL" +
		" AND " + q("available_at") + " <= " + fmt.Sprint(dueAt) +
		" ORDER BY " + q("available_at") + ", " + q("id") +
		" LIMIT " + fmt.Sprint(limit)
}

// explain renders a plan in whichever form the dialect understands.
func explain(t *testing.T, ctx context.Context, db *sql.DB, d sqlstore.Dialect, query string) string {
	t.Helper()
	prefix := "EXPLAIN ANALYZE "
	if d.Name() == "postgres" {
		prefix = "EXPLAIN (ANALYZE, BUFFERS) "
	}

	rows, err := db.QueryContext(ctx, prefix+query)
	if err != nil {
		return "EXPLAIN failed: " + err.Error()
	}
	defer rows.Close()

	cols, _ := rows.Columns()
	var out strings.Builder
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatalf("scan plan: %v", err)
		}
		for i, v := range vals {
			if i > 0 {
				out.WriteString(" | ")
			}
			if b, ok := v.([]byte); ok {
				out.Write(b)
			} else {
				fmt.Fprintf(&out, "%v", v)
			}
		}
		out.WriteByte('\n')
	}
	return out.String()
}

const (
	probeTenants   = 40
	probePerTenant = 2000
)

// probeBounds are the due instants the query list sweeps.
type probeBounds struct {
	none int64 // before anything is available
	hour int64 // a thin slice due
	day  int64 // about one batch due
	all  int64 // the whole table due
}

// seedProbeData writes synthetic rows straight into the commission table.
//
// It uses SQL rather than the port because the point is to reach a volume the
// port would take minutes to produce, and the planner cares only about the rows.
//
// Every row is pending. available_at is spread one minute apart starting from a
// fixed base, so the column has real cardinality and each bound in probeBounds
// selects a known fraction of the table: nothing, about an hour's worth, about a
// day's worth, and everything.
func seedProbeData(t *testing.T, ctx context.Context, db *sql.DB, d sqlstore.Dialect) probeBounds {
	t.Helper()

	const batch = 200
	minute := int64(time.Minute)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).UnixNano()

	bounds := probeBounds{
		none: base - minute,
		hour: base + 60*minute,
		day:  base + 24*60*minute,
		all:  base + int64(probeTenants*probePerTenant)*minute,
	}

	columns := []string{
		"tenant_id", "order_id", "order_item_id", "idem_key", "buyer_user_id",
		"agent_user_id", "layer", "base_amount", "rate_bp", "amount",
		"reversed_amount", "state", "freeze_days", "available_at", "settled_at",
		"accrued_at", "rule_version", "rule_snapshot", "reverse_of", "version",
	}
	quoted := make([]string, len(columns))
	for i, c := range columns {
		quoted[i] = d.Quote(c)
	}
	columnList := strings.Join(quoted, ", ")

	for tenant := int64(1); tenant <= probeTenants; tenant++ {
		for done := 0; done < probePerTenant; done += batch {
			var (
				rows []string
				argv []any
			)
			marker := func(v any) string {
				argv = append(argv, v)
				return d.Placeholder(len(argv))
			}
			for i := 0; i < batch; i++ {
				n := done + i
				availableAt := base + ((tenant-1)*probePerTenant+int64(n))*minute
				rows = append(rows, "("+strings.Join([]string{
					marker(tenant),
					marker(fmt.Sprintf("ORD-%d-%d", tenant, n)), // order_id is a varchar
					marker(""),
					marker(fmt.Sprintf("idem-%d-%d", tenant, n)),
					marker(int64(9000)), marker(int64(100)), marker(int64(1)),
					marker(int64(10000)), marker(int64(500)), marker(int64(500)),
					marker(int64(0)), marker(int64(0)), marker(int64(7)),
					// available_at is the column under test; settled_at stays
					// NULL because a pending commission has not settled, and
					// accrued_at is a real instant because it has accrued.
					marker(availableAt), marker(nil), marker(availableAt),
					marker(int64(0)), marker(""), marker(int64(0)), marker(int64(1)),
				}, ", ")+")")
			}
			query := "INSERT INTO " + d.Quote("dist_commission") + " (" + columnList +
				") VALUES " + strings.Join(rows, ", ")
			if _, err := db.ExecContext(ctx, query, argv...); err != nil {
				t.Fatalf("seed tenant %d: %v", tenant, err)
			}
		}
	}

	// Plans depend on statistics more than on rows: without them the planner
	// assumes a tiny table and every plan looks like a scan of nothing.
	if d.Name() == "postgres" {
		if _, err := db.ExecContext(ctx, "ANALYZE "+d.Quote("dist_commission")); err != nil {
			t.Logf("analyze: %v", err)
		}
	} else {
		if _, err := db.ExecContext(ctx, "ANALYZE TABLE "+d.Quote("dist_commission")); err != nil {
			t.Logf("analyze: %v", err)
		}
	}
	return bounds
}
