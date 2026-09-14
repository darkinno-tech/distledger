package distledger

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// This file holds the payout side of the engine: asking for money, reviewing
// the request, and paying it.
//
// Two properties shape everything here.
//
// First, money moves at application rather than at approval. The reserved
// bucket exists so that an agent cannot request a withdrawal and then spend the
// same balance again while it waits for review.
//
// Second, no external call happens inside a transaction. The storage port
// permits retrying a transaction by re-running its closure, so a payout
// attempted inside one would be attempted again on every retry — and a payout
// is the one action here that cannot be undone. The channel is therefore called
// between two transactions, and both the channel and the withdrawal carry the
// same idempotency key so that the window between them is survivable.

// PayoutRequest is one payout handed to a channel.
type PayoutRequest struct {
	// IdemKey makes the call idempotent. It is derived from the withdrawal, so
	// the same withdrawal always presents the same key.
	//
	// It has to exist: the payout happens outside any transaction, so a crash
	// between the channel succeeding and the outcome being recorded leads to
	// the withdrawal being paid again on recovery. A channel that does not
	// honour the key cannot be used safely, and that is a property of the
	// channel rather than of this library.
	IdemKey string
	// TenantID and UserID identify whose money it is.
	TenantID int64
	UserID   int64
	// Amount is what the destination receives: the withdrawal amount less the
	// fee. It is deliberately not the gross, because the fee never leaves the
	// platform.
	Amount Money
	// AccountInfo is the masked destination the caller supplied.
	AccountInfo string
	// WithdrawalID is the library's own id, for a channel that wants to record
	// a reference back.
	WithdrawalID int64
}

// PayoutResult is a channel's answer.
//
// A channel reports success or failure; it does not report "maybe". Channels
// that settle asynchronously are out of scope for v0.5 and are documented as
// such, because representing a payout whose outcome is unknown needs a state
// this library does not have yet — and inventing one without the reconciliation
// to go with it would be worse than not supporting it.
type PayoutResult struct {
	// Paid reports that the money has left.
	Paid bool
	// InvoiceNo is the provider's reference, recorded on the withdrawal.
	InvoiceNo string
	// FailReason explains a failure and is recorded so an operator can decide
	// whether to retry.
	FailReason string
}

// PayoutChannel moves money out of the platform.
//
// Implementing it is how this library reaches a payment provider without
// depending on one: the library has no payment qualification, and a channel
// implementation is the caller's.
type PayoutChannel interface {
	// Name identifies the channel, recorded on each withdrawal.
	Name() string

	// Pay attempts one payout.
	//
	// Implementations **must** be idempotent on req.IdemKey: paying twice for
	// one key is the one mistake here that no reconciliation can repair.
	Pay(ctx context.Context, req PayoutRequest) (PayoutResult, error)
}

// ManualChannel is a PayoutChannel for platforms that pay by hand.
//
// It does not move money. It records that a human moved it, which is the
// honest representation of "the operator made a bank transfer and typed the
// reference in". Using it means the payout gate is a person, and that is a
// choice rather than a limitation: ADR-010's principle is that money should not
// leave without somebody deciding it should.
type ManualChannel struct{}

// Name implements PayoutChannel.
func (ManualChannel) Name() string { return "manual" }

// Pay implements PayoutChannel.
//
// It succeeds immediately with an empty invoice number, because the reference
// is supplied by the operator afterwards through MarkWithdrawPaid. It is
// idempotent trivially: it has no side effect to repeat.
func (ManualChannel) Pay(context.Context, PayoutRequest) (PayoutResult, error) {
	return PayoutResult{Paid: true}, nil
}

// MockChannel is a PayoutChannel for tests.
//
// It fails when told to, so that the failure path — which is the path that
// strands money if it is wrong — can be exercised without a payment provider.
type MockChannel struct {
	// Fail makes every payout fail.
	Fail bool
	// Invoice is returned on success; a default is used when it is empty.
	Invoice string
	// Calls records every request, so a test can assert that a retry did not
	// reach the channel twice.
	Calls []PayoutRequest
}

// Name implements PayoutChannel.
func (*MockChannel) Name() string { return "mock" }

// Pay implements PayoutChannel.
func (c *MockChannel) Pay(_ context.Context, req PayoutRequest) (PayoutResult, error) {
	c.Calls = append(c.Calls, req)
	if c.Fail {
		return PayoutResult{FailReason: "mock channel configured to fail"}, nil
	}
	invoice := c.Invoice
	if invoice == "" {
		invoice = "MOCK-" + req.IdemKey
	}
	return PayoutResult{Paid: true, InvoiceNo: invoice}, nil
}

// WithdrawRequest is a request to pay an agent.
type WithdrawRequest struct {
	TenantID int64
	UserID   int64
	// Amount is the gross amount to take out of the withdrawable balance.
	Amount Money
	// IdemKey is the caller's identifier for this request. Required, for the
	// same reason as a refund's: a retry and a genuine second request of the
	// same size are indistinguishable in the data without it.
	IdemKey string
	// Channel names the payout route. Empty means "manual".
	Channel string
	// AccountInfo is where the money goes, already masked by the caller. The
	// library never sees an unmasked account number and stores what it is given.
	AccountInfo string
}

// WithdrawDecision is an approval or a rejection.
type WithdrawDecision struct {
	TenantID     int64
	WithdrawalID int64
	// Operator records who decided, for audit.
	Operator string
	// Reason is required for a rejection and recorded in the ledger remark.
	Reason string
}

// PayoutOutcome is the result of asking a channel to pay, or of recording that
// a person did.
type PayoutOutcome struct {
	TenantID     int64
	WithdrawalID int64
	Operator     string
	// InvoiceNo is the provider's reference, when the caller has one.
	InvoiceNo string
	// FailReason, when set, records a failed attempt instead of a success.
	FailReason string
}

// RequestWithdraw asks for money and reserves it.
//
// # Why the money moves here
//
// Reserving at application is what stops an agent from requesting a withdrawal
// and then spending the same balance again before anyone reviews it. The
// alternative — reserving at approval — leaves the balance spendable during the
// review window, which is exactly when a determined caller would spend it.
//
// # Idempotency
//
// A repeated IdemKey returns the original withdrawal with alreadyExisted true
// rather than reserving the money twice. That is the behavior a retry needs,
// and it is deliberately not an error: a caller retrying after a timeout has
// done nothing wrong.
func (l *Ledger) RequestWithdraw(ctx context.Context, req WithdrawRequest) (Withdrawal, bool, error) {
	if req.TenantID < 0 {
		return Withdrawal{}, false, fieldErrf("tenant_id", "must be >= 0, got %d", req.TenantID)
	}
	if req.UserID <= 0 {
		return Withdrawal{}, false, fieldErrf("user_id", "must be > 0, got %d", req.UserID)
	}
	if req.Amount <= 0 {
		return Withdrawal{}, false, fieldErrf("amount", "must be positive, got %s", req.Amount)
	}
	if err := validateIdemKey(req.IdemKey); err != nil {
		return Withdrawal{}, false, err
	}
	channel := req.Channel
	if channel == "" {
		channel = ManualChannel{}.Name()
	}
	if len(channel) > maxChannelLen {
		return Withdrawal{}, false, fieldErrf("channel",
			"must be at most %d bytes, got %d", maxChannelLen, len(channel))
	}
	if req.Amount < l.rules.MinWithdraw {
		return Withdrawal{}, false, fieldErrf("amount",
			"is below the minimum withdrawal of %s, got %s",
			l.rules.MinWithdraw, req.Amount)
	}

	fee, err := withdrawFee(req.Amount, l.rules.FeeRateBP, l.rules.Rounding)
	if err != nil {
		return Withdrawal{}, false, err
	}

	at := l.clock.Now()
	key := UserKey{TenantID: req.TenantID, UserID: req.UserID}
	idem := withdrawIdemKey(req.TenantID, req.IdemKey)

	var (
		out     Withdrawal
		existed bool
	)
	err = l.store.Update(ctx, func(ctx context.Context, tx Tx) error {
		out, existed = Withdrawal{}, false

		// Replays are resolved inside the transaction so that the answer is
		// read from the same snapshot that would have written the new record.
		if prior, err := tx.WithdrawalByIdemKey(ctx, req.TenantID, idem); err == nil {
			out, existed = prior, true
			return nil
		} else if !errors.Is(err, ErrNotFound) {
			return err
		}

		w := Withdrawal{
			Key: key, IdemKey: idem,
			Amount: req.Amount, Fee: fee, RealAmount: req.Amount - fee,
			Channel: channel, AccountInfo: req.AccountInfo,
			State:     WithdrawalApplied,
			AppliedAt: at,
		}
		saved, err := tx.AppendWithdrawal(ctx, w)
		if err != nil {
			if errors.Is(err, ErrDuplicate) {
				// Lost a race with a concurrent identical request: read the
				// winner back rather than failing the caller.
				prior, readErr := tx.WithdrawalByIdemKey(ctx, req.TenantID, idem)
				if readErr != nil {
					return readErr
				}
				out, existed = prior, true
				return nil
			}
			return err
		}

		// Reserve: Available -> Withdrawing. The guard is a predicate on the
		// same statement, so an overdraft cannot slip through between a check
		// and the write.
		acct, err := tx.IncrementAccount(ctx, AccountDelta{
			Key:                key,
			Available:          -req.Amount,
			Withdrawing:        req.Amount,
			RequireNonNegative: true,
			At:                 at,
		})
		if err != nil {
			// The transaction rolls back, so the withdrawal record written
			// above leaves nothing behind.
			return err
		}

		entry := newLedgerEntry(
			key, LedgerWithdrawHold, strconv.FormatInt(saved.ID, 10),
			"withdraw request "+strconv.FormatInt(saved.ID, 10)+
				" hold "+req.Amount.String()+" fee "+fee.String(),
			at, acct, BucketAvailable, -req.Amount,
		)
		BucketWithdrawing.AddDelta(&entry, req.Amount)
		if _, err := tx.AppendLedger(ctx, entry); err != nil {
			return err
		}
		out, existed = saved, false
		return nil
	})
	if err != nil {
		return Withdrawal{}, false, err
	}

	if !existed {
		l.log.InfoContext(ctx, "withdrawal requested",
			"withdrawal_id", out.ID, "user_id", req.UserID,
			"amount", out.Amount.String(), "fee", out.Fee.String(), "channel", out.Channel)
	}
	return out, existed, nil
}

// ApproveWithdraw clears a request for payout.
//
// The money does not move: it was already reserved at application. Approval
// only decides whether a payout may be attempted, which is why it is safe for a
// human to do slowly.
func (l *Ledger) ApproveWithdraw(ctx context.Context, req WithdrawDecision) (Withdrawal, error) {
	at := l.clock.Now()
	var out Withdrawal
	err := l.store.Update(ctx, func(ctx context.Context, tx Tx) error {
		cur, err := l.loadScopedWithdrawal(ctx, tx, req.TenantID, req.WithdrawalID)
		if err != nil {
			return err
		}
		out, err = tx.TransitionWithdrawal(ctx, cur.ID,
			WithdrawalApplied, WithdrawalApproved, cur.Version,
			WithdrawalTransition{Operator: req.Operator, AuditedAt: at})
		return err
	})
	if err != nil {
		return Withdrawal{}, err
	}
	l.log.InfoContext(ctx, "withdrawal approved",
		"withdrawal_id", out.ID, "operator", req.Operator)
	return out, nil
}

// RejectWithdraw refuses a request and returns the reserved money.
func (l *Ledger) RejectWithdraw(ctx context.Context, req WithdrawDecision) (Withdrawal, error) {
	if err := validateReason(req.Reason); err != nil {
		return Withdrawal{}, err
	}
	// A rejection must say why. The reason is what the agent is shown, and an
	// unexplained refusal is indistinguishable from the platform losing the
	// request - so an empty one is a field error rather than a silent default.
	if strings.TrimSpace(req.Reason) == "" {
		return Withdrawal{}, fieldErr("reason", "a rejection must state why")
	}
	at := l.clock.Now()
	var out Withdrawal
	err := l.store.Update(ctx, func(ctx context.Context, tx Tx) error {
		cur, err := l.loadScopedWithdrawal(ctx, tx, req.TenantID, req.WithdrawalID)
		if err != nil {
			return err
		}
		if !cur.State.Reserved() {
			return &TransitionError{
				Kind: "withdrawal reject", From: cur.State.String(), To: WithdrawalRejected.String(),
			}
		}
		if err := l.releaseReservation(ctx, tx, cur, LedgerWithdrawRefund, req.Reason, at); err != nil {
			return err
		}
		out, err = tx.TransitionWithdrawal(ctx, cur.ID,
			cur.State, WithdrawalRejected, cur.Version,
			WithdrawalTransition{Operator: req.Operator, FailReason: req.Reason, AuditedAt: at})
		return err
	})
	if err != nil {
		return Withdrawal{}, err
	}
	l.log.WarnContext(ctx, "withdrawal rejected",
		"withdrawal_id", out.ID, "operator", req.Operator, "reason", req.Reason)
	return out, nil
}

// PayWithdraw asks the configured channel to pay, then records the outcome.
//
// # Why the call sits between two transactions
//
// The store may retry a transaction by re-running its closure. A channel call
// inside one would therefore be attempted again on every retry, and a payout is
// the one action here that cannot be undone. So the state is read, the
// transaction closes, the channel is called, and a second transaction records
// what happened.
//
// That leaves a window in which the channel succeeded and the outcome is not
// yet recorded. A crash there means the withdrawal is still Approved on
// recovery, and a retry would pay again — which is why the channel receives the
// withdrawal's idempotency key and must honour it. That requirement is the
// price of not holding a database transaction open across a network call, and
// it is cheaper than the alternative.
func (l *Ledger) PayWithdraw(ctx context.Context, req PayoutOutcome) (Withdrawal, error) {
	channel := l.payout
	if channel == nil {
		return Withdrawal{}, ErrNoPayoutChannel
	}

	cur, err := l.loadWithdrawal(ctx, req.TenantID, req.WithdrawalID)
	if err != nil {
		return Withdrawal{}, err
	}
	// Approved is the first attempt; PayFailed is a retry. Both are legitimate
	// places to start a payout, and excluding the second would make the retry
	// path unreachable - the money would stay reserved with no way to pay it.
	switch cur.State {
	case WithdrawalApproved, WithdrawalPayFailed:
	default:
		return Withdrawal{}, &TransitionError{
			Kind: "withdrawal pay", From: cur.State.String(), To: WithdrawalPaid.String(),
		}
	}

	result, err := channel.Pay(ctx, PayoutRequest{
		IdemKey:      cur.IdemKey,
		TenantID:     cur.Key.TenantID,
		UserID:       cur.Key.UserID,
		Amount:       cur.RealAmount,
		AccountInfo:  cur.AccountInfo,
		WithdrawalID: cur.ID,
	})
	if err != nil {
		// The channel could not be reached. Recording that as a failure keeps
		// the money reserved and the reason visible, rather than leaving the
		// withdrawal silently stuck in Approved.
		return l.recordPayoutFailure(ctx, cur, "channel error: "+err.Error(), req.Operator)
	}
	if !result.Paid {
		reason := result.FailReason
		if reason == "" {
			reason = "channel reported failure without a reason"
		}
		return l.recordPayoutFailure(ctx, cur, reason, req.Operator)
	}

	return l.settlePayout(ctx, cur, result.InvoiceNo, req.Operator)
}

// MarkWithdrawPaid records that a payout happened outside the library.
//
// It is the entry point for a manual channel: the operator made the transfer
// and is recording the fact. It does not call any channel, and it is
// idempotent in the sense that a withdrawal that is already paid is returned
// unchanged rather than being paid twice.
func (l *Ledger) MarkWithdrawPaid(ctx context.Context, req PayoutOutcome) (Withdrawal, error) {
	if len(req.InvoiceNo) > maxRemarkLen {
		return Withdrawal{}, fieldErrf("invoice_no",
			"must be at most %d bytes, got %d", maxRemarkLen, len(req.InvoiceNo))
	}
	cur, err := l.loadWithdrawal(ctx, req.TenantID, req.WithdrawalID)
	if err != nil {
		return Withdrawal{}, err
	}
	switch cur.State {
	case WithdrawalPaid:
		// Already recorded. Returning the record rather than reporting a
		// conflict is what makes a retried confirmation safe.
		return cur, nil
	case WithdrawalApproved, WithdrawalPayFailed:
	default:
		return Withdrawal{}, &TransitionError{
			Kind: "withdrawal mark paid", From: cur.State.String(), To: WithdrawalPaid.String(),
		}
	}
	return l.settlePayout(ctx, cur, req.InvoiceNo, req.Operator)
}

// recordPayoutFailure moves an approved withdrawal into PayFailed, keeping the
// money reserved so that a retry pays the same amount rather than re-reserving.
func (l *Ledger) recordPayoutFailure(
	ctx context.Context, cur Withdrawal, reason, operator string,
) (Withdrawal, error) {
	if len(reason) > maxReasonLen {
		reason = reason[:maxReasonLen]
	}
	// No timestamp is recorded here. A failed attempt is not a lifecycle
	// milestone the schema carries, and filling AuditedAt with it would claim
	// the withdrawal had been reviewed when it had only been attempted. The
	// attempt time goes to the log, which is where an operator deciding whether
	// to retry looks for it anyway.
	var out Withdrawal
	err := l.store.Update(ctx, func(ctx context.Context, tx Tx) error {
		live, err := tx.WithdrawalByID(ctx, cur.ID)
		if err != nil {
			return err
		}
		out, err = tx.TransitionWithdrawal(ctx, live.ID,
			live.State, WithdrawalPayFailed, live.Version,
			WithdrawalTransition{FailReason: reason, Operator: operator})
		return err
	})
	if err != nil {
		return Withdrawal{}, err
	}
	l.log.WarnContext(ctx, "withdrawal payout failed",
		"withdrawal_id", out.ID, "reason", reason)
	return out, nil
}

// settlePayout moves the reserved money to Withdrawn and marks the withdrawal
// paid.
//
// # What moves
//
// The gross amount moves, not the net. The agent's account loses what it was
// debited, and the fee is revenue that belongs outside this ledger. Moving
// anything other than the gross would make the account disagree with the sum of
// its ledger deltas, which is invariant I1 - the fee is recorded on the
// withdrawal precisely so that the difference is explainable without being a
// separate money movement.
func (l *Ledger) settlePayout(
	ctx context.Context, cur Withdrawal, invoice, operator string,
) (Withdrawal, error) {
	if len(invoice) > maxRemarkLen {
		return Withdrawal{}, fieldErrf("invoice_no",
			"must be at most %d bytes, got %d", maxRemarkLen, len(invoice))
	}
	at := l.clock.Now()
	var out Withdrawal
	err := l.store.Update(ctx, func(ctx context.Context, tx Tx) error {
		live, err := tx.WithdrawalByID(ctx, cur.ID)
		if err != nil {
			return err
		}
		// Re-checked against the live row: between the channel call and here,
		// another caller may have moved it.
		if live.State == WithdrawalPaid {
			out = live
			return nil
		}
		amount := live.Outstanding()
		if amount <= 0 {
			out = live
			return nil
		}

		acct, err := tx.IncrementAccount(ctx, AccountDelta{
			Key:                live.Key,
			Withdrawing:        -amount,
			Withdrawn:          amount,
			RequireNonNegative: true,
			At:                 at,
		})
		if err != nil {
			return err
		}
		entry := newLedgerEntry(
			live.Key, LedgerWithdrawPaid, strconv.FormatInt(live.ID, 10),
			"withdraw paid "+strconv.FormatInt(live.ID, 10)+
				" amount "+amount.String()+
				" real "+live.RealAmount.String()+
				" fee "+live.Fee.String(),
			at, acct, BucketWithdrawing, -amount,
		)
		BucketWithdrawn.AddDelta(&entry, amount)
		if _, err := tx.AppendLedger(ctx, entry); err != nil {
			return err
		}

		if !live.PaidAt.IsZero() {
			return fmt.Errorf("distledger: withdrawal %d already records a payout", live.ID)
		}
		out, err = tx.TransitionWithdrawal(ctx, live.ID,
			live.State, WithdrawalPaid, live.Version,
			WithdrawalTransition{InvoiceNo: invoice, Operator: operator, PaidAt: at})
		return err
	})
	if err != nil {
		return Withdrawal{}, err
	}
	l.log.InfoContext(ctx, "withdrawal paid",
		"withdrawal_id", out.ID, "amount", out.Amount.String(),
		"real_amount", out.RealAmount.String(), "invoice_no", invoice)
	return out, nil
}

// releaseReservation returns reserved money to the withdrawable balance.
func (l *Ledger) releaseReservation(
	ctx context.Context, tx Tx, w Withdrawal, bizType LedgerBizType, reason string, at time.Time,
) error {
	amount := w.Outstanding()
	if amount <= 0 {
		return nil
	}
	acct, err := tx.IncrementAccount(ctx, AccountDelta{
		Key:                w.Key,
		Withdrawing:        -amount,
		Available:          amount,
		RequireNonNegative: true,
		At:                 at,
	})
	if err != nil {
		return err
	}
	entry := newLedgerEntry(
		w.Key, bizType, strconv.FormatInt(w.ID, 10),
		"withdraw released "+strconv.FormatInt(w.ID, 10)+" amount "+amount.String()+
			" reason "+reason,
		at, acct, BucketWithdrawing, -amount,
	)
	BucketAvailable.AddDelta(&entry, amount)
	_, err = tx.AppendLedger(ctx, entry)
	return err
}

// loadWithdrawal reads a withdrawal scoped to a tenant.
func (l *Ledger) loadWithdrawal(ctx context.Context, tenantID, id int64) (Withdrawal, error) {
	if tenantID < 0 {
		return Withdrawal{}, fieldErrf("tenant_id", "must be >= 0, got %d", tenantID)
	}
	if id <= 0 {
		return Withdrawal{}, fieldErrf("withdrawal_id", "must be > 0, got %d", id)
	}
	var out Withdrawal
	err := l.store.View(ctx, func(ctx context.Context, r Reader) error {
		w, err := r.WithdrawalByID(ctx, id)
		if err != nil {
			return err
		}
		if w.Key.TenantID != tenantID {
			// Indistinguishable from "not found" on purpose: revealing that an
			// id exists in another tenant is itself a leak.
			return ErrNotFound
		}
		out = w
		return nil
	})
	return out, err
}

// loadScopedWithdrawal is loadWithdrawal inside an existing transaction.
func (l *Ledger) loadScopedWithdrawal(ctx context.Context, tx Tx, tenantID, id int64) (Withdrawal, error) {
	if tenantID < 0 {
		return Withdrawal{}, fieldErrf("tenant_id", "must be >= 0, got %d", tenantID)
	}
	if id <= 0 {
		return Withdrawal{}, fieldErrf("withdrawal_id", "must be > 0, got %d", id)
	}
	cur, err := tx.WithdrawalByID(ctx, id)
	if err != nil {
		return Withdrawal{}, err
	}
	if cur.Key.TenantID != tenantID {
		return Withdrawal{}, ErrNotFound
	}
	return cur, nil
}

// withdrawFee computes the platform's cut of a withdrawal.
func withdrawFee(amount Money, rate Rate, rounding Rounding) (Money, error) {
	if rate == 0 {
		return 0, nil
	}
	fee, err := amount.Apply(rate, rounding)
	if err != nil {
		return 0, err
	}
	if fee >= amount {
		// A fee that swallows the amount means the agent pays to withdraw,
		// which is a configuration error rather than a payout.
		return 0, fieldErrf("fee_rate_bp",
			"a fee of %s on %s leaves nothing to pay out", fee, amount)
	}
	return fee, nil
}

// Withdrawal returns one withdrawal, scoped to a tenant.
func (l *Ledger) Withdrawal(ctx context.Context, tenantID, id int64) (Withdrawal, error) {
	return l.loadWithdrawal(ctx, tenantID, id)
}

// WithdrawalByIdemKey returns one withdrawal by the caller's own key, which is
// how a caller checks what became of a request it may have retried.
func (l *Ledger) WithdrawalByIdemKey(ctx context.Context, tenantID int64, callerKey string) (Withdrawal, error) {
	if err := validateIdemKey(callerKey); err != nil {
		return Withdrawal{}, err
	}
	var out Withdrawal
	err := l.store.View(ctx, func(ctx context.Context, r Reader) error {
		w, err := r.WithdrawalByIdemKey(ctx, tenantID,
			withdrawIdemKey(tenantID, callerKey))
		if err != nil {
			return err
		}
		out = w
		return nil
	})
	return out, err
}

// Withdrawals returns a tenant's withdrawals, paginated.
func (l *Ledger) Withdrawals(
	ctx context.Context, tenantID int64, p Page,
) ([]Withdrawal, error) {
	var out []Withdrawal
	err := l.store.View(ctx, func(ctx context.Context, r Reader) error {
		var err error
		out, err = r.WithdrawalsByTenant(ctx, tenantID, p)
		return err
	})
	return out, err
}

const (
	// maxChannelLen bounds a channel name, which is stored and echoed into
	// ledger remarks.
	maxChannelLen = 64
	// maxRemarkLen bounds a reason or invoice number, for the same reason.
	maxRemarkLen = 256
)

// validateIdemKey checks a caller-supplied idempotency key.
func validateIdemKey(key string) error {
	if key == "" {
		return fieldErr("idem_key", "must not be empty")
	}
	if len(key) > MaxIdemKeyLen {
		return fieldErrf("idem_key", "must be at most %d bytes, got %d", MaxIdemKeyLen, len(key))
	}
	if strings.TrimSpace(key) != key {
		return fieldErr("idem_key", "must not have leading or trailing whitespace")
	}
	return nil
}
