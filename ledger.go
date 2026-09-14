package distledger

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"
)

// Config is the set of dependencies needed to construct a Ledger.
//
// It deliberately has no "default global instance" form: this library ships
// no package-level singletons, and every dependency is passed in explicitly
// (see ADR-012).
type Config struct {
	// Store is the persistence implementation. Required.
	Store Store
	// Rules holds the business rules. Defaults to DefaultRules() when empty.
	Rules Rules
	// Clock is the time source. Defaults to SystemClock() when empty.
	//
	// The library never calls time.Now() directly, so a test can use
	// ManualClock to jump the freeze window forward instantly without
	// sleeping.
	Clock Clock
	// Logger is the log sink. When empty, all logs are discarded.
	Logger *slog.Logger
	// RateResolver decides the rate of each level. Defaults to the
	// "per-agent override > global rules" implementation when empty.
	RateResolver RateResolver
	// Eligibility decides whether an agent is eligible for a commission.
	// When empty, the agent is required to be in AgentActive state.
	Eligibility EligibilityChecker
	// Payout is the channel used by PayWithdraw.
	//
	// Empty means automatic payout is not configured: PayWithdraw returns
	// ErrNoPayoutChannel, while RequestWithdraw, ApproveWithdraw, RejectWithdraw
	// and MarkWithdrawPaid all still work. That is the right default for a
	// platform that pays by hand, and it is the conservative one - ADR-010's
	// principle is that money should not leave without somebody deciding it
	// should, and an unconfigured channel cannot leak it.
	Payout PayoutChannel
}

// Ledger is the public facade of the distribution ledger kernel.
//
// It is safe for concurrent use: all state lives in the Store, and the Ledger
// itself holds only immutable configuration.
type Ledger struct {
	store Store
	rules Rules
	clock Clock
	log   *slog.Logger
	rates RateResolver
	elig  EligibilityChecker
	// payout is the channel used by PayWithdraw. Nil means automatic payout is
	// not configured; MarkWithdrawPaid still works, and that is the right
	// default for a platform that pays by hand (ADR-010).
	payout PayoutChannel
	// ruleVersion is the content fingerprint of the rule set, computed once at
	// construction so that the hot paths do not recompute the SHA-256.
	ruleVersion int64
}

// New constructs a Ledger.
//
// It returns an error rather than proceeding best-effort when the
// configuration is not self-consistent: running with broken rules pays out the
// wrong money, and that cannot be undone.
func New(cfg Config) (*Ledger, error) {
	if cfg.Store == nil {
		return nil, fieldErr("store", "must not be nil")
	}

	rules := cfg.Rules.Normalize()
	if err := rules.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidConfig, err)
	}

	clock := cfg.Clock
	if clock == nil {
		clock = SystemClock()
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	rates := cfg.RateResolver
	if rates == nil {
		rates = defaultRateResolver{}
	}
	elig := cfg.Eligibility
	if elig == nil {
		elig = defaultEligibility{}
	}

	return &Ledger{
		store:       cfg.Store,
		rules:       rules,
		clock:       clock,
		log:         logger,
		rates:       rates,
		elig:        elig,
		payout:      cfg.Payout,
		ruleVersion: rules.Version(),
	}, nil
}

// Rules returns a copy of the rules in effect at construction.
func (l *Ledger) Rules() Rules {
	out := l.rules
	out.RateBP = append([]Rate(nil), l.rules.RateBP...)
	return out
}

// Now returns the time currently used by the ledger kernel.
func (l *Ledger) Now() time.Time { return l.clock.Now() }

// ── Relation chain ───────────────────────────────────────────────────

// BindAgent registers or updates an agent.
//
// # The parent cannot change quietly
//
// If the agent already exists and ParentID in the request differs from
// the stored value, ErrParentAlreadySet is returned. Changing a parent on a
// whim silently rewrites the attribution and the historical earnings of the
// whole downstream tree, so this method refuses to do it; an explicit rebind
// is a separate action, to be offered by a dedicated API in a later version
// (see ADR-006).
func (l *Ledger) BindAgent(ctx context.Context, req BindAgentRequest) (Agent, error) {
	if req.TenantID < 0 {
		return Agent{}, fieldErrf("tenant_id", "must be >= 0, got %d", req.TenantID)
	}
	if req.UserID <= 0 {
		return Agent{}, fieldErrf("user_id", "must be > 0, got %d", req.UserID)
	}
	if req.ParentID < 0 {
		return Agent{}, fieldErrf("parent_id", "must be >= 0, got %d", req.ParentID)
	}
	if req.ParentID == req.UserID {
		return Agent{}, fieldErr("parent_id", "must not equal user_id")
	}
	if !req.JoinType.Valid() {
		return Agent{}, fieldErrf("join_type", "unknown join type %d", uint8(req.JoinType))
	}
	if !req.Status.Valid() {
		return Agent{}, fieldErrf("status", "unknown status %d", uint8(req.Status))
	}
	if req.RateOverrideBP != nil && !req.RateOverrideBP.Valid() {
		return Agent{}, fieldErrf("rate_override_bp", "out of range [0, %d]: %d",
			MaxRateBP, *req.RateOverrideBP)
	}

	key := UserKey{TenantID: req.TenantID, UserID: req.UserID}
	var out Agent
	err := l.store.Update(ctx, func(ctx context.Context, tx Tx) error {
		cur, err := tx.Agent(ctx, key)
		switch {
		case err == nil:
			if cur.ParentID != req.ParentID {
				return ErrParentAlreadySet
			}
			cur.Status = req.Status
			cur.JoinType = req.JoinType
			cur.RateOverrideBP = req.RateOverrideBP
			cur.UpdatedAt = l.clock.Now()
			next, err := tx.PutAgent(ctx, cur)
			if err != nil {
				return err
			}
			out = next
			return nil
		case !errors.Is(err, ErrNotFound):
			return err
		}

		depth, err := l.resolveDepth(ctx, tx, key, req.ParentID)
		if err != nil {
			return err
		}
		now := l.clock.Now()
		next, err := tx.PutAgent(ctx, Agent{
			Key:            key,
			ParentID:       req.ParentID,
			Depth:          depth,
			Status:         req.Status,
			JoinType:       req.JoinType,
			RateOverrideBP: req.RateOverrideBP,
			CreatedAt:      now,
			UpdatedAt:      now,
		})
		if err != nil {
			return err
		}
		out = next
		return nil
	})
	if err != nil {
		return Agent{}, err
	}
	return out, nil
}

// resolveDepth computes the depth of a new agent and performs two safety
// checks along the way.
//
//   - Cycle detection: walk up the parent chain; reaching the agent itself or
//     a repeated node means there is a cycle.
//   - Depth ceiling: anything past MaxLevels is rejected outright, so that no
//     level exists for which no rate can be resolved.
//
// Because MaxLevels is only 3, the loop runs at most 3 times, so the cost is
// negligible.
func (l *Ledger) resolveDepth(ctx context.Context, r Reader, key UserKey, parentID int64) (int, error) {
	depth := 1
	cur := parentID
	seen := make(map[int64]struct{}, MaxLevels)
	for cur != 0 {
		if cur == key.UserID {
			return 0, ErrCycle
		}
		if _, dup := seen[cur]; dup {
			return 0, ErrCycle
		}
		seen[cur] = struct{}{}

		depth++
		if depth > MaxLevels {
			return 0, fmt.Errorf("%w: depth would exceed %d", ErrDepthExceeded, MaxLevels)
		}

		parent, err := r.Agent(ctx, UserKey{TenantID: key.TenantID, UserID: cur})
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				return 0, fieldErrf("parent_id", "agent %d does not exist", cur)
			}
			return 0, err
		}
		cur = parent.ParentID
	}
	return depth, nil
}

// BindBuyer creates the "buyer -> agent" attribution relationship.
//
// # Attribution rules
//
//   - First binding wins: as long as the existing binding is still effective
//     at BoundAt, it is not overwritten.
//   - Binding to a different agent returns ErrBindingLocked together
//     with the existing binding.
//   - An existing binding that has expired or been deactivated may be rebound,
//     with ReboundFrom recorded.
//
// Policies such as a rebind cooling-off period or a customer protection window
// belong to the rules layer and will be offered as a policy interface in a
// later version.
func (l *Ledger) BindBuyer(ctx context.Context, req BindBuyerRequest) (Binding, error) {
	buyer := UserKey{TenantID: req.TenantID, UserID: req.BuyerUserID}
	if err := buyer.Validate(); err != nil {
		return Binding{}, err
	}
	if req.AgentUserID <= 0 {
		return Binding{}, fieldErrf("agent_user_id", "must be > 0, got %d", req.AgentUserID)
	}
	if !req.Source.Valid() {
		return Binding{}, fieldErrf("source", "unknown bind source %d", uint8(req.Source))
	}
	if len(req.SourceRef) > MaxOrderItemIDLen {
		return Binding{}, fieldErrf("source_ref", "must be at most %d bytes, got %d",
			MaxOrderItemIDLen, len(req.SourceRef))
	}

	boundAt := req.BoundAt
	if boundAt.IsZero() {
		boundAt = l.clock.Now()
	}
	boundAt = boundAt.UTC()

	var (
		out     Binding
		locked  bool
		agentID = req.AgentUserID
	)

	err := l.store.Update(ctx, func(ctx context.Context, tx Tx) error {
		agentKey := UserKey{TenantID: req.TenantID, UserID: agentID}
		agent, err := tx.Agent(ctx, agentKey)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				return fieldErrf("agent_user_id", "agent %d does not exist", agentID)
			}
			return err
		}
		// Binding to an inactive or disabled agent is meaningless and
		// would pollute the attribution data.
		if agent.Status != AgentActive {
			return fieldErrf("agent_user_id",
				"agent %d is not active (status=%s)", agentID, agent.Status)
		}
		if agentID == req.BuyerUserID {
			return fieldErr("agent_user_id", "buyer must not bind to itself")
		}

		cur, err := tx.Binding(ctx, buyer)
		switch {
		case err == nil:
			if cur.Effective(boundAt) {
				out = cur
				if cur.AgentUserID != agentID {
					locked = true
				}
				return nil
			}
			// The old binding is no longer effective: rebinding is allowed,
			// and the source is recorded so that rebind history can be
			// audited.
			cur.AgentUserID = agentID
			cur.Source = req.Source
			cur.SourceRef = req.SourceRef
			cur.BoundAt = boundAt
			cur.ExpireAt = l.expiryFor(boundAt)
			cur.Active = true
			cur.ReboundFrom = cur.ID
			next, err := tx.PutBinding(ctx, cur)
			if err != nil {
				return err
			}
			out = next
			return nil
		case !errors.Is(err, ErrNotFound):
			return err
		}

		next, err := tx.PutBinding(ctx, Binding{
			Buyer:       buyer,
			AgentUserID: agentID,
			Source:      req.Source,
			SourceRef:   req.SourceRef,
			BoundAt:     boundAt,
			ExpireAt:    l.expiryFor(boundAt),
			Active:      true,
		})
		if err != nil {
			return err
		}
		out = next
		return nil
	})
	if err != nil {
		return Binding{}, err
	}
	if locked {
		return out, ErrBindingLocked
	}
	return out, nil
}

func (l *Ledger) expiryFor(boundAt time.Time) time.Time {
	if l.rules.BindExpireDays <= 0 {
		return time.Time{}
	}
	return boundAt.AddDate(0, 0, l.rules.BindExpireDays)
}

// ── Queries ──────────────────────────────────────────────────────────

// Balance returns the account balance of a user. A user whose money never
// moved returns a zero-valued account.
func (l *Ledger) Balance(ctx context.Context, tenantID, userID int64) (Account, error) {
	key := UserKey{TenantID: tenantID, UserID: userID}
	if err := key.Validate(); err != nil {
		return Account{}, err
	}
	var acct Account
	err := l.store.View(ctx, func(ctx context.Context, r Reader) error {
		var err error
		acct, err = r.Account(ctx, key)
		return err
	})
	return acct, err
}

// Agent returns agent information.
func (l *Ledger) Agent(ctx context.Context, tenantID, userID int64) (Agent, error) {
	key := UserKey{TenantID: tenantID, UserID: userID}
	if err := key.Validate(); err != nil {
		return Agent{}, err
	}
	var out Agent
	err := l.store.View(ctx, func(ctx context.Context, r Reader) error {
		var err error
		out, err = r.Agent(ctx, key)
		return err
	})
	return out, err
}

// CommissionsByOrder returns every commission one order produced, at all
// levels.
func (l *Ledger) CommissionsByOrder(ctx context.Context, tenantID int64, orderID string) ([]Commission, error) {
	key := OrderKey{TenantID: tenantID, OrderID: orderID}
	if err := key.Validate(); err != nil {
		return nil, err
	}
	var out []Commission
	err := l.store.View(ctx, func(ctx context.Context, r Reader) error {
		var err error
		out, err = r.CommissionsByOrder(ctx, key)
		return err
	})
	return out, err
}

// CommissionsByAgent returns one agent's commissions, paginated.
func (l *Ledger) CommissionsByAgent(ctx context.Context, tenantID, userID int64, q CommissionQuery) ([]Commission, error) {
	key := UserKey{TenantID: tenantID, UserID: userID}
	if err := key.Validate(); err != nil {
		return nil, err
	}
	var out []Commission
	err := l.store.View(ctx, func(ctx context.Context, r Reader) error {
		var err error
		out, err = r.CommissionsByAgent(ctx, key, q)
		return err
	})
	return out, err
}

// LedgerEntries returns one user's ledger entries, paginated.
func (l *Ledger) LedgerEntries(ctx context.Context, tenantID, userID int64, q LedgerQuery) ([]LedgerEntry, error) {
	key := UserKey{TenantID: tenantID, UserID: userID}
	if err := key.Validate(); err != nil {
		return nil, err
	}
	var out []LedgerEntry
	err := l.store.View(ctx, func(ctx context.Context, r Reader) error {
		var err error
		out, err = r.LedgerEntries(ctx, key, q)
		return err
	})
	return out, err
}

// Close closes the underlying Store.
func (l *Ledger) Close() error { return l.store.Close() }
