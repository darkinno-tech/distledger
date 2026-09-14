package distledger_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/im10furry/distledger"
	"github.com/im10furry/distledger/store/memory"
)

// 本文件使用**外部测试包**（package distledger_test）。
//
// 这有两个好处：一是验证公开 API 真的够用（不需要任何未导出符号），
// 二是避免 distledger_test → store/memory → distledger 的导入环。

const tenant = int64(0)

type fixture struct {
	t     *testing.T
	led   *distledger.Ledger
	clock *distledger.ManualClock
	store *memory.Store
}

func newFixture(t *testing.T, rules distledger.Rules) *fixture {
	t.Helper()
	clock := distledger.NewManualClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	store := memory.New()
	led, err := distledger.New(distledger.Config{
		Store: store,
		Clock: clock,
		Rules: rules,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = led.Close() })
	return &fixture{t: t, led: led, clock: clock, store: store}
}

func twoLevelRules() distledger.Rules {
	return distledger.Rules{
		Levels:     2,
		RateBP:     []distledger.Rate{500, 200},
		FreezeDays: 7,
	}
}

func (f *fixture) mustAgent(userID, parentID int64, status distledger.AgentStatus) distledger.Agent {
	f.t.Helper()
	a, err := f.led.BindAgent(context.Background(), distledger.BindAgentRequest{
		TenantID: tenant, UserID: userID, ParentID: parentID, Status: status,
	})
	if err != nil {
		f.t.Fatalf("BindAgent(%d): %v", userID, err)
	}
	return a
}

func (f *fixture) mustBuyer(buyerID, agentID int64) distledger.Binding {
	f.t.Helper()
	b, err := f.led.BindBuyer(context.Background(), distledger.BindBuyerRequest{
		TenantID: tenant, BuyerUserID: buyerID, AgentUserID: agentID,
		Source: distledger.SourceLink,
	})
	if err != nil {
		f.t.Fatalf("BindBuyer(%d -> %d): %v", buyerID, agentID, err)
	}
	return b
}

func (f *fixture) mustPay(orderID string, buyerID int64, amount distledger.Money) distledger.AccrueResult {
	f.t.Helper()
	res, err := f.led.OnOrderPaid(context.Background(), distledger.OrderPaidEvent{
		TenantID: tenant, OrderID: orderID, BuyerUserID: buyerID,
		PaidAmount: amount, PaidAt: f.clock.Now(),
	})
	if err != nil {
		f.t.Fatalf("OnOrderPaid(%s): %v", orderID, err)
	}
	return res
}

func (f *fixture) balance(userID int64) distledger.Account {
	f.t.Helper()
	bal, err := f.led.Balance(context.Background(), tenant, userID)
	if err != nil {
		f.t.Fatalf("Balance(%d): %v", userID, err)
	}
	return bal
}

// ── 分佣 ────────────────────────────────────────────────────────────────

func TestAccrueMultiLevel(t *testing.T) {
	f := newFixture(t, distledger.Rules{
		Levels: 3, RateBP: []distledger.Rate{500, 200, 100}, FreezeDays: 7,
	})
	f.mustAgent(3001, 0, distledger.AgentActive)
	f.mustAgent(3002, 3001, distledger.AgentActive)
	f.mustAgent(3003, 3002, distledger.AgentActive)
	f.mustBuyer(4001, 3003)

	res := f.mustPay("ORD-1", 4001, 100000) // 1000.00
	if !res.Attributed || res.AgentUserID != 3003 {
		t.Fatalf("attribution = %v/%d, want true/3003", res.Attributed, res.AgentUserID)
	}
	if len(res.Commissions) != 3 {
		t.Fatalf("got %d commissions, want 3", len(res.Commissions))
	}

	want := map[int64]distledger.Money{3003: 5000, 3002: 2000, 3001: 1000}
	for _, c := range res.Commissions {
		if got := want[c.AgentUserID]; c.Amount != got {
			t.Errorf("agent %d layer %d amount = %s, want %s", c.AgentUserID, c.Layer, c.Amount, got)
		}
		if c.Key.OrderID != "ORD-1" {
			t.Errorf("commission carries wrong order: %+v", c.Key)
		}
		if c.FreezeDays != 7 {
			t.Errorf("freeze days snapshot = %d, want 7", c.FreezeDays)
		}
		if c.RuleVersion == 0 {
			t.Error("rule version must be recorded for explainability")
		}
		if c.RuleSnapshot == "" {
			t.Error("rule snapshot must be recorded for explainability")
		}
	}

	// 资金进入「待结算」而非「可提现」。
	for userID, amount := range want {
		bal := f.balance(userID)
		if bal.Frozen != amount {
			t.Errorf("user %d frozen = %s, want %s", userID, bal.Frozen, amount)
		}
		if bal.Available != 0 {
			t.Errorf("user %d available = %s, want 0 before freeze elapses", userID, bal.Available)
		}
		if bal.TotalEarned != amount {
			t.Errorf("user %d total earned = %s, want %s", userID, bal.TotalEarned, amount)
		}
	}

	// 分佣总额不得超过基数。
	var total distledger.Money
	for _, c := range res.Commissions {
		total += c.Amount
	}
	if total > 100000 {
		t.Fatalf("allocated %s which exceeds the base 1000.00", total)
	}
}

func TestAccrueIsIdempotent(t *testing.T) {
	f := newFixture(t, twoLevelRules())
	f.mustAgent(3001, 0, distledger.AgentActive)
	f.mustAgent(3002, 3001, distledger.AgentActive)
	f.mustBuyer(4001, 3002)

	first := f.mustPay("ORD-1", 4001, 100000)
	if len(first.Commissions) != 2 {
		t.Fatalf("first call produced %d commissions, want 2", len(first.Commissions))
	}

	for i := 0; i < 5; i++ {
		again := f.mustPay("ORD-1", 4001, 100000)
		if !again.Replayed {
			t.Fatalf("call %d was not recognised as a replay", i+2)
		}
		if len(again.Commissions) != 0 {
			t.Fatalf("call %d produced %d new commissions", i+2, len(again.Commissions))
		}
	}

	if bal := f.balance(3002); bal.Frozen != 5000 {
		t.Fatalf("frozen = %s, want 50.00 exactly once", bal.Frozen)
	}
	if bal := f.balance(3001); bal.Frozen != 2000 {
		t.Fatalf("frozen = %s, want 20.00 exactly once", bal.Frozen)
	}
}

func TestAccrueWithoutBindingIsNotAnError(t *testing.T) {
	f := newFixture(t, twoLevelRules())
	res := f.mustPay("ORD-1", 4001, 100000)

	if res.Attributed {
		t.Fatal("order without binding must not be attributed")
	}
	if len(res.Commissions) != 0 {
		t.Fatalf("got %d commissions, want 0", len(res.Commissions))
	}
	if len(res.Skipped) == 0 || res.Skipped[0].Code != distledger.SkipNoParent {
		t.Fatalf("expected a SkipNoParent explanation, got %+v", res.Skipped)
	}
}

// TestNoRetroactiveAttribution 是一个安全属性测试。
//
// 若归因只看「当前绑定」，任何人只要在订单支付后把自己绑成买家的推广人
// 就能追认这笔收益。因此绑定必须在支付时刻就已经存在且有效。
func TestNoRetroactiveAttribution(t *testing.T) {
	f := newFixture(t, twoLevelRules())
	f.mustAgent(3001, 0, distledger.AgentActive)

	paidAt := f.clock.Now()
	if _, err := f.led.OnOrderPaid(context.Background(), distledger.OrderPaidEvent{
		TenantID: tenant, OrderID: "ORD-1", BuyerUserID: 4001,
		PaidAmount: 100000, PaidAt: paidAt,
	}); err != nil {
		t.Fatalf("OnOrderPaid: %v", err)
	}

	// 订单支付之后才建立绑定。
	f.clock.Advance(time.Hour)
	f.mustBuyer(4001, 3001)

	// 重放同一事件（沿用原支付时刻）：仍然不应归因。
	res, err := f.led.OnOrderPaid(context.Background(), distledger.OrderPaidEvent{
		TenantID: tenant, OrderID: "ORD-1", BuyerUserID: 4001,
		PaidAmount: 100000, PaidAt: paidAt,
	})
	if err != nil {
		t.Fatalf("OnOrderPaid: %v", err)
	}
	if res.Attributed {
		t.Fatal("a binding created after payment must not retroactively capture the order")
	}
	if len(res.Commissions) != 0 {
		t.Fatalf("got %d commissions, want 0", len(res.Commissions))
	}
}

func TestExpiredBindingIsNotAttributed(t *testing.T) {
	f := newFixture(t, distledger.Rules{
		Levels: 1, RateBP: []distledger.Rate{500},
		FreezeDays: 7, BindExpireDays: 30,
	})
	f.mustAgent(3001, 0, distledger.AgentActive)
	f.mustBuyer(4001, 3001)

	f.clock.Advance(31 * 24 * time.Hour)
	res := f.mustPay("ORD-1", 4001, 100000)
	if res.Attributed {
		t.Fatal("an expired binding must not be attributed")
	}
}

func TestDefaultRulesDoNotPayOut(t *testing.T) {
	f := newFixture(t, distledger.DefaultRules())
	f.mustAgent(3001, 0, distledger.AgentActive)
	f.mustBuyer(4001, 3001)

	res := f.mustPay("ORD-1", 4001, 100000)
	if res.Attributed != true {
		t.Fatal("order should still be attributed")
	}
	if len(res.Commissions) != 0 {
		t.Fatalf("default rules must not pay out, got %d commissions", len(res.Commissions))
	}
	if len(res.Skipped) == 0 || res.Skipped[0].Code != distledger.SkipZeroAmount {
		t.Fatalf("expected a SkipZeroAmount explanation, got %+v", res.Skipped)
	}
	if bal := f.balance(3001); bal.TotalEarned != 0 {
		t.Fatalf("total earned = %s, want 0 under default rules", bal.TotalEarned)
	}
}

func TestAccruePerOrderItem(t *testing.T) {
	f := newFixture(t, distledger.Rules{
		Levels: 1, RateBP: []distledger.Rate{1000}, FreezeDays: 7,
	})
	f.mustAgent(3001, 0, distledger.AgentActive)
	f.mustBuyer(4001, 3001)

	res, err := f.led.OnOrderPaid(context.Background(), distledger.OrderPaidEvent{
		TenantID: tenant, OrderID: "ORD-1", BuyerUserID: 4001,
		PaidAmount: 30000, PaidAt: f.clock.Now(),
		Items: []distledger.OrderItem{
			{ItemID: "SKU-A", Amount: 10000},
			{ItemID: "SKU-B", Amount: 20000},
		},
	})
	if err != nil {
		t.Fatalf("OnOrderPaid: %v", err)
	}
	if len(res.Commissions) != 2 {
		t.Fatalf("got %d commissions, want 2 (one per item)", len(res.Commissions))
	}
	byItem := map[string]distledger.Money{}
	for _, c := range res.Commissions {
		byItem[c.OrderItemID] = c.Amount
	}
	if byItem["SKU-A"] != 1000 || byItem["SKU-B"] != 2000 {
		t.Fatalf("unexpected per-item commissions: %+v", byItem)
	}
}

// TestItemAmountsCannotExceedPaidAmount 覆盖一个直接的资损入口：
// 如果明细金额之和可以大于实付金额，调用方只要塞一个虚高明细，
// 平台就会按高于实收的基数支付佣金。
func TestItemAmountsCannotExceedPaidAmount(t *testing.T) {
	f := newFixture(t, distledger.Rules{Levels: 1, RateBP: []distledger.Rate{1000}})
	_, err := f.led.OnOrderPaid(context.Background(), distledger.OrderPaidEvent{
		TenantID: tenant, OrderID: "ORD-1", BuyerUserID: 4001,
		PaidAmount: 10000, PaidAt: f.clock.Now(),
		Items: []distledger.OrderItem{
			{ItemID: "SKU-A", Amount: 10000},
			{ItemID: "SKU-B", Amount: 10000},
		},
	})
	if !errors.Is(err, distledger.ErrInvalidArgument) {
		t.Fatalf("expected ErrInvalidArgument, got %v", err)
	}
}

// TestCapIsEnforcedUnderRoundingUp 验证配额封顶在最容易越界的配置下生效：
// 向上取整 + 两级费率合计 100%，此时每级都会「多算出一点点」。
func TestCapIsEnforcedUnderRoundingUp(t *testing.T) {
	f := newFixture(t, distledger.Rules{
		Levels: 2, RateBP: []distledger.Rate{5000, 5000},
		Rounding: distledger.RoundUp, FreezeDays: 0,
	})
	f.mustAgent(3001, 0, distledger.AgentActive)
	f.mustAgent(3002, 3001, distledger.AgentActive)
	f.mustBuyer(4001, 3002)

	res := f.mustPay("ORD-1", 4001, 1) // 基数为 0.01 元
	var total distledger.Money
	for _, c := range res.Commissions {
		total += c.Amount
	}
	if total > 1 {
		t.Fatalf("allocated %s on a base of 0.01 — cap was not enforced", total)
	}
}

func TestIneligibleFirstLevelAgentBlocksAccrual(t *testing.T) {
	f := newFixture(t, distledger.Rules{Levels: 2, RateBP: []distledger.Rate{500, 200}})
	f.mustAgent(3001, 0, distledger.AgentActive)
	f.mustAgent(3002, 3001, distledger.AgentActive)
	f.mustBuyer(4001, 3002)

	// 绑定之后把第 1 级分销员停用。
	f.mustAgent(3002, 3001, distledger.AgentInactive)

	res := f.mustPay("ORD-1", 4001, 100000)
	if len(res.Commissions) != 0 {
		t.Fatalf("inactive agent must not earn, got %d commissions", len(res.Commissions))
	}
	if len(res.Skipped) == 0 || res.Skipped[0].Code != distledger.SkipIneligible {
		t.Fatalf("expected SkipIneligible, got %+v", res.Skipped)
	}
}

// TestIneligibleMidChainDoesNotBreakUpline 固化一条刻意的策略：
// 中间层级不合格只跳过该级，链条继续向上。否则一个被临时禁用的人会
// 无声地吞掉他所有上级的收益。
func TestIneligibleMidChainDoesNotBreakUpline(t *testing.T) {
	f := newFixture(t, distledger.Rules{
		Levels: 3, RateBP: []distledger.Rate{500, 200, 100},
	})
	f.mustAgent(3001, 0, distledger.AgentActive)
	f.mustAgent(3002, 3001, distledger.AgentInactive) // 中间层被禁用
	f.mustAgent(3003, 3002, distledger.AgentActive)
	f.mustBuyer(4001, 3003)

	res := f.mustPay("ORD-1", 4001, 100000)
	got := map[int64]distledger.Money{}
	for _, c := range res.Commissions {
		got[c.AgentUserID] = c.Amount
	}
	if got[3003] != 5000 {
		t.Errorf("layer 1 agent got %s, want 50.00", got[3003])
	}
	if _, earned := got[3002]; earned {
		t.Error("ineligible agent must not earn")
	}
	if got[3001] != 1000 {
		t.Errorf("layer 3 agent got %s, want 10.00 — the chain must not break", got[3001])
	}
}

// ── 关系链 ──────────────────────────────────────────────────────────────

func TestBindAgentRejectsCycle(t *testing.T) {
	f := newFixture(t, distledger.Rules{Levels: 3, RateBP: []distledger.Rate{100, 100, 100}})
	f.mustAgent(3001, 0, distledger.AgentActive)
	f.mustAgent(3002, 3001, distledger.AgentActive)

	_, err := f.led.BindAgent(context.Background(), distledger.BindAgentRequest{
		TenantID: tenant, UserID: 3001, ParentID: 3002, Status: distledger.AgentActive,
	})
	if !errors.Is(err, distledger.ErrParentAlreadySet) {
		t.Fatalf("expected ErrParentAlreadySet, got %v", err)
	}
}

func TestBindAgentRejectsSelfParent(t *testing.T) {
	f := newFixture(t, distledger.Rules{Levels: 1, RateBP: []distledger.Rate{100}})
	_, err := f.led.BindAgent(context.Background(), distledger.BindAgentRequest{
		TenantID: tenant, UserID: 3001, ParentID: 3001, Status: distledger.AgentActive,
	})
	if !errors.Is(err, distledger.ErrInvalidArgument) {
		t.Fatalf("expected ErrInvalidArgument, got %v", err)
	}
}

func TestBindAgentEnforcesDepthLimit(t *testing.T) {
	f := newFixture(t, distledger.Rules{Levels: 3, RateBP: []distledger.Rate{100, 100, 100}})
	f.mustAgent(1, 0, distledger.AgentActive)
	f.mustAgent(2, 1, distledger.AgentActive)
	f.mustAgent(3, 2, distledger.AgentActive)

	_, err := f.led.BindAgent(context.Background(), distledger.BindAgentRequest{
		TenantID: tenant, UserID: 4, ParentID: 3, Status: distledger.AgentActive,
	})
	if !errors.Is(err, distledger.ErrDepthExceeded) {
		t.Fatalf("expected ErrDepthExceeded, got %v", err)
	}
}

func TestBindAgentRejectsMissingParent(t *testing.T) {
	f := newFixture(t, distledger.Rules{Levels: 2, RateBP: []distledger.Rate{100, 100}})
	_, err := f.led.BindAgent(context.Background(), distledger.BindAgentRequest{
		TenantID: tenant, UserID: 3001, ParentID: 9999, Status: distledger.AgentActive,
	})
	if !errors.Is(err, distledger.ErrInvalidArgument) {
		t.Fatalf("expected ErrInvalidArgument, got %v", err)
	}
}

// TestBindBuyerIsFirstWins 固化防抢客规则。
func TestBindBuyerIsFirstWins(t *testing.T) {
	f := newFixture(t, distledger.Rules{Levels: 1, RateBP: []distledger.Rate{500}})
	f.mustAgent(3001, 0, distledger.AgentActive)
	f.mustAgent(3002, 0, distledger.AgentActive)

	f.mustBuyer(4001, 3001)

	binding, err := f.led.BindBuyer(context.Background(), distledger.BindBuyerRequest{
		TenantID: tenant, BuyerUserID: 4001, AgentUserID: 3002, Source: distledger.SourceLink,
	})
	if !errors.Is(err, distledger.ErrBindingLocked) {
		t.Fatalf("expected ErrBindingLocked, got %v", err)
	}
	if binding.AgentUserID != 3001 {
		t.Fatalf("existing binding was overwritten: agent = %d, want 3001", binding.AgentUserID)
	}
}

func TestBindBuyerAllowsRebindAfterExpiry(t *testing.T) {
	f := newFixture(t, distledger.Rules{
		Levels: 1, RateBP: []distledger.Rate{500}, BindExpireDays: 10,
	})
	f.mustAgent(3001, 0, distledger.AgentActive)
	f.mustAgent(3002, 0, distledger.AgentActive)
	first := f.mustBuyer(4001, 3001)

	f.clock.Advance(11 * 24 * time.Hour)
	second := f.mustBuyer(4001, 3002)

	if second.AgentUserID != 3002 {
		t.Fatalf("rebind failed: agent = %d, want 3002", second.AgentUserID)
	}
	if second.ReboundFrom != first.ID {
		t.Fatalf("ReboundFrom = %d, want %d (rebinding must be auditable)", second.ReboundFrom, first.ID)
	}
}

func TestBindBuyerRejectsInactiveAgent(t *testing.T) {
	f := newFixture(t, distledger.Rules{Levels: 1, RateBP: []distledger.Rate{500}})
	f.mustAgent(3001, 0, distledger.AgentInactive)
	_, err := f.led.BindBuyer(context.Background(), distledger.BindBuyerRequest{
		TenantID: tenant, BuyerUserID: 4001, AgentUserID: 3001, Source: distledger.SourceLink,
	})
	if !errors.Is(err, distledger.ErrInvalidArgument) {
		t.Fatalf("expected ErrInvalidArgument, got %v", err)
	}
}
