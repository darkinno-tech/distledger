package distledger

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"time"
)

// This file implements refund reversal.
//
// # The shape of a reversal
//
// A reversal never edits an existing commission. It creates a NEW commission
// record with a negative Amount, linked back to the original through
// Commission.ReverseOf (see ADR-004). The original keeps every financial field
// untouched; only its lifecycle fields move:
//
//	ReversedAmount  accumulates how much of the original has been clawed back
//	State           becomes Reversed once ReversedAmount reaches Amount
//
// ReversedAmount exists so settlement can move only the OUTSTANDING part of a
// partially reversed commission. Without it, Maintain would try to move the
// full Amount out of the frozen bucket after part of it had already been clawed
// back, and the balance guard would abort the whole batch.
//
// # Why a refund is expressed as a target rather than a delta
//
// For every affected commission the engine computes the total amount that
// SHOULD have been reversed once the refund is accounted for, then applies only
// the difference. That single choice buys several properties at once:
//
//   - Duplicate refund events are no-ops: the target is already reached.
//   - Refunds may arrive in any order and in any number of instalments.
//   - Over-refunding is impossible: the target is capped at the original amount.
//   - Concurrency is handled by the idempotency key, which encodes the target
//     state itself, so two racing calls cannot both apply the same step.

// refundShares maps an accrual item to the amount refunded for it in this call.
// The key is the OrderItemID; order-level accruals (no Items supplied to
// OnOrderPaid) use the empty string.
type refundShares map[string]Money

// OnOrderRefunded handles "the order was refunded" and claws back the
// commissions it produced.
//
// # Three mutually exclusive ways to describe the refund
//
//   - IsFull: reverse every commission of the order down to its floor.
//   - Items: per-SKU refunds; each ItemID must match one supplied to OnOrderPaid.
//   - Amount: a whole-order refund amount, distributed across accrual items in
//     proportion to their commission base.
//
// # Idempotency
//
// Safe to deliver repeatedly, in any order, and in any number of instalments.
// A repeat call reports ReversedAmount == 0 and AlreadyReversed == true.
//
// # Known boundary: refunds must arrive after accrual
//
// If a refund arrives before the matching payment event, the order has no
// commissions yet and the call is a no-op. Commissions accrued later by the
// late payment event will not be reversed by that refund.
//
// Integrations should deliver events for one order in order (for example by
// using OrderID as the message queue partition key). The library deliberately
// does not keep a refund register to paper over this: it would add a write to
// every refund, and ordered delivery is the integrator's responsibility anyway.
func (l *Ledger) OnOrderRefunded(ctx context.Context, ev OrderRefundedEvent) (RefundResult, error) {
	if err := ev.validate(); err != nil {
		return RefundResult{}, err
	}
	key := ev.orderKey()
	refundedAt := ev.RefundedAt.UTC()

	var result RefundResult
	err := l.store.Update(ctx, func(ctx context.Context, tx Tx) error {
		// The store contract allows the transaction to be retried, so the
		// result must be rebuilt from scratch on every entry.
		result = RefundResult{OrderKey: key}

		all, err := tx.CommissionsByOrder(ctx, key)
		if err != nil {
			return err
		}
		if len(all) == 0 {
			result.Skipped = append(result.Skipped, SkipReason{
				Code: SkipNoParent, Detail: "order has no commissions yet",
			})
			result.AlreadyReversed = true
			return nil
		}

		groups := groupAccrualsByItem(all)
		if len(groups) == 0 {
			result.Skipped = append(result.Skipped, SkipReason{
				Code: SkipFullyReversed, Detail: "order has no accrual records",
			})
			result.AlreadyReversed = true
			return nil
		}

		// How much has already been refunded per item, taken from the refund
		// vouchers rather than inferred from the commissions. This is what
		// makes a retry distinguishable from a second real refund.
		cumulative, err := l.cumulativeRefunds(ctx, tx, key)
		if err != nil {
			return err
		}

		shares, err := l.refundShares(ev, groups)
		if err != nil {
			return err
		}

		for _, itemID := range sortedItemIDs(groups) {
			base := baseOf(groups[itemID])
			target := cumulative[itemID] + shares[itemID]
			if target > base {
				target = base
			}
			if target <= cumulative[itemID] {
				// Already refunded to the ceiling; nothing left for this item.
				continue
			}

			// The voucher is written first. A duplicate idempotency key means
			// this very instalment was applied before, so the whole item is a
			// no-op - which is exactly the retry case.
			if _, err := tx.AppendRefund(ctx, Refund{
				Key:        key,
				ItemID:     itemID,
				Amount:     shares[itemID],
				Cumulative: target,
				IdemKey:    refundIdemKey(key, itemID, ev.IdemKey),
				RefundedAt: refundedAt,
			}); err != nil {
				if errors.Is(err, ErrDuplicate) {
					result.Skipped = append(result.Skipped, SkipReason{
						Layer: 1, Code: SkipFullyReversed,
						Detail: "refund instalment " + ev.IdemKey + " was already applied",
					})
					continue
				}
				return err
			}

			if err := l.reverseItem(ctx, tx, groups[itemID], base, target, refundedAt, &result); err != nil {
				return err
			}
		}

		// Report plainly rather than leaving the caller to infer it from an
		// empty slice.
		result.AlreadyReversed = len(result.Reversed) == 0
		return nil
	})
	if err != nil {
		return RefundResult{}, err
	}
	return result, nil
}

// groupAccrualsByItem groups the commissions of an order by accrual item,
// keeping only the positive accrual records.
//
// Reversal records (negative Amount) are the RESULT of a clawback, not
// something that can itself be clawed back, so they take no part in the
// distribution. Their effect is already reflected in the original's
// ReversedAmount.
func groupAccrualsByItem(all []Commission) map[string][]Commission {
	groups := make(map[string][]Commission)
	for _, c := range all {
		if c.Amount <= 0 {
			continue
		}
		groups[c.OrderItemID] = append(groups[c.OrderItemID], c)
	}
	return groups
}

// sortedItemIDs returns the accrual items in a stable order.
//
// Go randomises map iteration. A clawback writes to the ledger, and the order
// of those writes affects both the timing of idempotency key generation and the
// reproducibility of logs, so the order is forced here.
func sortedItemIDs(groups map[string][]Commission) []string {
	ids := make([]string, 0, len(groups))
	for id := range groups {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// refundShares distributes one refund across the accrual items.
func (l *Ledger) refundShares(ev OrderRefundedEvent, groups map[string][]Commission) (refundShares, error) {
	shares := make(refundShares, len(groups))

	if ev.IsFull {
		// A full refund needs no amounts: every item is reversed to its ceiling.
		for id, list := range groups {
			shares[id] = baseOf(list)
		}
		return shares, nil
	}

	if len(ev.Items) > 0 {
		for _, it := range ev.Items {
			list, ok := groups[it.ItemID]
			if !ok {
				return nil, fieldErrf("items",
					"refund item %q does not match any accrual item of order %s", it.ItemID, ev.OrderID)
			}
			base := baseOf(list)
			if it.Amount > base {
				// A refund larger than the item's commission base means the
				// caller mixed up the amounts. Letting it through would
				// silently truncate the clawback while the caller believes the
				// refund was fully accounted for.
				return nil, fieldErrf("items",
					"refund amount %s for item %q exceeds its accrual base %s",
					it.Amount, it.ItemID, base)
			}
			shares[it.ItemID] = it.Amount
		}
		return shares, nil
	}

	// Whole-order refund: distribute in proportion to each item's commission
	// base. The last item absorbs the rounding remainder so the distributed
	// total equals the requested amount exactly.
	ids := sortedItemIDs(groups)
	var totalBase Money
	for _, id := range ids {
		next, err := totalBase.Add(baseOf(groups[id]))
		if err != nil {
			return nil, err
		}
		totalBase = next
	}
	if totalBase <= 0 {
		return nil, fieldErrf("amount", "order %s has no allocatable base", ev.OrderID)
	}

	var allocated Money
	for i, id := range ids {
		base := baseOf(groups[id])
		var share Money
		if i == len(ids)-1 {
			var err error
			if share, err = ev.Amount.Sub(allocated); err != nil {
				return nil, err
			}
		} else {
			var err error
			if share, err = ev.Amount.MulDiv(base, totalBase, RoundDown); err != nil {
				return nil, err
			}
		}
		if share < 0 {
			share = 0
		}
		if share > base {
			share = base
		}
		shares[id] = share
		next, err := allocated.Add(share)
		if err != nil {
			return nil, err
		}
		allocated = next
	}
	return shares, nil
}

// baseOf returns the commission base of an accrual item.
//
// Every layer under one (order, item) necessarily shares the same BaseAmount:
// they come from a single accrual computation. Taking the first entry is
// therefore enough and needs no extra storage.
func baseOf(list []Commission) Money {
	if len(list) == 0 {
		return 0
	}
	return list[0].BaseAmount
}

// cumulativeRefunds returns, per accrual item, the largest cumulative refunded
// amount recorded for the order.
//
// Taking the maximum rather than the sum is deliberate: each voucher already
// carries the running total for its item, so summing instalments would
// double-count.
func (l *Ledger) cumulativeRefunds(ctx context.Context, tx Tx, key OrderKey) (map[string]Money, error) {
	refunds, err := tx.RefundsByOrder(ctx, key)
	if err != nil {
		return nil, err
	}
	out := make(map[string]Money, len(refunds))
	for _, r := range refunds {
		if r.Cumulative > out[r.ItemID] {
			out[r.ItemID] = r.Cumulative
		}
	}
	return out, nil
}

// reverseItem claws back every layer of one accrual item towards the given
// cumulative refunded amount.
func (l *Ledger) reverseItem(
	ctx context.Context,
	tx Tx,
	list []Commission,
	base, cumulative Money,
	at time.Time,
	result *RefundResult,
) error {
	if base <= 0 || cumulative <= 0 {
		return nil
	}

	// Layer order is fixed so ledger writes and idempotency key generation are
	// reproducible.
	ordered := append([]Commission(nil), list...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Layer < ordered[j].Layer })

	for _, original := range ordered {
		delta, target, skip := l.reversalDelta(original, cumulative, base, CommissionReversed)
		if skip != nil {
			result.Skipped = append(result.Skipped, *skip)
			continue
		}
		if delta <= 0 {
			continue
		}

		reversal, err := l.clawBack(ctx, tx, original, delta, target, CommissionReversed, "refund", at)
		if err != nil {
			if errors.Is(err, ErrDuplicate) {
				// A racing call already applied exactly this step.
				continue
			}
			return err
		}
		result.Reversed = append(result.Reversed, reversal)
		next, err := result.ReversedAmount.Add(delta)
		if err != nil {
			return err
		}
		result.ReversedAmount = next
	}
	return nil
}

// reversalDelta computes how much of one commission this call should reverse.
//
// Returns (increment to apply, cumulative total afterwards, skip reason).
// A non-nil skip means the commission takes no part; the reason is filled in
// and the caller only has to record it.
func (l *Ledger) reversalDelta(original Commission, cumulative, base Money, terminal CommissionState) (delta, target Money, skip *SkipReason) {
	layer := original.Layer
	agentID := original.AgentUserID

	if original.State == CommissionWithdrawn {
		// The money has already left the platform. Clawing it back needs a debt
		// policy, which arrives together with withdrawals in v0.5.
		return 0, 0, &SkipReason{
			Layer: layer, AgentUserID: agentID, Code: SkipAlreadyWithdrawn,
			Detail: "commission was already paid out",
		}
	}
	if original.State == CommissionReversed || original.State == CommissionVoid {
		return 0, 0, &SkipReason{
			Layer: layer, AgentUserID: agentID, Code: SkipFullyReversed,
			Detail: "commission state is " + original.State.String(),
		}
	}
	if terminal == CommissionVoid && original.Amount <= 0 {
		return 0, 0, nil
	}

	// The target is derived from the CUMULATIVE refunded amount, never from
	// this instalment alone. That is what makes the whole operation idempotent:
	// recomputing after a retry yields the same target, so the difference is
	// zero and nothing is written.
	//
	// Truncation is deliberate: rounding up would claw back more than the
	// refund justifies, and a agent noticing an over-claw is a support
	// ticket, while the platform losing a fraction of a cent is not.
	var want Money
	if terminal == CommissionVoid {
		// A void forfeits everything that is still outstanding.
		want = original.Amount
	} else {
		var err error
		if want, err = original.Amount.MulDiv(cumulative, base, RoundDown); err != nil {
			return 0, 0, nil
		}
		// Never reverse more than was accrued, whatever the caller sends.
		if want > original.Amount {
			want = original.Amount
		}
	}
	if want <= original.ReversedAmount {
		return 0, 0, nil
	}
	return want - original.ReversedAmount, want, nil
}

// clawBack writes the reversal record, moves the money and appends the ledger
// entry for one commission.
//
// Ordering is deliberate: the negative commission record is appended first,
// then the original's accumulator moves and its state advances, then the
// account is debited, then the ledger entry is written. Any failure rolls the
// whole transaction back, so there is no window in which a commission is marked
// as reversed while the money is still on the books.
func (l *Ledger) clawBack(
	ctx context.Context,
	tx Tx,
	original Commission,
	delta, target Money,
	terminal CommissionState,
	reason string,
	at time.Time,
) (Commission, error) {
	reversal, err := tx.AppendCommission(ctx, Commission{
		Key:          original.Key,
		IdemKey:      reverseIdemKey(original.Key, original.OrderItemID, original.AgentUserID, original.Layer, target),
		OrderItemID:  original.OrderItemID,
		BuyerUserID:  original.BuyerUserID,
		AgentUserID:  original.AgentUserID,
		Layer:        original.Layer,
		BaseAmount:   original.BaseAmount,
		Rate:         original.Rate,
		Amount:       -delta,
		State:        CommissionReversed,
		RuleVersion:  original.RuleVersion,
		RuleSnapshot: original.RuleSnapshot,
		ReverseOf:    original.ID,
		AccruedAt:    at,
	})
	if err != nil {
		return Commission{}, err
	}

	// 1) Record how much has been clawed back, and move to the terminal state
	//    when nothing is left.
	cur, err := tx.SetCommissionReversedAmount(ctx, original.ID, target, original.Version)
	if err != nil {
		return Commission{}, err
	}
	if cur.FullyReversed() && cur.State != terminal {
		if cur, err = tx.TransitionCommission(ctx, cur.ID, cur.State, terminal, cur.Version); err != nil {
			return Commission{}, err
		}
	}

	// 2) Debit the right bucket.
	//
	// Pending and risk-frozen commissions keep their money in Frozen; settled
	// ones keep it in Available. The bucket is chosen from the state the
	// commission was in, never from current configuration.
	kind, err := reversalBucket(original.State)
	if err != nil {
		return Commission{}, err
	}
	effect, err := l.debitBucket(ctx, tx, original, kind, delta, reason, reversal.ID, target, cur.Amount, at)
	if err != nil {
		return Commission{}, err
	}
	_ = effect
	return reversal, nil
}

// debitBucket removes the clawed-back amount from the account and appends the
// matching ledger entry.
func (l *Ledger) debitBucket(
	ctx context.Context,
	tx Tx,
	original Commission,
	kind reversalBucketKind,
	delta Money,
	reason string,
	reversalID int64,
	target, originalAmount Money,
	at time.Time,
) (bucketEffect, error) {
	key := UserKey{TenantID: original.Key.TenantID, UserID: original.AgentUserID}
	acct, err := tx.Account(ctx, key)
	if err != nil {
		return bucketEffect{}, err
	}
	acct.Key = key

	effect, err := kind.apply(&acct, delta)
	if err != nil {
		return bucketEffect{}, fmt.Errorf("%w: %v", ErrInsufficientBalance, err)
	}
	if acct.TotalReversed, err = acct.TotalReversed.Add(delta); err != nil {
		return bucketEffect{}, err
	}
	acct.UpdatedAt = at

	saved, err := tx.PutAccount(ctx, acct)
	if err != nil {
		return bucketEffect{}, err
	}

	bizType := LedgerReverse
	if reason == reasonVoid {
		bizType = LedgerVoid
	}
	_, err = tx.AppendLedger(ctx, LedgerEntry{
		Key:              key,
		BizType:          bizType,
		BizID:            strconv.FormatInt(reversalID, 10),
		DeltaFrozen:      effect.deltaFrozen,
		DeltaAvailable:   effect.deltaAvailable,
		AfterFrozen:      saved.Frozen,
		AfterAvailable:   saved.Available,
		AfterWithdrawing: saved.Withdrawing,
		AfterWithdrawn:   saved.Withdrawn,
		Remark: reason + " order " + original.Key.OrderID +
			" layer " + strconv.Itoa(original.Layer) +
			" (reversed " + target.String() + " of " + originalAmount.String() + ")",
		CreatedAt: at,
	})
	return effect, err
}

// reversalBucketKind says which bucket a clawback should draw from.
type reversalBucketKind uint8

const (
	bucketFrozen reversalBucketKind = iota
	bucketAvailable
)

type bucketEffect struct {
	deltaFrozen    Money
	deltaAvailable Money
}

func reversalBucket(state CommissionState) (reversalBucketKind, error) {
	switch state {
	case CommissionPending, CommissionFrozen:
		return bucketFrozen, nil
	case CommissionSettled:
		return bucketAvailable, nil
	default:
		return 0, &TransitionError{
			Kind: "commission clawback", From: state.String(), To: CommissionReversed.String(),
		}
	}
}

// apply debits the chosen bucket and returns the deltas the ledger entry needs.
//
// An insufficient balance returns an error instead of pushing the bucket
// negative: a negative balance would break invariant I1, and an account that
// "reads as negative" is far harder to clean up than one failed clawback.
// In v0.3 this path is unreachable — with no withdrawals the money is always
// still in the bucket. It is a safety net for v0.5, where a commission can be
// paid out before the order is refunded.
func (k reversalBucketKind) apply(acct *Account, amount Money) (bucketEffect, error) {
	var err error
	switch k {
	case bucketFrozen:
		if acct.Frozen < amount {
			return bucketEffect{}, fmt.Errorf(
				"frozen balance %s is less than the clawback %s", acct.Frozen, amount)
		}
		if acct.Frozen, err = acct.Frozen.Sub(amount); err != nil {
			return bucketEffect{}, err
		}
		return bucketEffect{deltaFrozen: -amount}, nil
	default:
		if acct.Available < amount {
			return bucketEffect{}, fmt.Errorf(
				"available balance %s is less than the clawback %s", acct.Available, amount)
		}
		if acct.Available, err = acct.Available.Sub(amount); err != nil {
			return bucketEffect{}, err
		}
		return bucketEffect{deltaAvailable: -amount}, nil
	}
}
