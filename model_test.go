package distledger

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

// This file covers pure functions and type behavior: enum string
// representations, validation branches, Error/Unwrap on the error types, and
// every failure face of rules validation.
//
// These look trivial, but they carry error messages and logs: an enum String()
// that names the wrong state makes the state seen while debugging disagree
// with reality, and debugging is the worst time to be misled.

func TestEnumStrings(t *testing.T) {
	joinTypes := map[JoinType]string{JoinFree: "free", JoinReviewed: "reviewed", JoinPaid: "paid"}
	for v, want := range joinTypes {
		if got := v.String(); got != want {
			t.Errorf("JoinType(%d).String() = %q, want %q", v, got, want)
		}
		if !v.Valid() {
			t.Errorf("JoinType(%d) should be valid", v)
		}
	}
	if JoinType(99).Valid() {
		t.Error("JoinType(99) must be invalid")
	}
	if got := JoinType(99).String(); !strings.Contains(got, "99") {
		t.Errorf("unknown JoinType should include its numeric value, got %q", got)
	}

	statuses := map[AgentStatus]string{
		AgentInactive: "inactive", AgentActive: "active", AgentDisabled: "disabled",
	}
	for v, want := range statuses {
		if got := v.String(); got != want {
			t.Errorf("AgentStatus(%d).String() = %q, want %q", v, got, want)
		}
	}
	if AgentStatus(9).Valid() {
		t.Error("AgentStatus(9) must be invalid")
	}

	sources := map[BindSource]string{
		SourceLink: "link", SourceInviteCode: "invite_code",
		SourceManual: "manual", SourceSalesperson: "salesperson",
	}
	for v, want := range sources {
		if got := v.String(); got != want {
			t.Errorf("BindSource(%d).String() = %q, want %q", v, got, want)
		}
		if !v.Valid() {
			t.Errorf("BindSource(%d) should be valid", v)
		}
	}
	if BindSource(0).Valid() || BindSource(9).Valid() {
		t.Error("BindSource 0 and 9 must be invalid")
	}

	states := map[CommissionState]string{
		CommissionPending: "pending", CommissionSettled: "settled",
		CommissionWithdrawn: "withdrawn", CommissionReversed: "reversed",
		CommissionFrozen: "frozen", CommissionVoid: "void",
	}
	for v, want := range states {
		if got := v.String(); got != want {
			t.Errorf("CommissionState(%d).String() = %q, want %q", v, got, want)
		}
		if !v.Valid() {
			t.Errorf("CommissionState(%d) should be valid", v)
		}
	}
	if CommissionState(42).Valid() {
		t.Error("CommissionState(42) must be invalid")
	}

	bizTypes := map[LedgerBizType]string{
		LedgerAccrue: "accrue", LedgerSettle: "settle", LedgerReverse: "reverse",
		LedgerWithdrawHold: "withdraw_hold", LedgerWithdrawPaid: "withdraw_paid",
		LedgerWithdrawRefund: "withdraw_refund", LedgerManualAdjust: "manual_adjust",
	}
	for v, want := range bizTypes {
		if got := v.String(); got != want {
			t.Errorf("LedgerBizType(%d).String() = %q, want %q", v, got, want)
		}
		if !v.Valid() {
			t.Errorf("LedgerBizType(%d) should be valid", v)
		}
	}
	if LedgerBizType(0).Valid() || LedgerBizType(200).Valid() {
		t.Error("LedgerBizType 0 and 200 must be invalid")
	}

	skips := map[SkipCode]string{
		SkipZeroAmount: "zero_amount", SkipIneligible: "ineligible",
		SkipNoParent: "no_parent", SkipCapExhausted: "cap_exhausted",
		SkipAgentMissing: "agent_missing",
	}
	for v, want := range skips {
		if got := v.String(); got != want {
			t.Errorf("SkipCode(%d).String() = %q, want %q", v, got, want)
		}
	}
	if got := SkipCode(200).String(); got != "unknown" {
		t.Errorf("unknown SkipCode = %q, want \"unknown\"", got)
	}

	if got := Rounding(200).String(); !strings.Contains(got, "200") {
		t.Errorf("unknown Rounding should include its numeric value, got %q", got)
	}
	if Rounding(200).Valid() {
		t.Error("Rounding(200) must be invalid")
	}
	for _, r := range []Rounding{RoundDown, RoundHalfUp, RoundUp} {
		if !r.Valid() {
			t.Errorf("Rounding(%d) should be valid", r)
		}
	}
	if RoundDown.String() != "down" || RoundHalfUp.String() != "half-up" || RoundUp.String() != "up" {
		t.Error("rounding mode names changed unexpectedly")
	}
}

func TestMoneyPredicates(t *testing.T) {
	tests := []struct {
		m                        Money
		zero, positive, negative bool
		sign                     int
	}{
		{0, true, false, false, 0},
		{1, false, true, false, 1},
		{-1, false, false, true, -1},
	}
	for _, tc := range tests {
		if tc.m.IsZero() != tc.zero || tc.m.IsPositive() != tc.positive || tc.m.IsNegative() != tc.negative {
			t.Errorf("predicates wrong for %d", tc.m)
		}
		if tc.m.Sign() != tc.sign {
			t.Errorf("Sign(%d) = %d, want %d", tc.m, tc.m.Sign(), tc.sign)
		}
	}

	if Money(1).Cmp(Money(1)) != 0 || Money(1).Cmp(Money(2)) != -1 || Money(2).Cmp(Money(1)) != 1 {
		t.Error("Cmp is inconsistent")
	}
}

func TestAccountSettleable(t *testing.T) {
	ok := Account{Frozen: 1, Available: 2, Withdrawing: 3, Withdrawn: 4}
	if !ok.Settleable() {
		t.Error("a healthy account must be settleable")
	}
	for _, bad := range []Account{
		{Frozen: -1}, {Available: -1}, {Withdrawing: -1}, {Withdrawn: -1},
	} {
		if bad.Settleable() {
			t.Errorf("account with a negative bucket must not be settleable: %+v", bad)
		}
	}
}

func TestLedgerEntryNetDelta(t *testing.T) {
	e := LedgerEntry{DeltaFrozen: -100, DeltaAvailable: 100}
	if got := e.NetDelta(); got != 0 {
		t.Fatalf("NetDelta = %d, want 0 for a pure bucket move", got)
	}
	e = LedgerEntry{DeltaFrozen: 100}
	if got := e.NetDelta(); got != 100 {
		t.Fatalf("NetDelta = %d, want 100", got)
	}
}

func TestCommissionDueAt(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c := Commission{}
	if c.DueAt(now) {
		t.Error("a commission without AvailableAt (order not received) is never due")
	}
	c.AvailableAt = now.Add(time.Hour)
	if c.DueAt(now) {
		t.Error("a commission must not be due before its AvailableAt")
	}
	if !c.DueAt(now.Add(time.Hour)) {
		t.Error("a commission is due exactly at its AvailableAt")
	}
}

func TestBindingEffective(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	b := Binding{Active: true}
	if !b.Effective(now) {
		t.Error("an active binding without expiry is always effective")
	}
	b.Active = false
	if b.Effective(now) {
		t.Error("an inactive binding is never effective")
	}
	b.Active = true
	b.ExpireAt = now.Add(-time.Second)
	if b.Effective(now) {
		t.Error("an expired binding is not effective")
	}
	b.ExpireAt = now.Add(time.Second)
	if !b.Effective(now) {
		t.Error("a binding not yet expired is effective")
	}
}

func TestAccrueResultAccruedAmount(t *testing.T) {
	res := AccrueResult{Commissions: []Commission{
		{Amount: 100}, {Amount: 250},
	}}
	got, err := res.AccruedAmount()
	if err != nil || got != 350 {
		t.Fatalf("AccruedAmount = %d, %v; want 350", got, err)
	}
	got, err = (AccrueResult{}).AccruedAmount()
	if err != nil || got != 0 {
		t.Fatalf("empty result = %d, %v; want 0", got, err)
	}
}

func TestRulesValidation(t *testing.T) {
	if err := DefaultRules().Validate(); err != nil {
		t.Fatalf("default rules must be valid: %v", err)
	}

	cases := map[string]Rules{
		"levels zero":         {Levels: 0, RateBP: nil, MaxAllocatableBP: MaxRateBP},
		"levels too deep":     {Levels: MaxLevels + 1, RateBP: []Rate{1, 1, 1, 1}, MaxAllocatableBP: MaxRateBP},
		"rate count mismatch": {Levels: 2, RateBP: []Rate{1}, MaxAllocatableBP: MaxRateBP},
		"rate out of range":   {Levels: 1, RateBP: []Rate{-1}, MaxAllocatableBP: MaxRateBP},
		"rate above 100":      {Levels: 1, RateBP: []Rate{MaxRateBP + 1}, MaxAllocatableBP: MaxRateBP},
		"sum exceeds cap":     {Levels: 2, RateBP: []Rate{5000, 5000}, MaxAllocatableBP: 9999},
		"cap zero":            {Levels: 1, RateBP: []Rate{0}, MaxAllocatableBP: 0},
		"negative freeze":     {Levels: 1, RateBP: []Rate{0}, MaxAllocatableBP: MaxRateBP, FreezeDays: -1},
		"freeze too long":     {Levels: 1, RateBP: []Rate{0}, MaxAllocatableBP: MaxRateBP, FreezeDays: MaxFreezeDays + 1},
		"bind expire too long": {Levels: 1, RateBP: []Rate{0}, MaxAllocatableBP: MaxRateBP,
			BindExpireDays: MaxBindExpireDays + 1},
		"bind expire negative": {Levels: 1, RateBP: []Rate{0}, MaxAllocatableBP: MaxRateBP,
			BindExpireDays: -1},
	}
	for name, rules := range cases {
		t.Run(name, func(t *testing.T) {
			if err := rules.Validate(); err == nil {
				t.Fatalf("rules %+v should have failed validation", rules)
			} else if !errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("validation error should wrap ErrInvalidArgument, got %v", err)
			}
		})
	}
}

func TestRulesNormalize(t *testing.T) {
	got := Rules{}.Normalize()
	if got.Levels != 2 {
		t.Errorf("Levels = %d, want 2", got.Levels)
	}
	if len(got.RateBP) != 2 {
		t.Errorf("RateBP has %d entries, want 2", len(got.RateBP))
	}
	if got.MaxAllocatableBP != MaxRateBP {
		t.Errorf("MaxAllocatableBP = %d, want %d", got.MaxAllocatableBP, MaxRateBP)
	}

	// A non-zero rate count that disagrees with Levels must **not** be padded
	// with zeros silently: that would turn "one level was left unconfigured"
	// into "that level pays nothing", which raises no alarm at all.
	bad := Rules{Levels: 3, RateBP: []Rate{500}}.Normalize()
	if err := bad.Validate(); err == nil {
		t.Fatal("mismatched rate count must still fail validation after Normalize")
	}
}

func TestRulesDescribeAndVersion(t *testing.T) {
	a := Rules{Levels: 2, RateBP: []Rate{500, 200}, FreezeDays: 7, MaxAllocatableBP: MaxRateBP}
	b := a

	if a.Describe() != b.Describe() {
		t.Fatal("identical rules must produce identical descriptions")
	}
	if a.Version() != b.Version() {
		t.Fatal("identical rules must produce identical versions")
	}
	if a.Version() < 0 {
		t.Fatal("version must be non-negative for readability in logs and SQL")
	}

	changed := a
	changed.FreezeDays = 8
	if changed.Version() == a.Version() {
		t.Fatal("changing any rule field must change the version")
	}

	desc := a.Describe()
	for _, want := range []string{`"levels":2`, `"rate_bp":[500,200]`, `"freeze_days":7`, `"rounding":"down"`} {
		if !strings.Contains(desc, want) {
			t.Errorf("description %s is missing %s", desc, want)
		}
	}
}

func TestDefaultRateResolver(t *testing.T) {
	r := defaultRateResolver{}
	rules := Rules{Levels: 2, RateBP: []Rate{500, 200}, MaxAllocatableBP: MaxRateBP}

	got, err := r.ResolveRate(context.Background(), RateInput{Layer: 2, Rules: rules})
	if err != nil || got != 200 {
		t.Fatalf("layer 2 rate = %d, %v; want 200", got, err)
	}

	override := Rate(900)
	got, err = r.ResolveRate(context.Background(), RateInput{
		Layer: 1, Rules: rules, Agent: Agent{RateOverrideBP: &override},
	})
	if err != nil || got != 900 {
		t.Fatalf("agent override = %d, %v; want 900", got, err)
	}

	if _, err := r.ResolveRate(context.Background(), RateInput{Layer: 3, Rules: rules}); err == nil {
		t.Fatal("a layer without a configured rate must be an error")
	}
	if _, err := r.ResolveRate(context.Background(), RateInput{Layer: 0, Rules: rules}); err == nil {
		t.Fatal("layer 0 must be rejected")
	}
}

func TestDefaultEligibility(t *testing.T) {
	e := defaultEligibility{}
	if ok, _ := e.Eligible(context.Background(), EligibilityInput{Agent: Agent{Status: AgentActive}}); !ok {
		t.Error("active agent must be eligible")
	}
	for _, s := range []AgentStatus{AgentInactive, AgentDisabled} {
		ok, reason := e.Eligible(context.Background(), EligibilityInput{Agent: Agent{Status: s}})
		if ok {
			t.Errorf("%s agent must not be eligible", s)
		}
		if reason == "" {
			t.Error("rejection must carry a human-readable reason")
		}
	}
}

func TestOrderPaidEventValidation(t *testing.T) {
	valid := OrderPaidEvent{
		TenantID: 0, OrderID: "ORD-1", BuyerUserID: 1,
		PaidAmount: 100, PaidAt: time.Now(),
	}
	if err := valid.validate(); err != nil {
		t.Fatalf("valid event rejected: %v", err)
	}

	bad := map[string]func(*OrderPaidEvent){
		"empty order id":    func(e *OrderPaidEvent) { e.OrderID = "" },
		"order id too long": func(e *OrderPaidEvent) { e.OrderID = strings.Repeat("x", MaxOrderIDLen+1) },
		"negative tenant":   func(e *OrderPaidEvent) { e.TenantID = -1 },
		"zero buyer":        func(e *OrderPaidEvent) { e.BuyerUserID = 0 },
		"negative amount":   func(e *OrderPaidEvent) { e.PaidAmount = -1 },
		"zero amount":       func(e *OrderPaidEvent) { e.PaidAmount = 0 },
		"missing paid at":   func(e *OrderPaidEvent) { e.PaidAt = time.Time{} },
		"bad idem key":      func(e *OrderPaidEvent) { e.IdemKey = "bad key!" },
		"empty item id":     func(e *OrderPaidEvent) { e.Items = []OrderItem{{ItemID: "", Amount: 1}} },
		"item id too long": func(e *OrderPaidEvent) {
			e.Items = []OrderItem{{ItemID: strings.Repeat("x", MaxOrderItemIDLen+1), Amount: 1}}
		},
		"negative item": func(e *OrderPaidEvent) { e.Items = []OrderItem{{ItemID: "a", Amount: -1}} },
		"duplicate item": func(e *OrderPaidEvent) {
			e.Items = []OrderItem{{ItemID: "a", Amount: 1}, {ItemID: "a", Amount: 1}}
		},
		"items exceed paid": func(e *OrderPaidEvent) {
			e.Items = []OrderItem{{ItemID: "a", Amount: 60}, {ItemID: "b", Amount: 60}}
		},
	}
	for name, mutate := range bad {
		t.Run(name, func(t *testing.T) {
			ev := valid
			mutate(&ev)
			if err := ev.validate(); err == nil {
				t.Fatalf("event should have been rejected: %+v", ev)
			} else if !errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("error should wrap ErrInvalidArgument, got %v", err)
			}
		})
	}

	// Items summing to exactly the paid amount are legal.
	edge := valid
	edge.PaidAmount = 100
	edge.Items = []OrderItem{{ItemID: "a", Amount: 40}, {ItemID: "b", Amount: 60}}
	if err := edge.validate(); err != nil {
		t.Fatalf("items summing exactly to the paid amount must be accepted: %v", err)
	}
}

func TestOrderReceivedEventValidation(t *testing.T) {
	if err := (OrderReceivedEvent{TenantID: 0, OrderID: "ORD-1"}).validate(); err != nil {
		t.Fatalf("valid event rejected: %v", err)
	}
	if err := (OrderReceivedEvent{TenantID: 0, OrderID: ""}).validate(); err == nil {
		t.Fatal("empty order id must be rejected")
	}
}

func TestKeyValidation(t *testing.T) {
	if err := (UserKey{TenantID: 0, UserID: 1}).Validate(); err != nil {
		t.Fatalf("valid user key rejected: %v", err)
	}
	if err := (UserKey{TenantID: -1, UserID: 1}).Validate(); err == nil {
		t.Fatal("negative tenant must be rejected")
	}
	if err := (UserKey{TenantID: 0, UserID: 0}).Validate(); err == nil {
		t.Fatal("zero user id must be rejected")
	}

	if err := (OrderKey{TenantID: 0, OrderID: "ORD"}).Validate(); err != nil {
		t.Fatalf("valid order key rejected: %v", err)
	}
	if err := (OrderKey{TenantID: -1, OrderID: "ORD"}).Validate(); err == nil {
		t.Fatal("negative tenant must be rejected")
	}
	if err := (OrderKey{TenantID: 0, OrderID: ""}).Validate(); err == nil {
		t.Fatal("empty order id must be rejected")
	}
	if err := (OrderKey{TenantID: 0, OrderID: strings.Repeat("x", MaxOrderIDLen+1)}).Validate(); err == nil {
		t.Fatal("over-long order id must be rejected")
	}

	if got := (UserKey{TenantID: 3, UserID: 7}).String(); got != "t3/u7" {
		t.Errorf("UserKey.String() = %q", got)
	}
	if got := (OrderKey{TenantID: 3, OrderID: "ORD"}).String(); got != "t3/ORD" {
		t.Errorf("OrderKey.String() = %q", got)
	}
}

func TestPageLimitNormalization(t *testing.T) {
	cases := map[int]int{
		0:                DefaultPageLimit,
		-5:               DefaultPageLimit,
		1:                1,
		MaxPageLimit:     MaxPageLimit,
		MaxPageLimit + 1: MaxPageLimit,
		1 << 30:          MaxPageLimit,
	}
	for in, want := range cases {
		if got := (Page{Limit: in}).LimitOrDefault(); got != want {
			t.Errorf("Page{Limit:%d}.LimitOrDefault() = %d, want %d", in, got, want)
		}
		if got := (AccountPage{Limit: in}).LimitOrDefault(); got != want {
			t.Errorf("AccountPage{Limit:%d}.LimitOrDefault() = %d, want %d", in, got, want)
		}
	}
}

func TestErrorTypes(t *testing.T) {
	fe := &FieldError{Field: "order_id", Reason: "must not be empty"}
	if !strings.Contains(fe.Error(), "order_id") || !strings.Contains(fe.Error(), "must not be empty") {
		t.Errorf("FieldError message is unhelpful: %s", fe.Error())
	}
	if !errors.Is(fe, ErrInvalidArgument) {
		t.Error("FieldError must unwrap to ErrInvalidArgument")
	}

	te := &TransitionError{Kind: "commission", From: "withdrawn", To: "pending"}
	if !strings.Contains(te.Error(), "withdrawn -> pending") {
		t.Errorf("TransitionError message is unhelpful: %s", te.Error())
	}
	if !errors.Is(te, ErrIllegalTransition) {
		t.Error("TransitionError must unwrap to ErrIllegalTransition")
	}

	ce := &ConflictError{Kind: "agent", ID: "t0/u1", Expected: 1, Actual: 2}
	if !strings.Contains(ce.Error(), "t0/u1") {
		t.Errorf("ConflictError message is unhelpful: %s", ce.Error())
	}
	if !errors.Is(ce, ErrConflict) {
		t.Error("ConflictError must unwrap to ErrConflict")
	}
}

func TestSystemClockIsUTC(t *testing.T) {
	now := SystemClock().Now()
	if now.Location() != time.UTC {
		t.Fatalf("SystemClock must return UTC, got %s", now.Location())
	}
}

func TestManualClock(t *testing.T) {
	start := time.Date(2026, 5, 1, 12, 0, 0, 0, time.FixedZone("CST", 8*3600))
	c := NewManualClock(start)

	if c.Now().Location() != time.UTC {
		t.Fatalf("ManualClock must normalize to UTC, got %s", c.Now().Location())
	}
	if !c.Now().Equal(start.UTC()) {
		t.Fatalf("ManualClock lost the instant: %s vs %s", c.Now(), start.UTC())
	}

	c.Advance(2 * time.Hour)
	if got := c.Now(); !got.Equal(start.UTC().Add(2 * time.Hour)) {
		t.Fatalf("Advance did not move forward: %s", got)
	}

	c.Advance(-time.Hour)
	if got := c.Now(); !got.Equal(start.UTC().Add(time.Hour)) {
		t.Fatalf("Advance did not move backward: %s", got)
	}

	target := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	c.Set(target)
	if !c.Now().Equal(target) {
		t.Fatalf("Set did not take effect: %s", c.Now())
	}
}

// TestIdemKeyLengthsAreBounded confirms the length ceiling is really enforced
// in validate, rather than merely written down in a comment on the constant.
func TestIdemKeyLengthsAreBounded(t *testing.T) {
	ev := OrderPaidEvent{
		TenantID: 0, OrderID: "ORD-1", BuyerUserID: 1,
		PaidAmount: 100, PaidAt: time.Now(),
		IdemKey: strings.Repeat("x", MaxIdemKeyLen+1),
	}
	if err := ev.validate(); err == nil {
		t.Fatal("over-long idempotency key must be rejected")
	}

	ev.IdemKey = strings.Repeat("x", MaxIdemKeyLen)
	if err := ev.validate(); err != nil {
		t.Fatalf("key at the length limit must be accepted: %v", err)
	}
}

// TestEveryAccountMoneyFieldIsClassified is the structural guard against the
// failure mode that made TotalEarned invisible to every invariant for a whole
// release: the checks named their fields by hand, so a new money field silently
// fell outside them.
//
// This test enumerates Account's Money fields through reflection and requires
// each one to be either a balance bucket or an explicitly exempt total. Adding a
// field therefore fails the build until someone decides which it is.
func TestEveryAccountMoneyFieldIsClassified(t *testing.T) {
	// Totals that are deliberately not buckets, with the reason they are safe.
	exemptTotals := map[string]string{
		"TotalEarned":   "gross accrual total, reconciled against commission records by invariant I3",
		"TotalReversed": "gross reversal total, reconciled against reversal records by invariant I3",
	}

	buckets := make(map[string]Bucket)
	for _, b := range AllBuckets() {
		buckets[b.String()] = b
	}

	typ := reflect.TypeOf(Account{})
	moneyType := reflect.TypeOf(Money(0))
	var classified int
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		if field.Type != moneyType {
			continue
		}
		classified++
		if _, ok := buckets[field.Name]; ok {
			continue
		}
		if reason, ok := exemptTotals[field.Name]; ok {
			if reason == "" {
				t.Errorf("Account.%s is exempted from the bucket checks without a reason", field.Name)
			}
			continue
		}
		t.Errorf("Account.%s is a Money field that is neither a bucket nor an exempt total.\n"+
			"Either add it as a Bucket case so I1 and Settleable check it, or list it in\n"+
			"exemptTotals with the invariant that covers it.", field.Name)
	}

	if classified == 0 {
		t.Fatal("reflection found no Money fields on Account; the guard is not checking anything")
	}

	// Every bucket name must correspond to a real field, otherwise Bucket.Value
	// and Bucket.Set would silently read and write nothing.
	for _, b := range AllBuckets() {
		if _, ok := typ.FieldByName(b.String()); !ok {
			t.Errorf("bucket %s has no matching Account field", b)
		}
	}
}

// TestBucketAccessorsRoundTrip pins that Value/Set/Delta/After are consistent
// with each other for every bucket.
func TestBucketAccessorsRoundTrip(t *testing.T) {
	buckets := AllBuckets()
	if len(buckets) == 0 {
		t.Fatal("no buckets defined")
	}
	for _, b := range buckets {
		if !b.Valid() {
			t.Errorf("bucket %d is not valid", b)
		}

		var a Account
		b.Set(&a, 1234)
		if got := b.Value(a); got != 1234 {
			t.Errorf("%s: Set then Value = %d, want 1234", b, got)
		}
		for _, other := range buckets {
			if other == b {
				continue
			}
			if got := other.Value(a); got != 0 {
				t.Errorf("setting %s also changed %s to %d", b, other, got)
			}
		}

		e := LedgerEntry{}
		b.SetAfter(&e, 99)
		b.AddDelta(&e, -7)
		if got := b.After(e); got != 99 {
			t.Errorf("%s: After = %d, want 99", b, got)
		}
		if got := b.Delta(e); got != -7 {
			t.Errorf("%s: Delta = %d, want -7", b, got)
		}
	}

	if Bucket(200).Valid() {
		t.Error("an out-of-range bucket must not be valid")
	}
	if got := Bucket(200).String(); got == "" {
		t.Error("an unknown bucket must still render something for diagnostics")
	}
}

// TestAllBucketsReturnsCopy keeps callers from redefining the bucket set.
func TestAllBucketsReturnsCopy(t *testing.T) {
	a := AllBuckets()
	a[0] = Bucket(200)
	if AllBuckets()[0] == Bucket(200) {
		t.Fatal("AllBuckets leaked its internal slice")
	}
}
