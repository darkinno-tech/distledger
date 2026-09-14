// Package safemath provides the integer safety primitives needed by amount
// arithmetic.
//
// This package is the only place in the library allowed to perform
// multiplications that may overflow, so it is deliberately small, dumb, and easy
// to verify by exhaustive testing. Every function's contract takes "return
// ok=false instead of panicking, and instead of silently wrapping around" as its
// first principle.
//
// Design constraints (see docs/design-decisions.md ADR-003):
//   - Use int64/uint64 integers throughout; no floating point in amount
//     arithmetic.
//   - Overflow must be detected and reported explicitly; silent wrap-around is
//     never allowed.
package safemath

import "math/bits"

// MulDiv computes a*b/d, where a and b are non-negative integers and d is a
// positive integer.
//
// Semantics:
//   - If roundUp is true, round up (ceiling).
//   - Otherwise, if halfUp is true, round half away from zero.
//   - Otherwise, truncate toward zero.
//
// A returned ok=false means the result cannot be represented as a uint64
// (overflow); the caller must abort the computation and return an error rather
// than use the returned value.
//
// roundUp takes precedence over halfUp: with both true, the result is the
// ceiling.
func MulDiv(a, b, d uint64, roundUp, halfUp bool) (uint64, bool) {
	if d == 0 {
		return 0, false
	}
	hi, lo := bits.Mul64(a, b)
	if hi >= d {
		// The quotient would exceed the uint64 range.
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
	if halfUp && r >= d-r { // Equivalent to 2r >= d, avoiding an overflow in 2r.
		if q == ^uint64(0) {
			return 0, false
		}
		return q + 1, true
	}
	return q, true
}

// Add reports whether a+b overflows.
func Add(a, b int64) (int64, bool) {
	s := a + b
	if (b > 0 && s < a) || (b < 0 && s > a) {
		return 0, false
	}
	return s, true
}

// Sub reports whether a-b overflows.
//
// The test: the sign of the result must change in the direction implied by -b.
// That test holds at every boundary, including a and b both equal to MinInt64
// (the result is 0, which is a legal operation). MinInt64 is deliberately not
// special-cased: any special case that "conservatively reports one extra
// overflow" would turn a legal operation into a failure, and in a money context
// a failure means a business outage.
func Sub(a, b int64) (int64, bool) {
	d := a - b
	if (b < 0 && d < a) || (b > 0 && d > a) {
		return 0, false
	}
	return d, true
}

// Neg reports whether negation overflows (negating minInt64 overflows).
func Neg(a int64) (int64, bool) {
	if a == minInt64 {
		return 0, false
	}
	return -a, true
}

const minInt64 = -1 << 63

// MaxInt64 is the maximum value of int64.
const MaxInt64 = 1<<63 - 1

// MinInt64 is the minimum value of int64.
const MinInt64 = minInt64
