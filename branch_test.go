package distledger

import (
	"errors"
	"math"
	"strings"
	"testing"
)

// This file covers defensive branches and public entry points that the
// behaviour-driven tests reach only indirectly. They are grouped here rather
// than scattered because their purpose is the same: pin down what happens on
// the paths that should never run in production.
//
// An uncovered guard is indistinguishable from a guard that does not work, so
// each of these is exercised at least once.

func TestMoneyMulDiv(t *testing.T) {
	tests := []struct {
		name                string
		m, numerator, denom Money
		mode                Rounding
		want                Money
	}{
		{"proportional share", 5000, 25000, 100000, RoundDown, 1250},
		{"truncates", 5000, 1, 3, RoundDown, 1666},
		{"round half up", 5000, 1, 2, RoundHalfUp, 2500},
		{"round up", 5000, 1, 3, RoundUp, 1667},
		{"zero numerator", 5000, 0, 100, RoundDown, 0},
		{"zero amount", 0, 50, 100, RoundDown, 0},
		{"negative amount", -5000, 50, 100, RoundDown, -2500},
		{"negative numerator", 5000, -50, 100, RoundDown, -2500},
		{"both negative", -5000, -50, 100, RoundDown, 2500},
		{"identity", 1000, 7, 7, RoundDown, 1000},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.m.MulDiv(tc.numerator, tc.denom, tc.mode)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("MulDiv = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestMoneyMulDivRejectsBadInput(t *testing.T) {
	if _, err := Money(100).MulDiv(1, 0, RoundDown); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("zero denominator = %v, want ErrInvalidArgument", err)
	}
	if _, err := Money(100).MulDiv(1, -1, RoundDown); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("negative denominator = %v, want ErrInvalidArgument", err)
	}
	if _, err := Money(100).MulDiv(1, 10, Rounding(99)); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("unknown rounding = %v, want ErrInvalidArgument", err)
	}
	// A product that cannot be represented must surface as an error rather than
	// wrap around into a wrong, plausible-looking number.
	if _, err := Money(math.MaxInt64).MulDiv(math.MaxInt64, 1, RoundDown); !errors.Is(err, ErrOverflow) {
		t.Fatalf("overflow = %v, want ErrOverflow", err)
	}
}

func TestBaseOfEmptyList(t *testing.T) {
	if got := baseOf(nil); got != 0 {
		t.Fatalf("baseOf(nil) = %d, want 0", got)
	}
}

func TestValidateReasonLength(t *testing.T) {
	if err := validateReason(strings.Repeat("x", maxReasonLen)); err != nil {
		t.Fatalf("reason at the limit rejected: %v", err)
	}
	err := validateReason(strings.Repeat("x", maxReasonLen+1))
	if !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("over-long reason = %v, want ErrInvalidArgument", err)
	}
}
