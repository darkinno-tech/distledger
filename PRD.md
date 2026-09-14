# PRD：distledger — Go 分销账务内核

| 项目 | 内容 |
|---|---|
| **项目名** | `distledger`（分销账务内核 / Distribution Ledger Kernel） |
| **文档版本** | v1.0 |
| **状态** | 设计阶段（Design / RFC） |
| **日期** | 2026-09-14 |
| **许可** | MIT |
| **定位** | 开源、非商业、零强依赖的 Go 分销**资金账务**内核 |
| **模块路径** | `github.com/darkinno-tech/distledger`（可按实际组织调整） |

---

## 0. TL;DR

> **把"分销的钱"算对、记清、可追溯的最小可复用内核。**
>
> 只解决三件事：**关系链**、**佣金账**、**资金流水**。
> 规则（几级、多少比例、要不要门槛）全部外置为可插拔策略。
> 默认不发钱、默认 2 级、默认人工打款 —— 宁可配置麻烦，不可默认错发。

**为什么是"内核"而不是"分销系统"**：分销的业务规则是无限长尾（每家公司都不一样），但资金账务流对所有人**完全一样**：

```
订单归属某分销员 → 按规则算出若干级佣金 → 进冻结 → 到期可提现 → 退款扣回 → 出金
```

把长尾做进库 → 库必烂；把不变量做进库 → 库可复用。

---

## 1. 背景与问题定义

### 1.1 现状（已核实的事实）

| 事实 | 证据 |
|---|---|
| Go 生态**没有**独立可直接用的分销库 | 市面开源分销均为 PHP/Java 电商系统的内置模块；Go 唯一代表 Golershop 的开源版**摘掉了 `internal/logic` 与路由绑定**，只剩表结构与接口声明 |
| 分销能力的**正确性要求极高、复用度极高、业务差异极小** | 各家差异集中在"比例/层级/门槛"，而"退款冲正、幂等、余额守恒"完全一致 |
| 每个团队都在重写同一段代码 | 且这段代码一旦写错，直接等于**丢钱** |

### 1.2 核心痛点（按严重度排序）

1. **P0 · 退款冲正漏了**：全退/部分退、已提现后追回、部分层级已结算 —— 处理不当直接资损
2. **P0 · 重复入账**：订单状态回调重复触发，佣金算两遍
3. **P1 · 冻结期配置变更污染历史**：改一个全局配置，历史订单的可提现时点跟着变
4. **P1 · 无法解释"为什么是这个数"**：三个月后有人问某笔佣金怎么算的，答不上来
5. **P2 · 接入成本高**：要理解一整套电商系统的概念才能加一个分销

### 1.3 我们不做的事（结论先行）

**不做"通用分销库"**。理由：规则在客户手里（长尾），责任在库里（钱不能错），两头都要必然长成巨无霸。**只做账务内核**。

---

## 2. 项目定位

### 2.1 一句话

> **distledger = 分销的复式记账内核 + 状态机 + 幂等保证。**
> 你告诉它"这笔订单成交了、归属于谁"，它负责"钱怎么记、什么时候能拿、退款后怎么扣回"。

### 2.2 它是什么 / 不是什么

| ✅ 是 | ❌ 不是 |
|---|---|
| 关系链存储与查询 | 分销员申请/审核的后台流程 |
| 佣金计算编排（不是算法本身） | 佣金算法本身（由 `Calculator` 接口提供） |
| 冻结 / 解冻 / 结算状态机 | 定时任务调度框架 |
| 退款冲正（含部分退、含已结算追回） | 退款业务本身（订单系统负责） |
| 资金流水与余额守恒 | 支付、分账、代收货款 |
| 提现单状态机 + 打款通道抽象 | 微信企业付款/支付宝转账的具体实现 |
| 幂等与并发安全 | HTTP 路由、Admin 后台、UI |

---

## 3. 目标与非目标

### 3.1 可度量的目标（v1.0 验收标准）

| 编号 | 目标 | 度量方式 |
|---|---|---|
| **G1** | 零依赖跑通全链路 | 新机器上 `git clone && cd examples/01-quickstart && go run .` —— **不需要 MySQL、不需要读源码**，30 秒内看到佣金数字 |
| **G2** | 接入已有系统的新增代码 ≤ 50 行 | examples/05 的实际行数 |
| **G3** | 三条不变量恒成立 | property-based + 随机事件序列测试，10 万次随机序列零违反 |
| **G4** | 生产可用 | MySQL store 支持事务 / 行级锁 / 幂等约束 |
| **G5** | 零第三方强依赖 | `go.mod` 的 `require` 为空（MySQL 驱动由用户提供） |
| **G6** | 出错可自诊 | `SelfCheck()` 一次调用给出结构化报告，覆盖 90% 接入问题 |

### 3.2 非目标（Non-Goals）——本清单是需求边界的法律

- ❌ 分销员注册/审核/等级晋升的流程编排（只存状态 + 抛事件）
- ❌ 推广链接生成、海报、小程序码、排行榜、收益中心
- ❌ 团队计酬、区域分红、链动、推三返一等玩法（**永不内置**）
- ❌ 按"拉人头/注册人数"计酬的能力（**刻意不提供**）
- ❌ 支付、分账、资金池、代收货款
- ❌ HTTP handler、路由注册、Admin 后台
- ❌ 多租户隔离（仅预留 `tenant_id` 字段，不做隔离策略）
- ❌ ORM / MQ / 配置中心的绑定
- ❌ 代码生成、反射注册、隐式全局状态

---

## 4. 目标用户与场景

| Persona | 画像 | 核心诉求 | 本库如何满足 |
|---|---|---|---|
| **P1 · 独立开发者** | 给现有小程序商城加分销，**没有企业付款资质** | 快、能跑、能人工打款 | 内存 store 零依赖体验 + `ManualChannel` 默认通道 |
| **P2 · 中型 SaaS 团队** | 一套代码服务多个客户，规则各不相同 | 规则可插拔、不 fork | `Calculator` / `Eligibility` 接口 + 结构化配置双轨 |
| **P3 · 已有商城团队** | 已用 Golershop/自研商城，想把分销账务抽出来 | 不改订单系统，旁路接入 | 事件驱动，2 个入口函数，**不接管生命周期** |

### 4.1 典型场景（User Story）

- **US-1**：用户 A 分享链接，B 下单支付 → 系统产生 A 的一级佣金（待结算）
- **US-2**：B 确认收货 7 天后 → A 的佣金从"待结算"变为"可提现"
- **US-3**：B 申请退款成功 → A 的佣金被扣回；若已提现，从后续收益扣减
- **US-4**：A 申请提现 100 元 → 平台审核 → 人工打款 → 记录银行流水号
- **US-5**：运营把佣金率从 5% 改成 3% → **历史订单的解释不受影响**
- **US-6**：订单回调重复到达 3 次 → 佣金只产生 1 笔

---

## 5. 设计铁律（Architecture Invariants）

| 编号 | 铁律 | 说明 |
|---|---|---|
| **R1** | **切分线**：能改变"钱**流向**"的 → 内核；只改变"钱**数值**"的 → 规则层 | 规则层**永远不许**修改状态。这是库能否通用的唯一分水岭 |
| **R2** | **默认不发钱** | 默认费率 0、默认 2 级、默认关闭自动打款。配置麻烦可以忍，默认错发钱不能忍 |
| **R3** | **金额一律 `int64` 最小货币单位（分）**，费率一律 `int` 万分比（bp） | 全程**禁止** `float64` / `decimal` 参与运算 |
| **R4** | **账本不可变** | 佣金明细与资金流水**只追加不修改**；冲正靠新增负记录 |
| **R5** | **失败安全（fail-safe）** | 非法状态迁移返回错误而非猜测；任何异常不得让钱凭空产生或消失 |
| **R6** | **可追溯** | 每一分钱都能回答：哪一单、哪条规则、什么版本、什么时候 |
| **R7** | **零魔法** | 无 codegen、无反射注册、无全局单例；一切通过 `ctx` + 显式入参 |

---

## 6. 领域模型

### 6.1 七张表（核心资产）

> 为什么是 7 张：常见实现只做 3 张（分销员/佣金/提现）。缺的 4 张 —— **绑定表、流水表、规则版本表、冻结期快照** —— 恰恰是出事的地方。

#### ① `dist_agent` — 分销员

| 字段 | 类型 | 说明 |
|---|---|---|
| `id` | BIGINT PK | |
| `tenant_id` | BIGINT | 预留 |
| `user_id` | BIGINT | 外部用户 ID（**库不拥有用户表**） |
| `parent_id` | BIGINT | 直接上级 `user_id`，0=顶级 |
| `path` | VARCHAR(512) | 物化路径，如 `/1/12/135/`，用于快速查团队 |
| `depth` | INT | 层级深度，1=顶级 |
| `status` | TINYINT | 0=未激活 1=正常 2=禁用 |
| `join_type` | TINYINT | 0=无门槛 1=需审核 2=付费 |
| `rate_override_bp` | INT NULL | 个体费率覆盖（万分比），NULL=用全局 |
| `created_at` / `updated_at` | BIGINT | |

索引：`UNIQUE(tenant_id, user_id)`、`INDEX(tenant_id, parent_id)`、`INDEX(tenant_id, path)`

#### ② `dist_binding` — 绑定/归因

| 字段 | 类型 | 说明 |
|---|---|---|
| `id` / `tenant_id` | | |
| `buyer_user_id` | BIGINT | 被绑定的买家 |
| `agent_user_id` | BIGINT | 归属的分销员 |
| `bind_type` | TINYINT | 1=首次点击 2=末次点击 |
| `source` | TINYINT | 1=链接 2=邀请码 3=人工 4=导购 |
| `source_ref` | VARCHAR(128) | 邀请码 / 推广位 ID |
| `bound_at` | BIGINT | |
| `expire_at` | BIGINT | 0=永久 |
| `is_active` | TINYINT | 是否生效 |

> **为什么单独成表**：绑定期限、抢客保护、换绑冷静期都需要**改历史**，塞进订单表就改不了。

#### ③ `dist_commission` — 佣金单（**不可变**）

| 字段 | 类型 | 说明 |
|---|---|---|
| `id` / `tenant_id` | | |
| `idem_key` | VARCHAR(128) | **幂等键**，`UNIQUE(tenant_id, idem_key)` |
| `order_id` / `order_item_id` | VARCHAR | |
| `buyer_user_id` / `agent_user_id` | BIGINT | |
| `layer` | TINYINT | 1/2/3 |
| `role` | TINYINT | 1=推广员 2=销售员（可选） |
| `base_amount` | BIGINT | **计佣基数快照**（已扣运费/优惠/退款） |
| `rate_bp` | INT | **费率快照**（万分比） |
| `amount` | BIGINT | 佣金（分），**可为负**（冲正） |
| `state` | TINYINT | 见 §7.1 |
| `freeze_days` | INT | **冻结期快照** ⭐ |
| `available_at` | BIGINT | 可提现时点 = 收货时间 + `freeze_days` |
| `rule_version` | BIGINT | 命中的规则版本号 |
| `rule_snapshot` | TEXT | 命中规则的 JSON 快照（可解释性） |
| `reverse_of` | BIGINT | 冲正时指向原佣金单（**v0.3 引入**，v0.1 不含该字段，避免恒为零的空壳字段） |
| `accrue_at` / `settle_at` | BIGINT | |

⭐ **`freeze_days` 快照是刻意的**：Golershop 用"全局配置 + 收货时间戳"在查询时推导可提现时点，改配置会**污染历史订单**。本库把冻结期在入账时快照下来，历史永不受配置变更影响。

#### ④ `dist_account` — 账户

`frozen`（待结算） / `available`（可提现） / `withdrawing`（提现中） / `withdrawn`（已提现） / `total`（累计收益，只增） / `version`（乐观锁）

约束：`CHECK (frozen >= 0 AND available >= 0 AND withdrawing >= 0)`

#### ⑤ `dist_ledger` — 资金流水（复式，**只追加**）

| 字段 | 说明 |
|---|---|
| `account_user_id` | |
| `biz_type` | 1=入账 2=结算 3=冲正 4=提现冻结 5=提现成功 6=提现驳回 7=人工调整 |
| `biz_id` | 关联业务 ID（佣金单/提现单） |
| `delta_frozen` / `delta_available` / `delta_withdrawing` / `delta_withdrawn` | 本次变动 |
| `after_frozen` / `after_available` / `after_withdrawing` / `after_withdrawn` | 变动后余额 |
| `remark` / `created_at` | |

> **明细不是账。** 没有这张表，对账时你会死。任何余额变动必须能由流水反推。

#### ⑥ `dist_withdraw` — 提现单

`idem_key`（打款幂等）/ `amount` / `fee` / `real_amount` / `channel` / `account_info`（脱敏）/ `state`（§7.2）/ `fail_reason` / `tax_amount`（预留代扣税）/ `invoice_no` / `operator` / `applied_at` / `audited_at` / `paid_at`

#### ⑦ `dist_rule_version` — 规则版本

`version`（自增）/ `rules_json`（规则快照）/ `effective_from` / `created_by` / `created_at`

> 改佣金率必须产生新版本并带 `effective_from`，否则"为什么是这个数"永远解释不清。

---

## 7. 状态机（内核中唯一写死的部分）

### 7.1 佣金单状态

```
                    ┌── 风控命中 ──> FROZEN ──申诉通过──> PENDING
                    │                  └──申诉驳回──> VOID
PENDING ──收货+冻期满──> SETTLED
   │                       │
   └────订单退款────> REVERSED <───┘（生成负额冲正记录，原记录不改）

> **`WITHDRAWN` 已移除（v0.2.0，ADR-044）。** 原设计里 `SETTLED ──提现──> WITHDRAWN`，
> 但提现是**针对账户的一笔金额**，而可提现桶里的钱在许多已结算佣金之间是**同质的**——
> 把某次打款归因到哪几条佣金上是任意的，没有任何不变量需要它。
> 更糟的是该状态的唯一使用者拿它来**跳过**退款扣回，那会悄悄丢掉平台的债权，
> 与 ADR-042 的欠款策略正好相反。现在「已出账多少」由账户的 `withdrawn` 桶、
> 提现单与提现流水回答，并由不变量 I4 对账。
```

| 状态 | 值 | 含义 |
|---|---|---|
| `PENDING` | 0 | 待结算（已入账，进入 `frozen`） |
| `SETTLED` | 1 | 已结算（进入 `available`） |
| ~~`WITHDRAWN`~~ | ~~2~~ | **已移除**（v0.2.0）。数值 2 被**退役而非复用**：状态以整数持久化，重编号会让已有数据库读错自己的行 |
| `REVERSED` | 3 | 已冲正 |
| `FROZEN` | 4 | 风控冻结 |
| `VOID` | 5 | 作废 |

**非法迁移一律返回 `ErrIllegalTransition`，不做任何猜测。**

### 7.2 提现单状态

```
APPLIED ─审核通过─> APPROVED ─打款─> PAID
   │                   │              │
   └──驳回──> REJECTED  └──失败──> PAY_FAILED ──重试（幂等）──> PAID
```

`APPLIED(0) / APPROVED(1) / REJECTED(2) / PAY_FAILED(3) / PAID(4)`

---

## 8. 事件契约

**全部是普通 Go struct —— 不用 protobuf、不用 codegen、不用反射。**（R7）

```go
type OrderPaidEvent struct {
    TenantID   int64
    OrderID    string
    BuyerID    int64
    PaidAmount int64        // 实付金额（分），已扣运费/平台券
    Items      []OrderItem  // 可选：SKU 级分佣需要
    PaidAt     time.Time
}

type OrderReceivedEvent struct {
    TenantID   int64
    OrderID    string
    ReceivedAt time.Time
}

type OrderRefundedEvent struct {
    TenantID     int64
    OrderID      string
    RefundAmount int64       // 本次退款金额（分）
    IsFull       bool        // 是否整单全退
    ItemIDs      []string    // 退哪些 SKU（部分退时用于按比例冲正）
    RefundedAt   time.Time
}

type OrderClosedEvent struct { TenantID int64; OrderID string; ClosedAt time.Time }
```

退款事件的三种互斥表达（`IsFull` / `Items` / `Amount`）与**必填的** `IdemKey` 见 event.go。

**幂等语义（对开发者最重要的承诺）**：上述每个事件都可以**安全地重复投递**。库内部从事件字段派生 `idem_key`，重复投递是 no-op。**开发者不需要自己设计去重。**

---

## 9. 对外 API

### 9.1 核心接口

`Ledger` 是一个具体类型而不是接口——它只有一种实现，抽象成接口只会让调用方
多写一层包装。可替换的是它依赖的 `Store`、`RateResolver`、`EligibilityChecker`。

```go
// 事件入口：全部幂等，可安全重试
func (l *Ledger) OnOrderPaid(ctx context.Context, ev OrderPaidEvent) (AccrueResult, error)
func (l *Ledger) OnOrderReceived(ctx context.Context, ev OrderReceivedEvent) (ReceiveResult, error)

// 退款冲正（幂等，需携带调用方的退款单号）
func (l *Ledger) OnOrderRefunded(ctx context.Context, ev OrderRefundedEvent) (RefundResult, error)

// 风控
func (l *Ledger) FreezeCommission(ctx context.Context, tenantID, commissionID int64, reason string) (Commission, error)
func (l *Ledger) UnfreezeCommission(ctx context.Context, tenantID, commissionID int64, reason string) (Commission, error)
func (l *Ledger) VoidCommission(ctx context.Context, tenantID, commissionID int64, reason string) (Commission, error)

// 心跳：推进所有时间驱动的状态（冻结到期 → 结算）
// 处理时间一律来自注入的 Clock，不接受调用方传时间（见 ADR-023）
func (l *Ledger) Maintain(ctx context.Context) (MaintainResult, error)

// 关系链
func (l *Ledger) BindAgent(ctx context.Context, req BindAgentRequest) (Agent, error)
func (l *Ledger) BindBuyer(ctx context.Context, req BindBuyerRequest) (Binding, error)

// 查询
func (l *Ledger) Agent(ctx context.Context, tenantID, userID int64) (Agent, error)
func (l *Ledger) Balance(ctx context.Context, tenantID, userID int64) (Account, error)
func (l *Ledger) CommissionsByOrder(ctx context.Context, tenantID int64, orderID string) ([]Commission, error)
func (l *Ledger) CommissionsByAgent(ctx context.Context, tenantID, userID int64, q CommissionQuery) ([]Commission, error)
func (l *Ledger) LedgerEntries(ctx context.Context, tenantID, userID int64, q LedgerQuery) ([]LedgerEntry, error)

// 运维
func (l *Ledger) SelfCheck(ctx context.Context, tenantID int64) (Report, error)
func (l *Ledger) Rules() Rules
func (l *Ledger) Now() time.Time
func (l *Ledger) Close() error
```

### 9.2 构造函数

```go
led, err := distledger.New(distledger.Config{
    Store:  memory.New(),              // 或 mysqlstore.Open(db)
    Rules:  distledger.DefaultRules(), // 或结构化配置 / 自定义实现
    Clock:  distledger.SystemClock(),  // 测试可注入 ManualClock
    Logger: slog.Default(),
})
```

配置不自洽时 `New` 直接返回错误，而不是"尽力而为"：带着错误规则跑起来的结果
是发出错误的钱，而那不可撤销。

### 9.3 规则层三段式（配置优先，接口兜底）

```go
// ① 零配置：默认规则（保守，费率 0，跑通链路但不发钱）
distledger.DefaultRules()

// ② 结构化配置：覆盖 95% 的需求
distledger.Rules{
    Levels:       2,             // 1..3
    RateBP:       []int{500, 200}, // 5% / 2%（万分比）
    FreezeDays:   7,             // 收货后 7 天可提现
    MinWithdraw:  10000,         // 起提 100 元
    FeeRateBP:    0,
    SelfPurchase: false,
    AllowNegative: false,
}

// ③ 接口兜底：配置表达不了的才实现接口
type Calculator interface {
    Compute(ctx context.Context, in CalcInput) ([]CommissionDraft, error)
}
type Eligibility interface {
    CanJoin(ctx context.Context, userID int64) (bool, string)
    CanEarn(ctx context.Context, agentID int64) (bool, string)
}
type Binder interface {
    Bind(ctx context.Context, in BindInput) (Binding, error)
}
```

> **原则**：能配置的先配置，配置表达不了的才实现接口。**接口是最后手段，不是默认姿势。**

---

## 10. ⭐ 快速接入设计（本章是本次 PRD 的重点）

> 便捷性不是"文档写得好"，而是**把接入成本拆成可度量的关卡，每关都设计一个明确的解法**。

### 10.1 接入成本模型：7 道关卡

| 关卡 | 开发者的真实障碍 | 本库的解法 | 目标耗时 |
|---|---|---|---|
| **L1 认知** | 不知道要理解多少概念 | 只要求懂 3 个词：**订单、分销员、佣金单**。README 3 屏内讲完 | 2 min |
| **L2 跑起来** | 要装 DB、建表、配环境 | **内存 Store + `go run` 零依赖**，不需要任何外部服务 | **30 s** |
| **L3 建表** | 写迁移脚本、对字段 | `mysqlstore.Migrate(ctx, db)` 幂等建表；同时提供 `schema.sql` 供手工/迁移工具使用 | 1 min |
| **L4 接线** | 自己的订单系统怎么通知库 | **2 个入口函数 + 纯 struct 事件**；无需 codegen、无需继承、无需改订单表 | 20 行 |
| **L5 定规则** | 佣金怎么算 | 三段式：默认 → 结构化配置 → 实现接口（§9.3） | 5 行 |
| **L6 推进时间** | 冻结期/结算谁来跑 | **一个 `Maintain(ctx)`**，挂到用户任意 cron；也提供内置 `Loop`。用户不需要知道内部有几个阶段 | 1 行 |
| **L7 出金** | 没有支付资质 | `PayoutChannel` 接口 + 内置 `ManualChannel`（人工打款）/ `MockChannel`（测试） | 1 个接口 |

**设计判据**：每道关卡都必须在 **≤2 分钟** 内通过，否则视为设计缺陷。

### 10.2 八条便捷性机制（每条都是刻意的取舍）

| # | 机制 | 为什么刻意这么做 | 代价（我们接受的） |
|---|---|---|---|
| **M1** | **双 Store：`memory` 与 `mysql` 接口完全同构** | 让"第一次体验"零门槛。开发者不必为了看一眼佣金数字先装 MySQL | 维护两套实现的成本 |
| **M2** | **`DefaultRules()` 开箱可用** | 不写任何规则代码也能跑出结果 | 默认费率 0（R2 优先于"看起来爽"） |
| **M3** | **幂等键由库生成** | 开发者可以**无脑重试**，不需要自己设计去重逻辑 | 库要保证 `idem_key` 派生规则稳定 |
| **M4** | **单一心跳 `Maintain()`** | 所有时间驱动逻辑收敛为一个函数，用户不必理解内部状态机阶段 | 内部复杂度被封装，粒度不可调 |
| **M5** | **`SelfCheck()` 自检** | 接入排障的核心工具：一次调用回答"表建了吗/不变量成立吗/配置自洽吗" | 需要额外实现检查逻辑 |
| **M6** | **5 个递进 example，每个都能 `go run`** | 学习路径 = 复制粘贴 + 改一行 | example 需与 API 同步维护 |
| **M7** | **零框架绑定** | 不注册路由、不接管生命周期、不依赖启动顺序 | 不提供"一键集成"，需用户显式调用 |
| **M8** | **测试友好** | `WithClock()` 注入时间（冻结期测试不用 `sleep`）；memory store 让单测不需要 DB | 所有时间必须走注入的 Clock，禁止直接 `time.Now()` |

### 10.3 目标 Quickstart（北极星 API，作为 G1/G2 的验收样例）

**已交付**：`examples/01-quickstart` 就是这个样例，`go run` 即可运行。

```go
led, _ := distledger.New(distledger.Config{
	Store: memory.New(),
	Clock: distledger.NewManualClock(time.Now()),
	Rules: distledger.Rules{
		Levels: 2, RateBP: []distledger.Rate{500, 200}, FreezeDays: 7,
	},
})

// 关系链：A(1001) ← B(1002)
led.BindAgent(ctx, distledger.BindAgentRequest{
	TenantID: 0, UserID: 1001, Status: distledger.AgentActive,
})
led.BindAgent(ctx, distledger.BindAgentRequest{
	TenantID: 0, UserID: 1002, ParentID: 1001, Status: distledger.AgentActive,
})
led.BindBuyer(ctx, distledger.BindBuyerRequest{
	TenantID: 0, BuyerUserID: 1003, AgentUserID: 1002, Source: distledger.SourceLink,
})

// C(1003) 下单 199 元
res, _ := led.OnOrderPaid(ctx, distledger.OrderPaidEvent{
	TenantID: 0, OrderID: "ORD-1", BuyerUserID: 1003,
	PaidAmount: distledger.Money(19900), PaidAt: clock.Now(),
})
// len(res.Commissions) == 2，且 res.Skipped 会解释每一级为什么没发钱

// 收货 → 冻结 7 天 → 推进时间 → 结算
led.OnOrderReceived(ctx, distledger.OrderReceivedEvent{
	TenantID: 0, OrderID: "ORD-1", ReceivedAt: clock.Now(),
})
clock.Advance(8 * 24 * time.Hour)
led.Maintain(ctx, time.Time{})

bal, _ := led.Balance(ctx, 0, 1001) // 佣金已从「待结算」进入「可提现」
```

**三个必须知道的约定**（都来自实现期的安全审查，见 ADR-018）：

1. `PaidAt` 必填，库不会用当前时间代替 —— 否则重放会被用来事后抢单。
2. 归因只在支付时刻成立：绑定必须**早于**支付时间。
3. 冻结期在入账时快照，改全局配置不影响历史。

### 10.4 五个递进示例（学习路径）

| 示例 | 内容 | 教会什么 |
|---|---|---|
| `examples/01-quickstart` | 内存 store + 一级分佣 | 最小可用 |
| `examples/02-levels` | 三级分销 + 等级差异费率 + 个体费率覆盖 | 规则配置 |
| `examples/03-refund` | 全退 / 部分退 / 已提现后追回 | **最容易出事的场景** |
| `examples/04-withdraw` | 提现申请 → 审核 → 人工打款 | 出金链路 |
| `examples/05-mysql-adapter` | MySQL store + 接自己的订单系统 | **真实接入**（目标 ≤50 行） |

### 10.5 反模式：我们**故意不做**的"更便捷"

| 不做 | 为什么 | 替代方案 |
|---|---|---|
| `AutoMigrateOnInit()` 隐式建表 | 生产环境隐式 DDL 是灾难，权限与变更不可控 | 显式 `Migrate()` + `schema.sql` |
| 反射自动注册规则 | 调试地狱；出问题时调用链断在反射里 | 显式传入 `Rules` |
| `AddDistributionToYourShop()` 一键改造 | 必然破坏用户架构，且无法回滚 | 事件驱动 + example |
| 强绑定 GORM | 绑架用户的数据访问层 | `Store` 接口；GORM 适配器放可选子包 |
| 内建 HTTP handler | 与用户的 Gin/Echo/Fiber/Kratos 打架 | 文档给 handler 示例，不进库 |
| 内建定时任务调度 | 重复造轮子，且和用户的调度器冲突 | 单一 `Maintain()`，用户自己挂 cron |
| 自动识别"哪笔订单该分佣" | 归因需要业务上下文，魔法必然出错 | 显式 `OrderPaidEvent` + 显式绑定 |

### 10.6 接入完成度自检清单（写进 README）

```
□ go get github.com/darkinno-tech/distledger
□ go run ./examples/01-quickstart          → 看到佣金数字（30 秒）
□ Store 换成 mysqlstore + 调 Migrate()      → 表已建好
□ 订单支付成功后调 OnOrderPaid()            → 佣金出现在 dist_commission
□ 收货后调 OnOrderReceived()                → available_at 已写入
□ cron 每小时调 Maintain()                  → 冻结到期自动结算
□ 提现：RequestWithdraw → Approve → MarkWithdrawPaid
□ led.SelfCheck() 全部 PASS                 → 接入完成
```

---

## 11. 配置与安全默认值

> **v0.1 只实现了下表中标注 ✅ 的项。** 未实现的项不会被"预留"成空壳配置——
> 用户设了却不生效的配置比缺少功能更危险（见 ADR-022）。

| 配置项 | 默认值 | 说明 | 为什么这个默认 |
|---|---|---|---|
| ✅ `Levels` | `2` | 分销层级数 | 合规保守（≤3 是法律红线，默认留一级余量） |
| ✅ `RateBP` | `[]Rate{0, 0}` | 各级费率（万分比） | **默认不发钱**（R2） |
| ✅ `FreezeDays` | `7` | 收货后 N 天可提现 | 覆盖售后窗口；入账时**快照** |
| ✅ `MinWithdraw` | `10000` | 起提门槛（100 元） | 避免小额打款成本倒挂 |
| ✅ `FeeRateBP` | `0` | 提现手续费 | 不额外收钱 |
| ⏳ `SelfPurchase` | `false` | 自购是否分佣 | 自购返利是刷单温床，默认关 |
| ✅ `AllowNegative` | `false` | 「已出账后追回」的欠款策略 | 默认拒绝并报出缺口，不猜；开启则允许可提现桶为负，后续收益自动抵扣（ADR-042） |
| ⏳ `AutoPayout` | `false` | 是否自动打款 | 默认人工审核闸门 |
| ⏳ `BindType` | `首次点击` | 归因方式（v0.1 固定为首位优先，不可配） | 防抢客优先于灵活 |
| ✅ `BindExpireDays` | `0`（永久） | 绑定有效期 | 显式配置才过期 |

---

## 12. 不变量与测试策略

### 12.1 三条不变量（v1.0 必须证明）

| 编号 | 不变量 | 违反后果 |
|---|---|---|
| **I1** | `SUM(dist_ledger.delta_*) == dist_account.*`（余额永远等于流水之和） | 账实不符 |
| **I2** | 每个计算单位的佣金合计 ≤ 该单位基数 × `MaxAllocatableBP`，且每条佣金都能双向追溯到入账流水 | 平台每单亏损 / 记了佣金却没动钱 |
| **I3** | 冲正累计额 == 冲正明细之和；`0 <= 累计额 <= 原始金额`；状态与累计额自洽；账户 `TotalEarned`/`TotalReversed` == 该分销员佣金汇总 | 负数发钱 / 少冲正 / 已冲正仍在结算 |

### 12.2 测试策略（**先写测试，再写实现**）

1. **Property-based**：随机生成 10 万条事件序列（支付/收货/退款/部分退/重复投递/乱序），断言 I1–I3 恒成立
2. **幂等测试**：同一事件重复投递 N 次，账务结果与 1 次完全一致
3. **时间旅行**：用 `WithClock()` 注入时间，覆盖冻结期边界（`available_at - 1s` / `+1s`）
4. **并发测试**：`-race` 下并发 `Maintain()` + 并发退款，验证乐观锁与行锁
5. **对账测试**：`SelfCheck()` 在人为破坏数据后必须能报出问题

---

## 13. 里程碑与版本规划

| 版本 | 范围 | 验收 |
|---|---|---|
| **v0.3** ✅ 已交付 | 退款冲正（整单/明细/部分、逐批累加）+ 风控冻结/解冻/没收 + 不变量 I3 + 对抗性审查修复 | 退款属性测试通过；三方对账恒成立 |
| **v0.1** ✅ 已交付 | 内存 store + 关系链 + 归因 + 多级分佣 + 幂等键 + 冻结快照 + `Maintain` 结算 + I1/I2 自检 | `examples/01` 可跑；`-race` 全绿；覆盖率 88.7%/93.5%/92.9% |
| **v0.2** | 规则层扩展（等级费率、SKU 级覆盖）+ `examples/02` | 规则可插拔验证 |
| **v0.3** | 退款冲正（全退/部分退/已结算追回）+ I3 | `examples/03` 可跑 |
| **v0.4** | 可移植 SQL 内核（`store/sql`）+ MySQL / PostgreSQL / SQLite 三个方言 + `Migrate` + `schema.sql` | 多实例生产可用；ADR-030 的重试契约首次被真实数据库执行 |
| **v0.5** | 提现链路 + `PayoutChannel` + `ManualChannel` | `examples/04` 可跑 |
| **v0.6** | `SelfCheck` + `Maintain` + 可观测性钩子 | 接入自诊可用 |
| **v1.0** | API 冻结 + 文档完整 + property-based 全绿 | 三个不变量通过 10 万次随机序列 |

> **API 稳定性承诺**：v1.0 前允许破坏性变更，但必须写进 `CHANGELOG.md`；v1.0 后遵循语义化版本。

---

## 14. 仓库结构与工程规范

```
distledger/
├── go.mod                    # 零第三方依赖（require 为空）
├── LICENSE                   # MIT
├── README.md                 # 面向开发者：3 屏内讲完 + Quickstart
├── PRD.md                    # 本文档
├── CHANGELOG.md
│
├── doc.go                    # 包文档与设计铁律速览
├── money.go                  # Money / Rate / Rounding + 防溢出运算
├── errors.go                 # 哨兵错误 + 类型化错误（FieldError / TransitionError / ConflictError）
├── clock.go                  # Clock 抽象 + ManualClock（禁止直接 time.Now()）
├── keys.go                   # UserKey / OrderKey + 长度上限
├── idem.go                   # 幂等键派生（长度前缀哈希）
├── model.go                  # 领域类型：Agent / Binding / Commission / Account / LedgerEntry
├── state.go                  # 佣金状态迁移表（内核中唯一写死的部分）
├── rule.go                   # Rules / RateResolver / EligibilityChecker
├── event.go                  # 事件与结果（纯 struct，无 codegen）
├── store.go                  # 持久化端口：Store / Reader / Writer / Tx
├── ledger.go                 # 门面：New / BindAgent / BindBuyer / 查询
├── accrual.go                # 分佣引擎
├── settle.go                 # 冻结、收货、Maintain 结算
├── reversal.go               # 退款冲正（按累计目标值）
├── risk.go                   # 风控：冻结 / 解冻 / 没收
├── selfcheck.go              # 不变量 I1 / I2 / I3 校验
│
├── internal/safemath/        # 128 位乘除与溢出检测原语
│
├── store/                    # 存储层：端口在根包，实现可替换
│   ├── memory/               # 内存实现（事务回滚 / 键集分页 / 到期索引 / 租户登记）
│   └── sql/                  # ⏳ v0.4 可移植 SQL 内核，通过 Dialect 拼装语句
├── store/mysql/              # ⏳ v0.4 仅提供 Dialect 与 Open
├── store/postgres/           # ⏳ v0.4
├── store/sqlite/             # ⏳ v0.4
│
├── examples/01-quickstart/   # 零依赖可运行示例
└── docs/
    ├── design-decisions.md   # 34 条 ADR：每个"刻意为之"的取舍与代价
    └── invariants.md         # ⏳ 形式化描述（当前以 selfcheck.go 的测试为准）
```

### 工程规范

- **Go 版本**：`go 1.22+`（使用 `slog`、`context`、泛型）
- **零依赖**：核心包不引入任何第三方库；MySQL 驱动由用户在 `store/mysql` 侧提供（`database/sql` 裸写 SQL，不引 ORM）
- **错误处理**：哨兵错误 + `errors.Is/As`；不使用 panic 处理业务错误
- **命名**：遵循 Go 官方风格；对外导出标识符必须有 doc comment
- **提交信息**：`feat:` / `fix:` / `docs:` / `test:` / `refactor:` 前缀（Conventional Commits）
- **测试**：核心逻辑测试覆盖率 ≥ 90%；状态机与不变量必须 100% 覆盖
- **CI**：`go vet` + `golangci-lint` + `go test -race` + 不变量随机测试

---

## 15. 许可、合规与治理

### 15.1 许可：MIT，以及"不商用"的准确含义

本项目采用 **MIT License**（全文见 `LICENSE`）。

> ⚠️ **必须澄清的一点**：MIT 协议**明确授予任何人商业使用权**。因此本项目的"不商用"指的是——
> **项目发起方不以盈利为目的**（不做商业版、不卖授权、不做 open core 裁剪），
> **而不是限制使用者商用**。使用者可以自由地把它用在自己的商业产品里。
>
> 如果你真正想要的是"禁止他人商用"，那就不能选 MIT，应改用 PolyForm Noncommercial / BSL 等非 OSI 许可 —— **但那就不再是"开源项目"了**。本 PRD 按"MIT + 项目方非商业"执行。

### 15.2 分销合规免责（必读）

分销在中国受《禁止传销条例》《刑法》第 224 条之一约束。本库**只提供账务能力，不提供合规判断**，使用者必须自行确保：

1. **层级 ≤ 3 级**（本库默认 2 级，配置上限硬编码为 3，防止运营误配第 4 级）
2. **计酬以真实商品成交为基数** —— 本库**刻意不提供**"按拉人头/注册人数计酬"的能力
3. **不收门槛费作为参与条件** —— `join_type=2`（付费加入）仅作状态标记，不参与计酬计算
4. **资金不得"二清"** —— 本库不碰资金流，出金必须走持牌通道或人工打款

> 一句话：**卖货分佣合法，拉人分钱违法。本库只帮前者。**

### 15.3 治理

- **范围守门人**：任何试图把"玩法"（团队计酬/区域分红/链动）加入核心的 PR，一律拒绝；只能作为 `Calculator` 的第三方实现
- **不承诺 SLA**：非商业项目，响应时间不保证
- **破坏性变更**：v1.0 前允许，须记入 CHANGELOG

---

## 16. 风险与缓解

| 风险 | 等级 | 缓解措施 |
|---|---|---|
| **通用性不足**（规则长尾覆盖不了） | 高 | 明确 Non-Goals；`Calculator`/`Eligibility`/`Binder` 三接口兜底；配置优先双轨 |
| **资金正确性责任** | 高 | 三条不变量 + property-based 测试 + 默认保守 + 免责声明 |
| **被误用于传销** | 高 | 层级硬上限 3；**不提供**按人头计酬；文档明确红线 |
| **接入方误用**（未走 `Maintain`、绕过状态机） | 中 | `SelfCheck()` 主动发现；非法迁移返回错误；文档清单 |
| **维护动力不足**（非商业项目） | 中 | 范围极小（7 表 / 2 状态机 / 3 接口）；不追求功能对齐商用产品 |
| **双 Store 实现漂移** | 中 | 同一套契约测试（contract test）跑在两个 store 上 |

---

## 17. 附录

### 17.1 术语表

| 术语 | 含义 |
|---|---|
| 归因 / 绑定 | 判定一笔订单归属于哪个分销员 |
| 计佣基数 | 参与佣金计算的金额（实付 − 运费 − 平台券 − 已退部分） |
| 冻结期 | 收货后到可提现之间的时间窗 |
| 冲正 | 退款后对已产生佣金的反向记账（新增负记录，不改原记录） |
| 幂等键 | 唯一确定一次账务动作的键，用于防重复入账 |
| 万分比（bp） | 费率的整数表示，5% = 500bp |
| 二清 | 平台代收货款后自行分发，属无证支付结算 |

### 17.2 与 Golershop 表设计的差异（刻意的改进）

| 维度 | Golershop（开源版） | distledger | 理由 |
|---|---|---|---|
| 冻结期 | 全局配置 + 收货时间戳，**查询时推导** | **入账时快照进佣金单** | 改配置不污染历史 |
| 金额类型 | `decimal` / `float64` | **`int64` 分** | 杜绝浮点误差 |
| 费率表示 | 百分比文本 | **万分比 `int`** | 无浮点 |
| 佣金记录 | 累计表 + 明细表 | 明细**不可变** + 复式流水表 | 可追溯、可对账 |
| 层级上限 | 配置项（含三级开关） | **硬编码 ≤3** | 合规兜底 |
| 归因信息 | 冗余在订单表 | 独立 `dist_binding` 表 + 订单表冗余 | 支持换绑/有效期 |

### 17.3 命名备选

`distledger`（当前） / `fxledger` / `commission-kernel` —— 选择 `distledger` 的理由：不绑定中文拼音（避免国际化障碍），且明确"ledger"这一本质。

---

*本文档为设计阶段 RFC，欢迎在 Issue 中讨论。核心约束（§5 铁律、§3.2 Non-Goals）在 v1.0 前不接受变更。*
