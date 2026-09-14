package distledger

import (
	"fmt"
	"time"
)

// JoinType 描述分销员的加入方式。
//
// 注意：JoinPaid（付费加入）只是**状态标记**，不参与任何计酬计算。
// 库刻意不提供「付费成为分销员即可获得返佣资格」这类链路——
// 「入门费 + 拉人头」的组合正是传销判定的核心（见 ADR-011、PRD §15.2）。
type JoinType uint8

const (
	// JoinFree 表示无门槛加入。
	JoinFree JoinType = iota
	// JoinReviewed 表示需要审核后加入。
	JoinReviewed
	// JoinPaid 表示付费加入（仅状态标记）。
	JoinPaid
)

// Valid 报告加入方式是否在已知取值范围内。
func (j JoinType) Valid() bool { return j <= JoinPaid }

// String 返回加入方式的英文名。
func (j JoinType) String() string {
	switch j {
	case JoinFree:
		return "free"
	case JoinReviewed:
		return "reviewed"
	case JoinPaid:
		return "paid"
	default:
		return fmt.Sprintf("JoinType(%d)", uint8(j))
	}
}

// AgentStatus 描述分销员的生命周期状态。
type AgentStatus uint8

const (
	// AgentInactive 表示已登记但尚未生效，不参与分佣。
	AgentInactive AgentStatus = iota
	// AgentActive 表示正常，参与分佣。
	AgentActive
	// AgentDisabled 表示已禁用，不参与分佣，且其下级的归属链在此处截断。
	AgentDisabled
)

// Valid 报告状态是否在已知取值范围内。
func (s AgentStatus) Valid() bool { return s <= AgentDisabled }

// String 返回状态名。
func (s AgentStatus) String() string {
	switch s {
	case AgentInactive:
		return "inactive"
	case AgentActive:
		return "active"
	case AgentDisabled:
		return "disabled"
	default:
		return fmt.Sprintf("AgentStatus(%d)", uint8(s))
	}
}

// Agent 是一个分销员。
//
// 关系链用「父指针 + 深度」表达。之所以不在 v0.1 引入物化路径（path），
// 是因为深度被硬限制在 3 以内，向上遍历最多 3 步，路径带来的复杂度
// 换不来可测量的收益；等到需要「查整个团队」时再引入（见 ADR-006）。
type Agent struct {
	Key      UserKey
	ParentID int64
	// Depth 是层级深度，顶级为 1。等于「向上到根的最少步数 + 1」。
	Depth    int
	Status   AgentStatus
	JoinType JoinType
	// RateOverrideBP 非 nil 时覆盖全局费率，用于「金牌分销员」这类个体差异。
	RateOverrideBP *Rate
	CreatedAt      time.Time
	UpdatedAt      time.Time
	// Version 是乐观锁版本号，每次持久化递增。
	Version int64
}

// BindSource 描述绑定关系的来源，用于归因分析与反作弊。
type BindSource uint8

const (
	// SourceLink 表示通过推广链接绑定。
	SourceLink BindSource = iota + 1
	// SourceInviteCode 表示通过邀请码绑定。
	SourceInviteCode
	// SourceManual 表示后台人工绑定。
	SourceManual
	// SourceSalesperson 表示导购工号绑定。
	SourceSalesperson
)

// Valid 报告来源是否在已知取值范围内。
func (s BindSource) Valid() bool { return s >= SourceLink && s <= SourceSalesperson }

// String 返回来源名。
func (s BindSource) String() string {
	switch s {
	case SourceLink:
		return "link"
	case SourceInviteCode:
		return "invite_code"
	case SourceManual:
		return "manual"
	case SourceSalesperson:
		return "salesperson"
	default:
		return fmt.Sprintf("BindSource(%d)", uint8(s))
	}
}

// Binding 是「买家 → 分销员」的归因关系。
//
// 它刻意独立成表而不是冗余在订单上：绑定期限、抢客保护、换绑冷静期
// 都需要修改绑定关系本身，塞进订单表就改不了历史（见 ADR-006）。
type Binding struct {
	ID int64
	// Buyer 是被绑定的买家。
	Buyer UserKey
	// AgentUserID 是买家归属的分销员。
	AgentUserID int64
	Source      BindSource
	SourceRef   string
	BoundAt     time.Time
	// ExpireAt 为零值表示永久有效。
	ExpireAt time.Time
	// Active 为 false 表示该绑定已被替换或失效。
	Active bool
	// ReboundFrom 记录本绑定替换掉的旧绑定 ID，0 表示首次绑定。
	ReboundFrom int64
	Version     int64
}

// Effective 报告该绑定在 at 时刻是否生效。
func (b Binding) Effective(at time.Time) bool {
	if !b.Active {
		return false
	}
	if b.ExpireAt.IsZero() {
		return true
	}
	return at.Before(b.ExpireAt)
}

// CommissionState 描述佣金单的生命周期状态。
type CommissionState uint8

const (
	// CommissionPending 表示已入账、待在冻结期结束后结算。
	CommissionPending CommissionState = iota
	// CommissionSettled 表示已结算，计入可提现余额。
	CommissionSettled
	// CommissionWithdrawn 表示已被提现。
	CommissionWithdrawn
	// CommissionReversed 表示已被退款冲正。
	CommissionReversed
	// CommissionFrozen 表示被风控冻结，等待人工处置。
	CommissionFrozen
	// CommissionVoid 表示已作废，不产生任何资金影响。
	CommissionVoid
)

// Valid 报告状态是否在已知取值范围内。
func (s CommissionState) Valid() bool { return s <= CommissionVoid }

// String 返回状态名。
func (s CommissionState) String() string {
	switch s {
	case CommissionPending:
		return "pending"
	case CommissionSettled:
		return "settled"
	case CommissionWithdrawn:
		return "withdrawn"
	case CommissionReversed:
		return "reversed"
	case CommissionFrozen:
		return "frozen"
	case CommissionVoid:
		return "void"
	default:
		return fmt.Sprintf("CommissionState(%d)", uint8(s))
	}
}

// Terminal 报告该状态是否终态（不再发生迁移）。
func (s CommissionState) Terminal() bool {
	switch s {
	case CommissionWithdrawn, CommissionReversed, CommissionVoid:
		return true
	default:
		return false
	}
}

// Commission 是一笔佣金。
//
// # 可变性契约（重要）
//
// 财务字段在创建后**永不改变**：
//
//	Key / IdemKey / OrderItemID / BuyerUserID / AgentUserID / Layer /
//	BaseAmount / Rate / Amount / FreezeDays / RuleVersion / RuleSnapshot / AccruedAt
//
// 生命周期字段会随状态推进而变化，并且每次变化都必须携带 Version 做 CAS：
//
//	State / AvailableAt / SettledAt / Version
//
// 退款不修改 Amount，而是**追加一条负额记录**并把原记录置为 Reversed
// （见 ADR-004）。这条契约由 Store 端口强制：写接口只有
// AppendCommission / SetCommissionAvailableAt / TransitionCommission，
// 不存在通用的 Update。
type Commission struct {
	ID int64
	// Key 是被分佣的订单。
	Key OrderKey
	// IdemKey 是幂等键，在同一租户内唯一。
	IdemKey     string
	OrderItemID string
	BuyerUserID int64
	AgentUserID int64
	// Layer 是层级：1 表示直接推广人，2 表示其上级，依此类推。
	Layer int
	// BaseAmount 是计佣基数快照（已扣运费/平台券）。
	BaseAmount Money
	// Rate 是费率快照。
	Rate Rate
	// Amount 是佣金金额，正数表示入账（退款冲正记录为负数）。
	Amount Money
	State  CommissionState
	// FreezeDays 是冻结期快照。它保证「当时的规则决定当时的到账时间」，
	// 后续修改全局配置不会追溯影响已入账的佣金（见 ADR-005）。
	FreezeDays int
	// AvailableAt 是入库时间 + FreezeDays。零值表示订单尚未确认收货，
	// 因此到账时间还不确定。
	AvailableAt time.Time
	// RuleVersion 指向入账时生效的规则版本。
	RuleVersion int64
	// RuleSnapshot 是命中规则的 JSON 快照，用于日后解释「为什么是这个数」。
	RuleSnapshot string
	AccruedAt    time.Time
	SettledAt    time.Time
	Version      int64
}

// DueAt 报告该佣金在 at 时刻是否已到可结算时点。
//
// 尚未确认收货（AvailableAt 为零值）的佣金永远不到期。
func (c Commission) DueAt(at time.Time) bool {
	if c.AvailableAt.IsZero() {
		return false
	}
	return !at.Before(c.AvailableAt)
}

// Account 是一个分销员的资金账户。
//
// 四个桶刻意分开而不是只存一个「余额」：用户看到的必须是
// 「待结算 / 可提现 / 提现中 / 已提现」四件事，糊成一个数字必然产生纠纷。
type Account struct {
	Key UserKey
	// Frozen 是待结算金额（已入账、冻结期未满）。
	Frozen Money
	// Available 是可提现金额。
	Available Money
	// Withdrawing 是提现中金额（已申请、未打款）。
	Withdrawing Money
	// Withdrawn 是已提现金额。
	Withdrawn Money
	// TotalEarned 是历史累计入账金额，只增不减；冲正不影响它，
	// 冲正体现在流水与佣金单上。
	TotalEarned Money
	Version     int64
	UpdatedAt   time.Time
}

// Settleable 报告账户的四个资金桶是否全部非负。
//
// 这是不变量 I1 的一部分：任何时刻都不允许出现负的桶余额。
func (a Account) Settleable() bool {
	return a.Frozen >= 0 && a.Available >= 0 && a.Withdrawing >= 0 && a.Withdrawn >= 0
}

// LedgerBizType 描述一次资金变动的业务类型。
type LedgerBizType uint8

const (
	// LedgerAccrue 表示佣金入账（进入 Frozen）。
	LedgerAccrue LedgerBizType = iota + 1
	// LedgerSettle 表示冻结期满结算（Frozen → Available）。
	LedgerSettle
	// LedgerReverse 表示退款冲正。
	LedgerReverse
	// LedgerWithdrawHold 表示提现申请冻结（Available → Withdrawing）。
	LedgerWithdrawHold
	// LedgerWithdrawPaid 表示提现打款完成（Withdrawing → Withdrawn）。
	LedgerWithdrawPaid
	// LedgerWithdrawRefund 表示提现驳回退回（Withdrawing → Available）。
	LedgerWithdrawRefund
	// LedgerManualAdjust 表示人工调整。
	LedgerManualAdjust
)

// Valid 报告业务类型是否在已知取值范围内。
func (t LedgerBizType) Valid() bool { return t >= LedgerAccrue && t <= LedgerManualAdjust }

// String 返回业务类型名。
func (t LedgerBizType) String() string {
	switch t {
	case LedgerAccrue:
		return "accrue"
	case LedgerSettle:
		return "settle"
	case LedgerReverse:
		return "reverse"
	case LedgerWithdrawHold:
		return "withdraw_hold"
	case LedgerWithdrawPaid:
		return "withdraw_paid"
	case LedgerWithdrawRefund:
		return "withdraw_refund"
	case LedgerManualAdjust:
		return "manual_adjust"
	default:
		return fmt.Sprintf("LedgerBizType(%d)", uint8(t))
	}
}

// LedgerEntry 是资金流水的一条记录。
//
// 它是**复式记账**的落点：每一次账户余额变动都必须产生一条流水，
// 且流水同时记录「变动量」与「变动后余额」。后者让对账可以只看单条记录
// 就能判断当时的余额状态，而不必重放全部历史。
//
// 流水**只追加、永不修改、永不删除**（见 ADR-004）。
type LedgerEntry struct {
	ID      int64
	Key     UserKey
	BizType LedgerBizType
	// BizID 关联业务单据（佣金 ID 或提现单 ID）。
	BizID string

	DeltaFrozen      Money
	DeltaAvailable   Money
	DeltaWithdrawing Money
	DeltaWithdrawn   Money

	AfterFrozen      Money
	AfterAvailable   Money
	AfterWithdrawing Money
	AfterWithdrawn   Money

	Remark    string
	CreatedAt time.Time
}

// NetDelta 返回四个桶的变动总量，用于快速校验流水是否「凭空造钱」。
func (e LedgerEntry) NetDelta() Money {
	return e.DeltaFrozen + e.DeltaAvailable + e.DeltaWithdrawing + e.DeltaWithdrawn
}
