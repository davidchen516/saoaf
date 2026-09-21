---
title: 03 Sovereign AI & Open AI Fabric 架构一致性审计
version: 1.1.0
status: reviewed
owner: AI 架构交付
created: 2026-09-21
updated: 2026-09-21
classification: internal
scope:
  - ./architecture.md
  - ./module-contracts.md
  - ./development-plan.md
  - ../../architecture/ai-routing-platform-4a.md
  - ../../architecture/ai-resource-router.md
  - ../../architecture/technology-stack.md
---

# 03 Sovereign AI & Open AI Fabric 架构一致性审计

## 1. 结论

03 工程的战略定位、八个模块、控制面/数据面、权威数据、与其他七大战略工程的边界，以及 AI Resource Router 与 MMR/MCP/A2A/Placement 的交互已经一致。可以进入跨工程架构评审和 Phase 0 现状盘点。

## 2. 一致性检查

| 检查 | 结果 | 结论 |
|---|---|---|
| 战略会话对齐 | 通过 | 覆盖 Data/Model/Compute/Context/Agent/Operation/Learning 七类主权 |
| ARR 项目上拔 | 通过 | ARR 明确为 03.3 子项目 |
| ARR/MMR 关系 | 通过 | 兄弟模块；ARR 跨域解析，MMR 模型域路由 |
| Registry 分层 | 通过 | Capability Registry 是跨域语义 SoT；专业 Registry 是域内 SoT |
| MCP/A2A 边界 | 通过 | Gateway 处理专业协议数据面，不进入 ARR |
| Identity 边界 | 通过 | 04 工程裁决；03 提供资源属性并执行/传播决定 |
| Context 边界 | 通过 | 02 工程拥有内容和装配；03 只注册能力与连接 |
| AI Factory 边界 | 通过 | 03 产生 Placement Plan；Factory 实际部署和调度 |
| Learning 边界 | 通过 | 08 生产学习资产；03 保证归属、导出和可迁移 |
| 开源优先 | 通过 | 标准/主路径、可选、实验、自研和否决项均已区分 |
| 技术栈可落地性 | 通过 | 默认 Go 主路径、MMR Java 覆盖规则、组件许可证、PoC 门禁和退出条件已定义 |
| 实施可执行性 | 通过 | 有团队、仓库、季度路线、Epic、PoC、CI/CD 和 Go/No-Go |
| 文档链接/metadata | 通过 | 架构包内链接和 metadata 已校验 |

## 3. 关键不变量

1. Resource Plan 不是工作流，也不是授权令牌。
2. ARR 不代理模型、Context、Tool、Agent 或 Compute 数据面。
3. MMR 是具体模型选择的唯一权威。
4. MCP Gateway 不拥有业务 API 和业务事务。
5. A2A Gateway 不拥有 Agent 内部编排和 Memory。
6. Placement Fabric 不创建或调度基础设施。
7. Capability Registry 不复制专业域的动态实例目录。
8. Policy Plane 不替代 04 工程的身份与授权裁决。
9. Sovereignty Ops 不复制全量业务载荷和专业观测事实。
10. 开放标准的企业扩展必须可版本化、可移除、可回归到标准核心。

## 4. Phase 0 阻塞项

| ID | 阻塞项 | 未关闭时的限制 |
|---|---|---|
| G-01 | MMR 技术栈和 profile/API 现状 | Go 为建议基线；确认是否触发 Java/Spring Boot 覆盖规则后才能冻结生产实现栈 |
| G-02 | 04 Identity/Trust 的 PDP、delegation、approval 接口 | 不能开放生产 Tool/A2A 执行 |
| G-03 | 企业 MCP/API Gateway 现状 | 不能确定复用、扩展或新建范围 |
| G-04 | AI Factory 多云/边缘控制面现状 | 不能选择 Crossplane/Karmada 等 adapter |
| G-05 | 首批 Capability 和 Provider Owner | 不能完成领域语义和验收 |
| G-06 | 供应商/法务退出条款和数据驻留规则 | 不能冻结 Sovereignty Policy v1 |

## 5. 开源决策审计

- MCP/A2A 使用官方规范与 SDK，企业层只做 policy/gateway adapter，禁止私有协议替代。
- Go 1.27.x、PostgreSQL 18、pgx/sqlc、OpenAPI/JSON Schema、CloudEvents、CEL-Go 和 OpenTelemetry 构成建议主路径；若 MMR 的 Java 基线复用价值更高则按 ADR 切换。
- Backstage、xRegistry、OPA、Valkey、Temporal、CloudNativePG 等都有明确 Owner、采用门槛和退出条件。
- MMR 已存在，因此不把 LiteLLM、Envoy AI Gateway 等重新引入 03 主路径。
- 项目特有的 Capability、Binding、Resource Plan、Sovereignty Score 和 Exit Rule 保持小型自研。
