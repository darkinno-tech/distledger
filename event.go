package distledger

import (
	"time"
)

// 本文件定义库与外部系统之间的**事件契约**。
//
// 刻意的设计选择（见 ADR-012）：
//   - 全部是普通 Go struct，不用 protobuf、不用 codegen、不用反射注册。
//   - 每个事件都是**幂等**的：重复投递不会产生第二笔账。接入方可以把它
//     直接挂在 at-least-once 的消息队列上，不需要自己设计去重。
//   - 事件里没有任何「库需要回调你」的字段：库不接管接入方的生命周期。

// OrderItem 是一笔订单中的一个明细行。
//
// 只有在需要按 SKU 分别计算佣金时才需要提供 Items；不提供时按订单整体计佣。
type OrderItem struct {
	// ItemID 是明细行标识，在同一订单内必须唯一。
	ItemID string
	// Amount 是该明细行参与计佣的金额（已扣该行分摊的优惠与运费）。
	Amount Money
}

// OrderPaidEvent 表示一笔订单已支付成功。
//
// 这是分佣的触发点。库会在收到本事件时完成：归因 → 计算各级佣金 → 入账到
// 「待结算」桶 → 写资金流水。整个过程在**一个事务**内完成。
type OrderPaidEvent struct {
	TenantID    int64
	OrderID     string
	BuyerUserID int64
	// PaidAmount 是实付金额（已扣运费与平台承担券），也是分佣的总额上限。
	PaidAmount Money
	// Items 可选。提供后按明细分别计佣；不提供则按订单整体计佣。
	Items []OrderItem
	// PaidAt 是支付时间，**必填**，用于判定绑定关系在支付时刻是否已经存在
	// 且有效。
	//
	// 它是必填而不是「留空取当前时间」，这是一个安全要求：如果用当前时间
	// 代替，那么一次发生在绑定建立之前的重放（首次投递时没有绑定、之后
	// 有人补上绑定再重放）就会被追认为有效归因——相当于给「事后抢单」
	// 留了一道门。宁可要求调用方多传一个字段。
	PaidAt time.Time
	// IdemKey 可选。留空时由库派生；提供时会被映射到独立命名空间，
	// 无法冒充库派生的键（见 idem.go）。
	IdemKey string
}

func (e OrderPaidEvent) orderKey() OrderKey {
	return OrderKey{TenantID: e.TenantID, OrderID: e.OrderID}
}

func (e OrderPaidEvent) validate() error {
	if err := e.orderKey().Validate(); err != nil {
		return err
	}
	if e.IdemKey != "" {
		if err := validateIdemKeyOverride(e.IdemKey); err != nil {
			return err
		}
	}
	if e.BuyerUserID <= 0 {
		return fieldErrf("buyer_user_id", "must be > 0, got %d", e.BuyerUserID)
	}
	if e.PaidAmount < 0 {
		return fieldErrf("paid_amount", "must be >= 0, got %s", e.PaidAmount)
	}
	if e.PaidAmount == 0 {
		return fieldErr("paid_amount", "must be > 0; zero-amount orders cannot be accrued")
	}
	if e.PaidAt.IsZero() {
		return fieldErr("paid_at",
			"must be set; the library refuses to substitute the current time "+
				"because that would let a binding created after payment capture the order")
	}
	seen := make(map[string]struct{}, len(e.Items))
	var sum Money
	for i, it := range e.Items {
		field := "items[" + i64s(int64(i)) + "].item_id"
		if it.ItemID == "" {
			return fieldErr(field, "must not be empty")
		}
		if len(it.ItemID) > MaxOrderItemIDLen {
			return fieldErrf(field, "must be at most %d bytes, got %d", MaxOrderItemIDLen, len(it.ItemID))
		}
		if it.Amount < 0 {
			return fieldErrf("items["+i64s(int64(i))+"].amount", "must be >= 0, got %s", it.Amount)
		}
		if _, dup := seen[it.ItemID]; dup {
			return fieldErrf(field, "duplicate item id %q", it.ItemID)
		}
		seen[it.ItemID] = struct{}{}
		next, err := sum.Add(it.Amount)
		if err != nil {
			return err
		}
		sum = next
	}
	// 关键的安全校验：明细金额之和不得超过实付金额。
	//
	// 否则调用方（或伪造事件的攻击者）只要塞入一个金额虚高的明细，
	// 就能让平台按高于实收的基数支付佣金——这是直接的资损入口。
	if sum > e.PaidAmount {
		return fieldErrf("items",
			"sum of item amounts (%s) exceeds paid amount (%s)", sum, e.PaidAmount)
	}
	return nil
}

// OrderReceivedEvent 表示买家已确认收货。
//
// 它把待结算佣金推进到「冻结期开始计时」的状态：
// AvailableAt = ReceivedAt + 入账时快照的 FreezeDays。
//
// 本事件是幂等的，且**先到先得**：同一次收货重复投递不会改变已确定的
// AvailableAt，避免有人用重复投递把到账时间往前刷。
type OrderReceivedEvent struct {
	TenantID   int64
	OrderID    string
	ReceivedAt time.Time
}

func (e OrderReceivedEvent) orderKey() OrderKey {
	return OrderKey{TenantID: e.TenantID, OrderID: e.OrderID}
}

func (e OrderReceivedEvent) validate() error {
	return e.orderKey().Validate()
}

// SkipCode 说明某一层级为什么没有产生佣金。
//
// 它存在的意义是「可解释性」：接入方排查「为什么我的分销员没拿到钱」时，
// 必须能从返回值里直接看到原因，而不是去猜（见 PRD 铁律 R6）。
type SkipCode uint8

const (
	// SkipZeroAmount 表示按费率算出的佣金为 0（通常是费率未配置）。
	SkipZeroAmount SkipCode = iota + 1
	// SkipIneligible 表示该分销员不具备获得佣金的资格（未激活/被禁用）。
	SkipIneligible
	// SkipNoParent 表示关系链在此处断开，没有更上一级了。
	SkipNoParent
	// SkipCapExhausted 表示订单可分佣总额已用尽。
	SkipCapExhausted
	// SkipAgentMissing 表示关系链中引用的上级分销员不存在。
	SkipAgentMissing
)

// String 返回跳过原因的稳定标识。
func (c SkipCode) String() string {
	switch c {
	case SkipZeroAmount:
		return "zero_amount"
	case SkipIneligible:
		return "ineligible"
	case SkipNoParent:
		return "no_parent"
	case SkipCapExhausted:
		return "cap_exhausted"
	case SkipAgentMissing:
		return "agent_missing"
	default:
		return "unknown"
	}
}

// SkipReason 记录某一层级被跳过的具体原因。
type SkipReason struct {
	// Layer 从 1 开始。
	Layer       int
	AgentUserID int64
	Code        SkipCode
	Detail      string
}

// AccrueResult 是 OnOrderPaid 的结果。
type AccrueResult struct {
	OrderKey OrderKey
	// Attributed 报告是否找到了有效的归因对象。
	//
	// 为 false 时**不是错误**：没有推广归属的订单只是不分佣，
	// 不应该让订单系统收到一个失败（见 PRD 兜底设计）。
	Attributed bool
	// AgentUserID 是订单归属的分销员；未归因时为 0。
	AgentUserID int64
	// Commissions 是本次**新产生**的佣金。重复投递时为空。
	Commissions []Commission
	// Replayed 报告本次调用是否命中了幂等重放（即此前已入账）。
	Replayed bool
	// Skipped 解释各级为什么没有产生佣金。
	Skipped []SkipReason
}

// AccruedAmount 返回本次新产生佣金的合计。
func (r AccrueResult) AccruedAmount() Money {
	var sum Money
	for _, c := range r.Commissions {
		sum += c.Amount
	}
	return sum
}

// ReceiveResult 是 OnOrderReceived 的结果。
type ReceiveResult struct {
	OrderKey OrderKey
	// Updated 是被本次调用首次确定到账时间的佣金笔数。重复投递时为 0。
	Updated int
}

// MaintainResult 是 Maintain 的结果。
type MaintainResult struct {
	// SettledCount 是被结算的佣金笔数。
	SettledCount int
	// SettledAmount 是结算金额合计。
	SettledAmount Money
}

// BindAgentRequest 是登记一个分销员的请求。
type BindAgentRequest struct {
	TenantID int64
	UserID   int64
	// ParentID 是直接上级的用户 ID；0 表示顶级分销员。
	ParentID int64
	JoinType JoinType
	// Status 留空（零值）时按 AgentInactive 处理，需要显式激活。
	// 接入方若采用「无门槛」模式，可传 AgentActive。
	Status AgentStatus
	// RateOverrideBP 可选，覆盖全局费率。
	RateOverrideBP *Rate
	// Idempotent 为 true 时，重复登记同一个 (UserID, ParentID) 不报错。
	// 默认（false）行为同样是幂等的，此字段保留给未来的严格模式。
	Idempotent bool
}

// BindBuyerRequest 是建立「买家 → 分销员」归因关系的请求。
type BindBuyerRequest struct {
	TenantID    int64
	BuyerUserID int64
	AgentUserID int64
	Source      BindSource
	SourceRef   string
	// BoundAt 为零值时取 Clock 当前时间。
	BoundAt time.Time
}
