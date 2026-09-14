package distledger_test

import (
	"context"
	"testing"
	"time"

	"github.com/darkinno-tech/distledger"
	"github.com/darkinno-tech/distledger/store/memory"
)

// newLedgerOn builds another Ledger on top of an existing Store.
//
// It is used to verify contracts such as "a config change does not affect
// historical data": under the same Store but different rules, historical
// commissions must behave identically.
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
	res, err := f.led.Maintain(context.Background())
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
	f.mustPay("ORD-1", 4001, 100000) // commission 100.00

	receivedAt := f.clock.Now()
	f.receive("ORD-1", receivedAt)

	// The freeze window has not elapsed: Maintain does nothing.
	f.clock.Advance(6 * 24 * time.Hour)
	if res := f.maintain(); res.SettledCount != 0 {
		t.Fatalf("settled %d commissions before the freeze elapsed", res.SettledCount)
	}
	if bal := f.balance(3001); bal.Frozen != 10000 || bal.Available != 0 {
		t.Fatalf("before freeze: frozen=%s available=%s, want 100.00/0.00", bal.Frozen, bal.Available)
	}

	// The freeze window has elapsed: settle.
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

	// Running it a second time is idempotent.
	if again := f.maintain(); again.SettledCount != 0 {
		t.Fatalf("second Maintain settled %d commissions, want 0", again.SettledCount)
	}
}

// TestFreezeDaysIsSnapshotted is the regression test for ADR-005.
//
// The freeze window must be snapshotted at accrual time. If it were instead
// read from global config at query time, one operations change would move the
// availability date of every historical order at once—unilaterally overriding
// the timing already promised to agents, with no way to prove which
// rules applied back then.
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

	// Operations extends the freeze window from 7 days to 30.
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

	// Day 8 is the interesting point: under the new 30-day configuration the
	// commission would still be frozen, but the snapshot taken at accrual time
	// says 7 days, so it must settle. This is the whole point of ADR-005.
	clock.Set(receivedAt.AddDate(0, 0, 8))
	res, err := after.Maintain(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.SettledCount != 1 {
		t.Fatalf("settled %d, want 1 — the snapshotted 7-day freeze should have elapsed", res.SettledCount)
	}
}

// TestReceiveIsFirstWins prevents "using duplicate deliveries to move the
// availability date forward".
func TestReceiveIsFirstWins(t *testing.T) {
	f := newFixture(t, distledger.Rules{
		Levels: 1, RateBP: []distledger.Rate{1000}, FreezeDays: 7,
	})
	f.mustAgent(3001, 0, distledger.AgentActive)
	f.mustBuyer(4001, 3001)
	f.mustPay("ORD-1", 4001, 100000)

	receivedAt := f.clock.Now()
	f.receive("ORD-1", receivedAt)
	want := receivedAt.AddDate(0, 0, 7)
	// want is well in the future; the ledger clock is advanced below only to
	// make the "earlier receipt" attack meaningful.

	commissions, err := f.led.CommissionsByOrder(context.Background(), tenant, "ORD-1")
	if err != nil {
		t.Fatal(err)
	}
	if !commissions[0].AvailableAt.Equal(want) {
		t.Fatalf("available at = %s, want %s", commissions[0].AvailableAt, want)
	}

	// The dangerous redelivery is not a later one - a later receipt would push
	// availability further out, which hurts nobody. The one worth defending
	// against carries an EARLIER receipt time, because that is what pulls the
	// payout forward. First-wins must reject it even though the date it would
	// set is perfectly plausible on its own.
	f.clock.Advance(6 * 24 * time.Hour)
	backdated := receivedAt.Add(-30 * 24 * time.Hour)
	if res := f.receive("ORD-1", backdated); res.Updated != 0 {
		t.Fatalf("a backdated repeat receipt updated %d commissions, want 0", res.Updated)
	}

	commissions, err = f.led.CommissionsByOrder(context.Background(), tenant, "ORD-1")
	if err != nil {
		t.Fatal(err)
	}
	if !commissions[0].AvailableAt.Equal(want) {
		t.Fatalf("available at = %s, want it pinned at the first-wins value %s",
			commissions[0].AvailableAt, want)
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

// TestSettleNeverMakesBalanceNegative verifies the fail-safe behind "settle
// first, record second": even when the account's frozen balance cannot cover a
// commission, the balance must never go negative.
func TestSettleNeverMakesBalanceNegative(t *testing.T) {
	f := newFixture(t, distledger.Rules{
		Levels: 1, RateBP: []distledger.Rate{1000}, FreezeDays: 0,
	})
	f.mustAgent(3001, 0, distledger.AgentActive)
	f.mustBuyer(4001, 3001)
	f.mustPay("ORD-1", 4001, 100000)
	f.receive("ORD-1", f.clock.Now())

	// Force the account's frozen balance to zero, manufacturing the corrupt
	// state where the commission exists but the money does not.
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

	if _, err := f.led.Maintain(ctx); err == nil {
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

	res, err := led.Maintain(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.SettledCount != 3 {
		t.Fatalf("settled %d commissions, want 3 (one per tenant)", res.SettledCount)
	}

	// Tenants must not leak data across each other.
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
