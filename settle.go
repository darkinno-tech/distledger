package distledger

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// This file owns the "freeze then settle" leg. It is where the state machine is
// actually driven forward.
//
// Common to both entries: the ledger's notion of processing time comes from the
// injected Clock, never from a parameter. Event times that describe something
// that happened in the business system (when an order was paid, when goods were
// received) are supplied by the caller; processing time is not, because letting
// a caller advance it would let them settle commissions early. See ADR-023.

const (
	// maintainBatch is the number of commissions handled per transaction.
	maintainBatch = 200
	// maxMaintainRounds bounds how many batches one Maintain call processes.
	//
	// The bound is deliberate: Maintain is normally driven by a per-minute cron
	// job, so a large backlog should be worked through over several ticks
	// rather than holding the job past its timeout.
	maxMaintainRounds = 100
)

// OnOrderReceived handles "the buyer confirmed receipt".
//
// It fixes the settlement time of pending commissions at "receipt time plus the
// freeze window snapshotted when the commission was accrued". Note that the
// freeze window comes from each commission's own snapshot rather than from the
// current configuration, so changing configuration never retroactively moves a
// settlement date the agent has already been promised (see ADR-005).
//
// This event is idempotent and first-wins: a repeated delivery cannot move the
// settlement date, which is what stops a agent from replaying a receipt
// to unlock their commission early.
//
// ReceivedAt is taken on trust because only the order system knows when the
// buyer confirmed receipt. It is validated for shape, not for plausibility: a
// backfilled receipt genuinely can predate the moment this ledger learned about
// the payment, and rejecting it would break legitimate backfill. An earlier
// ReceivedAt therefore does unlock earlier — that is the intended semantics of
// the field, not a loophole.
func (l *Ledger) OnOrderReceived(ctx context.Context, ev OrderReceivedEvent) (ReceiveResult, error) {
	if err := ev.validate(); err != nil {
		return ReceiveResult{}, err
	}
	at := ev.ReceivedAt
	if at.IsZero() {
		at = l.clock.Now()
	}
	at = at.UTC()

	key := ev.orderKey()
	var result ReceiveResult
	err := l.store.Update(ctx, func(ctx context.Context, tx Tx) error {
		result = ReceiveResult{OrderKey: key}

		list, err := tx.CommissionsByOrder(ctx, key)
		if err != nil {
			return err
		}
		for _, c := range list {
			// Frozen commissions are included on purpose. Receipt is a fact
			// about the order, not about the risk decision, and the money is
			// still in the frozen bucket. Recording the settlement date now
			// means that unfreezing a commission later makes it settle
			// immediately instead of stranding it until someone re-delivers
			// the receipt event.
			if c.State != CommissionPending && c.State != CommissionFrozen {
				continue
			}
			if !c.AvailableAt.IsZero() {
				continue
			}
			availableAt := at.AddDate(0, 0, c.FreezeDays)
			if _, err := tx.SetCommissionAvailableAt(ctx, c.ID, availableAt, c.Version); err != nil {
				if errors.Is(err, ErrConflict) {
					// A concurrent delivery already handled it. Skipping is
					// correct: the first writer wins by design.
					continue
				}
				return err
			}
			result.Updated++
		}
		return nil
	})
	if err != nil {
		return ReceiveResult{}, err
	}
	return result, nil
}

// Maintain advances everything that is driven by time.
//
// It is the library's single heartbeat: it settles pending commissions whose
// freeze window has elapsed into the withdrawable balance. Integrations only
// need to hang it off their own scheduler; they do not need to know how many
// internal phases exist.
//
// # Blast radius
//
// A failure in one tenant does not stop the others: per-tenant errors are
// collected and returned together. One corrupt record therefore cannot silently
// halt settlement for the whole installation.
//
// A pending commission whose outstanding amount is not positive is quarantined
// rather than treated as a fatal error. It is reported in
// MaintainResult.Quarantined, logged, and flagged by SelfCheck, while healthy
// commissions in the same run still settle. Aborting the batch would let a
// single bad row freeze every agent's payout.
//
// # Work bound
//
// One call processes at most maxMaintainRounds x maintainBatch commissions;
// anything left over waits for the next tick.
func (l *Ledger) Maintain(ctx context.Context) (MaintainResult, error) {
	now := l.clock.Now().UTC()

	var (
		total  MaintainResult
		failed []error
	)
	tenants, err := l.tenants(ctx)
	if err != nil {
		return total, err
	}
	for _, tenantID := range tenants {
		part, err := l.maintainTenant(ctx, tenantID, now)
		total.Quarantined = append(total.Quarantined, part.Quarantined...)
		total.SettledCount += part.SettledCount
		if amount, addErr := total.SettledAmount.Add(part.SettledAmount); addErr != nil {
			return total, addErr
		} else {
			total.SettledAmount = amount
		}
		if err != nil {
			failed = append(failed, fmt.Errorf("tenant %d: %w", tenantID, err))
		}
	}
	if len(failed) > 0 {
		return total, errors.Join(failed...)
	}
	if total.SettledCount > 0 || len(total.Quarantined) > 0 {
		l.log.InfoContext(ctx, "maintain finished",
			"settled_count", total.SettledCount,
			"settled_amount", total.SettledAmount.String(),
			"quarantined", len(total.Quarantined))
	}
	return total, nil
}

func (l *Ledger) tenants(ctx context.Context) ([]int64, error) {
	var out []int64
	err := l.store.View(ctx, func(ctx context.Context, r Reader) error {
		var err error
		out, err = r.Tenants(ctx)
		return err
	})
	return out, err
}

func (l *Ledger) maintainTenant(ctx context.Context, tenantID int64, now time.Time) (MaintainResult, error) {
	var (
		out MaintainResult
		// A quarantined commission stays in the settlement index (nothing
		// changed about it), so the next round would find it again. The set
		// keeps the report honest without silently dropping it from the index.
		seenQuarantine = make(map[int64]struct{})
	)
	for round := 0; round < maxMaintainRounds; round++ {
		// Each batch is its own transaction: batches do not share a lock, and
		// the impact of one failure stays bounded.
		var (
			roundCount      int
			roundAmount     Money
			roundQuarantine []int64
		)
		err := l.store.Update(ctx, func(ctx context.Context, tx Tx) error {
			// The store may retry the whole function, so per-batch counters are
			// reset on every entry.
			roundCount = 0
			roundAmount = 0
			roundQuarantine = nil

			due, err := tx.DueCommissions(ctx, tenantID, now, maintainBatch)
			if err != nil {
				return err
			}
			for _, c := range due {
				outstanding := c.OutstandingAmount()
				if c.Amount <= 0 || outstanding <= 0 {
					// A pending commission with nothing left to settle is a
					// corrupt record. Quarantine it: report and move on rather
					// than aborting the batch.
					roundQuarantine = append(roundQuarantine, c.ID)
					continue
				}
				settled, err := l.settleOne(ctx, tx, c, outstanding, now)
				if err != nil {
					if errors.Is(err, ErrConflict) {
						// Already handled concurrently: no progress, so the
						// loop cannot spin on it.
						continue
					}
					return err
				}
				if settled {
					roundCount++
					amount, err := roundAmount.Add(outstanding)
					if err != nil {
						return err
					}
					roundAmount = amount
				}
			}
			return nil
		})
		if err != nil {
			return out, err
		}

		out.SettledCount += roundCount
		if amount, err := out.SettledAmount.Add(roundAmount); err != nil {
			return out, err
		} else {
			out.SettledAmount = amount
		}
		for _, id := range roundQuarantine {
			if _, dup := seenQuarantine[id]; dup {
				continue
			}
			seenQuarantine[id] = struct{}{}
			out.Quarantined = append(out.Quarantined, id)
			l.log.WarnContext(ctx, "commission quarantined during settlement",
				"tenant_id", tenantID, "commission_id", id)
		}

		// No progress means either nothing is due or every candidate hit a
		// conflict. Both mean stop, otherwise this becomes a spin loop.
		if roundCount == 0 {
			break
		}
	}
	return out, nil
}

// settleOne moves one due commission into the withdrawable balance.
//
// outstanding is the part of the commission that has not been clawed back. It
// is passed in rather than recomputed so that the amount moved is exactly the
// amount the caller reported and counted.
func (l *Ledger) settleOne(ctx context.Context, tx Tx, c Commission, outstanding Money, now time.Time) (bool, error) {
	if _, err := tx.TransitionCommission(ctx, c.ID, CommissionPending, CommissionSettled, c.Version); err != nil {
		return false, err
	}

	key := UserKey{TenantID: c.Key.TenantID, UserID: c.AgentUserID}
	acct, err := tx.Account(ctx, key)
	if err != nil {
		return false, err
	}
	acct.Key = key

	if acct.Frozen < outstanding {
		return false, fmt.Errorf("%w: account %s frozen balance %s is less than commission %s",
			ErrInsufficientBalance, key, acct.Frozen, outstanding)
	}
	if acct.Frozen, err = acct.Frozen.Sub(outstanding); err != nil {
		return false, err
	}
	if acct.Available, err = acct.Available.Add(outstanding); err != nil {
		return false, err
	}
	acct.UpdatedAt = now

	saved, err := tx.PutAccount(ctx, acct)
	if err != nil {
		return false, err
	}

	if _, err := tx.AppendLedger(ctx, LedgerEntry{
		Key:              key,
		BizType:          LedgerSettle,
		BizID:            strconv.FormatInt(c.ID, 10),
		DeltaFrozen:      -outstanding,
		DeltaAvailable:   outstanding,
		AfterFrozen:      saved.Frozen,
		AfterAvailable:   saved.Available,
		AfterWithdrawing: saved.Withdrawing,
		AfterWithdrawn:   saved.Withdrawn,
		Remark:           "settle order " + c.Key.OrderID + " layer " + strconv.Itoa(c.Layer),
		CreatedAt:        now,
	}); err != nil {
		return false, err
	}
	return true, nil
}
