---
adr: ADR-PHASE0-0006
title: 仓库内模块布局采用 internal/<module> 顶层化命名
status: accepted
owner: David
decided: 2026-09-22
supersedes: -
classification: public
---

# ADR-PHASE0-0006：仓库内模块布局采用 internal/<module> 顶层化命名

## 决策

Go 模块化单体在仓库内采用 `internal/<module>` 顶层布局：`internal/policy`（03.1）、`internal/registry`（03.2，含 hub）、`internal/resolver`（03.3）、`internal/mmr`（03.4）、`internal/evidence`、`internal/ops`（03.8），共享叶子为 `internal/platform`。CI/工具在 `tools/`，进程入口在 `cmd/`。

## 与设计文档的关系

[technology-stack.md](../architecture/technology-stack.md) §3.2 的 `sovereigntyops/contracts/adapters` 与 [development-plan.md](../programs/03-sovereign-ai-open-ai-fabric/development-plan.md) §3 的 `modules/sovereignty-policy/...` 均为**逻辑模块划分示意**，非仓库目录规范。本仓库是单一 Go 模块（module path `github.com/davidchen516/saoaf`），Go 的 `internal/` 可见性规则与模块边界语义天然对齐（仓库外不可导入、boundarycheck 强制模块间禁依赖）。语义映射在每处 `doc.go` 注明 03.x 归属。

## 依据

- 单仓库单模块下，`internal/<module>` 以语言机制提供与设计边界等价的隔离，无需额外目录层级。
- 开发计划 §3 本身允许"其他服务可以多仓"的灵活性；本仓库作为控制面单体的落地形态由本 ADR 固定。

## 影响

- 后续 Issue（I06–I17）在既有 `internal/<module>` 占位包内实现；新增顶层包须先过 boundarycheck 规则评估。
- 设计文档不回改（保留为逻辑视图）；实现视图以本 ADR 为准。
