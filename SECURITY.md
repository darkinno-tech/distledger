# Security Policy

## Reporting a vulnerability

**Please do not open a public issue.** Use GitHub's private reporting:

**https://github.com/darkinno-tech/distledger/security/advisories/new**

If that is unavailable to you, open a public issue that says only *"I have a security
report and need a private channel"* — with no details — and a maintainer will arrange one.
The maintainers can also be reached at **DarkInno@mail.im10furry.cn**, which is the
organization's published contact address.

There is no guaranteed response time; this is maintained in the open. You will get an
acknowledgement when someone sees it, and credit in the advisory unless you prefer otherwise.

## What counts as a vulnerability here

This library moves money and is embedded in someone else's process, so the relevant classes
are narrower and more specific than in a web service. In scope:

| Class | Example |
|---|---|
| **Accounting integrity** | An input that makes balances disagree with the ledger without returning an error. This is the worst kind and is treated as critical |
| **Authorization / tenant isolation** | Any way to read or affect another tenant's money. Tenants are the isolation boundary, and the library treats cross-tenant access as a hard failure |
| **Arithmetic** | `int64` overflow, sign confusion, or truncation that produces a wrong amount rather than an error. Amounts are minor units and rates are basis points; floating point must never participate |
| **Idempotency** | A replay that applies twice, or a genuine distinct event that is silently swallowed |
| **SQL injection** | Identifiers and values are parameterized or quoted through `Dialect`; a path that bypasses that is a vulnerability |
| **Denial of service** | An input that makes a per-minute `Maintain` unbounded, or makes reconciliation or self-check unbounded |
| **Dependency surface** | Anything that would require adding a third-party dependency. The empty `require` block is a security property, not a style rule |

## What is not a vulnerability

These are deliberate, documented behaviours. Reporting them as vulnerabilities is a
misunderstanding rather than a finding:

- **The library does not make compliance judgements.** It does accounting only. Whether a
  distribution scheme is lawful in your jurisdiction is your responsibility, and the
  README says so at length.
- **It does not authenticate or authorize callers.** Tenant ids come from your application.
  If your code lets a user choose an arbitrary tenant id, that is your access-control bug.
- **It does not encrypt data at rest.** Use an encrypted database or volume.
- **`Available` does not mean paid out.** Withdrawals are not built yet, so the library
  cannot and does not report that money has left.
- **Zero rates pay nothing by default, and `Levels` is capped at 3.** Both are intentional
  and are documented as such (ADR-010, and the compliance notice in the README).
- **Amounts are truncated rather than rounded up by default.** Conservative by design.

If you are unsure which side of that line something falls on, report it privately and say
so. A misclassified report costs a reply; an unreported accounting bug costs money.

## Supported versions

The project is pre-1.0. Only the latest tagged release is supported:

| Version | Supported |
|---|---|
| v0.1.0 (the only release so far) | ✅ |
| `main` | ✅ — fixes land here first |
| Older tags | ❌ |

Once v1.0 exists, this table will be replaced with a real support window.

## What to include

A minimal reproduction is worth more than a description. Specifically:

- The version and Go version
- Which store backend, and whether it reproduces on `store/memory`
  (a report that reproduces in memory is far easier to act on)
- A minimal Go program, ideally against `store/memory` so it needs nothing installed
- The expected and actual amounts, if amounts are involved
- `led.SelfCheck(ctx, tenantID)` output, if balances are involved

## What happens next

1. Acknowledgement, and a first read on severity.
2. A fix on `main`, with a test that fails before it — the same standard as any other bug,
   because a security fix without a regression test is a fix that can silently regress.
3. A patch release, and a GitHub security advisory crediting you unless you prefer otherwise.

Accounting-integrity and tenant-isolation reports are treated as critical and prioritised
ahead of everything else in the backlog.
