package safemath

import (
	"math"
	"testing"
)

func TestMulDivExact(t *testing.T) {
	tests := []struct {
		a, b, d uint64
		roundUp bool
		halfUp  bool
		want    uint64
		wantOK  bool
		name    string
	}{
		{19900, 500, 10000, false, false, 995, true, "exact division"},
		{100, 1, 10000, false, false, 0, true, "truncates to zero"},
		{100, 1, 10000, true, false, 1, true, "ceiling rounds up"},
		{100, 1, 10000, false, true, 0, true, "half-up rounds down below half"},
		{150, 1, 10000, false, true, 0, true, "half-up at 0.015 rounds down"},
		{5000, 1, 10000, false, true, 1, true, "half-up at exactly half rounds up"},
		{5001, 1, 10000, false, true, 1, true, "half-up above half rounds up"},
		{19901, 500, 10000, false, false, 995, true, "non-terminating truncates"},
		{0, 500, 10000, false, false, 0, true, "zero multiplicand"},
		{19900, 0, 10000, false, false, 0, true, "zero rate"},
		{19900, 10000, 10000, false, false, 19900, true, "100 percent is identity"},
		{1, 1, 1, false, false, 1, true, "divide by one"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := MulDiv(tc.a, tc.b, tc.d, tc.roundUp, tc.halfUp)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if ok && got != tc.want {
				t.Fatalf("MulDiv(%d,%d,%d) = %d, want %d", tc.a, tc.b, tc.d, got, tc.want)
			}
		})
	}
}

func TestMulDivRejectsZeroDivisor(t *testing.T) {
	if _, ok := MulDiv(1, 1, 0, false, false); ok {
		t.Fatal("division by zero must not report ok")
	}
}

// TestMulDivNeverOverflowsSilently is the most important test in this package.
//
// It verifies that there is either a correct answer or an explicit overflow
// report, and never a third outcome.
func TestMulDivNeverOverflowsSilently(t *testing.T) {
	inputs := []uint64{0, 1, 2, 3, 7, 255, 256, 1 << 31, 1<<32 - 1, 1 << 62, math.MaxUint64}
	divisors := []uint64{1, 2, 3, 10, 10000, 1 << 32, 1<<63 - 1, math.MaxUint64}

	for _, a := range inputs {
		for _, b := range inputs {
			for _, d := range divisors {
				got, ok := MulDiv(a, b, d, false, false)
				if !ok {
					continue
				}
				// Check with 128-bit multiplication: got*d <= a*b < (got+1)*d
				hi1, lo1 := mul128(got, d)
				hi2, lo2 := mul128(a, b)
				if !le128(hi1, lo1, hi2, lo2) {
					t.Fatalf("MulDiv(%d,%d,%d)=%d is too large", a, b, d, got)
				}
				hi3, lo3 := mul128(got+1, d)
				if got != math.MaxUint64 && le128(hi3, lo3, hi2, lo2) {
					t.Fatalf("MulDiv(%d,%d,%d)=%d is too small", a, b, d, got)
				}
			}
		}
	}
}

func mul128(a, b uint64) (hi, lo uint64) {
	const mask = 0xFFFFFFFF
	a0, a1 := a&mask, a>>32
	b0, b1 := b&mask, b>>32
	p00 := a0 * b0
	p01 := a0 * b1
	p10 := a1 * b0
	p11 := a1 * b1
	mid := p00>>32 + p01&mask + p10&mask
	lo = (mid&mask)<<32 | p00&mask
	hi = p11 + p01>>32 + p10>>32 + mid>>32
	return hi, lo
}

func le128(hi1, lo1, hi2, lo2 uint64) bool {
	if hi1 != hi2 {
		return hi1 < hi2
	}
	return lo1 <= lo2
}

func TestAddOverflow(t *testing.T) {
	tests := []struct {
		a, b   int64
		want   int64
		wantOK bool
	}{
		{1, 2, 3, true},
		{-1, -2, -3, true},
		{math.MaxInt64, 1, 0, false},
		{math.MinInt64, -1, 0, false},
		{math.MaxInt64, -1, math.MaxInt64 - 1, true},
		{math.MinInt64, 1, math.MinInt64 + 1, true},
		{0, 0, 0, true},
	}
	for _, tc := range tests {
		got, ok := Add(tc.a, tc.b)
		if ok != tc.wantOK {
			t.Fatalf("Add(%d,%d) ok = %v, want %v", tc.a, tc.b, ok, tc.wantOK)
		}
		if ok && got != tc.want {
			t.Fatalf("Add(%d,%d) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestSubOverflow(t *testing.T) {
	tests := []struct {
		a, b   int64
		want   int64
		wantOK bool
	}{
		{3, 2, 1, true},
		{2, 3, -1, true},
		{math.MaxInt64, -1, 0, false},
		{math.MinInt64, 1, 0, false},
		{0, math.MinInt64, 0, false},
		{math.MinInt64, math.MinInt64, 0, true},
		{math.MaxInt64, math.MaxInt64, 0, true},
		{math.MinInt64, math.MaxInt64, 0, false},
		{math.MaxInt64, math.MinInt64, 0, false},
		// -1 - MaxInt64 == MinInt64, which is exactly representable, so it is not an
		// overflow.
		{-1, math.MaxInt64, math.MinInt64, true},
		{math.MinInt64, -1, math.MinInt64 + 1, true},
	}
	for _, tc := range tests {
		got, ok := Sub(tc.a, tc.b)
		if ok != tc.wantOK {
			t.Fatalf("Sub(%d,%d) ok = %v, want %v", tc.a, tc.b, ok, tc.wantOK)
		}
		if ok && got != tc.want {
			t.Fatalf("Sub(%d,%d) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestNegOverflow(t *testing.T) {
	if _, ok := Neg(math.MinInt64); ok {
		t.Fatal("Neg(MinInt64) must report overflow")
	}
	got, ok := Neg(math.MaxInt64)
	if !ok || got != -math.MaxInt64 {
		t.Fatalf("Neg(MaxInt64) = %d, %v", got, ok)
	}
}
