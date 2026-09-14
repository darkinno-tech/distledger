package distledger

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"strings"
)

// MaxLevels is the hard ceiling on distribution depth.
//
// It is not a configuration knob but a deliberately hard-coded compliance
// backstop: distribution deeper than three levels sits in high-risk
// territory under Chinese law (the Regulations on Prohibition of Pyramid
// Selling and Article 224-1 of the Criminal Law). Letting operations set it
// to 4 would let a single configuration accident become legal exposure (see
// ADR-011).
const MaxLevels = 3

// MaxFreezeDays is the upper bound on the freeze window (10 years).
//
// The bound exists to catch a freeze window entered in seconds where days
// were meant, which would leave a commission permanently out of reach
// without ever raising an error. Silent mistakes like that are far harder to
// track down than an outright error.
const MaxFreezeDays = 3650

// MaxBindExpireDays is the upper bound on the binding validity period (100 years).
const MaxBindExpireDays = 36500

// Rules holds the **business rules** of distribution.
//
// It answers only "how much, at how many levels, and when does the money
// land"; it never takes part in deciding how money flows. That is the split
// defined by ADR-002: no field in Rules can change the shape of the state
// machine.
type Rules struct {
	// Levels is the number of commission levels, in [1, MaxLevels]. Default 2.
	Levels int

	// RateBP is the rate of each level; its length must equal Levels.
	//
	// Index 0 is level 1 (the agent the order is attributed to). The
	// default is all zeros, i.e. "the chain works but no money is paid out",
	// which is the conservative default of ADR-010.
	RateBP []Rate

	// Rounding is the rounding mode used when computing commissions.
	// Defaults to RoundDown.
	Rounding Rounding

	// FreezeDays is "withdrawable N days after receipt confirmation".
	// Default 7.
	//
	// It is **snapshotted** into the commission when that commission is
	// accrued, so changing this field does not retroactively affect
	// commissions already accrued (see ADR-005).
	FreezeDays int

	// MaxAllocatableBP caps, as a ratio of the commission base, how much one
	// order may pay out in total. Defaults to 100%.
	//
	// It is the platform-side last gate: even if RateBP is misconfigured as
	// {5000, 5000} (100% in total), the engine still guarantees that the
	// commissions paid out never exceed the base. Validation rejects illegal
	// RateBP combinations too, so this field is the runtime second line of
	// defence (see PRD invariant I2).
	MaxAllocatableBP Rate

	// BindExpireDays is the validity period of a buyer binding, in days.
	// 0 means it never expires. Default 0.
	BindExpireDays int
}

// DefaultRules returns the conservative default rules.
//
// Zero rates, two levels, a 7-day freeze window and settlement driven by the
// caller rather than by any internal timer. A first integration must
// configure rates explicitly before any commission is actually paid out; that
// step is a deliberate confirmation (see ADR-010).
func DefaultRules() Rules {
	return Rules{
		Levels:           2,
		RateBP:           []Rate{0, 0},
		Rounding:         RoundDown,
		FreezeDays:       7,
		MaxAllocatableBP: MaxRateBP,
		BindExpireDays:   0,
	}
}

// Normalize fills in zero-valued fields so that Rules can be partially
// initialized.
//
// The rule: only fields whose zero value is meaningless are filled in.
// A zero Levels is illegal rather than a default, so the caller must set it
// explicitly or use DefaultRules() as a whole.
func (r Rules) Normalize() Rules {
	// The zero value of Rounding is RoundDown, so "unset" and "explicitly
	// truncate" cannot be told apart — and there is no need to: both give the
	// same result, and both are the most conservative choice.
	if r.MaxAllocatableBP == 0 {
		r.MaxAllocatableBP = MaxRateBP
	}
	if r.Levels == 0 {
		r.Levels = 2
	}
	// Fill in rates only when they are missing entirely. A non-zero length
	// that does not match Levels is a configuration error and must be
	// reported rather than silently zero-filled: silent zero-filling turns
	// "one level was left unconfigured" into "that level pays nothing", an
	// error that never raises an alarm.
	if len(r.RateBP) == 0 {
		r.RateBP = make([]Rate, r.Levels)
	}
	return r
}

// Validate checks that the rules are self-consistent.
//
// Rules that fail validation are always rejected at startup rather than
// handled best-effort: running with broken rules pays out the wrong money,
// and that cannot be undone.
func (r Rules) Validate() error {
	if r.Levels < 1 || r.Levels > MaxLevels {
		return fieldErrf("rules.levels", "must be in [1, %d], got %d", MaxLevels, r.Levels)
	}
	if len(r.RateBP) != r.Levels {
		return fieldErrf("rules.rate_bp",
			"length must equal levels (%d), got %d", r.Levels, len(r.RateBP))
	}
	if !r.Rounding.Valid() {
		return fieldErrf("rules.rounding", "unknown mode %d", r.Rounding)
	}
	if r.FreezeDays < 0 || r.FreezeDays > MaxFreezeDays {
		return fieldErrf("rules.freeze_days", "must be in [0, %d], got %d", MaxFreezeDays, r.FreezeDays)
	}
	if r.BindExpireDays < 0 || r.BindExpireDays > MaxBindExpireDays {
		return fieldErrf("rules.bind_expire_days",
			"must be in [0, %d], got %d", MaxBindExpireDays, r.BindExpireDays)
	}
	if !r.MaxAllocatableBP.Valid() {
		return fieldErrf("rules.max_allocatable_bp",
			"must be in [0, %d], got %d", MaxRateBP, r.MaxAllocatableBP)
	}
	if r.MaxAllocatableBP == 0 {
		return fieldErr("rules.max_allocatable_bp", "must be > 0")
	}

	var sum int64
	for i, rate := range r.RateBP {
		if !rate.Valid() {
			return fieldErrf("rules.rate_bp", "level %d out of range [0, %d]: %d",
				i+1, MaxRateBP, rate)
		}
		sum += int64(rate)
	}
	if sum > int64(r.MaxAllocatableBP) {
		return fieldErrf("rules.rate_bp",
			"sum of rates (%d bp) exceeds max allocatable (%d bp)", sum, r.MaxAllocatableBP)
	}
	return nil
}

// Describe returns a compact description of the rules, for the RuleSnapshot
// written onto a commission.
//
// It deliberately excludes timestamps and machine information: the same rules
// must always produce exactly the same snapshot string, so that "which
// commissions used the same version of the rules" can be grouped and counted
// with a plain string comparison.
func (r Rules) Describe() string {
	var b strings.Builder
	fmt.Fprintf(&b, `{"levels":%d,"rate_bp":[`, r.Levels)
	for i, rate := range r.RateBP {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "%d", rate)
	}
	fmt.Fprintf(&b, `],"rounding":%q,"freeze_days":%d,"max_allocatable_bp":%d}`,
		r.Rounding.String(), r.FreezeDays, r.MaxAllocatableBP)
	return b.String()
}

// Version returns the **content fingerprint** of the rule set.
//
// It is a content-addressed version number: the same rules always yield the
// same value, and a change anywhere in the rules yields a different one.
// Recording it on a commission makes it possible to answer, three months
// later, "which version of the rules was this commission computed with".
//
// A self-incrementing version number plus a version table is deliberately
// avoided here: that would need extra persistence and operational process,
// while a content fingerprint already delivers the same explainability in
// v0.1.
func (r Rules) Version() int64 {
	sum := sha256.Sum256([]byte(r.Describe()))
	// Shifting right by one guarantees a non-negative result, which makes it
	// easier to read in logs and SQL.
	return int64(binary.BigEndian.Uint64(sum[:8]) >> 1)
}

// RateInput is the input to rate resolution.
type RateInput struct {
	Order OrderKey
	// Layer starts at 1.
	Layer int
	Agent Agent
	Rules Rules
}

// RateResolver decides which rate level `layer` should use.
//
// This is the core extension point of the rules layer. The default
// implementation resolves in the order "per-agent override > global rules",
// which covers the vast majority of cases; implement your own only for
// requirements that structured configuration cannot express, such as "float
// by product category" or "tier by agent rank" (see ADR-002).
type RateResolver interface {
	ResolveRate(ctx context.Context, in RateInput) (Rate, error)
}

// EligibilityInput is everything an EligibilityChecker needs to decide whether
// one agent may earn from one order.
type EligibilityInput struct {
	Agent Agent
	Order OrderKey
	// BuyerUserID is the buyer of the order.
	//
	// It is part of the input because self-purchase is the single most common
	// abuse of a distribution programme: a agent buys through their own
	// link and pays themselves a commission. An eligibility hook that cannot
	// see the buyer cannot express that rule at all.
	BuyerUserID int64
}

// EligibilityChecker decides whether an agent is eligible for a commission
// on one order.
//
// The default implementation requires the agent to be in the AgentActive
// state and the buyer not to be the agent itself. Implementations must return
// (false, reason) rather than an error to express "not eligible": that is a
// business conclusion, not a program failure.
type EligibilityChecker interface {
	Eligible(ctx context.Context, in EligibilityInput) (bool, string)
}

// defaultRateResolver is the default rate resolver.
type defaultRateResolver struct{}

func (defaultRateResolver) ResolveRate(_ context.Context, in RateInput) (Rate, error) {
	if in.Agent.RateOverrideBP != nil {
		return *in.Agent.RateOverrideBP, nil
	}
	idx := in.Layer - 1
	if idx < 0 || idx >= len(in.Rules.RateBP) {
		return 0, fieldErrf("layer", "no rate configured for layer %d (levels=%d)",
			in.Layer, in.Rules.Levels)
	}
	return in.Rules.RateBP[idx], nil
}

// defaultEligibility is the default eligibility rule.
//
// It enforces two things, both of them conservative defaults rather than
// configurable switches (see ADR-022: no configuration without a real
// implementation behind it):
//
//  1. The agent must be active.
//  2. The buyer must not be the agent. Self-purchase is the classic way to
//     launder a discount into commission, so it is off by default. Integrations
//     that genuinely want it supply their own EligibilityChecker.
type defaultEligibility struct{}

func (defaultEligibility) Eligible(_ context.Context, in EligibilityInput) (bool, string) {
	if in.Agent.Status != AgentActive {
		return false, "agent status is " + in.Agent.Status.String()
	}
	if in.BuyerUserID != 0 && in.Agent.Key.UserID == in.BuyerUserID {
		return false, "self-purchase is not eligible by default"
	}
	return true, ""
}
