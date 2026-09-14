package memory

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"time"

	"github.com/im10furry/distledger"
)

// tx 是事务内的读写句柄。
//
// 它内嵌 reader：读操作直接落到同一份数据上，因此**天然读得到本事务
// 已写入的内容**（read-your-writes），不需要额外维护影子副本。
// 未提交的数据不会被其它事务看到，因为写锁在整个事务期间持有。
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
	// 负余额在这里就被拦下，而不是等到对账时才发现。
	// 这是不变量 I1 的第一道防线。
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

	d := t.reader.d
	ref := idemRef{tenantID: c.Key.TenantID, key: c.IdemKey}
	if _, dup := d.idem[ref]; dup {
		// 幂等命中。调用方应当把它当作「已经记过账」而非失败。
		return c, distledger.ErrDuplicate
	}

	d.nextID++
	c.ID = d.nextID

	saveIdem(t.u, d, ref)
	d.idem[ref] = c.ID
	saveCommission(t.u, d, c.ID)
	d.commissions[c.ID] = c
	d.commissionIDs = append(d.commissionIDs, c.ID)

	t.u.truncByOrder(d, c.Key)
	d.byOrder[c.Key] = append(d.byOrder[c.Key], c.ID)

	agentKey := distledger.UserKey{TenantID: c.Key.TenantID, UserID: c.AgentUserID}
	t.u.truncByAgent(d, agentKey)
	d.byAgent[agentKey] = append(d.byAgent[agentKey], c.ID)

	if c.State == distledger.CommissionPending && !c.AvailableAt.IsZero() {
		insertDue(d, t.u, c.AvailableAt, c.ID)
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
	// 先到先得：到账时间一旦确定就不再改变。
	//
	// 这条规则防的是「用重复投递把到账时间往前刷」——如果没有它，
	// 分销员只要反复触发收货回调就能提前提现。
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

	if c.State == distledger.CommissionPending {
		insertDue(d, t.u, c.AvailableAt, id)
	}
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
		// 当前状态与预期不符，说明有人已经推进过它。
		// 这属于并发冲突而不是非法迁移，区分开才能让调用方正确重试。
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

	// 离开待结算状态后，到期索引中的条目立即失效并从索引中移除，
	// 避免索引随结算推进无限膨胀。
	if from == distledger.CommissionPending && !c.AvailableAt.IsZero() {
		removeDue(d, t.u, c.AvailableAt, id)
	}
	return c, nil
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

	t.u.truncLedgerByUser(d, e.Key)
	d.ledgerByUser[e.Key] = append(d.ledgerByUser[e.Key], e.ID)

	t.u.truncLedgerByTenant(d, e.Key.TenantID)
	d.ledgerByTenant[e.Key.TenantID] = append(d.ledgerByTenant[e.Key.TenantID], e.ID)

	return e, nil
}

// checkVersion 实现乐观锁校验。
//
// 已存在期望版本必须相等；不存在时期望版本必须为 0（表示「我认为它是新的」）。
// 这条规则让「两个事务同时创建同一个对象」也能被检测出来：
// 后提交的那个会发现对象已存在而自己的期望版本是 0。
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

// insertDue 把一项按 (at, id) 升序插入到期索引。
func insertDue(d *data, u *undo, at time.Time, id int64) {
	idx := d.pendingDue
	pos := sort.Search(len(idx), func(i int) bool {
		if idx[i].at.Equal(at) {
			return idx[i].id >= id
		}
		return idx[i].at.After(at)
	})
	if pos == len(idx) {
		// 尾部追加：回滚只需截断，代价 O(1)。
		u.noteDueAppend(d)
		d.pendingDue = append(idx, dueRef{at: at, id: id})
		return
	}
	// 中间插入：元素会被搬移，必须升级为全量快照。
	u.noteDueStructural(d)
	d.pendingDue = slices.Insert(idx, pos, dueRef{at: at, id: id})
}

// removeDue 从到期索引中移除一项。
func removeDue(d *data, u *undo, at time.Time, id int64) {
	idx := d.pendingDue
	pos := sort.Search(len(idx), func(i int) bool {
		if idx[i].at.Equal(at) {
			return idx[i].id >= id
		}
		return idx[i].at.After(at)
	})
	if pos >= len(idx) || idx[pos].id != id || !idx[pos].at.Equal(at) {
		return
	}
	u.noteDueStructural(d)
	d.pendingDue = slices.Delete(idx, pos, pos+1)
}

// fieldErr / fieldErrf 在包内复用根包的校验错误格式。
//
// 这里刻意不导出根包的构造函数，而是定义两个等价的本地函数：
// 它们产生的错误同样能被 errors.Is(err, distledger.ErrInvalidArgument) 命中，
// 因为根包的错误类型是导出的。
func fieldErr(field, reason string) error {
	return &distledger.FieldError{Field: field, Reason: reason}
}

func fieldErrf(field, format string, args ...any) error {
	return &distledger.FieldError{Field: field, Reason: fmt.Sprintf(format, args...)}
}
