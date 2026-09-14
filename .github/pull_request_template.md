## What and why

<!-- The "why" is the part that cannot be read off the diff. What was wrong, or what was impossible? -->

Closes #

## Which contract does this touch?

<!-- Tick what applies. If it touches an invariant or the store port, say more below. -->

- [ ] Nothing in the public contract — internal only
- [ ] An invariant (I1 / I2 / I3 / I4)
- [ ] The `Store` / `Reader` / `Writer` / `Tx` port
- [ ] A dialect (`store/mysql`, `store/postgres`, `store/sqlite`)
- [ ] Behaviour of one of the four entry points (`OnOrderPaid`, `OnOrderReceived`, `OnOrderRefunded`, `Maintain`)
- [ ] Docs only

## The two hard rules

- [ ] `go test ./...` at the repository root passes **without a database**
- [ ] `go.mod` still has an empty `require` block

<!-- CI checks both, so this is a reminder rather than the gate. -->

## Tests

<!--
A change to accounting behaviour without a test that would fail before it is likely to be
sent back. The failure mode of this library is money silently disappearing, and a test is
the only defence.
-->

- [ ] I added a test that **fails without this change**
- [ ] Not applicable — docs or comments only

If the change is about cost rather than results, describe what you measured:

<!--
Some properties here cannot be seen from results. A heartbeat that asks for work and one
that asks for tenants settle identical commissions; the difference only shows at scale.
If that is what you fixed, say how you pinned it — a counted round trip, a query plan, or
an assertion on a bound being reached rather than merely respected.
-->

## Verification

```bash
go vet ./... && gofmt -l .
go test -race ./...
```

<!-- Paste the result. If you also ran the integration suite, say which backends. -->

## Anything reviewers should know

<!--
Deliberate trade-offs, things you tried and rejected, parts you are unsure about, or a
follow-up you are consciously leaving out. Saying "I am unsure about X" is useful, not
a weakness.
-->
