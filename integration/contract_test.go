package integration

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/im10furry/distledger"
)

// The contract suite.
//
// Every assertion here runs against every backend. A backend that disagrees with
// the others fails, which is the point: the port is supposed to describe one
// behaviour, and the only way to know it does is to ask all of them the same
// questions.

var epoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func TestAgentRoundTripAndVersioning(t *testing.T) {
	for _, b := range backends(t) {
		t.Run(b.name, func(t *testing.T) {
			ctx := context.Background()
			s := b.open(t)

			created := seedAgent(t, ctx, s, 100, 0)
			if created.Version != 1 {
				t.Fatalf("new agent version = %d, want 1", created.Version)
			}

			got := read(t, ctx, s, func(ctx context.Context, r distledger.Reader) (distledger.Agent, error) {
				return r.Agent(ctx, agentKey(100))
			})
			if got.Key != created.Key || got.Status != distledger.AgentActive || got.Depth != 1 {
				t.Fatalf("round trip lost fields: %+v -> %+v", created, got)
			}

			// A stale version must be refused rather than silently winning. Two
			// concurrent writers are the normal case in a ledger, and last-write
			// -wins here would erase whatever the other one recorded.
			err := s.Update(ctx, func(ctx context.Context, tx distledger.Tx) error {
				stale := created
				stale.Version = 0
				_, err := tx.PutAgent(ctx, stale)
				return err
			})
			if !errors.Is(err, distledger.ErrConflict) {
				t.Fatalf("stale write = %v, want ErrConflict", err)
			}

			// With the current version it must succeed and advance the version.
			updated := read(t, ctx, s, func(ctx context.Context, r distledger.Reader) (distledger.Agent, error) {
				return r.Agent(ctx, agentKey(100))
			})
			updated.Status = distledger.AgentDisabled
			var after distledger.Agent
			if err := s.Update(ctx, func(ctx context.Context, tx distledger.Tx) error {
				var err error
				after, err = tx.PutAgent(ctx, updated)
				return err
			}); err != nil {
				t.Fatalf("fresh write: %v", err)
			}
			if after.Version != updated.Version+1 {
				t.Fatalf("version = %d, want %d", after.Version, updated.Version+1)
			}
			if after.Status != distledger.AgentDisabled {
				t.Fatalf("status = %s, want disabled", after.Status)
			}
		})
	}
}

func TestMissingEntitiesAreNotFound(t *testing.T) {
	for _, b := range backends(t) {
		t.Run(b.name, func(t *testing.T) {
			ctx := context.Background()
			s := b.open(t)

			// Agent and binding have no "zero" reading, so absence must be
			// reported. Account is the opposite and is checked below.
			if _, err := read2(t, ctx, s, func(ctx context.Context, r distledger.Reader) (distledger.Agent, error) {
				return r.Agent(ctx, agentKey(404))
			}); !errors.Is(err, distledger.ErrNotFound) {
				t.Fatalf("missing agent = %v, want ErrNotFound", err)
			}
			if _, err := read2(t, ctx, s, func(ctx context.Context, r distledger.Reader) (distledger.Binding, error) {
				return r.Binding(ctx, agentKey(404))
			}); !errors.Is(err, distledger.ErrNotFound) {
				t.Fatalf("missing binding = %v, want ErrNotFound", err)
			}
			if _, err := read2(t, ctx, s, func(ctx context.Context, r distledger.Reader) (distledger.Commission, error) {
				return r.Commission(ctx, 404)
			}); !errors.Is(err, distledger.ErrNotFound) {
				t.Fatalf("missing commission = %v, want ErrNotFound", err)
			}

			// A user who never moved money has the zero account, not an error.
			acct, err := read2(t, ctx, s, func(ctx context.Context, r distledger.Reader) (distledger.Account, error) {
				return r.Account(ctx, agentKey(404))
			})
			if err != nil {
				t.Fatalf("missing account = %v, want the zero account", err)
			}
			if acct.Frozen != 0 || acct.Available != 0 || acct.Version != 0 {
				t.Fatalf("missing account = %+v, want all zero", acct)
			}
		})
	}
}

func TestCommissionIdempotencyIsDuplicateNotFailure(t *testing.T) {
	for _, b := range backends(t) {
		t.Run(b.name, func(t *testing.T) {
			ctx := context.Background()
			s := b.open(t)
			first := seedCommission(t, ctx, s, "ORD-1", 100)

			// The unique key on (tenant, idem_key) is the entire basis for
			// idempotency, so a second insert with the same key must be
			// recognisable as a duplicate rather than surfacing as a driver error.
			err := s.Update(ctx, func(ctx context.Context, tx distledger.Tx) error {
				_, err := tx.AppendCommission(ctx, distledger.Commission{
					Key:         orderKey("ORD-1"),
					IdemKey:     "idem-ORD-1",
					BuyerUserID: 9000,
					AgentUserID: 100,
					Layer:       1,
					BaseAmount:  10000,
					Rate:        500,
					Amount:      500,
					State:       distledger.CommissionPending,
					AccruedAt:   epoch,
				})
				return err
			})
			if !errors.Is(err, distledger.ErrDuplicate) {
				t.Fatalf("duplicate insert = %v, want ErrDuplicate", err)
			}

			list := read(t, ctx, s, func(ctx context.Context, r distledger.Reader) ([]distledger.Commission, error) {
				return r.CommissionsByOrder(ctx, orderKey("ORD-1"))
			})
			if len(list) != 1 || list[0].ID != first.ID {
				t.Fatalf("duplicate insert changed the rows: %+v", list)
			}
		})
	}
}

func TestAvailableAtIsFirstWriterWins(t *testing.T) {
	for _, b := range backends(t) {
		t.Run(b.name, func(t *testing.T) {
			ctx := context.Background()
			s := b.open(t)
			c := seedCommission(t, ctx, s, "ORD-1", 100)

			settleAt := epoch.Add(7 * 24 * time.Hour)
			setAvailable := func(at time.Time, version int64) (distledger.Commission, error) {
				var out distledger.Commission
				err := s.Update(ctx, func(ctx context.Context, tx distledger.Tx) error {
					var err error
					out, err = tx.SetCommissionAvailableAt(ctx, c.ID, at, version)
					return err
				})
				return out, err
			}

			first, err := setAvailable(settleAt, c.Version)
			if err != nil {
				t.Fatalf("first write: %v", err)
			}
			if !first.AvailableAt.Equal(settleAt) {
				t.Fatalf("available at = %s, want %s", first.AvailableAt, settleAt)
			}

			// A second write must be a no-op, not an error and not an overwrite.
			// Overwriting is what would let a redelivered receipt pull a payout
			// forward.
			second, err := setAvailable(settleAt.Add(-30*24*time.Hour), first.Version)
			if err != nil {
				t.Fatalf("second write: %v", err)
			}
			if !second.AvailableAt.Equal(settleAt) {
				t.Fatalf("available at = %s, want it pinned at %s", second.AvailableAt, settleAt)
			}
			if second.Version != first.Version {
				t.Fatalf("a no-op write changed the version: %d -> %d", first.Version, second.Version)
			}
		})
	}
}

func TestTransitionGuards(t *testing.T) {
	for _, b := range backends(t) {
		t.Run(b.name, func(t *testing.T) {
			ctx := context.Background()
			s := b.open(t)
			c := seedCommission(t, ctx, s, "ORD-1", 100)

			transition := func(from, to distledger.CommissionState, version int64) error {
				return s.Update(ctx, func(ctx context.Context, tx distledger.Tx) error {
					_, err := tx.TransitionCommission(ctx, c.ID, from, to, version)
					return err
				})
			}

			// Pending -> Withdrawn skips a step and must be refused as illegal.
			if err := transition(distledger.CommissionPending, distledger.CommissionWithdrawn, c.Version); !errors.Is(err, distledger.ErrIllegalTransition) {
				t.Fatalf("illegal transition = %v, want ErrIllegalTransition", err)
			}
			// Claiming the wrong current state is a conflict, not an illegal
			// transition: the two must stay distinguishable because only one of
			// them is retryable.
			if err := transition(distledger.CommissionSettled, distledger.CommissionWithdrawn, c.Version); !errors.Is(err, distledger.ErrConflict) {
				t.Fatalf("wrong current state = %v, want ErrConflict", err)
			}
			// The legal one succeeds.
			var after distledger.Commission
			if err := s.Update(ctx, func(ctx context.Context, tx distledger.Tx) error {
				var err error
				after, err = tx.TransitionCommission(ctx, c.ID, distledger.CommissionPending, distledger.CommissionSettled, c.Version)
				return err
			}); err != nil {
				t.Fatalf("legal transition: %v", err)
			}
			if after.State != distledger.CommissionSettled {
				t.Fatalf("state = %s, want settled", after.State)
			}
		})
	}
}

func TestReversedAmountBounds(t *testing.T) {
	for _, b := range backends(t) {
		t.Run(b.name, func(t *testing.T) {
			ctx := context.Background()
			s := b.open(t)
			c := seedCommission(t, ctx, s, "ORD-1", 100)

			set := func(v distledger.Money, version int64) error {
				return s.Update(ctx, func(ctx context.Context, tx distledger.Tx) error {
					_, err := tx.SetCommissionReversedAmount(ctx, c.ID, v, version)
					return err
				})
			}
			// Beyond the amount accrued: refused, because letting the accumulator
			// exceed the amount is what invariant I3 exists to catch.
			if err := set(c.Amount+1, c.Version); !errors.Is(err, distledger.ErrInvalidArgument) {
				t.Fatalf("over-reversal = %v, want ErrInvalidArgument", err)
			}
			if err := set(-1, c.Version); !errors.Is(err, distledger.ErrInvalidArgument) {
				t.Fatalf("negative reversal = %v, want ErrInvalidArgument", err)
			}

			var after distledger.Commission
			if err := s.Update(ctx, func(ctx context.Context, tx distledger.Tx) error {
				var err error
				after, err = tx.SetCommissionReversedAmount(ctx, c.ID, 200, c.Version)
				return err
			}); err != nil {
				t.Fatalf("in-range reversal: %v", err)
			}
			if after.ReversedAmount != 200 {
				t.Fatalf("reversed = %s, want 2.00", after.ReversedAmount)
			}
		})
	}
}

func TestKeysetPagination(t *testing.T) {
	for _, b := range backends(t) {
		t.Run(b.name, func(t *testing.T) {
			ctx := context.Background()
			s := b.open(t)
			for i := 1; i <= 5; i++ {
				seedCommission(t, ctx, s, orderID(i), 100)
			}
			// A second tenant's rows must never appear in the first tenant's pages.
			if err := s.Update(ctx, func(ctx context.Context, tx distledger.Tx) error {
				_, err := tx.AppendCommission(ctx, distledger.Commission{
					Key:         distledger.OrderKey{TenantID: 2, OrderID: "OTHER"},
					IdemKey:     "other",
					BuyerUserID: 1, AgentUserID: 1, Layer: 1,
					BaseAmount: 100, Rate: 500, Amount: 5,
					State:     distledger.CommissionPending,
					AccruedAt: epoch,
				})
				return err
			}); err != nil {
				t.Fatal(err)
			}

			var seen []int64
			var afterID int64
			for page := 0; page < 10; page++ {
				batch := read(t, ctx, s, func(ctx context.Context, r distledger.Reader) ([]distledger.Commission, error) {
					return r.CommissionsByTenant(ctx, testTenant, distledger.Page{AfterID: afterID, Limit: 2})
				})
				if len(batch) == 0 {
					break
				}
				for _, c := range batch {
					if c.Key.TenantID != testTenant {
						t.Fatalf("tenant filter leaked: %+v", c.Key)
					}
					seen = append(seen, c.ID)
					afterID = c.ID
				}
			}
			if len(seen) != 5 {
				t.Fatalf("read %d rows over pages, want 5: %v", len(seen), seen)
			}
			if !slices.IsSorted(seen) {
				t.Fatalf("keyset pagination returned rows out of order: %v", seen)
			}
		})
	}
}

func TestDueCommissionsAndTenantsWithWork(t *testing.T) {
	for _, b := range backends(t) {
		t.Run(b.name, func(t *testing.T) {
			ctx := context.Background()
			s := b.open(t)

			// Two tenants with work at different times, one without.
			mk := func(tenantID int64, orderID string, dueIn time.Duration, receive bool) {
				t.Helper()
				var c distledger.Commission
				if err := s.Update(ctx, func(ctx context.Context, tx distledger.Tx) error {
					var err error
					c, err = tx.AppendCommission(ctx, distledger.Commission{
						Key:         distledger.OrderKey{TenantID: tenantID, OrderID: orderID},
						IdemKey:     orderID,
						BuyerUserID: 1, AgentUserID: 1, Layer: 1,
						BaseAmount: 100, Rate: 500, Amount: 5,
						State:     distledger.CommissionPending,
						AccruedAt: epoch,
					})
					return err
				}); err != nil {
					t.Fatal(err)
				}
				if !receive {
					return
				}
				if err := s.Update(ctx, func(ctx context.Context, tx distledger.Tx) error {
					_, err := tx.SetCommissionAvailableAt(ctx, c.ID, epoch.Add(dueIn), c.Version)
					return err
				}); err != nil {
					t.Fatal(err)
				}
			}
			mk(1, "A", -2*time.Hour, true)
			mk(5, "B", -time.Hour, true)
			mk(7, "C", 48*time.Hour, true) // not due yet
			mk(9, "D", 0, false)           // never received

			tenants := read(t, ctx, s, func(ctx context.Context, r distledger.Reader) ([]int64, error) {
				return r.TenantsWithDueWork(ctx, epoch, 100)
			})
			if !slices.Equal(tenants, []int64{1, 5}) {
				t.Fatalf("TenantsWithDueWork = %v, want [1 5]", tenants)
			}

			// The limit must bound the answer.
			limited := read(t, ctx, s, func(ctx context.Context, r distledger.Reader) ([]int64, error) {
				return r.TenantsWithDueWork(ctx, epoch, 1)
			})
			if !slices.Equal(limited, []int64{1}) {
				t.Fatalf("limited = %v, want [1]", limited)
			}

			due := read(t, ctx, s, func(ctx context.Context, r distledger.Reader) ([]distledger.Commission, error) {
				return r.DueCommissions(ctx, 1, epoch, 10)
			})
			if len(due) != 1 || due[0].Key.OrderID != "A" {
				t.Fatalf("DueCommissions = %+v, want just order A", due)
			}

			// Nothing due anywhere is the common case for the heartbeat.
			none := read(t, ctx, s, func(ctx context.Context, r distledger.Reader) ([]int64, error) {
				return r.TenantsWithDueWork(ctx, epoch.Add(-100*time.Hour), 100)
			})
			if len(none) != 0 {
				t.Fatalf("expected no work, got %v", none)
			}
		})
	}
}

func TestRefundVoucherAndDuplicate(t *testing.T) {
	for _, b := range backends(t) {
		t.Run(b.name, func(t *testing.T) {
			ctx := context.Background()
			s := b.open(t)
			seedCommission(t, ctx, s, "ORD-1", 100)

			appendRefund := func(idem string) error {
				return s.Update(ctx, func(ctx context.Context, tx distledger.Tx) error {
					_, err := tx.AppendRefund(ctx, distledger.Refund{
						Key: orderKey("ORD-1"), ItemID: "SKU-A", Amount: 100,
						Cumulative: 100, IdemKey: idem, RefundedAt: epoch,
					})
					return err
				})
			}
			if err := appendRefund("r1"); err != nil {
				t.Fatalf("first refund: %v", err)
			}
			// The voucher's unique key is what distinguishes a retry from a
			// second genuine refund of the same size.
			if err := appendRefund("r1"); !errors.Is(err, distledger.ErrDuplicate) {
				t.Fatalf("duplicate refund = %v, want ErrDuplicate", err)
			}
			if err := appendRefund("r2"); err != nil {
				t.Fatalf("second instalment: %v", err)
			}

			got := read(t, ctx, s, func(ctx context.Context, r distledger.Reader) ([]distledger.Refund, error) {
				return r.RefundsByOrder(ctx, orderKey("ORD-1"))
			})
			if len(got) != 2 {
				t.Fatalf("got %d vouchers, want 2", len(got))
			}
		})
	}
}

func TestCountsAndLedger(t *testing.T) {
	for _, b := range backends(t) {
		t.Run(b.name, func(t *testing.T) {
			ctx := context.Background()
			s := b.open(t)
			seedCommission(t, ctx, s, "ORD-1", 100)
			seedCommission(t, ctx, s, "ORD-2", 100)

			counts := read(t, ctx, s, func(ctx context.Context, r distledger.Reader) (map[distledger.CommissionState]int64, error) {
				return r.CountCommissionsByState(ctx, testTenant)
			})
			if counts[distledger.CommissionPending] != 2 {
				t.Fatalf("pending count = %d, want 2", counts[distledger.CommissionPending])
			}
			// Another tenant must not be counted.
			other := read(t, ctx, s, func(ctx context.Context, r distledger.Reader) (map[distledger.CommissionState]int64, error) {
				return r.CountCommissionsByState(ctx, 99)
			})
			if len(other) != 0 {
				t.Fatalf("other tenant has counts: %v", other)
			}

			var entry distledger.LedgerEntry
			if err := s.Update(ctx, func(ctx context.Context, tx distledger.Tx) error {
				var err error
				entry, err = tx.AppendLedger(ctx, distledger.LedgerEntry{
					Key: agentKey(100), BizType: distledger.LedgerAccrue,
					BizID: "1", DeltaFrozen: 500, AfterFrozen: 500, CreatedAt: epoch,
				})
				return err
			}); err != nil {
				t.Fatalf("append ledger: %v", err)
			}
			if entry.ID == 0 {
				t.Fatal("ledger entry got no id from the database")
			}

			entries := read(t, ctx, s, func(ctx context.Context, r distledger.Reader) ([]distledger.LedgerEntry, error) {
				return r.LedgerEntries(ctx, agentKey(100), distledger.LedgerQuery{})
			})
			if len(entries) != 1 || entries[0].DeltaFrozen != 500 {
				t.Fatalf("ledger round trip: %+v", entries)
			}
		})
	}
}

// TestTransactionRollbackIsComplete is the property the engine depends on.
//
// The retry contract in distledger.Store says a retry rolls back completely and
// re-runs against the pre-attempt state. That is only sound if a rollback really
// leaves nothing behind, which is what this checks on a real database rather than
// in memory (ADR-030).
func TestTransactionRollbackIsComplete(t *testing.T) {
	for _, b := range backends(t) {
		t.Run(b.name, func(t *testing.T) {
			ctx := context.Background()
			s := b.open(t)

			sentinel := errors.New("boom")
			err := s.Update(ctx, func(ctx context.Context, tx distledger.Tx) error {
				if _, err := tx.PutAgent(ctx, distledger.Agent{
					Key: agentKey(500), Status: distledger.AgentActive, Depth: 1,
				}); err != nil {
					return err
				}
				if _, err := tx.AppendCommission(ctx, distledger.Commission{
					Key: orderKey("ORD-ROLLBACK"), IdemKey: "rollback",
					BuyerUserID: 1, AgentUserID: 500, Layer: 1,
					BaseAmount: 100, Rate: 500, Amount: 5,
					State: distledger.CommissionPending, AccruedAt: epoch,
				}); err != nil {
					return err
				}
				if _, err := tx.AppendLedger(ctx, distledger.LedgerEntry{
					Key: agentKey(500), BizType: distledger.LedgerAccrue,
					BizID: "x", DeltaFrozen: 5, CreatedAt: epoch,
				}); err != nil {
					return err
				}
				return sentinel
			})
			if !errors.Is(err, sentinel) {
				t.Fatalf("expected the sentinel to propagate, got %v", err)
			}

			if _, err := read2(t, ctx, s, func(ctx context.Context, r distledger.Reader) (distledger.Agent, error) {
				return r.Agent(ctx, agentKey(500))
			}); !errors.Is(err, distledger.ErrNotFound) {
				t.Fatalf("agent survived the rollback: %v", err)
			}
			if list := read(t, ctx, s, func(ctx context.Context, r distledger.Reader) ([]distledger.Commission, error) {
				return r.CommissionsByOrder(ctx, orderKey("ORD-ROLLBACK"))
			}); len(list) != 0 {
				t.Fatalf("commission survived the rollback: %+v", list)
			}
			if entries := read(t, ctx, s, func(ctx context.Context, r distledger.Reader) ([]distledger.LedgerEntry, error) {
				return r.LedgerEntries(ctx, agentKey(500), distledger.LedgerQuery{})
			}); len(entries) != 0 {
				t.Fatalf("ledger entry survived the rollback: %+v", entries)
			}

			// The freed keys must be reusable: a rolled-back transaction that
			// keeps its idempotency key would make that idempotency key
			// permanently unusable, which is a silent way to lose a commission.
			if _, err := read2(t, ctx, s, func(ctx context.Context, r distledger.Reader) (distledger.Agent, error) {
				return r.Agent(ctx, agentKey(1))
			}); !errors.Is(err, distledger.ErrNotFound) {
				t.Fatalf("unexpected pre-existing agent: %v", err)
			}
			seedCommission(t, ctx, s, "ORD-ROLLBACK", 500)
		})
	}
}

func TestLedgerQueryFilters(t *testing.T) {
	for _, b := range backends(t) {
		t.Run(b.name, func(t *testing.T) {
			ctx := context.Background()
			s := b.open(t)

			for i, bizType := range []distledger.LedgerBizType{
				distledger.LedgerAccrue, distledger.LedgerSettle, distledger.LedgerReverse,
			} {
				if err := s.Update(ctx, func(ctx context.Context, tx distledger.Tx) error {
					_, err := tx.AppendLedger(ctx, distledger.LedgerEntry{
						Key: agentKey(100), BizType: bizType, BizID: orderID(i),
						DeltaFrozen: 1, CreatedAt: epoch,
					})
					return err
				}); err != nil {
					t.Fatal(err)
				}
			}

			all := read(t, ctx, s, func(ctx context.Context, r distledger.Reader) ([]distledger.LedgerEntry, error) {
				return r.LedgerEntries(ctx, agentKey(100), distledger.LedgerQuery{})
			})
			if len(all) != 3 {
				t.Fatalf("got %d entries, want 3", len(all))
			}

			filtered := read(t, ctx, s, func(ctx context.Context, r distledger.Reader) ([]distledger.LedgerEntry, error) {
				return r.LedgerEntries(ctx, agentKey(100), distledger.LedgerQuery{BizType: distledger.LedgerSettle})
			})
			if len(filtered) != 1 || filtered[0].BizType != distledger.LedgerSettle {
				t.Fatalf("type filter: %+v", filtered)
			}

			byTenant := read(t, ctx, s, func(ctx context.Context, r distledger.Reader) ([]distledger.LedgerEntry, error) {
				return r.LedgerByTenant(ctx, testTenant, distledger.Page{Limit: 2})
			})
			if len(byTenant) != 2 {
				t.Fatalf("tenant page has %d rows, want 2", len(byTenant))
			}
		})
	}
}

// read2 is read for functions that return an error alongside the value.
func read2[T any](t *testing.T, ctx context.Context, s distledger.Store, fn func(context.Context, distledger.Reader) (T, error)) (T, error) {
	t.Helper()
	var out T
	err := s.View(ctx, func(ctx context.Context, r distledger.Reader) error {
		var err error
		out, err = fn(ctx, r)
		return err
	})
	return out, err
}

func orderID(i int) string { return "ORD-" + string(rune('0'+i)) }
