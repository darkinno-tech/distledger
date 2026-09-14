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

// TestRandomEventSequencePreservesInvariants is the most important property test.
//
// It hammers the invariants with random event sequences (duplicate deliveries,
// out-of-order receipts, time jumps) and requires, after every round:
//
//  1. SelfCheck passes completely (balance == ledger sum, no negative
//     balances, no dangling references, no cap violations).
//  2. Each pending commission's account balance agrees with the sum of that
//     agent's commissions.
//
// Failures print the random seed so they can be reproduced exactly.
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

	// 8 buyers, each randomly attributed to one of the three agents.
	const buyers = 8
	for i := 0; i < buyers; i++ {
		buyerID := int64(4000 + i)
		f.mustBuyer(buyerID, agents[rng.Intn(len(agents))])
	}

	type orderState struct {
		id      string
		paid    bool
		amount  distledger.Money
		buyerID int64
	}
	orders := make([]*orderState, 0, 16)
	nextOrder := 0

	const ops = 220
	for op := 0; op < ops; op++ {
		switch rng.Intn(10) {
		case 0, 1, 2, 3, 4: // order paid
			if len(orders) >= 16 {
				continue
			}
			nextOrder++
			buyerID := int64(4000 + rng.Intn(buyers))
			o := &orderState{
				id:      fmt.Sprintf("ORD-%d", nextOrder),
				amount:  distledger.Money(100 + rng.Intn(500000)),
				buyerID: buyerID,
			}
			_, err := f.led.OnOrderPaid(context.Background(), distledger.OrderPaidEvent{
				TenantID: tenant, OrderID: o.id, BuyerUserID: buyerID,
				PaidAmount: o.amount, PaidAt: f.clock.Now(),
			})
			if err != nil {
				t.Fatalf("seed=%d op=%d OnOrderPaid: %v", seed, op, err)
			}
			o.paid = true
			orders = append(orders, o)

		case 5: // duplicate delivery (must have no side effects at all)
			if len(orders) == 0 {
				continue
			}
			// Replay the ORIGINAL buyer: the library rejects a second event
			// for the same order that names a different buyer.
			o := orders[rng.Intn(len(orders))]
			if _, err := f.led.OnOrderPaid(context.Background(), distledger.OrderPaidEvent{
				TenantID: tenant, OrderID: o.id, BuyerUserID: o.buyerID,
				PaidAmount: o.amount, PaidAt: f.clock.Now(),
			}); err != nil {
				t.Fatalf("seed=%d op=%d replay: %v", seed, op, err)
			}

		case 6, 7: // receipt (timestamps may arrive out of order)
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

		case 8: // advance time
			f.clock.Advance(time.Duration(rng.Intn(96)) * time.Hour)

		case 9: // heartbeat
			if _, err := f.led.Maintain(context.Background()); err != nil {
				t.Fatalf("seed=%d op=%d Maintain: %v", seed, op, err)
			}
		}
	}

	// Wrap up: advance far enough and settle every matured commission.
	f.clock.Advance(400 * 24 * time.Hour)
	for i := 0; i < 10; i++ {
		res, err := f.led.Maintain(context.Background())
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

	// Backstop assertion: each agent's frozen + available buckets must
	// equal the portion of their total commissions that has not been reversed.
	// v0.1 has no reversals, so the two must be exactly equal.
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

// TestConcurrentAccrualProducesExactlyOneSet verifies idempotency under concurrency.
//
// 32 goroutines deliver the same order at once: the result must be identical to
// delivering it once. This is the real-world case—payment callback retries and
// MQ redeliveries are usually concurrent, not sequential.
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

// TestConcurrentMaintainSettlesExactlyOnce verifies that concurrent heartbeats
// do not settle the same commission twice.
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
			res, err := f.led.Maintain(context.Background())
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

// TestConcurrentMixedTraffic runs payment, receipt, and heartbeat concurrently.
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
			_, err := f.led.Maintain(context.Background())
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

	// One more heartbeat settles whatever else has matured.
	f.clock.Advance(48 * time.Hour)
	for i := 0; i < 5; i++ {
		res, err := f.led.Maintain(context.Background())
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

// TestRandomRefundSequencePreservesInvariants extends the property test to the
// refund path, which is where the money most easily goes wrong.
//
// Every round mixes partial refunds, whole-order refunds, full refunds,
// duplicate deliveries of the SAME refund identifier, and genuine repeat refunds
// with fresh identifiers. At the end the self check must pass, and each
// agent's balance must reconcile with their commission records.
func TestRandomRefundSequencePreservesInvariants(t *testing.T) {
	const iterations = 25
	for seed := int64(100); seed < 100+iterations; seed++ {
		seed := seed
		t.Run(fmt.Sprintf("seed-%d", seed), func(t *testing.T) {
			runRandomRefundSequence(t, seed)
		})
	}
}

func runRandomRefundSequence(t *testing.T, seed int64) {
	t.Helper()
	rng := rand.New(rand.NewSource(seed))

	f := newFixture(t, distledger.Rules{
		Levels: 3, RateBP: []distledger.Rate{500, 200, 100},
		FreezeDays: 3, Rounding: distledger.RoundHalfUp,
	})

	agents := []int64{3001, 3002, 3003}
	f.mustAgent(agents[0], 0, distledger.AgentActive)
	f.mustAgent(agents[1], agents[0], distledger.AgentActive)
	f.mustAgent(agents[2], agents[1], distledger.AgentActive)

	const buyers = 4
	for i := 0; i < buyers; i++ {
		f.mustBuyer(int64(4000+i), agents[rng.Intn(len(agents))])
	}

	type orderState struct {
		id       string
		amount   distledger.Money
		buyerID  int64
		refunded distledger.Money
		base     distledger.Money // order-level accrual base
	}
	var orders []*orderState
	nextOrder := 0

	// lastRefundID lets the same identifier be replayed deliberately.
	lastRefundID := ""
	refundCounter := 0

	const ops = 200
	for op := 0; op < ops; op++ {
		switch rng.Intn(12) {
		case 0, 1, 2, 3, 4:
			if len(orders) >= 10 {
				continue
			}
			nextOrder++
			buyerID := int64(4000 + rng.Intn(buyers))
			o := &orderState{
				id:      fmt.Sprintf("ORD-%d", nextOrder),
				amount:  distledger.Money(1000 + rng.Intn(200000)),
				buyerID: buyerID,
			}
			if _, err := f.led.OnOrderPaid(context.Background(), distledger.OrderPaidEvent{
				TenantID: tenant, OrderID: o.id, BuyerUserID: buyerID,
				PaidAmount: o.amount, PaidAt: f.clock.Now(),
			}); err != nil {
				t.Fatalf("seed=%d op=%d pay: %v", seed, op, err)
			}
			o.base = o.amount
			orders = append(orders, o)

		case 5, 6:
			if len(orders) == 0 {
				continue
			}
			o := orders[rng.Intn(len(orders))]
			at := f.clock.Now().Add(-time.Duration(rng.Intn(24)) * time.Hour)
			if _, err := f.led.OnOrderReceived(context.Background(), distledger.OrderReceivedEvent{
				TenantID: tenant, OrderID: o.id, ReceivedAt: at,
			}); err != nil {
				t.Fatalf("seed=%d op=%d receive: %v", seed, op, err)
			}

		case 7:
			f.clock.Advance(time.Duration(rng.Intn(72)) * time.Hour)

		case 8:
			if _, err := f.led.Maintain(context.Background()); err != nil {
				t.Fatalf("seed=%d op=%d maintain: %v", seed, op, err)
			}

		case 9, 10, 11:
			if len(orders) == 0 {
				continue
			}
			o := orders[rng.Intn(len(orders))]
			remaining := o.base - o.refunded
			if remaining <= 0 {
				continue
			}

			// One in four refund events replays the previous identifier, which
			// must be a no-op.
			idem := lastRefundID
			replay := idem != "" && rng.Intn(4) == 0
			if !replay {
				refundCounter++
				idem = fmt.Sprintf("refund-%d", refundCounter)
				lastRefundID = idem
			}

			ev := distledger.OrderRefundedEvent{
				TenantID: tenant, OrderID: o.id, IdemKey: idem,
				RefundedAt: f.clock.Now(),
			}
			switch rng.Intn(3) {
			case 0:
				ev.Amount = distledger.Money(1 + rng.Int63n(int64(remaining)))
			case 1:
				ev.Amount = remaining // exhausts the order
			default:
				ev.IsFull = true
			}

			if _, err := f.led.OnOrderRefunded(context.Background(), ev); err != nil {
				t.Fatalf("seed=%d op=%d refund: %v", seed, op, err)
			}
			if !replay {
				if ev.IsFull {
					o.refunded = o.base
				} else if next := o.refunded + ev.Amount; next > o.base {
					o.refunded = o.base
				} else {
					o.refunded = next
				}
			}
		}
	}

	// Drain everything.
	f.clock.Advance(400 * 24 * time.Hour)
	for i := 0; i < 10; i++ {
		res, err := f.led.Maintain(context.Background())
		if err != nil {
			t.Fatalf("seed=%d final maintain: %v", seed, err)
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

	for _, agentID := range agents {
		commissions, err := f.led.CommissionsByAgent(context.Background(), tenant, agentID,
			distledger.CommissionQuery{})
		if err != nil {
			t.Fatalf("seed=%d CommissionsByAgent: %v", seed, err)
		}
		var net, reversed distledger.Money
		for _, c := range commissions {
			net += c.Amount
			if c.Amount < 0 {
				reversed += -c.Amount
			}
		}
		if net < 0 {
			t.Fatalf("seed=%d agent %d has a negative net commission total %s", seed, agentID, net)
		}
		bal := f.balance(agentID)
		buckets := bal.Frozen + bal.Available + bal.Withdrawing + bal.Withdrawn
		if buckets != net {
			t.Fatalf("seed=%d agent %d buckets sum to %s but net commissions are %s",
				seed, agentID, buckets, net)
		}
		if bal.TotalReversed != reversed {
			t.Fatalf("seed=%d agent %d total reversed is %s but records sum to %s",
				seed, agentID, bal.TotalReversed, reversed)
		}
		if bal.TotalEarned-bal.TotalReversed != net {
			t.Fatalf("seed=%d agent %d gross-minus-reversed is %s but net is %s",
				seed, agentID, bal.TotalEarned-bal.TotalReversed, net)
		}
	}
}
