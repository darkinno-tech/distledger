package distledger

import (
	"encoding/json"
	"math"
	"testing"
)

func TestMoneyApplyRounding(t *testing.T) {
	tests := []struct {
		name string
		m    Money
		r    Rate
		mode Rounding
		want Money
	}{
		{"5 percent of 199.00", 19900, 500, RoundDown, 995},
		{"2 percent of 199.00", 19900, 200, RoundDown, 398},
		{"truncates fraction", 19901, 500, RoundDown, 995},
		{"half-up rounds up at exactly .5", 100, 50, RoundHalfUp, 1},
		{"half-up rounds down below .5", 100, 49, RoundHalfUp, 0},
		{"up always rounds up", 100, 1, RoundUp, 1},
		{"up keeps exact value", 100, 100, RoundUp, 1},
		{"zero rate", 19900, 0, RoundDown, 0},
		{"zero amount", 0, 500, RoundDown, 0},
		{"hundred percent", 19900, 10000, RoundDown, 19900},
		{"negative amount is symmetric", -19900, 500, RoundDown, -995},
		{"negative truncates toward zero", -19901, 500, RoundDown, -995},
		{"negative half-up away from zero", -100, 50, RoundHalfUp, -1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.m.Apply(tc.r, tc.mode)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("%s.Apply(%s, %s) = %s, want %s",
					tc.m, tc.r, tc.mode, got, tc.want)
			}
		})
	}
}

// TestMoneyApplyNeverAmplifies pins a key safety property: a legal rate
// (100% or less) never amplifies the amount, so Apply cannot overflow.
func TestMoneyApplyNeverAmplifies(t *testing.T) {
	amounts := []Money{0, 1, -1, 99, 100, 101, 19900, -19900, 1 << 40, -(1 << 40), math.MaxInt64, math.MinInt64 + 1}
	rates := []Rate{0, 1, 5, 100, 500, 9999, 10000}
	modes := []Rounding{RoundDown, RoundHalfUp, RoundUp}

	for _, m := range amounts {
		for _, r := range rates {
			for _, mode := range modes {
				got, err := m.Apply(r, mode)
				if err != nil {
					t.Fatalf("%d.Apply(%d, %s) failed: %v", m, r, mode, err)
				}
				if abs64(int64(got)) > abs64(int64(m)) {
					t.Fatalf("%d.Apply(%d, %s) = %d amplifies the amount", m, r, mode, got)
				}
			}
		}
	}
}

func abs64(v int64) int64 {
	if v < 0 {
		if v == math.MinInt64 {
			return math.MaxInt64 // saturating is enough; only magnitudes are compared
		}
		return -v
	}
	return v
}

func TestMoneyApplyRejectsInvalidRate(t *testing.T) {
	for _, r := range []Rate{-1, MaxRateBP + 1} {
		if _, err := Money(100).Apply(r, RoundDown); err == nil {
			t.Fatalf("rate %d should have been rejected", r)
		}
	}
	if _, err := Money(100).Apply(100, Rounding(99)); err == nil {
		t.Fatal("unknown rounding mode should have been rejected")
	}
}

func TestMoneyArithmeticOverflow(t *testing.T) {
	if _, err := Money(math.MaxInt64).Add(1); err == nil {
		t.Fatal("Add overflow must be reported")
	}
	if _, err := Money(math.MinInt64).Sub(1); err == nil {
		t.Fatal("Sub overflow must be reported")
	}
	if _, err := Money(math.MinInt64).Neg(); err == nil {
		t.Fatal("Neg overflow must be reported")
	}
	if got, err := Money(math.MaxInt64).Add(-1); err != nil || got != math.MaxInt64-1 {
		t.Fatalf("Add(-1) = %d, %v", got, err)
	}
}

func TestRateString(t *testing.T) {
	tests := []struct {
		r    Rate
		want string
	}{
		{0, "0%"},
		{1, "0.01%"},
		{10, "0.1%"},
		{50, "0.5%"},
		{100, "1%"},
		{500, "5%"},
		{250, "2.5%"},
		{10000, "100%"},
	}
	for _, tc := range tests {
		if got := tc.r.String(); got != tc.want {
			t.Fatalf("Rate(%d).String() = %q, want %q", tc.r, got, tc.want)
		}
	}
}

func TestMoneyString(t *testing.T) {
	tests := []struct {
		m    Money
		want string
	}{
		{0, "0.00"},
		{1, "0.01"},
		{99, "0.99"},
		{100, "1.00"},
		{19900, "199.00"},
		{-19900, "-199.00"},
		{-1, "-0.01"},
		{100000000, "1000000.00"},
	}
	for _, tc := range tests {
		if got := tc.m.String(); got != tc.want {
			t.Fatalf("Money(%d).String() = %q, want %q", tc.m, got, tc.want)
		}
	}
}

func TestMoneyJSONRoundTrip(t *testing.T) {
	type payload struct {
		Amount Money `json:"amount"`
	}

	raw := []byte(`{"amount":"199.00"}`)
	var p payload
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if p.Amount != 19900 {
		t.Fatalf("got %d, want 19900", p.Amount)
	}

	out, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(out) != `{"amount":"199.00"}` {
		t.Fatalf("marshal produced %s", out)
	}

	// An integer JSON number is accepted as well.
	var p2 payload
	if err := json.Unmarshal([]byte(`{"amount":19900}`), &p2); err != nil {
		t.Fatalf("unmarshal int: %v", err)
	}
	if p2.Amount != 19900 {
		t.Fatalf("got %d, want 19900", p2.Amount)
	}

	// A fractional number must be rejected: it means money was sent as a float.
	var p3 payload
	if err := json.Unmarshal([]byte(`{"amount":199.5}`), &p3); err == nil {
		t.Fatal("float JSON number must be rejected to protect precision")
	}
}

func TestParseMoney(t *testing.T) {
	ok := map[string]Money{
		"0":        0,
		"199":      19900,
		"199.5":    19950,
		"199.50":   19950,
		"1.2":      120,
		"-1.23":    -123,
		"+1.23":    123,
		".5":       50,
		"  1.23  ": 123,
	}
	for in, want := range ok {
		got, err := ParseMoney(in)
		if err != nil {
			t.Fatalf("ParseMoney(%q) failed: %v", in, err)
		}
		if got != want {
			t.Fatalf("ParseMoney(%q) = %d, want %d", in, got, want)
		}
	}

	// More than two decimals must fail, not truncate silently: quiet loss is worse.
	for _, bad := range []string{"", "abc", "1.234", "1.", ".", "1..2", "1e3", "--1", "1,000"} {
		if _, err := ParseMoney(bad); err == nil {
			t.Fatalf("ParseMoney(%q) should have failed", bad)
		}
	}
}

func TestMoneyNegSuccess(t *testing.T) {
	got, err := Money(19900).Neg()
	if err != nil || got != -19900 {
		t.Fatalf("Neg = %d, %v; want -19900", got, err)
	}
	got, err = Money(0).Neg()
	if err != nil || got != 0 {
		t.Fatalf("Neg(0) = %d, %v; want 0", got, err)
	}
}

func TestMoneySubAndStringEdges(t *testing.T) {
	got, err := Money(100).Sub(Money(250))
	if err != nil || got != -150 {
		t.Fatalf("Sub = %d, %v; want -150", got, err)
	}
	got, err = Money(-100).Sub(Money(-250))
	if err != nil || got != 150 {
		t.Fatalf("Sub = %d, %v; want 150", got, err)
	}
}

func TestMoneyUnmarshalJSONEdges(t *testing.T) {
	var m Money
	if err := m.UnmarshalJSON([]byte("null")); err != nil || m != 0 {
		t.Fatalf("null = %d, %v; want 0", m, err)
	}
	if err := m.UnmarshalJSON([]byte(`"-1.23"`)); err != nil || m != -123 {
		t.Fatalf("negative string = %d, %v; want -123", m, err)
	}
	for _, bad := range []string{`"abc"`, `"1.234"`, `1.5e3`, `"`, `x`, `""`} {
		if err := m.UnmarshalJSON([]byte(bad)); err == nil {
			t.Errorf("UnmarshalJSON(%s) should have failed", bad)
		}
	}
}
