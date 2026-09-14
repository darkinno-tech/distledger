package distledger

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/im10furry/distledger/internal/safemath"
)

// RateScale is the fixed denominator for rates. A rate is stored in basis
// points, so the stored integer is the percentage times 100 (equivalently, the
// decimal fraction times 10000).
//
//	5%    -> 500
//	0.01% -> 1
//	100%  -> 10000
const RateScale int64 = 10000

// Rate is a fee rate expressed in basis points.
//
// Fixed-point integers are used instead of floating point so that a value such
// as "5%" is exactly representable in binary, which guarantees that the same
// order produces exactly the same commission on any machine (see ADR-003).
type Rate int32

// MaxRateBP is the logical upper bound of a single rate (100%). Any rate above
// it is treated as a configuration error.
const MaxRateBP Rate = Rate(RateScale)

// Valid reports whether the rate falls within [0, 100%].
func (r Rate) Valid() bool { return r >= 0 && r <= MaxRateBP }

// String returns a human-readable percentage, such as "5%" or "0.01%".
//
// Basis points relate to percent as bp = percent * 100, so the integer part is
// v/100 and the fractional part (two digits) is v%100.
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
	// Pad on the left to two digits, then trim trailing zeros: 1 -> "0.01%",
	// 50 -> "0.5%".
	s := strconv.FormatInt(frac+100, 10)[1:]
	s = strings.TrimRight(s, "0")
	return neg + strconv.FormatInt(whole, 10) + "." + s + "%"
}

// Rounding specifies the rounding mode used in amount arithmetic.
//
// The default is RoundDown (truncation): the same conservative principle as
// "pay nothing by default" — better to underpay one cent than to overpay
// through rounding (see ADR-010).
type Rounding uint8

const (
	// RoundDown truncates toward zero.
	RoundDown Rounding = iota
	// RoundHalfUp rounds half away from zero.
	RoundHalfUp
	// RoundUp rounds up, away from zero.
	RoundUp
)

// Valid reports whether the rounding mode is a known value.
func (r Rounding) Valid() bool { return r <= RoundUp }

// String returns the name of the rounding mode.
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

// Money represents an amount in minor units (cents for CNY).
//
// It is deliberately a distinct type rather than a bare int64: the compiler
// then refuses to add Money to other quantities (item counts, points, bytes),
// a mix-up that is a real source of incidents in money systems.
//
// All arithmetic is integer arithmetic, and **overflow returns ErrOverflow
// rather than wrapping around**.
type Money int64

// Zero is the zero amount.
const Zero Money = 0

// Add returns m+o, or ErrOverflow on overflow.
func (m Money) Add(o Money) (Money, error) {
	s, ok := safemath.Add(int64(m), int64(o))
	if !ok {
		return 0, fmt.Errorf("%w: %d + %d", ErrOverflow, m, o)
	}
	return Money(s), nil
}

// Sub returns m-o, or ErrOverflow on overflow.
func (m Money) Sub(o Money) (Money, error) {
	d, ok := safemath.Sub(int64(m), int64(o))
	if !ok {
		return 0, fmt.Errorf("%w: %d - %d", ErrOverflow, m, o)
	}
	return Money(d), nil
}

// Neg returns -m, or ErrOverflow for math.MinInt64.
func (m Money) Neg() (Money, error) {
	n, ok := safemath.Neg(int64(m))
	if !ok {
		return 0, fmt.Errorf("%w: -(%d)", ErrOverflow, m)
	}
	return Money(n), nil
}

// IsZero reports whether the amount is zero.
func (m Money) IsZero() bool { return m == 0 }

// IsPositive reports whether the amount is positive.
func (m Money) IsPositive() bool { return m > 0 }

// IsNegative reports whether the amount is negative.
func (m Money) IsNegative() bool { return m < 0 }

// Sign returns -1, 0, or 1.
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

// Cmp compares two amounts: -1 when m<o, 0 when m==o, and 1 when m>o.
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

// Apply computes m*r with rate r and rounding mode mode.
//
// The result has the same sign as m; it returns an error when r is invalid or
// the arithmetic overflows.
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

// MulDiv returns m * numerator / denominator, rounded according to mode.
//
// It exists for calculations such as pro-rata allocation: computing a ratio
// first and then calling Apply would mean quantizing that ratio into basis
// points, and that intermediate multiplication adds an unnecessary overflow
// surface. Here it is a single step, the intermediate result uses 128 bits,
// and overflow is reported explicitly.
//
// denominator must be positive; numerator may have any sign.
func (m Money) MulDiv(numerator, denominator Money, mode Rounding) (Money, error) {
	if denominator <= 0 {
		return 0, fieldErrf("denominator", "must be > 0, got %d", denominator)
	}
	if !mode.Valid() {
		return 0, fieldErrf("rounding", "unknown mode: %d", mode)
	}
	if m == 0 || numerator == 0 {
		return Zero, nil
	}

	neg := (m < 0) != (numerator < 0)
	q, ok := safemath.MulDiv(
		absUint64(int64(m)), absUint64(int64(numerator)), uint64(denominator),
		mode == RoundUp, mode == RoundHalfUp,
	)
	if !ok || q > math.MaxInt64 {
		return 0, fmt.Errorf("%w: %d x %d / %d", ErrOverflow, m, numerator, denominator)
	}
	res := int64(q)
	if neg {
		res = -res
	}
	return Money(res), nil
}

// absUint64 returns |v| as a uint64, and is safe for math.MinInt64 as well.
func absUint64(v int64) uint64 {
	if v < 0 {
		return uint64(-(v + 1)) + 1
	}
	return uint64(v)
}

// String returns a decimal string in major units (yuan) with exactly two
// decimal places, such as "199.00".
//
// It returns a string rather than a float so that JSON numbers do not lose
// precision on the JavaScript side (see MarshalJSON).
func (m Money) String() string {
	v := int64(m)
	sign := ""
	if v < 0 {
		sign = "-"
		// Negating MinInt64 overflows and would render as garbage. This value
		// is unreachable through normal arithmetic, but String is exactly what
		// gets called when someone is diagnosing a corrupted balance, so it
		// must not be the thing that produces nonsense.
		if v == math.MinInt64 {
			return "-92233720368547758.08"
		}
		v = -v
	}
	whole := v / 100
	frac := v % 100
	return sign + strconv.FormatInt(whole, 10) + "." + strconv.FormatInt(frac+100, 10)[1:]
}

// MarshalJSON serializes the amount as a decimal string (such as "199.00").
//
// Avoiding a JSON number is deliberate: JavaScript's Number has only 53 bits
// of precision, so a large amount silently loses cents across a JSON number
// round trip.
func (m Money) MarshalJSON() ([]byte, error) {
	return []byte(`"` + m.String() + `"`), nil
}

// UnmarshalJSON parses an amount field.
//
// Two forms are accepted:
//   - a decimal string: "199.00", "199", "-1.23"
//   - an integer number: 199
//
// A fractional number (such as 1.23) is rejected: it means the caller is
// expressing an amount in floating point, exactly the precision risk this
// library exists to rule out. Pass the string "1.23" instead.
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

// ParseMoney parses a decimal string in major units (yuan), such as "199",
// "199.5", or "-1.23".
//
// More than two decimal places is always an error rather than a silent
// truncation: silently losing money is far more dangerous than failing.
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
