// Command quickstart walks the whole distledger flow with **no external dependencies**.
//
//	go run ./examples/01-quickstart
//
// It covers: agent chain → attribution → multi-level commission → freeze →
// settlement → self-check.
package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/darkinno-tech/distledger"
	"github.com/darkinno-tech/distledger/store/memory"
)

func main() {
	ctx := context.Background()

	// Use a manually advanced clock, so the freeze window does not need a real 7-day wait.
	clock := distledger.NewManualClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))

	led, err := distledger.New(distledger.Config{
		Store: memory.New(), // in-memory store: zero external dependencies
		Clock: clock,
		Rules: distledger.Rules{
			Levels:     2,
			RateBP:     []distledger.Rate{500, 200}, // level 1 = 5%, level 2 = 2%
			FreezeDays: 7,                           // withdrawable 7 days after receipt
		},
	})
	if err != nil {
		log.Fatal(err)
	}
	defer led.Close()

	const tenant = int64(0)

	// ── 1. Build the agent chain: A(1001) is B(1002)'s upline ─────────
	check(led.BindAgent(ctx, distledger.BindAgentRequest{
		TenantID: tenant, UserID: 1001,
		Status: distledger.AgentActive,
	}))
	check(led.BindAgent(ctx, distledger.BindAgentRequest{
		TenantID: tenant, UserID: 1002, ParentID: 1001,
		Status: distledger.AgentActive,
	}))
	fmt.Println("1) agent chain ready: A(1001) ← B(1002)")

	// ── 2. B invites C(1003) to establish attribution ─────────────────
	if _, err := led.BindBuyer(ctx, distledger.BindBuyerRequest{
		TenantID: tenant, BuyerUserID: 1003, AgentUserID: 1002,
		Source: distledger.SourceLink, SourceRef: "poster-b",
	}); err != nil {
		log.Fatal(err)
	}
	fmt.Println("2) C(1003) is now attributed to B(1002)")

	// ── 3. C places a CNY 199 order and pays for it ───────────────────
	res, err := led.OnOrderPaid(ctx, distledger.OrderPaidEvent{
		TenantID: tenant, OrderID: "ORD-1",
		BuyerUserID: 1003, PaidAmount: distledger.Money(19900), PaidAt: clock.Now(),
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("3) attributed=%v, %d commission(s) accrued\n", res.Attributed, len(res.Commissions))
	for _, c := range res.Commissions {
		fmt.Printf("   level %d  agent=%d  base=%s  rate=%s  commission=%s\n",
			c.Layer, c.AgentUserID, c.BaseAmount, c.Rate, c.Amount)
	}

	// Redelivering the same event must have no side effects at all.
	replay, err := led.OnOrderPaid(ctx, distledger.OrderPaidEvent{
		TenantID: tenant, OrderID: "ORD-1",
		BuyerUserID: 1003, PaidAmount: distledger.Money(19900), PaidAt: clock.Now(),
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("   redelivered: replayed=%v, new commissions=%d (idempotency holds)\n",
		replay.Replayed, len(replay.Commissions))

	// ── 4. Buyer confirms receipt → the freeze window starts ─────────
	if _, err := led.OnOrderReceived(ctx, distledger.OrderReceivedEvent{
		TenantID: tenant, OrderID: "ORD-1", ReceivedAt: clock.Now(),
	}); err != nil {
		log.Fatal(err)
	}
	printBalance(ctx, led, tenant, 1001, "4) after receipt")

	// ── 5. Advance time by 8 days → Maintain settles ─────────────────
	clock.Advance(8 * 24 * time.Hour)
	m, err := led.Maintain(ctx)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("5) after 8 days: %d settled, total %s\n", m.SettledCount, m.SettledAmount)
	printBalance(ctx, led, tenant, 1001, "   A")
	printBalance(ctx, led, tenant, 1002, "   B")

	// ── 6. The ledger entries are auditable ─────────────────────────
	entries, err := led.LedgerEntries(ctx, tenant, 1002, distledger.LedgerQuery{})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("6) ledger entries for B:")
	for _, e := range entries {
		fmt.Printf("   #%d %-7s frozen%+6s available%+6s  balance[frozen=%s available=%s]\n",
			e.ID, e.BizType, e.DeltaFrozen, e.DeltaAvailable, e.AfterFrozen, e.AfterAvailable)
	}

	// ── 7. Self-check ───────────────────────────────────────────────
	rep, err := led.SelfCheck(ctx, tenant)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("7) self-check: store=%s overall=%v\n", rep.StoreKind, rep.OK)
	for _, inv := range rep.Invariants {
		fmt.Printf("   [%s] %s (%d checked)\n", pass(inv.OK), inv.Name, inv.Checked)
	}
}

func printBalance(ctx context.Context, led *distledger.Ledger, tenant, user int64, label string) {
	bal, err := led.Balance(ctx, tenant, user)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("%s user %d: frozen=%s available=%s total=%s\n",
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
