---
title: AI Resource Router 子项目架构一致性审计报告
version: 1.2.0
status: reviewed
owner: AI 架构交付
created: 2026-09-21
updated: 2026-09-21
classification: internal
scope:
  - ../programs/03-sovereign-ai-open-ai-fabric/architecture.md
  - ../programs/03-sovereign-ai-open-ai-fabric/module-contracts.md
  - ./ai-routing-platform-4a.md
  - ./ai-resource-router.md
  - ./technology-stack.md
  - ../specs/resource-resolver-api.md
  - ../specs/resource-resolver-events-and-data.md
  - ../specs/multi-model-router-integration.md
  - ../plans/resource-resolver-implementation-plan.md
  - ../runbooks/resource-resolver-operations.md
---

# AI Resource Router 子项目架构一致性审计报告

## 1. 审计结论

架构包在项目边界、资源分类、权威数据、控制面/数据面、MMR 关系、接口名、生命周期和落地阶段上保持一致。ARR 已明确归属 03 Sovereign AI & Open AI Fabric 的 03.3 子项目，并与 MMR、MCP、A2A、Placement 保持兄弟模块关系。当前未决项在 Phase 0 结束前必须关闭，才能冻结实现基线。

## 2. 检查结果

| 检查项 | 结果 | 说明 |
|---|---|---|
| 文档 metadata | 通过 | 所有交付文档含 title/version/status/owner/date/classification 等元数据 |
| 相对链接 | 通过 | 架构包内部链接均可解析 |
| 项目定位 | 通过 | ARR 始终定义为跨域能力解析，不承担专业数据面 |
| MMR 权威边界 | 通过 | MMR 始终为模型域唯一权威，ARR 不保存具体模型与路由策略 |
| 资源分类 | 通过 | 一期统一为 MODEL/CONTEXT/TOOL；AGENT 是后续可选 |
| Provider 调用路径 | 通过 | Harness 直接调用各专业服务，ARR 不代理流量 |
| API/数据字段 | 通过 | Plan、Binding、Snapshot、Decision ID 和 revision 语义一致 |
| 事件与事务 | 通过 | CloudEvents + Transactional Outbox；至少一次、消费者幂等 |
| 安全边界 | 通过 | 身份/授权权威在 IAM，ARR 只消费声明和决定引用 |
| 技术栈边界 | 通过 | 默认 Go 1.27.x；MMR/企业 Java 基线满足覆盖规则时切换 Java 25 + Spring Boot 4.1，禁止长期双栈 |
| 开源优先 | 通过 | 主路径、可选、自研和否决组件已分组，并含许可证/退出条件 |
| 实施可执行性 | 通过 | 有 PoC、WBS、测试、CI/CD、Go/No-Go、Runbook 和验收场景 |

## 3. 架构追踪矩阵

| 目标 | 架构机制 | 规格/实施证据 | 运行证据 |
|---|---|---|---|
| Agent 去硬编码 | Capability Binding + Resource Plan | Resolve API、MMR adapter | Plan/Decision Ledger |
| MMR 不重复建设 | MODEL_PROVIDER 只绑定 logical profile | MMR 集成设计 | model route decision 关联 |
| 变更不重发 Agent | 版本化 Binding、短 TTL 缓存 | publish/rollback API | BindingActivated、cache revision |
| 跨域可审计 | plan ID + child evidence ID | 数据模型和三域契约 | traces、Decision Record |
| 不成为数据面瓶颈 | Harness 直连专业服务 | 时序图和 endpoint_ref | ARR 无模型/工具 payload |
| 配置可回滚 | 不可变 revision + active pointer | 实施计划、管理 API | Runbook 回滚演练 |
| 敏感数据最小化 | 禁止字段和摘要化 | API/Data schema | 日志/Trace/DLP 扫描 |

## 4. 开源选型审计

| 类别 | 结论 | 审计意见 |
|---|---|---|
| 主路径 | Go、PostgreSQL、pgx/sqlc、OpenAPI/JSON Schema、CloudEvents、CEL-Go、OpenTelemetry；运行平台复用 MMR | 成熟、可替换，未侵入领域权威边界 |
| 可选 | Backstage、OPA、Kafka/NATS、Valkey、Temporal、CloudNativePG | 均设置业务/规模准入条件，不作无依据的 Day-1 依赖 |
| 实验 | xRegistry | 规范和实现仍需稳定性/兼容 PoC，当前不承载生产 SoT |
| 自研 | Binding、Resolver、Plan、Snapshot adapters | 属于项目特有且代码面小，已通过端口隔离 |
| 否决 | 新模型网关、Consul Runtime Registry、工作流/规则引擎、搜索/图/向量库 | 有明确边界、许可证、复杂度或能力不匹配理由 |

## 5. Phase 0 必须关闭的事项

| ID | 阻塞的决定 | 所需证据 |
|---|---|---|
| A-01 | 确认采用 Go 主路径或触发 Java 覆盖规则 | MMR 代码库、公共 SDK、团队能力与流水线盘点 |
| A-02 | MMR profile/Snapshot 具体 Schema | MMR 现有别名、版本和变更流程 |
| A-03 | 生产容量与缓存参数 | QPS、对象规模、工厂/区域数量、压测 |
| A-04 | 保留期和灾备等级 | 合规政策、业务 RTO/RPO 签字 |
| A-05 | IAM、审批和 break-glass 集成 | 企业 IAM/PDP/ITSM 当前接口 |
| A-06 | Context/Tool 一期可用性 | 两个专业域的 Owner、接口和 SLO |

## 6. 评审建议

1. 先评审“边界与权威”，再讨论技术组件；边界未通过时不得进入框架选型。
2. Phase 0 必须用 MMR 真实接口完成最难链路 PoC，不能只用 Mock 得出结论。
3. xRegistry、OPA、Kafka/NATS、Valkey、Temporal 均按门槛采用；未达门槛不进入生产依赖清单。
4. 架构批准后，把 OpenAPI、JSON Schema、数据库 migration 和联合契约测试纳入同一版本库并设置 breaking-change 门禁。
