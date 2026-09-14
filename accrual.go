package distledger

import (
	"context"
	"errors"
	"strconv"
	"time"
)

// This file is the commission engine. It only orchestrates; it does not
// decide: "how much" goes to the RateResolver, "is this agent eligible" goes
// to the EligibilityChecker, and "may the money move this way" is guaranteed
// jointly by the state machine and the transaction boundaries drawn here (see
// ADR-002).

// accrualBase is one unit of commission computation.
//
// When Items is not supplied, the whole order is a single unit (itemID is the
// empty string); when Items is supplied, each line item is its own unit with
// its own quota.
type accrualBase struct {
	itemID string
	amount Money
}

// basesOf breaks an event down into commission computation units.
func basesOf(ev OrderPaidEvent) []accrualBase {
	if len(ev.Items) == 0 {
		return []accrualBase{{itemID: "", amount: ev.PaidAmount}}
	}
	out := make([]accrualBase, 0, len(ev.Items))
	for _, it := range ev.Items {
		out = append(out, accrualBase{itemID: it.ItemID, amount: it.Amount})
	}
	return out
}

// OnOrderPaid handles "the order has been paid": attribution, commission
// computation, accrual and bookkeeping.
//
// # Idempotency
//
// It is safe to deliver repeatedly. A repeat delivery returns Replayed=true
// with an empty Commissions slice. Idempotency is guaranteed twice over: by
// the short circuit on "the order already has commissions" and by the unique
// idempotency key of every commission.
//
// # Attribution rules
//
// Attribution happens only when the binding already existed and was effective
// at the **moment of payment**. This rule guards against after-the-fact
// poaching: if only the "current binding" were consulted, anyone could bind
// themselves as the buyer's referrer after the order was paid and have the
// earnings confirmed retroactively.
//
// # No attribution is not an error
//
// When the buyer has no referral attribution, it returns Attributed=false with
// err nil. An order system should not receive a failure merely because "this
// order has no referrer".
//
// # Known boundary
//
// The idempotency key is a function of (order, line item, agent, level).
// So if the rate configuration is changed after the first delivery and the
// same event is replayed, commissions can appear that the first delivery did
// not produce. In production, fix the rates before orders are created; freezing
// the rule version per order is a capability of a later version.
func (l *Ledger) OnOrderPaid(ctx context.Context, ev OrderPaidEvent) (AccrueResult, error) {
	if err := ev.validate(); err != nil {
		return AccrueResult{}, err
	}
	paidAt := ev.PaidAt
	if paidAt.IsZero() {
		paidAt = l.clock.Now()
	}
	paidAt = paidAt.UTC()

	key := ev.orderKey()
	buyerKey := UserKey{TenantID: ev.TenantID, UserID: ev.BuyerUserID}

	var result AccrueResult
	err := l.store.Update(ctx, func(ctx context.Context, tx Tx) error {
		// The Store may retry the transaction, so every entry must rebuild
		// the result from scratch; otherwise a retry would accumulate the
		// result twice.
		result = AccrueResult{OrderKey: key}

		existing, err := tx.CommissionsByOrder(ctx, key)
		if err != nil {
			return err
		}
		if len(existing) > 0 {
			// The order-level short circuit keys on OrderID alone, so a second
			// event for the same order naming a DIFFERENT buyer is
			// contradictory input rather than a replay. Swallowing it would
			// hide a caller bug behind a success, so it is rejected.
			if buyer := existing[0].BuyerUserID; buyer != ev.BuyerUserID {
				return fieldErrf("buyer_user_id",
					"order %s was already accrued for buyer %d but this event claims buyer %d",
					ev.OrderID, buyer, ev.BuyerUserID)
			}
			// "Attributed with no agent" is not a state the domain has. If the
			// level-1 record is missing, the data is inconsistent, so say so
			// rather than returning an impossible pair.
			agentUserID := layerOneAgent(existing)
			result.Replayed = true
			if agentUserID == 0 {
				result.Skipped = append(result.Skipped, SkipReason{
					Layer: 1, Code: SkipAgentMissing,
					Detail: "order has commissions but no level-1 record",
				})
				return nil
			}
			result.Attributed = true
			result.AgentUserID = agentUserID
			return nil
		}

		binding, err := tx.Binding(ctx, buyerKey)
		if errors.Is(err, ErrNotFound) {
			result.Skipped = append(result.Skipped, SkipReason{
				Layer: 1, Code: SkipNoParent, Detail: "buyer has no binding",
			})
			return nil
		}
		if err != nil {
			return err
		}
		if binding.BoundAt.After(paidAt) {
			result.Skipped = append(result.Skipped, SkipReason{
				Layer: 1, Code: SkipNoParent,
				Detail: "binding was created after the order was paid",
			})
			return nil
		}
		if !binding.Effective(paidAt) {
			result.Skipped = append(result.Skipped, SkipReason{
				Layer: 1, Code: SkipNoParent, Detail: "binding is not effective at paid time",
			})
			return nil
		}

		result.Attributed = true
		result.AgentUserID = binding.AgentUserID

		first, err := tx.Agent(ctx, UserKey{TenantID: ev.TenantID, UserID: binding.AgentUserID})
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				result.Skipped = append(result.Skipped, SkipReason{
					Layer: 1, AgentUserID: binding.AgentUserID, Code: SkipAgentMissing,
					Detail: "attributed agent no longer exists",
				})
				return nil
			}
			return err
		}
		// Level 1 being ineligible means the attribution itself is invalid, so
		// the whole order pays no commission. This differs from the handling
		// of an ineligible middle level; see the note on accrueBase.
		if ok, reason := l.elig.Eligible(ctx, EligibilityInput{Agent: first, Order: key, BuyerUserID: ev.BuyerUserID}); !ok {
			result.Skipped = append(result.Skipped, SkipReason{
				Layer: 1, AgentUserID: first.Key.UserID, Code: SkipIneligible, Detail: reason,
			})
			return nil
		}

		var allocatedBase Money
		for _, base := range basesOf(ev) {
			next, err := allocatedBase.Add(base.amount)
			if err != nil {
				return err
			}
			allocatedBase = next
			if err := l.accrueBase(ctx, tx, ev, base, first.Key.UserID, &result); err != nil {
				return err
			}
		}
		if ev.PaidAmount > allocatedBase {
			result.UnallocatedBase = ev.PaidAmount - allocatedBase
		}
		return nil
	})
	if err != nil {
		return AccrueResult{}, err
	}
	return result, nil
}

// layerOneAgent finds the level-1 agent among existing commissions, to
// backfill the result on a replay.
func layerOneAgent(list []Commission) int64 {
	for _, c := range list {
		if c.Layer == 1 {
			return c.AgentUserID
		}
	}
	return 0
}

// accrueBase walks up the relation chain and generates the commission of every
// level for one computation unit.
//
// # When a middle level is ineligible
//
// An ineligible level is skipped and the chain continues upward. The reason is
// that the relation chain is an objective topology, while eligibility is a
// property of an individual. If an ineligible node truncated the chain, one
// temporarily disabled person would silently swallow the earnings of every one
// of their uplines, a problem that is extremely hard to diagnose and to smooth
// over in production.
//
// Level 1 is the exception (see OnOrderPaid): it being ineligible means the
// attribution is invalid and the whole order pays no commission.
func (l *Ledger) accrueBase(ctx context.Context, tx Tx, ev OrderPaidEvent, base accrualBase, firstAgentID int64, result *AccrueResult) error {
	order := ev.orderKey()

	// The quota is computed per computation unit rather than per whole order;
	// otherwise, in an order with several line items, the item handled first
	// would eat into the allowance of the ones handled after it.
	capAmount, err := base.amount.Apply(l.rules.MaxAllocatableBP, RoundDown)
	if err != nil {
		return err
	}
	var allocated Money

	agentUserID := firstAgentID
	for layer := 1; layer <= l.rules.Levels; layer++ {
		if agentUserID == 0 {
			result.Skipped = append(result.Skipped, SkipReason{
				Layer: layer, Code: SkipNoParent, Detail: "relation chain ends here",
			})
			break
		}

		agentKey := UserKey{TenantID: ev.TenantID, UserID: agentUserID}
		agent, err := tx.Agent(ctx, agentKey)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				result.Skipped = append(result.Skipped, SkipReason{
					Layer: layer, AgentUserID: agentUserID, Code: SkipAgentMissing,
					Detail: "agent referenced by relation chain does not exist",
				})
				break
			}
			return err
		}

		if layer > 1 {
			if ok, reason := l.elig.Eligible(ctx, EligibilityInput{Agent: agent, Order: order, BuyerUserID: ev.BuyerUserID}); !ok {
				result.Skipped = append(result.Skipped, SkipReason{
					Layer: layer, AgentUserID: agentUserID, Code: SkipIneligible, Detail: reason,
				})
				agentUserID = agent.ParentID
				continue
			}
		}

		rate, err := l.rates.ResolveRate(ctx, RateInput{
			Order: order, Layer: layer, Agent: agent, Rules: l.rules,
		})
		if err != nil {
			return err
		}

		amount, err := base.amount.Apply(rate, l.rules.Rounding)
		if err != nil {
			return err
		}

		// Quota cap: even if the rate configuration has been broken, the total
		// paid out never exceeds the base ceiling. This is the runtime
		// guarantee of invariant I2.
		if remaining := capAmount - allocated; amount > remaining {
			result.Skipped = append(result.Skipped, SkipReason{
				Layer: layer, AgentUserID: agentUserID, Code: SkipCapExhausted,
				Detail: "amount " + amount.String() + " truncated to remaining " + remaining.String(),
			})
			amount = remaining
		}

		if amount <= 0 {
			result.Skipped = append(result.Skipped, SkipReason{
				Layer: layer, AgentUserID: agentUserID, Code: SkipZeroAmount,
				Detail: "rate " + rate.String() + " yields zero",
			})
			agentUserID = agent.ParentID
			continue
		}

		now := l.clock.Now()
		c, err := tx.AppendCommission(ctx, Commission{
			Key:          order,
			IdemKey:      accrueIdemKey(order, base.itemID, agentUserID, layer, ev.IdemKey),
			OrderItemID:  base.itemID,
			BuyerUserID:  ev.BuyerUserID,
			AgentUserID:  agentUserID,
			Layer:        layer,
			BaseAmount:   base.amount,
			Rate:         rate,
			Amount:       amount,
			State:        CommissionPending,
			FreezeDays:   l.rules.FreezeDays,
			RuleVersion:  l.ruleVersion,
			RuleSnapshot: l.rules.Describe(),
			AccruedAt:    now,
		})
		if err != nil {
			if errors.Is(err, ErrDuplicate) {
				// Duplicates cannot happen inside one transaction; this is a
				// defensive backstop so that a duplicate record never
				// interrupts the accrual for the whole order.
				agentUserID = agent.ParentID
				continue
			}
			return err
		}

		allocated += amount
		if err := l.creditAccrual(ctx, tx, c, now); err != nil {
			return err
		}
		result.Commissions = append(result.Commissions, c)

		agentUserID = agent.ParentID
	}
	return nil
}

// creditAccrual credits a commission into the account's "pending settlement"
// bucket and appends one ledger entry.
//
// The entry is written after the account is updated so that the After* values
// in it reflect the true post-change balance: during reconciliation, a single
// entry then suffices to tell what the balance was at that moment.
func (l *Ledger) creditAccrual(ctx context.Context, tx Tx, c Commission, now time.Time) error {
	key := UserKey{TenantID: c.Key.TenantID, UserID: c.AgentUserID}

	acct, err := tx.Account(ctx, key)
	if err != nil {
		return err
	}
	// When the account does not exist, Account returns a zero value whose Key
	// is empty, so the Key must be filled in explicitly.
	acct.Key = key

	if acct.Frozen, err = acct.Frozen.Add(c.Amount); err != nil {
		return err
	}
	if acct.TotalEarned, err = acct.TotalEarned.Add(c.Amount); err != nil {
		return err
	}
	acct.UpdatedAt = now

	saved, err := tx.PutAccount(ctx, acct)
	if err != nil {
		return err
	}

	_, err = tx.AppendLedger(ctx, newLedgerEntry(
		key, LedgerAccrue, strconv.FormatInt(c.ID, 10),
		"order "+c.Key.OrderID+" layer "+strconv.Itoa(c.Layer),
		now, saved, BucketFrozen, c.Amount,
	))
	return err
}
