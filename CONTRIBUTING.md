# Contributing to distledger

Thanks for considering it. This is a small library with an unusually strict set of rules,
and the rules exist because the code moves money. This document is the whole of them,
so that a contribution is not rejected for something that was never written down.

**中文版见 [CONTRIBUTING.zh-CN.md](CONTRIBUTING.zh-CN.md)。**

---

## The two hard rules

Every change must satisfy both. CI enforces them, so you will find out immediately rather
than in review.

### 1. `go test ./...` at the repository root must pass without a database

The root module is the library. It must be testable with nothing installed, because that
is the only way its tests stay runnable by everyone. A test that needs a real database
belongs in `integration/`, which is a **separate Go module** with its own `go.mod`
precisely so that drivers cannot reach the library.

### 2. `go.mod` keeps an empty `require` block

No third-party dependencies in the library. Not for logging, not for errors, not for
testing. A distribution ledger whose dependency tree you cannot audit is a ledger you
cannot audit, and this is a hard rule rather than a preference.

The `integration/` module may use drivers and testing helpers freely — it is test code
for a module nobody imports.

If you believe you need a dependency, open an issue first and argue for it. The answer has
been no so far, including for cases where the standard library was genuinely awkward.

---

## What is welcome

- **Invariant tests, boundary cases, and regression tests for real bugs.** The highest-value
  contribution there is. See "Tests that pin cost, not just results" below.
- **New `Store` implementations.** The storage port is deliberately demanding, so a new
  backend is the best pressure test this design gets. Adding one is about 100 lines of
  `Dialect`, not another store — see `store/sql/dialect.go` and the four existing dialects.
- **Documentation corrections**, especially where this project's docs claim something the
  code does not do. That is a bug.
- **New examples** under `examples/`.
- **Fault reviews** for bugs you fixed, in `docs/fault-reviews/`. See the format below.

## What is out of scope

- **"Game mechanics" in the core** — team commissions, regional dividends, chain payouts,
  per-head or per-signup payouts. These belong in a third-party `RateResolver`. The full
  list is PRD §3.2, and **that list is binding**: it is what keeps this package from
  becoming a distribution system.
- **Compliance judgement.** The library does accounting. It does not decide whether a
  business model is lawful.
- **Withdrawals and payout channels** are not built yet. Once they are, channel
  implementations (WeChat, Alipay, bank transfer) will be very welcome.

---

## Language and style

| Artifact | Language |
|---|---|
| Code comments, doc comments | **English**, American spelling, em dashes |
| Commit messages | **English**, Conventional Commits |
| `README.md`, `CONTRIBUTING.md` | English |
| `README.zh-CN.md`, `CONTRIBUTING.zh-CN.md` | Chinese |
| `PRD.md`, `docs/*.md`, `CHANGELOG.md` | Chinese, deliberately |

The docs are Chinese because they record design intent and trade-offs for the maintainer,
and those arguments were made in Chinese. The code is English because it is read by
everyone else. This split is intentional; please do not "fix" it in one direction.

### Comments explain why, not what

The code says what it does. A comment earns its place by recording something the code
cannot: why this approach rather than the obvious one, what it costs, and what was
rejected.

```go
// Bad: restates the code
// Increment version by one.
version++

// Good: records a decision and its cost
// The version is bumped unconditionally, because MySQL reports zero affected
// rows for an UPDATE that changes nothing, so affected rows cannot be used to
// detect a wasted write.
version++
```

A comment that contradicts its code is worse than no comment. Several have been found
and fixed during review passes; if you spot one, that is a real bug report.

### Formatting

`gofmt` and `go vet` must be clean. Both are checked in CI.

---

## Tests that pin cost, not just results

This is the part of the codebase most unlike a typical Go library, so it is worth reading
before writing tests.

Many properties here **cannot be observed from results**. A heartbeat that asks the store
"which tenants have work" and one that asks "what work is due" settle exactly the same
commissions; the difference appears only at scale, which is the worst place to find out.
So those properties are tested by counting store round trips or asserting query shape.

Look at `scale_test.go` and `integration/explain_test.go` for the pattern.

Three more rules that came out of real bugs:

1. **Never discard an error in a test.** A concurrency test here once counted failures
   without recording the first error, which turned a wrong-error-message bug on a money
   path into what looked like a flaky test. If your test can fail for two reasons, make it
   say which.
2. **Assert the bound is reached, not merely respected.** A budget test that only checks
   `<= limit` passes when the limit is accidentally zero.
3. **A test must not be satisfiable by a no-op.** "The healthy case passes" is also passed
   by a query that returns nothing unconditionally. Inject the corruption and assert it is
   found.

---

## Decisions need an ADR

Any change that makes a trade-off — a new port method, a storage representation, a
concurrency strategy — needs an entry in `docs/design-decisions.md`, appended as the next
ADR number. Do not edit an accepted ADR; write a new one that supersedes it.

An ADR here has a consistent shape, and reviewers will ask for the missing parts:

- **Background** — what forced a decision
- **Decision** — what was chosen
- **Cost** — what it makes worse. An ADR with no cost is not finished
- **Rejected alternatives** — and why, usually with the measurement that decided it

Measurements beat reasoning. "The covering index range scan stops at the limit, so it is
O(limit) not O(table)" is a claim; an `EXPLAIN` output is evidence.

---

## Commits

Conventional Commits, English, imperative mood:

```
feat: reconcile accounts in the store instead of streaming the ledger
fix(store): stop reporting insufficient balance for a concurrent increment
perf: locate the heartbeat's work by due time and bound it with one budget
docs: say what each invariant guarantees and what it does not
```

Types in use: `feat`, `fix`, `perf`, `refactor`, `docs`, `test`, `chore`, `style`.
A scope in parentheses when it helps (`store`, `sql`, `memory`, `integration`).

The body is where the value is. Explain **why**, what was measured, and what was rejected.
A commit that only restates the diff is not reviewable six months later:

```
fix(store): stop reporting insufficient balance for a concurrent increment

IncrementAccount treated "the row exists and the UPDATE matched nothing" as
proof that the guard refused the change. It is not proof. The row can also
have been created by another transaction in the window between the UPDATE
and the SELECT that follows it, and in that case the increment was never
applied at all.
```

One logical change per commit. A fix bundled into a feature commit cannot be reverted or
cherry-picked on its own, which for a money library is not a stylistic concern.

---

## Never commit

- Build output, coverage files, profiles, or editor and OS clutter — `.gitignore` covers
  most of it, but check `git status` before committing.
- Local database files or dumps from running the integration suite.
- Secrets, DSNs, or credentials, including in test files. Integration tests read DSNs from
  environment variables and skip when they are unset; keep it that way.

## Running things

**Check your Go version before running the integration suite.** The library needs Go 1.22,
but `integration/` needs **Go 1.25**, because its drivers declare it. With the wrong
toolchain you get `go.mod requires go >= 1.25.0` from a command that looks unrelated.

```bash
go test ./...                              # library tests, no database needed
go vet ./... && gofmt -l .                 # must both be clean
go test -race ./...                        # concurrency paths

go run ./examples/01-quickstart            # end to end in 30 seconds

# Integration suite, in its own module. Skipped unless DSNs are set.
cd integration && docker compose up -d --wait
DISTLEDGER_TEST_MYSQL_DSN='root:distledger@tcp(127.0.0.1:13307)/distledger?charset=utf8mb4&parseTime=false' \
DISTLEDGER_TEST_POSTGRES_DSN='postgres://postgres:distledger@127.0.0.1:15433/distledger?sslmode=disable' \
  go test ./...
docker compose down
```

The integration suite runs the same assertions against memory, SQLite, MySQL and
PostgreSQL. A backend that is unset is skipped, not failed, so you can run it against
whichever database you have.

---

## Fault reviews

When you fix a bug that was not caught by an existing test, please add a review to
`docs/fault-reviews/`, named `YYYY-MM-DD-<short description>.md`. The template and the
existing entries are the guide.

The part worth the effort is the analysis chain: symptom → direct cause → root cause →
**why it was not caught earlier**. That last step is where the useful lesson usually is.
In the existing entry, the answer was "the test discarded the error", which is why the
fix in `CONTRIBUTING.md` rule 1 under "Tests that pin cost" exists.

---

## Pull requests

The template will ask for the essential things. In short:

- What changed and **why** — link the issue if there is one
- Which invariant or contract this affects, if any
- What you measured, if the change is about performance
- Confirmation that the two hard rules hold

A change to accounting behaviour without a test that would fail before it is likely to be
sent back. This is not gatekeeping for its own sake: the failure mode of this library is
money silently disappearing, and the only defence is that a test would have caught it.

## Response time

There is none guaranteed. This is maintained in the open by people with other work. A
polite nudge after a couple of weeks is fine and welcome.
