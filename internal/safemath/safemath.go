// Package safemath 提供金额运算所需的整数安全原语。
//
// 本包是整个库中唯一允许做「乘法可能溢出」运算的地方，因此它刻意做得很小、
// 很笨、很容易被穷举验证。所有函数的契约都以「返回 ok=false 而不是 panic、
// 不是静默回绕」为第一原则。
//
// 设计约束（见 docs/design-decisions.md ADR-003）：
//   - 全程使用 int64/uint64 整数，禁止任何浮点参与金额运算。
//   - 溢出必须被显式检测并上报，绝不允许静默 wrap-around。
package safemath

import "math/bits"

// MulDiv 计算 a*b/d，其中 a、b 为非负整数，d 为正整数。
//
// 语义：
//   - 若 roundUp 为 true，则向上取整（ceiling）。
//   - 否则若 halfUp 为 true，则四舍五入（half away from zero）。
//   - 否则向零截断。
//
// 返回 ok=false 表示结果无法用 uint64 表示（溢出），调用方必须据此中止运算
// 并返回错误，而不是使用返回值。
//
// roundUp 优先于 halfUp：两者同时为 true 时按 ceiling 处理。
func MulDiv(a, b, d uint64, roundUp, halfUp bool) (uint64, bool) {
	if d == 0 {
		return 0, false
	}
	hi, lo := bits.Mul64(a, b)
	if hi >= d {
		// 商将超出 uint64 表示范围。
		return 0, false
	}
	q, r := bits.Div64(hi, lo, d)
	if r == 0 {
		return q, true
	}
	if roundUp {
		if q == ^uint64(0) {
			return 0, false
		}
		return q + 1, true
	}
	if halfUp && r >= d-r { // 等价于 2r >= d，且避免 2r 溢出
		if q == ^uint64(0) {
			return 0, false
		}
		return q + 1, true
	}
	return q, true
}

// Add 检测 a+b 是否溢出。
func Add(a, b int64) (int64, bool) {
	s := a + b
	if (b > 0 && s < a) || (b < 0 && s > a) {
		return 0, false
	}
	return s, true
}

// Sub 检测 a-b 是否溢出。
//
// 判据：结果的符号变化方向必须与 -b 一致。这个判据在所有边界上都成立，
// 包括 a 与 b 同为 MinInt64 的情形（结果为 0，是合法运算）。
// 这里刻意不对 MinInt64 做特判：任何「保守地多报一次溢出」的特判
// 都会把合法运算判成失败，而在金额场景里失败就等于业务中断。
func Sub(a, b int64) (int64, bool) {
	d := a - b
	if (b < 0 && d < a) || (b > 0 && d > a) {
		return 0, false
	}
	return d, true
}

// Neg 检测取负是否溢出（minInt64 取负会溢出）。
func Neg(a int64) (int64, bool) {
	if a == minInt64 {
		return 0, false
	}
	return -a, true
}

const minInt64 = -1 << 63

// MaxInt64 是 int64 的最大值。
const MaxInt64 = 1<<63 - 1

// MinInt64 是 int64 的最小值。
const MinInt64 = minInt64
