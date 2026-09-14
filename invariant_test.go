package distledger_test

import (
	"context"
	"fmt"
	"math/rand"
	"sync"
	"testing"
	"time"

	"github.com/im10furry/distledger"
)

// TestRandomEventSequencePreservesInvariants 是最重要的属性测试。
//
// 它用随机的事件序列（含重复投递、乱序收货、时间跳跃）去撞不变量，
// 每轮结束后要求：
//
//  1. SelfCheck 全部通过（余额 == 流水和、无负余额、无悬空引用、不超配额）。
//  2. 每条待结算佣金的账户余额与佣金之和一致。
//
// 失败信息里会打印随机种子，便于精确复现。
func TestRandomEventSequencePreservesInvariants(t *testing.T) {
	const iterations = 40
	for seed := int64(1); seed <= iterations; seed++ {
		seed := seed
		t.Run(fmt.Sprintf("seed-%d", seed), func(t *testing.T) {
			runRandomSequence(t, seed)
		})
	}
}

func runRandomSequence(t *testing.T, seed int64) {
	t.Helper()
	rng := rand.New(rand.NewSource(seed))

	f := newFixture(t, distledger.Rules{
		Levels: 3, RateBP: []distledger.Rate{500, 200, 100},
		FreezeDays: 7, Rounding: distledger.RoundHalfUp,
	})

	agents := []int64{3001, 3002, 3003}
	f.mustAgent(agents[0], 0, distledger.AgentActive)
	f.mustAgent(agents[1], agents[0], distledger.AgentActive)
	f.mustAgent(agents[2], agents[1], distledger.AgentActive)

	// 8 个买家，随机归属到三个分销员之一。
	const buyers = 8
	for i := 0; i < buyers; i++ {
		buyerID := int64(4000 + i)
		f.mustBuyer(buyerID, agents[rng.Intn(len(agents))])
	}

	type orderState struct {
		id     string
		paid   bool
		amount distledger.Money
	}
	orders := make([]*orderState, 0, 16)
	nextOrder := 0

	const ops = 220
	for op := 0; op < ops; op++ {
		switch rng.Intn(10) {
		case 0, 1, 2, 3, 4: // 下单支付
			if len(orders) >= 16 {
				continue
			}
			nextOrder++
			o := &orderState{
				id:     fmt.Sprintf("ORD-%d", nextOrder),
				amount: distledger.Money(100 + rng.Intn(500000)),
			}
			buyerID := int64(4000 + rng.Intn(buyers))
			_, err := f.led.OnOrderPaid(context.Background(), distledger.OrderPaidEvent{
				TenantID: tenant, OrderID: o.id, BuyerUserID: buyerID,
				PaidAmount: o.amount, PaidAt: f.clock.Now(),
			})
			if err != nil {
				t.Fatalf("seed=%d op=%d OnOrderPaid: %v", seed, op, err)
			}
			o.paid = true
			orders = append(orders, o)

		case 5: // 重复投递（应当完全无副作用）
			if len(orders) == 0 {
				continue
			}
			o := orders[rng.Intn(len(orders))]
			if _, err := f.led.OnOrderPaid(context.Background(), distledger.OrderPaidEvent{
				TenantID: tenant, OrderID: o.id, BuyerUserID: 4001,
				PaidAmount: o.amount, PaidAt: f.clock.Now(),
			}); err != nil {
				t.Fatalf("seed=%d op=%d replay: %v", seed, op, err)
			}

		case 6, 7: // 收货（时间可以乱序）
			if len(orders) == 0 {
				continue
			}
			o := orders[rng.Intn(len(orders))]
			at := f.clock.Now().Add(-time.Duration(rng.Intn(72)) * time.Hour)
			if _, err := f.led.OnOrderReceived(context.Background(), distledger.OrderReceivedEvent{
				TenantID: tenant, OrderID: o.id, ReceivedAt: at,
			}); err != nil {
				t.Fatalf("seed=%d op=%d OnOrderReceived: %v", seed, op, err)
			}

		case 8: // 推进时间
			f.clock.Advance(time.Duration(rng.Intn(96)) * time.Hour)

		case 9: // 心跳
			if _, err := f.led.Maintain(context.Background(), f.clock.Now()); err != nil {
				t.Fatalf("seed=%d op=%d Maintain: %v", seed, op, err)
			}
		}
	}

	// 收尾：推进足够久并把所有到期佣金结清。
	f.clock.Advance(400 * 24 * time.Hour)
	for i := 0; i < 10; i++ {
		res, err := f.led.Maintain(context.Background(), f.clock.Now())
		if err != nil {
			t.Fatalf("seed=%d final Maintain: %v", seed, err)
		}
		if res.SettledCount == 0 {
			break
		}
	}

	rep, err := f.led.SelfCheck(context.Background(), tenant)
	if err != nil {
		t.Fatalf("seed=%d SelfCheck: %v", seed, err)
	}
	if !rep.OK {
		t.Fatalf("seed=%d invariants violated: %+v", seed, rep.Invariants)
	}

	// 兜底断言：每个分销员的「待结算 + 可提现」必须等于其全部佣金之和中
	// 尚未被冲正的部分。v0.1 没有冲正，因此应当完全相等。
	for _, agentID := range agents {
		commissions, err := f.led.CommissionsByAgent(context.Background(), tenant, agentID,
			distledger.CommissionQuery{})
		if err != nil {
			t.Fatalf("seed=%d CommissionsByAgent: %v", seed, err)
		}
		var expected distledger.Money
		for _, c := range commissions {
			expected += c.Amount
		}
		bal := f.balance(agentID)
		got := bal.Frozen + bal.Available + bal.Withdrawing + bal.Withdrawn
		if got != expected {
			t.Fatalf("seed=%d agent %d: balance buckets sum to %s but commissions sum to %s",
				seed, agentID, got, expected)
		}
	}
}

// TestConcurrentAccrualProducesExactlyOneSet 验证并发下的幂等性。
//
// 32 个 goroutine 同时投递同一笔订单：结果必须与投递一次完全相同。
// 这是真实场景——支付回调重试与 MQ 重投常常是并发的，而不是顺序的。
func TestConcurrentAccrualProducesExactlyOneSet(t *testing.T) {
	f := newFixture(t, twoLevelRules())
	f.mustAgent(3001, 0, distledger.AgentActive)
	f.mustAgent(3002, 3001, distledger.AgentActive)
	f.mustBuyer(4001, 3002)

	const workers = 32
	paidAt := f.clock.Now()

	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		created  int
		failures []error
	)
	start := make(chan struct{})
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			res, err := f.led.OnOrderPaid(context.Background(), distledger.OrderPaidEvent{
				TenantID: tenant, OrderID: "ORD-1", BuyerUserID: 4001,
				PaidAmount: 100000, PaidAt: paidAt,
			})
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				failures = append(failures, err)
				return
			}
			created += len(res.Commissions)
		}()
	}
	close(start)
	wg.Wait()

	for _, err := range failures {
		t.Fatalf("concurrent accrual failed: %v", err)
	}
	if created != 2 {
		t.Fatalf("%d commissions were created across %d concurrent calls, want exactly 2",
			created, workers)
	}
	if bal := f.balance(3002); bal.Frozen != 5000 {
		t.Fatalf("agent 3002 frozen = %s, want 50.00", bal.Frozen)
	}
	if bal := f.balance(3001); bal.Frozen != 2000 {
		t.Fatalf("agent 3001 frozen = %s, want 20.00", bal.Frozen)
	}

	rep := f.selfCheck()
	if !rep.OK {
		t.Fatalf("invariants violated after concurrent accrual: %+v", rep.Invariants)
	}
}

// TestConcurrentMaintainSettlesExactlyOnce 验证并发心跳不会重复结算。
func TestConcurrentMaintainSettlesExactlyOnce(t *testing.T) {
	f := newFixture(t, distledger.Rules{
		Levels: 1, RateBP: []distledger.Rate{1000}, FreezeDays: 0,
	})
	f.mustAgent(3001, 0, distledger.AgentActive)
	f.mustBuyer(4001, 3001)

	const orderCount = 20
	for i := 0; i < orderCount; i++ {
		orderID := fmt.Sprintf("ORD-%d", i)
		f.mustPay(orderID, 4001, 10000)
		f.receive(orderID, f.clock.Now())
	}

	const workers = 16
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		settled  int
		failures []error
	)
	start := make(chan struct{})
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			res, err := f.led.Maintain(context.Background(), f.clock.Now())
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				failures = append(failures, err)
				return
			}
			settled += res.SettledCount
		}()
	}
	close(start)
	wg.Wait()

	for _, err := range failures {
		t.Fatalf("concurrent Maintain failed: %v", err)
	}
	if settled != orderCount {
		t.Fatalf("%d commissions were settled across %d concurrent Maintain calls, want %d",
			settled, workers, orderCount)
	}
	if bal := f.balance(3001); bal.Available != 1000*distledger.Money(orderCount) {
		t.Fatalf("available = %s, want %s", bal.Available, 1000*distledger.Money(orderCount))
	}

	rep := f.selfCheck()
	if !rep.OK {
		t.Fatalf("invariants violated after concurrent settle: %+v", rep.Invariants)
	}
}

// TestConcurrentMixedTraffic 让支付、收货、心跳同时发生。
func TestConcurrentMixedTraffic(t *testing.T) {
	f := newFixture(t, distledger.Rules{
		Levels: 2, RateBP: []distledger.Rate{500, 300}, FreezeDays: 1,
	})
	f.mustAgent(3001, 0, distledger.AgentActive)
	f.mustAgent(3002, 3001, distledger.AgentActive)
	f.mustBuyer(4001, 3002)

	const orders = 30
	paidAt := f.clock.Now()

	var wg sync.WaitGroup
	errCh := make(chan error, orders*3)

	for i := 0; i < orders; i++ {
		orderID := fmt.Sprintf("ORD-%d", i)
		wg.Add(3)
		go func() {
			defer wg.Done()
			_, err := f.led.OnOrderPaid(context.Background(), distledger.OrderPaidEvent{
				TenantID: tenant, OrderID: orderID, BuyerUserID: 4001,
				PaidAmount: 20000, PaidAt: paidAt,
			})
			errCh <- err
		}()
		go func() {
			defer wg.Done()
			_, err := f.led.OnOrderReceived(context.Background(), distledger.OrderReceivedEvent{
				TenantID: tenant, OrderID: orderID, ReceivedAt: paidAt,
			})
			errCh <- err
		}()
		go func() {
			defer wg.Done()
			_, err := f.led.Maintain(context.Background(), paidAt)
			errCh <- err
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatalf("mixed traffic failed: %v", err)
		}
	}

	// 再次心跳把剩余到期的结清。
	f.clock.Advance(48 * time.Hour)
	for i := 0; i < 5; i++ {
		res, err := f.led.Maintain(context.Background(), f.clock.Now())
		if err != nil {
			t.Fatal(err)
		}
		if res.SettledCount == 0 {
			break
		}
	}

	rep := f.selfCheck()
	if !rep.OK {
		t.Fatalf("invariants violated under mixed traffic: %+v", rep.Invariants)
	}
}
