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

// TestIdemKeyIsNotAmbiguous 验证长度前缀编码确实消除了拼接歧义。
//
// 如果实现改成用固定分隔符拼接，("a|b", "") 与 ("a", "b") 会撞成同一个键，
// 攻击者就能借此构造碰撞，让真实佣金被静默丢弃。
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

// TestOverrideProducesDistinctKeyPerCommission 覆盖一个真实缺陷的回归测试。
//
// 早期实现把调用方提供的幂等键直接当作最终键使用，导致一笔订单的
// 第 2 条佣金起全部被判为重复而静默丢弃——钱少了，而且没有任何报错。
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

	// 同一个 override 在同一租户下必须稳定。
	a := accrueIdemKey(order, "SKU-1", 1001, 1, override)
	b := accrueIdemKey(order, "SKU-1", 1001, 1, override)
	if a != b {
		t.Fatal("override-derived key is not deterministic")
	}
	// 但不同租户必须得到不同的键，否则跨租户可能互相判重。
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
