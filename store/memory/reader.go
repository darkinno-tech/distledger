package memory

import (
	"context"
	"slices"
	"sort"
	"time"

	"github.com/im10furry/distledger"
)

// reader 是只读句柄。调用方持有它时必须已经持有 Store 的读锁或写锁。
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
	// 账户不存在不是错误：从未产生过资金变动的用户，其账户就是零值账户。
	// 把「零余额」和「账户不存在」区分开，只会让调用方多写一个分支。
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
		// byAgent 按 ID 升序追加，因此可以直接跳过键集锚点之前的部分。
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

// DueCommissions 返回已到可结算时点、且仍处于待结算状态的佣金。
//
// 实现要点：pendingDue 按 (at, id) 升序，因此「第一个 at > dueAt 的下标」
// 之前的全部条目构成候选集。二分定位后只扫描候选集，不扫描全表。
func (r *reader) CommissionsByTenant(ctx context.Context, tenantID int64, p distledger.Page) ([]distledger.Commission, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	limit := p.LimitOrDefault()
	ids := r.d.commissionIDs
	// commissionIDs 升序，二分找到第一个大于锚点的位置。
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

func (r *reader) DueCommissions(ctx context.Context, tenantID int64, dueAt time.Time, limit int) ([]distledger.Commission, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = distledger.DefaultPageLimit
	}
	idx := r.d.pendingDue
	hi := sort.Search(len(idx), func(i int) bool { return idx[i].at.After(dueAt) })
	out := make([]distledger.Commission, 0, minInt(limit, hi))
	for i := 0; i < hi; i++ {
		ref := idx[i]
		c, ok := r.d.commissions[ref.id]
		if !ok {
			continue
		}
		// 新鲜度校验：索引项必须与佣金当前状态完全一致才被采信。
		// 这让索引可以在状态变化时被「惰性失效」，而不必在每次状态
		// 变化时都付出删除代价。
		if c.State != distledger.CommissionPending || !c.AvailableAt.Equal(ref.at) {
			continue
		}
		if c.Key.TenantID != tenantID {
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
	// 账户没有自增 ID，因此以 UserID 作为键集锚点。
	keys := make([]distledger.UserKey, 0, minInt(limit*4, len(r.d.accounts)))
	for k := range r.d.accounts {
		if k.TenantID != tenantID || k.UserID <= p.AfterUserID {
			continue
		}
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].UserID < keys[j].UserID })
	if len(keys) > limit {
		keys = keys[:limit]
	}
	out := make([]distledger.Account, 0, len(keys))
	for _, k := range keys {
		out = append(out, r.d.accounts[k])
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
		// 这是对账路径上的 O(全部佣金) 统计，不在请求链路上。
		// 每 1024 条检查一次取消，避免长时间不可中断。
		n++
		if n%1024 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}

// collectLedger 按键集分页收集流水。
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

func (r *reader) Tenants(ctx context.Context) ([]int64, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	seen := make(map[int64]struct{})
	collect := func(t int64) { seen[t] = struct{}{} }
	for k := range r.d.agents {
		collect(k.TenantID)
	}
	for k := range r.d.bindings {
		collect(k.TenantID)
	}
	for k := range r.d.accounts {
		collect(k.TenantID)
	}
	for k := range r.d.byOrder {
		collect(k.TenantID)
	}
	out := make([]int64, 0, len(seen))
	for t := range seen {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
