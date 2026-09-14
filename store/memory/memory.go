// Package memory 提供 distledger 的内存 Store 实现。
//
// 它的定位是**参考实现与轻量部署**：单元测试、演示、单进程小规模部署。
// 生产环境请使用 store/mysql（具备真正的行锁与索引）。
//
// # 一致性模型
//
//   - Update 持有全局写锁，事务之间完全串行，等价于可串行化隔离级别。
//   - View 持有读锁，不会读到未提交的数据（写锁与读锁互斥）。
//   - 事务失败时通过**撤销日志**回滚。回滚代价与「本次事务触碰的对象数量」
//     成正比，而不是与数据总量成正比。
//
// # 回滚为什么会写这么细
//
// 大多数写操作都是 map upsert 或切片追加，回滚只需「还原旧值」或
// 「截断到原长度」。唯独到期索引 pendingDue 需要在中间插入和删除，
// 这两种操作会让「截断到原长度」恢复出错误的内容（元素被搬移过）。
// 因此本文件对切片采用**混合快照**：
//
//   - 尾部追加：只记录原长度（廉价）。
//   - 中间插入/删除：升级为全量克隆（昂贵但罕见，且必须正确）。
//
// 这是刻意为之：宁可在一个罕见路径上多拷一份索引，也不接受一个
// 「回滚后数据看起来正常、实际错位」的隐蔽缺陷。
package memory

import (
	"context"
	"errors"
	"slices"
	"sync"
	"time"

	"github.com/im10furry/distledger"
)

// idemRef 是幂等键的复合主键。
type idemRef struct {
	tenantID int64
	key      string
}

// dueRef 是到期索引中的一项。
type dueRef struct {
	at time.Time
	id int64
}

// data 是全部状态。所有字段仅在持有 Store.mu 时访问。
type data struct {
	agents   map[distledger.UserKey]distledger.Agent
	bindings map[distledger.UserKey]distledger.Binding
	accounts map[distledger.UserKey]distledger.Account

	commissions map[int64]distledger.Commission
	// commissionIDs 按 ID 升序保存全部佣金 ID，用于键集分页。
	// 它是纯追加索引（ID 单调递增），因此回滚只需记录长度。
	commissionIDs []int64
	idem          map[idemRef]int64
	byOrder       map[distledger.OrderKey][]int64
	byAgent       map[distledger.UserKey][]int64

	ledger         map[int64]distledger.LedgerEntry
	ledgerIDs      []int64
	ledgerByUser   map[distledger.UserKey][]int64
	ledgerByTenant map[int64][]int64

	// pendingDue 按 (at, id) 升序保存「已确定到账时间」的佣金 ID。
	//
	// 它是查询加速用的派生索引：即使内容与真实状态不一致也不会算错钱
	// （读取时校验新鲜度并跳过失效项），只是会多做几次无用的扫描。
	// 正因为它不是事实来源，删除与插入都可以放心地在这里做。
	pendingDue []dueRef

	nextID int64
}

func newData() *data {
	return &data{
		agents:         make(map[distledger.UserKey]distledger.Agent),
		bindings:       make(map[distledger.UserKey]distledger.Binding),
		accounts:       make(map[distledger.UserKey]distledger.Account),
		commissions:    make(map[int64]distledger.Commission),
		idem:           make(map[idemRef]int64),
		byOrder:        make(map[distledger.OrderKey][]int64),
		byAgent:        make(map[distledger.UserKey][]int64),
		ledger:         make(map[int64]distledger.LedgerEntry),
		ledgerByUser:   make(map[distledger.UserKey][]int64),
		ledgerByTenant: make(map[int64][]int64),
	}
}

// Store 是 distledger.Store 的内存实现。零值不可用，请使用 New 构造。
type Store struct {
	mu     sync.RWMutex
	d      *data
	closed bool
}

// New 返回一个空的、可用的内存 Store。
func New() *Store { return &Store{d: newData()} }

// StoreKind 实现 distledger.ReportedStore。
func (s *Store) StoreKind() string { return "memory" }

// Close 释放 Store。重复调用安全。
//
// 关闭后所有读写返回 ErrClosed；已写入的数据仍然保留，便于关闭后审计。
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}

// errNestedTx 表示在事务内部又开启了事务。
//
// 内存实现用全局互斥锁，嵌套事务会直接死锁。与其让调用方遇到一个卡死的
// 进程，不如显式报错——死锁在生产环境里几乎无法定位。
var errNestedTx = errors.New("distledger/memory: nested transaction is not supported")

type txMarkerKey struct{}

func inTx(ctx context.Context) bool { return ctx.Value(txMarkerKey{}) != nil }

// View 在只读事务中执行 fn。
func (s *Store) View(ctx context.Context, fn func(ctx context.Context, r distledger.Reader) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if inTx(ctx) {
		return errNestedTx
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return distledger.ErrClosed
	}
	return fn(context.WithValue(ctx, txMarkerKey{}, true), &reader{d: s.d})
}

// Update 在读写事务中执行 fn。
//
// fn 返回错误时，本次事务内已完成的所有写入都会被撤销，
// 数据回到事务开始前的状态。
func (s *Store) Update(ctx context.Context, fn func(ctx context.Context, tx distledger.Tx) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if inTx(ctx) {
		return errNestedTx
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return distledger.ErrClosed
	}

	u := newUndo(s.d)
	tx := &tx{reader: reader{d: s.d}, u: u}
	if err := fn(context.WithValue(ctx, txMarkerKey{}, true), tx); err != nil {
		u.rollback(s.d)
		return err
	}
	return nil
}

// ── 撤销日志 ────────────────────────────────────────────────────────────

type undoEntry[V any] struct {
	val     V
	existed bool
}

// dueUndo 保存 pendingDue 的回滚信息。
type dueUndo struct {
	saved bool
	// full 非 nil 表示已做全量快照，回滚时整体替换。
	full []dueRef
	// length 仅在 full 为 nil 时有效，表示原始长度。
	length int
}

// undo 记录事务中被触碰过的对象在事务开始前的值。
type undo struct {
	agents      map[distledger.UserKey]undoEntry[distledger.Agent]
	bindings    map[distledger.UserKey]undoEntry[distledger.Binding]
	accounts    map[distledger.UserKey]undoEntry[distledger.Account]
	commissions map[int64]undoEntry[distledger.Commission]
	ledger      map[int64]undoEntry[distledger.LedgerEntry]
	idem        map[idemRef]undoEntry[int64]

	// 纯追加的索引：只需记录原始长度。
	byOrderLen        map[distledger.OrderKey]int
	byAgentLen        map[distledger.UserKey]int
	ledgerByUserLen   map[distledger.UserKey]int
	ledgerByTenantLen map[int64]int
	ledgerIDsLen      int
	commissionIDsLen  int

	// 需要在中间增删的索引：使用混合快照。
	due dueUndo

	nextID int64
}

func newUndo(d *data) *undo {
	return &undo{
		agents:            make(map[distledger.UserKey]undoEntry[distledger.Agent]),
		bindings:          make(map[distledger.UserKey]undoEntry[distledger.Binding]),
		accounts:          make(map[distledger.UserKey]undoEntry[distledger.Account]),
		commissions:       make(map[int64]undoEntry[distledger.Commission]),
		ledger:            make(map[int64]undoEntry[distledger.LedgerEntry]),
		idem:              make(map[idemRef]undoEntry[int64]),
		byOrderLen:        make(map[distledger.OrderKey]int),
		byAgentLen:        make(map[distledger.UserKey]int),
		ledgerByUserLen:   make(map[distledger.UserKey]int),
		ledgerByTenantLen: make(map[int64]int),
		ledgerIDsLen:      len(d.ledgerIDs),
		commissionIDsLen:  len(d.commissionIDs),
		nextID:            d.nextID,
	}
}

func saveAgent(u *undo, d *data, k distledger.UserKey) {
	if _, ok := u.agents[k]; ok {
		return
	}
	v, existed := d.agents[k]
	u.agents[k] = undoEntry[distledger.Agent]{val: v, existed: existed}
}

func saveBinding(u *undo, d *data, k distledger.UserKey) {
	if _, ok := u.bindings[k]; ok {
		return
	}
	v, existed := d.bindings[k]
	u.bindings[k] = undoEntry[distledger.Binding]{val: v, existed: existed}
}

func saveAccount(u *undo, d *data, k distledger.UserKey) {
	if _, ok := u.accounts[k]; ok {
		return
	}
	v, existed := d.accounts[k]
	u.accounts[k] = undoEntry[distledger.Account]{val: v, existed: existed}
}

func saveCommission(u *undo, d *data, id int64) {
	if _, ok := u.commissions[id]; ok {
		return
	}
	v, existed := d.commissions[id]
	u.commissions[id] = undoEntry[distledger.Commission]{val: v, existed: existed}
}

func saveLedger(u *undo, d *data, id int64) {
	if _, ok := u.ledger[id]; ok {
		return
	}
	v, existed := d.ledger[id]
	u.ledger[id] = undoEntry[distledger.LedgerEntry]{val: v, existed: existed}
}

func saveIdem(u *undo, d *data, k idemRef) {
	if _, ok := u.idem[k]; ok {
		return
	}
	v, existed := d.idem[k]
	u.idem[k] = undoEntry[int64]{val: v, existed: existed}
}

// 以下 trunc* 方法在向「纯追加索引」写入之前记录原始长度，
// 回滚时按长度截断即可——因为这类索引永远不会在中间插入或删除。

func (u *undo) truncByOrder(d *data, k distledger.OrderKey) {
	if _, ok := u.byOrderLen[k]; !ok {
		u.byOrderLen[k] = len(d.byOrder[k])
	}
}

func (u *undo) truncByAgent(d *data, k distledger.UserKey) {
	if _, ok := u.byAgentLen[k]; !ok {
		u.byAgentLen[k] = len(d.byAgent[k])
	}
}

func (u *undo) truncLedgerByUser(d *data, k distledger.UserKey) {
	if _, ok := u.ledgerByUserLen[k]; !ok {
		u.ledgerByUserLen[k] = len(d.ledgerByUser[k])
	}
}

func (u *undo) truncLedgerByTenant(d *data, t int64) {
	if _, ok := u.ledgerByTenantLen[t]; !ok {
		u.ledgerByTenantLen[t] = len(d.ledgerByTenant[t])
	}
}

// noteDueAppend 记录一次「尾部追加」。
func (u *undo) noteDueAppend(d *data) {
	if u.due.saved {
		return
	}
	u.due.saved = true
	u.due.length = len(d.pendingDue)
}

// noteDueStructural 记录一次「中间插入或删除」。
//
// 一旦发生结构性变更，必须升级为全量快照：此时按长度截断会恢复出
// 元素错位的内容。
//
// 关键细节：如果此前已经记录过长度快照，说明到此刻为止只发生过尾部追加，
// 因此**事务开始时的内容恰好是当前切片的前 length 个元素**。
// 直接克隆整个当前切片会把本事务追加的条目也当成原始状态，
// 导致回滚不彻底——索引里会残留本该消失的条目。
func (u *undo) noteDueStructural(d *data) {
	if u.due.full != nil {
		return
	}
	if u.due.saved {
		u.due.full = slices.Clone(d.pendingDue[:u.due.length])
	} else {
		u.due.full = slices.Clone(d.pendingDue)
	}
	u.due.saved = true
}

// rollback 把 data 恢复到事务开始前的状态。
func (u *undo) rollback(d *data) {
	restoreMap(u.agents, d.agents)
	restoreMap(u.bindings, d.bindings)
	restoreMap(u.accounts, d.accounts)
	restoreMap(u.commissions, d.commissions)
	restoreMap(u.ledger, d.ledger)
	restoreMap(u.idem, d.idem)

	for k, n := range u.byOrderLen {
		d.byOrder[k] = d.byOrder[k][:n]
	}
	for k, n := range u.byAgentLen {
		d.byAgent[k] = d.byAgent[k][:n]
	}
	for k, n := range u.ledgerByUserLen {
		d.ledgerByUser[k] = d.ledgerByUser[k][:n]
	}
	for k, n := range u.ledgerByTenantLen {
		d.ledgerByTenant[k] = d.ledgerByTenant[k][:n]
	}
	d.ledgerIDs = d.ledgerIDs[:u.ledgerIDsLen]
	d.commissionIDs = d.commissionIDs[:u.commissionIDsLen]

	if u.due.saved {
		if u.due.full != nil {
			d.pendingDue = u.due.full
		} else {
			d.pendingDue = d.pendingDue[:u.due.length]
		}
	}
	d.nextID = u.nextID
}

func restoreMap[K comparable, V any](src map[K]undoEntry[V], dst map[K]V) {
	for k, e := range src {
		if e.existed {
			dst[k] = e.val
		} else {
			delete(dst, k)
		}
	}
}
