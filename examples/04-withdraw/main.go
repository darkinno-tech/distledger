// Command 04-withdraw walks the payout path end to end.
//
// It is the example for the part of this library that touches real money
// leaving the platform, and it deliberately shows three things a reader would
// otherwise have to discover from the source:
//
//  1. Money is reserved when the agent asks, not when an operator approves.
//     That is what the reserved bucket is for, and the output shows it moving.
//  2. The fee is recorded rather than routed. The agent's account loses the
//     gross and the ledger stays conserved; the destination receives the net.
//  3. A refund after a payout has no money left to claw back, and the debt
//     policy decides what happens next. Both answers are shown, because the
//     default is a refusal and that is a surprise worth seeing before it
//     happens in production.
//
// Run it with: go run .
package main

import (
	"context"
	"fmt"
	"time"

	"github.com/darkinno-tech/distledger"
	"github.com/darkinno-tech/distledger/store/memory"
)

const tenant = int64(0)

func main() {
	ctx := context.Background()
	clock := distledger.NewManualClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))

	rules := distledger.Rules{
		Levels:      2,
		RateBP:      []distledger.Rate{500, 200},
		FreezeDays:  7,
		MinWithdraw: 10000, // 100.00 minimum, so a one-cent payout is refused
		FeeRateBP:   200,   // 2% platform fee
	}

	led, err := distledger.New(distledger.Config{
		Store: memory.New(),
		Clock: clock,
		Rules: rules,
		// No Payout channel is configured, which is the default and means
		// automatic payout is off. MarkWithdrawPaid is how an operator records
		// a transfer they made by hand.
	})
	if err != nil {
		panic(err)
	}
	defer led.Close()

	// ── Earn something to withdraw ────────────────────────────────────
	must(led.BindAgent(ctx, distledger.BindAgentRequest{
		TenantID: tenant, UserID: 1001, Status: distledger.AgentActive,
	}))
	must(led.BindBuyer(ctx, distledger.BindBuyerRequest{
		TenantID: tenant, BuyerUserID: 2001, AgentUserID: 1001, Source: distledger.SourceLink,
	}))
	must(led.OnOrderPaid(ctx, distledger.OrderPaidEvent{
		TenantID: tenant, OrderID: "ORD-1", BuyerUserID: 2001,
		PaidAmount: 1000000, PaidAt: clock.Now(),
	}))
	must(led.OnOrderReceived(ctx, distledger.OrderReceivedEvent{
		TenantID: tenant, OrderID: "ORD-1", ReceivedAt: clock.Now(),
	}))
	clock.Advance(8 * 24 * time.Hour)
	must(led.Maintain(ctx))

	show(led, ctx, "1) after the freeze window elapsed")
	available := balance(led, ctx).Available

	// ── Below the minimum is refused ──────────────────────────────────
	if _, _, err := led.RequestWithdraw(ctx, distledger.WithdrawRequest{
		TenantID: tenant, UserID: 1001, Amount: 1, IdemKey: "wd-too-small",
	}); err != nil {
		fmt.Printf("2) a 1-cent request is refused: %v\n", err)
	}

	// ── Reserve ───────────────────────────────────────────────────────
	w, existed, err := led.RequestWithdraw(ctx, distledger.WithdrawRequest{
		TenantID: tenant, UserID: 1001, Amount: available,
		IdemKey: "wd-1", AccountInfo: "6222****1234",
	})
	if err != nil {
		panic(err)
	}
	fmt.Printf("3) requested %s (fee %s, so %s is paid out); existed=%v\n",
		w.Amount, w.Fee, w.RealAmount, existed)
	show(led, ctx, "   the money has already left the withdrawable bucket")

	// A retry with the same key returns the same withdrawal and reserves
	// nothing more. This is what makes a retried request safe.
	again, existed, err := led.RequestWithdraw(ctx, distledger.WithdrawRequest{
		TenantID: tenant, UserID: 1001, Amount: available,
		IdemKey: "wd-1", AccountInfo: "6222****1234",
	})
	if err != nil {
		panic(err)
	}
	fmt.Printf("4) the same key returns withdrawal %d again (existed=%v), reserved once\n",
		again.ID, existed)
	show(led, ctx, "   unchanged, so the retry did not reserve twice")

	// ── Review and pay ────────────────────────────────────────────────
	must(led.ApproveWithdraw(ctx, distledger.WithdrawDecision{
		TenantID: tenant, WithdrawalID: w.ID, Operator: "alice",
	}))
	fmt.Println("5) approved by alice")

	paid, err := led.MarkWithdrawPaid(ctx, distledger.PayoutOutcome{
		TenantID: tenant, WithdrawalID: w.ID, Operator: "alice", InvoiceNo: "BANK-20260914-001",
	})
	if err != nil {
		panic(err)
	}
	fmt.Printf("6) paid, invoice %s at %s\n", paid.InvoiceNo, paid.PaidAt.Format(time.RFC3339))
	show(led, ctx, "   the gross moved to withdrawn; the fee is revenue outside this ledger")

	// ── A refund now has nothing to claw back ─────────────────────────
	fmt.Println("7) the order is refunded after the money has left:")
	_, err = led.OnOrderRefunded(ctx, distledger.OrderRefundedEvent{
		TenantID: tenant, OrderID: "ORD-1", IsFull: true,
		IdemKey: "refund-1", RefundedAt: clock.Now(),
	})
	if err != nil {
		fmt.Printf("   refused by the default debt policy:\n   %v\n", err)
		fmt.Println("   the books are untouched, and the shortfall above is what has to be recovered")
	}
	report(led, ctx)

	// ── The same refund with the debt policy enabled ──────────────────
	fmt.Println("\n8) the same scenario with Rules.AllowNegative = true:")
	rules.AllowNegative = true
	led2, err := distledger.New(distledger.Config{
		Store: memory.New(), Clock: clock, Rules: rules,
	})
	if err != nil {
		panic(err)
	}
	defer led2.Close()

	must(led2.BindAgent(ctx, distledger.BindAgentRequest{
		TenantID: tenant, UserID: 1001, Status: distledger.AgentActive,
	}))
	must(led2.BindBuyer(ctx, distledger.BindBuyerRequest{
		TenantID: tenant, BuyerUserID: 2001, AgentUserID: 1001, Source: distledger.SourceLink,
	}))
	must(led2.OnOrderPaid(ctx, distledger.OrderPaidEvent{
		TenantID: tenant, OrderID: "ORD-2", BuyerUserID: 2001,
		PaidAmount: 1000000, PaidAt: clock.Now(),
	}))
	must(led2.OnOrderReceived(ctx, distledger.OrderReceivedEvent{
		TenantID: tenant, OrderID: "ORD-2", ReceivedAt: clock.Now(),
	}))
	clock.Advance(8 * 24 * time.Hour)
	must(led2.Maintain(ctx))

	w2, _, err := led2.RequestWithdraw(ctx, distledger.WithdrawRequest{
		TenantID: tenant, UserID: 1001, Amount: balance(led2, ctx).Available,
		IdemKey: "wd-2", AccountInfo: "6222****1234",
	})
	if err != nil {
		panic(err)
	}
	must(led2.ApproveWithdraw(ctx, distledger.WithdrawDecision{
		TenantID: tenant, WithdrawalID: w2.ID, Operator: "alice",
	}))
	must(led2.MarkWithdrawPaid(ctx, distledger.PayoutOutcome{
		TenantID: tenant, WithdrawalID: w2.ID, Operator: "alice", InvoiceNo: "BANK-2",
	}))
	if _, err := led2.OnOrderRefunded(ctx, distledger.OrderRefundedEvent{
		TenantID: tenant, OrderID: "ORD-2", IsFull: true,
		IdemKey: "refund-2", RefundedAt: clock.Now(),
	}); err != nil {
		panic(err)
	}
	report(led2, ctx)
	fmt.Println("   the debt is visible as a note, and is not counted as a violation")
	fmt.Println("   later earnings will pay it off automatically")
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

func balance(led *distledger.Ledger, ctx context.Context) distledger.Account {
	bal, err := led.Balance(ctx, tenant, 1001)
	if err != nil {
		panic(err)
	}
	return bal
}

func show(led *distledger.Ledger, ctx context.Context, label string) {
	bal := balance(led, ctx)
	fmt.Printf("   %s\n     frozen=%s available=%s withdrawing=%s withdrawn=%s\n",
		label, bal.Frozen, bal.Available, bal.Withdrawing, bal.Withdrawn)
}

func report(led *distledger.Ledger, ctx context.Context) {
	rep, err := led.SelfCheck(ctx, tenant)
	if err != nil {
		panic(err)
	}
	fmt.Printf("   self-check: overall=%v\n", rep.OK)
	for _, inv := range rep.Invariants {
		fmt.Printf("     [%s] %s (%d checked)\n", pass(inv.OK), inv.Name, inv.Checked)
		for _, v := range inv.Violations {
			fmt.Printf("         violation: %s\n", v)
		}
		for _, n := range inv.Notes {
			fmt.Printf("         note: %s\n", n)
		}
	}
}

func pass(ok bool) string {
	if ok {
		return "PASS"
	}
	return "FAIL"
}
