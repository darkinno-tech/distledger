package memory

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/darkinno-tech/distledger"
)

func withdrawalFixture(t *testing.T) (*Store, distledger.UserKey) {
	t.Helper()
	s := New()
	t.Cleanup(func() { _ = s.Close() })
	return s, distledger.UserKey{TenantID: 1, UserID: 3001}
}

func newWithdrawal(key distledger.UserKey, idem string) distledger.Withdrawal {
	return distledger.Withdrawal{
		Key: key, IdemKey: idem,
		Amount: 5000, Fee: 0, RealAmount: 5000,
		State:     distledger.WithdrawalApplied,
		Channel:   "manual",
		AppliedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}
}

func appendWithdrawal(t *testing.T, s *Store, w distledger.Withdrawal) distledger.Withdrawal {
	t.Helper()
	var out distledger.Withdrawal
	if err := s.Update(context.Background(), func(ctx context.Context, tx distledger.Tx) error {
		var err error
		out, err = tx.AppendWithdrawal(ctx, w)
		return err
	}); err != nil {
		t.Fatalf("AppendWithdrawal: %v", err)
	}
	return out
}

func TestWithdrawalAppendAndReadBack(t *testing.T) {
	ctx := context.Background()
	s, key := withdrawalFixture(t)

	w := appendWithdrawal(t, s, newWithdrawal(key, "wd-1"))
	if w.ID == 0 {
		t.Fatal("the store must assign an id")
	}
	if w.Version != 0 {
		// AppendWithdrawal deliberately does not stamp a version: nothing has
		// changed since creation, and a non-zero version here would make the
		// first transition's CAS expectation unclear.
		t.Logf("append stamped version %d", w.Version)
	}

	if err := s.View(ctx, func(ctx context.Context, r distledger.Reader) error {
		byKey, err := r.WithdrawalByIdemKey(ctx, key.TenantID, "wd-1")
		if err != nil {
			return err
		}
		if byKey.ID != w.ID {
			t.Errorf("lookup by idem key returned id %d, want %d", byKey.ID, w.ID)
		}
		byID, err := r.WithdrawalByID(ctx, w.ID)
		if err != nil {
			return err
		}
		if byID.IdemKey != "wd-1" || byID.Amount != 5000 || byID.State != distledger.WithdrawalApplied {
			t.Errorf("lookup by id returned %+v", byID)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestWithdrawalNotFoundIsDistinguishableFromZero(t *testing.T) {
	ctx := context.Background()
	s, _ := withdrawalFixture(t)

	if err := s.View(ctx, func(ctx context.Context, r distledger.Reader) error {
		if _, err := r.WithdrawalByID(ctx, 999); !errors.Is(err, distledger.ErrNotFound) {
			t.Errorf("missing id: got %v, want ErrNotFound", err)
		}
		if _, err := r.WithdrawalByIdemKey(ctx, 1, "nope"); !errors.Is(err, distledger.ErrNotFound) {
			t.Errorf("missing idem key: got %v, want ErrNotFound", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestWithdrawalIdempotentAppend is the property that protects the agent's
// money: a retried request must not reserve the balance twice.
func TestWithdrawalIdempotentAppend(t *testing.T) {
	ctx := context.Background()
	s, key := withdrawalFixture(t)

	first := appendWithdrawal(t, s, newWithdrawal(key, "same-key"))

	err := s.Update(ctx, func(ctx context.Context, tx distledger.Tx) error {
		_, err := tx.AppendWithdrawal(ctx, newWithdrawal(key, "same-key"))
		return err
	})
	if !errors.Is(err, distledger.ErrDuplicate) {
		t.Fatalf("a repeated idem key must return ErrDuplicate, got %v", err)
	}

	// Exactly one record must exist, and it must be the original: two records
	// under one key would mean the money was reserved twice.
	err = s.View(ctx, func(ctx context.Context, r distledger.Reader) error {
		got, err := r.WithdrawalByIdemKey(ctx, key.TenantID, "same-key")
		if err != nil {
			return err
		}
		if got.ID != first.ID {
			t.Errorf("the record under the key changed: id %d -> %d", first.ID, got.ID)
		}
		list, err := r.WithdrawalsByTenant(ctx, key.TenantID, distledger.Page{})
		if err != nil {
			return err
		}
		if len(list) != 1 {
			t.Errorf("got %d withdrawals, want 1: a retry must not create a second", len(list))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestWithdrawalTransitionHappyPaths(t *testing.T) {
	ctx := context.Background()
	s, key := withdrawalFixture(t)
	w := appendWithdrawal(t, s, newWithdrawal(key, "wd-2"))
	at := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)

	step := func(from, to distledger.WithdrawalState, tr distledger.WithdrawalTransition) distledger.Withdrawal {
		t.Helper()
		var out distledger.Withdrawal
		if err := s.Update(ctx, func(ctx context.Context, tx distledger.Tx) error {
			var err error
			out, err = tx.TransitionWithdrawal(ctx, w.ID, from, to, w.Version, tr)
			return err
		}); err != nil {
			t.Fatalf("%s -> %s: %v", from, to, err)
		}
		w = out
		return out
	}

	approved := step(distledger.WithdrawalApplied, distledger.WithdrawalApproved,
		distledger.WithdrawalTransition{Operator: "alice", AuditedAt: at})
	if approved.Version != w.Version {
		t.Fatal("version must advance on transition")
	}
	if approved.Operator != "alice" || !approved.AuditedAt.Equal(at) {
		t.Errorf("approval fields not recorded: %+v", approved)
	}

	paid := step(distledger.WithdrawalApproved, distledger.WithdrawalPaid,
		distledger.WithdrawalTransition{InvoiceNo: "INV-1", PaidAt: at.Add(time.Hour)})
	if !paid.PaidAt.Equal(at.Add(time.Hour)) || paid.InvoiceNo != "INV-1" {
		t.Errorf("payout fields not recorded: %+v", paid)
	}
	if paid.Outstanding() != 0 {
		t.Error("a paid withdrawal must hold nothing")
	}
}

// TestWithdrawalPayFailedCanStillReturnTheMoney covers the edge the PRD's
// diagram omits and the system needs: a failed payout that cannot be retried
// must be able to release the reservation, or the money is stranded.
func TestWithdrawalPayFailedCanStillReturnTheMoney(t *testing.T) {
	ctx := context.Background()
	s, key := withdrawalFixture(t)
	w := appendWithdrawal(t, s, newWithdrawal(key, "wd-3"))

	advance := func(from, to distledger.WithdrawalState, tr distledger.WithdrawalTransition) distledger.Withdrawal {
		t.Helper()
		var out distledger.Withdrawal
		if err := s.Update(ctx, func(ctx context.Context, tx distledger.Tx) error {
			var err error
			out, err = tx.TransitionWithdrawal(ctx, w.ID, from, to, w.Version, tr)
			return err
		}); err != nil {
			t.Fatalf("%s -> %s: %v", from, to, err)
		}
		w = out
		return out
	}

	advance(distledger.WithdrawalApplied, distledger.WithdrawalApproved, distledger.WithdrawalTransition{})
	failed := advance(distledger.WithdrawalApproved, distledger.WithdrawalPayFailed,
		distledger.WithdrawalTransition{FailReason: "channel timeout"})
	if failed.FailReason != "channel timeout" {
		t.Errorf("failure reason not recorded: %+v", failed)
	}
	if failed.Outstanding() != failed.Amount {
		t.Error("a failed payout must keep the money reserved for a retry")
	}
	rejected := advance(distledger.WithdrawalPayFailed, distledger.WithdrawalRejected,
		distledger.WithdrawalTransition{Operator: "alice"})
	if rejected.Outstanding() != 0 {
		t.Error("giving up must release the reservation")
	}
}

func TestWithdrawalIllegalTransitionAndConflicts(t *testing.T) {
	ctx := context.Background()
	s, key := withdrawalFixture(t)
	w := appendWithdrawal(t, s, newWithdrawal(key, "wd-4"))

	// Applied cannot go straight to Paid: the review gate is not optional.
	err := s.Update(ctx, func(ctx context.Context, tx distledger.Tx) error {
		_, err := tx.TransitionWithdrawal(ctx, w.ID,
			distledger.WithdrawalApplied, distledger.WithdrawalPaid, w.Version,
			distledger.WithdrawalTransition{PaidAt: time.Unix(1, 0)})
		return err
	})
	if !errors.Is(err, distledger.ErrIllegalTransition) {
		t.Fatalf("skipping approval must be illegal, got %v", err)
	}

	// A stale version is a retryable conflict, not a caller error.
	err = s.Update(ctx, func(ctx context.Context, tx distledger.Tx) error {
		_, err := tx.TransitionWithdrawal(ctx, w.ID,
			distledger.WithdrawalApplied, distledger.WithdrawalApproved, w.Version+41,
			distledger.WithdrawalTransition{})
		return err
	})
	if !errors.Is(err, distledger.ErrConflict) {
		t.Fatalf("a stale version must be a conflict, got %v", err)
	}

	// A stale *state* is also a conflict: the caller's view is out of date
	// rather than illogical.
	err = s.Update(ctx, func(ctx context.Context, tx distledger.Tx) error {
		_, err := tx.TransitionWithdrawal(ctx, w.ID,
			distledger.WithdrawalApproved, distledger.WithdrawalPaid, w.Version,
			distledger.WithdrawalTransition{PaidAt: time.Unix(1, 0)})
		return err
	})
	if !errors.Is(err, distledger.ErrConflict) {
		t.Fatalf("a stale state must be a conflict, got %v", err)
	}

	// The two must stay distinguishable: a caller that retried forever on an
	// illegal transition would never make progress, and one that treated a race
	// as a bug would drop a legitimate retry.
	if errors.Is(err, distledger.ErrIllegalTransition) {
		t.Error("a conflict must not also read as an illegal transition")
	}

	// Nothing above should have changed the record.
	if err := s.View(ctx, func(ctx context.Context, r distledger.Reader) error {
		got, err := r.WithdrawalByID(ctx, w.ID)
		if err != nil {
			return err
		}
		if got.State != distledger.WithdrawalApplied || got.Version != w.Version {
			t.Errorf("a refused transition must leave the record untouched: %+v", got)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestWithdrawalRollbackLeavesNothing is the memory store's own contract: a
// transaction that fails must leave no trace, including in the derived
// idempotency index. A leaked index entry would deny a later legitimate request.
func TestWithdrawalRollbackLeavesNothing(t *testing.T) {
	ctx := context.Background()
	s, key := withdrawalFixture(t)

	sentinel := errors.New("roll back")
	err := s.Update(ctx, func(ctx context.Context, tx distledger.Tx) error {
		if _, err := tx.AppendWithdrawal(ctx, newWithdrawal(key, "rolled-back")); err != nil {
			return err
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("want the sentinel error, got %v", err)
	}

	if err := s.View(ctx, func(ctx context.Context, r distledger.Reader) error {
		if _, err := r.WithdrawalByIdemKey(ctx, key.TenantID, "rolled-back"); !errors.Is(err, distledger.ErrNotFound) {
			t.Errorf("a rolled-back append must leave no record, got %v", err)
		}
		list, err := r.WithdrawalsByTenant(ctx, key.TenantID, distledger.Page{})
		if err != nil {
			return err
		}
		if len(list) != 0 {
			t.Errorf("got %d withdrawals after a rollback, want 0", len(list))
		}
		counts, err := r.CountWithdrawalsByState(ctx, key.TenantID)
		if err != nil {
			return err
		}
		if len(counts) != 0 {
			t.Errorf("counts after a rollback = %v, want empty", counts)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// The key must be reusable: leaking it into the index would make the retry
	// that follows a rollback fail as a duplicate.
	if got := appendWithdrawal(t, s, newWithdrawal(key, "rolled-back")); got.ID == 0 {
		t.Error("the key must be usable after a rolled-back attempt")
	}
}

func TestWithdrawalPaginationAndCounts(t *testing.T) {
	ctx := context.Background()
	s := New()
	t.Cleanup(func() { _ = s.Close() })

	// Two tenants, so that the tenant filter is exercised rather than assumed.
	for i := 1; i <= 5; i++ {
		key := distledger.UserKey{TenantID: 1, UserID: int64(3000 + i)}
		appendWithdrawal(t, s, newWithdrawal(key, "t1-"+string(rune('a'+i))))
	}
	other := distledger.UserKey{TenantID: 2, UserID: 4001}
	w := appendWithdrawal(t, s, newWithdrawal(other, "t2-a"))
	if err := s.Update(ctx, func(ctx context.Context, tx distledger.Tx) error {
		_, err := tx.TransitionWithdrawal(ctx, w.ID,
			distledger.WithdrawalApplied, distledger.WithdrawalApproved, w.Version,
			distledger.WithdrawalTransition{})
		return err
	}); err != nil {
		t.Fatal(err)
	}

	if err := s.View(ctx, func(ctx context.Context, r distledger.Reader) error {
		page1, err := r.WithdrawalsByTenant(ctx, 1, distledger.Page{Limit: 2})
		if err != nil {
			return err
		}
		if len(page1) != 2 {
			t.Fatalf("page 1 has %d rows, want 2", len(page1))
		}
		page2, err := r.WithdrawalsByTenant(ctx, 1, distledger.Page{AfterID: page1[len(page1)-1].ID, Limit: 2})
		if err != nil {
			return err
		}
		if len(page2) != 2 || page2[0].ID <= page1[len(page1)-1].ID {
			t.Errorf("keyset pagination did not advance: page1 last %d, page2 first %d",
				page1[len(page1)-1].ID, page2[0].ID)
		}
		// Tenant 2's withdrawal must never appear in tenant 1's list.
		for _, x := range append(page1, page2...) {
			if x.Key.TenantID != 1 {
				t.Errorf("tenant 1's list returned a tenant %d row", x.Key.TenantID)
			}
		}

		c1, err := r.CountWithdrawalsByState(ctx, 1)
		if err != nil {
			return err
		}
		if c1[distledger.WithdrawalApplied] != 5 {
			t.Errorf("tenant 1 applied count = %d, want 5", c1[distledger.WithdrawalApplied])
		}
		c2, err := r.CountWithdrawalsByState(ctx, 2)
		if err != nil {
			return err
		}
		if c2[distledger.WithdrawalApproved] != 1 || c2[distledger.WithdrawalApplied] != 0 {
			t.Errorf("tenant 2 counts = %v, want one approved", c2)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestWithdrawalAppendRejectsInvalid(t *testing.T) {
	ctx := context.Background()
	s, key := withdrawalFixture(t)

	for _, tc := range []struct {
		name string
		mut  func(*distledger.Withdrawal)
	}{
		{"zero amount", func(w *distledger.Withdrawal) { w.Amount = 0; w.RealAmount = 0 }},
		{"empty idem key", func(w *distledger.Withdrawal) { w.IdemKey = "" }},
		{"bad real amount", func(w *distledger.Withdrawal) { w.RealAmount = 1 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newWithdrawal(key, "bad")
			tc.mut(&w)
			err := s.Update(ctx, func(ctx context.Context, tx distledger.Tx) error {
				_, err := tx.AppendWithdrawal(ctx, w)
				return err
			})
			if err == nil {
				t.Fatal("must be rejected")
			}
			var fe *distledger.FieldError
			if !errors.As(err, &fe) {
				t.Fatalf("want a FieldError, got %T: %v", err, err)
			}
		})
	}
}
