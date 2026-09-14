package distledger

// 本文件是内核中唯一「写死」的状态迁移定义。
//
// 为什么状态机不可插拔：状态机决定「钱能怎么流」。如果用户能替换状态机，
// 库就无法对任何不变量负责，也就失去了存在的意义（见 ADR-002）。
// 用户能改变的只有「钱的数值」——费率、层级数、门槛，那些在 rule.go 里。

// commissionTransitions 是佣金单的合法迁移表。
//
// 迁移表用「显式枚举」而不是 if/else 链，好处是可以被穷举测试：
// transition_test.go 会遍历全部 6×6 组合并核对本表的每一项。
var commissionTransitions = map[CommissionState][]CommissionState{
	// 待结算：可结算、可被退款冲正、可被风控冻结、可作废。
	CommissionPending: {
		CommissionSettled,
		CommissionReversed,
		CommissionFrozen,
		CommissionVoid,
	},
	// 已结算：可被提现、可被退款冲正。
	CommissionSettled: {
		CommissionWithdrawn,
		CommissionReversed,
	},
	// 风控冻结：申诉通过回到待结算，或申诉失败作废。
	CommissionFrozen: {
		CommissionPending,
		CommissionVoid,
	},
	// 以下为终态，不再迁移。
	//
	// CommissionWithdrawn 之所以是终态：钱已经出账，追回属于资金动作
	// （走 Reverse 策略与负余额策略），不属于状态迁移（v0.3 交付）。
	CommissionWithdrawn: nil,
	CommissionReversed:  nil,
	CommissionVoid:      nil,
}

// allCommissionStates 按声明顺序列出全部状态，用于穷举测试与遍历。
var allCommissionStates = []CommissionState{
	CommissionPending,
	CommissionSettled,
	CommissionWithdrawn,
	CommissionReversed,
	CommissionFrozen,
	CommissionVoid,
}

// AllCommissionStates 返回全部佣金状态的副本。
//
// 返回副本而不是内部切片：调用方若拿到内部切片并修改，会破坏状态机的
// 单一事实来源。
func AllCommissionStates() []CommissionState {
	out := make([]CommissionState, len(allCommissionStates))
	copy(out, allCommissionStates)
	return out
}

// CanTransitionCommission 报告 from → to 是否为合法迁移。
//
// 同状态「迁移」到自身永远为 false：状态推进必须是真实推进，
// 否则重复投递会被误认为迁移成功而重复记账。
func CanTransitionCommission(from, to CommissionState) bool {
	if !from.Valid() || !to.Valid() {
		return false
	}
	for _, allowed := range commissionTransitions[from] {
		if allowed == to {
			return true
		}
	}
	return false
}

// ValidateCommissionTransition 在非法迁移时返回 *TransitionError。
func ValidateCommissionTransition(from, to CommissionState) error {
	if !from.Valid() {
		return fieldErrf("state", "unknown commission state %d", uint8(from))
	}
	if !to.Valid() {
		return fieldErrf("state", "unknown commission state %d", uint8(to))
	}
	if !CanTransitionCommission(from, to) {
		return &TransitionError{Kind: "commission", From: from.String(), To: to.String()}
	}
	return nil
}
