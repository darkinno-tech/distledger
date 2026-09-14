package distledger_test

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/im10furry/distledger"
)

// refundSeq hands out distinct refund identifiers, standing in for the refund
// order ids an integration would receive from its payment provider.
var refundSeq int64

func idem() string {
	refundSeq++
	return "refund-" + strconv.FormatInt(refundSeq, 10)
}

func (f *fixture) refund(ev distledger.OrderRefundedEvent) distledger.RefundResult {
	f.t.Helper()
	ev.TenantID = tenant
	if ev.RefundedAt.IsZero() {
		ev.RefundedAt = f.clock.Now()
	}
	res, err := f.led.OnOrderRefunded(context.Background(), ev)
	if err != nil {
		f.t.Fatalf("OnOrderRefunded(%s): %v", ev.OrderID, err)
	}
	return res
}

// seededOrders builds a two-level chain and pays a set of orders through it.
func (f *fixture) seededOrders(orderIDs ...string) {
	f.t.Helper()
	f.mustAgent(3001, 0, distledger.AgentActive)
	f.mustAgent(3002, 3001, distledger.AgentActive)
	f.mustBuyer(4001, 3002)
	for _, id := range orderIDs {
		f.mustPay(id, 4001, 100000) // 1000.00 -> 50.00 / 20.00
	}
}

func TestFullRefundReversesEveryLayer(t *testing.T) {
	f := newFixture(t, twoLevelRules())
	f.seededOrders("ORD-1")

	res := f.refund(distledger.OrderRefundedEvent{OrderID: "ORD-1", IsFull: true, IdemKey: idem()})

	if len(res.Reversed) != 2 {
		t.Fatalf("got %d reversal records, want 2 (one per layer)", len(res.Reversed))
	}
	if res.ReversedAmount != 7000 {
		t.Fatalf("reversed amount = %s, want 70.00", res.ReversedAmount)
	}
	for _, r := range res.Reversed {
		if r.Amount >= 0 {
			t.Errorf("reversal record %d has non-negative amount %s", r.ID, r.Amount)
		}
		if r.ReverseOf == 0 {
			t.Errorf("reversal record %d has no reverse_of link", r.ID)
		}
		if r.State != distledger.CommissionReversed {
			t.Errorf("reversal record %d state = %s, want reversed", r.ID, r.State)
		}
	}

	// Every accrual is fully reversed, so no balance remains on either side.
	for _, agentID := range []int64{3001, 3002} {
		bal := f.balance(agentID)
		if bal.Frozen != 0 || bal.Available != 0 {
			t.Errorf("agent %d still holds frozen=%s available=%s after a full refund",
				agentID, bal.Frozen, bal.Available)
		}
		if bal.TotalReversed == 0 {
			t.Errorf("agent %d total reversed was not recorded", agentID)
		}
		if bal.TotalEarned != bal.TotalReversed {
			t.Errorf("agent %d gross %s != reversed %s after a full refund",
				agentID, bal.TotalEarned, bal.TotalReversed)
		}
	}

	commissions, err := f.led.CommissionsByOrder(context.Background(), tenant, "ORD-1")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range commissions {
		if c.Amount > 0 && c.State != distledger.CommissionReversed {
			t.Errorf("accrual %d state = %s, want reversed", c.ID, c.State)
		}
		if c.Amount > 0 && c.ReversedAmount != c.Amount {
			t.Errorf("accrual %d reversed %s of %s", c.ID, c.ReversedAmount, c.Amount)
		}
	}

	if rep := f.selfCheck(); !rep.OK {
		t.Fatalf("invariants violated after a full refund: %+v", rep.Invariants)
	}
}

func TestPartialRefundIsProportional(t *testing.T) {
	f := newFixture(t, twoLevelRules())
	f.seededOrders("ORD-1")

	// Refund 40% of the order: each layer gives back 40% of its commission.
	res := f.refund(distledger.OrderRefundedEvent{OrderID: "ORD-1", Amount: 40000, IdemKey: idem()})
	if res.ReversedAmount != 2800 { // 40% of 70.00
		t.Fatalf("reversed amount = %s, want 28.00", res.ReversedAmount)
	}

	if bal := f.balance(3002); bal.Frozen != 3000 { // 50.00 - 20.00
		t.Fatalf("layer 1 frozen = %s, want 30.00", bal.Frozen)
	}
	if bal := f.balance(3001); bal.Frozen != 1200 { // 20.00 - 8.00
		t.Fatalf("layer 2 frozen = %s, want 12.00", bal.Frozen)
	}

	// A partially reversed accrual stays pending: it is not finished yet.
	commissions, err := f.led.CommissionsByOrder(context.Background(), tenant, "ORD-1")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range commissions {
		if c.Amount > 0 && c.State != distledger.CommissionPending {
			t.Errorf("accrual %d state = %s, want pending after a partial refund", c.ID, c.State)
		}
	}
}

// TestPartialReversalThenSettlementMovesOnlyTheOutstandingAmount is the reason
// Commission.ReversedAmount exists at all.
//
// Without it, Maintain would try to move the full Amount out of the frozen
// bucket even though part of it had already been clawed back, and the balance
// guard would abort settlement for the whole tenant.
func TestPartialReversalThenSettlementMovesOnlyTheOutstandingAmount(t *testing.T) {
	f := newFixture(t, distledger.Rules{
		Levels: 1, RateBP: []distledger.Rate{1000}, FreezeDays: 7,
	})
	f.mustAgent(3001, 0, distledger.AgentActive)
	f.mustBuyer(4001, 3001)
	f.mustPay("ORD-1", 4001, 100000) // 100.00 commission

	f.refund(distledger.OrderRefundedEvent{OrderID: "ORD-1", Amount: 30000, IdemKey: idem()}) // 30% -> 30.00 back

	if bal := f.balance(3001); bal.Frozen != 7000 {
		t.Fatalf("frozen after partial refund = %s, want 70.00", bal.Frozen)
	}

	f.receive("ORD-1", f.clock.Now())
	f.clock.Advance(8 * 24 * time.Hour)

	m := f.maintain()
	if m.SettledCount != 1 {
		t.Fatalf("settled %d commissions, want 1", m.SettledCount)
	}
	if m.SettledAmount != 7000 {
		t.Fatalf("settled amount = %s, want the outstanding 70.00", m.SettledAmount)
	}

	bal := f.balance(3001)
	if bal.Frozen != 0 || bal.Available != 7000 {
		t.Fatalf("after settlement: frozen=%s available=%s, want 0.00/70.00", bal.Frozen, bal.Available)
	}
	if rep := f.selfCheck(); !rep.OK {
		t.Fatalf("invariants violated: %+v", rep.Invariants)
	}
}

func TestRefundAfterSettlementClawsBackFromAvailable(t *testing.T) {
	f := newFixture(t, distledger.Rules{
		Levels: 1, RateBP: []distledger.Rate{1000}, FreezeDays: 0,
	})
	f.mustAgent(3001, 0, distledger.AgentActive)
	f.mustBuyer(4001, 3001)
	f.mustPay("ORD-1", 4001, 100000)
	f.receive("ORD-1", f.clock.Now())
	f.maintain()

	if bal := f.balance(3001); bal.Available != 10000 {
		t.Fatalf("available before refund = %s, want 100.00", bal.Available)
	}

	f.refund(distledger.OrderRefundedEvent{OrderID: "ORD-1", IsFull: true, IdemKey: idem()})

	bal := f.balance(3001)
	if bal.Available != 0 {
		t.Fatalf("available after refund = %s, want 0.00", bal.Available)
	}
	if bal.Frozen != 0 {
		t.Fatalf("frozen after refund = %s, want 0.00 (money was in Available)", bal.Frozen)
	}

	// The clawback must be visible in the ledger against the right bucket.
	entries, err := f.led.LedgerEntries(context.Background(), tenant, 3001, distledger.LedgerQuery{})
	if err != nil {
		t.Fatal(err)
	}
	last := entries[len(entries)-1]
	if last.BizType != distledger.LedgerReverse {
		t.Fatalf("last ledger entry is %s, want reverse", last.BizType)
	}
	if last.DeltaAvailable != -10000 || last.DeltaFrozen != 0 {
		t.Fatalf("unexpected clawback deltas: available=%s frozen=%s",
			last.DeltaAvailable, last.DeltaFrozen)
	}
	if rep := f.selfCheck(); !rep.OK {
		t.Fatalf("invariants violated: %+v", rep.Invariants)
	}
}

// TestRefundIsIdempotent covers the property that matters most in production:
// refund events are retried, duplicated and delivered out of order.
func TestRefundIsIdempotent(t *testing.T) {
	f := newFixture(t, twoLevelRules())
	f.seededOrders("ORD-1")

	ev := distledger.OrderRefundedEvent{OrderID: "ORD-1", Amount: 25000, IdemKey: idem()}
	first := f.refund(ev)
	if first.ReversedAmount == 0 {
		t.Fatal("the first refund did nothing")
	}

	for i := 0; i < 4; i++ {
		again := f.refund(ev)
		if again.ReversedAmount != 0 || len(again.Reversed) != 0 {
			t.Fatalf("delivery %d reversed another %s", i+2, again.ReversedAmount)
		}
		if !again.AlreadyReversed {
			t.Fatalf("delivery %d was not reported as already reversed", i+2)
		}
	}

	// A second, genuinely different refund of the same amount is a NEW
	// instalment and must be applied, unlike the retry above.
	second := f.refund(distledger.OrderRefundedEvent{OrderID: "ORD-1", Amount: 25000, IdemKey: idem()})
	if second.ReversedAmount != first.ReversedAmount {
		t.Fatalf("second instalment reversed %s, want the same step %s",
			second.ReversedAmount, first.ReversedAmount)
	}

	if bal := f.balance(3002); bal.Frozen != 2500 { // 50.00 - 12.50 - 12.50
		t.Fatalf("layer 1 frozen = %s, want 25.00", bal.Frozen)
	}
	if rep := f.selfCheck(); !rep.OK {
		t.Fatalf("invariants violated: %+v", rep.Invariants)
	}
}

// TestRefundCannotExceedWhatWasAccrued guards invariant I3 directly: whatever
// the caller sends, the clawback stops at the original amount.
func TestRefundCannotExceedWhatWasAccrued(t *testing.T) {
	f := newFixture(t, distledger.Rules{
		Levels: 1, RateBP: []distledger.Rate{1000}, FreezeDays: 7,
	})
	f.mustAgent(3001, 0, distledger.AgentActive)
	f.mustBuyer(4001, 3001)
	f.mustPay("ORD-1", 4001, 100000)

	// Refund the whole base, then refund it all over again.
	f.refund(distledger.OrderRefundedEvent{OrderID: "ORD-1", Amount: 100000, IdemKey: idem()})
	second := f.refund(distledger.OrderRefundedEvent{OrderID: "ORD-1", Amount: 100000, IdemKey: idem()})
	if second.ReversedAmount != 0 {
		t.Fatalf("a second full refund reversed another %s", second.ReversedAmount)
	}

	bal := f.balance(3001)
	if bal.Frozen != 0 || bal.TotalReversed != 10000 {
		t.Fatalf("frozen=%s totalReversed=%s, want 0.00/100.00", bal.Frozen, bal.TotalReversed)
	}
	if bal.TotalReversed > bal.TotalEarned {
		t.Fatalf("reversed %s exceeds earned %s", bal.TotalReversed, bal.TotalEarned)
	}

	commissions, err := f.led.CommissionsByOrder(context.Background(), tenant, "ORD-1")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range commissions {
		if c.Amount > 0 && c.ReversedAmount > c.Amount {
			t.Fatalf("accrual %d reversed %s of %s", c.ID, c.ReversedAmount, c.Amount)
		}
	}
}

func TestPerItemRefund(t *testing.T) {
	f := newFixture(t, distledger.Rules{
		Levels: 1, RateBP: []distledger.Rate{1000}, FreezeDays: 7,
	})
	f.mustAgent(3001, 0, distledger.AgentActive)
	f.mustBuyer(4001, 3001)

	if _, err := f.led.OnOrderPaid(context.Background(), distledger.OrderPaidEvent{
		TenantID: tenant, OrderID: "ORD-1", BuyerUserID: 4001,
		PaidAmount: 30000, PaidAt: f.clock.Now(),
		Items: []distledger.OrderItem{
			{ItemID: "SKU-A", Amount: 10000},
			{ItemID: "SKU-B", Amount: 20000},
		},
	}); err != nil {
		t.Fatal(err)
	}

	// Refund only SKU-A.
	res := f.refund(distledger.OrderRefundedEvent{
		OrderID: "ORD-1",
		IdemKey: idem(),
		Items:   []distledger.RefundItem{{ItemID: "SKU-A", Amount: 10000}},
	})
	if res.ReversedAmount != 1000 {
		t.Fatalf("reversed %s, want the SKU-A commission only (10.00)", res.ReversedAmount)
	}

	commissions, err := f.led.CommissionsByOrder(context.Background(), tenant, "ORD-1")
	if err != nil {
		t.Fatal(err)
	}
	reversed := map[string]distledger.Money{}
	for _, c := range commissions {
		if c.Amount < 0 {
			reversed[c.OrderItemID] += -c.Amount
		}
	}
	if reversed["SKU-A"] != 1000 || reversed["SKU-B"] != 0 {
		t.Fatalf("unexpected per-item reversals: %+v", reversed)
	}
}

func TestRefundValidation(t *testing.T) {
	f := newFixture(t, twoLevelRules())
	f.seededOrders("ORD-1")
	ctx := context.Background()

	cases := map[string]distledger.OrderRefundedEvent{
		"no mode":             {OrderID: "ORD-1"},
		"two modes":           {OrderID: "ORD-1", IsFull: true, Amount: 100},
		"missing refunded_at": {OrderID: "ORD-1", IsFull: true, RefundedAt: time.Now()},
		"unknown item":        {OrderID: "ORD-1", Items: []distledger.RefundItem{{ItemID: "NOPE", Amount: 1}}},
		"refund above base":   {OrderID: "ORD-1", Items: []distledger.RefundItem{{ItemID: "", Amount: 200000}}},
		"zero item amount":    {OrderID: "ORD-1", Items: []distledger.RefundItem{{ItemID: "", Amount: 0}}},
		"bad idempotency key": {OrderID: "ORD-1", IsFull: true, IdemKey: "not valid!"},
		"empty order":         {IsFull: true},
	}
	for name, ev := range cases {
		t.Run(name, func(t *testing.T) {
			ev.TenantID = tenant
			if ev.RefundedAt.IsZero() && name != "missing refunded_at" {
				ev.RefundedAt = f.clock.Now()
			}
			if _, err := f.led.OnOrderRefunded(ctx, ev); err == nil {
				t.Fatalf("expected an error for %+v", ev)
			} else if !errors.Is(err, distledger.ErrInvalidArgument) {
				t.Fatalf("error should wrap ErrInvalidArgument, got %v", err)
			}
		})
	}
}

func TestRefundOnOrderWithoutCommissionsIsANoOp(t *testing.T) {
	f := newFixture(t, twoLevelRules())
	res := f.refund(distledger.OrderRefundedEvent{OrderID: "ORD-UNKNOWN", IsFull: true, IdemKey: idem()})
	if !res.AlreadyReversed || res.ReversedAmount != 0 {
		t.Fatalf("expected a no-op, got %+v", res)
	}
}

func TestConcurrentRefundsApplyExactlyOnce(t *testing.T) {
	f := newFixture(t, distledger.Rules{
		Levels: 1, RateBP: []distledger.Rate{1000}, FreezeDays: 7,
	})
	f.mustAgent(3001, 0, distledger.AgentActive)
	f.mustBuyer(4001, 3001)
	f.mustPay("ORD-1", 4001, 100000)

	const workers = 16
	ev := distledger.OrderRefundedEvent{
		TenantID: tenant, OrderID: "ORD-1", Amount: 50000,
		IdemKey: idem(), RefundedAt: f.clock.Now(),
	}
	var (
		total    distledger.Money
		failures []error
		done     = make(chan struct{})
	)
	results := make(chan distledger.RefundResult, workers)
	errs := make(chan error, workers)
	for i := 0; i < workers; i++ {
		go func() {
			res, err := f.led.OnOrderRefunded(context.Background(), ev)
			results <- res
			errs <- err
			done <- struct{}{}
		}()
	}
	for i := 0; i < workers; i++ {
		<-done
	}
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			failures = append(failures, err)
		}
	}
	for res := range results {
		total += res.ReversedAmount
	}
	for _, err := range failures {
		t.Fatalf("concurrent refund failed: %v", err)
	}

	if total != 5000 {
		t.Fatalf("%d concurrent refunds reversed %s in total, want exactly 50.00", workers, total)
	}
	if bal := f.balance(3001); bal.Frozen != 5000 {
		t.Fatalf("frozen = %s, want 50.00", bal.Frozen)
	}
	if rep := f.selfCheck(); !rep.OK {
		t.Fatalf("invariants violated after concurrent refunds: %+v", rep.Invariants)
	}
}

// ── risk control ────────────────────────────────────────────────────────

func TestFreezeAndUnfreeze(t *testing.T) {
	f := newFixture(t, distledger.Rules{Levels: 1, RateBP: []distledger.Rate{1000}})
	f.mustAgent(3001, 0, distledger.AgentActive)
	f.mustBuyer(4001, 3001)
	f.mustPay("ORD-1", 4001, 100000)

	commissions, err := f.led.CommissionsByOrder(context.Background(), tenant, "ORD-1")
	if err != nil {
		t.Fatal(err)
	}
	id := commissions[0].ID

	frozen, err := f.led.FreezeCommission(context.Background(), tenant, id, "suspected fraud")
	if err != nil {
		t.Fatalf("FreezeCommission: %v", err)
	}
	if frozen.State != distledger.CommissionFrozen {
		t.Fatalf("state = %s, want frozen", frozen.State)
	}
	// Freezing does not move money.
	if bal := f.balance(3001); bal.Frozen != 10000 {
		t.Fatalf("frozen balance = %s, want 100.00 unchanged", bal.Frozen)
	}

	// A frozen commission is not settleable even once its window elapses.
	f.receive("ORD-1", f.clock.Now())
	f.clock.Advance(30 * 24 * time.Hour)
	if m := f.maintain(); m.SettledCount != 0 {
		t.Fatalf("a frozen commission was settled (%d)", m.SettledCount)
	}

	back, err := f.led.UnfreezeCommission(context.Background(), tenant, id, "appeal accepted")
	if err != nil {
		t.Fatalf("UnfreezeCommission: %v", err)
	}
	if back.State != distledger.CommissionPending {
		t.Fatalf("state = %s, want pending", back.State)
	}
	if m := f.maintain(); m.SettledCount != 1 {
		t.Fatalf("unfrozen commission was not settled (%d)", m.SettledCount)
	}
	if rep := f.selfCheck(); !rep.OK {
		t.Fatalf("invariants violated: %+v", rep.Invariants)
	}
}

func TestVoidForfeitsTheCommission(t *testing.T) {
	f := newFixture(t, distledger.Rules{Levels: 1, RateBP: []distledger.Rate{1000}})
	f.mustAgent(3001, 0, distledger.AgentActive)
	f.mustBuyer(4001, 3001)
	f.mustPay("ORD-1", 4001, 100000)

	commissions, err := f.led.CommissionsByOrder(context.Background(), tenant, "ORD-1")
	if err != nil {
		t.Fatal(err)
	}
	id := commissions[0].ID

	voided, err := f.led.VoidCommission(context.Background(), tenant, id, "confirmed fraud")
	if err != nil {
		t.Fatalf("VoidCommission: %v", err)
	}
	if voided.State != distledger.CommissionVoid {
		t.Fatalf("state = %s, want void", voided.State)
	}
	if voided.ReversedAmount != voided.Amount {
		t.Fatalf("reversed %s of %s, want the full amount", voided.ReversedAmount, voided.Amount)
	}

	bal := f.balance(3001)
	if bal.Frozen != 0 || bal.TotalReversed != 10000 {
		t.Fatalf("frozen=%s totalReversed=%s, want 0.00/100.00", bal.Frozen, bal.TotalReversed)
	}
	if rep := f.selfCheck(); !rep.OK {
		t.Fatalf("invariants violated after a void: %+v", rep.Invariants)
	}

	// A void is terminal.
	if _, err := f.led.FreezeCommission(context.Background(), tenant, id, "again"); err == nil {
		t.Fatal("freezing a voided commission must fail")
	}
}

func TestCommissionOperationsAreTenantScoped(t *testing.T) {
	f := newFixture(t, distledger.Rules{Levels: 1, RateBP: []distledger.Rate{1000}})
	f.mustAgent(3001, 0, distledger.AgentActive)
	f.mustBuyer(4001, 3001)
	f.mustPay("ORD-1", 4001, 100000)

	commissions, err := f.led.CommissionsByOrder(context.Background(), tenant, "ORD-1")
	if err != nil {
		t.Fatal(err)
	}
	id := commissions[0].ID

	// Commission ids are global, so an id alone must not grant access.
	_, err = f.led.FreezeCommission(context.Background(), 999, id, "cross tenant")
	if !errors.Is(err, distledger.ErrNotFound) {
		t.Fatalf("cross-tenant freeze = %v, want ErrNotFound", err)
	}
	_, err = f.led.VoidCommission(context.Background(), 999, id, "cross tenant")
	if !errors.Is(err, distledger.ErrNotFound) {
		t.Fatalf("cross-tenant void = %v, want ErrNotFound", err)
	}
}
