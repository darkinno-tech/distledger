package distledger

import (
	"context"
	"fmt"
	"sort"
	"strconv"
)

// maxViolationsPerCheck 限制每个不变量记录的证据条数。
//
// 报告不是日志：它的用途是「一眼看出有没有问题、问题长什么样」。
// 记录一千万条违规与记录二十条违规，对定位问题的价值是一样的，
// 但前者会把内存和调用方的日志系统打挂。
const maxViolationsPerCheck = 20

// InvariantResult 是一项不变量的检查结果。
type InvariantResult struct {
	// Name 是不变量的稳定标识。
	Name string
	// OK 报告是否全部通过。
	OK bool
	// Checked 是本次检查的对象数量，用于判断「通过」是不是因为「没查」。
	Checked int
	// Violations 是违规样本，最多 maxViolationsPerCheck 条。
	Violations []string
	// TotalViolations 是违规总数，可能大于 len(Violations)。
	TotalViolations int
}

func (r *InvariantResult) add(format string, args ...any) {
	r.TotalViolations++
	if len(r.Violations) < maxViolationsPerCheck {
		r.Violations = append(r.Violations, fmt.Sprintf(format, args...))
	}
	r.OK = false
}

// Report 是 SelfCheck 的结果。
type Report struct {
	TenantID int64
	// OK 只有在全部不变量通过、且没有表结构问题时才为 true。
	OK bool
	// StoreKind 是存储类型标识，例如 "memory"。
	StoreKind string
	// SchemaIssues 是存储层报告的表结构问题（内存存储不会产生）。
	SchemaIssues []string
	// Invariants 是各项不变量的检查结果。
	Invariants []InvariantResult
	// Counters 是各状态佣金笔数，用于发现「卡住不动」的数据。
	Counters map[string]int64
}

// SelfCheck 对某个租户执行账务自检。
//
// 这是接入排障的唯一入口：它一次性回答「账本是否守恒、有没有负余额、
// 分佣是否超出配额、有没有指向不存在记录的流水、有没有卡住的数据」。
//
// 它只读，不会修改任何数据。
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

// checkLedgerConservation 校验不变量 I1。
//
// 三项检查合在一起才构成完整的 I1：
//
//  1. 账户的每个资金桶等于该账户全部流水对应增量的和。
//  2. 最后一条流水的 After* 等于账户当前值（防「改了账户却忘了记账」）。
//  3. 不存在负的资金桶。
//
// 只做第 1 项是不够的：如果有人同时错误地修改了账户与流水，第 1 项
// 仍然可能通过，而第 2 项会立刻暴露出来。
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
				// 没有流水的账户必须全零：账户只能由流水驱动产生。
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

// allocKey 是配额校验的归组键。
type allocKey struct {
	orderKey    OrderKey
	orderItemID string
}

type allocAgg struct {
	base   Money
	amount Money
}

// checkAllocationCap 校验不变量 I2 与佣金-流水之间的引用完整性。
//
// 它同时回答三个问题：
//
//  1. 每条佣金是否都有对应的入账流水（防「记了佣金却没动钱」）。
//  2. 每条入账流水是否都指向存在的佣金（防悬空引用）。
//  3. 每个「计算单位」分出的佣金总额是否在配额之内。
//
// 第 1 项必须靠**直接枚举佣金**来完成。早期实现只顺着流水反查佣金，
// 结果是「记了佣金但没动钱」这一最需要被发现的破坏形态恰好不可见。
func (l *Ledger) checkAllocationCap(ctx context.Context, r Reader, tenantID int64) (InvariantResult, error) {
	res := InvariantResult{Name: "I2: commission ledger linkage and allocation cap", OK: true}
	groups := make(map[allocKey]*allocAgg)

	// 先扫一遍租户流水，收集全部入账引用。
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
			if e.BizType != LedgerAccrue {
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

	// 再直接枚举佣金：既校验反向引用，也做配额归组。
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
				res.add("commission %d (order %s layer %d, %s) has no accrual ledger entry",
					c.ID, c.Key.OrderID, c.Layer, c.Amount)
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

	// 校验正向引用：流水指向的佣金必须存在。
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

	// 最后校验配额。
	keys := make([]allocKey, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	// 排序是为了让报告可复现：同样的数据永远得到同样的违规顺序。
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
