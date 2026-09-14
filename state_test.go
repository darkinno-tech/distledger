package distledger

import (
	"errors"
	"testing"
)

// TestCommissionTransitionMatrix 穷举全部 6×6 组合。
//
// 期望值在测试里**独立重述了一遍**，而不是引用被测的那张表：
// 如果实现里的迁移表被误改，而测试直接引用它，测试会跟着一起改对，
// 这种「同源验证」等于没有验证。
func TestCommissionTransitionMatrix(t *testing.T) {
	want := map[CommissionState]map[CommissionState]bool{
		CommissionPending: {
			CommissionSettled:  true,
			CommissionReversed: true,
			CommissionFrozen:   true,
			CommissionVoid:     true,
		},
		CommissionSettled: {
			CommissionWithdrawn: true,
			CommissionReversed:  true,
		},
		CommissionFrozen: {
			CommissionPending: true,
			CommissionVoid:    true,
		},
		CommissionWithdrawn: {},
		CommissionReversed:  {},
		CommissionVoid:      {},
	}

	states := AllCommissionStates()
	if len(states) != len(want) {
		t.Fatalf("state count mismatch: %d states but %d expected groups", len(states), len(want))
	}

	for _, from := range states {
		for _, to := range states {
			expected := want[from][to]
			got := CanTransitionCommission(from, to)
			if got != expected {
				t.Errorf("CanTransitionCommission(%s, %s) = %v, want %v",
					from, to, got, expected)
			}
		}
	}
}

// TestSelfTransitionAlwaysRejected 单独强调一条容易被忽略的规则：
// 状态推进必须是真实推进。若允许 Pending → Pending 成立，
// 重复投递就可能被当成一次成功的状态迁移而重复记账。
func TestSelfTransitionAlwaysRejected(t *testing.T) {
	for _, s := range AllCommissionStates() {
		if CanTransitionCommission(s, s) {
			t.Errorf("%s -> %s must not be a legal transition", s, s)
		}
	}
}

func TestTerminalStates(t *testing.T) {
	terminal := map[CommissionState]bool{
		CommissionWithdrawn: true,
		CommissionReversed:  true,
		CommissionVoid:      true,
	}
	for _, s := range AllCommissionStates() {
		if s.Terminal() != terminal[s] {
			t.Errorf("%s.Terminal() = %v, want %v", s, s.Terminal(), terminal[s])
		}
		for _, to := range AllCommissionStates() {
			if s.Terminal() && CanTransitionCommission(s, to) {
				t.Errorf("terminal state %s must not transition to %s", s, to)
			}
		}
	}
}

func TestValidateCommissionTransitionError(t *testing.T) {
	err := ValidateCommissionTransition(CommissionWithdrawn, CommissionPending)
	if !errors.Is(err, ErrIllegalTransition) {
		t.Fatalf("expected ErrIllegalTransition, got %v", err)
	}
	var te *TransitionError
	if !errors.As(err, &te) {
		t.Fatalf("expected *TransitionError, got %T", err)
	}
	if te.From != "withdrawn" || te.To != "pending" {
		t.Fatalf("unexpected transition error payload: %+v", te)
	}

	if err := ValidateCommissionTransition(CommissionPending, CommissionSettled); err != nil {
		t.Fatalf("legal transition reported an error: %v", err)
	}

	if err := ValidateCommissionTransition(CommissionState(99), CommissionPending); err == nil {
		t.Fatal("unknown source state must be rejected")
	}
	if err := ValidateCommissionTransition(CommissionPending, CommissionState(99)); err == nil {
		t.Fatal("unknown target state must be rejected")
	}
}

// TestAllCommissionStatesReturnsCopy 防止调用方通过修改返回值破坏状态机。
func TestAllCommissionStatesReturnsCopy(t *testing.T) {
	a := AllCommissionStates()
	a[0] = CommissionState(200)
	b := AllCommissionStates()
	if b[0] == CommissionState(200) {
		t.Fatal("AllCommissionStates leaked its internal slice")
	}
}
