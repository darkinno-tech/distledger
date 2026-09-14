# distledger

> **Go 分销账务内核** — 把"分销的钱"算对、记清、可追溯的最小可复用内核。

[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)
[![Go Version](https://img.shields.io/badge/go-1.22%2B-blue.svg)](https://go.dev/)
[![Dependencies](https://img.shields.io/badge/dependencies-zero-brightgreen.svg)](#设计原则)

`distledger` 只解决三件事：**关系链**、**佣金账**、**资金流水**。
规则（几级、多少比例、要不要门槛）全部外置为可插拔策略。

**它不是一个分销系统**，而是你自建分销系统时那块最容易写错、又最不该重写的底座。

> 📄 完整需求与设计见 **[PRD.md](PRD.md)** ｜ 关键决策记录见 **[docs/design-decisions.md](docs/design-decisions.md)**

---

## 为什么需要它

分销的业务规则是无限长尾（每家公司都不一样），但资金账务流对所有人**完全一样**：

```
订单归属某分销员 → 按规则算出若干级佣金 → 进冻结 → 到期可提现 → 退款扣回 → 出金
```

把长尾做进库 → 库必烂；把不变量做进库 → 库可复用。

**Go 生态目前没有可直接用的分销库**（市面开源分销都是 PHP/Java 电商系统的内置模块），而这段代码一旦写错，直接等于丢钱。

---

## 设计原则

| 原则 | 含义 |
|---|---|
| **切分线** | 能改变"钱**流向**"的进内核；只改变"钱**数值**"的做规则。规则层永远不许改状态 |
| **默认不发钱** | 默认费率 0、默认 2 级、默认人工打款。宁可配置麻烦，不可默认错发 |
| **整数金额** | 一律 `int64` 分 + 万分比 `int`，全程禁止浮点 |
| **账本不可变** | 明细与流水只追加；冲正靠新增负记录 |
| **零魔法** | 无 codegen、无反射注册、无全局单例 |
| **零强依赖** | 核心包不引入任何第三方库 |

---

## 快速开始（30 秒，不需要 MySQL）

```bash
git clone https://github.com/im10furry/distledger.git
cd distledger/examples/01-quickstart
go run .
```

```go
package main

import (
	"context"
	"fmt"
	"time"

	"github.com/im10furry/distledger"
	memstore "github.com/im10furry/distledger/store/memory"
)

func main() {
	ctx := context.Background()

	led, _ := distledger.New(distledger.Config{
		Store: memstore.New(), // 内存 store，零外部依赖
		Rules: distledger.Rules{Levels: 2, RateBP: []int{500, 200}, FreezeDays: 7},
	})

	// A(1001) 邀请 B(1002)，B 推荐 C(1003) 下单
	_ = led.BindAgent(ctx, 0, 1001, 0)
	_ = led.BindAgent(ctx, 0, 1002, 1001)

	now := time.Now()
	res, _ := led.OnOrderPaid(ctx, distledger.OrderPaidEvent{
		OrderID: "ORD-1", BuyerID: 1003, PaidAmount: 19900, PaidAt: now,
	})
	fmt.Println("产生佣金笔数:", len(res.Commissions)) // 2

	// 收货 → 7 天后解冻
	_ = led.OnOrderReceived(ctx, distledger.OrderReceivedEvent{OrderID: "ORD-1", ReceivedAt: now})
	_, _ = led.Maintain(ctx, now.AddDate(0, 0, 8))

	bal, _ := led.Balance(ctx, 0, 1001)
	fmt.Println("可提现:", bal.Available)

	// 退款 → 自动冲正
	_, _ = led.OnOrderRefunded(ctx, distledger.OrderRefundedEvent{
		OrderID: "ORD-1", RefundAmount: 19900, IsFull: true, RefundedAt: now,
	})
	bal, _ = led.Balance(ctx, 0, 1001)
	fmt.Println("退款后可提现:", bal.Available)
}
```

**核心 API 只有两个入口 + 一个心跳：**

| API | 用途 |
|---|---|
| `OnOrderPaid` / `OnOrderReceived` / `OnOrderRefunded` | 告诉库"业务发生了什么"（**全部幂等，可无脑重试**） |
| `Maintain(ctx, now)` | 推进所有时间驱动的状态（冻结到期、结算、超时）—— 挂到你的 cron 即可 |

---

## 接入清单

```
□ go get github.com/im10furry/distledger
□ go run ./examples/01-quickstart          → 30 秒看到佣金数字
□ Store 换成 mysqlstore + 调 Migrate()      → 表已建好
□ 订单支付成功后调 OnOrderPaid()            → 佣金出现在 dist_commission
□ 收货后调 OnOrderReceived()                → available_at 已写入
□ cron 每小时调 Maintain()                  → 冻结到期自动结算
□ 提现：RequestWithdraw → Approve → MarkWithdrawPaid
□ led.SelfCheck() 全部 PASS                 → 接入完成
```

**接入已有系统的目标成本：新增代码 ≤ 50 行，不改订单表结构，不接管你的生命周期。**

---

## 数据模型

7 张表：

| 表 | 作用 |
|---|---|
| `dist_agent` | 分销员 + 关系链（物化路径 `path`） |
| `dist_binding` | 绑定 / 归因（支持有效期、换绑） |
| `dist_commission` | 佣金单，**不可变**，含费率/基数/冻结期**快照** |
| `dist_account` | 账户（待结算 / 可提现 / 提现中 / 已提现） |
| `dist_ledger` | 资金流水，**复式只追加** |
| `dist_withdraw` | 提现单 + 状态机 |
| `dist_rule_version` | 规则版本（改费率不污染历史） |

详见 [PRD.md §6](PRD.md#6-领域模型)。

---

## 三条不变量

这是本库存在的全部意义，也是测试的重点：

1. **余额永远等于流水之和**：`SUM(dist_ledger.delta_*) == dist_account.*`
2. **分佣总额不超过可分配上限**：`SUM(佣金绝对值) <= order.allocatable_cap`
3. **任意退款序列后净额非负**：`净佣金 == 已结算 - 已冲正 >= 0`

---

## 它不做什么

`distledger` **刻意不做**：推广链接/海报/排行榜/收益中心 UI、分销员申请审核流程、支付与分账、团队计酬/区域分红/链动等玩法、**按拉人头或注册人数计酬**、HTTP 路由与 Admin 后台、ORM/MQ/框架绑定。

理由见 [PRD.md §3.2 Non-Goals](PRD.md#32-非目标non-goals本清单是需求边界的法律) —— **这份清单是需求边界的法律。**

---

## ⚠️ 合规声明

分销在中国受《禁止传销条例》《刑法》第 224 条之一约束。本库**只提供账务能力，不提供合规判断**。

使用者必须自行确保：层级 ≤ 3 级 · 计酬以真实成交为基数 · 不以缴费作为参与条件 · 资金不走"二清"。

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
- ❌ 向核心加入"玩法"（团队计酬 / 区域分红 / 链动）—— 这类能力只能作为第三方 `Calculator` 实现存在
