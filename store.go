package distledger

import (
	"context"
	"time"
)

// This file defines the **persistence port** (the port in ports & adapters).
//
// Why the port lives in the root package instead of a store subpackage: the Go
// convention is that interfaces are defined by their consumer. The root package
// both implements the orchestration and exposes replaceable storage to users;
// putting the port in a store subpackage would force the root package to import
// store while the store implementations import the root package for their
// types, creating a cycle. With the port in the root package and the
// implementations in store/memory and store/mysql, the dependency direction
// stays one-way and clear.
//
// # Capabilities the interfaces deliberately **do not** provide
//
//   - Modifying a commission amount: financial fields are immutable, only
//     AppendCommission exists.
//   - Modifying or deleting a ledger entry: the ledger is append-only.
//   - A generic Update/Delete: any "overwrite everything" entry point would
//     bypass invariant checks.
//
// In other words, "the ledger is immutable" is guaranteed by the shape of the
// port, not by code review.

// Pagination caps. They exist to bound the work of any single query, so that
// one `LedgerEntries{Limit: 1<<31}` cannot take the service down.
const (
	// DefaultPageLimit is the page size used when Limit is not specified.
	DefaultPageLimit = 100
	// MaxPageLimit is the largest page size allowed for a single query.
	MaxPageLimit = 1000
)

// normalizeLimit clamps a caller-supplied page size into [1, MaxPageLimit].
//
// The upper bound exists so that no single query can be asked for unbounded
// work; the lower bound turns "unset" into the default rather than into zero
// rows.
func normalizeLimit(limit int) int {
	switch {
	case limit <= 0:
		return DefaultPageLimit
	case limit > MaxPageLimit:
		return MaxPageLimit
	default:
		return limit
	}
}

// Page uses **keyset pagination** rather than offset pagination.
//
// That is a correctness requirement, not a performance preference: when
// reconciliation pages with OFFSET while new rows are being appended, OFFSET
// pagination skips or repeats records, and in a reconciliation scenario a
// skipped record makes the conclusion "the books are balanced" invalid. Keyset
// pagination anchors on AfterID and is naturally unaffected by appends.
type Page struct {
	// AfterID returns only records with a larger ID; 0 means start at the
	// beginning.
	AfterID int64
	// Limit 0 means DefaultPageLimit; the maximum is MaxPageLimit.
	Limit int
}

// LimitOrDefault clamps Limit into [1, MaxPageLimit].
func (p Page) LimitOrDefault() int { return normalizeLimit(p.Limit) }

// AccountPage is the pagination condition for listing accounts.
//
// Accounts have no auto-increment ID, so UserID serves as the keyset anchor.
type AccountPage struct {
	// AfterUserID returns only accounts with a larger UserID.
	AfterUserID int64
	// Limit 0 means DefaultPageLimit; the maximum is MaxPageLimit.
	Limit int
}

// LimitOrDefault clamps Limit into [1, MaxPageLimit].
func (p AccountPage) LimitOrDefault() int { return normalizeLimit(p.Limit) }

// CommissionQuery is the query condition for listing commissions.
type CommissionQuery struct {
	Page
	// States empty means no state filtering.
	States []CommissionState
}

// LedgerQuery is the query condition for the money ledger.
type LedgerQuery struct {
	Page
	// BizType 0 means no business type filtering.
	BizType LedgerBizType
}

// Reader is the read-only access port.
//
// Every method is forced to carry the tenant dimension (through UserKey /
// OrderKey / an explicit tenantID), so a caller cannot forget the tenant
// condition and leak data across tenants.
type Reader interface {
	// Agent returns the given agent; ErrNotFound when it does not exist.
	Agent(ctx context.Context, key UserKey) (Agent, error)

	// Binding returns the buyer's currently effective binding; ErrNotFound
	// when there is none.
	Binding(ctx context.Context, buyer UserKey) (Binding, error)

	// Account returns the given user's account; when the account has never had
	// a money movement it returns a zero-value account instead of ErrNotFound —
	// "zero balance" and "no such account" are indistinguishable to a caller.
	Account(ctx context.Context, key UserKey) (Account, error)

	// Commission returns a commission by ID.
	Commission(ctx context.Context, id int64) (Commission, error)

	// CommissionsByOrder returns every commission of an order (all levels,
	// reversal records included).
	CommissionsByOrder(ctx context.Context, key OrderKey) ([]Commission, error)

	// CommissionsByAgent returns one agent's commissions, paginated.
	CommissionsByAgent(ctx context.Context, key UserKey, q CommissionQuery) ([]Commission, error)

	// RefundsByOrder returns every refund voucher of an order.
	RefundsByOrder(ctx context.Context, key OrderKey) ([]Refund, error)

	// CommissionsByTenant returns every commission under a tenant, paginated,
	// for reconciliation and self-check.
	//
	// Why it has to exist: enumerating commissions only by walking the ledger
	// backwards would miss the "commission booked but no money moved" shape of
	// corruption — and that is exactly the shape that most needs to be found.
	CommissionsByTenant(ctx context.Context, tenantID int64, p Page) ([]Commission, error)

	// DueCommissions returns commissions that have reached their settleable
	// point and are still pending settlement.
	//
	// Implementations should guarantee "O(1) when nothing is due" — Maintain is
	// called once a minute, and the vast majority of those calls should return
	// immediately.
	DueCommissions(ctx context.Context, tenantID int64, dueAt time.Time, limit int) ([]Commission, error)

	// LedgerEntries returns one user's money ledger, paginated.
	LedgerEntries(ctx context.Context, key UserKey, q LedgerQuery) ([]LedgerEntry, error)

	// AccountsByTenant returns every account under a tenant, paginated, for
	// reconciliation.
	AccountsByTenant(ctx context.Context, tenantID int64, p AccountPage) ([]Account, error)

	// LedgerByTenant returns the tenant's entire ledger, paginated, for
	// reconciliation.
	LedgerByTenant(ctx context.Context, tenantID int64, p Page) ([]LedgerEntry, error)

	// CountCommissionsByState counts commissions by state.
	CountCommissionsByState(ctx context.Context, tenantID int64) (map[CommissionState]int64, error)

	// WithdrawalByIdemKey returns one withdrawal by the caller's idempotency
	// key. It returns ErrNotFound when no such withdrawal exists.
	//
	// This is what makes a retried request safe: the caller's key is the only
	// handle it has, so the lookup has to be by key rather than by the
	// library's own id.
	WithdrawalByIdemKey(ctx context.Context, tenantID int64, idemKey string) (Withdrawal, error)

	// WithdrawalByID returns one withdrawal by its internal id.
	WithdrawalByID(ctx context.Context, id int64) (Withdrawal, error)

	// WithdrawalsByTenant returns a tenant's withdrawals, paginated, for
	// reconciliation and for an operator reviewing a queue.
	WithdrawalsByTenant(ctx context.Context, tenantID int64, p Page) ([]Withdrawal, error)

	// CountWithdrawalsByState counts withdrawals by state, which tells a
	// queue's shape at a glance.
	CountWithdrawalsByState(ctx context.Context, tenantID int64) (map[WithdrawalState]int64, error)

	// DueWork returns pending commissions whose settlement time has arrived,
	// across every tenant, earliest first, at most limit of them.
	//
	// # Why the port asks for work rather than for tenants
	//
	// Two earlier shapes of this method were wrong, and both were caught by
	// measuring rather than reasoning.
	//
	// The first listed every tenant, and Maintain opened a transaction per tenant:
	// at a thousand tenants that is a thousand transactions per tick whether or
	// not any work exists. The library does not own tenants - they belong to the
	// caller's domain model - so it has no business enumerating them.
	//
	// The second asked for the distinct tenants that have work, which reads as one
	// bounded lookup and is not. Producing a distinct list in tenant order makes
	// the planner consult the whole due set; on 80k rows it read all of them to
	// return 40 tenants, and bounding the read with a window made it slower still,
	// because the window then had to be sorted before being deduplicated.
	//
	// # Cost
	//
	// An implementation MUST answer this with a range scan over an index ordered
	// by (state, available_at, id), stopping at limit rows. That makes the cost
	// proportional to the batch, not to the backlog, and - the part that matters
	// most for a per-minute heartbeat - it makes the empty case a seek that finds
	// nothing rather than a scan that proves there is nothing.
	//
	// The caller groups the batch by tenant itself and opens one transaction per
	// tenant, which keeps settlement isolated per tenant without giving up the
	// bounded read.
	DueWork(ctx context.Context, dueAt time.Time, limit int) ([]Commission, error)
}

// Writer is the write port.
//
// Every write method must take care of concurrency protection itself:
//   - Put* uses optimistic locking (a Version mismatch returns ErrConflict).
//   - Append* uses a unique constraint (an idempotency key conflict returns
//     ErrDuplicate).
type Writer interface {
	// PutAgent saves an agent; a Version mismatch returns ErrConflict. It
	// returns the persisted entity (with the incremented Version).
	PutAgent(ctx context.Context, a Agent) (Agent, error)

	// PutBinding saves a binding; a Version mismatch returns ErrConflict.
	PutBinding(ctx context.Context, b Binding) (Binding, error)

	// PutAccount saves an account, replacing every balance with the given values;
	// a Version mismatch returns ErrConflict.
	//
	// It models an ADMINISTRATIVE adjustment: an operator setting a balance to a
	// specific figure during a reconciliation or a data migration. That is also
	// exactly the shape of the corruption invariant I1 exists to catch - someone
	// changed the balance and forgot the ledger - which is why the self check
	// tests inject their corruption through it.
	//
	// The engine's own money movements do NOT go through here. They go through
	// IncrementAccount. The difference is the meaning: PutAccount says "I believe
	// the current value is this, set it to that", while IncrementAccount says
	// "whatever it currently is, add this" - and only the latter is what accrual
	// and settlement actually need.
	PutAccount(ctx context.Context, a Account) (Account, error)

	// IncrementAccount applies a set of bucket deltas atomically and returns the
	// updated account.
	//
	// # Why it has to be atomic
	//
	// Read-modify-write does not hold up on a hot account. When many orders belong
	// to one popular agent, optimistic locking makes all but one of them fail and
	// retry the whole event. A delta lets the database serialise them with a row
	// lock: no version conflict, no retry, and no way for a concurrent write to be
	// silently overwritten.
	//
	// A missing account is created, with the delta as its initial values.
	//
	// When delta.RequireNonNegative is set and any bucket would end up negative,
	// the call returns ErrInsufficientBalance and changes nothing. That guard has
	// to live inside the same statement: checking and then writing is two
	// statements, and another writer can get in between them.
	IncrementAccount(ctx context.Context, delta AccountDelta) (Account, error)

	// AppendCommission appends a commission; an idempotency key conflict
	// returns ErrDuplicate. It returns the persisted entity (with the assigned
	// ID).
	AppendCommission(ctx context.Context, c Commission) (Commission, error)

	// SetCommissionAvailableAt determines a commission's payout time for the
	// first time and returns the persisted commission.
	//
	// The semantics are "first writer wins": if the commission already has a
	// non-zero AvailableAt, this method changes nothing and returns it as it
	// is — a duplicate delivery of a receipt event must not push the payout
	// time earlier.
	SetCommissionAvailableAt(ctx context.Context, id int64, at time.Time, expectedVersion int64) (Commission, error)

	// SetCommissionReversedAmount writes the cumulative amount reversed on a
	// commission and returns the persisted commission.
	//
	// It is a dedicated write path for one lifecycle field, not a generic
	// "update a commission": a caller cannot reach any financial field through
	// it. The cumulative amount must fall within [0, Amount]; out of range
	// returns ErrInvalidArgument.
	SetCommissionReversedAmount(ctx context.Context, id int64, reversed Money, expectedVersion int64) (Commission, error)

	// TransitionCommission advances a commission's state and returns the
	// persisted commission.
	//
	// An illegal transition returns ErrIllegalTransition; a current state or
	// version that does not match the expectation returns ErrConflict (the two
	// must stay distinct: the former is a caller logic error, the latter a
	// retryable concurrency conflict).
	TransitionCommission(ctx context.Context, id int64, from, to CommissionState, expectedVersion int64) (Commission, error)

	// AppendWithdrawal appends a withdrawal.
	//
	// An idempotency key conflict returns ErrDuplicate carrying the existing
	// record, so that a retried request returns the original withdrawal rather
	// than reserving the agent's money a second time.
	AppendWithdrawal(ctx context.Context, w Withdrawal) (Withdrawal, error)

	// TransitionWithdrawal advances a withdrawal's state and returns the
	// persisted record.
	//
	// An illegal transition returns ErrIllegalTransition; a current state or
	// version that does not match the expectation returns ErrConflict. The two
	// stay distinct for the same reason as for commissions: one is a caller
	// logic error and the other is a retryable race.
	//
	// The lifecycle timestamps travel in tr rather than being read from a clock
	// inside the store, because processing time has exactly one source, the
	// injected Clock (ADR-023). A store that read its own clock would produce a
	// ledger whose timestamps depend on which backend was underneath.
	TransitionWithdrawal(ctx context.Context, id int64, from, to WithdrawalState, expectedVersion int64, tr WithdrawalTransition) (Withdrawal, error)

	// AppendRefund appends a refund voucher and returns the persisted record.
	//
	// An idempotency key conflict returns ErrDuplicate: that is precisely how
	// "the same refund delivered twice" is recognized, and a caller should
	// treat it as "already refunded" rather than as a failure.
	AppendRefund(ctx context.Context, r Refund) (Refund, error)

	// AppendLedger appends a ledger entry and returns the persisted record.
	// Ledger entries can never be modified or deleted.
	AppendLedger(ctx context.Context, e LedgerEntry) (LedgerEntry, error)
}

// Tx is a read/write handle within one transaction.
type Tx interface {
	Reader
	Writer
}

// Store is the entry point of the persistence port.
//
// # Transaction and retry contract
//
// The function passed to Update **must be safe to run repeatedly**: an
// implementation may retry the whole function on a version conflict. The
// function must therefore have no side effects outside the transaction (no
// message publishing, no external calls, no mutation of variables from the
// enclosing closure).
//
// "Retry" means exactly this, and nothing weaker:
//
//  1. the failed attempt is rolled back completely, and
//  2. the function is invoked again against the state as it was BEFORE that
//     attempt.
//
// Retrying without rolling back first is not permitted. It would expose the
// function to its own partial effects, so a half-applied step would look like
// pre-existing state on the second run. The engine depends on this: settling a
// commission moves its state and then debits the account, and running that again
// is only safe because the first attempt left nothing behind.
type Store interface {
	// View runs fn in a read-only transaction. fn must not call Update/View (it
	// would deadlock or return an error).
	View(ctx context.Context, fn func(ctx context.Context, r Reader) error) error

	// Update runs fn in a read/write transaction. A nil return commits; an
	// error rolls back.
	Update(ctx context.Context, fn func(ctx context.Context, tx Tx) error) error

	// Close releases the underlying resources. Repeated calls must be safe.
	Close() error
}

// WithdrawalTransition carries the lifecycle fields that change when a
// withdrawal advances.
//
// It is a struct rather than a parameter list because the fields are not
// independent of each other: which of them matter depends on the destination
// state. A struct also lets a store write exactly what it was given, with no
// clock of its own and no inference about which timestamp a state implies.
type WithdrawalTransition struct {
	// FailReason explains a payout that did not succeed.
	FailReason string
	// InvoiceNo is the payout provider's reference, set when a payout succeeds.
	InvoiceNo string
	// Operator records who approved or paid it.
	Operator string
	// AuditedAt is the processing time of an approval or rejection.
	AuditedAt time.Time
	// PaidAt is the processing time of a successful payout.
	//
	// A store must reject a transition to Paid without it: a paid withdrawal
	// that does not say when is not auditable (ADR-018).
	PaidAt time.Time
}

// SchemaChecker is an optional interface: a storage layer implements it to take
// part in SelfCheck's schema checks.
//
// The in-memory store not implementing it is reasonable (it has no schema)
// rather than "a placeholder left behind" — not implementing it means not
// taking part, with no half-finished state.
type SchemaChecker interface {
	// CheckSchema returns descriptions of missing or mismatched schema; nil
	// when everything is in place.
	CheckSchema(ctx context.Context) []string
}

// ReportedStore is an optional interface: a storage layer can report its own
// kind through it, which self-check output uses.
type ReportedStore interface {
	// StoreKind returns the storage kind identifier, such as "memory" or
	// "mysql".
	StoreKind() string
}

// AccountReconciliation is one account whose stored state a reconciling store
// could not confirm is consistent with that account's own ledger entries.
//
// It carries the three inputs the conservation check needs — the account row,
// the sum of its ledger deltas, and its last entry — rather than a verdict. The
// store decides which accounts are worth reporting; the library decides whether
// they are actually wrong. That split is deliberate: it keeps one implementation
// of the check, so a reconciling store and a row-wise one cannot drift into
// disagreeing about what a violation is.
type AccountReconciliation struct {
	// Key identifies the account.
	Key UserKey
	// Stored is the account exactly as it is stored.
	Stored Account
	// Summed holds, per bucket, the sum of the account's ledger deltas. Only
	// bucket fields are meaningful; it is an Account so that the same bucket
	// accessors work on it as on any other.
	Summed Account
	// Tail is the account's highest-id ledger entry. HasTail is false when the
	// account has no ledger entries at all, in which case Tail is zero.
	Tail    LedgerEntry
	HasTail bool
}

// Reconciler is an optional interface: a storage layer implements it to check
// accounts against their ledger entries using its own aggregation.
//
// # Why this is not just an optimization
//
// Checking conservation row-wise means streaming every ledger entry of every
// account through the caller. At a billion entries that is both a billion rows
// over the wire and, at a page size of a thousand, a million round trips: hours
// of work holding a read transaction, to discover — in the normal case — that
// nothing is wrong.
//
// A store with an aggregation engine can do the same comparison where the data
// is and hand back only the accounts that fail it, which in the normal case is
// no rows at all. The comparison is still the library's, via
// AccountReconciliation.
//
// Implementing it is optional and mutually exclusive with the row-wise path: a
// store that does not implement it is checked row-wise rather than not checked.
type Reconciler interface {
	// ReconcileAccounts reports up to limit accounts that may disagree with
	// their ledger entries, in ascending account-key order, calling fn for
	// each. It returns an error if fn does.
	//
	// It also returns how many accounts it examined, which is not the same as
	// how many it reported: a healthy tenant reports no candidates out of
	// however many accounts exist. The count is what lets the caller tell a
	// check that found nothing wrong apart from a check that looked at nothing,
	// and those two must not be reported the same way.
	//
	// An implementation must not decide that an account is *correct*: it
	// reports candidates, and the library judges them.
	ReconcileAccounts(
		ctx context.Context,
		tenantID int64,
		limit int,
		fn func(AccountReconciliation) error,
	) (examined int, err error)
}
