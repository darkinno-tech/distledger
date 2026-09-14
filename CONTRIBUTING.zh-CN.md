# 参与 distledger 贡献

感谢你考虑参与。这是一个很小的库，但规则异常严格——因为这份代码经手的是钱。
本文把规则一次写完，这样任何贡献都不会因为「没写下来的规矩」被拒。

**English: [CONTRIBUTING.md](CONTRIBUTING.md).**

---

## 两条硬规则

每个改动都必须同时满足。CI 会强制检查，所以你会立刻知道，而不是等到 review。

### 1. 仓库根目录的 `go test ./...` 必须在**没有数据库**的情况下通过

根模块就是这个库。它必须能在什么都没装的环境里跑测试——这是让所有人都跑得动测试的唯一办法。
需要真实数据库的测试属于 `integration/`，那是一个**独立的 Go 模块**，
它有自己的 `go.mod`，正是为了让驱动包进不了这个库。

### 2. `go.mod` 的 `require` 块保持为空

库不引入任何第三方依赖。日志不行、错误处理不行、测试也不行。
**一棵你审计不了的依赖树，就是一本你审计不了的账** —— 这是硬规则，不是偏好。

`integration/` 模块可以随意使用驱动与测试工具：它是没人 import 的模块的测试代码。

如果你认为某个依赖确实必要，请先开 issue 论证。到目前为止答案一直是不，
包括标准库确实用起来别扭的那些场合。

---

## 欢迎的贡献

- **不变量测试、边界用例，以及真实缺陷的回归测试。** 这是价值最高的一类贡献。
  见下面「测试要固定成本，而不只是结果」。
- **新的 `Store` 实现。** 存储端口刻意设计得苛刻，所以新增后端是对这套设计最好的压力测试。
  加一个后端约 100 行 `Dialect`，不是再写一个 store —— 参考
  `store/sql/dialect.go` 与已有的四个方言。
- **文档修正**，尤其是本文档或其它文档声称了代码并没做的事。那本身就是 bug。
- **`examples/` 下的新示例。**
- **故障复盘**：你修掉的、且此前没有测试能抓到的缺陷，请写入 `docs/fault-reviews/`，格式见下文。

## 明确不在范围内

- **往核心里加「玩法」**：团队计酬、区域分红、链动、按人头或按注册付费。
  这些只能作为第三方 `RateResolver` 存在。完整清单见 PRD §3.2，
  **那份清单是需求边界的法律** —— 它是这个包不变成一个分销系统的原因。
- **合规判定。** 库做账务，不判断商业模式是否合法。
- **提现与打款通道**尚未实现。落地之后，通道实现（微信 / 支付宝 / 银行卡）非常欢迎。

---

## 语言与风格

| 产物 | 语言 |
|---|---|
| 代码注释、doc 注释 | **英文**，美式拼写，em dash |
| 提交信息 | **英文**，Conventional Commits |
| `README.md`、`CONTRIBUTING.md` | 英文 |
| `README.zh-CN.md`、`CONTRIBUTING.zh-CN.md` | 中文 |
| `PRD.md`、`docs/*.md`、`CHANGELOG.md` | 中文，这是刻意的 |

设计文档用中文，因为它们记录的是设计意图与取舍，而这些争论是用中文进行的；
代码用英文，因为读它的是所有人。这个分工是刻意的，请不要把它「统一」成一种。

### 注释解释为什么，而不是做什么

代码已经写清楚了它做什么。一条注释的价值在于记录代码说不出来的东西：
为什么选这条路而不是那条、代价是什么、否决了什么。

```go
// 差：重复了代码
// 版本号加一。
version++

// 好：记录了一个判断及其代价
// 版本号无条件递增，因为 MySQL 对「改了等于没改」的 UPDATE 报告 0 行受影响，
// 所以不能靠受影响行数识别空写。
version++
```

**一条与代码矛盾的注释比没有注释更糟。** 历次 review 中已发现并修掉数条；
你若发现一条，那是一个真实的 bug 报告。

### 格式

`gofmt` 与 `go vet` 必须干净，CI 会检查。

---

## 测试要固定成本，而不只是结果

这是本仓库与常见 Go 库差别最大的地方，写测试前值得一读。

这里很多性质**从结果上看不出来**。一个问「哪些租户有活」的心跳和一个问「哪些活到期了」的心跳，
结算的佣金**完全一样**；差别只在规模上暴露——而那是最不该发现它的地方。
因此这类性质靠**数操作次数**或断言**查询形态**来固定。

参考 `scale_test.go` 与 `integration/explain_test.go`。

另外三条来自真实缺陷的规则：

1. **测试里绝不丢弃 error。** 这里曾有一个并发测试只统计失败**数量**、不记录首个错误，
   结果把一个「资金路径上返回了错误类型」的缺陷变成了看起来像 flaky 的偶发失败。
   如果一个测试可能因为两种原因失败，就让它说清楚是哪一种。
2. **断言上界被**用满**，而不只是没超过。** 一个只检查 `<= limit` 的预算测试，
   在 limit 因别的 bug 被压成 0 时照样通过。
3. **测试不能被「什么都不做」满足。** 「健康数据通过」这个断言，
   一条永远返回空结果的查询也能通过。要注入损坏并断言它**被发现**。

---

## 做决策就要写 ADR

任何带来取舍的改动——新的端口方法、存储表示、并发策略——都需要在
`docs/design-decisions.md` 追加一条 ADR（用下一个编号）。
**不要修改已接受的 ADR**，写一条新的声明它取代旧的。

本项目的 ADR 有固定结构，review 时会要求补齐缺失部分：

- **背景**：是什么迫使要做决定
- **决策**：选了什么
- **代价**：它让什么变差了。**没有代价的 ADR 是没写完的**
- **被否决的替代方案**：以及为什么，通常附上做出该判断的那次测量

**测量胜过推理。** 「覆盖索引范围扫描读到 limit 就停，所以是 O(limit) 而不是 O(表)」
是一个主张；一段 `EXPLAIN` 输出才是证据。

---

## 提交

Conventional Commits，英文，祈使句：

```
feat: reconcile accounts in the store instead of streaming the ledger
fix(store): stop reporting insufficient balance for a concurrent increment
perf: locate the heartbeat's work by due time and bound it with one budget
docs: say what each invariant guarantees and what it does not
```

使用的类型：`feat`、`fix`、`perf`、`refactor`、`docs`、`test`、`chore`、`style`。
必要时加括号作用域（`store`、`sql`、`memory`、`integration`）。

**价值在正文里。** 解释**为什么**、测到了什么、否决了什么。
一条只是复述 diff 的提交，半年后没法 review：

```
fix(store): stop reporting insufficient balance for a concurrent increment

IncrementAccount treated "the row exists and the UPDATE matched nothing" as
proof that the guard refused the change. It is not proof. The row can also
have been created by another transaction in the window between the UPDATE
and the SELECT that follows it, and in that case the increment was never
applied at all.
```

**一个提交只做一件逻辑上的事。** 混进 feature 提交里的修复无法单独回滚或摘取——
对一个资金库来说，这不是风格问题。

---

## 绝不要提交

- 构建产物、覆盖率文件、性能采样、编辑器与系统垃圾文件。`.gitignore` 覆盖了大部分，
  但提交前请看一眼 `git status`。
- 跑集成测试留下的本地数据库文件或转储。
- 密钥、DSN、凭据，**包括写在测试文件里的**。集成测试从环境变量读 DSN，
  未设置就跳过，请保持这个做法。

## 怎么跑

**跑集成测试前先确认 Go 版本。** 库本身需要 Go 1.22，但 `integration/` 需要 **Go 1.25**
——它的驱动声明了这个要求。版本不对时，你会在一个看起来无关的命令上收到
`go.mod requires go >= 1.25.0`。

```bash
go test ./...                              # 库的测试，不需要数据库
go vet ./... && gofmt -l .                 # 两者都必须干净
go test -race ./...                        # 并发路径

go run ./examples/01-quickstart            # 30 秒跑完端到端

# 集成测试在独立模块里。未设置 DSN 则跳过。
cd integration && docker compose up -d --wait
DISTLEDGER_TEST_MYSQL_DSN='root:distledger@tcp(127.0.0.1:13307)/distledger?charset=utf8mb4&parseTime=false' \
DISTLEDGER_TEST_POSTGRES_DSN='postgres://postgres:distledger@127.0.0.1:15433/distledger?sslmode=disable' \
  go test ./...
docker compose down
```

集成测试对内存、SQLite、MySQL、PostgreSQL **跑同一套断言**。
未配置的后端会被**跳过**而不是失败，所以你可以只用手上有的那个数据库跑。

---

## 故障复盘

当你修掉一个此前没有测试能抓到的缺陷时，请往 `docs/fault-reviews/` 加一份复盘，
命名为 `YYYY-MM-DD-简短描述.md`。模板与已有条目就是范例。

最值得花力气的是那条分析链：表象 → 直接原因 → 根本原因 → **为什么此前没被发现**。
最后一步通常才是真正有用的教训。在已有那份复盘里，答案是「测试把 error 丢了」——
这也正是本文「测试要固定成本」第 1 条规则的由来。

---

## Pull Request

模板会问关键信息，简要说就是：

- 改了什么、**为什么** —— 有 issue 就关联
- 影响哪条不变量或契约（如果有）
- 属于性能改动的话，你测到了什么
- 确认两条硬规则成立

**改动账务行为却不带一个「改动前会失败」的测试，很可能会被退回。**
这不是为了设门槛：这个库的失败形态是**钱悄悄消失**，唯一的防线就是有测试能抓住它。

## 响应时间

不承诺响应时间。这是由有其它工作在身的人开源维护的。
等上一两周后礼貌地问一句完全可以，也很欢迎。
