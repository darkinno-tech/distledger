package distledger

import (
	"context"
	"fmt"
	"sort"
	"strconv"
)

// maxViolationsPerCheck caps how much evidence is recorded per invariant.
//
// A report is not a log: its purpose is to show at a glance whether anything
// is wrong and what the problem looks like. Recording ten million violations
// and recording twenty are worth the same when locating the problem, but the
// former would take down both memory and the caller's logging system.
const maxViolationsPerCheck = 20

// InvariantResult is the check result of one invariant.
type InvariantResult struct {
	// Name is the stable identifier of the invariant.
	Name string
	// OK reports whether the check passed entirely.
	OK bool
	// Checked is the number of objects examined by this check, which tells a
	// real pass apart from a check that examined nothing.
	Checked int
	// Violations are sample violations, at most maxViolationsPerCheck of them.
	Violations []string
	// TotalViolations is the total number of violations, which may exceed
	// len(Violations).
	TotalViolations int
}

func (r *InvariantResult) add(format string, args ...any) {
	r.TotalViolations++
	if len(r.Violations) < maxViolationsPerCheck {
		r.Violations = append(r.Violations, fmt.Sprintf(format, args...))
	}
	r.OK = false
}

// Report is the result of SelfCheck.
type Report struct {
	TenantID int64
	// OK is true only when every invariant passed and there are no schema
	// issues.
	OK bool
	// StoreKind is the store type identifier, for example "memory".
	StoreKind string
	// SchemaIssues are the schema issues reported by the store layer (the
	// in-memory store produces none).
	SchemaIssues []string
	// Invariants are the check results of the individual invariants.
	Invariants []InvariantResult
	// Counters are the commission counts per state, useful for spotting data
	// that is stuck and no longer moving.
	Counters map[string]int64
}

// SelfCheck runs a ledger self-check for one tenant.
//
// This is the single entry point for integration troubleshooting: it answers in
// one shot whether the ledger is conserved, whether any balance is negative,
// whether commissions exceeded their quota, whether any ledger entry points at
// a record that does not exist, and whether any data is stuck.
//
// It is read-only and modifies no data.
func (l *Ledger) SelfCheck(ctx context.Context, tenantID int64) (Report, error) {
	if tenantID < 0 {
		return Report{}, fieldErrf("tenant_id", "must be >= 0, got %d", tenantID)
	}

	rep := Report{TenantID: tenantID}
	if rs, ok := l.store.(ReportedStore); ok {
		rep.StoreKind = rs.StoreKind()
	}
	if sc, ok := l.store.(SchemaChecker); ok {
		rep.SchemaIssues = sc.CheckSchema(ctx)
	}

	err := l.store.View(ctx, func(ctx context.Context, r Reader) error {
		rep.Invariants = nil

		states, err := r.CountCommissionsByState(ctx, tenantID)
		if err != nil {
			return err
		}
		rep.Counters = make(map[string]int64, len(states)+1)
		for _, s := range AllCommissionStates() {
			rep.Counters[s.String()] = states[s]
		}

		i1, err := l.checkLedgerConservation(ctx, r, tenantID)
		if err != nil {
			return err
		}
		rep.Invariants = append(rep.Invariants, i1)

		i2, err := l.checkAllocationCap(ctx, r, tenantID)
		if err != nil {
			return err
		}
		rep.Invariants = append(rep.Invariants, i2)

		i3, err := l.checkReversalReconciliation(ctx, r, tenantID)
		if err != nil {
			return err
		}
		rep.Invariants = append(rep.Invariants, i3)
		return nil
	})
	if err != nil {
		return Report{}, err
	}

	rep.OK = len(rep.SchemaIssues) == 0
	for _, inv := range rep.Invariants {
		if !inv.OK {
			rep.OK = false
		}
	}
	return rep, nil
}

// checkLedgerConservation verifies invariant I1.
//
// A complete I1 requires all three checks together:
//
//  1. Every money bucket of an account equals the sum of the corresponding
//     deltas of all that account's ledger entries.
//  2. The After* values of the last ledger entry equal the account's current
//     values (this catches "the account was changed but the entry was
//     forgotten").
//  3. No money bucket is negative.
//
// Check 1 alone is not enough: if someone mistakenly changed both the account
// and the ledger entries, check 1 can still pass, while check 2 exposes it
// immediately.
func (l *Ledger) checkLedgerConservation(ctx context.Context, r Reader, tenantID int64) (InvariantResult, error) {
	res := InvariantResult{Name: "I1: account balance equals sum of ledger deltas", OK: true}

	var afterUserID int64
	for {
		accounts, err := r.AccountsByTenant(ctx, tenantID, AccountPage{
			AfterUserID: afterUserID, Limit: MaxPageLimit,
		})
		if err != nil {
			return res, err
		}
		if len(accounts) == 0 {
			break
		}
		for _, acct := range accounts {
			afterUserID = acct.Key.UserID
			res.Checked++

			if !acct.Settleable() {
				res.add("account %s has negative bucket: frozen=%s available=%s withdrawing=%s withdrawn=%s",
					acct.Key, acct.Frozen, acct.Available, acct.Withdrawing, acct.Withdrawn)
			}

			var (
				sumFrozen, sumAvailable, sumWithdrawing, sumWithdrawn Money
				last                                                  LedgerEntry
				entries                                               int
			)
			var afterID int64
			for {
				page, err := r.LedgerEntries(ctx, acct.Key, LedgerQuery{
					Page: Page{AfterID: afterID, Limit: MaxPageLimit},
				})
				if err != nil {
					return res, err
				}
				if len(page) == 0 {
					break
				}
				for _, e := range page {
					afterID = e.ID
					last = e
					entries++
					if sumFrozen, err = sumFrozen.Add(e.DeltaFrozen); err != nil {
						return res, err
					}
					if sumAvailable, err = sumAvailable.Add(e.DeltaAvailable); err != nil {
						return res, err
					}
					if sumWithdrawing, err = sumWithdrawing.Add(e.DeltaWithdrawing); err != nil {
						return res, err
					}
					if sumWithdrawn, err = sumWithdrawn.Add(e.DeltaWithdrawn); err != nil {
						return res, err
					}
				}
				if len(page) < MaxPageLimit {
					break
				}
			}

			if entries == 0 {
				// An account with no ledger entries must be all zeros:
				// accounts can only come into being driven by ledger entries.
				if acct.Frozen != 0 || acct.Available != 0 || acct.Withdrawing != 0 || acct.Withdrawn != 0 {
					res.add("account %s has balances %s/%s/%s/%s but no ledger entries",
						acct.Key, acct.Frozen, acct.Available, acct.Withdrawing, acct.Withdrawn)
				}
				continue
			}

			if sumFrozen != acct.Frozen || sumAvailable != acct.Available ||
				sumWithdrawing != acct.Withdrawing || sumWithdrawn != acct.Withdrawn {
				res.add("account %s mismatch: ledger sums %s/%s/%s/%s vs account %s/%s/%s/%s",
					acct.Key,
					sumFrozen, sumAvailable, sumWithdrawing, sumWithdrawn,
					acct.Frozen, acct.Available, acct.Withdrawing, acct.Withdrawn)
			}
			if last.AfterFrozen != acct.Frozen || last.AfterAvailable != acct.Available ||
				last.AfterWithdrawing != acct.Withdrawing || last.AfterWithdrawn != acct.Withdrawn {
				res.add("account %s tail mismatch: last ledger entry #%d recorded %s/%s/%s/%s, account is %s/%s/%s/%s",
					acct.Key, last.ID,
					last.AfterFrozen, last.AfterAvailable, last.AfterWithdrawing, last.AfterWithdrawn,
					acct.Frozen, acct.Available, acct.Withdrawing, acct.Withdrawn)
			}
		}
		if len(accounts) < MaxPageLimit {
			break
		}
	}
	return res, nil
}

// allocKey is the grouping key of the allocation check.
type allocKey struct {
	orderKey    OrderKey
	orderItemID string
}

type allocAgg struct {
	base   Money
	amount Money
}

// checkAllocationCap verifies invariant I2 and the referential integrity
// between commissions and ledger entries.
//
// It answers three questions at once:
//
//  1. Does every commission have a matching accrual ledger entry (this catches
//     "the commission was recorded but no money moved")?
//  2. Does every accrual ledger entry point at a commission that exists (this
//     catches dangling references)?
//  3. Is the total commission allocated to each computation unit within its
//     quota?
//
// Check 1 requires **enumerating commissions directly**. An earlier
// implementation only looked commissions up backwards from ledger entries, so
// the failure mode most in need of detection — "the commission was recorded
// but no money moved" — was exactly the one that stayed invisible.
func (l *Ledger) checkAllocationCap(ctx context.Context, r Reader, tenantID int64) (InvariantResult, error) {
	res := InvariantResult{Name: "I2: commission ledger linkage and allocation cap", OK: true}
	groups := make(map[allocKey]*allocAgg)

	// Scan the tenant's ledger first and collect every accrual reference.
	ledgerRefs := make(map[int64]struct{})
	var afterID int64
	for {
		page, err := r.LedgerByTenant(ctx, tenantID, Page{AfterID: afterID, Limit: MaxPageLimit})
		if err != nil {
			return res, err
		}
		if len(page) == 0 {
			break
		}
		for _, e := range page {
			afterID = e.ID
			// Accruals, reversals and voids all carry a commission id in BizID.
			// Collecting only accruals made every reversal record look like a
			// commission with no ledger entry behind it.
			if !e.BizType.referencesCommission() {
				continue
			}
			id, err := strconv.ParseInt(e.BizID, 10, 64)
			if err != nil {
				res.add("ledger entry #%d has non-numeric accrual biz id %q", e.ID, e.BizID)
				continue
			}
			ledgerRefs[id] = struct{}{}
		}
		if len(page) < MaxPageLimit {
			break
		}
	}

	// Then enumerate the commissions directly: this both verifies the reverse
	// references and groups them for the quota check.
	known := make(map[int64]struct{})
	var afterCommissionID int64
	for {
		page, err := r.CommissionsByTenant(ctx, tenantID, Page{AfterID: afterCommissionID, Limit: MaxPageLimit})
		if err != nil {
			return res, err
		}
		if len(page) == 0 {
			break
		}
		for _, c := range page {
			afterCommissionID = c.ID
			res.Checked++
			known[c.ID] = struct{}{}

			if _, linked := ledgerRefs[c.ID]; !linked {
				res.add("commission %d (order %s layer %d, %s) has no ledger entry",
					c.ID, c.Key.OrderID, c.Layer, c.Amount)
			}
			if c.Amount <= 0 {
				// Reversal records are not accruals and take no part in the
				// allocation cap; their own linkage was checked above.
				continue
			}
			k := allocKey{orderKey: c.Key, orderItemID: c.OrderItemID}
			g, ok := groups[k]
			if !ok {
				g = &allocAgg{base: c.BaseAmount}
				groups[k] = g
			}
			if g.amount, err = g.amount.Add(c.Amount); err != nil {
				return res, err
			}
		}
		if len(page) < MaxPageLimit {
			break
		}
	}

	// Verify the forward references: every commission a ledger entry points at
	// must exist.
	refs := make([]int64, 0, len(ledgerRefs))
	for id := range ledgerRefs {
		refs = append(refs, id)
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i] < refs[j] })
	for _, id := range refs {
		if _, ok := known[id]; !ok {
			res.add("accrual ledger entry references missing commission %d", id)
		}
	}

	// Finally, verify the quota.
	keys := make([]allocKey, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	// Sorting keeps the report reproducible: the same data always yields the
	// same violation order.
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].orderKey.TenantID != keys[j].orderKey.TenantID {
			return keys[i].orderKey.TenantID < keys[j].orderKey.TenantID
		}
		if keys[i].orderKey.OrderID != keys[j].orderKey.OrderID {
			return keys[i].orderKey.OrderID < keys[j].orderKey.OrderID
		}
		return keys[i].orderItemID < keys[j].orderItemID
	})

	for _, k := range keys {
		g := groups[k]
		capAmount, err := g.base.Apply(l.rules.MaxAllocatableBP, RoundDown)
		if err != nil {
			return res, err
		}
		if g.amount > capAmount {
			item := k.orderItemID
			if item == "" {
				item = "<whole order>"
			}
			res.add("order %s item %s allocated %s which exceeds cap %s (base %s)",
				k.orderKey, item, g.amount, capAmount, g.base)
		}
	}
	return res, nil
}

// checkReversalReconciliation verifies invariant I3.
//
// It answers three questions:
//
//  1. Does each commission's "reversed accumulator" equal the sum of the
//     absolute values of all reversal records under its name? The two are the
//     "balance" and the "journal" sides of one relationship and must always
//     agree.
//  2. Does the accumulator fall within [0, original amount], and is the state
//     consistent with it (a commission reversed in full must be in a terminal
//     state)?
//  3. Do the gross and reversed totals on the account equal the roll-up of all
//     that agent's commissions?
//
// Check 3 is deliberately independent of I1. I1 reconciles only the four money
// buckets, and TotalEarned and TotalReversed are not among them. If those two
// fields were left to "trust", a single mistaken write could make "total
// earnings" diverge from the facts forever while the books themselves stayed
// balanced everywhere.
func (l *Ledger) checkReversalReconciliation(ctx context.Context, r Reader, tenantID int64) (InvariantResult, error) {
	res := InvariantResult{Name: "I3: reversal accumulator, state and account totals reconcile", OK: true}

	originalsWithReversal := make(map[int64]Money)
	reversedByOriginal := make(map[int64]Money)
	originalAmount := make(map[int64]Money)
	accrued := make(map[int64]Money)
	reversed := make(map[int64]Money)

	var afterID int64
	for {
		page, err := r.CommissionsByTenant(ctx, tenantID, Page{AfterID: afterID, Limit: MaxPageLimit})
		if err != nil {
			return res, err
		}
		if len(page) == 0 {
			break
		}
		for _, c := range page {
			afterID = c.ID
			agent := c.AgentUserID

			if c.Amount > 0 {
				if accrued[agent], err = accrued[agent].Add(c.Amount); err != nil {
					return res, err
				}
				if c.ReversedAmount != 0 {
					res.Checked++
					originalsWithReversal[c.ID] = c.ReversedAmount
					originalAmount[c.ID] = c.Amount
					if c.ReversedAmount < 0 || c.ReversedAmount > c.Amount {
						res.add("commission %d has reversed amount %s outside [0, %s]",
							c.ID, c.ReversedAmount, c.Amount)
					}
					if c.FullyReversed() != (c.State == CommissionReversed || c.State == CommissionVoid) {
						res.add("commission %d reversed %s of %s but state is %s",
							c.ID, c.ReversedAmount, c.Amount, c.State)
					}
				}
				continue
			}

			// A negative record is a reversal: it must point at a real accrual.
			res.Checked++
			if c.ReverseOf <= 0 {
				res.add("commission %d has a negative amount %s but no reverse_of link", c.ID, c.Amount)
				continue
			}
			original, err := r.Commission(ctx, c.ReverseOf)
			if err != nil {
				res.add("commission %d reverses missing commission %d", c.ID, c.ReverseOf)
				continue
			}
			if original.Amount <= 0 {
				res.add("commission %d reverses commission %d which is not an accrual", c.ID, c.ReverseOf)
				continue
			}
			amount := -c.Amount
			if reversedByOriginal[c.ReverseOf], err = reversedByOriginal[c.ReverseOf].Add(amount); err != nil {
				return res, err
			}
			if reversed[agent], err = reversed[agent].Add(amount); err != nil {
				return res, err
			}
			if _, seen := originalAmount[c.ReverseOf]; !seen {
				originalAmount[c.ReverseOf] = original.Amount
			}
		}
		if len(page) < MaxPageLimit {
			break
		}
	}

	// 1) and 2): the accumulator must equal the sum of its detail records.
	ids := make([]int64, 0, len(originalsWithReversal)+len(reversedByOriginal))
	seen := make(map[int64]struct{}, len(originalsWithReversal)+len(reversedByOriginal))
	for id := range originalsWithReversal {
		if _, ok := seen[id]; !ok {
			seen[id] = struct{}{}
			ids = append(ids, id)
		}
	}
	for id := range reversedByOriginal {
		if _, ok := seen[id]; !ok {
			seen[id] = struct{}{}
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	for _, id := range ids {
		want := reversedByOriginal[id]
		got := originalsWithReversal[id]
		if got != want {
			res.add("commission %d records %s reversed but its reversal records sum to %s",
				id, got, want)
		}
		if amt, ok := originalAmount[id]; ok && want > amt {
			res.add("commission %d was reversed %s which exceeds its amount %s", id, want, amt)
		}
	}

	// 3): account totals must equal the per-agent roll-up.
	var afterUserID int64
	for {
		accounts, err := r.AccountsByTenant(ctx, tenantID, AccountPage{
			AfterUserID: afterUserID, Limit: MaxPageLimit,
		})
		if err != nil {
			return res, err
		}
		if len(accounts) == 0 {
			break
		}
		for _, acct := range accounts {
			afterUserID = acct.Key.UserID
			res.Checked++
			if got, want := acct.TotalEarned, accrued[acct.Key.UserID]; got != want {
				res.add("account %s total earned is %s but its accruals sum to %s",
					acct.Key, got, want)
			}
			if got, want := acct.TotalReversed, reversed[acct.Key.UserID]; got != want {
				res.add("account %s total reversed is %s but its reversals sum to %s",
					acct.Key, got, want)
			}
		}
		if len(accounts) < MaxPageLimit {
			break
		}
	}
	return res, nil
}
