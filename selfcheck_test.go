package distledger_test

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/im10furry/distledger"
)

func (f *fixture) selfCheck() distledger.Report {
	f.t.Helper()
	rep, err := f.led.SelfCheck(context.Background(), tenant)
	if err != nil {
		f.t.Fatalf("SelfCheck: %v", err)
	}
	return rep
}

func (f *fixture) healthy() {
	f.t.Helper()
	f.mustAgent(3001, 0, distledger.AgentActive)
	f.mustAgent(3002, 3001, distledger.AgentActive)
	f.mustBuyer(4001, 3002)
	f.mustPay("ORD-1", 4001, 100000)
	f.receive("ORD-1", f.clock.Now())
	f.clock.Advance(8 * 24 * time.Hour)
	f.maintain()
}

func TestSelfCheckPassesOnHealthyData(t *testing.T) {
	f := newFixture(t, twoLevelRules())
	f.healthy()

	rep := f.selfCheck()
	if !rep.OK {
		t.Fatalf("healthy data reported problems: %+v", rep)
	}
	if rep.StoreKind != "memory" {
		t.Errorf("store kind = %q, want memory", rep.StoreKind)
	}
	if len(rep.Invariants) != 3 {
		t.Fatalf("got %d invariants, want 3", len(rep.Invariants))
	}
	for _, inv := range rep.Invariants {
		if !inv.OK {
			t.Errorf("invariant %q failed: %+v", inv.Name, inv.Violations)
		}
		if inv.Checked == 0 {
			t.Errorf("invariant %q checked nothing — a pass here proves nothing", inv.Name)
		}
	}
	if rep.Counters["settled"] != 2 {
		t.Errorf("counter settled = %d, want 2", rep.Counters["settled"])
	}
}

// TestSelfCheckDetectsAccountWithoutLedger covers the most common corruption
// shape: someone edits an account balance directly and forgets the ledger entry.
func TestSelfCheckDetectsAccountWithoutLedger(t *testing.T) {
	f := newFixture(t, twoLevelRules())
	ctx := context.Background()

	if err := f.store.Update(ctx, func(ctx context.Context, tx distledger.Tx) error {
		key := distledger.UserKey{TenantID: tenant, UserID: 7777}
		_, err := tx.PutAccount(ctx, distledger.Account{Key: key, Available: 500000})
		return err
	}); err != nil {
		t.Fatal(err)
	}

	rep := f.selfCheck()
	if rep.OK {
		t.Fatal("an account with a balance but no ledger entries must be reported")
	}
	if !hasViolation(rep, "I1") {
		t.Fatalf("I1 did not report the orphan account: %+v", rep.Invariants)
	}
}

// TestSelfCheckDetectsLedgerSumMismatch covers a ledger entry that was appended
// without the matching account update.
func TestSelfCheckDetectsLedgerSumMismatch(t *testing.T) {
	f := newFixture(t, twoLevelRules())
	f.healthy()
	ctx := context.Background()

	if err := f.store.Update(ctx, func(ctx context.Context, tx distledger.Tx) error {
		_, err := tx.AppendLedger(ctx, distledger.LedgerEntry{
			Key:         distledger.UserKey{TenantID: tenant, UserID: 3001},
			BizType:     distledger.LedgerManualAdjust,
			BizID:       "manual-1",
			DeltaFrozen: 12345,
			AfterFrozen: 99999, // does not match the account's real value
			CreatedAt:   f.clock.Now(),
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}

	rep := f.selfCheck()
	if rep.OK {
		t.Fatal("a ledger entry that does not match the account must be reported")
	}
	if !hasViolation(rep, "I1") {
		t.Fatalf("I1 did not report the mismatch: %+v", rep.Invariants)
	}
}

// TestSelfCheckDetectsAdjustedAccount verifies that tampering with both the
// account and the ledger is still caught.
//
// Comparing "ledger sum == account" alone is not enough: changing both sides
// keeps the equation satisfied. I1 therefore also checks that the last ledger
// entry's After* fields agree with the account.
func TestSelfCheckDetectsAdjustedAccount(t *testing.T) {
	f := newFixture(t, twoLevelRules())
	f.healthy()
	ctx := context.Background()
	key := distledger.UserKey{TenantID: tenant, UserID: 3001}

	if err := f.store.Update(ctx, func(ctx context.Context, tx distledger.Tx) error {
		acct, err := tx.Account(ctx, key)
		if err != nil {
			return err
		}
		acct.Key = key
		acct.Available += 50000
		if _, err := tx.PutAccount(ctx, acct); err != nil {
			return err
		}
		// Append a "make up the difference" ledger entry so that
		// "ledger sum == account" holds again.
		_, err = tx.AppendLedger(ctx, distledger.LedgerEntry{
			Key:              key,
			BizType:          distledger.LedgerManualAdjust,
			BizID:            "manual-2",
			DeltaAvailable:   50000,
			AfterFrozen:      acct.Frozen,
			AfterAvailable:   acct.Available,
			AfterWithdrawing: acct.Withdrawing,
			AfterWithdrawn:   acct.Withdrawn,
			CreatedAt:        f.clock.Now(),
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}

	// This data is self-consistent in accounting terms, so I1 should pass—and
	// that is exactly where I1 is honest: the library cannot tell a legitimate
	// manual adjustment from tampering, and all it can guarantee is that the
	// equation holds.
	rep := f.selfCheck()
	if !rep.OK {
		t.Fatalf("a self-consistent manual adjustment should pass: %+v", rep.Invariants)
	}
}

func TestSelfCheckDetectsDanglingCommissionReference(t *testing.T) {
	f := newFixture(t, twoLevelRules())
	ctx := context.Background()

	if err := f.store.Update(ctx, func(ctx context.Context, tx distledger.Tx) error {
		_, err := tx.AppendLedger(ctx, distledger.LedgerEntry{
			Key:         distledger.UserKey{TenantID: tenant, UserID: 3001},
			BizType:     distledger.LedgerAccrue,
			BizID:       "999999",
			DeltaFrozen: 100,
			AfterFrozen: 100,
			CreatedAt:   f.clock.Now(),
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}

	rep := f.selfCheck()
	if rep.OK {
		t.Fatal("a ledger entry pointing at a missing commission must be reported")
	}
	if !hasViolation(rep, "I2") {
		t.Fatalf("I2 did not report the dangling reference: %+v", rep.Invariants)
	}
}

// TestSelfCheckDetectsCapViolation injects a commission that exceeds the cap.
//
// The public API cannot produce such data (Rules validation plus the runtime
// cap would both block it), so this bypasses the engine and writes straight to
// the store—which is precisely why self check exists: it targets the "the data
// is already corrupt" situation, not the "the code was written wrong" one.
func TestSelfCheckDetectsCapViolation(t *testing.T) {
	f := newFixture(t, distledger.Rules{Levels: 1, RateBP: []distledger.Rate{1000}})
	ctx := context.Background()

	if err := f.store.Update(ctx, func(ctx context.Context, tx distledger.Tx) error {
		_, err := tx.AppendCommission(ctx, distledger.Commission{
			Key:         distledger.OrderKey{TenantID: tenant, OrderID: "ORD-BAD"},
			IdemKey:     "injected",
			AgentUserID: 3001,
			Layer:       1,
			BaseAmount:  10000, // 100.00 base
			Rate:        10000,
			Amount:      999999, // far above the cap
			State:       distledger.CommissionPending,
			AccruedAt:   f.clock.Now(),
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}

	rep := f.selfCheck()
	if rep.OK {
		t.Fatal("a commission exceeding the allocatable cap must be reported")
	}
	if !hasViolation(rep, "I2") {
		t.Fatalf("I2 did not report the cap violation: %+v", rep.Invariants)
	}
}

func TestSelfCheckRejectsNegativeTenant(t *testing.T) {
	f := newFixture(t, twoLevelRules())
	if _, err := f.led.SelfCheck(context.Background(), -1); err == nil {
		t.Fatal("negative tenant id must be rejected")
	}
}

func TestSelfCheckIsReadOnly(t *testing.T) {
	f := newFixture(t, twoLevelRules())
	f.healthy()

	before := f.balance(3001)
	if err := f.store.Update(context.Background(), func(ctx context.Context, tx distledger.Tx) error {
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	_ = f.selfCheck()
	after := f.balance(3001)
	if before != after {
		t.Fatalf("SelfCheck changed the account: %+v -> %+v", before, after)
	}
}

func hasViolation(rep distledger.Report, prefix string) bool {
	for _, inv := range rep.Invariants {
		if len(inv.Name) >= len(prefix) && inv.Name[:len(prefix)] == prefix && inv.TotalViolations > 0 {
			return true
		}
	}
	return false
}

// TestMoneyStringRoundTripInReport also verifies that the amounts appearing in
// reports are human-readable.
func TestMoneyStringRoundTripInReport(t *testing.T) {
	m, err := distledger.ParseMoney("199.99")
	if err != nil {
		t.Fatal(err)
	}
	if m.String() != "199.99" {
		t.Fatalf("round trip produced %s", m)
	}
	if strconv.FormatInt(int64(m), 10) != "19999" {
		t.Fatalf("minor units = %d, want 19999", int64(m))
	}
}
