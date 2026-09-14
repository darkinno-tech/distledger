package memory

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/im10furry/distledger"
)

// This file covers the validation branches of the read and write paths. They
// take no part in money arithmetic, but they decide whether troubleshooting
// and reconciliation are even feasible: a query interface that cannot filter
// or page turns self-checks into full table scans, so once the data volume
// grows nobody dares to "run a self-check".

func withTx(t *testing.T, s *Store, fn func(ctx context.Context, tx distledger.Tx) error) {
	t.Helper()
	if err := s.Update(context.Background(), fn); err != nil {
		t.Fatalf("Update: %v", err)
	}
}

func withRead(t *testing.T, s *Store, fn func(ctx context.Context, r distledger.Reader) error) {
	t.Helper()
	if err := s.View(context.Background(), fn); err != nil {
		t.Fatalf("View: %v", err)
	}
}

func seedAgentIn(t *testing.T, s *Store, tenantID, userID int64) {
	t.Helper()
	withTx(t, s, func(ctx context.Context, tx distledger.Tx) error {
		_, err := tx.PutAgent(ctx, distledger.Agent{
			Key:      distledger.UserKey{TenantID: tenantID, UserID: userID},
			Status:   distledger.AgentActive,
			Depth:    1,
			JoinType: distledger.JoinFree,
		})
		return err
	})
}

func seedCommissionIn(t *testing.T, s *Store, tenantID int64, orderID string, agentID int64, state distledger.CommissionState) distledger.Commission {
	t.Helper()
	var out distledger.Commission
	withTx(t, s, func(ctx context.Context, tx distledger.Tx) error {
		var err error
		out, err = tx.AppendCommission(ctx, distledger.Commission{
			Key:         distledger.OrderKey{TenantID: tenantID, OrderID: orderID},
			IdemKey:     fmt.Sprintf("%d-%s", tenantID, orderID),
			AgentUserID: agentID,
			Layer:       1,
			BaseAmount:  10000,
			Rate:        500,
			Amount:      500,
			State:       state,
		})
		return err
	})
	return out
}

func TestStoreKind(t *testing.T) {
	if got := New().StoreKind(); got != "memory" {
		t.Fatalf("StoreKind = %q, want memory", got)
	}
}

func TestMissingEntitiesReturnNotFound(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	withRead(t, s, func(ctx context.Context, r distledger.Reader) error {
		if _, err := r.Agent(ctx, agentKey(404)); !errors.Is(err, distledger.ErrNotFound) {
			t.Errorf("Agent = %v, want ErrNotFound", err)
		}
		if _, err := r.Binding(ctx, agentKey(404)); !errors.Is(err, distledger.ErrNotFound) {
			t.Errorf("Binding = %v, want ErrNotFound", err)
		}
		if _, err := r.Commission(ctx, 404); !errors.Is(err, distledger.ErrNotFound) {
			t.Errorf("Commission = %v, want ErrNotFound", err)
		}
		return nil
	})

	err := s.Update(ctx, func(ctx context.Context, tx distledger.Tx) error {
		_, err := tx.SetCommissionAvailableAt(ctx, 404, time.Now(), 0)
		return err
	})
	if !errors.Is(err, distledger.ErrNotFound) {
		t.Errorf("SetCommissionAvailableAt = %v, want ErrNotFound", err)
	}

	err = s.Update(ctx, func(ctx context.Context, tx distledger.Tx) error {
		_, err := tx.TransitionCommission(ctx, 404,
			distledger.CommissionPending, distledger.CommissionSettled, 0)
		return err
	})
	if !errors.Is(err, distledger.ErrNotFound) {
		t.Errorf("TransitionCommission = %v, want ErrNotFound", err)
	}
}

func TestPutBindingValidation(t *testing.T) {
	s := newStore(t)
	cases := map[string]distledger.Binding{
		"bad buyer":  {Buyer: distledger.UserKey{TenantID: 0, UserID: 0}, AgentUserID: 1, Source: distledger.SourceLink},
		"bad agent":  {Buyer: agentKey(1), AgentUserID: 0, Source: distledger.SourceLink},
		"bad source": {Buyer: agentKey(1), AgentUserID: 2, Source: distledger.BindSource(99)},
	}
	for name, b := range cases {
		t.Run(name, func(t *testing.T) {
			err := s.Update(context.Background(), func(ctx context.Context, tx distledger.Tx) error {
				_, err := tx.PutBinding(ctx, b)
				return err
			})
			if !errors.Is(err, distledger.ErrInvalidArgument) {
				t.Fatalf("got %v, want ErrInvalidArgument", err)
			}
		})
	}
}

func TestPutAgentValidation(t *testing.T) {
	s := newStore(t)
	cases := map[string]distledger.Agent{
		"bad key":         {Key: distledger.UserKey{TenantID: 0, UserID: 0}},
		"negative parent": {Key: agentKey(1), ParentID: -1},
		"self parent":     {Key: agentKey(1), ParentID: 1},
		"bad status":      {Key: agentKey(1), Status: distledger.AgentStatus(9)},
		"bad join type":   {Key: agentKey(1), JoinType: distledger.JoinType(9)},
	}
	for name, a := range cases {
		t.Run(name, func(t *testing.T) {
			err := s.Update(context.Background(), func(ctx context.Context, tx distledger.Tx) error {
				_, err := tx.PutAgent(ctx, a)
				return err
			})
			if !errors.Is(err, distledger.ErrInvalidArgument) {
				t.Fatalf("got %v, want ErrInvalidArgument", err)
			}
		})
	}
}

func TestAppendCommissionValidation(t *testing.T) {
	s := newStore(t)
	base := distledger.Commission{
		Key:         distledger.OrderKey{TenantID: 0, OrderID: "ORD-1"},
		IdemKey:     "k",
		AgentUserID: 1, Layer: 1, BaseAmount: 100, Rate: 500, Amount: 5,
		State: distledger.CommissionPending,
	}
	cases := map[string]func(*distledger.Commission){
		"bad order":   func(c *distledger.Commission) { c.Key.OrderID = "" },
		"no idem key": func(c *distledger.Commission) { c.IdemKey = "" },
		"bad agent":   func(c *distledger.Commission) { c.AgentUserID = 0 },
		"layer zero":  func(c *distledger.Commission) { c.Layer = 0 },
		"layer deep":  func(c *distledger.Commission) { c.Layer = distledger.MaxLevels + 1 },
		"bad state":   func(c *distledger.Commission) { c.State = distledger.CommissionState(99) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			c := base
			mutate(&c)
			err := s.Update(context.Background(), func(ctx context.Context, tx distledger.Tx) error {
				_, err := tx.AppendCommission(ctx, c)
				return err
			})
			if !errors.Is(err, distledger.ErrInvalidArgument) {
				t.Fatalf("got %v, want ErrInvalidArgument", err)
			}
		})
	}
}

func TestAppendLedgerValidation(t *testing.T) {
	s := newStore(t)
	cases := map[string]distledger.LedgerEntry{
		"bad key":      {Key: distledger.UserKey{TenantID: 0, UserID: 0}, BizType: distledger.LedgerAccrue},
		"bad biz type": {Key: agentKey(1), BizType: distledger.LedgerBizType(0)},
		"long remark":  {Key: agentKey(1), BizType: distledger.LedgerAccrue, Remark: string(make([]byte, distledger.MaxRemarkLen+1))},
	}
	for name, e := range cases {
		t.Run(name, func(t *testing.T) {
			err := s.Update(context.Background(), func(ctx context.Context, tx distledger.Tx) error {
				_, err := tx.AppendLedger(ctx, e)
				return err
			})
			if !errors.Is(err, distledger.ErrInvalidArgument) {
				t.Fatalf("got %v, want ErrInvalidArgument", err)
			}
		})
	}
}

func TestCommissionsByTenantFiltersAndPaginates(t *testing.T) {
	s := newStore(t)
	for i := 1; i <= 5; i++ {
		seedCommissionIn(t, s, 1, fmt.Sprintf("ORD-%d", i), 100, distledger.CommissionPending)
	}
	seedCommissionIn(t, s, 2, "OTHER", 100, distledger.CommissionPending)

	withRead(t, s, func(ctx context.Context, r distledger.Reader) error {
		page, err := r.CommissionsByTenant(ctx, 1, distledger.Page{Limit: 2})
		if err != nil {
			return err
		}
		if len(page) != 2 {
			t.Fatalf("first page has %d rows, want 2", len(page))
		}

		next, err := r.CommissionsByTenant(ctx, 1, distledger.Page{AfterID: page[len(page)-1].ID, Limit: 10})
		if err != nil {
			return err
		}
		if len(next) != 3 {
			t.Fatalf("second page has %d rows, want 3", len(next))
		}
		for _, c := range next {
			if c.Key.TenantID != 1 {
				t.Errorf("tenant filter leaked: %+v", c.Key)
			}
		}

		empty, err := r.CommissionsByTenant(ctx, 9, distledger.Page{})
		if err != nil {
			return err
		}
		if len(empty) != 0 {
			t.Errorf("unknown tenant returned %d commissions", len(empty))
		}
		return nil
	})
}

func TestCommissionsByAgentFiltersStates(t *testing.T) {
	s := newStore(t)
	pending := seedCommissionIn(t, s, 1, "ORD-A", 100, distledger.CommissionPending)
	seedCommissionIn(t, s, 1, "ORD-B", 100, distledger.CommissionSettled)
	seedCommissionIn(t, s, 1, "ORD-C", 200, distledger.CommissionPending)

	agent1 := distledger.UserKey{TenantID: 1, UserID: 100}
	withRead(t, s, func(ctx context.Context, r distledger.Reader) error {
		all, err := r.CommissionsByAgent(ctx, agent1, distledger.CommissionQuery{})
		if err != nil {
			return err
		}
		if len(all) != 2 {
			t.Fatalf("agent index returned %d rows, want 2", len(all))
		}

		onlyPending, err := r.CommissionsByAgent(ctx, agent1, distledger.CommissionQuery{
			States: []distledger.CommissionState{distledger.CommissionPending},
		})
		if err != nil {
			return err
		}
		if len(onlyPending) != 1 || onlyPending[0].ID != pending.ID {
			t.Fatalf("state filter returned %+v", onlyPending)
		}

		after, err := r.CommissionsByAgent(ctx, agent1, distledger.CommissionQuery{
			Page: distledger.Page{AfterID: pending.ID},
		})
		if err != nil {
			return err
		}
		if len(after) != 1 || after[0].Key.OrderID != "ORD-B" {
			t.Fatalf("keyset filter returned %+v", after)
		}
		return nil
	})
}

func TestLedgerQueries(t *testing.T) {
	s := newStore(t)
	for i := 0; i < 4; i++ {
		withTx(t, s, func(ctx context.Context, tx distledger.Tx) error {
			_, err := tx.AppendLedger(ctx, distledger.LedgerEntry{
				Key: agentKey(100), BizType: distledger.LedgerAccrue, BizID: "b", DeltaFrozen: 1,
			})
			return err
		})
	}
	withTx(t, s, func(ctx context.Context, tx distledger.Tx) error {
		_, err := tx.AppendLedger(ctx, distledger.LedgerEntry{
			Key: agentKey(100), BizType: distledger.LedgerSettle, BizID: "c", DeltaAvailable: 1,
		})
		return err
	})
	withTx(t, s, func(ctx context.Context, tx distledger.Tx) error {
		_, err := tx.AppendLedger(ctx, distledger.LedgerEntry{
			Key: agentKey(200), BizType: distledger.LedgerAccrue, BizID: "d", DeltaFrozen: 1,
		})
		return err
	})

	withRead(t, s, func(ctx context.Context, r distledger.Reader) error {
		byUser, err := r.LedgerEntries(ctx, agentKey(100), distledger.LedgerQuery{})
		if err != nil {
			return err
		}
		if len(byUser) != 5 {
			t.Fatalf("user ledger has %d rows, want 5", len(byUser))
		}

		byType, err := r.LedgerEntries(ctx, agentKey(100), distledger.LedgerQuery{
			BizType: distledger.LedgerSettle,
		})
		if err != nil {
			return err
		}
		if len(byType) != 1 {
			t.Fatalf("type filter returned %d rows, want 1", len(byType))
		}

		byTenant, err := r.LedgerByTenant(ctx, 0, distledger.Page{Limit: 3})
		if err != nil {
			return err
		}
		if len(byTenant) != 3 {
			t.Fatalf("tenant ledger page has %d rows, want 3", len(byTenant))
		}

		rest, err := r.LedgerByTenant(ctx, 0, distledger.Page{AfterID: byTenant[len(byTenant)-1].ID, Limit: 10})
		if err != nil {
			return err
		}
		if len(rest) != 3 {
			t.Fatalf("tenant ledger second page has %d rows, want 3", len(rest))
		}

		if got, err := r.LedgerByTenant(ctx, 77, distledger.Page{}); err != nil || len(got) != 0 {
			t.Fatalf("unknown tenant ledger = %v, %v", got, err)
		}
		return nil
	})
}

func TestCountCommissionsByState(t *testing.T) {
	s := newStore(t)
	seedCommissionIn(t, s, 1, "A", 100, distledger.CommissionPending)
	seedCommissionIn(t, s, 1, "B", 100, distledger.CommissionPending)
	seedCommissionIn(t, s, 1, "C", 100, distledger.CommissionSettled)
	seedCommissionIn(t, s, 2, "D", 100, distledger.CommissionVoid)

	withRead(t, s, func(ctx context.Context, r distledger.Reader) error {
		counts, err := r.CountCommissionsByState(ctx, 1)
		if err != nil {
			return err
		}
		if counts[distledger.CommissionPending] != 2 {
			t.Errorf("pending = %d, want 2", counts[distledger.CommissionPending])
		}
		if counts[distledger.CommissionSettled] != 1 {
			t.Errorf("settled = %d, want 1", counts[distledger.CommissionSettled])
		}
		if counts[distledger.CommissionVoid] != 0 {
			t.Errorf("other tenant's rows leaked into the count: void = %d", counts[distledger.CommissionVoid])
		}
		return nil
	})
}

func TestDueCommissionsFiltering(t *testing.T) {
	s := newStore(t)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	mk := func(tenantID int64, orderID string, dueIn time.Duration, state distledger.CommissionState) distledger.Commission {
		c := seedCommissionIn(t, s, tenantID, orderID, 100, state)
		if state != distledger.CommissionPending {
			return c
		}
		withTx(t, s, func(ctx context.Context, tx distledger.Tx) error {
			cur, err := tx.Commission(ctx, c.ID)
			if err != nil {
				return err
			}
			_, err = tx.SetCommissionAvailableAt(ctx, c.ID, base.Add(dueIn), cur.Version)
			return err
		})
		return c
	}

	due := mk(1, "DUE", time.Hour, distledger.CommissionPending)
	mk(1, "FUTURE", 48*time.Hour, distledger.CommissionPending)
	// A due commission belonging to another tenant must never show up in this
	// tenant's results.
	mk(2, "OTHER-TENANT", time.Hour, distledger.CommissionPending)
	// A settled commission must not appear even when its due time has already
	// passed (it never entered the due index).
	mk(1, "SETTLED", time.Hour, distledger.CommissionSettled)

	withRead(t, s, func(ctx context.Context, r distledger.Reader) error {
		got, err := r.DueCommissions(ctx, 1, base.Add(2*time.Hour), 100)
		if err != nil {
			return err
		}
		var ids []int64
		for _, c := range got {
			ids = append(ids, c.ID)
			if c.State != distledger.CommissionPending {
				t.Errorf("non-pending commission returned: %+v", c)
			}
			if c.Key.TenantID != 1 {
				t.Errorf("tenant filter leaked: %+v", c.Key)
			}
		}
		if len(ids) == 0 {
			t.Fatal("expected at least one due commission")
		}
		for _, id := range ids {
			if id != due.ID {
				t.Fatalf("unexpected due commission %d (want only %d)", id, due.ID)
			}
		}

		// limit must take effect.
		limited, err := r.DueCommissions(ctx, 1, base.Add(2*time.Hour), 1)
		if err != nil {
			return err
		}
		if len(limited) != 1 {
			t.Fatalf("limit=1 returned %d rows", len(limited))
		}

		// With nothing due it must return empty promptly.
		none, err := r.DueCommissions(ctx, 1, base.Add(-time.Hour), 100)
		if err != nil {
			return err
		}
		if len(none) != 0 {
			t.Fatalf("expected no due commissions, got %d", len(none))
		}
		return nil
	})
}

func TestDueCommissionsSkipsStaleIndexEntries(t *testing.T) {
	s := newStore(t)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	c := seedCommissionIn(t, s, 1, "ORD-1", 100, distledger.CommissionPending)
	withTx(t, s, func(ctx context.Context, tx distledger.Tx) error {
		_, err := tx.SetCommissionAvailableAt(ctx, c.ID, base, c.Version)
		return err
	})

	// Tamper with the commission state directly to fabricate a stale index
	// entry that claims the commission is due while it has actually settled.
	s.mu.Lock()
	stale := s.d.commissions[c.ID]
	stale.State = distledger.CommissionSettled
	s.d.commissions[c.ID] = stale
	s.mu.Unlock()

	withRead(t, s, func(ctx context.Context, r distledger.Reader) error {
		got, err := r.DueCommissions(ctx, 1, base.Add(time.Hour), 100)
		if err != nil {
			return err
		}
		if len(got) != 0 {
			t.Fatalf("stale index entry was returned as due: %+v", got)
		}
		return nil
	})
}

func TestAccountsByTenantPaginationAndFiltering(t *testing.T) {
	s := newStore(t)
	for _, u := range []int64{10, 20, 30} {
		withTx(t, s, func(ctx context.Context, tx distledger.Tx) error {
			_, err := tx.PutAccount(ctx, distledger.Account{
				Key: distledger.UserKey{TenantID: 1, UserID: u}, Available: 100,
			})
			return err
		})
	}
	withTx(t, s, func(ctx context.Context, tx distledger.Tx) error {
		_, err := tx.PutAccount(ctx, distledger.Account{
			Key: distledger.UserKey{TenantID: 2, UserID: 10}, Available: 100,
		})
		return err
	})

	withRead(t, s, func(ctx context.Context, r distledger.Reader) error {
		page, err := r.AccountsByTenant(ctx, 1, distledger.AccountPage{Limit: 2})
		if err != nil {
			return err
		}
		if len(page) != 2 || page[0].Key.UserID != 10 || page[1].Key.UserID != 20 {
			t.Fatalf("unexpected first page: %+v", page)
		}

		rest, err := r.AccountsByTenant(ctx, 1, distledger.AccountPage{AfterUserID: 20, Limit: 10})
		if err != nil {
			return err
		}
		if len(rest) != 1 || rest[0].Key.UserID != 30 {
			t.Fatalf("unexpected second page: %+v", rest)
		}
		for _, a := range append(page, rest...) {
			if a.Key.TenantID != 1 {
				t.Errorf("tenant filter leaked: %+v", a.Key)
			}
		}
		return nil
	})
}

func TestSetCommissionAvailableAtIsIdempotent(t *testing.T) {
	s := newStore(t)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c := seedCommissionIn(t, s, 1, "ORD-1", 100, distledger.CommissionPending)

	var first distledger.Commission
	withTx(t, s, func(ctx context.Context, tx distledger.Tx) error {
		var err error
		first, err = tx.SetCommissionAvailableAt(ctx, c.ID, base, c.Version)
		return err
	})
	if !first.AvailableAt.Equal(base) {
		t.Fatalf("AvailableAt = %s, want %s", first.AvailableAt, base)
	}

	// The second call passes a clearly earlier time and must be ignored
	// (first writer wins).
	withTx(t, s, func(ctx context.Context, tx distledger.Tx) error {
		second, err := tx.SetCommissionAvailableAt(ctx, c.ID, base.Add(-time.Hour), first.Version)
		if err != nil {
			return err
		}
		if !second.AvailableAt.Equal(base) {
			t.Errorf("AvailableAt was overwritten to %s", second.AvailableAt)
		}
		if second.Version != first.Version {
			t.Errorf("a no-op write must not bump the version: %d -> %d", first.Version, second.Version)
		}
		return nil
	})
}

func TestSetCommissionAvailableAtRejectsStaleVersion(t *testing.T) {
	s := newStore(t)
	c := seedCommissionIn(t, s, 1, "ORD-1", 100, distledger.CommissionPending)

	err := s.Update(context.Background(), func(ctx context.Context, tx distledger.Tx) error {
		_, err := tx.SetCommissionAvailableAt(ctx, c.ID, time.Now(), c.Version+5)
		return err
	})
	if !errors.Is(err, distledger.ErrConflict) {
		t.Fatalf("got %v, want ErrConflict", err)
	}
}

func TestPutBindingInsertAndUpdate(t *testing.T) {
	s := newStore(t)
	buyer := distledger.UserKey{TenantID: 1, UserID: 4001}
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	var created distledger.Binding
	withTx(t, s, func(ctx context.Context, tx distledger.Tx) error {
		var err error
		created, err = tx.PutBinding(ctx, distledger.Binding{
			Buyer: buyer, AgentUserID: 100, Source: distledger.SourceLink,
			BoundAt: base, Active: true,
		})
		return err
	})
	if created.Version != 1 {
		t.Fatalf("created binding version = %d, want 1", created.Version)
	}

	withRead(t, s, func(ctx context.Context, r distledger.Reader) error {
		got, err := r.Binding(ctx, buyer)
		if err != nil {
			return err
		}
		if got.AgentUserID != 100 || !got.Active {
			t.Fatalf("unexpected binding: %+v", got)
		}
		return nil
	})

	// Update: rebind to a different agent and record ReboundFrom for auditing.
	withTx(t, s, func(ctx context.Context, tx distledger.Tx) error {
		cur, err := tx.Binding(ctx, buyer)
		if err != nil {
			return err
		}
		cur.AgentUserID = 200
		cur.ReboundFrom = cur.ID
		_, err = tx.PutBinding(ctx, cur)
		return err
	})

	withRead(t, s, func(ctx context.Context, r distledger.Reader) error {
		got, err := r.Binding(ctx, buyer)
		if err != nil {
			return err
		}
		if got.AgentUserID != 200 || got.ReboundFrom != created.ID {
			t.Fatalf("rebinding was not recorded: %+v", got)
		}
		if got.Version != 2 {
			t.Fatalf("version = %d, want 2", got.Version)
		}
		return nil
	})

	// Writing with a stale version must fail.
	err := s.Update(context.Background(), func(ctx context.Context, tx distledger.Tx) error {
		_, err := tx.PutBinding(ctx, created)
		return err
	})
	if !errors.Is(err, distledger.ErrConflict) {
		t.Fatalf("stale binding write = %v, want ErrConflict", err)
	}
}

func TestRollbackRestoresBinding(t *testing.T) {
	s := newStore(t)
	buyer := distledger.UserKey{TenantID: 1, UserID: 4001}
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	withTx(t, s, func(ctx context.Context, tx distledger.Tx) error {
		_, err := tx.PutBinding(ctx, distledger.Binding{
			Buyer: buyer, AgentUserID: 100, Source: distledger.SourceLink,
			BoundAt: base, Active: true,
		})
		return err
	})

	err := s.Update(context.Background(), func(ctx context.Context, tx distledger.Tx) error {
		cur, err := tx.Binding(ctx, buyer)
		if err != nil {
			return err
		}
		cur.Active = false
		if _, err := tx.PutBinding(ctx, cur); err != nil {
			return err
		}
		return errors.New("boom")
	})
	if err == nil {
		t.Fatal("expected the transaction to fail")
	}

	withRead(t, s, func(ctx context.Context, r distledger.Reader) error {
		got, err := r.Binding(ctx, buyer)
		if err != nil {
			return err
		}
		if !got.Active {
			t.Fatal("binding change survived a rolled-back transaction")
		}
		return nil
	})
}

func TestRemovingMissingDueEntryIsHarmless(t *testing.T) {
	s := newStore(t)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	// Call the internal helper directly to verify that removing a non-existent
	// index entry does not corrupt the index.
	before := len(s.d.pendingDue)
	removeDue(s.d, newUndo(s.d), 0, base, 999)
	if len(s.d.pendingDue) != before {
		t.Fatalf("removing a missing entry changed the index length: %d -> %d",
			before, len(s.d.pendingDue))
	}

	// It must be safe with an empty index too.
	insertDue(s.d, newUndo(s.d), 0, base, 1)
	if len(s.d.pendingDue) != 1 {
		t.Fatalf("insert into an empty index failed: %+v", s.d.pendingDue)
	}
	removeDue(s.d, newUndo(s.d), 0, base, 2) // same due time but a different ID
	if len(s.d.pendingDue) != 1 {
		t.Fatal("removing a non-matching entry must be a no-op")
	}
	removeDue(s.d, newUndo(s.d), 0, base, 1)
	if len(s.d.pendingDue) != 0 {
		t.Fatal("removing the matching entry failed")
	}
}

// TestReaderMethodsHonourCancellation covers the cancellation check in every
// read method.
//
// These branches never execute on the normal path, but they decide how long a
// reconciliation scan takes to stop after a user hits cancel. Without them a
// cancellation request only takes effect once the whole scan has finished --
// on tens of millions of ledger rows that is tens of seconds.
func TestReaderMethodsHonourCancellation(t *testing.T) {
	s := newStore(t)
	seedAgentIn(t, s, 1, 100)
	seedCommissionIn(t, s, 1, "ORD-1", 100, distledger.CommissionPending)
	withTx(t, s, func(ctx context.Context, tx distledger.Tx) error {
		if _, err := tx.PutBinding(ctx, distledger.Binding{
			Buyer:       distledger.UserKey{TenantID: 1, UserID: 4001},
			AgentUserID: 100, Source: distledger.SourceLink, Active: true,
		}); err != nil {
			return err
		}
		_, err := tx.AppendLedger(ctx, distledger.LedgerEntry{
			Key:     distledger.UserKey{TenantID: 1, UserID: 100},
			BizType: distledger.LedgerAccrue, BizID: "1", DeltaFrozen: 1,
		})
		return err
	})

	user := distledger.UserKey{TenantID: 1, UserID: 100}
	calls := map[string]func(ctx context.Context, r distledger.Reader) error{
		"Agent": func(ctx context.Context, r distledger.Reader) error {
			_, err := r.Agent(ctx, user)
			return err
		},
		"Binding": func(ctx context.Context, r distledger.Reader) error {
			_, err := r.Binding(ctx, distledger.UserKey{TenantID: 1, UserID: 4001})
			return err
		},
		"Account": func(ctx context.Context, r distledger.Reader) error {
			_, err := r.Account(ctx, user)
			return err
		},
		"Commission": func(ctx context.Context, r distledger.Reader) error {
			_, err := r.Commission(ctx, 1)
			return err
		},
		"CommissionsByOrder": func(ctx context.Context, r distledger.Reader) error {
			_, err := r.CommissionsByOrder(ctx, distledger.OrderKey{TenantID: 1, OrderID: "ORD-1"})
			return err
		},
		"CommissionsByAgent": func(ctx context.Context, r distledger.Reader) error {
			_, err := r.CommissionsByAgent(ctx, user, distledger.CommissionQuery{})
			return err
		},
		"CommissionsByTenant": func(ctx context.Context, r distledger.Reader) error {
			_, err := r.CommissionsByTenant(ctx, 1, distledger.Page{})
			return err
		},
		"DueCommissions": func(ctx context.Context, r distledger.Reader) error {
			_, err := r.DueCommissions(ctx, 1, time.Now().Add(time.Hour), 10)
			return err
		},
		"LedgerEntries": func(ctx context.Context, r distledger.Reader) error {
			_, err := r.LedgerEntries(ctx, user, distledger.LedgerQuery{})
			return err
		},
		"LedgerByTenant": func(ctx context.Context, r distledger.Reader) error {
			_, err := r.LedgerByTenant(ctx, 1, distledger.Page{})
			return err
		},
		"AccountsByTenant": func(ctx context.Context, r distledger.Reader) error {
			_, err := r.AccountsByTenant(ctx, 1, distledger.AccountPage{})
			return err
		},
		"CountCommissionsByState": func(ctx context.Context, r distledger.Reader) error {
			_, err := r.CountCommissionsByState(ctx, 1)
			return err
		},
		"TenantsWithDueWork": func(ctx context.Context, r distledger.Reader) error {
			_, err := r.TenantsWithDueWork(ctx, time.Now().Add(time.Hour), 10)
			return err
		},
	}

	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			cancelled, cancel := context.WithCancel(context.Background())
			cancel()
			var inner error
			if err := s.View(context.Background(), func(_ context.Context, r distledger.Reader) error {
				inner = call(cancelled, r)
				return nil
			}); err != nil {
				t.Fatalf("View: %v", err)
			}
			if !errors.Is(inner, context.Canceled) {
				t.Fatalf("%s ignored cancellation: %v", name, inner)
			}
		})
	}
}

// TestTransactionTouchesSameEntityTwice covers the "record each object only
// once" branch of the undo log. It guarantees that after several modifications
// to the same object, a rollback restores the state from **before the
// transaction started**, not the state before the first modification.
func TestTransactionTouchesSameEntityTwice(t *testing.T) {
	s := newStore(t)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	key := distledger.UserKey{TenantID: 1, UserID: 100}

	seedAgentIn(t, s, 1, 100)
	withTx(t, s, func(ctx context.Context, tx distledger.Tx) error {
		_, err := tx.PutAccount(ctx, distledger.Account{Key: key, Available: 1000})
		return err
	})
	created := seedCommissionIn(t, s, 1, "ORD-1", 100, distledger.CommissionPending)

	err := s.Update(context.Background(), func(ctx context.Context, tx distledger.Tx) error {
		// The same account is modified twice.
		for i := 0; i < 2; i++ {
			acct, err := tx.Account(ctx, key)
			if err != nil {
				return err
			}
			acct.Key = key
			acct.Available += 100
			if _, err := tx.PutAccount(ctx, acct); err != nil {
				return err
			}
		}
		// The same commission is touched by two different write operations.
		cur, err := tx.Commission(ctx, created.ID)
		if err != nil {
			return err
		}
		updated, err := tx.SetCommissionAvailableAt(ctx, created.ID, base, cur.Version)
		if err != nil {
			return err
		}
		if _, err := tx.TransitionCommission(ctx, created.ID,
			distledger.CommissionPending, distledger.CommissionSettled, updated.Version); err != nil {
			return err
		}
		// The same agent is modified twice.
		agent, err := tx.Agent(ctx, key)
		if err != nil {
			return err
		}
		agent.Status = distledger.AgentDisabled
		if _, err := tx.PutAgent(ctx, agent); err != nil {
			return err
		}
		agent2, err := tx.Agent(ctx, key)
		if err != nil {
			return err
		}
		agent2.Status = distledger.AgentActive
		if _, err := tx.PutAgent(ctx, agent2); err != nil {
			return err
		}
		return errors.New("boom")
	})
	if err == nil {
		t.Fatal("expected the transaction to fail")
	}

	withRead(t, s, func(ctx context.Context, r distledger.Reader) error {
		acct, err := r.Account(ctx, key)
		if err != nil {
			return err
		}
		if acct.Available != 1000 {
			t.Errorf("account available = %s, want 1000 — rollback restored an intermediate state",
				acct.Available)
		}
		c, err := r.Commission(ctx, created.ID)
		if err != nil {
			return err
		}
		if c.State != distledger.CommissionPending || !c.AvailableAt.IsZero() {
			t.Errorf("commission was not restored: %+v", c)
		}
		a, err := r.Agent(ctx, key)
		if err != nil {
			return err
		}
		if a.Status != distledger.AgentActive {
			t.Errorf("agent status = %s, want active", a.Status)
		}
		return nil
	})
}

// TestMultipleDueAppendsInOneTransaction covers several tail appends to the
// due index within a single transaction: the length snapshot only needs to be
// recorded on the **first** append.
func TestMultipleDueAppendsInOneTransaction(t *testing.T) {
	s := newStore(t)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	a := seedCommissionIn(t, s, 1, "A", 100, distledger.CommissionPending)
	b := seedCommissionIn(t, s, 1, "B", 100, distledger.CommissionPending)

	withTx(t, s, func(ctx context.Context, tx distledger.Tx) error {
		ca, err := tx.Commission(ctx, a.ID)
		if err != nil {
			return err
		}
		if _, err := tx.SetCommissionAvailableAt(ctx, a.ID, base, ca.Version); err != nil {
			return err
		}
		cb, err := tx.Commission(ctx, b.ID)
		if err != nil {
			return err
		}
		_, err = tx.SetCommissionAvailableAt(ctx, b.ID, base.Add(time.Hour), cb.Version)
		return err
	})

	if len(s.d.pendingDue) != 2 {
		t.Fatalf("index has %d entries, want 2", len(s.d.pendingDue))
	}
}

// TestTenantsWithDueWorkReadsTheIndex pins the contract on
// Reader.TenantsWithDueWork, which replaced an earlier "list every tenant"
// method. The earlier shape made Maintain open one transaction per tenant per
// tick; this shape makes it proportional to the tenants that actually have work
// (ADR-035).
func TestTenantsWithDueWorkReadsTheIndex(t *testing.T) {
	s := newStore(t)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	// Three tenants get a settlement date; one commission never does.
	withDue := func(tenantID int64, orderID string, dueIn time.Duration) {
		t.Helper()
		c := seedCommissionIn(t, s, tenantID, orderID, 100, distledger.CommissionPending)
		withTx(t, s, func(ctx context.Context, tx distledger.Tx) error {
			cur, err := tx.Commission(ctx, c.ID)
			if err != nil {
				return err
			}
			_, err = tx.SetCommissionAvailableAt(ctx, c.ID, base.Add(dueIn), cur.Version)
			return err
		})
	}
	withDue(1, "ORD-A", -2*time.Hour)                                     // due two hours ago
	withDue(5, "ORD-B", -time.Hour)                                       // due one hour ago
	withDue(9, "ORD-C", 48*time.Hour)                                     // not due yet
	seedCommissionIn(t, s, 7, "ORD-D", 100, distledger.CommissionPending) // never received

	withRead(t, s, func(ctx context.Context, r distledger.Reader) error {
		got, err := r.TenantsWithDueWork(ctx, base, 100)
		if err != nil {
			return err
		}
		want := []int64{1, 5}
		if !slices.Equal(got, want) {
			t.Fatalf("TenantsWithDueWork = %v, want %v", got, want)
		}

		// The limit bounds the answer and the order stays ascending, so a caller
		// can resume deterministically.
		limited, err := r.TenantsWithDueWork(ctx, base, 1)
		if err != nil {
			return err
		}
		if !slices.Equal(limited, []int64{1}) {
			t.Fatalf("limited = %v, want [1]", limited)
		}

		// Nothing due anywhere is the common case and must cost nothing.
		none, err := r.TenantsWithDueWork(ctx, base.Add(-100*time.Hour), 100)
		if err != nil {
			return err
		}
		if len(none) != 0 {
			t.Fatalf("expected no tenants with work, got %v", none)
		}
		return nil
	})
}

// TestTenantsWithDueWorkIgnoresStaleIndexEntries keeps the stale-entry rule
// consistent with DueCommissions: an index entry that disagrees with the
// commission it points at must not create work.
func TestTenantsWithDueWorkIgnoresStaleIndexEntries(t *testing.T) {
	s := newStore(t)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	c := seedCommissionIn(t, s, 3, "ORD-1", 100, distledger.CommissionPending)
	withTx(t, s, func(ctx context.Context, tx distledger.Tx) error {
		_, err := tx.SetCommissionAvailableAt(ctx, c.ID, base, c.Version)
		return err
	})

	// Make the index entry stale without removing it, the way a crashed writer
	// would leave it.
	s.mu.Lock()
	stale := s.d.commissions[c.ID]
	stale.State = distledger.CommissionSettled
	s.d.commissions[c.ID] = stale
	s.mu.Unlock()

	withRead(t, s, func(ctx context.Context, r distledger.Reader) error {
		got, err := r.TenantsWithDueWork(ctx, base.Add(time.Hour), 100)
		if err != nil {
			return err
		}
		if len(got) != 0 {
			t.Fatalf("a stale index entry produced work for %v", got)
		}
		return nil
	})
}

// TestTenantsWithDueWorkSeesEveryTenantInABlock covers the binary search that
// skips from one tenant's entries to the next. A bug there would silently hide
// every tenant after the first, which is the kind of failure that looks like
// "settlement stopped for some tenants" months later.
func TestTenantsWithDueWorkSeesEveryTenantInABlock(t *testing.T) {
	s := newStore(t)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	// Several commissions per tenant, some due and some not, so each tenant's
	// block holds more than one entry and the skip has to be exact.
	for _, tenantID := range []int64{1, 2, 3, 4, 5} {
		for i, dueIn := range []time.Duration{-time.Hour, time.Hour, 48 * time.Hour} {
			c := seedCommissionIn(t, s, tenantID,
				fmt.Sprintf("ORD-%d-%d", tenantID, i), 100, distledger.CommissionPending)
			withTx(t, s, func(ctx context.Context, tx distledger.Tx) error {
				cur, err := tx.Commission(ctx, c.ID)
				if err != nil {
					return err
				}
				_, err = tx.SetCommissionAvailableAt(ctx, c.ID, base.Add(dueIn), cur.Version)
				return err
			})
		}
	}

	withRead(t, s, func(ctx context.Context, r distledger.Reader) error {
		got, err := r.TenantsWithDueWork(ctx, base, 100)
		if err != nil {
			return err
		}
		want := []int64{1, 2, 3, 4, 5}
		if !slices.Equal(got, want) {
			t.Fatalf("TenantsWithDueWork = %v, want %v", got, want)
		}
		return nil
	})
}
