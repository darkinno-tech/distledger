# Changelog

本项目遵循 [语义化版本](https://semver.org/lang/zh-CN/) 与 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/) 格式。

> **API 稳定性承诺**：`v1.0` 之前允许破坏性变更，但必须记录在本文件；`v1.0` 之后遵循语义化版本。

---

## [Unreleased]

### Added
- `PRD.md`：完整产品需求与设计文档（领域模型、状态机、事件契约、快速接入设计、不变量与测试策略、里程碑）
- `README.md`：面向开发者的项目入口与 30 秒 Quickstart
- `docs/design-decisions.md`：15 条关键设计决策记录（ADR），含"从 Golershop 学到什么"的正反面对照
- `LICENSE`：MIT
- `.gitignore`：Go 项目忽略规则

### Notes
- 当前处于**设计阶段**，尚无 Go 代码。
- 下一步：`v0.1`（内存 store + 一级分佣 + 幂等键 + 不变量 I1），见 [PRD.md §13](PRD.md#13-里程碑与版本规划)。

---

## 版本规划（摘自 PRD §13）

| 版本 | 范围 |
|---|---|
| v0.1 | 内存 store + 一级分佣 + 幂等键 + 不变量 I1 |
| v0.2 | 多级分佣 + 规则配置 + 冻结期快照 + I2 |
| v0.3 | 退款冲正（全退/部分退/已结算追回）+ I3 |
| v0.4 | MySQL store + 事务/乐观锁 + `Migrate` |
| v0.5 | 提现链路 + `PayoutChannel` + `ManualChannel` |
| v0.6 | `SelfCheck` + `Maintain` + 可观测性钩子 |
| v1.0 | API 冻结 + 文档完整 + property-based 全绿 |
