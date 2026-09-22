---
title: SAOAF 当前未决项清单
version: 1.2.0
status: proposed
owner: David
created: 2026-09-22
updated: 2026-09-22
classification: public
---

# SAOAF 当前未决项清单

本清单只保留仍会改变范围、生产责任或验收结果的事项。治理负责人、仓库可见性、MMR/身份 Mock、MVP 容量、数据库、消息平台和 Evidence 基线已按项目发起人指示关闭，详见 [Phase 0 最小 MVP 基线](../architecture/mvp-baseline.md)。

## A. 仍需关闭

| ID | 优先级 | 输入/决策 | Owner | 阻塞范围 |
|---|---|---|---|---|
| OPEN-02 | P0 | 已建 MMR 的精确镜像 digest、企业 fork 差异及真实 endpoint | David | 真实联调；Mock 和 ARR 开发不受阻 |
| OPEN-03 | P1 | 首批 10–20 个 Capability、Owner、critical 等级与验收样例 | David | Registry 首批数据和业务验收 |
| OPEN-04 | P1 | MCP Registry/Gateway 与 Context Router 现有接口 | David | 03.5 契约冻结；本期不实现运行时 |
| OPEN-05 | P1 | A2A 首批跨 Agent 场景和正式信任域 | David | 03.6 契约冻结；本期不实现运行时 |
| OPEN-06 | P1 | AI Factory 多云/边缘编排接口 | David | 03.7 契约冻结；本期不实现运行时 |
| OPEN-07 | P1 | 关键供应商清单、退出周期和数据导出条款 | David（架构），采购/法务协同 | 03.8 真实退出演练 |
| OPEN-08 | P1 | 生产 S3 Object Lock 产品和 WORM 合规验证报告 | David（模块），安全/存储/合规协同 | Evidence 生产准入；本地契约开发不受阻 |

## B. 本轮已关闭

| 原问题 | 决定 |
|---|---|
| 工程治理责任 | 工程总负责人、架构委员会以及 03.1–03.8 各模块负责人均为 David；生产高风险操作仍需不同身份主体执行申请、批准、执行和审计 |
| 仓库可见性 | GitHub 仓库继续公开；文档按 `public` 分级；尚未授予开源许可证 |
| MMR | 使用 vLLM Semantic Router v0.3.0 契约基线，提供 `mocks/mmr`；真实版本差异作为 OPEN-02 |
| 身份与授权 | 固定 Mock issuer/audience/scope、TOTP MFA、短期 X.509 workload identity、AuthZEN PDP 和审批 API |
| 租户模型 | 单实例逻辑多租户；`tenant_ref` 来自受信身份；所有权威表和审计查询强制租户范围 |
| 容量/SLO | 峰值 100 Resolve QPS、P99 ≤ 100 ms、99.9% 月可用性、单地域三故障域 |
| 数据库 | PostgreSQL 18.6 + CloudNativePG 1.30.0；三实例；WAL/PITR + 每日 base backup |
| 消息平台 | NATS JetStream 2.14.7；本地单节点 Mock，生产三节点；at-least-once + 消费者去重 |
| Evidence | 在线明细按数据类型保留；每日 Evidence Pack 保存 1 年；高风险变更 2 年；S3 Object Lock COMPLIANCE；主权地域内保存 |

## C. 已固定的模块边界

- 03.3 AI Resource Router 与 03.4 Multi-Model Router 是兄弟模块，不合并权威状态或数据面。
- 03.4 已实现，以 vLLM Semantic Router 为主要开源决策核心；SAOAF 只做契约集成。
- 03.5 MCP、03.6 A2A、03.7 Placement 本期只定义接口，不实现运行时。
- 03.8 Sovereignty Operations & Exit Assurance 本期实现。
- 03 不自建生产 IAM/PDP；本地/CI Mock 可替换，生产由 04 Identity & Trust 持有。

## D. 下一关闭顺序

1. 由 David 提供并确认 MMR 真实镜像 digest、fork 差异和 endpoint。
2. 由 David 选定首批 Capability 及其业务归属，完成 MMR、Identity、Evidence 三个 PoC。
3. 对候选对象存储执行 WORM 删除绕过、legal hold、跨故障域恢复和权限隔离测试。
4. 冻结 Phase 0 ADR，再进入 Registry、Binding、Resolver 和 Outbox 实现。
