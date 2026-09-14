# Integration tests

Runs the store port's contract against every backend, and runs a real
`distledger.Ledger` on top of each of them.

## Why this is a separate Go module

The library depends on nothing. Its `go.mod` has an empty `require` block, and
that is a rule rather than a preference — the point of the design is that a
caller brings their own database driver and the library never pulls one in.

Exercising the SQL store against real databases needs drivers. So they live here,
in a module the library never imports:

```
distledger/                go.mod           require: (empty)
└── integration/           go.mod           require: mysql, pgx, sqlite, ...
```

The consequence is that `go test ./...` at the repository root needs no database
and downloads no driver, while this module can be as thorough as it likes.

## Running

```bash
cd integration
docker compose up -d --wait

export DISTLEDGER_TEST_MYSQL_DSN='root:distledger@tcp(127.0.0.1:13307)/distledger?charset=utf8mb4&parseTime=false'
export DISTLEDGER_TEST_POSTGRES_DSN='postgres://postgres:distledger@127.0.0.1:15433/distledger?sslmode=disable'

go test ./...
```

SQLite needs no container and always runs, using a temporary file.

MySQL and PostgreSQL are skipped, with a log line, when their DSN variable is
unset. Skipping rather than failing is deliberate: the root module's test suite
must stay runnable on a machine with no database at all.

Ports are non-default (13307, 15433) so that a database already running on the
usual ports is not disturbed.

## What it covers

**`contract_test.go`** runs the same assertions against every backend, one
subtest each:

- agent round trip and version checking, including that a stale version is
  refused rather than silently winning
- absence: `ErrNotFound` for agents, bindings and commissions, and the zero
  account for a user who has never moved money
- idempotency: a duplicate `IdemKey` reads as `ErrDuplicate`, not as a driver
  error
- first-writer-wins on the settlement date, including that a no-op write does not
  bump the version
- transition guards: an illegal move versus a wrong current state, which must stay
  distinguishable because only one of them is retryable
- the reversal accumulator's bounds
- keyset pagination, including that one tenant's rows never appear in another's
  pages
- `DueCommissions` and `TenantsWithDueWork`, including the empty case that the
  heartbeat hits on almost every tick
- refund vouchers and duplicate detection
- counts, ledger append, and ledger query filters
- complete transaction rollback, and that a rolled-back idempotency key is freed
  for reuse

**`engine_test.go`** builds a real `Ledger` over each backend and runs the whole
lifecycle: multi-level accrual, redelivery, receipt, settlement, partial refund,
refund redelivery, full refund, risk-control freeze, self check, and concurrent
accrual.

Two of those are worth calling out:

- **`TestEngineSurvivesTransactionRetry`** exercises the contract in ADR-030
  against a backend that actually retries. The in-memory store serialises
  everything behind one lock and therefore never conflicts, so before this the
  engine's claim to be safe under retry had only ever been argued, never run.
- **`TestEngineSelfCheckCatchesCorruption`** injects an account with a balance and
  no ledger entries, and requires `SelfCheck` to notice on a real database. A
  reconciliation that always passes is worse than none.

## Dialect differences this suite found

Being run against three real databases is what surfaced these. Each is now
recorded in the code where it matters.

1. **PostgreSQL aborts the whole transaction when a statement fails.** The
   "insert and catch the duplicate-key error" pattern works on MySQL and SQLite,
   where the transaction survives, but on PostgreSQL it poisons the transaction
   and the eventual `COMMIT` becomes a rollback. PostgreSQL therefore needs
   `ON CONFLICT DO NOTHING` and reads the outcome from the affected-row count.
   This is `Dialect.InsertConflictClause`.

2. **`NOT NULL` text columns must not receive NULL.** Columns where the empty
   string carries meaning — an empty `order_item_id` means "order-level accrual"
   — are `NOT NULL`. Sending NULL fails on all three backends, and did so on the
   first run of this suite.

3. **A zero affected-row count needs care.** MySQL reports zero affected rows for
   an UPDATE that changes nothing, so "no row matched" and "the row already had
   these values" would be indistinguishable. Every versioned update bumps
   `version` unconditionally, which guarantees the statement always changes the
   row.

## What this suite does not cover

- **Scale.** These run on empty databases. The cost characteristics in
  [`../docs/scale.md`](../docs/scale.md) are estimates from complexity and index
  shape, not measurements, and that document says so.
- **Dialect-specific tuning.** Isolation levels, connection-pool sizing and
  partition layout are deployment decisions and are not asserted here.
