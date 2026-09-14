package distledger

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"strings"
)

// MaxLevels 是分销层级的硬上限。
//
// 它不是可配置项，而是刻意写死的合规兜底：三级以上分销在中国法律下
// 即属高风险区（《禁止传销条例》《刑法》第 224 条之一）。
// 允许运营把它配成 4 就是允许一次配置事故变成法律风险（见 ADR-011）。
const MaxLevels = 3

// MaxFreezeDays 是冻结期的上限（10 年）。
//
// 上限存在的意义是防止误把「秒」当成「天」填进来，导致佣金永久不到账
// 却又不报错——这类静默错误比直接报错难排查得多。
const MaxFreezeDays = 3650

// MaxBindExpireDays 是绑定有效期的上限（100 年）。
const MaxBindExpireDays = 36500

// Rules 是分销的**业务规则**。
//
// 它只回答「算多少、分几级、什么时候到账」，绝不参与「钱怎么流」的决策。
// 这是 ADR-002 定义的切分线：Rules 里的任何字段都无法改变状态机的形状。
type Rules struct {
	// Levels 是分佣层级数，取值 [1, MaxLevels]。默认 2。
	Levels int

	// RateBP 是每一级的费率，长度必须等于 Levels。
	//
	// 下标 0 对应第 1 级（订单归属的分销员本身）。默认全 0，
	// 即「跑通链路但不发钱」——这是 ADR-010 的保守默认。
	RateBP []Rate

	// Rounding 是佣金计算时的舍入方式。默认 RoundDown。
	Rounding Rounding

	// FreezeDays 是「确认收货后 N 天可提现」。默认 7。
	//
	// 它会在佣金入账时被**快照**进佣金单，因此修改本字段不会追溯
	// 影响已入账的佣金（见 ADR-005）。
	FreezeDays int

	// MaxAllocatableBP 是本笔订单可分佣总额占计佣基数的比例上限，默认 100%。
	//
	// 它是平台侧的最后一道闸门：即使 RateBP 被误配成 {5000, 5000}（合计 100%），
	// 引擎仍会保证分出去的佣金不超过基数。校验期也会拦下非法的 RateBP 组合，
	// 本字段是运行期的双保险（见 PRD 不变量 I2）。
	MaxAllocatableBP Rate

	// BindExpireDays 是买家绑定关系的有效期（天）。0 表示永久有效。默认 0。
	BindExpireDays int
}

// DefaultRules 返回保守的默认规则。
//
// 费率为 0、层级为 2、冻结期 7 天、人工审核式结算。首次接入者必须显式
// 配置费率才会真的发出佣金——这一步是刻意的确认动作（见 ADR-010）。
func DefaultRules() Rules {
	return Rules{
		Levels:           2,
		RateBP:           []Rate{0, 0},
		Rounding:         RoundDown,
		FreezeDays:       7,
		MaxAllocatableBP: MaxRateBP,
		BindExpireDays:   0,
	}
}

// Normalize 补齐零值字段，使 Rules 可以直接被部分初始化。
//
// 规则：只对「零值无意义」的字段做补齐。Levels 为零是非法而非默认，
// 因此必须由调用方显式指定或整体使用 DefaultRules()。
func (r Rules) Normalize() Rules {
	// Rounding 的零值就是 RoundDown，因此「未设置」与「显式要截断」无法区分，
	// 也无需区分——两者结果一致，且都是最保守的选择。
	if r.MaxAllocatableBP == 0 {
		r.MaxAllocatableBP = MaxRateBP
	}
	if r.Levels == 0 {
		r.Levels = 2
	}
	// 只在费率整体缺省时补齐。长度非零但与 Levels 不符属于配置错误，
	// 必须报错而不是静默补零——静默补零会把「少配了一级」变成
	// 「那一级不发钱」，是一个不会报警的错误。
	if len(r.RateBP) == 0 {
		r.RateBP = make([]Rate, r.Levels)
	}
	return r
}

// Validate 校验规则是否自洽。
//
// 校验失败的规则一律拒绝启动，而不是「尽力而为」——带着错误规则跑起来
// 的结果是发出错误的钱，而那不可撤销。
func (r Rules) Validate() error {
	if r.Levels < 1 || r.Levels > MaxLevels {
		return fieldErrf("rules.levels", "must be in [1, %d], got %d", MaxLevels, r.Levels)
	}
	if len(r.RateBP) != r.Levels {
		return fieldErrf("rules.rate_bp",
			"length must equal levels (%d), got %d", r.Levels, len(r.RateBP))
	}
	if !r.Rounding.Valid() {
		return fieldErrf("rules.rounding", "unknown mode %d", r.Rounding)
	}
	if r.FreezeDays < 0 || r.FreezeDays > MaxFreezeDays {
		return fieldErrf("rules.freeze_days", "must be in [0, %d], got %d", MaxFreezeDays, r.FreezeDays)
	}
	if r.BindExpireDays < 0 || r.BindExpireDays > MaxBindExpireDays {
		return fieldErrf("rules.bind_expire_days",
			"must be in [0, %d], got %d", MaxBindExpireDays, r.BindExpireDays)
	}
	if !r.MaxAllocatableBP.Valid() {
		return fieldErrf("rules.max_allocatable_bp",
			"must be in [0, %d], got %d", MaxRateBP, r.MaxAllocatableBP)
	}
	if r.MaxAllocatableBP == 0 {
		return fieldErr("rules.max_allocatable_bp", "must be > 0")
	}

	var sum int64
	for i, rate := range r.RateBP {
		if !rate.Valid() {
			return fieldErrf("rules.rate_bp", "level %d out of range [0, %d]: %d",
				i+1, MaxRateBP, rate)
		}
		sum += int64(rate)
	}
	if sum > int64(r.MaxAllocatableBP) {
		return fieldErrf("rules.rate_bp",
			"sum of rates (%d bp) exceeds max allocatable (%d bp)", sum, r.MaxAllocatableBP)
	}
	return nil
}

// Describe 返回规则的紧凑描述，用于写进佣金单的 RuleSnapshot。
//
// 刻意不含时间戳与机器信息：同一份规则必须产生完全相同的快照字符串，
// 这样「哪些佣金用了同一版规则」可以直接用字符串比较来分组统计。
func (r Rules) Describe() string {
	var b strings.Builder
	fmt.Fprintf(&b, `{"levels":%d,"rate_bp":[`, r.Levels)
	for i, rate := range r.RateBP {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "%d", rate)
	}
	fmt.Fprintf(&b, `],"rounding":%q,"freeze_days":%d,"max_allocatable_bp":%d}`,
		r.Rounding.String(), r.FreezeDays, r.MaxAllocatableBP)
	return b.String()
}

// Version 返回规则集的**内容指纹**。
//
// 它是一个内容寻址的版本号：同一份规则永远得到同一个值，规则任一处变化
// 都会得到不同的值。佣金单上记录它，就能在三个月后回答
// 「这笔佣金是按哪一版规则算出来的」。
//
// 这里刻意不用「自增版本号 + 版本表」：那需要额外的持久化与运维流程，
// 而内容指纹在 v0.1 就能提供同样的可解释性。
func (r Rules) Version() int64 {
	sum := sha256.Sum256([]byte(r.Describe()))
	// 右移一位保证结果恒为非负，便于在日志与 SQL 里阅读。
	return int64(binary.BigEndian.Uint64(sum[:8]) >> 1)
}

// RateInput 是费率解析的输入。
type RateInput struct {
	Order OrderKey
	// Layer 从 1 开始。
	Layer int
	Agent Agent
	Rules Rules
}

// RateResolver 决定「第 layer 级应当使用什么费率」。
//
// 这是规则层的核心扩展点。默认实现按「个体覆盖 > 全局规则」的顺序解析，
// 覆盖了绝大多数场景；只有形如「按商品类目浮动」「按等级阶梯」这类
// 结构化配置表达不了的诉求，才需要自己实现它（见 ADR-002）。
type RateResolver interface {
	ResolveRate(ctx context.Context, in RateInput) (Rate, error)
}

// EligibilityChecker 判定某个分销员是否有资格就这笔订单获得佣金。
//
// 默认实现要求分销员处于 AgentActive 状态。实现必须返回
// (false, reason) 而不是 error 来表达「不符合资格」——那是业务结论，
// 不是程序错误。
type EligibilityChecker interface {
	Eligible(ctx context.Context, agent Agent, order OrderKey) (bool, string)
}

// defaultRateResolver 是默认费率解析器。
type defaultRateResolver struct{}

func (defaultRateResolver) ResolveRate(_ context.Context, in RateInput) (Rate, error) {
	if in.Agent.RateOverrideBP != nil {
		return *in.Agent.RateOverrideBP, nil
	}
	idx := in.Layer - 1
	if idx < 0 || idx >= len(in.Rules.RateBP) {
		return 0, fieldErrf("layer", "no rate configured for layer %d (levels=%d)",
			in.Layer, in.Rules.Levels)
	}
	return in.Rules.RateBP[idx], nil
}

// defaultEligibility 是默认资格判定器：只有启用状态的分销员参与分佣。
type defaultEligibility struct{}

func (defaultEligibility) Eligible(_ context.Context, agent Agent, _ OrderKey) (bool, string) {
	if agent.Status != AgentActive {
		return false, "agent status is " + agent.Status.String()
	}
	return true, ""
}
