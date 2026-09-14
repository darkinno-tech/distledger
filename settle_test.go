package distledger_test

import (
	"context"
	"testing"
	"time"

	"github.com/im10furry/distledger"
	"github.com/im10furry/distledger/store/memory"
)

// newLedgerOn 在既有 Store 上再建一个 Ledger。
//
// 它用于验证「配置变更不影响历史数据」这类契约：同一个 Store、不同的规则，
// 历史佣金的表现必须完全一致。
func newLedgerOn(t *testing.T, store *memory.Store, clock distledger.Clock, rules distledger.Rules) *distledger.Ledger {
	t.Helper()
	led, err := distledger.New(distledger.Config{Store: store, Clock: clock, Rules: rules})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return led
}

func (f *fixture) receive(orderID string, at time.Time) distledger.ReceiveResult {
	f.t.Helper()
	res, err := f.led.OnOrderReceived(context.Background(), distledger.OrderReceivedEvent{
		TenantID: tenant, OrderID: orderID, ReceivedAt: at,
	})
	if err != nil {
		f.t.Fatalf("OnOrderReceived(%s): %v", orderID, err)
	}
	return res
}

func (f *fixture) maintain() distledger.MaintainResult {
	f.t.Helper()
	res, err := f.led.Maintain(context.Background(), time.Time{})
	if err != nil {
		f.t.Fatalf("Maintain: %v", err)
	}
	return res
}

func TestMaintainSettlesOnlyAfterFreezeElapses(t *testing.T) {
	f := newFixture(t, distledger.Rules{
		Levels: 1, RateBP: []distledger.Rate{1000}, FreezeDays: 7,
	})
	f.mustAgent(3001, 0, distledger.AgentActive)
	f.mustBuyer(4001, 3001)
	f.mustPay("ORD-1", 4001, 100000) // 佣金 100.00

	receivedAt := f.clock.Now()
	f.receive("ORD-1", receivedAt)

	// 冻结期未满：Maintain 什么都不做。
	f.clock.Advance(6 * 24 * time.Hour)
	if res := f.maintain(); res.SettledCount != 0 {
		t.Fatalf("settled %d commissions before the freeze elapsed", res.SettledCount)
	}
	if bal := f.balance(3001); bal.Frozen != 10000 || bal.Available != 0 {
		t.Fatalf("before freeze: frozen=%s available=%s, want 100.00/0.00", bal.Frozen, bal.Available)
	}

	// 冻结期满：结算。
	f.clock.Advance(2 * 24 * time.Hour)
	res := f.maintain()
	if res.SettledCount != 1 {
		t.Fatalf("settled %d commissions, want 1", res.SettledCount)
	}
	if res.SettledAmount != 10000 {
		t.Fatalf("settled amount = %s, want 100.00", res.SettledAmount)
	}
	bal := f.balance(3001)
	if bal.Frozen != 0 || bal.Available != 10000 {
		t.Fatalf("after freeze: frozen=%s available=%s, want 0.00/100.00", bal.Frozen, bal.Available)
	}

	// 再跑一次是幂等的。
	if again := f.maintain(); again.SettledCount != 0 {
		t.Fatalf("second Maintain settled %d commissions, want 0", again.SettledCount)
	}
}

// TestFreezeDaysIsSnapshotted 是 ADR-005 的回归测试。
//
// 冻结期必须在入账时快照。如果改成「查询时读全局配置」，运营改一次配置
// 就会把所有历史订单的到账时间一起改掉——已经承诺给分销员的时间被
// 单方面推翻，且无法自证当时的规则。
func TestFreezeDaysIsSnapshotted(t *testing.T) {
	clock := distledger.NewManualClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	store := memory.New()
	t.Cleanup(func() { _ = store.Close() })

	short := distledger.Rules{Levels: 1, RateBP: []distledger.Rate{1000}, FreezeDays: 7}
	logger := newLedgerOn(t, store, clock, short)

	ctx := context.Background()
	if _, err := logger.BindAgent(ctx, distledger.BindAgentRequest{
		TenantID: tenant, UserID: 3001, Status: distledger.AgentActive,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := logger.BindBuyer(ctx, distledger.BindBuyerRequest{
		TenantID: tenant, BuyerUserID: 4001, AgentUserID: 3001, Source: distledger.SourceLink,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := logger.OnOrderPaid(ctx, distledger.OrderPaidEvent{
		TenantID: tenant, OrderID: "ORD-1", BuyerUserID: 4001,
		PaidAmount: 100000, PaidAt: clock.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	// 运营把冻结期从 7 天改成 30 天。
	longer := distledger.Rules{Levels: 1, RateBP: []distledger.Rate{1000}, FreezeDays: 30}
	after := newLedgerOn(t, store, clock, longer)

	receivedAt := clock.Now()
	if _, err := after.OnOrderReceived(ctx, distledger.OrderReceivedEvent{
		TenantID: tenant, OrderID: "ORD-1", ReceivedAt: receivedAt,
	}); err != nil {
		t.Fatal(err)
	}

	commissions, err := after.CommissionsByOrder(ctx, tenant, "ORD-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(commissions) != 1 {
		t.Fatalf("got %d commissions, want 1", len(commissions))
	}
	c := commissions[0]
	if c.FreezeDays != 7 {
		t.Fatalf("freeze days = %d, want the snapshotted 7 (not the new 30)", c.FreezeDays)
	}
	want := receivedAt.AddDate(0, 0, 7)
	if !c.AvailableAt.Equal(want) {
		t.Fatalf("available at = %s, want %s (the snapshot must win)", c.AvailableAt, want)
	}

	// 按新的 30 天配置，第 8 天不应该结算。
	clock.Set(receivedAt.AddDate(0, 0, 8))
	res, err := after.Maintain(ctx, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if res.SettledCount != 1 {
		t.Fatalf("settled %d, want 1 — the snapshotted 7-day freeze should have elapsed", res.SettledCount)
	}
}

// TestReceiveIsFirstWins 防止「用重复投递把到账时间往前刷」。
func TestReceiveIsFirstWins(t *testing.T) {
	f := newFixture(t, distledger.Rules{
		Levels: 1, RateBP: []distledger.Rate{1000}, FreezeDays: 7,
	})
	f.mustAgent(3001, 0, distledger.AgentActive)
	f.mustBuyer(4001, 3001)
	f.mustPay("ORD-1", 4001, 100000)

	f.receive("ORD-1", f.clock.Now())

	// 稍后有人重新投递收货事件，试图把冻结期重新开始计时（从而更早到账）。
	f.clock.Advance(6 * 24 * time.Hour)
	if res := f.receive("ORD-1", f.clock.Now()); res.Updated != 0 {
		t.Fatalf("repeat receive updated %d commissions, want 0", res.Updated)
	}

	commissions, err := f.led.CommissionsByOrder(context.Background(), tenant, "ORD-1")
	if err != nil {
		t.Fatal(err)
	}
	want := f.clock.Now().AddDate(0, 0, 1) // 首次收货 + 7 天
	if !commissions[0].AvailableAt.Equal(want) {
		t.Fatalf("available at = %s, want the first-wins value %s", commissions[0].AvailableAt, want)
	}
}

func TestSettleWritesLedgerWithCorrectBalances(t *testing.T) {
	f := newFixture(t, distledger.Rules{
		Levels: 1, RateBP: []distledger.Rate{1000}, FreezeDays: 0,
	})
	f.mustAgent(3001, 0, distledger.AgentActive)
	f.mustBuyer(4001, 3001)
	f.mustPay("ORD-1", 4001, 100000)

	f.receive("ORD-1", f.clock.Now())
	if res := f.maintain(); res.SettledCount != 1 {
		t.Fatalf("settled %d, want 1 (freeze days is 0)", res.SettledCount)
	}

	entries, err := f.led.LedgerEntries(context.Background(), tenant, 3001, distledger.LedgerQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d ledger entries, want 2 (accrue + settle)", len(entries))
	}

	accrue, settle := entries[0], entries[1]
	if accrue.BizType != distledger.LedgerAccrue || accrue.DeltaFrozen != 10000 {
		t.Fatalf("unexpected accrue entry: %+v", accrue)
	}
	if accrue.AfterFrozen != 10000 {
		t.Fatalf("accrue AfterFrozen = %s, want 100.00", accrue.AfterFrozen)
	}
	if settle.BizType != distledger.LedgerSettle {
		t.Fatalf("unexpected settle entry: %+v", settle)
	}
	if settle.DeltaFrozen != -10000 || settle.DeltaAvailable != 10000 {
		t.Fatalf("settle deltas = %s/%s, want -100.00/100.00", settle.DeltaFrozen, settle.DeltaAvailable)
	}
	if settle.AfterFrozen != 0 || settle.AfterAvailable != 10000 {
		t.Fatalf("settle after = %s/%s, want 0.00/100.00", settle.AfterFrozen, settle.AfterAvailable)
	}
}

// TestSettleNeverMakesBalanceNegative 验证「先结算、再记账」的失败安全：
// 即使账户里的待结算余额不足以覆盖佣金，也绝不能变成负数。
func TestSettleNeverMakesBalanceNegative(t *testing.T) {
	f := newFixture(t, distledger.Rules{
		Levels: 1, RateBP: []distledger.Rate{1000}, FreezeDays: 0,
	})
	f.mustAgent(3001, 0, distledger.AgentActive)
	f.mustBuyer(4001, 3001)
	f.mustPay("ORD-1", 4001, 100000)
	f.receive("ORD-1", f.clock.Now())

	// 人为把账户的待结算余额清零，制造「佣金存在但钱不在」的破坏状态。
	ctx := context.Background()
	if err := f.store.Update(ctx, func(ctx context.Context, tx distledger.Tx) error {
		acct, err := tx.Account(ctx, distledger.UserKey{TenantID: tenant, UserID: 3001})
		if err != nil {
			return err
		}
		acct.Key = distledger.UserKey{TenantID: tenant, UserID: 3001}
		acct.Frozen = 0
		_, err = tx.PutAccount(ctx, acct)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := f.led.Maintain(ctx, time.Time{}); err == nil {
		t.Fatal("Maintain must fail loudly when the frozen balance cannot cover a commission")
	}

	bal := f.balance(3001)
	if bal.Frozen < 0 || bal.Available < 0 {
		t.Fatalf("balance went negative: frozen=%s available=%s", bal.Frozen, bal.Available)
	}
}

func TestMaintainAcrossMultipleTenants(t *testing.T) {
	clock := distledger.NewManualClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	store := memory.New()
	t.Cleanup(func() { _ = store.Close() })
	led := newLedgerOn(t, store, clock, distledger.Rules{
		Levels: 1, RateBP: []distledger.Rate{1000}, FreezeDays: 0,
	})
	defer led.Close()

	ctx := context.Background()
	for _, tn := range []int64{1, 2, 3} {
		if _, err := led.BindAgent(ctx, distledger.BindAgentRequest{
			TenantID: tn, UserID: 3001, Status: distledger.AgentActive,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := led.BindBuyer(ctx, distledger.BindBuyerRequest{
			TenantID: tn, BuyerUserID: 4001, AgentUserID: 3001, Source: distledger.SourceLink,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := led.OnOrderPaid(ctx, distledger.OrderPaidEvent{
			TenantID: tn, OrderID: "ORD-1", BuyerUserID: 4001,
			PaidAmount: 100000, PaidAt: clock.Now(),
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := led.OnOrderReceived(ctx, distledger.OrderReceivedEvent{
			TenantID: tn, OrderID: "ORD-1", ReceivedAt: clock.Now(),
		}); err != nil {
			t.Fatal(err)
		}
	}

	res, err := led.Maintain(ctx, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if res.SettledCount != 3 {
		t.Fatalf("settled %d commissions, want 3 (one per tenant)", res.SettledCount)
	}

	// 跨租户不能串数据。
	for _, tn := range []int64{1, 2, 3} {
		bal, err := led.Balance(ctx, tn, 3001)
		if err != nil {
			t.Fatal(err)
		}
		if bal.Available != 10000 {
			t.Fatalf("tenant %d available = %s, want 100.00", tn, bal.Available)
		}
	}
}
