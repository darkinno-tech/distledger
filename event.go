package distledger

import (
	"time"
)

// This file defines the **event contract** between the library and external
// systems.
//
// Deliberate design choices (see ADR-012):
//   - Everything is a plain Go struct: no protobuf, no codegen, no reflection
//     registry.
//   - Every event is **idempotent**: a duplicate delivery never books a second
//     entry. A caller can attach it straight to an at-least-once message queue
//     without designing its own deduplication.
//   - No event carries any "the library needs to call you back" field: the
//     library never takes over the caller's lifecycle.

// OrderItem is one line item within an order.
//
// Items only need to be supplied when commission has to be computed per SKU;
// without them the whole order is commissioned as a single unit.
type OrderItem struct {
	// ItemID is the line item identifier, unique within the order.
	ItemID string
	// Amount is the amount of this line that joins the commission base (the
	// line's share of discounts and shipping already deducted).
	Amount Money
}

// OrderPaidEvent means an order was paid successfully.
//
// This is the trigger for commission. On receiving the event the library
// performs: attribution → per-level commission computation → accrual into the
// "pending settlement" bucket → ledger write. The whole thing happens inside
// **one transaction**.
type OrderPaidEvent struct {
	TenantID    int64
	OrderID     string
	BuyerUserID int64
	// PaidAmount is the amount actually paid (shipping and platform-funded
	// coupons deducted); it is also the total cap on commission.
	PaidAmount Money
	// Items is optional. When supplied, commission is computed per line item;
	// when omitted, the order is commissioned as a whole.
	Items []OrderItem
	// OrderID identifies the order and must be unique within the tenant, and
	// never reused.
	//
	// Order-level idempotency is keyed on it, so reusing an id for a genuinely
	// different order makes the second one look like a replay of the first and it
	// would be dropped. The buyer check in OnOrderPaid catches reuse across
	// different buyers; reuse by the same buyer cannot be detected from the event
	// alone, which is why this is a contract rather than a validation.
	//
	// PaidAt is the payment time and is **required**; it decides whether the
	// binding existed and was effective at the moment of payment.
	//
	// It is required rather than "leave it empty and take the current time",
	// and that is a safety requirement: substituting the current time would let
	// a replay that happened before the binding existed (no binding on the
	// first delivery, someone adds one later and replays) be retroactively
	// accepted as valid attribution — which is to say, it would leave a door
	// open for "grabbing the order after the fact". Better to ask the caller
	// for one extra field.
	PaidAt time.Time
	// IdemKey is optional. When empty the library derives one; when supplied it
	// is mapped into a separate namespace, so it cannot impersonate a
	// library-derived key (see idem.go).
	IdemKey string
}

func (e OrderPaidEvent) orderKey() OrderKey {
	return OrderKey{TenantID: e.TenantID, OrderID: e.OrderID}
}

func (e OrderPaidEvent) validate() error {
	if err := e.orderKey().Validate(); err != nil {
		return err
	}
	if e.IdemKey != "" {
		if err := validateIdemKeyOverride(e.IdemKey); err != nil {
			return err
		}
	}
	if e.BuyerUserID <= 0 {
		return fieldErrf("buyer_user_id", "must be > 0, got %d", e.BuyerUserID)
	}
	if e.PaidAmount < 0 {
		return fieldErrf("paid_amount", "must be >= 0, got %s", e.PaidAmount)
	}
	if e.PaidAmount == 0 {
		return fieldErr("paid_amount", "must be > 0; zero-amount orders cannot be accrued")
	}
	if e.PaidAt.IsZero() {
		return fieldErr("paid_at",
			"must be set; the library refuses to substitute the current time "+
				"because that would let a binding created after payment capture the order")
	}
	if len(e.Items) > MaxOrderItems {
		return fieldErrf("items", "must contain at most %d entries, got %d", MaxOrderItems, len(e.Items))
	}

	seen := make(map[string]struct{}, len(e.Items))
	var sum Money
	for i, it := range e.Items {
		field := "items[" + i64s(int64(i)) + "].item_id"
		if it.ItemID == "" {
			return fieldErr(field, "must not be empty")
		}
		if len(it.ItemID) > MaxOrderItemIDLen {
			return fieldErrf(field, "must be at most %d bytes, got %d", MaxOrderItemIDLen, len(it.ItemID))
		}
		if it.Amount <= 0 {
			// A zero-amount line cannot produce a commission, so it signals a
			// mapping bug far more often than a real gift item. Rejecting it
			// turns a silent no-op into a loud error, and it also removes the
			// cheapest way to make the engine do unbounded work.
			return fieldErrf("items["+i64s(int64(i))+"].amount", "must be > 0, got %s", it.Amount)
		}
		if _, dup := seen[it.ItemID]; dup {
			return fieldErrf(field, "duplicate item id %q", it.ItemID)
		}
		seen[it.ItemID] = struct{}{}
		next, err := sum.Add(it.Amount)
		if err != nil {
			return err
		}
		sum = next
	}
	// The critical safety check: the sum of the item amounts must not exceed
	// the paid amount.
	//
	// Otherwise a caller (or an attacker forging events) only has to slip in
	// one line with an inflated amount to make the platform pay commission on a
	// base larger than what it actually received — a direct money-loss entry
	// point.
	if sum > e.PaidAmount {
		return fieldErrf("items",
			"sum of item amounts (%s) exceeds paid amount (%s)", sum, e.PaidAmount)
	}
	return nil
}

// OrderReceivedEvent means the buyer confirmed receipt.
//
// It advances pending commissions to the state where the freeze window starts
// counting: AvailableAt = ReceivedAt + the FreezeDays snapshotted at accrual
// time.
//
// This event is idempotent and **first writer wins**: a duplicate delivery of
// the same receipt never changes an AvailableAt that is already determined,
// which stops anyone from pushing the payout time earlier by replaying.
type OrderReceivedEvent struct {
	TenantID   int64
	OrderID    string
	ReceivedAt time.Time
}

func (e OrderReceivedEvent) orderKey() OrderKey {
	return OrderKey{TenantID: e.TenantID, OrderID: e.OrderID}
}

func (e OrderReceivedEvent) validate() error {
	return e.orderKey().Validate()
}

// MaxOrderItems is the maximum number of line items one order may carry.
//
// It exists not because "business should never have more", but because **work
// must be predictable**: commission runs inside a transaction that holds the
// global write lock, so a single request carrying a hundred thousand line
// items would stall the accounting of every tenant for tens of milliseconds. A
// bound is what buys a predictable tail latency.
const MaxOrderItems = 200

// SkipCode explains why one level produced no commission.
//
// Its reason to exist is explainability: when a caller investigates "why did my
// agent not get paid", the answer must be directly visible in the return value
// rather than guessed (see PRD iron rule R6).
type SkipCode uint8

const (
	// SkipZeroAmount means the commission computed from the rate is 0 (usually
	// the rate is not configured).
	SkipZeroAmount SkipCode = iota + 1
	// SkipIneligible means the agent is not eligible for commission (not
	// active / disabled).
	SkipIneligible
	// SkipNoParent means the relation chain breaks here; there is no higher
	// level.
	SkipNoParent
	// SkipCapExhausted means the order's total commissionable amount is used
	// up.
	SkipCapExhausted
	// SkipAgentMissing means an upline agent referenced by the relation chain
	// does not exist.
	SkipAgentMissing
	// SkipFullyReversed means the reversible balance of this commission is
	// already zero.
	SkipFullyReversed
	// SkipAlreadyWithdrawn means the commission has already been paid out and
	// clawing it back needs a debt policy.
	SkipAlreadyWithdrawn
)

// String returns the stable identifier of the skip reason.
func (c SkipCode) String() string {
	switch c {
	case SkipZeroAmount:
		return "zero_amount"
	case SkipIneligible:
		return "ineligible"
	case SkipNoParent:
		return "no_parent"
	case SkipCapExhausted:
		return "cap_exhausted"
	case SkipAgentMissing:
		return "agent_missing"
	case SkipFullyReversed:
		return "fully_reversed"
	case SkipAlreadyWithdrawn:
		return "already_withdrawn"
	default:
		return "unknown"
	}
}

// SkipReason records the concrete reason one level was skipped.
type SkipReason struct {
	// Layer starts at 1.
	Layer       int
	AgentUserID int64
	Code        SkipCode
	Detail      string
}

// AccrueResult is the result of OnOrderPaid.
type AccrueResult struct {
	OrderKey OrderKey
	// Attributed reports whether a valid attribution target was found.
	//
	// false is **not an error**: an order with no referral attribution simply
	// earns no commission, and the order system should not be handed a failure
	// (see the PRD fallback design).
	Attributed bool
	// AgentUserID is the agent the order is attributed to; 0 when unattributed.
	AgentUserID int64
	// Commissions are the commissions **newly created** by this call. Empty on
	// a duplicate delivery.
	Commissions []Commission
	// Replayed reports whether this call was recognized as a replay of an
	// earlier accrual, in which case Commissions is empty.
	Replayed bool
	// UnallocatedBase is the part of the paid amount that no order item claimed.
	//
	// Items define the commissionable base per line, so a total below the paid
	// amount is legitimate (shipping, gift lines, goods that carry no
	// commission). Reporting the gap turns a silently smaller payout into a
	// number the caller can act on: before this existed, forgetting an item
	// looked exactly like a successful accrual.
	UnallocatedBase Money
	// Skipped explains why each level produced no commission.
	Skipped []SkipReason
}

// AccruedAmount returns the total of the commissions newly created by this
// call.
//
// It returns an error instead of silently wrapping: the most common use of this
// function is writing to a log or a reconciliation report, and a total that
// quietly overflowed into a negative number would send the investigation in a
// completely wrong direction.
func (r AccrueResult) AccruedAmount() (Money, error) {
	var sum Money
	var err error
	for _, c := range r.Commissions {
		if sum, err = sum.Add(c.Amount); err != nil {
			return 0, err
		}
	}
	return sum, nil
}

// ReceiveResult is the result of OnOrderReceived.
type ReceiveResult struct {
	OrderKey OrderKey
	// Updated is the number of commissions whose payout time this call
	// determined for the first time. 0 on a duplicate delivery.
	Updated int
}

// MaintainResult is the result of Maintain.
type MaintainResult struct {
	// SettledCount is the number of commissions settled.
	SettledCount int
	// SettledAmount is the total amount settled.
	SettledAmount Money
	// Quarantined holds the IDs of commissions quarantined during this
	// heartbeat because of anomalous data.
	//
	// Quarantining rather than failing bounds the blast radius: one bad record
	// must not bring every agent's withdrawals to a halt. Quarantined records
	// are also logged and flagged by SelfCheck as invariant violations, so they
	// never disappear quietly.
	Quarantined []int64
}

// RefundItem describes how much one line item returned in this refund.
type RefundItem struct {
	// ItemID must match the ItemID supplied in OrderPaidEvent.Items.
	ItemID string
	// Amount is the amount refunded for this line item in this refund.
	Amount Money
}

// OrderRefundedEvent means a refund happened on an order.
//
// It is **idempotent** and may be delivered in any order, any number of times,
// in instalments: the engine computes "how much should have been reversed in
// total", not "how much to deduct this time".
//
// Exactly **one** of three representations must be supplied:
//
//	IsFull  —— full order refund, reverses every computation unit to its cap
//	Items   —— per line item refund, ItemID must match accrual time
//	Amount  —— order-level refund amount, split automatically by each unit's
//	           share of the commission base
type OrderRefundedEvent struct {
	TenantID int64
	OrderID  string
	// IsFull true means Items and Amount are ignored.
	IsFull bool
	// Items is the line item list used for a per-item refund.
	Items []RefundItem
	// Amount is the order-level refund amount when line items are not
	// distinguished.
	Amount Money
	// RefundedAt is when the refund happened and is **required**.
	//
	// It is required for the same reason as PaidAt: a reversal record is an
	// audit voucher, and the time on a voucher must come from the business
	// system, not from the accounting library's current instant.
	RefundedAt time.Time
	// IdemKey identifies this refund in the caller's system and is **required**.
	//
	// It belongs to the same class of agreement as PaidAt: the library needs a
	// fact from the business system to guarantee idempotency rather than
	// guessing. Without it, one redelivery and two genuine refunds of the same
	// amount are completely equivalent in the data, and any implementation can
	// only get it wrong by picking one of the two.
	//
	// In practice this is the refund number returned by the payment channel, or
	// the caller's own refund transaction number.
	IdemKey string
}

func (e OrderRefundedEvent) orderKey() OrderKey {
	return OrderKey{TenantID: e.TenantID, OrderID: e.OrderID}
}

func (e OrderRefundedEvent) validate() error {
	if err := e.orderKey().Validate(); err != nil {
		return err
	}
	if e.RefundedAt.IsZero() {
		return fieldErr("refunded_at", "must be set; an audit record needs the caller's event time")
	}
	if e.IdemKey == "" {
		return fieldErr("idem_key",
			"must be set to the caller's refund identifier; without it a retry and "+
				"a second refund of the same amount are indistinguishable")
	}
	if err := validateIdemKeyOverride(e.IdemKey); err != nil {
		return err
	}

	modes := 0
	if e.IsFull {
		modes++
	}
	if len(e.Items) > 0 {
		modes++
	}
	if e.Amount > 0 {
		modes++
	}
	switch {
	case modes == 0:
		return fieldErr("is_full", "exactly one of is_full, items or amount must be provided")
	case modes > 1:
		return fieldErr("is_full", "is_full, items and amount are mutually exclusive")
	}

	if e.Amount < 0 {
		return fieldErrf("amount", "must be >= 0, got %s", e.Amount)
	}

	seen := make(map[string]struct{}, len(e.Items))
	for i, it := range e.Items {
		field := "items[" + i64s(int64(i)) + "].item_id"
		if len(it.ItemID) > MaxOrderItemIDLen {
			return fieldErrf(field, "must be at most %d bytes, got %d", MaxOrderItemIDLen, len(it.ItemID))
		}
		if it.Amount <= 0 {
			return fieldErrf("items["+i64s(int64(i))+"]..amount", "must be > 0, got %s", it.Amount)
		}
		if _, dup := seen[it.ItemID]; dup {
			return fieldErrf(field, "duplicate item id %q", it.ItemID)
		}
		seen[it.ItemID] = struct{}{}
	}
	return nil
}

// RefundResult is the result of OnOrderRefunded.
type RefundResult struct {
	OrderKey OrderKey
	// Reversed are the reversal records **newly created** by this call
	// (negative Amount).
	Reversed []Commission
	// ReversedAmount is the total amount reversed by this call (positive).
	ReversedAmount Money
	// AlreadyReversed reports that this call left no new reversal behind.
	//
	// When it is true and err is nil, the meaning is "the reversal this refund
	// needed was already done" — a duplicate delivery, or the refund is already
	// refunded down to the floor.
	AlreadyReversed bool
	// Skipped explains which commissions were not reversed, and why.
	Skipped []SkipReason
}

// BindAgentRequest is a request to register one agent.
type BindAgentRequest struct {
	TenantID int64
	UserID   int64
	// ParentID is the user ID of the direct upline; 0 means a top-level agent.
	ParentID int64
	JoinType JoinType
	// Status, when left empty (the zero value), is treated as AgentInactive
	// and must be activated explicitly. A caller using a "no entry requirement"
	// mode may pass AgentActive.
	Status AgentStatus
	// RateOverrideBP is optional and overrides the global rate.
	RateOverrideBP *Rate
	// Idempotent true means registering the same (UserID, ParentID) twice does
	// not error. The default false behavior is also idempotent; this field is
	// reserved for a future strict mode.
	Idempotent bool
}

// BindBuyerRequest is a request to create a "buyer → agent" attribution
// relation.
type BindBuyerRequest struct {
	TenantID    int64
	BuyerUserID int64
	AgentUserID int64
	Source      BindSource
	SourceRef   string
	// BoundAt, when zero, means the Clock's current time is used.
	BoundAt time.Time
}
