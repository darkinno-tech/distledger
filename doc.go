// Package distledger is a Go distribution ledger core.
//
// It solves exactly three things: the relation chain, the commission ledger, and
// the funds flow. All rules (how many levels, what rates, whether there is a
// threshold) are externalized as pluggable strategies, because "rules" are an
// endless long tail while "accounting" is a constant across every distribution
// scenario:
//
//	order attributed to an agent → N commission levels per the rules →
//	frozen → withdrawable at maturity → clawed back on refund → payout
//
// # Design invariants
//
//   - Where to draw the line: decisions that change where the money flows go
//     into the core; decisions that only change the amounts become rules. The
//     rules layer is never allowed to modify state.
//   - Pay nothing by default: rate 0 and 2 levels by default. Tedious
//     configuration beats a wrong payout.
//   - Integer amounts: always int64 minor units (Money) plus integer
//     basis-point rates (Rate); floating point never enters the arithmetic.
//   - Immutable ledger: commission financial fields and the funds flow are
//     append-only; a reversal is a new negative record.
//   - Zero magic: no codegen, no reflection-based registration, no global
//     singletons.
//
// # Quick start
//
//	led, err := distledger.New(distledger.Config{
//		Store: memory.New(),
//		Rules: distledger.Rules{Levels: 2, RateBP: []int32{500, 200}, FreezeDays: 7},
//	})
//	if err != nil {
//		log.Fatal(err)
//	}
//	_, err = led.OnOrderPaid(ctx, distledger.OrderPaidEvent{
//		OrderID: "ORD-1", BuyerUserID: 1003, PaidAmount: distledger.Money(19900),
//	})
//
// See examples/01-quickstart for a complete runnable example.
//
// # What it does not do
//
// Promo links, posters, leaderboards, earnings centers, agent approval flows,
// payment and split settlement, team commissions / regional dividends /
// chain-driven payouts, per-head or per-signup payouts, HTTP routing, and an
// admin back office. PRD section 3.2 carries the full list, and that list is
// binding: it is what keeps this package from growing into a distribution
// system.
package distledger
