package memory

import (
	"context"
	"slices"
	"sort"
	"time"

	"github.com/im10furry/distledger"
)

// reader is the read-only handle. The caller must already hold either the read
// or the write lock of the owning Store.
type reader struct{ d *data }

var _ distledger.Reader = (*reader)(nil)

func (r *reader) Agent(ctx context.Context, key distledger.UserKey) (distledger.Agent, error) {
	if err := ctx.Err(); err != nil {
		return distledger.Agent{}, err
	}
	a, ok := r.d.agents[key]
	if !ok {
		return distledger.Agent{}, distledger.ErrNotFound
	}
	return a, nil
}

func (r *reader) Binding(ctx context.Context, buyer distledger.UserKey) (distledger.Binding, error) {
	if err := ctx.Err(); err != nil {
		return distledger.Binding{}, err
	}
	b, ok := r.d.bindings[buyer]
	if !ok {
		return distledger.Binding{}, distledger.ErrNotFound
	}
	return b, nil
}

func (r *reader) Account(ctx context.Context, key distledger.UserKey) (distledger.Account, error) {
	if err := ctx.Err(); err != nil {
		return distledger.Account{}, err
	}
	// A missing account is not an error: a user who has never moved money
	// simply has the zero account. Distinguishing "zero balance" from "no
	// account" would only force every caller to write another branch.
	return r.d.accounts[key], nil
}

func (r *reader) Commission(ctx context.Context, id int64) (distledger.Commission, error) {
	if err := ctx.Err(); err != nil {
		return distledger.Commission{}, err
	}
	c, ok := r.d.commissions[id]
	if !ok {
		return distledger.Commission{}, distledger.ErrNotFound
	}
	return c, nil
}

func (r *reader) CommissionsByOrder(ctx context.Context, key distledger.OrderKey) ([]distledger.Commission, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	ids := r.d.byOrder[key]
	out := make([]distledger.Commission, 0, len(ids))
	for _, id := range ids {
		if c, ok := r.d.commissions[id]; ok {
			out = append(out, c)
		}
	}
	return out, nil
}

func (r *reader) CommissionsByAgent(ctx context.Context, key distledger.UserKey, q distledger.CommissionQuery) ([]distledger.Commission, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	ids := r.d.byAgent[key]
	limit := q.Page.LimitOrDefault()
	out := make([]distledger.Commission, 0, minInt(limit, len(ids)))
	for _, id := range ids {
		// byAgent is appended in ascending id order, so the keyset anchor can
		// be skipped directly.
		if id <= q.Page.AfterID {
			continue
		}
		c, ok := r.d.commissions[id]
		if !ok {
			continue
		}
		if len(q.States) > 0 && !slices.Contains(q.States, c.State) {
			continue
		}
		out = append(out, c)
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

func (r *reader) RefundsByOrder(ctx context.Context, key distledger.OrderKey) ([]distledger.Refund, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	ids := r.d.refundsByOrder[key]
	out := make([]distledger.Refund, 0, len(ids))
	for _, id := range ids {
		if refund, ok := r.d.refunds[id]; ok {
			out = append(out, refund)
		}
	}
	return out, nil
}

func (r *reader) CommissionsByTenant(ctx context.Context, tenantID int64, p distledger.Page) ([]distledger.Commission, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	limit := p.LimitOrDefault()
	ids := r.d.commissionIDs
	// commissionIDs is ascending, so a binary search finds the first entry past
	// the keyset anchor.
	start := sort.Search(len(ids), func(i int) bool { return ids[i] > p.AfterID })
	out := make([]distledger.Commission, 0, minInt(limit, len(ids)-start))
	for i := start; i < len(ids) && len(out) < limit; i++ {
		c, ok := r.d.commissions[ids[i]]
		if !ok || c.Key.TenantID != tenantID {
			continue
		}
		out = append(out, c)
	}
	return out, nil
}

// DueCommissions returns pending commissions whose settlement time has arrived.
//
// The implementation matters here: pendingDue is sorted by (tenant, at, id), so
// one tenant's slice is a contiguous range found by two binary searches. Only
// that range is scanned. Filtering by tenant AFTER a global scan - which is what
// a naive implementation does - would make every tenant's heartbeat linear in
// the installation-wide backlog, and would quietly break the O(log n + limit)
// promise the port makes.
func (r *reader) DueCommissions(ctx context.Context, tenantID int64, dueAt time.Time, limit int) ([]distledger.Commission, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = distledger.DefaultPageLimit
	}
	idx := r.d.pendingDue

	lo := sort.Search(len(idx), func(i int) bool { return idx[i].tenantID >= tenantID })
	hi := lo + sort.Search(len(idx)-lo, func(i int) bool {
		e := idx[lo+i]
		return e.tenantID != tenantID || e.at.After(dueAt)
	})

	out := make([]distledger.Commission, 0, minInt(limit, hi-lo))
	for i := lo; i < hi; i++ {
		ref := idx[i]
		c, ok := r.d.commissions[ref.id]
		if !ok {
			continue
		}
		// Freshness check: the index entry is only trusted when it agrees with
		// the commission's current state. That lets stale entries be invalidated
		// lazily instead of paying for a delete on every state change.
		if c.State != distledger.CommissionPending || !c.AvailableAt.Equal(ref.at) {
			continue
		}
		out = append(out, c)
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

func (r *reader) LedgerEntries(ctx context.Context, key distledger.UserKey, q distledger.LedgerQuery) ([]distledger.LedgerEntry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return collectLedger(r.d, r.d.ledgerByUser[key], q), nil
}

func (r *reader) AccountsByTenant(ctx context.Context, tenantID int64, p distledger.AccountPage) ([]distledger.Account, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	limit := p.LimitOrDefault()
	idx := r.d.accountIDs

	// Accounts have no auto-increment id, so UserID is the keyset anchor.
	start := sort.Search(len(idx), func(i int) bool {
		if idx[i].TenantID != tenantID {
			return idx[i].TenantID >= tenantID
		}
		return idx[i].UserID > p.AfterUserID
	})

	out := make([]distledger.Account, 0, limit)
	for i := start; i < len(idx) && len(out) < limit; i++ {
		if idx[i].TenantID != tenantID {
			break
		}
		if acct, ok := r.d.accounts[idx[i]]; ok {
			out = append(out, acct)
		}
	}
	return out, nil
}

func (r *reader) LedgerByTenant(ctx context.Context, tenantID int64, p distledger.Page) ([]distledger.LedgerEntry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return collectLedger(r.d, r.d.ledgerByTenant[tenantID], distledger.LedgerQuery{Page: p}), nil
}

func (r *reader) CountCommissionsByState(ctx context.Context, tenantID int64) (map[distledger.CommissionState]int64, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	out := make(map[distledger.CommissionState]int64, len(distledger.AllCommissionStates()))
	n := 0
	for _, c := range r.d.commissions {
		if c.Key.TenantID != tenantID {
			continue
		}
		out[c.State]++
		// This is an O(all commissions) statistic on the reconciliation path,
		// not on a request path. Cancellation is checked periodically so the
		// call stays interruptible on a large tenant.
		n++
		if n%1024 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}

// DueWork returns the earliest due commissions across every tenant.
//
// The index is sorted by (tenant, due time, id), which is not the order this
// needs, so it cannot simply take a prefix. It walks the index once and keeps the
// deadlines that arrive soonest, which is a bounded selection rather than a sort
// of everything: the heap never holds more than limit entries no matter how many
// commissions are pending.
//
// The cost is therefore O(pending entries with a settlement date) for the walk,
// with O(log limit) work per candidate. That is the wrong side of the trade for a
// production-sized installation, which is why the SQL backends answer this from
// an index instead - but it keeps the in-memory store, whose documented role is
// tests and small deployments, free of a second ordering to maintain.
func (r *reader) DueWork(ctx context.Context, dueAt time.Time, limit int) ([]distledger.Commission, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = distledger.DefaultPageLimit
	}

	// A max-heap keyed by (due time, id) keeps the soonest `limit` candidates,
	// with the worst of them on top so a better one can replace it in O(log n).
	worst := func(a, b dueRef) bool {
		if !a.at.Equal(b.at) {
			return a.at.After(b.at)
		}
		return a.id > b.id
	}
	heap := make([]dueRef, 0, minInt(limit, len(r.d.pendingDue)))

	for _, ref := range r.d.pendingDue {
		if ref.at.After(dueAt) {
			continue
		}
		c, ok := r.d.commissions[ref.id]
		if !ok {
			continue
		}
		// The same freshness rule as everywhere else: an index entry that
		// disagrees with its commission is ignored rather than trusted.
		if c.State != distledger.CommissionPending || !c.AvailableAt.Equal(ref.at) {
			continue
		}
		if len(heap) < limit {
			heap = append(heap, ref)
			if len(heap) == limit {
				heapifyWorstFirst(heap, worst)
			}
			continue
		}
		if worst(heap[0], ref) {
			heap[0] = ref
			siftDownWorstFirst(heap, worst)
		}
	}

	out := make([]distledger.Commission, 0, len(heap))
	for _, ref := range heap {
		if c, ok := r.d.commissions[ref.id]; ok {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].AvailableAt.Equal(out[j].AvailableAt) {
			return out[i].AvailableAt.Before(out[j].AvailableAt)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// heapifyWorstFirst arranges refs so the least desirable entry is at the root.
func heapifyWorstFirst(refs []dueRef, worse func(a, b dueRef) bool) {
	for i := len(refs)/2 - 1; i >= 0; i-- {
		siftDownWorstFirstFrom(refs, i, worse)
	}
}

func siftDownWorstFirst(refs []dueRef, worse func(a, b dueRef) bool) {
	siftDownWorstFirstFrom(refs, 0, worse)
}

func siftDownWorstFirstFrom(refs []dueRef, i int, worse func(a, b dueRef) bool) {
	for {
		l, rr := 2*i+1, 2*i+2
		largest := i
		if l < len(refs) && worse(refs[l], refs[largest]) {
			largest = l
		}
		if rr < len(refs) && worse(refs[rr], refs[largest]) {
			largest = rr
		}
		if largest == i {
			return
		}
		refs[i], refs[largest] = refs[largest], refs[i]
		i = largest
	}
}

// collectLedger gathers ledger entries through a keyset-paginated id index.
func collectLedger(d *data, ids []int64, q distledger.LedgerQuery) []distledger.LedgerEntry {
	limit := q.Page.LimitOrDefault()
	out := make([]distledger.LedgerEntry, 0, minInt(limit, len(ids)))
	for _, id := range ids {
		if id <= q.Page.AfterID {
			continue
		}
		e, ok := d.ledger[id]
		if !ok {
			continue
		}
		if q.BizType != 0 && e.BizType != q.BizType {
			continue
		}
		out = append(out, e)
		if len(out) == limit {
			break
		}
	}
	return out
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
