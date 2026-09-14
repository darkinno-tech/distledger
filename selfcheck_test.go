package distledger_test

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/darkinno-tech/distledger"
	"github.com/darkinno-tech/distledger/store/memory"
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

// reconcilingStore wraps the in-memory store and adds the optional Reconciler
// capability, so that the wiring between SelfCheck and a reconciling store can be
// tested without a database.
type reconcilingStore struct {
	distledger.Store
	calls    int
	examined int
	// candidates are reported to the caller as if the store had evaluated them.
	candidates []distledger.AccountReconciliation
}

func (s *reconcilingStore) ReconcileAccounts(
	ctx context.Context,
	tenantID int64,
	limit int,
	fn func(distledger.AccountReconciliation) error,
) (int, error) {
	s.calls++
	for _, m := range s.candidates {
		if err := fn(m); err != nil {
			return 0, err
		}
	}
	return s.examined, nil
}

// newReconcilingFixture builds a fixture whose ledger talks to a store that can
// reconcile, with the reconciling store's answers under the test's control.
func newReconcilingFixture(
	t *testing.T, rules distledger.Rules, rec *reconcilingStore,
) *fixture {
	t.Helper()
	clock := distledger.NewManualClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	inner := memory.New()
	rec.Store = inner
	led, err := distledger.New(distledger.Config{
		Store: rec,
		Clock: clock,
		Rules: rules,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = led.Close() })
	// store stays the inner memory store: tests reach it directly to inject
	// states the engine would never produce.
	return &fixture{t: t, led: led, clock: clock, store: inner}
}

// TestSelfCheckDelegatesToAReconcilingStore pins the two things that make the
// delegation worth having.
//
// The first is that the store is asked at all. A store that can aggregate in
// place is the difference between a check whose cost tracks the number of ledger
// entries and one whose cost tracks the number of broken accounts, so silently
// falling back to the row-wise path would be a large and invisible regression.
//
// The second is that Checked reports the accounts examined rather than the
// candidates reported. A healthy tenant reports no candidates, so counting
// candidates would make "examined everything and found nothing wrong" look
// identical to "examined nothing" - the one distinction Checked exists to draw.
func TestSelfCheckDelegatesToAReconcilingStore(t *testing.T) {
	ctx := context.Background()
	rec := &reconcilingStore{examined: 7}
	f := newReconcilingFixture(t, twoLevelRules(), rec)
	f.healthy()

	rep, err := f.led.SelfCheck(ctx, tenant)
	if err != nil {
		t.Fatal(err)
	}
	if rec.calls != 1 {
		t.Fatalf("reconciler called %d times, want 1", rec.calls)
	}
	if !rep.Reconciled {
		t.Fatal("Report.Reconciled is false although the store reconciled")
	}
	if !rep.OK {
		t.Fatalf("healthy data reported problems: %+v", rep.Invariants)
	}

	i1, ok := invariantNamed(rep, "I1")
	if !ok {
		t.Fatalf("no I1 result in %+v", rep.Invariants)
	}
	if i1.Checked != 7 {
		t.Fatalf("I1 Checked = %d, want 7: the accounts examined, not the 0 candidates reported",
			i1.Checked)
	}
	if !i1.OK {
		t.Fatalf("I1 failed on healthy data: %+v", i1)
	}
}

// TestSelfCheckJudgesReconcilerCandidates checks that a candidate is still judged
// by the library rather than taken as a verdict.
//
// A reconciling store reports accounts it could not confirm; it does not get to
// decide they are wrong. If SelfCheck trusted the candidate list, then any store
// that over-reported - through a conservative filter, or a bug - would turn into
// false alarms, and the invariant would mean different things depending on which
// storage layer was underneath.
func TestSelfCheckJudgesReconcilerCandidates(t *testing.T) {
	ctx := context.Background()
	rec := &reconcilingStore{examined: 1}
	f := newReconcilingFixture(t, twoLevelRules(), rec)
	f.healthy()

	key := distledger.UserKey{TenantID: tenant, UserID: 3001}
	acct := readAccount(t, ctx, f.store, key)

	var summed distledger.Account
	for _, b := range distledger.AllBuckets() {
		b.Set(&summed, b.Value(acct))
	}
	rec.candidates = []distledger.AccountReconciliation{{
		Key: key, Stored: acct, Summed: summed,
		Tail: lastLedgerEntry(t, ctx, f.store, key), HasTail: true,
	}}

	// A candidate that is in fact perfectly consistent. Reporting it was allowed:
	// the store may be over-inclusive, because a false candidate costs a
	// comparison while a missed one costs an undetected loss.
	rep, err := f.led.SelfCheck(ctx, tenant)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.OK {
		t.Fatalf("a consistent account reported as a candidate was treated as a violation: %+v",
			rep.Invariants)
	}

	// The same candidate with a genuinely broken account row: the judgement must
	// go the other way.
	broken := acct
	broken.Available += 999
	rec.candidates[0].Stored = broken

	rep, err = f.led.SelfCheck(ctx, tenant)
	if err != nil {
		t.Fatal(err)
	}
	if rep.OK {
		t.Fatal("a broken account passed because the store called it a candidate rather than a violation")
	}
}

func invariantNamed(rep distledger.Report, prefix string) (distledger.InvariantResult, bool) {
	for _, inv := range rep.Invariants {
		if len(inv.Name) >= len(prefix) && inv.Name[:len(prefix)] == prefix {
			return inv, true
		}
	}
	return distledger.InvariantResult{}, false
}

func readAccount(t *testing.T, ctx context.Context, s distledger.Store, key distledger.UserKey) distledger.Account {
	t.Helper()
	var out distledger.Account
	if err := s.View(ctx, func(ctx context.Context, r distledger.Reader) error {
		var err error
		out, err = r.Account(ctx, key)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

func lastLedgerEntry(
	t *testing.T, ctx context.Context, s distledger.Store, key distledger.UserKey,
) distledger.LedgerEntry {
	t.Helper()
	var out distledger.LedgerEntry
	if err := s.View(ctx, func(ctx context.Context, r distledger.Reader) error {
		page, err := r.LedgerEntries(ctx, key, distledger.LedgerQuery{
			Page: distledger.Page{Limit: distledger.MaxPageLimit},
		})
		if err != nil {
			return err
		}
		if len(page) > 0 {
			out = page[len(page)-1]
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return out
}
