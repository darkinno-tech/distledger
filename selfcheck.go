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
	// Notes are observations that are not violations and do not affect OK.
	//
	// The one producer today is a debt carried under Rules.AllowNegative: a
	// negative withdrawable balance the rules permit. Visible, not failing.
	Notes []string
}

func (r *InvariantResult) add(format string, args ...any) {
	r.TotalViolations++
	if len(r.Violations) < maxViolationsPerCheck {
		r.Violations = append(r.Violations, fmt.Sprintf(format, args...))
	}
	r.OK = false
}

// note records something worth seeing that is not a violation.
//
// It exists for exactly one case, and the reason is worth stating: when
// Rules.AllowNegative is set, an account whose withdrawable bucket is negative
// is carrying a debt the rules permit. That is not an inconsistency, so it must
// not fail the check - but a balance below zero is alarming enough that
// silently accepting it would be its own kind of bug. A note is the honest
// middle: reported, and not counted against the invariant.
func (r *InvariantResult) note(format string, args ...any) {
	if len(r.Notes) < maxViolationsPerCheck {
		r.Notes = append(r.Notes, fmt.Sprintf(format, args...))
	}
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
	// Reconciled reports that I1 was checked by the store's own aggregation
	// rather than row-wise.
	//
	// It is reported because the two paths cost wildly different amounts and
	// only one of them scales, so "the check passed" means something different
	// depending on which ran. An operator diagnosing a slow self-check needs to
	// know whether the store is doing the work or the client is.
	Reconciled bool
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

	// I1 is checked before the shared read transaction is opened, because a
	// reconciling store runs its own aggregation and therefore owns its own read.
	// A store without that ability is checked row-wise inside the transaction
	// below, so the invariant is always checked, only differently - never
	// skipped, which would be far worse than checking it the slow way.
	var (
		i1     InvariantResult
		haveI1 bool
	)
	if rec, ok := l.store.(Reconciler); ok {
		var err error
		i1, err = l.checkLedgerConservationReconciled(ctx, rec, tenantID)
		if err != nil {
			return Report{}, err
		}
		haveI1 = true
		rep.Reconciled = true
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

		if !haveI1 {
			i1, err = l.checkLedgerConservation(ctx, r, tenantID)
			if err != nil {
				return err
			}
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

		i4, err := l.checkWithdrawalReconciliation(ctx, r, tenantID)
		if err != nil {
			return err
		}
		rep.Invariants = append(rep.Invariants, i4)
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

			// Walk the shared bucket definition rather than naming the four
			// fields. Hand-written enumeration is how a newly added money field
			// ends up outside every invariant while the ledger still balances.
			//
			// The sum is accumulated into an Account rather than a map so that
			// the judgement below reads a bucket the same way whichever path
			// produced it.
			buckets := AllBuckets()
			var sums Account
			var (
				last    LedgerEntry
				entries int
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
					for _, b := range buckets {
						sum, err := b.Value(sums).Add(b.Delta(e))
						if err != nil {
							return res, err
						}
						b.Set(&sums, sum)
					}
				}
				if len(page) < MaxPageLimit {
					break
				}
			}

			accountConservationViolations(&res, acct, sums, last, entries, l.rules.AllowNegative)
		}
		if len(accounts) < MaxPageLimit {
			break
		}
	}
	return res, nil
}

// accountConservationViolations records every way one account can violate I1.
//
// This is the single implementation of the judgement. Both the row-wise path and
// the reconciled path call it, so the two cannot drift into disagreeing about
// what counts as a violation, or about how one is described.
//
// sums holds the sum of the account's ledger deltas per bucket, tail is its
// highest-id entry, and entries is how many entries it has. When entries is zero
// there is no tail and sums are all zero, which is why the zero-entry case is
// decided from the account alone.
func accountConservationViolations(
	res *InvariantResult, acct Account, sums Account, tail LedgerEntry, entries int,
	allowNegative bool,
) {
	buckets := AllBuckets()

	if !acct.Settleable() {
		for _, b := range buckets {
			v := b.Value(acct)
			if v >= 0 {
				continue
			}
			// A debt is only ever permitted in the withdrawable bucket, and
			// only when the rules say so. Any other bucket below zero means a
			// commission was clawed back twice, which is a bug rather than a
			// debt - so it stays a violation either way.
			if allowNegative && b == BucketAvailable {
				res.note("account %s carries a debt of %s in the %s bucket, "+
					"permitted by Rules.AllowNegative", acct.Key, v, b)
				continue
			}
			res.add("account %s has a negative %s bucket: %s", acct.Key, b, v)
		}
	}

	if entries == 0 {
		// An account with no ledger entries must be all zeros: accounts can
		// only come into being driven by ledger entries.
		for _, b := range buckets {
			if v := b.Value(acct); v != 0 {
				res.add("account %s has %s=%s but no ledger entries", acct.Key, b, v)
			}
		}
		return
	}

	for _, b := range buckets {
		if sum := b.Value(sums); sum != b.Value(acct) {
			res.add("account %s mismatch on %s: ledger deltas sum to %s but the account holds %s",
				acct.Key, b, sum, b.Value(acct))
		}
		if after := b.After(tail); after != b.Value(acct) {
			res.add("account %s tail mismatch on %s: last ledger entry #%d recorded %s but the account holds %s",
				acct.Key, b, tail.ID, after, b.Value(acct))
		}
	}
}

// checkLedgerConservationReconciled verifies invariant I1 using a store that can
// aggregate in place.
//
// The judgement is the same function the row-wise path calls. What differs is
// that the sums, the tail entry and the entry count arrive from the store instead
// of being accumulated one page at a time, which is the whole point: the cost of
// the check stops depending on how many ledger entries exist and starts
// depending only on how many accounts are actually wrong.
func (l *Ledger) checkLedgerConservationReconciled(
	ctx context.Context, rec Reconciler, tenantID int64,
) (InvariantResult, error) {
	res := InvariantResult{Name: "I1: account balance equals sum of ledger deltas", OK: true}

	examined, err := rec.ReconcileAccounts(ctx, tenantID, MaxPageLimit, func(m AccountReconciliation) error {
		accountConservationViolations(&res, m.Stored, m.Summed, m.Tail, entriesFromTail(m),
			l.rules.AllowNegative)
		return nil
	})
	if err != nil {
		return res, err
	}
	// Checked counts the accounts examined, not the candidates reported. The
	// difference matters: a healthy tenant reports no candidates, so counting
	// candidates would make a clean check indistinguishable from a check that
	// examined nothing.
	res.Checked = examined
	return res, nil
}

// entriesFromTail reports whether the reconciled account has any ledger entries.
//
// Only zero and non-zero are distinguished, because that is all the judgement
// above needs: it decides the zero-entry case from the account alone rather than
// from a count it would have to trust.
func entriesFromTail(m AccountReconciliation) int {
	if m.HasTail {
		return 1
	}
	return 0
}

// checkWithdrawalReconciliation verifies invariant I4.
//
// # What it covers that no other invariant does
//
// A withdrawal is the one record in this library whose state claims something
// about money *outside* the account: Paid means the money has left the
// platform. Every other state is a promise the account can still be checked
// against, but a paid withdrawal has already gone.
//
// Two things therefore have to be verified, and they fail in different ways.
//
// **The reservation must match the bucket.** The withdrawable account's
// reserved bucket holds exactly the money of the withdrawals that are still
// reserved. If a withdrawal is marked paid without the money being moved - which
// is what a half-applied payout leaves behind - the bucket disagrees with the
// sum and this catches it. The state machine cannot: the transition is legal,
// and only the money is missing.
//
// **The ledger must corroborate the state.** A reserved withdrawal must have a
// hold entry, a paid one a paid entry, a rejected one a refund entry. A state
// without its entry means the two halves of the library disagree about what
// happened.
func (l *Ledger) checkWithdrawalReconciliation(
	ctx context.Context, r Reader, tenantID int64,
) (InvariantResult, error) {
	res := InvariantResult{Name: "I4: withdrawal state, reservation and ledger agree", OK: true}

	// The ledger's withdraw entries, indexed by the withdrawal they reference.
	// Walking the tenant's ledger once is the same cost as I1's walk, and it
	// avoids a read per withdrawal.
	holds := map[int64]bool{}
	paids := map[int64]bool{}
	refunds := map[int64]bool{}
	var afterEntry int64
	for {
		page, err := r.LedgerByTenant(ctx, tenantID, Page{AfterID: afterEntry, Limit: MaxPageLimit})
		if err != nil {
			return res, err
		}
		if len(page) == 0 {
			break
		}
		for _, e := range page {
			afterEntry = e.ID
			// Filter before parsing. A manual adjustment carries a free-form
			// BizID, so parsing every entry turns one into a reported defect.
			if !e.BizType.referencesWithdrawal() {
				continue
			}
			id, err := strconv.ParseInt(e.BizID, 10, 64)
			if err != nil {
				// A withdraw entry whose reference is not a number cannot be
				// matched to anything, which is itself worth reporting: it means
				// the state and the money have no way back to each other.
				res.add("ledger entry #%d is a withdrawal entry with non-numeric biz id %q",
					e.ID, e.BizID)
				continue
			}
			switch e.BizType {
			case LedgerWithdrawHold:
				holds[id] = true
			case LedgerWithdrawPaid:
				paids[id] = true
			case LedgerWithdrawRefund:
				refunds[id] = true
			}
		}
		if len(page) < MaxPageLimit {
			break
		}
	}

	reserved := map[UserKey]Money{}
	var afterID int64
	for {
		page, err := r.WithdrawalsByTenant(ctx, tenantID, Page{AfterID: afterID, Limit: MaxPageLimit})
		if err != nil {
			return res, err
		}
		if len(page) == 0 {
			break
		}
		for _, w := range page {
			afterID = w.ID
			res.Checked++

			if w.State == WithdrawalPaid && w.PaidAt.IsZero() {
				res.add("withdrawal %d is paid but records no payout time", w.ID)
			}
			if w.State != WithdrawalPaid && !w.PaidAt.IsZero() {
				res.add("withdrawal %d records a payout time but is %s", w.ID, w.State)
			}
			// Every withdrawal that moved money must have its hold entry; it is
			// written in the same transaction as the reservation, so a missing
			// one means the transaction was not atomic.
			if !holds[w.ID] {
				res.add("withdrawal %d has no hold ledger entry, so its reservation is not on the ledger",
					w.ID)
			}
			switch w.State {
			case WithdrawalPaid:
				if !paids[w.ID] {
					res.add("withdrawal %d is paid but has no paid ledger entry: the money never moved",
						w.ID)
				}
			case WithdrawalRejected:
				if !refunds[w.ID] {
					res.add("withdrawal %d is rejected but has no refund ledger entry: the money never came back",
						w.ID)
				}
			}
			if amount := w.Outstanding(); amount > 0 {
				sum, err := reserved[w.Key].Add(amount)
				if err != nil {
					return res, err
				}
				reserved[w.Key] = sum
			}
		}
		if len(page) < MaxPageLimit {
			break
		}
	}

	// The money half: the reserved bucket must hold exactly the outstanding
	// withdrawals. Accounts are enumerated from the account side so that an
	// account with reserved money and no withdrawal - the case a deleted or
	// never-written withdrawal leaves - is caught too.
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
			// Counted because the money comparison below is a real check even
			// when the tenant has no withdrawals: it is what proves the bucket
			// is empty because nothing reserved it, rather than because the
			// query found nothing.
			res.Checked++
			want := reserved[acct.Key]
			if got := acct.Withdrawing; got != want {
				res.add("account %s holds %s in the withdrawing bucket but its outstanding "+
					"withdrawals total %s", acct.Key, got, want)
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
