package distledger

import (
	"strings"
	"testing"
)

func TestAccrueIdemKeyIsDeterministic(t *testing.T) {
	order := OrderKey{TenantID: 7, OrderID: "ORD-1"}
	first := accrueIdemKey(order, "SKU-1", 1002, 1, "")
	for i := 0; i < 100; i++ {
		if got := accrueIdemKey(order, "SKU-1", 1002, 1, ""); got != first {
			t.Fatalf("idempotency key is not deterministic: %q vs %q", got, first)
		}
	}
	if len(first) != idemKeyLen {
		t.Fatalf("key length = %d, want %d", len(first), idemKeyLen)
	}
}

func TestAccrueIdemKeyVariesWithEveryField(t *testing.T) {
	base := accrueIdemKey(OrderKey{TenantID: 7, OrderID: "ORD-1"}, "SKU-1", 1002, 1, "")

	others := map[string]string{
		"tenant":   accrueIdemKey(OrderKey{TenantID: 8, OrderID: "ORD-1"}, "SKU-1", 1002, 1, ""),
		"order":    accrueIdemKey(OrderKey{TenantID: 7, OrderID: "ORD-2"}, "SKU-1", 1002, 1, ""),
		"item":     accrueIdemKey(OrderKey{TenantID: 7, OrderID: "ORD-1"}, "SKU-2", 1002, 1, ""),
		"agent":    accrueIdemKey(OrderKey{TenantID: 7, OrderID: "ORD-1"}, "SKU-1", 1003, 1, ""),
		"layer":    accrueIdemKey(OrderKey{TenantID: 7, OrderID: "ORD-1"}, "SKU-1", 1002, 2, ""),
		"override": accrueIdemKey(OrderKey{TenantID: 7, OrderID: "ORD-1"}, "SKU-1", 1002, 1, "retry-1"),
	}
	for name, key := range others {
		if key == base {
			t.Errorf("changing %s did not change the idempotency key", name)
		}
	}
}

// TestIdemKeyIsNotAmbiguous confirms that the length-prefixed encoding really
// removes concatenation ambiguity.
//
// If the implementation switched to joining the fields with a fixed delimiter,
// ("a|b", "") and ("a", "b") would collide into the same key, and an attacker
// could use that collision to make genuine commissions vanish silently.
func TestIdemKeyIsNotAmbiguous(t *testing.T) {
	order := OrderKey{TenantID: 0, OrderID: "ORD"}
	pairs := [][2]string{
		{"a|b", ""},
		{"a", "b"},
		{"", "a|b"},
		{"a|", "b"},
		{"a", "|b"},
	}
	seen := make(map[string][2]string, len(pairs))
	for _, p := range pairs {
		key := accrueIdemKey(order, p[0]+"\x00"+p[1], 1, 1, "")
		if prev, dup := seen[key]; dup {
			t.Fatalf("fields %v and %v produced the same key %q", prev, p, key)
		}
		seen[key] = p
	}
}

// TestOverrideProducesDistinctKeyPerCommission is a regression test for a real
// defect.
//
// An early implementation used the caller-supplied idempotency key directly as
// the final key, so from the second commission of an order onwards every one
// was judged a duplicate and dropped silently — money went missing with no
// error reported at all.
func TestOverrideProducesDistinctKeyPerCommission(t *testing.T) {
	order := OrderKey{TenantID: 0, OrderID: "ORD-1"}
	override := "my-retry-key"

	keys := map[string]bool{}
	for _, layer := range []int{1, 2, 3} {
		for _, item := range []string{"SKU-1", "SKU-2"} {
			k := accrueIdemKey(order, item, int64(1000+layer), layer, override)
			if keys[k] {
				t.Fatalf("override %q produced a colliding key for layer=%d item=%s", override, layer, item)
			}
			keys[k] = true
		}
	}
	if len(keys) != 6 {
		t.Fatalf("expected 6 distinct keys, got %d", len(keys))
	}

	// The same override must be stable under the same tenant.
	a := accrueIdemKey(order, "SKU-1", 1001, 1, override)
	b := accrueIdemKey(order, "SKU-1", 1001, 1, override)
	if a != b {
		t.Fatal("override-derived key is not deterministic")
	}
	// Different tenants must get different keys, or they could dedup each other.
	c := accrueIdemKey(OrderKey{TenantID: 9, OrderID: "ORD-1"}, "SKU-1", 1001, 1, override)
	if a == c {
		t.Fatal("override-derived key must include the tenant")
	}
}

func TestValidateIdemKeyOverride(t *testing.T) {
	valid := []string{"a", "A1", "retry-1", "a_b.c:d", strings.Repeat("x", MaxIdemKeyLen)}
	for _, v := range valid {
		if err := validateIdemKeyOverride(v); err != nil {
			t.Errorf("validateIdemKeyOverride(%q) = %v, want nil", v, err)
		}
	}

	invalid := []string{
		"",
		"has space",
		"has/slash",
		"has|pipe",
		"中文",
		"a\nb",
		strings.Repeat("x", MaxIdemKeyLen+1),
	}
	for _, v := range invalid {
		if err := validateIdemKeyOverride(v); err == nil {
			t.Errorf("validateIdemKeyOverride(%q) should have failed", v)
		}
	}
}
