package memory

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/im10furry/distledger"
)

// 本文件是内存 Store 的**内部**测试：它会直接检查 pendingDue 这类派生索引。
//
// 为什么必须检查索引本身而不是只看行为：读取路径带「新鲜度校验」，会把
// 失效的索引项静默跳过。这保护了正确性，但也意味着索引错位不会表现为
// 错误结果——它只会表现为越来越慢。回滚逻辑一旦写错，问题会以性能事故
// 的形式在几个月后出现，那时几乎无法归因。

func newStore(t *testing.T) *Store {
	t.Helper()
	s := New()
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func agentKey(userID int64) distledger.UserKey {
	return distledger.UserKey{TenantID: 0, UserID: userID}
}

func seedAgent(t *testing.T, s *Store, userID int64) distledger.Agent {
	t.Helper()
	var out distledger.Agent
	err := s.Update(context.Background(), func(ctx context.Context, tx distledger.Tx) error {
		var err error
		out, err = tx.PutAgent(ctx, distledger.Agent{
			Key: agentKey(userID), Status: distledger.AgentActive, Depth: 1,
		})
		return err
	})
	if err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	return out
}

func seedCommission(t *testing.T, s *Store, orderID string, agentID int64) distledger.Commission {
	t.Helper()
	var out distledger.Commission
	err := s.Update(context.Background(), func(ctx context.Context, tx distledger.Tx) error {
		var err error
		out, err = tx.AppendCommission(ctx, distledger.Commission{
			Key:         distledger.OrderKey{TenantID: 0, OrderID: orderID},
			IdemKey:     "idem-" + orderID,
			AgentUserID: agentID,
			Layer:       1,
			BaseAmount:  10000,
			Rate:        500,
			Amount:      500,
			State:       distledger.CommissionPending,
			AccruedAt:   time.Unix(0, 0).UTC(),
		})
		return err
	})
	if err != nil {
		t.Fatalf("seed commission: %v", err)
	}
	return out
}

func TestPutAgentRejectsNegativeVersionOnCreate(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	err := s.Update(ctx, func(ctx context.Context, tx distledger.Tx) error {
		_, err := tx.PutAgent(ctx, distledger.Agent{
			Key: agentKey(1), Version: 5, Status: distledger.AgentActive,
		})
		return err
	})
	if !errors.Is(err, distledger.ErrConflict) {
		t.Fatalf("expected ErrConflict, got %v", err)
	}
}

func TestPutAgentOptimisticLock(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	agent := seedAgent(t, s, 1) // Version == 1

	// 带着「我以为它还不存在」的版本号写入必须失败。
	stale := agent
	stale.Version = 0
	err := s.Update(ctx, func(ctx context.Context, tx distledger.Tx) error {
		_, err := tx.PutAgent(ctx, stale)
		return err
	})
	if !errors.Is(err, distledger.ErrConflict) {
		t.Fatalf("expected ErrConflict on stale version, got %v", err)
	}

	// 用真正陈旧的副本（版本落后于存储）写入也必须失败。
	if err := s.Update(ctx, func(ctx context.Context, tx distledger.Tx) error {
		cur, err := tx.Agent(ctx, agentKey(1))
		if err != nil {
			return err
		}
		cur.Status = distledger.AgentDisabled
		_, err = tx.PutAgent(ctx, cur) // 存储版本变为 2
		return err
	}); err != nil {
		t.Fatalf("update with fresh version failed: %v", err)
	}
	err = s.Update(ctx, func(ctx context.Context, tx distledger.Tx) error {
		_, err := tx.PutAgent(ctx, agent) // 仍然带着 Version 1
		return err
	})
	if !errors.Is(err, distledger.ErrConflict) {
		t.Fatalf("expected ErrConflict when writing an outdated copy, got %v", err)
	}

	// 用正确版本重新写入应当成功。
	err = s.Update(ctx, func(ctx context.Context, tx distledger.Tx) error {
		cur, err := tx.Agent(ctx, agentKey(1))
		if err != nil {
			return err
		}
		cur.Status = distledger.AgentActive
		_, err = tx.PutAgent(ctx, cur)
		return err
	})
	if err != nil {
		t.Fatalf("update with fresh version failed: %v", err)
	}

	var got distledger.Agent
	if err := s.View(ctx, func(ctx context.Context, r distledger.Reader) error {
		var err error
		got, err = r.Agent(ctx, agentKey(1))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// 三次成功的写入：创建(1) → 停用(2) → 重新启用(3)。
	if got.Status != distledger.AgentActive || got.Version != 3 {
		t.Fatalf("unexpected agent after updates: %+v", got)
	}
}

func TestPutAccountRejectsNegativeBucket(t *testing.T) {
	s := newStore(t)
	err := s.Update(context.Background(), func(ctx context.Context, tx distledger.Tx) error {
		_, err := tx.PutAccount(ctx, distledger.Account{
			Key: agentKey(1), Frozen: -1,
		})
		return err
	})
	if !errors.Is(err, distledger.ErrInvalidArgument) {
		t.Fatalf("negative bucket must be rejected, got %v", err)
	}
}

func TestAppendCommissionRejectsDuplicateIdemKey(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	seedCommission(t, s, "ORD-1", 1)

	err := s.Update(ctx, func(ctx context.Context, tx distledger.Tx) error {
		_, err := tx.AppendCommission(ctx, distledger.Commission{
			Key:         distledger.OrderKey{TenantID: 0, OrderID: "ORD-1"},
			IdemKey:     "idem-ORD-1", // 与已存在的完全相同
			AgentUserID: 1, Layer: 1, BaseAmount: 10000, Rate: 500, Amount: 500,
			State: distledger.CommissionPending,
		})
		return err
	})
	if !errors.Is(err, distledger.ErrDuplicate) {
		t.Fatalf("expected ErrDuplicate, got %v", err)
	}

	var count int
	if err := s.View(ctx, func(ctx context.Context, r distledger.Reader) error {
		list, err := r.CommissionsByOrder(ctx, distledger.OrderKey{TenantID: 0, OrderID: "ORD-1"})
		count = len(list)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("duplicate insert created a second commission (count=%d)", count)
	}
}

func TestIdemKeyIsScopedPerTenant(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	// 不同租户可以合法地使用相同的幂等键字符串。
	for _, tn := range []int64{1, 2} {
		if err := s.Update(ctx, func(ctx context.Context, tx distledger.Tx) error {
			_, err := tx.AppendCommission(ctx, distledger.Commission{
				Key:         distledger.OrderKey{TenantID: tn, OrderID: "ORD-1"},
				IdemKey:     "same-key",
				AgentUserID: 1, Layer: 1, BaseAmount: 10000, Rate: 500, Amount: 500,
				State: distledger.CommissionPending,
			})
			return err
		}); err != nil {
			t.Fatalf("tenant %d: %v", tn, err)
		}
	}
}

// ── 回滚 ────────────────────────────────────────────────────────────────

func TestRollbackRestoresEveryEntityKind(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	agent := seedAgent(t, s, 1)
	seedCommission(t, s, "ORD-1", 1)

	sentinel := errors.New("boom")
	err := s.Update(ctx, func(ctx context.Context, tx distledger.Tx) error {
		if _, err := tx.PutAgent(ctx, func() distledger.Agent {
			a := agent
			a.Status = distledger.AgentDisabled
			return a
		}()); err != nil {
			return err
		}
		if _, err := tx.PutAccount(ctx, distledger.Account{
			Key: agentKey(1), Available: 12345,
		}); err != nil {
			return err
		}
		if _, err := tx.AppendCommission(ctx, distledger.Commission{
			Key:         distledger.OrderKey{TenantID: 0, OrderID: "ORD-2"},
			IdemKey:     "idem-ORD-2",
			AgentUserID: 1, Layer: 1, BaseAmount: 10000, Rate: 500, Amount: 500,
			State: distledger.CommissionPending,
		}); err != nil {
			return err
		}
		if _, err := tx.AppendLedger(ctx, distledger.LedgerEntry{
			Key: agentKey(1), BizType: distledger.LedgerAccrue, BizID: "x", DeltaFrozen: 1,
		}); err != nil {
			return err
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected the sentinel error to propagate, got %v", err)
	}

	if err := s.View(ctx, func(ctx context.Context, r distledger.Reader) error {
		got, err := r.Agent(ctx, agentKey(1))
		if err != nil {
			return err
		}
		if got.Status != distledger.AgentActive {
			t.Errorf("agent status = %s, want active (rollback failed)", got.Status)
		}
		acct, err := r.Account(ctx, agentKey(1))
		if err != nil {
			return err
		}
		if acct.Available != 0 {
			t.Errorf("account available = %s, want 0 (rollback failed)", acct.Available)
		}
		list, err := r.CommissionsByOrder(ctx, distledger.OrderKey{TenantID: 0, OrderID: "ORD-2"})
		if err != nil {
			return err
		}
		if len(list) != 0 {
			t.Errorf("rolled-back commission is still visible (%d rows)", len(list))
		}
		entries, err := r.LedgerEntries(ctx, agentKey(1), distledger.LedgerQuery{})
		if err != nil {
			return err
		}
		if len(entries) != 0 {
			t.Errorf("rolled-back ledger entry is still visible (%d rows)", len(entries))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestRollbackFreesIdemKey 保证回滚后同一个幂等键可以再次使用。
//
// 如果回滚没有清理幂等索引，一次失败的事务会让那笔业务**永久无法入账**：
// 后续重试全部被判为「重复」而静默丢弃。这是一个极其隐蔽的丢钱方式。
func TestRollbackFreesIdemKey(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	err := s.Update(ctx, func(ctx context.Context, tx distledger.Tx) error {
		if _, err := tx.AppendCommission(ctx, distledger.Commission{
			Key:         distledger.OrderKey{TenantID: 0, OrderID: "ORD-1"},
			IdemKey:     "idem-ORD-1",
			AgentUserID: 1, Layer: 1, BaseAmount: 10000, Rate: 500, Amount: 500,
			State: distledger.CommissionPending,
		}); err != nil {
			return err
		}
		return errors.New("boom")
	})
	if err == nil {
		t.Fatal("expected the transaction to fail")
	}

	// 重试必须成功，而不是被判为重复。
	var out distledger.Commission
	if err := s.Update(ctx, func(ctx context.Context, tx distledger.Tx) error {
		var err error
		out, err = tx.AppendCommission(ctx, distledger.Commission{
			Key:         distledger.OrderKey{TenantID: 0, OrderID: "ORD-1"},
			IdemKey:     "idem-ORD-1",
			AgentUserID: 1, Layer: 1, BaseAmount: 10000, Rate: 500, Amount: 500,
			State: distledger.CommissionPending,
		})
		return err
	}); err != nil {
		t.Fatalf("retry after rollback failed: %v", err)
	}
	if out.ID == 0 {
		t.Fatal("retry did not assign an ID")
	}
}

// TestRollbackRestoresDueIndexAfterMiddleInsert 是本文件里最关键的测试。
//
// pendingDue 按 (到期时间, ID) 有序。当新到的佣金到期时间早于已有条目时，
// 插入发生在中间，元素会被搬移。此时「按长度截断」的回滚方式会恢复出
// 错位的内容。本测试通过直接比对索引快照来捕捉这类错误。
func TestRollbackRestoresDueIndexAfterMiddleInsert(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	// 先放两个到期时间较晚的条目。
	late1 := seedCommission(t, s, "LATE-1", 1)
	late2 := seedCommission(t, s, "LATE-2", 1)
	setAvailable := func(id int64, version int64, at time.Time) {
		t.Helper()
		if err := s.Update(ctx, func(ctx context.Context, tx distledger.Tx) error {
			_, err := tx.SetCommissionAvailableAt(ctx, id, at, version)
			return err
		}); err != nil {
			t.Fatalf("SetCommissionAvailableAt(%d): %v", id, err)
		}
	}
	setAvailable(late1.ID, late1.Version, base.Add(20*24*time.Hour))
	setAvailable(late2.ID, late2.Version, base.Add(30*24*time.Hour))

	before := slices.Clone(s.d.pendingDue)
	if len(before) != 2 {
		t.Fatalf("index has %d entries, want 2", len(before))
	}

	// 现在做一次「中间插入 + 失败」的事务。
	early := seedCommission(t, s, "EARLY-1", 1)
	err := s.Update(ctx, func(ctx context.Context, tx distledger.Tx) error {
		if _, err := tx.SetCommissionAvailableAt(ctx, early.ID, base.Add(24*time.Hour), early.Version); err != nil {
			return err
		}
		return errors.New("boom")
	})
	if err == nil {
		t.Fatal("expected the transaction to fail")
	}

	after := s.d.pendingDue
	if !slices.Equal(before, after) {
		t.Fatalf("due index was not restored after rollback:\n before=%v\n after =%v", before, after)
	}

	// 佣金本身的 AvailableAt 也必须回到零值。
	if err := s.View(ctx, func(ctx context.Context, r distledger.Reader) error {
		c, err := r.Commission(ctx, early.ID)
		if err != nil {
			return err
		}
		if !c.AvailableAt.IsZero() {
			t.Errorf("commission AvailableAt = %s, want zero after rollback", c.AvailableAt)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestRollbackAfterAppendThenMiddleInsert 覆盖混合场景：
// 同一事务里先尾部追加、再中间插入、然后失败。
func TestRollbackAfterAppendThenMiddleInsert(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	existing := seedCommission(t, s, "MID-1", 1)
	if err := s.Update(ctx, func(ctx context.Context, tx distledger.Tx) error {
		_, err := tx.SetCommissionAvailableAt(ctx, existing.ID, base.Add(10*24*time.Hour), existing.Version)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	appended := seedCommission(t, s, "APPEND-1", 1)
	middle := seedCommission(t, s, "MID-0", 1)
	before := slices.Clone(s.d.pendingDue)

	err := s.Update(ctx, func(ctx context.Context, tx distledger.Tx) error {
		// 尾部追加（到期时间最晚）。
		if _, err := tx.SetCommissionAvailableAt(ctx, appended.ID,
			base.Add(40*24*time.Hour), appended.Version); err != nil {
			return err
		}
		// 中间插入（到期时间最早）。
		if _, err := tx.SetCommissionAvailableAt(ctx, middle.ID,
			base.Add(1*24*time.Hour), middle.Version); err != nil {
			return err
		}
		return errors.New("boom")
	})
	if err == nil {
		t.Fatal("expected the transaction to fail")
	}

	if !slices.Equal(before, s.d.pendingDue) {
		t.Fatalf("due index was not restored:\n before=%v\n after =%v", before, s.d.pendingDue)
	}
}

func TestTransitionCommissionRemovesDueIndexEntry(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	c := seedCommission(t, s, "ORD-1", 1)
	if err := s.Update(ctx, func(ctx context.Context, tx distledger.Tx) error {
		_, err := tx.SetCommissionAvailableAt(ctx, c.ID, base, c.Version)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(s.d.pendingDue) != 1 {
		t.Fatalf("index has %d entries, want 1", len(s.d.pendingDue))
	}

	if err := s.Update(ctx, func(ctx context.Context, tx distledger.Tx) error {
		cur, err := tx.Commission(ctx, c.ID)
		if err != nil {
			return err
		}
		_, err = tx.TransitionCommission(ctx, c.ID,
			distledger.CommissionPending, distledger.CommissionSettled, cur.Version)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(s.d.pendingDue) != 0 {
		t.Fatalf("settled commission left %d stale index entries", len(s.d.pendingDue))
	}
}

func TestTransitionRejectsIllegalMove(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	c := seedCommission(t, s, "ORD-1", 1)

	err := s.Update(ctx, func(ctx context.Context, tx distledger.Tx) error {
		// Pending -> Withdrawn 是非法迁移。
		_, err := tx.TransitionCommission(ctx, c.ID,
			distledger.CommissionPending, distledger.CommissionWithdrawn, c.Version)
		return err
	})
	if !errors.Is(err, distledger.ErrIllegalTransition) {
		t.Fatalf("expected ErrIllegalTransition, got %v", err)
	}
}

func TestTransitionReportsConflictOnWrongCurrentState(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	c := seedCommission(t, s, "ORD-1", 1)

	err := s.Update(ctx, func(ctx context.Context, tx distledger.Tx) error {
		// 当前状态是 Pending，却按 Settled 发起迁移。
		_, err := tx.TransitionCommission(ctx, c.ID,
			distledger.CommissionSettled, distledger.CommissionWithdrawn, c.Version)
		return err
	})
	if !errors.Is(err, distledger.ErrConflict) {
		t.Fatalf("expected ErrConflict, got %v", err)
	}
}

// ── 事务语义 ────────────────────────────────────────────────────────────

func TestNestedTransactionIsRejected(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	err := s.Update(ctx, func(ctx context.Context, tx distledger.Tx) error {
		return s.Update(ctx, func(ctx context.Context, tx distledger.Tx) error { return nil })
	})
	if err == nil {
		t.Fatal("nested Update must be rejected instead of deadlocking")
	}

	err = s.Update(ctx, func(ctx context.Context, tx distledger.Tx) error {
		return s.View(ctx, func(ctx context.Context, r distledger.Reader) error { return nil })
	})
	if err == nil {
		t.Fatal("View inside Update must be rejected instead of deadlocking")
	}
}

func TestReadYourWritesInsideTransaction(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	if err := s.Update(ctx, func(ctx context.Context, tx distledger.Tx) error {
		if _, err := tx.PutAgent(ctx, distledger.Agent{
			Key: agentKey(1), Status: distledger.AgentActive, Depth: 1,
		}); err != nil {
			return err
		}
		got, err := tx.Agent(ctx, agentKey(1))
		if err != nil {
			return err
		}
		if got.Status != distledger.AgentActive {
			t.Errorf("uncommitted write is not visible inside the same transaction")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestUncommittedWritesAreNotVisibleOutside(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	_ = s.Update(ctx, func(ctx context.Context, tx distledger.Tx) error {
		if _, err := tx.PutAgent(ctx, distledger.Agent{
			Key: agentKey(1), Status: distledger.AgentActive, Depth: 1,
		}); err != nil {
			return err
		}
		return errors.New("boom")
	})

	err := s.View(ctx, func(ctx context.Context, r distledger.Reader) error {
		_, err := r.Agent(ctx, agentKey(1))
		return err
	})
	if !errors.Is(err, distledger.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestClosedStoreRejectsOperations(t *testing.T) {
	s := New()
	ctx := context.Background()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close must be idempotent: %v", err)
	}
	if err := s.View(ctx, func(ctx context.Context, r distledger.Reader) error { return nil }); !errors.Is(err, distledger.ErrClosed) {
		t.Fatalf("View on closed store = %v, want ErrClosed", err)
	}
	if err := s.Update(ctx, func(ctx context.Context, tx distledger.Tx) error { return nil }); !errors.Is(err, distledger.ErrClosed) {
		t.Fatalf("Update on closed store = %v, want ErrClosed", err)
	}
}

func TestContextCancellationIsHonoured(t *testing.T) {
	s := newStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.Update(ctx, func(ctx context.Context, tx distledger.Tx) error { return nil }); err == nil {
		t.Fatal("cancelled context must abort the transaction")
	}
	if err := s.View(ctx, func(ctx context.Context, r distledger.Reader) error { return nil }); err == nil {
		t.Fatal("cancelled context must abort the read")
	}
}

// ── 分页 ────────────────────────────────────────────────────────────────

// TestKeysetPaginationIsStableUnderAppends 说明为什么分页用键集而不是偏移。
//
// 键集分页保证的是「已存在的记录不会被跳过、也不会被读两次」。
// 用 OFFSET 分页时，翻页途中追加的新记录会把后续窗口整体后移，
// 于是有记录从未被读到——在对账场景里，漏读直接等于「账是平的」
// 这个结论不成立。
//
// 注意：键集分页**不**隐藏新追加的记录（它们的 ID 大于游标，理应被读到）。
// 本测试断言的正是「原有的 5 条一条不漏、一条不重」。
func TestKeysetPaginationIsStableUnderAppends(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	key := agentKey(1)

	appendEntry := func(n int) {
		t.Helper()
		if err := s.Update(ctx, func(ctx context.Context, tx distledger.Tx) error {
			for i := 0; i < n; i++ {
				if _, err := tx.AppendLedger(ctx, distledger.LedgerEntry{
					Key: key, BizType: distledger.LedgerAccrue, BizID: "b", DeltaFrozen: 1,
				}); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}

	appendEntry(5)
	original := make(map[int64]bool, 5)
	for id := int64(1); id <= 5; id++ {
		original[id] = true
	}

	var seen []int64
	var afterID int64
	for page := 0; page < 50; page++ {
		var batch []distledger.LedgerEntry
		if err := s.View(ctx, func(ctx context.Context, r distledger.Reader) error {
			var err error
			batch, err = r.LedgerEntries(ctx, key, distledger.LedgerQuery{
				Page: distledger.Page{AfterID: afterID, Limit: 2},
			})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if len(batch) == 0 {
			break
		}
		// 翻页途中持续追加新数据。
		appendEntry(1)
		for _, e := range batch {
			seen = append(seen, e.ID)
			afterID = e.ID
		}
	}

	// 顺序必须严格递增：重复读与乱序都会破坏对账。
	for i := 1; i < len(seen); i++ {
		if seen[i] <= seen[i-1] {
			t.Fatalf("pagination returned entries out of order or duplicated: %v", seen)
		}
	}

	// 原有 5 条必须一条不漏、一条不重。
	counts := make(map[int64]int, len(seen))
	for _, id := range seen {
		counts[id]++
	}
	for id := range original {
		switch counts[id] {
		case 1:
		case 0:
			t.Fatalf("original entry %d was skipped during pagination (seen=%v)", id, seen)
		default:
			t.Fatalf("original entry %d was read %d times", id, counts[id])
		}
	}
}

func TestPageLimitIsBounded(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	for i := 0; i < 50; i++ {
		seedCommission(t, s, "ORD-"+string(rune('A'+i%26))+string(rune('0'+i/26)), 1)
	}
	var got int
	if err := s.View(ctx, func(ctx context.Context, r distledger.Reader) error {
		list, err := r.CommissionsByTenant(ctx, 0, distledger.Page{Limit: 1 << 30})
		got = len(list)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if got > distledger.MaxPageLimit {
		t.Fatalf("query returned %d rows, want at most %d", got, distledger.MaxPageLimit)
	}
}

func TestAccountOfUnknownUserIsZeroValue(t *testing.T) {
	s := newStore(t)
	if err := s.View(context.Background(), func(ctx context.Context, r distledger.Reader) error {
		acct, err := r.Account(ctx, agentKey(999))
		if err != nil {
			return err
		}
		if !acct.Frozen.IsZero() || !acct.Available.IsZero() {
			t.Errorf("unknown user returned a non-zero account: %+v", acct)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
