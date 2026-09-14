package distledger

import (
	"fmt"
	"time"
)

// JoinType describes how an agent joined the programme.
//
// Note: JoinPaid (paid enrolment) is only a **status marker** and takes no part
// in any payout computation. The library deliberately offers no path of the
// form "pay to become an agent and thereby qualify for commission" — the
// combination of an entry fee plus recruitment of downlines is the core of a
// pyramid-scheme finding (see ADR-011, PRD §15.2).
type JoinType uint8

const (
	// JoinFree means the agent joined with no entry requirement.
	JoinFree JoinType = iota
	// JoinReviewed means the agent joined after a review.
	JoinReviewed
	// JoinPaid means the agent paid to join (a status marker only).
	JoinPaid
)

// Valid reports whether the join type is a known value.
func (j JoinType) Valid() bool { return j <= JoinPaid }

// String returns the English name of the join type.
func (j JoinType) String() string {
	switch j {
	case JoinFree:
		return "free"
	case JoinReviewed:
		return "reviewed"
	case JoinPaid:
		return "paid"
	default:
		return fmt.Sprintf("JoinType(%d)", uint8(j))
	}
}

// AgentStatus describes the lifecycle state of an agent.
type AgentStatus uint8

const (
	// AgentInactive means the agent is registered but not yet active and earns
	// no commission.
	AgentInactive AgentStatus = iota
	// AgentActive means the agent is in good standing and earns commission.
	AgentActive
	// AgentDisabled means the agent is disabled, earns no commission, and the
	// attribution chain of its downlines is truncated here.
	AgentDisabled
)

// Valid reports whether the status is a known value.
func (s AgentStatus) Valid() bool { return s <= AgentDisabled }

// String returns the status name.
func (s AgentStatus) String() string {
	switch s {
	case AgentInactive:
		return "inactive"
	case AgentActive:
		return "active"
	case AgentDisabled:
		return "disabled"
	default:
		return fmt.Sprintf("AgentStatus(%d)", uint8(s))
	}
}

// Agent is one agent.
//
// The relation chain is expressed as a parent pointer plus a depth. A
// materialised path was deliberately left out of v0.1: the depth is hard-capped
// at 3, so walking upwards takes at most 3 steps and a path would add
// complexity with no measurable payoff; it can be introduced when "query the
// whole team" becomes a requirement (see ADR-006).
type Agent struct {
	Key      UserKey
	ParentID int64
	// Depth is the level depth, 1 for a top-level agent. It equals "the
	// minimum number of steps up to the root, plus 1".
	Depth    int
	Status   AgentStatus
	JoinType JoinType
	// RateOverrideBP, when non-nil, overrides the global rate for per-agent
	// differences such as "gold" agents.
	RateOverrideBP *Rate
	CreatedAt      time.Time
	UpdatedAt      time.Time
	// Version is the optimistic-locking version, incremented on every persist.
	Version int64
}

// BindSource describes where a binding came from, for attribution analysis and
// anti-fraud.
type BindSource uint8

const (
	// SourceLink means the binding came from a referral link.
	SourceLink BindSource = iota + 1
	// SourceInviteCode means the binding came from an invite code.
	SourceInviteCode
	// SourceManual means an operator created the binding by hand.
	SourceManual
	// SourceSalesperson means the binding came from a salesperson staff ID.
	SourceSalesperson
)

// Valid reports whether the source is a known value.
func (s BindSource) Valid() bool { return s >= SourceLink && s <= SourceSalesperson }

// String returns the source name.
func (s BindSource) String() string {
	switch s {
	case SourceLink:
		return "link"
	case SourceInviteCode:
		return "invite_code"
	case SourceManual:
		return "manual"
	case SourceSalesperson:
		return "salesperson"
	default:
		return fmt.Sprintf("BindSource(%d)", uint8(s))
	}
}

// Binding is the "buyer → agent" attribution relation.
//
// It is deliberately its own table rather than duplicated on the order: the
// binding term, poaching protection and the rebinding cooldown all need to
// modify the binding itself, and stuffing them into the order table would make
// history unchangeable (see ADR-006).
type Binding struct {
	ID int64
	// Buyer is the buyer being bound.
	Buyer UserKey
	// AgentUserID is the agent the buyer is attributed to.
	AgentUserID int64
	Source      BindSource
	SourceRef   string
	BoundAt     time.Time
	// ExpireAt, when zero, means the binding never expires.
	ExpireAt time.Time
	// Active false means this binding was replaced or has lapsed.
	Active bool
	// ReboundFrom records the ID of the binding this one replaced; 0 means a
	// first-time binding.
	ReboundFrom int64
	Version     int64
}

// Effective reports whether the binding is in effect at time at.
func (b Binding) Effective(at time.Time) bool {
	if !b.Active {
		return false
	}
	if b.ExpireAt.IsZero() {
		return true
	}
	return at.Before(b.ExpireAt)
}

// CommissionState describes the lifecycle state of a commission.
type CommissionState uint8

const (
	// CommissionPending means the commission is accrued and waits for
	// settlement once the freeze window ends.
	CommissionPending CommissionState = iota
	// CommissionSettled means it is settled and counts towards the withdrawable
	// balance.
	CommissionSettled
	// CommissionWithdrawn means it has been withdrawn.
	CommissionWithdrawn
	// CommissionReversed means it was reversed by a refund.
	CommissionReversed
	// CommissionFrozen means risk control froze it pending manual handling.
	CommissionFrozen
	// CommissionVoid means it is void and has no financial effect whatsoever.
	CommissionVoid
)

// Valid reports whether the state is a known value.
func (s CommissionState) Valid() bool { return s <= CommissionVoid }

// String returns the state name.
func (s CommissionState) String() string {
	switch s {
	case CommissionPending:
		return "pending"
	case CommissionSettled:
		return "settled"
	case CommissionWithdrawn:
		return "withdrawn"
	case CommissionReversed:
		return "reversed"
	case CommissionFrozen:
		return "frozen"
	case CommissionVoid:
		return "void"
	default:
		return fmt.Sprintf("CommissionState(%d)", uint8(s))
	}
}

// Terminal reports whether the state is final (no further transitions).
func (s CommissionState) Terminal() bool {
	switch s {
	case CommissionWithdrawn, CommissionReversed, CommissionVoid:
		return true
	default:
		return false
	}
}

// Commission is one commission payout.
//
// # Mutability contract (important)
//
// Financial fields **never change** after creation:
//
//	Key / IdemKey / OrderItemID / BuyerUserID / AgentUserID / Layer /
//	BaseAmount / Rate / Amount / FreezeDays / RuleVersion / RuleSnapshot / AccruedAt
//
// Lifecycle fields change as the state advances, and every change must carry a
// Version for CAS:
//
//	State / AvailableAt / SettledAt / ReversedAmount / Version
//
// A refund does not modify Amount; it **appends a negative-amount record** and
// marks the original as Reversed (see ADR-004). The Store port enforces this
// contract: the only write methods are AppendCommission /
// SetCommissionAvailableAt / TransitionCommission, and there is no generic
// Update.
type Commission struct {
	ID int64
	// Key is the order the commission was earned on.
	Key OrderKey
	// IdemKey is the idempotency key, unique within a tenant.
	IdemKey     string
	OrderItemID string
	BuyerUserID int64
	AgentUserID int64
	// Layer is the level: 1 is the direct referrer, 2 its upline, and so on.
	Layer int
	// BaseAmount is the snapshotted commission base (shipping and platform
	// coupons already deducted).
	BaseAmount Money
	// Rate is the snapshotted rate.
	Rate Rate
	// Amount is the commission amount; a positive value is an accrual (a refund
	// reversal record is negative).
	Amount Money
	State  CommissionState
	// FreezeDays is the snapshotted freeze window. It guarantees that "the
	// rules in force at the time decide the payout time of the time": later
	// edits to global configuration never retroactively affect commissions
	// already accrued (see ADR-005).
	FreezeDays int
	// AvailableAt is the accrual time plus FreezeDays. A zero value means the
	// order has not been receipt-confirmed yet, so the payout time is not yet
	// determined.
	AvailableAt time.Time
	// RuleVersion points at the rule version in force at accrual time.
	RuleVersion int64
	// RuleSnapshot is a JSON snapshot of the matched rule, used later to
	// explain "why this number".
	RuleSnapshot string
	AccruedAt    time.Time
	SettledAt    time.Time
	// ReversedAmount is the cumulative amount of this commission that has been
	// reversed (positive, never greater than Amount).
	//
	// It is a lifecycle field rather than a financial field: Amount records
	// "how much was computed back then", while ReversedAmount records "how much
	// of that has been taken back by refunds". Keeping the two apart is what
	// lets the original entry stay immutable while still answering "how much is
	// left to settle".
	//
	// The reversal detail still lands in separate negative-amount records;
	// ReversedAmount is the cumulative view over them. SelfCheck reconciles the
	// two (invariant I3).
	ReversedAmount Money
	// ReverseOf, on a reversal record, points at the original commission ID
	// that was reversed; it is 0 on forward records.
	ReverseOf int64
	Version   int64
}

// OutstandingAmount returns the amount of this commission that is not yet
// reversed.
//
// It is the amount settlement should actually move from "pending" to
// "withdrawable". Using Amount directly would settle a partially reversed
// commission a second time, paying out money that was already taken back.
func (c Commission) OutstandingAmount() Money { return c.Amount - c.ReversedAmount }

// MaxReversible returns the upper bound of ReversedAmount.
//
// For a forward record the bound is its own amount; for a reversal record
// (negative Amount) the bound is 0 — a reversal can never itself be reversed.
func (c Commission) MaxReversible() Money {
	if c.Amount <= 0 {
		return 0
	}
	return c.Amount
}

// FullyReversed reports whether this commission has been fully reversed.
func (c Commission) FullyReversed() bool { return c.Amount > 0 && c.ReversedAmount >= c.Amount }

// DueAt reports whether the commission has reached its settleable point at
// time at.
//
// A commission whose receipt is not yet confirmed (zero AvailableAt) is never
// due.
func (c Commission) DueAt(at time.Time) bool {
	if c.AvailableAt.IsZero() {
		return false
	}
	return !at.Before(c.AvailableAt)
}

// Account is one agent's money account.
//
// The four buckets are deliberately kept apart instead of a single stored
// "balance": what users see must be four distinct things — pending settlement,
// withdrawable, withdrawing, withdrawn — and collapsing them into one number is
// guaranteed to cause disputes.
type Account struct {
	Key UserKey
	// Frozen is the amount pending settlement (accrued, freeze window not over).
	Frozen Money
	// Available is the withdrawable amount.
	Available Money
	// Withdrawing is the amount being withdrawn (requested, not yet paid out).
	Withdrawing Money
	// Withdrawn is the amount already paid out.
	Withdrawn Money
	// TotalEarned is the lifetime accrued amount (gross); it only ever grows.
	TotalEarned Money
	// TotalReversed is the lifetime reversed amount (positive); it only ever
	// grows.
	//
	// It is used in pairs with TotalEarned: net earnings = TotalEarned -
	// TotalReversed. TotalEarned is left untouched by reversals in order to
	// preserve "gross" as an independent fact — a field that only reflects the
	// net cannot answer "how much commission has this agent generated in
	// total".
	TotalReversed Money
	Version       int64
	UpdatedAt     time.Time
}

// Settleable reports whether all four money buckets of the account are
// non-negative.
//
// This is part of invariant I1: a negative bucket balance is never allowed.
func (a Account) Settleable() bool {
	return a.Frozen >= 0 && a.Available >= 0 && a.Withdrawing >= 0 && a.Withdrawn >= 0
}

// Refund is the in-library record of one refund instalment for one accrual item.
//
// # Why this record has to exist
//
// Without it, two situations are indistinguishable: the same refund event being
// delivered twice, and two genuine refunds of the same amount. Any design that
// tries to derive the clawback from "the refund amount in this event" will
// either double-apply a retry or ignore a legitimate second refund.
//
// The record carries the cumulative refunded amount for its item, so the
// clawback can be expressed as a target
// (amount x cumulative / base) rather than as a delta. Combined with a required
// caller-supplied IdemKey, that makes retries free and repeated refunds exact.
type Refund struct {
	ID  int64
	Key OrderKey
	// ItemID matches Commission.OrderItemID; order-level accruals use "".
	ItemID string
	// Amount is what this instalment refunded for the item.
	Amount Money
	// Cumulative is the total refunded for the item up to and including this
	// instalment. It never exceeds the item's accrual base.
	Cumulative Money
	// IdemKey identifies the refund event in the caller's system.
	IdemKey    string
	RefundedAt time.Time
}

// LedgerBizType describes the business type of one money movement.
type LedgerBizType uint8

const (
	// LedgerAccrue means a commission accrual (into Frozen).
	LedgerAccrue LedgerBizType = iota + 1
	// LedgerSettle means settlement at the end of the freeze window
	// (Frozen → Available).
	LedgerSettle
	// LedgerReverse means a refund reversal.
	LedgerReverse
	// LedgerVoid means a risk-control confiscation (the commission was judged
	// not to exist and is clawed back in full).
	LedgerVoid
	// LedgerWithdrawHold means a withdrawal request holds funds
	// (Available → Withdrawing).
	LedgerWithdrawHold
	// LedgerWithdrawPaid means a withdrawal payout completed
	// (Withdrawing → Withdrawn).
	LedgerWithdrawPaid
	// LedgerWithdrawRefund means a rejected withdrawal returns funds
	// (Withdrawing → Available).
	LedgerWithdrawRefund
	// LedgerManualAdjust means a manual adjustment.
	LedgerManualAdjust
)

// Valid reports whether the business type is a known value.
func (t LedgerBizType) Valid() bool { return t >= LedgerAccrue && t <= LedgerManualAdjust }

// String returns the business type name.
func (t LedgerBizType) String() string {
	switch t {
	case LedgerAccrue:
		return "accrue"
	case LedgerSettle:
		return "settle"
	case LedgerReverse:
		return "reverse"
	case LedgerVoid:
		return "void"
	case LedgerWithdrawHold:
		return "withdraw_hold"
	case LedgerWithdrawPaid:
		return "withdraw_paid"
	case LedgerWithdrawRefund:
		return "withdraw_refund"
	case LedgerManualAdjust:
		return "manual_adjust"
	default:
		return fmt.Sprintf("LedgerBizType(%d)", uint8(t))
	}
}

// referencesCommission reports whether a ledger entry's BizID points at a
// commission record.
func (t LedgerBizType) referencesCommission() bool {
	switch t {
	case LedgerAccrue, LedgerReverse, LedgerVoid:
		return true
	default:
		return false
	}
}

// LedgerEntry is one record of the money ledger.
//
// It is the landing point of **double-entry bookkeeping**: every change to an
// account balance must produce a ledger entry, and the entry records both the
// delta and the balance after the change. The latter lets reconciliation judge
// the balance at that moment from a single record rather than replaying the
// whole history.
//
// Ledger entries are **append-only: never modified, never deleted** (see
// ADR-004).
type LedgerEntry struct {
	ID      int64
	Key     UserKey
	BizType LedgerBizType
	// BizID links the business document (a commission ID or a withdrawal ID).
	BizID string

	DeltaFrozen      Money
	DeltaAvailable   Money
	DeltaWithdrawing Money
	DeltaWithdrawn   Money

	AfterFrozen      Money
	AfterAvailable   Money
	AfterWithdrawing Money
	AfterWithdrawn   Money

	Remark    string
	CreatedAt time.Time
}

// NetDelta returns the total change across the four buckets, used to check
// quickly whether a ledger entry mints money out of thin air.
func (e LedgerEntry) NetDelta() Money {
	return e.DeltaFrozen + e.DeltaAvailable + e.DeltaWithdrawing + e.DeltaWithdrawn
}
