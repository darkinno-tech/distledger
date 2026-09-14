package distledger

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// 本文件负责「冻结 → 结算」这一段。它是状态机真正被推动的地方。

const (
	// maintainBatch 是单次批处理的条数。
	maintainBatch = 200
	// maxMaintainRounds 限制单次 Maintain 的批次数，给工作量一个上界。
	//
	// 有上界是刻意的：Maintain 通常挂在每分钟一次的定时任务上，
	// 如果积压了几百万条到期佣金，一次调用应该做一部分并尽快返回，
	// 而不是把定时任务占住直到超时。
	maxMaintainRounds = 100
)

// OnOrderReceived 处理「买家已确认收货」。
//
// 它把待结算佣金的到账时间确定为「收货时间 + 入账时快照的冻结天数」。
// 注意冻结天数来自每条佣金自己的快照，而不是当前配置：这样修改全局
// 配置不会追溯改变已经承诺给分销员的到账时间（见 ADR-005）。
//
// 本事件幂等且先到先得：重复投递不会改变已经确定的到账时间。
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
			if c.State != CommissionPending || !c.AvailableAt.IsZero() {
				continue
			}
			availableAt := at.AddDate(0, 0, c.FreezeDays)
			if _, err := tx.SetCommissionAvailableAt(ctx, c.ID, availableAt, c.Version); err != nil {
				if errors.Is(err, ErrConflict) {
					// 并发的另一次收货投递已经处理过它，跳过即可。
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

// Maintain 推进所有「时间驱动」的状态。
//
// 这是库对外的唯一心跳：把冻结期已满的待结算佣金结算到可提现余额。
// 调用方只需要把它挂到自己的定时任务上，不需要理解内部有几个阶段。
//
// now 为零值时取 Clock 当前时间。
//
// # 工作量上界
//
// 单次调用最多处理 maxMaintainRounds × maintainBatch 条，超出的部分留给
// 下一次调用。这样积压不会让定时任务长时间占用资源。
func (l *Ledger) Maintain(ctx context.Context, now time.Time) (MaintainResult, error) {
	if now.IsZero() {
		now = l.clock.Now()
	}
	now = now.UTC()

	var total MaintainResult
	tenants, err := l.tenants(ctx)
	if err != nil {
		return total, err
	}
	for _, tenantID := range tenants {
		part, err := l.maintainTenant(ctx, tenantID, now)
		if err != nil {
			return total, err
		}
		total.SettledCount += part.SettledCount
		amount, err := total.SettledAmount.Add(part.SettledAmount)
		if err != nil {
			return total, err
		}
		total.SettledAmount = amount
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
	var out MaintainResult
	for round := 0; round < maxMaintainRounds; round++ {
		// 每批一个独立事务：批与批之间不共享锁，也让单次失败的影响有界。
		var (
			roundCount  int
			roundAmount Money
		)
		err := l.store.Update(ctx, func(ctx context.Context, tx Tx) error {
			// Update 允许被重试，因此批内计数每次进入都要清零。
			roundCount = 0
			roundAmount = 0

			due, err := tx.DueCommissions(ctx, tenantID, now, maintainBatch)
			if err != nil {
				return err
			}
			for _, c := range due {
				settled, err := l.settleOne(ctx, tx, c, now)
				if err != nil {
					if errors.Is(err, ErrConflict) {
						// 已被并发处理，不计入本批进度，避免死循环。
						continue
					}
					return err
				}
				if settled {
					roundCount++
					amount, err := roundAmount.Add(c.Amount)
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
		amount, err := out.SettledAmount.Add(roundAmount)
		if err != nil {
			return out, err
		}
		out.SettledAmount = amount

		// 没有任何进展说明要么已无到期项，要么全部落到冲突分支。
		// 两种情况都应该停止，否则会变成一个转不完的循环。
		if roundCount == 0 {
			break
		}
	}
	return out, nil
}

// settleOne 把一笔到期的待结算佣金推进到可提现余额。
func (l *Ledger) settleOne(ctx context.Context, tx Tx, c Commission, now time.Time) (bool, error) {
	// 防御性校验：待结算的佣金金额必须为正。
	// 出现非正数说明账务数据已被破坏，此时宁可让 Maintain 失败并告警，
	// 也不能「聪明地」跳过——静默跳过等于把钱藏起来。
	if c.Amount <= 0 {
		return false, fmt.Errorf("%w: pending commission %d has non-positive amount %s",
			ErrInvalidConfig, c.ID, c.Amount)
	}

	if _, err := tx.TransitionCommission(ctx, c.ID, CommissionPending, CommissionSettled, c.Version); err != nil {
		return false, err
	}

	key := UserKey{TenantID: c.Key.TenantID, UserID: c.AgentUserID}
	acct, err := tx.Account(ctx, key)
	if err != nil {
		return false, err
	}
	acct.Key = key

	if acct.Frozen < c.Amount {
		return false, fmt.Errorf("%w: account %s frozen balance %s is less than commission %s",
			ErrInvalidConfig, key, acct.Frozen, c.Amount)
	}
	if acct.Frozen, err = acct.Frozen.Sub(c.Amount); err != nil {
		return false, err
	}
	if acct.Available, err = acct.Available.Add(c.Amount); err != nil {
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
		DeltaFrozen:      mustNeg(c.Amount),
		DeltaAvailable:   c.Amount,
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

// mustNeg 取负。调用点已保证金额为正且远未接近 int64 边界。
func mustNeg(m Money) Money { return -m }
