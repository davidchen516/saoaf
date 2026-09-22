---
title: SAOAF 当前未决项清单
version: 1.0.0
status: proposed
owner: TODO-03工程负责人
created: 2026-09-22
updated: 2026-09-22
classification: internal
---

# SAOAF 当前未决项清单

本清单合并架构、技术栈、接口、运行手册和实施计划中的待确认项。身份 Mock 已确定，生产身份事实仍需 04 工程提供。

## A. 需要项目发起人/治理层确认

| 优先级 | 决策 | 建议默认值 | 影响 |
|---|---|---|---|
| P0 | 03 工程正式 Owner、架构委员会、各子项目负责人 | 03 工程一名 accountable owner；模块各一名 technical owner | 无 Owner 无法批准 ADR、SLO 和发布 |
| P0 | 仓库可见性与发布政策 | 当前闭源；GitHub 仓库设为 private，暂停公开 Pages | 当前仓库公开会与 `internal` 分级冲突 |
| P0 | 一期租户模型 | 逻辑多租户，数据按 `tenant_ref` 隔离 | 决定索引、授权、容量与审计模型 |
| P0 | 一期容量/SLO | 明确 Resolve 峰值 QPS、P99、可用性和地域数量 | 决定缓存、HA、压测与成本 |
| P1 | 关键 Capability 分级和主权 KPI | 先选 10–20 个高价值能力，定义 critical 等级 | 决定首批验收与替代覆盖率 |
| P1 | RTO/RPO 与跨地域等级 | 当前草案 60 min / 15 min | 决定数据库、事件和灾备方案 |

## B. 需要现有系统负责人提供事实

| 优先级 | 输入 | Owner | 当前状态 |
|---|---|---|---|
| P0 | MMR 的 vLLM Semantic Router 精确版本/digest、fork 差异、Envoy/ExtProc 拓扑、entrypoint/recipe、API、错误码、decision ID、事件、OTel、CI/CD | MMR 负责人 | 仅“以 vLLM Semantic Router 为主”已确认，工程事实未提供 |
| P0 | 企业生产 OIDC issuer/audience/scope、client 注册、MFA、workload identity、AuthZEN/PDP 与审批接口 | 04 Identity & Trust | 标准与 Mock 已确定，生产参数未提供 |
| P0 | PostgreSQL 支持版本、HA、备份、K8s 运维方式 | 数据库平台 | 未提供 |
| P0 | Kafka/NATS/Pulsar 现状与标准事件平台 | 集成平台 | 未提供 |
| P0 | Evidence 保留期、WORM 要求、对象存储和数据驻留 | 合规/安全/存储 | 未提供 |
| P1 | MCP Registry/Gateway、API Gateway 当前能力 | 集成平台 | 03.5 仅接口，本期不实现 |
| P1 | Context Router 与 Tool Gateway 的能力摘要接口和成熟度 | 对应领域 Owner | 未提供 |
| P1 | A2A 首批场景和信任域 | Agentic/Identity | 03.6 仅接口，本期不实现 |
| P1 | AI Factory 多云/边缘编排现状 | 基础设施 | 03.7 仅接口，本期不实现 |
| P1 | 关键供应商、退出周期、数据导出条款 | 采购/法务/架构 | 影响 03.8 验收 |

## C. 已决，不应再次讨论

- 03.3 AI Resource Router 与 03.4 Multi-Model Router 是兄弟模块，不合并权威状态或数据面。
- 03.4 已实现，以 vLLM Semantic Router 为主要开源决策核心；本项目只做契约集成。
- 03.5 MCP、03.6 A2A、03.7 Placement 本期只定义接口，不实现运行时。
- 03.8 Sovereignty Operations & Exit Assurance 本期实现。
- 03 不自建 IAM/PDP；本地/CI 采用 Keycloak + Prism Mock，生产接 04 Identity & Trust。
- 当前资料按闭源、`internal` 分级维护；对外发布必须重新审批。

## D. 关闭顺序

1. 先关闭治理 Owner、仓库可见性、MMR 工程事实和生产 IAM/PDP 四项。
2. 再锁定容量/SLO、数据库、消息平台和保留期，形成 Phase 0 ADR。
3. 随后完成 MMR、Identity、Evidence 三个 PoC；通过后冻结接口和版本。
4. MCP/A2A/Placement 的输入不阻塞本期接口定义，但阻塞其数据面上线。
