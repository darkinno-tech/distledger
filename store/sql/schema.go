package sqlstore

import (
	"fmt"
	"strings"
)

// The portable schema.
//
// It is written once, in ColumnKind terms, and every dialect renders it. That is
// the whole point: if each dialect carried its own CREATE TABLE statements, the
// backends could disagree about a column and nothing would notice until a query
// failed in production. Here a missing column is a compile-time omission from one
// shared list, and the test below checks that every dialect emits every column.
//
// Column names match the domain structs, so a reader can move between model.go
// and this file without a translation table.

// ColumnDef describes one column of the portable schema.
type ColumnDef struct {
	Name string
	Kind ColumnKind
	// Size is the declared length for KindString columns.
	Size int
	// NotNull marks a column that can never be NULL. Instant columns are
	// deliberately not NotNull: see the note on instantOrNull.
	NotNull bool
	// Default is emitted verbatim when non-empty.
	Default string
}

// TableDef describes one table of the portable schema.
type TableDef struct {
	Name    string
	Columns []ColumnDef
	// Primary is the primary key column list.
	Primary []string
	// Uniques are unique constraints, each a column list.
	Uniques [][]string
	// Indexes are non-unique indexes, each a column list.
	Indexes [][]string
}

const (
	tableTenant     = "dist_tenant"
	tableAgent      = "dist_agent"
	tableBinding    = "dist_binding"
	tableAccount    = "dist_account"
	tableCommission = "dist_commission"
	tableLedger     = "dist_ledger"
	tableRefund     = "dist_refund"
)

// column sizes. They mirror the byte limits the root package enforces on the
// corresponding values, so a value that passes validation always fits its column.
const (
	sizeOrderID     = 64  // MaxOrderIDLen
	sizeItemID      = 128 // MaxOrderItemIDLen
	sizeIdemKey     = 64  // MaxIdemKeyLen
	sizeRemark      = 256 // MaxRemarkLen
	sizeSourceRef   = 128
	sizeReason      = 128
	sizeShortString = 32
)

// Schema returns the portable table definitions.
//
// The result is a fresh copy, so a caller cannot edit the schema by keeping the
// slice it was handed.
func Schema() []TableDef {
	out := make([]TableDef, len(schemaTables))
	for i, t := range schemaTables {
		out[i] = t
		out[i].Columns = append([]ColumnDef(nil), t.Columns...)
		out[i].Primary = append([]string(nil), t.Primary...)
		out[i].Uniques = cloneCols(t.Uniques)
		out[i].Indexes = cloneCols(t.Indexes)
	}
	return out
}

func cloneCols(in [][]string) [][]string {
	out := make([][]string, len(in))
	for i, c := range in {
		out[i] = append([]string(nil), c...)
	}
	return out
}

var schemaTables = []TableDef{
	{
		// The tenant registry exists so that Reader.Tenants is one indexed lookup
		// instead of a DISTINCT over several large tables, which Maintain would
		// otherwise run on every tick (ADR-034).
		Name: tableTenant,
		Columns: []ColumnDef{
			{Name: "tenant_id", Kind: KindInt64, NotNull: true},
			{Name: "created_at", Kind: KindInstant},
		},
		Primary: []string{"tenant_id"},
	},
	{
		Name: tableAgent,
		Columns: []ColumnDef{
			{Name: "tenant_id", Kind: KindInt64, NotNull: true},
			{Name: "user_id", Kind: KindInt64, NotNull: true},
			{Name: "parent_id", Kind: KindInt64, NotNull: true, Default: "0"},
			{Name: "depth", Kind: KindInt64, NotNull: true, Default: "0"},
			{Name: "status", Kind: KindInt64, NotNull: true, Default: "0"},
			{Name: "join_type", Kind: KindInt64, NotNull: true, Default: "0"},
			// NULL means "no per-agent override". 0 is a valid rate, so it cannot
			// double as the absent value.
			{Name: "rate_override_bp", Kind: KindInt64},
			{Name: "created_at", Kind: KindInstant},
			{Name: "updated_at", Kind: KindInstant},
			{Name: "version", Kind: KindInt64, NotNull: true, Default: "0"},
		},
		Primary: []string{"tenant_id", "user_id"},
		Indexes: [][]string{{"tenant_id", "parent_id"}},
	},
	{
		Name: tableBinding,
		Columns: []ColumnDef{
			{Name: "tenant_id", Kind: KindInt64, NotNull: true},
			{Name: "buyer_user_id", Kind: KindInt64, NotNull: true},
			{Name: "agent_user_id", Kind: KindInt64, NotNull: true},
			{Name: "source", Kind: KindInt64, NotNull: true, Default: "0"},
			{Name: "source_ref", Kind: KindString, Size: sizeSourceRef},
			{Name: "bound_at", Kind: KindInstant},
			{Name: "expire_at", Kind: KindInstant},
			{Name: "active", Kind: KindInt64, NotNull: true, Default: "0"},
			{Name: "rebound_from", Kind: KindInt64, NotNull: true, Default: "0"},
			{Name: "version", Kind: KindInt64, NotNull: true, Default: "0"},
		},
		Primary: []string{"tenant_id", "buyer_user_id"},
		Indexes: [][]string{{"tenant_id", "agent_user_id"}},
	},
	{
		// The primary key doubles as the index for keyset pagination over
		// (tenant_id, user_id), which is what SelfCheck walks.
		Name: tableAccount,
		Columns: []ColumnDef{
			{Name: "tenant_id", Kind: KindInt64, NotNull: true},
			{Name: "user_id", Kind: KindInt64, NotNull: true},
			{Name: "frozen", Kind: KindInt64, NotNull: true, Default: "0"},
			{Name: "available", Kind: KindInt64, NotNull: true, Default: "0"},
			{Name: "withdrawing", Kind: KindInt64, NotNull: true, Default: "0"},
			{Name: "withdrawn", Kind: KindInt64, NotNull: true, Default: "0"},
			{Name: "total_earned", Kind: KindInt64, NotNull: true, Default: "0"},
			{Name: "total_reversed", Kind: KindInt64, NotNull: true, Default: "0"},
			{Name: "version", Kind: KindInt64, NotNull: true, Default: "0"},
			{Name: "updated_at", Kind: KindInstant},
		},
		Primary: []string{"tenant_id", "user_id"},
	},
	{
		Name: tableCommission,
		Columns: []ColumnDef{
			{Name: "id", Kind: KindInt64, NotNull: true},
			{Name: "tenant_id", Kind: KindInt64, NotNull: true},
			{Name: "order_id", Kind: KindString, Size: sizeOrderID, NotNull: true},
			{Name: "order_item_id", Kind: KindString, Size: sizeItemID, NotNull: true, Default: "''"},
			{Name: "idem_key", Kind: KindString, Size: sizeIdemKey, NotNull: true},
			{Name: "buyer_user_id", Kind: KindInt64, NotNull: true, Default: "0"},
			{Name: "agent_user_id", Kind: KindInt64, NotNull: true},
			{Name: "layer", Kind: KindInt64, NotNull: true},
			{Name: "base_amount", Kind: KindInt64, NotNull: true, Default: "0"},
			{Name: "rate_bp", Kind: KindInt64, NotNull: true, Default: "0"},
			{Name: "amount", Kind: KindInt64, NotNull: true, Default: "0"},
			{Name: "reversed_amount", Kind: KindInt64, NotNull: true, Default: "0"},
			{Name: "state", Kind: KindInt64, NotNull: true, Default: "0"},
			{Name: "freeze_days", Kind: KindInt64, NotNull: true, Default: "0"},
			{Name: "available_at", Kind: KindInstant},
			{Name: "settled_at", Kind: KindInstant},
			{Name: "accrued_at", Kind: KindInstant},
			{Name: "rule_version", Kind: KindInt64, NotNull: true, Default: "0"},
			{Name: "rule_snapshot", Kind: KindString, Size: sizeRemark},
			{Name: "reverse_of", Kind: KindInt64, NotNull: true, Default: "0"},
			{Name: "version", Kind: KindInt64, NotNull: true, Default: "0"},
		},
		Primary: []string{"id"},
		Uniques: [][]string{{"tenant_id", "idem_key"}},
		Indexes: [][]string{
			{"tenant_id", "id"},                          // CommissionsByTenant keyset
			{"tenant_id", "order_id"},                    // CommissionsByOrder
			{"tenant_id", "agent_user_id", "id"},         // CommissionsByAgent keyset
			{"tenant_id", "state", "available_at", "id"}, // DueCommissions
		},
	},
	{
		Name: tableLedger,
		Columns: []ColumnDef{
			{Name: "id", Kind: KindInt64, NotNull: true},
			{Name: "tenant_id", Kind: KindInt64, NotNull: true},
			{Name: "user_id", Kind: KindInt64, NotNull: true},
			{Name: "biz_type", Kind: KindInt64, NotNull: true},
			{Name: "biz_id", Kind: KindString, Size: sizeIdemKey, NotNull: true, Default: "''"},
			{Name: "delta_frozen", Kind: KindInt64, NotNull: true, Default: "0"},
			{Name: "delta_available", Kind: KindInt64, NotNull: true, Default: "0"},
			{Name: "delta_withdrawing", Kind: KindInt64, NotNull: true, Default: "0"},
			{Name: "delta_withdrawn", Kind: KindInt64, NotNull: true, Default: "0"},
			{Name: "after_frozen", Kind: KindInt64, NotNull: true, Default: "0"},
			{Name: "after_available", Kind: KindInt64, NotNull: true, Default: "0"},
			{Name: "after_withdrawing", Kind: KindInt64, NotNull: true, Default: "0"},
			{Name: "after_withdrawn", Kind: KindInt64, NotNull: true, Default: "0"},
			{Name: "remark", Kind: KindString, Size: sizeRemark},
			{Name: "created_at", Kind: KindInstant},
		},
		Primary: []string{"id"},
		Indexes: [][]string{
			{"tenant_id", "id"},            // LedgerByTenant keyset
			{"tenant_id", "user_id", "id"}, // LedgerEntries keyset
			{"tenant_id", "biz_type", "biz_id"},
		},
	},
	{
		Name: tableRefund,
		Columns: []ColumnDef{
			{Name: "id", Kind: KindInt64, NotNull: true},
			{Name: "tenant_id", Kind: KindInt64, NotNull: true},
			{Name: "order_id", Kind: KindString, Size: sizeOrderID, NotNull: true},
			{Name: "item_id", Kind: KindString, Size: sizeItemID, NotNull: true, Default: "''"},
			{Name: "idem_key", Kind: KindString, Size: sizeIdemKey, NotNull: true},
			{Name: "amount", Kind: KindInt64, NotNull: true, Default: "0"},
			{Name: "cumulative", Kind: KindInt64, NotNull: true, Default: "0"},
			{Name: "refunded_at", Kind: KindInstant},
		},
		Primary: []string{"id"},
		Uniques: [][]string{{"tenant_id", "idem_key"}},
		Indexes: [][]string{{"tenant_id", "order_id"}},
	},
}

// Statements renders the whole schema as CREATE statements for a dialect.
//
// It is what Migrate executes, and it is also the input to the schema parity test
// that checks every dialect emits every column.
func Statements(d Dialect) []string {
	out := make([]string, 0, len(schemaTables)*4)
	for _, t := range schemaTables {
		out = append(out, createTable(d, t))
		for i, cols := range t.Uniques {
			out = append(out, d.IndexDDL(t.Name, indexName(t.Name, "uniq", i), cols, kindsOf(t), true))
		}
		for i, cols := range t.Indexes {
			out = append(out, d.IndexDDL(t.Name, indexName(t.Name, "idx", i), cols, kindsOf(t), false))
		}
	}
	return out
}

func createTable(d Dialect, t TableDef) string {
	var b strings.Builder
	b.WriteString("CREATE TABLE IF NOT EXISTS ")
	b.WriteString(d.Quote(t.Name))
	b.WriteString(" (")
	for i, c := range t.Columns {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(d.Quote(c.Name))
		b.WriteByte(' ')
		if c.Name == "id" {
			// The generated key column is rendered by the dialect; every other
			// column goes through the portable type mapping.
			b.WriteString(d.AutoIDColumn())
		} else {
			b.WriteString(d.TypeName(c.Kind, c.Size))
			if c.NotNull {
				b.WriteString(" NOT NULL")
			}
			if c.Default != "" {
				b.WriteString(" DEFAULT ")
				b.WriteString(c.Default)
			}
		}
	}
	// A dialect whose generated-key column already declares the primary key must
	// not receive a second, table-level one.
	if len(t.Primary) > 0 && !autoIDOnlyTable(d, t) {
		b.WriteString(", PRIMARY KEY (")
		b.WriteString(quoteList(d, t.Primary))
		b.WriteByte(')')
	}
	b.WriteString(")")
	b.WriteString(d.TableSuffix())
	return b.String()
}

// autoIDOnlyTable reports whether a table's entire primary key is the generated
// id column, and the dialect declares that key inline.
func autoIDOnlyTable(d Dialect, t TableDef) bool {
	return d.AutoIDIsPrimary() && len(t.Primary) == 1 && t.Primary[0] == "id"
}

func indexName(table, kind string, i int) string {
	return fmt.Sprintf("%s_%s_%d", table, kind, i)
}

func quoteList(d Dialect, cols []string) string {
	parts := make([]string, 0, len(cols))
	for _, c := range cols {
		parts = append(parts, d.Quote(c))
	}
	return strings.Join(parts, ", ")
}

func kindsOf(t TableDef) map[string]ColumnKind {
	out := make(map[string]ColumnKind, len(t.Columns))
	for _, c := range t.Columns {
		out[c.Name] = c.Kind
	}
	return out
}
