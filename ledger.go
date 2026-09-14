package distledger

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"
)

// Config 是构造 Ledger 所需的依赖。
//
// 它刻意没有「默认全局实例」这种形态：本库不提供任何包级单例，
// 一切依赖显式传入（见 ADR-012）。
type Config struct {
	// Store 是持久化实现，必填。
	Store Store
	// Rules 是业务规则。留空时使用 DefaultRules()。
	Rules Rules
	// Clock 是时间源。留空时使用 SystemClock()。
	//
	// 库内部不会直接调用 time.Now()，因此测试可以用 ManualClock 把
	// 冻结期瞬间推进，不需要 sleep。
	Clock Clock
	// Logger 是日志出口。留空时丢弃全部日志。
	Logger *slog.Logger
	// RateResolver 决定各层级费率。留空时使用「个体覆盖 > 全局规则」的默认实现。
	RateResolver RateResolver
	// Eligibility 决定分销员是否有资格获得佣金。留空时要求状态为 AgentActive。
	Eligibility EligibilityChecker
}

// Ledger 是分销账务内核的对外门面。
//
// 它是并发安全的：所有状态都在 Store 内，Ledger 自身只持有不可变的配置。
type Ledger struct {
	store Store
	rules Rules
	clock Clock
	log   *slog.Logger
	rates RateResolver
	elig  EligibilityChecker
	// ruleVersion 是规则集的内容指纹，构造时算一次，避免在每个热路径上
	// 重复做 SHA-256。
	ruleVersion int64
}

// New 构造一个 Ledger。
//
// 配置不自洽时返回错误而不是「尽力而为」：带着错误规则跑起来的结果是
// 发出错误的钱，而那不可撤销。
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
		ruleVersion: rules.Version(),
	}, nil
}

// Rules 返回构造时生效的规则副本。
func (l *Ledger) Rules() Rules {
	out := l.rules
	out.RateBP = append([]Rate(nil), l.rules.RateBP...)
	return out
}

// Now 返回账务内核当前使用的时间。
func (l *Ledger) Now() time.Time { return l.clock.Now() }

// ── 关系链 ──────────────────────────────────────────────────────────────

// BindAgent 登记或更新一个分销员。
//
// # 上级不可悄悄变更
//
// 若该分销员已存在且请求中的 ParentID 与现有值不同，返回 ErrParentAlreadySet。
// 擅自改上级会无声地重写整个下级树的归属与历史收益，因此本方法拒绝执行；
// 显式的换绑是另一个动作，会在后续版本以独立 API 提供（见 ADR-006）。
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

// resolveDepth 计算新分销员的层级深度，并顺带完成两项安全检查。
//
//   - 环检测：沿上级链向上走，若遇到自己或遇到重复节点即为环。
//   - 深度上限：超过 MaxLevels 直接拒绝，避免出现「配不出费率」的层级。
//
// 因为 MaxLevels 只有 3，这个循环最多走 3 次，代价可忽略。
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

// BindBuyer 建立「买家 → 分销员」的归因关系。
//
// # 归因规则（v0.1）
//
//   - 首次绑定优先：只要现有绑定在 BoundAt 时刻仍然有效，就不覆盖。
//   - 绑定到不同的分销员时返回 ErrBindingLocked，同时返回现有绑定。
//   - 现有绑定已过期或已失效时允许重新绑定，并记录 ReboundFrom。
//
// 换绑冷静期、客户保护期等策略属于规则层，会在后续版本以策略接口形式提供。
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
		// 绑定到一个未生效或被禁用的分销员没有意义，而且会污染归因数据。
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
			// 旧绑定已失效：允许重新绑定，并记录来源，便于审计换绑历史。
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

// ── 查询 ────────────────────────────────────────────────────────────────

// Balance 返回用户的账户余额。从未产生过资金变动的用户返回零值账户。
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

// Agent 返回分销员信息。
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

// CommissionsByOrder 返回某订单产生的全部佣金（含各级）。
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

// CommissionsByAgent 分页返回某分销员的佣金。
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

// LedgerEntries 分页返回某用户的资金流水。
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

// Close 关闭底层 Store。
func (l *Ledger) Close() error { return l.store.Close() }
