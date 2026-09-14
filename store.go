package distledger

import (
	"context"
	"time"
)

// 本文件定义**持久化端口**（ports & adapters 中的 port）。
//
// 为什么端口定义在根包而不是 store 子包：Go 的惯例是「接口由消费方定义」。
// 根包既实现编排逻辑、又向用户暴露可替换的存储，如果把端口放在 store 子包，
// 根包就必须 import store，而 store 的实现又要 import 根包取类型，形成环。
// 端口留在根包，实现放在 store/memory、store/mysql，依赖方向单向且清晰。
//
// # 接口刻意**不提供**的能力
//
//   - 修改佣金金额：财务字段不可变，只有 AppendCommission。
//   - 修改或删除流水：账本只能追加。
//   - 通用 Update/Delete：任何「全量覆盖」入口都会绕过不变量校验。
//
// 也就是说，「账本不可变」不是靠代码评审来保证的，而是靠端口形状保证的。

// 分页上限。它们存在的意义是让任何一次查询的工作量有上界，
// 避免一次 `LedgerEntries{Limit: 1<<31}` 把服务打挂。
const (
	// DefaultPageLimit 是未指定 Limit 时使用的条数。
	DefaultPageLimit = 100
	// MaxPageLimit 是单次查询允许的最大条数。
	MaxPageLimit = 1000
)

// Normalize 把 Limit 收敛到 [1, MaxPageLimit]。
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

// Page 使用**键集分页**（keyset）而不是偏移分页。
//
// 这不是性能偏好而是正确性要求：对账时如果一边用 OFFSET 翻页、一边有新数据
// 追加，OFFSET 分页会漏读或重复读记录，而漏读在对账场景里等于「账是平的」
// 这个结论不成立。键集分页以 AfterID 为锚点，天然不受追加影响。
type Page struct {
	// AfterID 只返回 ID 大于它的记录；0 表示从头开始。
	AfterID int64
	// Limit 为 0 时取 DefaultPageLimit，最大 MaxPageLimit。
	Limit int
}

// LimitOrDefault 把 Limit 收敛到 [1, MaxPageLimit]。
func (p Page) LimitOrDefault() int { return normalizeLimit(p.Limit) }

// AccountPage 是账户列表的分页条件。
//
// 账户没有自增 ID，因此使用 UserID 作为键集锚点。
type AccountPage struct {
	// AfterUserID 只返回 UserID 大于它的账户。
	AfterUserID int64
	// Limit 为 0 时取 DefaultPageLimit，最大 MaxPageLimit。
	Limit int
}

// LimitOrDefault 把 Limit 收敛到 [1, MaxPageLimit]。
func (p AccountPage) LimitOrDefault() int { return normalizeLimit(p.Limit) }

// CommissionQuery 是佣金列表查询条件。
type CommissionQuery struct {
	Page
	// States 为空表示不过滤状态。
	States []CommissionState
}

// LedgerQuery 是资金流水查询条件。
type LedgerQuery struct {
	Page
	// BizType 为 0 表示不过滤业务类型。
	BizType LedgerBizType
}

// Reader 是只读访问端口。
//
// 所有方法都强制携带租户维度（通过 UserKey / OrderKey / 显式 tenantID），
// 避免调用方漏写租户条件导致跨租户数据泄露。
type Reader interface {
	// Agent 返回指定分销员；不存在时返回 ErrNotFound。
	Agent(ctx context.Context, key UserKey) (Agent, error)

	// Binding 返回指定买家当前生效的绑定；不存在时返回 ErrNotFound。
	Binding(ctx context.Context, buyer UserKey) (Binding, error)

	// Account 返回指定用户的账户；账户从未产生过资金变动时返回零值账户
	// 而不是 ErrNotFound——「零余额」与「账户不存在」对调用方没有区别。
	Account(ctx context.Context, key UserKey) (Account, error)

	// Commission 按 ID 返回佣金。
	Commission(ctx context.Context, id int64) (Commission, error)

	// CommissionsByOrder 返回某订单的全部佣金（含各级、含冲正记录）。
	CommissionsByOrder(ctx context.Context, key OrderKey) ([]Commission, error)

	// CommissionsByAgent 分页返回某分销员的佣金。
	CommissionsByAgent(ctx context.Context, key UserKey, q CommissionQuery) ([]Commission, error)

	// CommissionsByTenant 分页返回租户下的全部佣金，供对账与自检使用。
	//
	// 它存在的必要性：只靠「顺着流水反查佣金」来枚举佣金会漏掉
	// 「记了佣金但没动钱」这一类破坏形态——那正是最需要被发现的形态。
	CommissionsByTenant(ctx context.Context, tenantID int64, p Page) ([]Commission, error)

	// DueCommissions 返回已到可结算时点、仍处于待结算状态的佣金。
	//
	// 实现应当保证「没有到期项时是 O(1)」——Maintain 会被每分钟调用一次，
	// 绝大多次调用都应当立刻返回。
	DueCommissions(ctx context.Context, tenantID int64, dueAt time.Time, limit int) ([]Commission, error)

	// LedgerEntries 分页返回某用户的资金流水。
	LedgerEntries(ctx context.Context, key UserKey, q LedgerQuery) ([]LedgerEntry, error)

	// AccountsByTenant 分页返回租户下的全部账户，供对账使用。
	AccountsByTenant(ctx context.Context, tenantID int64, p AccountPage) ([]Account, error)

	// LedgerByTenant 分页返回租户下的全部流水，供对账使用。
	LedgerByTenant(ctx context.Context, tenantID int64, p Page) ([]LedgerEntry, error)

	// CountCommissionsByState 统计各状态的佣金笔数。
	CountCommissionsByState(ctx context.Context, tenantID int64) (map[CommissionState]int64, error)

	// Tenants 返回库中出现过的全部租户 ID，升序。
	//
	// Maintain 与 SelfCheck 需要遍历全部租户。库刻意不维护独立的租户表：
	// 租户属于接入方的领域模型，本库只把它当作数据分区键。
	Tenants(ctx context.Context) ([]int64, error)
}

// Writer 是写入端口。
//
// 每个写方法都必须自行完成并发保护：
//   - Put* 使用乐观锁（Version 不匹配返回 ErrConflict）。
//   - Append* 使用唯一约束（幂等键冲突返回 ErrDuplicate）。
type Writer interface {
	// PutAgent 保存分销员，Version 不匹配时返回 ErrConflict。
	// 返回写入后的实体（含递增后的 Version）。
	PutAgent(ctx context.Context, a Agent) (Agent, error)

	// PutBinding 保存绑定关系，Version 不匹配时返回 ErrConflict。
	PutBinding(ctx context.Context, b Binding) (Binding, error)

	// PutAccount 保存账户，Version 不匹配时返回 ErrConflict。
	PutAccount(ctx context.Context, a Account) (Account, error)

	// AppendCommission 追加一条佣金，幂等键冲突时返回 ErrDuplicate。
	// 返回写入后的实体（含分配的 ID）。
	AppendCommission(ctx context.Context, c Commission) (Commission, error)

	// SetCommissionAvailableAt 首次确定佣金的到账时间，并返回写入后的佣金。
	//
	// 语义是「先到先得」：若该佣金已经有非零 AvailableAt，本方法不做任何
	// 修改并原样返回——重复投递收货事件不应该能把到账时间往前刷。
	SetCommissionAvailableAt(ctx context.Context, id int64, at time.Time, expectedVersion int64) (Commission, error)

	// TransitionCommission 推进佣金状态并返回写入后的佣金。
	//
	// 非法迁移返回 ErrIllegalTransition；当前状态或版本与预期不符返回
	// ErrConflict（这两者要区分开：前者是调用方的逻辑错误，后者是可重试的
	// 并发冲突）。
	TransitionCommission(ctx context.Context, id int64, from, to CommissionState, expectedVersion int64) (Commission, error)

	// AppendLedger 追加一条资金流水并返回写入后的记录。
	// 流水不可修改、不可删除。
	AppendLedger(ctx context.Context, e LedgerEntry) (LedgerEntry, error)
}

// Tx 是一次事务内的读写句柄。
type Tx interface {
	Reader
	Writer
}

// Store 是持久化端口的入口。
//
// # 事务与重试契约
//
// Update 传入的函数**必须可以被安全地重复执行**：实现可以因为版本冲突而
// 重试整个函数。因此函数内不得有事务外的副作用（发消息、调外部接口、
// 修改闭包外的变量）。
type Store interface {
	// View 在只读事务中执行 fn。fn 内不得调用 Update/View（会死锁或返回错误）。
	View(ctx context.Context, fn func(ctx context.Context, r Reader) error) error

	// Update 在读写事务中执行 fn。fn 返回 nil 时提交，返回错误时回滚。
	Update(ctx context.Context, fn func(ctx context.Context, tx Tx) error) error

	// Close 释放底层资源。重复调用必须安全。
	Close() error
}

// SchemaChecker 是可选接口：存储层实现它即可参与 SelfCheck 的表结构检查。
//
// 内存存储不实现它是合理的（它没有表结构），而不是「留了个空壳」——
// 不实现即不参与，没有半成品状态。
type SchemaChecker interface {
	// CheckSchema 返回缺失或不匹配的表结构描述；全部就绪时返回 nil。
	CheckSchema(ctx context.Context) []string
}

// ReportedStore 是可选接口：存储层可以通过它报告自身类型，便于自检输出。
type ReportedStore interface {
	// StoreKind 返回存储类型标识，例如 "memory"、"mysql"。
	StoreKind() string
}
