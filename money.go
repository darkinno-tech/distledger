package distledger

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/im10furry/distledger/internal/safemath"
)

// RateScale 是费率的固定分母：费率以「万分比」(basis point) 乘 10000 表示。
//
//	5%    -> 500
//	0.01% -> 1
//	100%  -> 10000
const RateScale int64 = 10000

// Rate 是以万分比表示的费率。
//
// 使用定点整数而非浮点，是为了让「5%」这类值在二进制中可精确表示，
// 从而保证同一笔订单在任何机器上算出完全相同的佣金（见 ADR-003）。
type Rate int32

// MaxRateBP 是单个费率的逻辑上限（100%）。超过它的费率一律视为配置错误。
const MaxRateBP Rate = Rate(RateScale)

// Valid 报告费率是否落在 [0, 100%] 区间内。
func (r Rate) Valid() bool { return r >= 0 && r <= MaxRateBP }

// String 返回人类可读的百分比形式，例如 "5%"、"0.01%"。
//
// 万分比与百分比的关系是 bp = percent × 100，因此整数部分是 v/100、
// 小数部分（两位）是 v%100。
func (r Rate) String() string {
	neg := ""
	v := int64(r)
	if v < 0 {
		neg = "-"
		v = -v
	}
	whole := v / 100
	frac := v % 100
	if frac == 0 {
		return neg + strconv.FormatInt(whole, 10) + "%"
	}
	// 左侧补零到两位，并去掉末尾多余的零：1 -> "0.01%"，50 -> "0.5%"。
	s := strconv.FormatInt(frac+100, 10)[1:]
	s = strings.TrimRight(s, "0")
	return neg + strconv.FormatInt(whole, 10) + "." + s + "%"
}

// Rounding 指定金额计算时的舍入方式。
//
// 默认值为 RoundDown（截断）：与「默认不发钱」同源的保守原则——
// 宁可少发一分，不可因为舍入而超发（见 ADR-010）。
type Rounding uint8

const (
	// RoundDown 向零截断。
	RoundDown Rounding = iota
	// RoundHalfUp 四舍五入（远离零方向）。
	RoundHalfUp
	// RoundUp 向上取整（远离零方向）。
	RoundUp
)

// Valid 报告舍入模式是否在已知取值范围内。
func (r Rounding) Valid() bool { return r <= RoundUp }

// String 返回舍入模式名称。
func (r Rounding) String() string {
	switch r {
	case RoundDown:
		return "down"
	case RoundHalfUp:
		return "half-up"
	case RoundUp:
		return "up"
	default:
		return "unknown(" + strconv.Itoa(int(r)) + ")"
	}
}

// Money 表示以最小货币单位（人民币为「分」）计的金额。
//
// 刻意定义成独立类型而不是直接用 int64：编译器会阻止把 Money 与其它
// 数量（件数、积分数、字节数）相加，这类混用在金额系统里是真实事故来源。
//
// 一切运算都是整数运算，并且**溢出会返回 ErrOverflow 而不是回绕**。
type Money int64

// Zero 是零金额。
const Zero Money = 0

// Add 返回 m+o；溢出时返回 ErrOverflow。
func (m Money) Add(o Money) (Money, error) {
	s, ok := safemath.Add(int64(m), int64(o))
	if !ok {
		return 0, fmt.Errorf("%w: %d + %d", ErrOverflow, m, o)
	}
	return Money(s), nil
}

// Sub 返回 m-o；溢出时返回 ErrOverflow。
func (m Money) Sub(o Money) (Money, error) {
	d, ok := safemath.Sub(int64(m), int64(o))
	if !ok {
		return 0, fmt.Errorf("%w: %d - %d", ErrOverflow, m, o)
	}
	return Money(d), nil
}

// Neg 返回 -m；对 math.MinInt64 返回 ErrOverflow。
func (m Money) Neg() (Money, error) {
	n, ok := safemath.Neg(int64(m))
	if !ok {
		return 0, fmt.Errorf("%w: -(%d)", ErrOverflow, m)
	}
	return Money(n), nil
}

// IsZero 报告金额是否为零。
func (m Money) IsZero() bool { return m == 0 }

// IsPositive 报告金额是否为正。
func (m Money) IsPositive() bool { return m > 0 }

// IsNegative 报告金额是否为负。
func (m Money) IsNegative() bool { return m < 0 }

// Sign 返回 -1、0 或 1。
func (m Money) Sign() int {
	switch {
	case m < 0:
		return -1
	case m > 0:
		return 1
	default:
		return 0
	}
}

// Cmp 比较两个金额：m<o 返回 -1，m==o 返回 0，m>o 返回 1。
func (m Money) Cmp(o Money) int {
	switch {
	case m < o:
		return -1
	case m > o:
		return 1
	default:
		return 0
	}
}

// Apply 按费率 r 与舍入方式 mode 计算 m*r。
//
// 结果与 m 同号；r 非法或运算溢出时返回错误。
func (m Money) Apply(r Rate, mode Rounding) (Money, error) {
	if !r.Valid() {
		return 0, fieldErrf("rate", "out of range [0, %d]: %d", MaxRateBP, r)
	}
	if !mode.Valid() {
		return 0, fieldErrf("rounding", "unknown mode: %d", mode)
	}
	if m == 0 || r == 0 {
		return Zero, nil
	}

	neg := m < 0
	abs := absUint64(int64(m))

	q, ok := safemath.MulDiv(abs, uint64(r), uint64(RateScale), mode == RoundUp, mode == RoundHalfUp)
	if !ok || q > math.MaxInt64 {
		return 0, fmt.Errorf("%w: %d x %s", ErrOverflow, m, r)
	}
	res := int64(q)
	if neg {
		res = -res
	}
	return Money(res), nil
}

// absUint64 返回 |v| 的 uint64 形式，对 math.MinInt64 也安全。
func absUint64(v int64) uint64 {
	if v < 0 {
		return uint64(-(v + 1)) + 1
	}
	return uint64(v)
}

// String 返回以「元」为单位的十进制字符串，固定两位小数，例如 "199.00"。
//
// 采用字符串而非浮点序列化，是为了避免 JSON 数字在 JS 侧丢失精度
// （见 MarshalJSON）。
func (m Money) String() string {
	v := int64(m)
	sign := ""
	if v < 0 {
		sign = "-"
		v = -v
	}
	whole := v / 100
	frac := v % 100
	return sign + strconv.FormatInt(whole, 10) + "." + strconv.FormatInt(frac+100, 10)[1:]
}

// MarshalJSON 把金额序列化为十进制字符串（如 "199.00"）。
//
// 不用 JSON number 是刻意的：JavaScript 的 Number 只有 53 位有效精度，
// 大额金额经 JSON number 往返会静默丢分。
func (m Money) MarshalJSON() ([]byte, error) {
	return []byte(`"` + m.String() + `"`), nil
}

// UnmarshalJSON 解析金额字段。
//
// 接受两种形式：
//   - 十进制字符串："199.00"、"199"、"-1.23"
//   - 整数 number：199
//
// 拒绝小数 number（如 1.23）：它意味着调用方在用浮点表达金额，
// 这正是本库要杜绝的精度风险。请改用字符串 "1.23"。
func (m *Money) UnmarshalJSON(b []byte) error {
	s := string(b)
	if s == "null" {
		*m = 0
		return nil
	}
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		v, err := ParseMoney(s[1 : len(s)-1])
		if err != nil {
			return err
		}
		*m = v
		return nil
	}
	if strings.ContainsAny(s, ".eE") {
		return fieldErrf("money", "refusing to parse non-integer JSON number %s; "+
			"pass a decimal string like \"1.23\" instead", s)
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return fieldErrf("money", "cannot parse %s as integer minor units", s)
	}
	*m = Money(v)
	return nil
}

// ParseMoney 解析「元」为单位的十进制字符串，例如 "199"、"199.5"、"-1.23"。
//
// 超过两位小数一律报错而不是静默截断：静默丢钱比报错危险得多。
func ParseMoney(s string) (Money, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fieldErr("money", "empty string")
	}
	sign := int64(1)
	switch s[0] {
	case '-':
		sign = -1
		s = s[1:]
	case '+':
		s = s[1:]
	}
	if s == "" {
		return 0, fieldErrf("money", "no digits")
	}

	intPart, fracPart, hasFrac := strings.Cut(s, ".")
	if intPart == "" {
		intPart = "0"
	}
	if !isASCIIDigits(intPart) {
		return 0, fieldErrf("money", "invalid integer part %q", intPart)
	}
	if hasFrac {
		if fracPart == "" || !isASCIIDigits(fracPart) {
			return 0, fieldErrf("money", "invalid fraction part %q", fracPart)
		}
		if len(fracPart) > 2 {
			return 0, fieldErrf("money",
				"more than 2 decimal places in %q would lose money; round it explicitly first", s)
		}
	}
	for len(fracPart) < 2 {
		fracPart += "0"
	}

	whole, err := strconv.ParseInt(intPart, 10, 64)
	if err != nil {
		return 0, fieldErrf("money", "integer part out of range: %q", intPart)
	}
	if whole > math.MaxInt64/100 {
		return 0, fmt.Errorf("%w: %q", ErrOverflow, s)
	}
	minor := whole * 100
	if hasFrac {
		f, _ := strconv.ParseInt(fracPart, 10, 64)
		minor += f
	}
	if sign < 0 {
		minor = -minor
	}
	return Money(minor), nil
}

func isASCIIDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
