package distledger_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/darkinno-tech/distledger"
	"github.com/darkinno-tech/distledger/store/memory"
)

func TestNewRejectsInvalidConfig(t *testing.T) {
	t.Run("nil store", func(t *testing.T) {
		if _, err := distledger.New(distledger.Config{}); err == nil {
			t.Fatal("a missing store must be rejected instead of panicking later")
		}
	})

	t.Run("invalid rules", func(t *testing.T) {
		_, err := distledger.New(distledger.Config{
			Store: memory.New(),
			Rules: distledger.Rules{Levels: 2, RateBP: []distledger.Rate{1}}, // count != Levels
		})
		if !errors.Is(err, distledger.ErrInvalidConfig) {
			t.Fatalf("expected ErrInvalidConfig, got %v", err)
		}
	})

	t.Run("rates exceeding the cap", func(t *testing.T) {
		_, err := distledger.New(distledger.Config{
			Store: memory.New(),
			Rules: distledger.Rules{
				Levels: 2, RateBP: []distledger.Rate{6000, 6000},
				MaxAllocatableBP: 10000,
			},
		})
		if err == nil {
			t.Fatal("rate sum above the allocatable cap must be rejected at construction")
		}
	})

	t.Run("levels above the legal ceiling", func(t *testing.T) {
		_, err := distledger.New(distledger.Config{
			Store: memory.New(),
			Rules: distledger.Rules{
				Levels: distledger.MaxLevels + 1,
				RateBP: []distledger.Rate{100, 100, 100, 100},
			},
		})
		if err == nil {
			t.Fatal("four levels must be rejected — the ceiling is a compliance guard")
		}
	})
}

func TestNewAppliesSafeDefaults(t *testing.T) {
	led, err := distledger.New(distledger.Config{Store: memory.New()})
	if err != nil {
		t.Fatalf("zero-value rules must be usable: %v", err)
	}
	defer led.Close()

	rules := led.Rules()
	if rules.Levels != 2 {
		t.Errorf("Levels = %d, want the conservative default 2", rules.Levels)
	}
	for i, r := range rules.RateBP {
		if r != 0 {
			t.Errorf("level %d rate = %d, want 0 — default must not pay out", i+1, r)
		}
	}
	if rules.MaxAllocatableBP != distledger.MaxRateBP {
		t.Errorf("MaxAllocatableBP = %d, want %d", rules.MaxAllocatableBP, distledger.MaxRateBP)
	}
}

// TestRulesCopyIsIsolated keeps the rules copy handed to a caller from
// leaking back into the kernel.
func TestRulesCopyIsIsolated(t *testing.T) {
	led, err := distledger.New(distledger.Config{
		Store: memory.New(),
		Rules: distledger.Rules{Levels: 2, RateBP: []distledger.Rate{500, 200}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer led.Close()

	got := led.Rules()
	got.RateBP[0] = 9999
	got.Levels = 1

	again := led.Rules()
	if again.RateBP[0] != 500 || again.Levels != 2 {
		t.Fatalf("the ledger's rules were mutated through the returned copy: %+v", again)
	}
}

func TestClockAccessor(t *testing.T) {
	clock := distledger.NewManualClock(time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC))
	led, err := distledger.New(distledger.Config{Store: memory.New(), Clock: clock})
	if err != nil {
		t.Fatal(err)
	}
	defer led.Close()

	if !led.Now().Equal(clock.Now()) {
		t.Fatalf("Now() = %s, want %s", led.Now(), clock.Now())
	}
	clock.Advance(time.Hour)
	if !led.Now().Equal(clock.Now()) {
		t.Fatalf("Now() did not follow the injected clock: %s", led.Now())
	}
}

func TestAgentAccessor(t *testing.T) {
	f := newFixture(t, twoLevelRules())
	created := f.mustAgent(3001, 0, distledger.AgentActive)

	got, err := f.led.Agent(context.Background(), tenant, 3001)
	if err != nil {
		t.Fatalf("Agent: %v", err)
	}
	if got.Key != created.Key || got.Status != distledger.AgentActive {
		t.Fatalf("Agent returned %+v, want %+v", got, created)
	}
	if got.Depth != 1 {
		t.Fatalf("Depth = %d, want 1 for a root agent", got.Depth)
	}

	if _, err := f.led.Agent(context.Background(), tenant, 9999); !errors.Is(err, distledger.ErrNotFound) {
		t.Fatalf("missing agent = %v, want ErrNotFound", err)
	}
	if _, err := f.led.Agent(context.Background(), tenant, 0); !errors.Is(err, distledger.ErrInvalidArgument) {
		t.Fatalf("user id 0 = %v, want ErrInvalidArgument", err)
	}
}

func TestQueryAccessorsValidateInput(t *testing.T) {
	f := newFixture(t, twoLevelRules())
	ctx := context.Background()

	if _, err := f.led.Balance(ctx, tenant, 0); !errors.Is(err, distledger.ErrInvalidArgument) {
		t.Errorf("Balance with user 0 = %v, want ErrInvalidArgument", err)
	}
	if _, err := f.led.CommissionsByOrder(ctx, tenant, ""); !errors.Is(err, distledger.ErrInvalidArgument) {
		t.Errorf("empty order id = %v, want ErrInvalidArgument", err)
	}
	if _, err := f.led.CommissionsByAgent(ctx, tenant, 0, distledger.CommissionQuery{}); !errors.Is(err, distledger.ErrInvalidArgument) {
		t.Errorf("CommissionsByAgent with user 0 = %v, want ErrInvalidArgument", err)
	}
	if _, err := f.led.LedgerEntries(ctx, tenant, 0, distledger.LedgerQuery{}); !errors.Is(err, distledger.ErrInvalidArgument) {
		t.Errorf("LedgerEntries with user 0 = %v, want ErrInvalidArgument", err)
	}
	if _, err := f.led.BindAgent(ctx, distledger.BindAgentRequest{TenantID: -1, UserID: 1}); !errors.Is(err, distledger.ErrInvalidArgument) {
		t.Errorf("negative tenant = %v, want ErrInvalidArgument", err)
	}
	if _, err := f.led.BindBuyer(ctx, distledger.BindBuyerRequest{
		TenantID: tenant, BuyerUserID: 0, AgentUserID: 1, Source: distledger.SourceLink,
	}); !errors.Is(err, distledger.ErrInvalidArgument) {
		t.Errorf("bad buyer = %v, want ErrInvalidArgument", err)
	}
}

// TestRulesAreFrozenPerLedger verifies that rules are frozen at construction.
//
// This avoids a subtle trap: if rules could be changed while the ledger runs,
// "orders from the same day were charged different rates" would be neither
// explainable nor reproducible.
func TestRulesAreFrozenPerLedger(t *testing.T) {
	f := newFixture(t, distledger.Rules{
		Levels: 1, RateBP: []distledger.Rate{1000}, FreezeDays: 7,
	})
	f.mustAgent(3001, 0, distledger.AgentActive)
	f.mustBuyer(4001, 3001)
	f.mustPay("ORD-1", 4001, 10000)

	f.mustPay("ORD-2", 4001, 10000)

	commissions, err := f.led.CommissionsByOrder(context.Background(), tenant, "ORD-2")
	if err != nil {
		t.Fatal(err)
	}
	// Both orders must use the same rule version, so RuleVersion matches.
	first, err := f.led.CommissionsByOrder(context.Background(), tenant, "ORD-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(commissions) != 1 || len(first) != 1 {
		t.Fatalf("unexpected commission counts: %d, %d", len(first), len(commissions))
	}
	if first[0].RuleVersion != commissions[0].RuleVersion {
		t.Fatal("the same ledger must apply one frozen rule version")
	}
	if commissions[0].Amount != 1000 {
		t.Fatalf("commission = %s, want 10.00", commissions[0].Amount)
	}
}
