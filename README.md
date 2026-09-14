# distledger

> **A distribution ledger kernel for Go** — the smallest reusable core that gets distribution money *right*: computed correctly, recorded clearly, traceable afterwards.

[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)
[![Go Version](https://img.shields.io/badge/go-1.22%2B-blue.svg)](https://go.dev/)
[![Dependencies](https://img.shields.io/badge/dependencies-zero-brightgreen.svg)](#design-invariants)

`distledger` solves exactly three things: the **relation chain**, the **commission ledger**, and the **money trail**.
Every rule — how many levels, what rates, whether there is an entry bar — is an injectable strategy.

**It is not a distribution system.** It is the part of one that is easiest to get wrong and should never be rewritten.

> 📄 Full requirements and design: **[PRD.md](PRD.md)** ｜ Decisions and their costs: **[docs/design-decisions.md](docs/design-decisions.md)** ｜
> Behavior at billion-row scale: **[docs/scale.md](docs/scale.md)** (all three are Chinese; they record design intent for the maintainer)
> 中文说明见 **[README.zh-CN.md](README.zh-CN.md)**。

---

## Implementation status

| Capability | Status |
|---|---|
| Money and rate arithmetic (integer, overflow-checked, explicit rounding, allocation cap) | ✅ v0.1 |
| Relation chain (parent links, depth ceiling, cycle detection, parent cannot change quietly) | ✅ v0.1 |
| Attribution binding (first-wins, expiry, poaching protection, no retroactive capture) | ✅ v0.1 |
| Multi-level accrual (per SKU, idempotency keys, typed reasons for skipped levels) | ✅ v0.1 |
| Freeze snapshot + `Maintain` settlement (state machine + optimistic locking) | ✅ v0.1 |
| `SelfCheck` invariants I1 / I2 | ✅ v0.1 |
| In-memory store (transactional rollback, keyset pagination, due index) | ✅ v0.1 |
| Refund reversal (full order / per item / partial, accumulated in instalments) | ✅ v0.3 |
| Risk control: freeze, unfreeze, void | ✅ v0.3 |
| `SelfCheck` invariant I3 (reversal accumulator ↔ detail ↔ account reconciliation) | ✅ v0.3 |
| **Pluggable SQL store: one core, MySQL / PostgreSQL / SQLite dialects** | ⏳ v0.4 |
| Withdrawals and payout channels (including the debt policy for clawing back money already paid out) | ⏳ v0.5 |

**v0.1–v0.3 are usable today** for single-process deployments and test environments.
For a multi-instance production deployment, wait for the SQL store in v0.4.

---

## Why this exists

The *business* rules of distribution are an endless long tail — every company differs. The *money movement* is identical for everyone:

```
order attributed to an agent → compute commissions for N levels → hold them frozen
  → releasable once the freeze window elapses → clawed back if the order is refunded → paid out
```

Build the long tail into the library and it rots. Build the invariants into it and it is reusable.

**Go has no ready-to-use distribution library.** The open-source options elsewhere are modules inside PHP or Java commerce platforms, and this is code where a mistake means money disappears.

---

## Storage is pluggable

The persistence port lives in the root package and speaks only domain types — no SQL, no driver, no backend assumption leaks through it.
Pick a backend by importing one package:

```go
import (
	"github.com/im10furry/distledger"
	memstore "github.com/im10furry/distledger/store/memory"
)

led, _ := distledger.New(distledger.Config{Store: memstore.New(), Rules: rules})
```

| Backend | Import | Use it for |
|---|---|---|
| In-memory | `store/memory` | Tests, demos, single-process deployments. Zero external dependencies. |
| MySQL | `store/mysql` | ⏳ v0.4 |
| PostgreSQL | `store/postgres` | ⏳ v0.4 |
| SQLite | `store/sqlite` | ⏳ v0.4, embedded deployments |
| Portable core | `store/sql` (package `sqlstore`) | Shared by the SQL backends; not used directly |

The SQL backends share **one** portable core that builds statements through a `Dialect`. Each dialect package only supplies what genuinely differs between databases: placeholders (`?` versus `$1`), identifier quoting, type names, the generated-key form, duplicate-key and retryable-error classification, and index DDL. **Adding a backend means writing a dialect of about 100 lines, not another store** — because several copies of the accounting rules would drift apart, and a fix applied to only one of them raises no error.

**The library ships no database driver and depends on nothing.** You open your own `*sql.DB` with whichever driver you already use and hand it over:

```go
import (
	"database/sql"

	_ "github.com/go-sql-driver/mysql" // your import, not ours
	sqlstore "github.com/im10furry/distledger/store/sql"
	"github.com/im10furry/distledger/store/mysql"
)

db, _ := sql.Open("mysql", dsn)
store, _ := sqlstore.Open(db, mysql.Dialect{})
```

The driver import stays in your module. This is why the library's `go.mod` has an empty `require` block — a hard rule, not a preference. It also shapes the internals: even duplicate-key detection reads a driver error **structurally** rather than importing the driver's error type.

---

## Design invariants

| Rule | Meaning |
|---|---|
| **The dividing line** | Decisions that change *where* money flows belong in the kernel; decisions that only change *how much* are pluggable. The rule layer may never change state. |
| **Pay nothing by default** | Zero rates, two levels, caller-driven settlement. Verbose configuration is survivable; paying out by accident is not. |
| **Integer money** | Always `int64` minor units plus basis-point rates. Floating point never participates. |
| **Append-only ledger** | Financial fields and ledger entries are only ever appended; a reversal adds a negative record. This is enforced by the shape of the store port, not by code review. |
| **Fail safe** | An illegal state transition is an error, never a guess. When an invariant does not hold, the heartbeat fails loudly rather than proceeding. |
| **No magic** | No code generation, no reflection-based registration, no package-level singletons. |
| **Zero dependencies** | `go.mod` has an empty `require` block. |

---

## Getting started (30 seconds, no database required)

```bash
git clone https://github.com/im10furry/distledger.git
cd distledger/examples/01-quickstart
go run .
```

Output (abridged):

```
3) attributed=true, 2 commission(s) accrued
   level 1  agent=1002  base=199.00  rate=5%  commission=9.95
   level 2  agent=1001  base=199.00  rate=2%  commission=3.98
   redelivered: replayed=true, new commissions=0 (idempotency holds)
4) after receipt user 1001: frozen=3.98 available=0.00 total=3.98
5) after 8 days: 2 settled, total 13.93
7) self-check: store=memory overall=true
   [PASS] I1: account balance equals sum of ledger deltas (2 checked)
   [PASS] I2: commission ledger linkage and allocation cap (2 checked)
   [PASS] I3: reversal accumulator, state and account totals reconcile (2 checked)
```

The smallest useful program:

```go
package main

import (
	"context"
	"fmt"
	"time"

	"github.com/im10furry/distledger"
	"github.com/im10furry/distledger/store/memory"
)

func main() {
	ctx := context.Background()
	clock := distledger.NewManualClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))

	led, err := distledger.New(distledger.Config{
		Store: memory.New(), // in-memory store, no external dependencies
		Clock: clock,
		Rules: distledger.Rules{
			Levels:     2,
			RateBP:     []distledger.Rate{500, 200}, // 5% on level 1, 2% on level 2
			FreezeDays: 7,
		},
	})
	if err != nil {
		panic(err)
	}
	defer led.Close()

	const tenant = int64(0)

	// Relation chain: A(1001) <- B(1002)
	_, _ = led.BindAgent(ctx, distledger.BindAgentRequest{
		TenantID: tenant, UserID: 1001, Status: distledger.AgentActive,
	})
	_, _ = led.BindAgent(ctx, distledger.BindAgentRequest{
		TenantID: tenant, UserID: 1002, ParentID: 1001, Status: distledger.AgentActive,
	})

	// B refers C(1003)
	_, _ = led.BindBuyer(ctx, distledger.BindBuyerRequest{
		TenantID: tenant, BuyerUserID: 1003, AgentUserID: 1002, Source: distledger.SourceLink,
	})

	// C pays 199.00
	res, _ := led.OnOrderPaid(ctx, distledger.OrderPaidEvent{
		TenantID: tenant, OrderID: "ORD-1", BuyerUserID: 1003,
		PaidAmount: distledger.Money(19900), PaidAt: clock.Now(),
	})
	fmt.Println("commissions:", len(res.Commissions)) // 2

	// Receipt starts the 7-day freeze; advance 8 days; settle.
	_, _ = led.OnOrderReceived(ctx, distledger.OrderReceivedEvent{
		TenantID: tenant, OrderID: "ORD-1", ReceivedAt: clock.Now(),
	})
	clock.Advance(8 * 24 * time.Hour)
	_, _ = led.Maintain(ctx)

	bal, _ := led.Balance(ctx, tenant, 1001)
	fmt.Println("A can withdraw:", bal.Available)
}
```

**The core API is three event entry points plus one heartbeat:**

| API | Purpose |
|---|---|
| `OnOrderPaid` | Order paid → attribution → accrual → credit. **Idempotent: retry it freely.** |
| `OnOrderReceived` | Receipt confirmed → fixes the settlement date using the **snapshotted** freeze window. First writer wins. |
| `OnOrderRefunded` | Order refunded → claws back each level according to the **cumulative** refunded amount. **Idempotent; requires your refund identifier.** |
| `Maintain(ctx)` | Advances everything driven by time. Put it on your scheduler. |

Risk control adds `FreezeCommission` / `UnfreezeCommission` / `VoidCommission`, and `SelfCheck` reports the health of the books.

---

## Integration checklist

```
□ go get github.com/im10furry/distledger
□ go run ./examples/01-quickstart          → see commission numbers in 30 seconds
□ set RateBP explicitly in Rules           → the default rate is zero, deliberately (ADR-010)
□ call OnOrderPaid() after a successful payment   → commission appears as "frozen"
□ call OnOrderReceived() when the buyer confirms  → available_at is written
□ call Maintain() from cron                       → expired freezes settle automatically
□ call OnOrderRefunded() from your refund flow    → pass your refund id as IdemKey
□ led.SelfCheck(ctx, tenant) reports OK           → you are done
```

**Target integration cost: under 50 lines, no changes to your order tables, no takeover of your application lifecycle.**

---

## Seven behavior contracts you must know

All of them are deliberate, and each one closes a real loss or dispute scenario:

1. **`PaidAt` is required.** The library will not substitute the current time. Doing so would let a replay that happened *before* a binding existed be attributed retroactively — a door for after-the-fact poaching (ADR-018).
2. **Attribution is evaluated at the moment of payment.** A binding must already exist and be effective then. A binding created afterwards does not capture the order.
3. **The freeze window is snapshotted at accrual time.** Changing the global `FreezeDays` later does not move settlement dates that have already been promised (ADR-005).
4. **`OrderRefundedEvent.IdemKey` is required.** It is your identifier for that refund, usually the refund order id from your payment provider. Without it, a retry and a second refund of the same amount are indistinguishable in the data, and every possible implementation gets one of them wrong (ADR-024).
5. **`Maintain` takes no time argument.** Processing time comes only from the injected `Clock`; letting a caller advance it is the same as letting them decide when to pay out (ADR-023).
6. **`OrderID` is unique per tenant and never reused.** Order-level idempotency keys on it, so reuse makes a genuine new order look like a replay and it is dropped.
7. **A custom `Store` must roll back completely before retrying.** Weakening this lets the engine's second attempt see its own half-applied work (ADR-030).

---

## Data model

Seven logical tables (the in-memory store implements them as maps and indexes):

| Table | Purpose |
|---|---|
| `dist_agent` | Agents and the relation chain (parent pointer plus depth) |
| `dist_binding` | Buyer-to-agent attribution (first-wins, expiry, auditable rebinding) |
| `dist_commission` | Commission records. **Financial fields are immutable**; carries rate, base and freeze-window **snapshots** |
| `dist_account` | Balances: frozen / available / withdrawing / withdrawn, plus gross and reversed totals |
| `dist_ledger` | Double-entry money trail. **Append-only.** |
| `dist_refund` | Refund vouchers: one row per instalment per item, carrying the **cumulative refunded amount** |
| `dist_withdraw` | ⏳ v0.5 |

See [PRD.md §6](PRD.md) for the field-level design.

---

## Three invariants

These are the entire reason the library exists, and the things `SelfCheck` should be run against daily:

| ID | Invariant | How it is checked |
|---|---|---|
| **I1** | An account's balance equals the sum of the ledger deltas for that account; the last entry's post-balance agrees with the account; no bucket is negative | `SelfCheck` re-sums the ledger and compares, bucket by bucket |
| **I2** | Every commission has a matching ledger entry (referential integrity in both directions), and each accrual item allocates no more than its cap | `SelfCheck` enumerates both ways and verifies the cap |
| **I3** | The reversal accumulator equals the sum of its detail records, and the account's gross and reversed totals equal the agent's commission roll-up | `SelfCheck` reconciles three directions — this is where refund bugs hide |

> The bucket set is defined in exactly one place (`Bucket`), and the negative-balance guard, I1 and ledger construction all walk it.
> A reflection test forces every new `Money` field on `Account` to be classified as either a bucket or an explicitly exempt total with a named covering invariant,
> so an invariant cannot quietly stop applying to a new field (ADR-029).

**Cost is tested, not just correctness.** The heartbeat's operation count is asserted directly, because a
per-tenant loop and a per-work loop settle exactly the same commissions — the difference only appears at scale,
which is the worst place to discover it. With 500 idle tenants and nothing due, `Maintain` must perform
exactly one read and zero transactions. See [docs/scale.md](docs/scale.md).

**Testing strategy**: invariants first. Two randomized property tests — one over payment/receipt/settlement (40 seeds × 220 operations), one over refunds
(25 seeds × 200 operations mixing instalment refunds, deliberate replays of the *same* refund id, and genuine repeat refunds with fresh ids).
Plus concurrent accrual, concurrent refund, transaction-retry contract and cross-tenant authorization tests, all under `-race`, each round ending with a full self check.

---

## What it deliberately does not do

Promo links, posters, leaderboards, earnings dashboards, agent onboarding and approval flows, payments and split settlement,
team commissions / regional dividends / chain-driven payouts, **per-head or per-signup payouts**, HTTP routing, admin back office,
and any binding to an ORM, message queue or web framework.

The full list is in PRD §3.2 — **that list is binding: it is what keeps this package from becoming a distribution system.**

---

## ⚠️ Compliance notice

Distribution is regulated in China by 《禁止传销条例》 (the Regulations on Prohibition of Pyramid Selling) and
《刑法》第二百二十四条之一 (Article 224-1 of the Criminal Law).
**This library provides accounting capabilities only. It does not provide compliance judgement.**

You must ensure on your own that: depth stays at or below three levels · commission is based on real sales · joining never requires a payment ·
funds never pass through an unlicensed "second clearing" arrangement.

The library already enforces what it can: `Levels` is **hard-capped at 3** (not configurable), and there is **no ability to pay per head or per signup**.

> Selling goods and sharing commission is legal. Paying people to recruit people is not. This library only helps with the former.

---

## License

[MIT](LICENSE) — free to use in commercial products.

"Non-commercial" here describes **the maintainers' intent** (no commercial edition, no license sales, no open-core trimming). **It is not a restriction on users.**
If you want to forbid commercial use, you need a non-OSI license such as PolyForm Noncommercial — at which point it is no longer open source.

---

## Contributing

A non-commercial open-source project with no response-time guarantee. Welcome:

- ✅ Invariant tests, boundary cases, documentation fixes, new examples
- ✅ New `Store` implementations — MySQL, PostgreSQL, SQLite, or anything else
- ✅ New payout channel implementations (WeChat, Alipay, bank transfer)
- ❌ Adding "game mechanics" to the core (team commissions, regional dividends, chain payouts) — those can only exist as third-party `RateResolver` implementations
