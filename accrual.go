package distledger

import (
	"context"
	"errors"
	"strconv"
	"time"
)

// 本文件是分佣引擎。它只做编排，不做判断：
// 「算多少」交给 RateResolver，「够不够格」交给 EligibilityChecker，
// 「钱能不能这么流」由状态机与这里的事务边界共同保证（见 ADR-002）。

// accrualBase 是一次分佣的计算单位。
//
// 未提供 Items 时，整笔订单是一个计算单位（itemID 为空串）；
// 提供了 Items 时，每个明细各自是一个计算单位，配额也各自独立。
type accrualBase struct {
	itemID string
	amount Money
}

// basesOf 把事件拆解为分佣计算单位。
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

// OnOrderPaid 处理「订单已支付」，完成归因、分佣计算、入账与记账。
//
// # 幂等
//
// 可以安全地重复投递。重复投递时返回 Replayed=true 且 Commissions 为空。
// 幂等性由「订单已有佣金」的短路判断与每条佣金的唯一幂等键双重保证。
//
// # 归因规则
//
// 只有当绑定关系在**支付时刻**已经存在且有效时才会归因。这条规则防的是
// 事后抢单：如果只看「当前绑定」，任何人只要在订单支付后把自己绑成买家
// 的推广人就能追认这笔收益。
//
// # 没有归因不是错误
//
// 买家没有推广归属时返回 Attributed=false 且 err 为 nil。订单系统不应该
// 因为「这单没有推广人」而收到一个失败。
//
// # 已知边界
//
// 幂等键是 (订单, 明细, 分销员, 层级) 的函数。因此如果在第一次投递之后
// 修改了费率配置再重放同一事件，可能产生第一次没有的佣金。生产环境中
// 请在订单产生前就固定费率；按订单冻结规则版本是后续版本的能力。
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
		// Store 的事务允许被重试，因此每次进入都必须从零重建结果，
		// 否则重试会导致结果被累加两次。
		result = AccrueResult{OrderKey: key}

		existing, err := tx.CommissionsByOrder(ctx, key)
		if err != nil {
			return err
		}
		if len(existing) > 0 {
			result.Replayed = true
			result.Attributed = true
			result.AgentUserID = layerOneAgent(existing)
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
		// 第 1 级不合格意味着归因本身无效，整单不再分佣。
		// 这与「中间层级不合格」的处理不同，见 accrueBase 的说明。
		if ok, reason := l.elig.Eligible(ctx, first, key); !ok {
			result.Skipped = append(result.Skipped, SkipReason{
				Layer: 1, AgentUserID: first.Key.UserID, Code: SkipIneligible, Detail: reason,
			})
			return nil
		}

		for _, base := range basesOf(ev) {
			if err := l.accrueBase(ctx, tx, ev, base, first.Key.UserID, &result); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return AccrueResult{}, err
	}
	return result, nil
}

// layerOneAgent 从已有佣金中找出第 1 级的分销员，用于重放时回填结果。
func layerOneAgent(list []Commission) int64 {
	for _, c := range list {
		if c.Layer == 1 {
			return c.AgentUserID
		}
	}
	return 0
}

// accrueBase 沿关系链向上为单个计算单位生成各级佣金。
//
// # 中间层级不合格时的处理
//
// 某一级不合格只跳过该级，链条继续向上。理由是：关系链是客观存在的拓扑，
// 而资格是个体的属性。如果让不合格节点截断链条，一个被临时禁用的人会
// 无声地吞掉他所有上级的收益，这类问题在生产中极难排查和安抚。
//
// 第 1 级是例外（见 OnOrderPaid）：它不合格意味着归因无效，整单不分佣。
func (l *Ledger) accrueBase(ctx context.Context, tx Tx, ev OrderPaidEvent, base accrualBase, firstAgentID int64, result *AccrueResult) error {
	order := ev.orderKey()

	// 配额按「计算单位」独立计算，而不是按整单：否则一笔多明细订单会
	// 让先处理的明细吃掉后处理明细的额度。
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
			if ok, reason := l.elig.Eligible(ctx, agent, order); !ok {
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

		// 配额封顶：即使费率配置被改坏，分出去的总额也不会超过基数上限。
		// 这是不变量 I2 的运行期保证。
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
				// 同一事务内不会重复，这里是防御性兜底：绝不因为一条重复
				// 记录而中断整单分佣。
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

// creditAccrual 把佣金计入账户的「待结算」桶，并追加一条资金流水。
//
// 记完账再写流水，是为了让流水里的 After* 反映变动后的真实余额——
// 对账时只看单条流水就能判断那一时刻的余额状态。
func (l *Ledger) creditAccrual(ctx context.Context, tx Tx, c Commission, now time.Time) error {
	key := UserKey{TenantID: c.Key.TenantID, UserID: c.AgentUserID}

	acct, err := tx.Account(ctx, key)
	if err != nil {
		return err
	}
	// 账户不存在时 Account 返回零值，其 Key 是空的，必须显式补上。
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

	_, err = tx.AppendLedger(ctx, LedgerEntry{
		Key:              key,
		BizType:          LedgerAccrue,
		BizID:            strconv.FormatInt(c.ID, 10),
		DeltaFrozen:      c.Amount,
		AfterFrozen:      saved.Frozen,
		AfterAvailable:   saved.Available,
		AfterWithdrawing: saved.Withdrawing,
		AfterWithdrawn:   saved.Withdrawn,
		Remark:           "order " + c.Key.OrderID + " layer " + strconv.Itoa(c.Layer),
		CreatedAt:        now,
	})
	return err
}
