# distledger

> **Go 分销账务内核** — 把"分销的钱"算对、记清、可追溯的最小可复用内核。

[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)
[![Go Version](https://img.shields.io/badge/go-1.22%2B-blue.svg)](https://go.dev/)
[![Dependencies](https://img.shields.io/badge/dependencies-zero-brightgreen.svg)](#设计铁律)

`distledger` 只解决三件事：**关系链**、**佣金账**、**资金流水**。
规则（几级、多少比例、要不要门槛）全部外置为可插拔策略。

**它不是一个分销系统**，而是你自建分销系统时那块最容易写错、又最不该重写的底座。

> 📄 完整需求与设计见 **[PRD.md](PRD.md)** ｜ 关键决策与取舍见 **[docs/design-decisions.md](docs/design-decisions.md)**

---

## 当前实现状态

| 能力 | 状态 |
|---|---|
| 金额/费率安全运算（整数、防溢出、显式舍入、配额封顶） | ✅ v0.1 |
| 关系链（父子、深度上限、环检测、上级不可悄悄变更） | ✅ v0.1 |
| 归因绑定（首次优先、有效期、防抢客、防事后追认） | ✅ v0.1 |
| 多级分佣（含按 SKU 分佣、幂等键、可解释的跳过原因） | ✅ v0.1 |
| 冻结期快照 + `Maintain` 结算（状态机 + 乐观锁） | ✅ v0.1 |
| `SelfCheck` 不变量自检（I1 余额守恒 / I2 配额与引用完整性） | ✅ v0.1 |
| 内存 Store（事务回滚、键集分页、到期索引） | ✅ v0.1 |
| 退款冲正（整单全额 / 按明细 / 整单部分，逐批累加） | ✅ v0.3 |
| 风控冻结 / 解冻 / 没收（`Freeze` / `Unfreeze` / `Void`） | ✅ v0.3 |
| `SelfCheck` 不变量 I3（冲正累计额 ↔ 明细 ↔ 账户三方对账） | ✅ v0.3 |
| MySQL Store | ⏳ v0.4 |
| 提现链路与打款通道（含「已出账后追回」的欠款策略） | ⏳ v0.5 |

**v0.1 已可直接用于单进程部署与测试环境**；生产多实例部署请等 v0.4 的 MySQL Store。

---

## 为什么需要它

分销的业务规则是无限长尾（每家公司都不一样），但资金账务流对所有人**完全一样**：

```
订单归属某分销员 → 按规则算出若干级佣金 → 进冻结 → 到期可提现 → 退款扣回 → 出金
```

把长尾做进库 → 库必烂；把不变量做进库 → 库可复用。

**Go 生态目前没有可直接用的分销库**（市面开源分销都是 PHP/Java 电商系统的内置模块），而这段代码一旦写错，直接等于丢钱。

---

## 设计铁律

| 原则 | 含义 |
|---|---|
| **切分线** | 能改变"钱**流向**"的进内核；只改变"钱**数值**"的做规则。规则层永远不许改状态 |
| **默认不发钱** | 默认费率 0、默认 2 级、默认人工打款。宁可配置麻烦，不可默认错发 |
| **整数金额** | 一律 `int64` 分 + 万分比 `int`，全程禁止浮点 |
| **账本不可变** | 财务字段与流水只追加；冲正靠新增负记录。这条契约由 Store 端口的形状保证，不靠代码评审 |
| **失败安全** | 非法状态迁移返回错误而非猜测；不变量不成立时宁可让心跳失败并告警 |
| **零魔法** | 无 codegen、无反射注册、无全局单例 |
| **零强依赖** | `go.mod` 的 `require` 为空 |

---

## 快速开始（30 秒，不需要 MySQL）

```bash
git clone https://github.com/im10furry/distledger.git
cd distledger/examples/01-quickstart
go run .
```

输出（节选）：

```
③ 归因成功=true，产生 2 笔佣金
   第 1 级  分销员=1002  基数=199.00  费率=5%  佣金=9.95
   第 2 级  分销员=1001  基数=199.00  费率=2%  佣金=3.98
   重复投递：重放=true，新增佣金=0 笔（幂等生效）
④ 收货后 用户 1001：待结算=3.98 可提现=0.00 累计=3.98
⑤ 推进 8 天后结算 2 笔，合计 13.93
⑦ 自检：存储=memory 总体=true
   [PASS] I1: account balance equals sum of ledger deltas（检查 2 项）
   [PASS] I2: commission ledger linkage and allocation cap（检查 1 项）
```

最小可用代码：

```go
package main

import (
	"context"
	"fmt"
	"time"

	"github.com/im10furry/distledger"
	"github.com/im10furry/distledger/store/memory"
)

func main() {
	ctx := context.Background()
	clock := distledger.NewManualClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))

	led, err := distledger.New(distledger.Config{
		Store: memory.New(), // 内存 store，零外部依赖
		Clock: clock,
		Rules: distledger.Rules{
			Levels:     2,
			RateBP:     []distledger.Rate{500, 200}, // 一级 5%，二级 2%
			FreezeDays: 7,
		},
	})
	if err != nil {
		panic(err)
	}
	defer led.Close()

	const tenant = int64(0)

	// 关系链：A(1001) ← B(1002)
	_, _ = led.BindAgent(ctx, distledger.BindAgentRequest{
		TenantID: tenant, UserID: 1001, Status: distledger.AgentActive,
	})
	_, _ = led.BindAgent(ctx, distledger.BindAgentRequest{
		TenantID: tenant, UserID: 1002, ParentID: 1001, Status: distledger.AgentActive,
	})

	// B 邀请 C(1003)
	_, _ = led.BindBuyer(ctx, distledger.BindBuyerRequest{
		TenantID: tenant, BuyerUserID: 1003, AgentUserID: 1002, Source: distledger.SourceLink,
	})

	// C 下单 199 元
	res, _ := led.OnOrderPaid(ctx, distledger.OrderPaidEvent{
		TenantID: tenant, OrderID: "ORD-1", BuyerUserID: 1003,
		PaidAmount: distledger.Money(19900), PaidAt: clock.Now(),
	})
	fmt.Println("佣金笔数:", len(res.Commissions)) // 2

	// 收货 → 冻结 7 天 → 推进 8 天 → 结算
	_, _ = led.OnOrderReceived(ctx, distledger.OrderReceivedEvent{
		TenantID: tenant, OrderID: "ORD-1", ReceivedAt: clock.Now(),
	})
	clock.Advance(8 * 24 * time.Hour)
	_, _ = led.Maintain(ctx, time.Time{})

	bal, _ := led.Balance(ctx, tenant, 1001)
	fmt.Println("A 可提现:", bal.Available)
}
```

**核心 API 只有三个事件入口 + 一个心跳：**

| API | 用途 |
|---|---|
| `OnOrderPaid` | 订单支付成功 → 归因 → 分佣 → 入账（**幂等，可无脑重试**） |
| `OnOrderReceived` | 确认收货 → 用**快照的**冻结天数确定到账时间（**先到先得**） |
| `OnOrderRefunded` | 订单退款 → 按累计退款额冲正各级佣金（**幂等，需携带退款单号**） |
| `Maintain(ctx)` | 推进所有时间驱动的状态 —— 挂到你的 cron 即可 |

---

## 接入清单

```
□ go get github.com/im10furry/distledger
□ go run ./examples/01-quickstart          → 30 秒看到佣金数字
□ Rules 里显式配置 RateBP                  → 默认费率是 0（刻意，见 ADR-010）
□ 订单支付成功后调 OnOrderPaid()            → 佣金出现在「待结算」
□ 收货后调 OnOrderReceived()                → available_at 已写入
□ cron 每分钟调 Maintain()                  → 冻结到期自动结算
□ led.SelfCheck(ctx, tenant) 全部 PASS      → 接入完成
```

**接入已有系统的目标成本：新增代码 ≤ 50 行，不改订单表结构，不接管你的生命周期。**

---

## 七个「必须知道」的行为约定

它们都是刻意的，且各自防住了一类真实的资损或纠纷：

1. **`PaidAt` 必填。** 库不会用「当前时间」代替它。否则一次发生在绑定建立之前的重放，会被追认为有效归因 —— 相当于给"事后抢单"留了一道门（见 ADR-018）。
2. **归因只在支付时刻成立。** 绑定必须**在支付之前**已经存在且有效。订单支付后补建的绑定不会追认这笔收益。
3. **冻结期在入账时快照。** 之后修改全局 `FreezeDays` 不会改变已入账佣金的到账时间（见 ADR-005）。
4. **`OrderRefundedEvent.IdemKey` 必填。** 它是这次退款在你系统里的标识（通常是支付渠道的退款单号）。没有它，「同一次退款被重投」与「两次金额相同的真实退款」在数据上完全等价，任何实现都只能二选一地做错（见 ADR-024）。
5. **`Maintain` 不吃时间参数。** 处理时间一律来自注入的 `Clock`——允许调用方推进它，就等于允许调用方决定什么时候发钱（见 ADR-023）。
6. **`OrderID` 在租户内唯一且不可复用。** 订单级幂等以它为键，复用会让一笔真实的新订单被当成重放而丢弃（见 event.go 的字段说明）。
7. **自定义 `Store` 的重试必须先完全回滚再重跑。** 弱化这个定义会让引擎第二次执行时看到自己上次留下的半成品（见 ADR-030）。

---

## 数据模型

v0.1 落地 6 张逻辑表（内存 Store 实现）：

| 表 | 作用 |
|---|---|
| `dist_agent` | 分销员 + 关系链（父指针 + 深度） |
| `dist_binding` | 绑定 / 归因（首次优先、有效期、可审计换绑） |
| `dist_commission` | 佣金单，**财务字段不可变**，含费率/基数/冻结期**快照** |
| `dist_account` | 账户（待结算 / 可提现 / 提现中 / 已提现 / 累计） |
| `dist_ledger` | 资金流水，**复式只追加** |
| `dist_refund` | 退款凭证：每批次 × 每明细一行，含该明细的**累计退款额** |
| `dist_withdraw` | ⏳ v0.5 |

详见 [PRD.md §6](PRD.md#6-领域模型)。

---

## 三条不变量

这是本库存在的全部意义，也是 `SelfCheck` 每天该跑的东西：

| 编号 | 不变量 | 检查方式 |
|---|---|---|
| **I1** | 账户余额 == 流水增量之和（且最后一条流水的 `After*` 与账户一致，且无负余额） | `SelfCheck` 遍历账户与流水重新求和比对 |
| **I2** | 每条佣金都有对应的流水（双向引用完整），且每个计算单位分出的佣金不超过配额 | `SelfCheck` 双向枚举 + 配额校验 |
| **I3** | 冲正累计额 == 冲正明细之和，且账户的毛额/冲正额 == 该分销员佣金的汇总 | `SelfCheck` 三方对账（这正是退款场景下最容易出错的地方） |

> 资金桶的定义只有一处（`Bucket`），负余额守卫、I1 与流水构造都遍历它；
> 一个反射测试强制 `Account` 上新增的每个金额字段都必须被归类为「桶」或「显式豁免并写明由哪条不变量覆盖」——
> 不变量不会因为新增字段而悄悄失效（见 ADR-029）。

**测试策略**：不变量优先。两条随机属性测试——一条覆盖支付/收货/结算（40 组种子 × 220 次操作），
一条覆盖退款（25 组种子 × 200 次操作，混合分批退款、**故意重投同一个退款单号**、以及使用新单号的真实二次退款）。
另有并发投递、并发退款、事务重试契约与跨租户越权测试，全部在 `-race` 下运行，每组结束后都要求完整自检通过。

---

## 它不做什么

`distledger` **刻意不做**：推广链接/海报/排行榜/收益中心 UI、分销员申请审核流程、支付与分账、团队计酬/区域分红/链动等玩法、**按拉人头或注册人数计酬**、HTTP 路由与 Admin 后台、ORM/MQ/框架绑定。

理由见 [PRD.md §3.2 Non-Goals](PRD.md#32-非目标non-goals本清单是需求边界的法律) —— **这份清单是需求边界的法律。**

---

## ⚠️ 合规声明

分销在中国受《禁止传销条例》《刑法》第 224 条之一约束。本库**只提供账务能力，不提供合规判断**。

使用者必须自行确保：层级 ≤ 3 级 · 计酬以真实成交为基数 · 不以缴费作为参与条件 · 资金不走"二清"。

库层面已经做的兜底：`Levels` 上限**硬编码为 3**（不可配置），且**不提供**任何按拉人头/注册人数计酬的能力。

> **卖货分佣合法，拉人分钱违法。本库只帮前者。**

---

## 许可

[MIT](LICENSE) —— 可自由用于商业产品。

本项目的"非商业"指**项目发起方不以盈利为目的**（不做商业版、不卖授权、不做 open core 裁剪），**不是限制使用者商用**。
如需"禁止他人商用"，须改用非 OSI 许可（如 PolyForm Noncommercial），那就不再是开源项目了。

---

## 参与贡献

非商业开源项目，不承诺响应时间。欢迎以下类型的贡献：

- ✅ 不变量测试、边界用例、文档修正、示例补充
- ✅ 新的 `Store` 实现（PostgreSQL / SQLite / Redis）
- ✅ 新的 `PayoutChannel` 实现（微信 / 支付宝 / 银行卡）
- ❌ 向核心加入"玩法"（团队计酬 / 区域分红 / 链动）—— 这类能力只能作为第三方 `RateResolver` 实现存在
