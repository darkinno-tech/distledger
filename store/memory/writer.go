package memory

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"time"

	"github.com/im10furry/distledger"
)

// tx is the read-write handle used inside a transaction.
//
// It embeds reader, so reads hit the same maps the transaction is writing. That
// gives read-your-writes for free without maintaining a shadow copy, and
// uncommitted data stays invisible to other transactions because the write lock
// is held for the whole transaction.
type tx struct {
	reader
	u *undo
}

var _ distledger.Tx = (*tx)(nil)

func (t *tx) PutAgent(ctx context.Context, a distledger.Agent) (distledger.Agent, error) {
	if err := ctx.Err(); err != nil {
		return a, err
	}
	if err := a.Key.Validate(); err != nil {
		return a, err
	}
	if a.ParentID < 0 {
		return a, fieldErrf("parent_id", "must be >= 0, got %d", a.ParentID)
	}
	if a.ParentID != 0 && a.ParentID == a.Key.UserID {
		return a, fieldErr("parent_id", "must not equal user_id")
	}
	if !a.Status.Valid() {
		return a, fieldErrf("status", "unknown agent status %d", uint8(a.Status))
	}
	if !a.JoinType.Valid() {
		return a, fieldErrf("join_type", "unknown join type %d", uint8(a.JoinType))
	}

	d := t.reader.d
	cur, existed := d.agents[a.Key]
	if err := checkVersion("agent", a.Key.String(), a.Version, cur.Version, existed); err != nil {
		return a, err
	}

	saveAgent(t.u, d, a.Key)
	a.Version++
	d.agents[a.Key] = a
	return a, nil
}

func (t *tx) PutBinding(ctx context.Context, b distledger.Binding) (distledger.Binding, error) {
	if err := ctx.Err(); err != nil {
		return b, err
	}
	if err := b.Buyer.Validate(); err != nil {
		return b, err
	}
	if b.AgentUserID <= 0 {
		return b, fieldErrf("agent_user_id", "must be > 0, got %d", b.AgentUserID)
	}
	if !b.Source.Valid() {
		return b, fieldErrf("source", "unknown bind source %d", uint8(b.Source))
	}

	d := t.reader.d
	cur, existed := d.bindings[b.Buyer]
	if err := checkVersion("binding", b.Buyer.String(), b.Version, cur.Version, existed); err != nil {
		return b, err
	}

	saveBinding(t.u, d, b.Buyer)
	b.Version++
	d.bindings[b.Buyer] = b
	return b, nil
}

func (t *tx) PutAccount(ctx context.Context, a distledger.Account) (distledger.Account, error) {
	if err := ctx.Err(); err != nil {
		return a, err
	}
	if err := a.Key.Validate(); err != nil {
		return a, err
	}
	// Negative buckets are rejected here rather than being discovered during
	// reconciliation. This is the first line of defence for invariant I1.
	if !a.Settleable() {
		return a, fieldErrf("account",
			"negative bucket balance: frozen=%s available=%s withdrawing=%s withdrawn=%s",
			a.Frozen, a.Available, a.Withdrawing, a.Withdrawn)
	}

	d := t.reader.d
	cur, existed := d.accounts[a.Key]
	if err := checkVersion("account", a.Key.String(), a.Version, cur.Version, existed); err != nil {
		return a, err
	}

	saveAccount(t.u, d, a.Key)
	if !existed {
		insertAccountID(d, t.u, a.Key)
	}
	a.Version++
	d.accounts[a.Key] = a
	return a, nil
}

func (t *tx) AppendCommission(ctx context.Context, c distledger.Commission) (distledger.Commission, error) {
	if err := ctx.Err(); err != nil {
		return c, err
	}
	if err := c.Key.Validate(); err != nil {
		return c, err
	}
	if c.IdemKey == "" {
		return c, fieldErr("idem_key", "must not be empty")
	}
	if c.AgentUserID <= 0 {
		return c, fieldErrf("agent_user_id", "must be > 0, got %d", c.AgentUserID)
	}
	if c.Layer < 1 || c.Layer > distledger.MaxLevels {
		return c, fieldErrf("layer", "must be in [1, %d], got %d", distledger.MaxLevels, c.Layer)
	}
	if !c.State.Valid() {
		return c, fieldErrf("state", "unknown commission state %d", uint8(c.State))
	}
	// Sign and linkage must agree. An accrual is positive and stands on its own;
	// a reversal is negative and must point at what it reverses. Allowing a
	// negative record with no link, or a linked record with a positive amount,
	// would let a caller inject rows the engine can neither interpret nor
	// reconcile.
	switch {
	case c.Amount == 0:
		return c, fieldErr("amount", "a zero-amount commission carries no meaning; omit it instead")
	case c.Amount < 0 && c.ReverseOf <= 0:
		return c, fieldErr("reverse_of", "a negative commission must reference the accrual it reverses")
	case c.Amount > 0 && c.ReverseOf != 0:
		return c, fieldErr("reverse_of", "an accrual must not reference another commission")
	}
	if c.ReversedAmount < 0 || c.ReversedAmount > c.MaxReversible() {
		return c, fieldErrf("reversed_amount",
			"must be within [0, %s]; got %s", c.MaxReversible(), c.ReversedAmount)
	}
	if c.ReverseOf < 0 {
		return c, fieldErrf("reverse_of", "must be >= 0, got %d", c.ReverseOf)
	}

	d := t.reader.d
	ref := idemRef{tenantID: c.Key.TenantID, key: c.IdemKey}
	if _, dup := d.idem[ref]; dup {
		// Idempotency hit. Callers should treat this as "already booked"
		// rather than as a failure.
		return c, distledger.ErrDuplicate
	}

	d.nextID++
	c.ID = d.nextID

	saveIdem(t.u, d, ref)
	d.idem[ref] = c.ID
	saveCommission(t.u, d, c.ID)
	d.commissions[c.ID] = c
	d.commissionIDs = append(d.commissionIDs, c.ID)

	noteAppendIndex(t.u.byOrder, d.byOrder, c.Key)
	d.byOrder[c.Key] = append(d.byOrder[c.Key], c.ID)

	agentKey := distledger.UserKey{TenantID: c.Key.TenantID, UserID: c.AgentUserID}
	noteAppendIndex(t.u.byAgent, d.byAgent, agentKey)
	d.byAgent[agentKey] = append(d.byAgent[agentKey], c.ID)

	if dueTracked(c.State) && !c.AvailableAt.IsZero() {
		insertDue(d, t.u, c.Key.TenantID, c.AvailableAt, c.ID)
	}
	return c, nil
}

func (t *tx) SetCommissionAvailableAt(ctx context.Context, id int64, at time.Time, expectedVersion int64) (distledger.Commission, error) {
	if err := ctx.Err(); err != nil {
		return distledger.Commission{}, err
	}
	d := t.reader.d
	c, ok := d.commissions[id]
	if !ok {
		return distledger.Commission{}, distledger.ErrNotFound
	}
	// First write wins: once the settlement date is fixed it never changes.
	//
	// The rule defends against replaying a receipt event to pull the settlement
	// date forward. Without it, a agent could unlock their commission
	// simply by re-triggering the callback.
	if !c.AvailableAt.IsZero() {
		return c, nil
	}
	if c.Version != expectedVersion {
		return c, &distledger.ConflictError{
			Kind: "commission", ID: id, Expected: expectedVersion, Actual: c.Version,
		}
	}

	saveCommission(t.u, d, id)
	c.AvailableAt = at.UTC()
	c.Version++
	d.commissions[id] = c

	if dueTracked(c.State) {
		insertDue(d, t.u, c.Key.TenantID, c.AvailableAt, id)
	}
	return c, nil
}

func (t *tx) SetCommissionReversedAmount(ctx context.Context, id int64, reversed distledger.Money, expectedVersion int64) (distledger.Commission, error) {
	if err := ctx.Err(); err != nil {
		return distledger.Commission{}, err
	}
	d := t.reader.d
	c, ok := d.commissions[id]
	if !ok {
		return distledger.Commission{}, distledger.ErrNotFound
	}
	if reversed < 0 || reversed > c.MaxReversible() {
		// The accumulator is bounded by the original amount. Letting it exceed
		// that would let a refund claw back more than was ever accrued, which
		// is exactly what invariant I3 exists to prevent.
		return c, fieldErrf("reversed_amount",
			"must be within [0, %s]; got %s", c.MaxReversible(), reversed)
	}
	if c.Version != expectedVersion {
		return c, &distledger.ConflictError{
			Kind: "commission", ID: id, Expected: expectedVersion, Actual: c.Version,
		}
	}
	if c.ReversedAmount == reversed {
		return c, nil
	}

	saveCommission(t.u, d, id)
	c.ReversedAmount = reversed
	c.Version++
	d.commissions[id] = c
	return c, nil
}

func (t *tx) TransitionCommission(ctx context.Context, id int64, from, to distledger.CommissionState, expectedVersion int64) (distledger.Commission, error) {
	if err := ctx.Err(); err != nil {
		return distledger.Commission{}, err
	}
	d := t.reader.d
	c, ok := d.commissions[id]
	if !ok {
		return distledger.Commission{}, distledger.ErrNotFound
	}
	if c.State != from {
		// The current state is not what the caller expected, so somebody else
		// already advanced it. That is a retryable conflict rather than an
		// illegal transition, and the two must stay distinguishable.
		return c, &distledger.ConflictError{
			Kind: "commission state", ID: id, Expected: int64(from), Actual: int64(c.State),
		}
	}
	if c.Version != expectedVersion {
		return c, &distledger.ConflictError{
			Kind: "commission", ID: id, Expected: expectedVersion, Actual: c.Version,
		}
	}
	if err := distledger.ValidateCommissionTransition(from, to); err != nil {
		return c, err
	}

	saveCommission(t.u, d, id)
	c.State = to
	c.Version++
	d.commissions[id] = c

	// Leaving the tracked set invalidates the settlement index entry straight
	// away, so the index cannot grow without bound as settlement progresses.
	//
	// Freezing keeps the entry: the settlement date stays known, and a later
	// unfreeze must make the commission settle immediately rather than strand
	// it until somebody re-delivers the receipt event.
	if !dueTracked(to) && !c.AvailableAt.IsZero() {
		removeDue(d, t.u, c.Key.TenantID, c.AvailableAt, id)
	}
	return c, nil
}

// dueTracked reports whether a state keeps its settlement date in the index.
func dueTracked(s distledger.CommissionState) bool {
	return s == distledger.CommissionPending || s == distledger.CommissionFrozen
}

func (t *tx) AppendRefund(ctx context.Context, r distledger.Refund) (distledger.Refund, error) {
	if err := ctx.Err(); err != nil {
		return r, err
	}
	if err := r.Key.Validate(); err != nil {
		return r, err
	}
	if r.IdemKey == "" {
		return r, fieldErr("idem_key", "must not be empty")
	}
	if r.Amount <= 0 {
		return r, fieldErrf("amount", "must be > 0, got %s", r.Amount)
	}
	if r.Cumulative < r.Amount {
		return r, fieldErrf("cumulative",
			"must be at least the instalment amount %s, got %s", r.Amount, r.Cumulative)
	}

	d := t.reader.d
	ref := idemRef{tenantID: r.Key.TenantID, key: r.IdemKey}
	if _, dup := d.refundIdem[ref]; dup {
		return r, distledger.ErrDuplicate
	}

	d.nextID++
	r.ID = d.nextID

	saveRefundIdem(t.u, d, ref)
	d.refundIdem[ref] = r.ID
	saveRefund(t.u, d, r.ID)
	d.refunds[r.ID] = r

	noteAppendIndex(t.u.refundsByOrder, d.refundsByOrder, r.Key)
	d.refundsByOrder[r.Key] = append(d.refundsByOrder[r.Key], r.ID)
	return r, nil
}

func (t *tx) AppendLedger(ctx context.Context, e distledger.LedgerEntry) (distledger.LedgerEntry, error) {
	if err := ctx.Err(); err != nil {
		return e, err
	}
	if err := e.Key.Validate(); err != nil {
		return e, err
	}
	if !e.BizType.Valid() {
		return e, fieldErrf("biz_type", "unknown ledger biz type %d", uint8(e.BizType))
	}
	if len(e.Remark) > distledger.MaxRemarkLen {
		return e, fieldErrf("remark", "must be at most %d bytes, got %d",
			distledger.MaxRemarkLen, len(e.Remark))
	}

	d := t.reader.d
	d.nextID++
	e.ID = d.nextID

	saveLedger(t.u, d, e.ID)
	d.ledger[e.ID] = e
	d.ledgerIDs = append(d.ledgerIDs, e.ID)

	noteAppendIndex(t.u.ledgerByUser, d.ledgerByUser, e.Key)
	d.ledgerByUser[e.Key] = append(d.ledgerByUser[e.Key], e.ID)

	noteAppendIndex(t.u.ledgerByTenant, d.ledgerByTenant, e.Key.TenantID)
	d.ledgerByTenant[e.Key.TenantID] = append(d.ledgerByTenant[e.Key.TenantID], e.ID)

	return e, nil
}

// checkVersion implements optimistic locking.
//
// When the object already exists the expected version must match; when it does
// not, the expected version must be zero, meaning "I believe this is new". That
// rule also catches two transactions racing to create the same object: the one
// that commits second finds it already there while expecting version zero.
func checkVersion(kind, id string, expected, actual int64, existed bool) error {
	if !existed {
		if expected != 0 {
			return &distledger.ConflictError{Kind: kind, ID: id, Expected: expected, Actual: 0}
		}
		return nil
	}
	if expected != actual {
		return &distledger.ConflictError{Kind: kind, ID: id, Expected: expected, Actual: actual}
	}
	return nil
}

// insertDue adds an entry to the settlement index, keeping it sorted by
// (tenant, at, id).
func insertDue(d *data, u *undo, tenantID int64, at time.Time, id int64) {
	idx := d.pendingDue
	ref := dueRef{tenantID: tenantID, at: at.UTC(), id: id}
	pos := sort.Search(len(idx), func(i int) bool { return !idx[i].less(ref) })
	if pos == len(idx) {
		// Tail append: the rollback only needs the old length.
		u.due.noteAppend(idx)
		d.pendingDue = append(idx, ref)
		return
	}
	// Middle insert: elements shift, so escalate to a full snapshot.
	u.due.noteStructural(idx)
	d.pendingDue = slices.Insert(idx, pos, ref)
}

// removeDue drops an entry from the settlement index.
func removeDue(d *data, u *undo, tenantID int64, at time.Time, id int64) {
	idx := d.pendingDue
	ref := dueRef{tenantID: tenantID, at: at.UTC(), id: id}
	pos := sort.Search(len(idx), func(i int) bool { return !idx[i].less(ref) })
	if pos >= len(idx) || idx[pos] != ref {
		return
	}
	u.due.noteStructural(idx)
	d.pendingDue = slices.Delete(idx, pos, pos+1)
}

// insertAccountID keeps the account index sorted by (tenant, user).
func insertAccountID(d *data, u *undo, key distledger.UserKey) {
	idx := d.accountIDs
	pos := sort.Search(len(idx), func(i int) bool {
		if idx[i].TenantID != key.TenantID {
			return idx[i].TenantID >= key.TenantID
		}
		return idx[i].UserID >= key.UserID
	})
	if pos < len(idx) && idx[pos] == key {
		return
	}
	if pos == len(idx) {
		u.accountIDs.noteAppend(idx)
		d.accountIDs = append(idx, key)
		return
	}
	u.accountIDs.noteStructural(idx)
	d.accountIDs = slices.Insert(idx, pos, key)
}

// fieldErr / fieldErrf build the same validation errors the root package uses.
//
// The constructors are not exported by the root package on purpose; the error
// type is, so these still satisfy errors.Is(err, distledger.ErrInvalidArgument).
func fieldErr(field, reason string) error {
	return &distledger.FieldError{Field: field, Reason: reason}
}

func fieldErrf(field, format string, args ...any) error {
	return &distledger.FieldError{Field: field, Reason: fmt.Sprintf(format, args...)}
}
