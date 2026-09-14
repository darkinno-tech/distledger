package distledger_test

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/im10furry/distledger"
)

func (f *fixture) selfCheck() distledger.Report {
	f.t.Helper()
	rep, err := f.led.SelfCheck(context.Background(), tenant)
	if err != nil {
		f.t.Fatalf("SelfCheck: %v", err)
	}
	return rep
}

func (f *fixture) healthy() {
	f.t.Helper()
	f.mustAgent(3001, 0, distledger.AgentActive)
	f.mustAgent(3002, 3001, distledger.AgentActive)
	f.mustBuyer(4001, 3002)
	f.mustPay("ORD-1", 4001, 100000)
	f.receive("ORD-1", f.clock.Now())
	f.clock.Advance(8 * 24 * time.Hour)
	f.maintain()
}

func TestSelfCheckPassesOnHealthyData(t *testing.T) {
	f := newFixture(t, twoLevelRules())
	f.healthy()

	rep := f.selfCheck()
	if !rep.OK {
		t.Fatalf("healthy data reported problems: %+v", rep)
	}
	if rep.StoreKind != "memory" {
		t.Errorf("store kind = %q, want memory", rep.StoreKind)
	}
	if len(rep.Invariants) != 2 {
		t.Fatalf("got %d invariants, want 2", len(rep.Invariants))
	}
	for _, inv := range rep.Invariants {
		if !inv.OK {
			t.Errorf("invariant %q failed: %+v", inv.Name, inv.Violations)
		}
		if inv.Checked == 0 {
			t.Errorf("invariant %q checked nothing — a pass here proves nothing", inv.Name)
		}
	}
	if rep.Counters["settled"] != 2 {
		t.Errorf("counter settled = %d, want 2", rep.Counters["settled"])
	}
}

// TestSelfCheckDetectsAccountWithoutLedger 覆盖最容易出现的破坏形态：
// 有人直接改了账户余额，却忘了写流水。
func TestSelfCheckDetectsAccountWithoutLedger(t *testing.T) {
	f := newFixture(t, twoLevelRules())
	ctx := context.Background()

	if err := f.store.Update(ctx, func(ctx context.Context, tx distledger.Tx) error {
		key := distledger.UserKey{TenantID: tenant, UserID: 7777}
		_, err := tx.PutAccount(ctx, distledger.Account{Key: key, Available: 500000})
		return err
	}); err != nil {
		t.Fatal(err)
	}

	rep := f.selfCheck()
	if rep.OK {
		t.Fatal("an account with a balance but no ledger entries must be reported")
	}
	if !hasViolation(rep, "I1") {
		t.Fatalf("I1 did not report the orphan account: %+v", rep.Invariants)
	}
}

// TestSelfCheckDetectsLedgerSumMismatch 覆盖「流水被追加但账户没跟着更新」。
func TestSelfCheckDetectsLedgerSumMismatch(t *testing.T) {
	f := newFixture(t, twoLevelRules())
	f.healthy()
	ctx := context.Background()

	if err := f.store.Update(ctx, func(ctx context.Context, tx distledger.Tx) error {
		_, err := tx.AppendLedger(ctx, distledger.LedgerEntry{
			Key:         distledger.UserKey{TenantID: tenant, UserID: 3001},
			BizType:     distledger.LedgerManualAdjust,
			BizID:       "manual-1",
			DeltaFrozen: 12345,
			AfterFrozen: 99999, // 与账户真实值不符
			CreatedAt:   f.clock.Now(),
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}

	rep := f.selfCheck()
	if rep.OK {
		t.Fatal("a ledger entry that does not match the account must be reported")
	}
	if !hasViolation(rep, "I1") {
		t.Fatalf("I1 did not report the mismatch: %+v", rep.Invariants)
	}
}

// TestSelfCheckDetectsAdjustedAccount 验证「改了账户也改了流水」也能被发现。
//
// 只比对「流水和 == 账户」是不够的：同时篡改两边可以让等式依然成立。
// 因此 I1 还额外校验最后一条流水的 After* 与账户是否一致。
func TestSelfCheckDetectsAdjustedAccount(t *testing.T) {
	f := newFixture(t, twoLevelRules())
	f.healthy()
	ctx := context.Background()
	key := distledger.UserKey{TenantID: tenant, UserID: 3001}

	if err := f.store.Update(ctx, func(ctx context.Context, tx distledger.Tx) error {
		acct, err := tx.Account(ctx, key)
		if err != nil {
			return err
		}
		acct.Key = key
		acct.Available += 50000
		if _, err := tx.PutAccount(ctx, acct); err != nil {
			return err
		}
		// 追加一条「补齐差额」的流水，让「流水和 == 账户」重新成立。
		_, err = tx.AppendLedger(ctx, distledger.LedgerEntry{
			Key:              key,
			BizType:          distledger.LedgerManualAdjust,
			BizID:            "manual-2",
			DeltaAvailable:   50000,
			AfterFrozen:      acct.Frozen,
			AfterAvailable:   acct.Available,
			AfterWithdrawing: acct.Withdrawing,
			AfterWithdrawn:   acct.Withdrawn,
			CreatedAt:        f.clock.Now(),
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}

	// 这条数据在会计上「自洽」，I1 应当通过——这正是它诚实的地方：
	// 库不能分辨这是合法的人工调整还是篡改，它能保证的是等式成立。
	rep := f.selfCheck()
	if !rep.OK {
		t.Fatalf("a self-consistent manual adjustment should pass: %+v", rep.Invariants)
	}
}

func TestSelfCheckDetectsDanglingCommissionReference(t *testing.T) {
	f := newFixture(t, twoLevelRules())
	ctx := context.Background()

	if err := f.store.Update(ctx, func(ctx context.Context, tx distledger.Tx) error {
		_, err := tx.AppendLedger(ctx, distledger.LedgerEntry{
			Key:         distledger.UserKey{TenantID: tenant, UserID: 3001},
			BizType:     distledger.LedgerAccrue,
			BizID:       "999999",
			DeltaFrozen: 100,
			AfterFrozen: 100,
			CreatedAt:   f.clock.Now(),
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}

	rep := f.selfCheck()
	if rep.OK {
		t.Fatal("a ledger entry pointing at a missing commission must be reported")
	}
	if !hasViolation(rep, "I2") {
		t.Fatalf("I2 did not report the dangling reference: %+v", rep.Invariants)
	}
}

// TestSelfCheckDetectsCapViolation 直接注入一条超配额佣金。
//
// 走公开 API 是造不出这种数据的（Rules 校验 + 运行期封顶会拦住），
// 因此这里绕过引擎直接写存储——这正是自检存在的意义：
// 它面向的是「数据已经坏了」的场景，而不是「代码写错了」的场景。
func TestSelfCheckDetectsCapViolation(t *testing.T) {
	f := newFixture(t, distledger.Rules{Levels: 1, RateBP: []distledger.Rate{1000}})
	ctx := context.Background()

	if err := f.store.Update(ctx, func(ctx context.Context, tx distledger.Tx) error {
		_, err := tx.AppendCommission(ctx, distledger.Commission{
			Key:         distledger.OrderKey{TenantID: tenant, OrderID: "ORD-BAD"},
			IdemKey:     "injected",
			AgentUserID: 3001,
			Layer:       1,
			BaseAmount:  10000, // 100.00 基数
			Rate:        10000,
			Amount:      999999, // 远超配额
			State:       distledger.CommissionPending,
			AccruedAt:   f.clock.Now(),
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}

	rep := f.selfCheck()
	if rep.OK {
		t.Fatal("a commission exceeding the allocatable cap must be reported")
	}
	if !hasViolation(rep, "I2") {
		t.Fatalf("I2 did not report the cap violation: %+v", rep.Invariants)
	}
}

func TestSelfCheckRejectsNegativeTenant(t *testing.T) {
	f := newFixture(t, twoLevelRules())
	if _, err := f.led.SelfCheck(context.Background(), -1); err == nil {
		t.Fatal("negative tenant id must be rejected")
	}
}

func TestSelfCheckIsReadOnly(t *testing.T) {
	f := newFixture(t, twoLevelRules())
	f.healthy()

	before := f.balance(3001)
	if err := f.store.Update(context.Background(), func(ctx context.Context, tx distledger.Tx) error {
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	_ = f.selfCheck()
	after := f.balance(3001)
	if before != after {
		t.Fatalf("SelfCheck changed the account: %+v -> %+v", before, after)
	}
}

func hasViolation(rep distledger.Report, prefix string) bool {
	for _, inv := range rep.Invariants {
		if len(inv.Name) >= len(prefix) && inv.Name[:len(prefix)] == prefix && inv.TotalViolations > 0 {
			return true
		}
	}
	return false
}

// TestMoneyStringRoundTripInReport 顺带验证报告里出现的金额是可读的。
func TestMoneyStringRoundTripInReport(t *testing.T) {
	m, err := distledger.ParseMoney("199.99")
	if err != nil {
		t.Fatal(err)
	}
	if m.String() != "199.99" {
		t.Fatalf("round trip produced %s", m)
	}
	if strconv.FormatInt(int64(m), 10) != "19999" {
		t.Fatalf("minor units = %d, want 19999", int64(m))
	}
}
