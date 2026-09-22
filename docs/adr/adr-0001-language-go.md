---
adr: ADR-PHASE0-0001
title: Phase 0 冻结：Go 1.27.x 为控制面主语言
status: accepted
owner: David
decided: 2026-09-22
supersedes: -
classification: public
---

# ADR-PHASE0-0001：Go 1.27.x 为控制面主语言

## 决策

Phase 0 实现基线采用 Go 1.27.x（每次发布跟进所选 minor 的最新安全补丁）。切换 Java 25 LTS + Spring Boot 4.1.x 只能通过新的 superseding ADR，触发条件限于 technology-stack.md 第 4.2 节定义的三种情形（MMR Java 基线、团队 Java 能力显著占优、企业强制 Java 技术基线）。

## 依据

- Resolve 是短请求、低延迟、高并发、无模型计算的控制面服务，与 Go 的标准 HTTP、并发和静态二进制特性匹配。
- 既有 MMR 以 vLLM Semantic Router（Python 系）为决策核心，未提供可复用的 Java/Spring 公共 SDK，不满足 Java 覆盖条件。
- Issue #1 确认基线表："主语言 Go 1.27.x；切换 Java 必须新建 superseding ADR"。

## 证据

- [technology-stack.md 第 4 节开发语言决策](../../architecture/technology-stack.md)
- [Issue #1 已确认基线](https://github.com/davidchen516/saoaf/issues/1)
- [Go release history](https://go.dev/doc/devel/release)

## 影响

- I02 工程骨架按 Go 模块化单体建立（API/Worker 两进程形态）。
- 本 ADR 为 ACCEPTED 后，原 technology-stack.md 第 13 节 ADR-TECH-001（Proposed）由本 ADR 承接冻结；后续修订走新 ADR supersede，不原地改写。
