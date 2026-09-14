// Command quickstart 演示 distledger 的完整链路，**不需要任何外部依赖**。
//
//	go run ./examples/01-quickstart
//
// 覆盖：建立关系链 → 归因 → 多级分佣 → 冻结 → 结算 → 自检。
package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/im10furry/distledger"
	"github.com/im10furry/distledger/store/memory"
)

func main() {
	ctx := context.Background()

	// 用可手动推进的时钟，这样冻结期不需要真的等 7 天。
	clock := distledger.NewManualClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))

	led, err := distledger.New(distledger.Config{
		Store: memory.New(), // 内存存储：零外部依赖
		Clock: clock,
		Rules: distledger.Rules{
			Levels:     2,
			RateBP:     []distledger.Rate{500, 200}, // 一级 5%，二级 2%
			FreezeDays: 7,                           // 收货后 7 天可提现
		},
	})
	if err != nil {
		log.Fatal(err)
	}
	defer led.Close()

	const tenant = int64(0)

	// ── 1. 建立关系链：A(1001) 是 B(1002) 的上级 ──────────────────────
	check(led.BindAgent(ctx, distledger.BindAgentRequest{
		TenantID: tenant, UserID: 1001,
		Status: distledger.AgentActive,
	}))
	check(led.BindAgent(ctx, distledger.BindAgentRequest{
		TenantID: tenant, UserID: 1002, ParentID: 1001,
		Status: distledger.AgentActive,
	}))
	fmt.Println("① 关系链就绪：A(1001) ← B(1002)")

	// ── 2. B 邀请 C(1003)：建立归因关系 ───────────────────────────────
	if _, err := led.BindBuyer(ctx, distledger.BindBuyerRequest{
		TenantID: tenant, BuyerUserID: 1003, AgentUserID: 1002,
		Source: distledger.SourceLink, SourceRef: "poster-b",
	}); err != nil {
		log.Fatal(err)
	}
	fmt.Println("② C(1003) 已归属 B(1002)")

	// ── 3. C 下单 199 元并支付 ────────────────────────────────────────
	res, err := led.OnOrderPaid(ctx, distledger.OrderPaidEvent{
		TenantID: tenant, OrderID: "ORD-1",
		BuyerUserID: 1003, PaidAmount: distledger.Money(19900), PaidAt: clock.Now(),
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("③ 归因成功=%v，产生 %d 笔佣金\n", res.Attributed, len(res.Commissions))
	for _, c := range res.Commissions {
		fmt.Printf("   第 %d 级  分销员=%d  基数=%s  费率=%s  佣金=%s\n",
			c.Layer, c.AgentUserID, c.BaseAmount, c.Rate, c.Amount)
	}

	// 重复投递同一事件：应当没有任何副作用。
	replay, err := led.OnOrderPaid(ctx, distledger.OrderPaidEvent{
		TenantID: tenant, OrderID: "ORD-1",
		BuyerUserID: 1003, PaidAmount: distledger.Money(19900), PaidAt: clock.Now(),
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("   重复投递：重放=%v，新增佣金=%d 笔（幂等生效）\n",
		replay.Replayed, len(replay.Commissions))

	// ── 4. 买家确认收货 → 冻结期开始计时 ─────────────────────────────
	if _, err := led.OnOrderReceived(ctx, distledger.OrderReceivedEvent{
		TenantID: tenant, OrderID: "ORD-1", ReceivedAt: clock.Now(),
	}); err != nil {
		log.Fatal(err)
	}
	printBalance(ctx, led, tenant, 1001, "④ 收货后")

	// ── 5. 时间推进 8 天 → Maintain 结算 ─────────────────────────────
	clock.Advance(8 * 24 * time.Hour)
	m, err := led.Maintain(ctx, time.Time{})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("⑤ 推进 8 天后结算 %d 笔，合计 %s\n", m.SettledCount, m.SettledAmount)
	printBalance(ctx, led, tenant, 1001, "   A")
	printBalance(ctx, led, tenant, 1002, "   B")

	// ── 6. 流水可追溯 ───────────────────────────────────────────────
	entries, err := led.LedgerEntries(ctx, tenant, 1002, distledger.LedgerQuery{})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("⑥ B 的资金流水：")
	for _, e := range entries {
		fmt.Printf("   #%d %-7s 待结算%+6s 可提现%+6s  余额[待结算=%s 可提现=%s]\n",
			e.ID, e.BizType, e.DeltaFrozen, e.DeltaAvailable, e.AfterFrozen, e.AfterAvailable)
	}

	// ── 7. 自检 ─────────────────────────────────────────────────────
	rep, err := led.SelfCheck(ctx, tenant)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("⑦ 自检：存储=%s 总体=%v\n", rep.StoreKind, rep.OK)
	for _, inv := range rep.Invariants {
		fmt.Printf("   [%s] %s（检查 %d 项）\n", pass(inv.OK), inv.Name, inv.Checked)
	}
}

func printBalance(ctx context.Context, led *distledger.Ledger, tenant, user int64, label string) {
	bal, err := led.Balance(ctx, tenant, user)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("%s 用户 %d：待结算=%s 可提现=%s 累计=%s\n",
		label, user, bal.Frozen, bal.Available, bal.TotalEarned)
}

func check(_ distledger.Agent, err error) {
	if err != nil {
		log.Fatal(err)
	}
}

func pass(ok bool) string {
	if ok {
		return "PASS"
	}
	return "FAIL"
}
