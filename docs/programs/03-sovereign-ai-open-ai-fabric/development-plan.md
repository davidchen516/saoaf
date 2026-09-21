---
title: 03 Sovereign AI & Open AI Fabric 2027 开发实施计划
version: 1.0.0
status: proposed
owner: TODO-03工程交付负责人
reviewers:
  - TODO-03工程架构负责人
  - TODO-各子项目负责人
  - TODO-安全负责人
  - TODO-SRE负责人
created: 2026-09-21
updated: 2026-09-21
classification: internal
related:
  - ./architecture.md
  - ./module-contracts.md
  - ../../plans/resource-resolver-implementation-plan.md
---

# 03 Sovereign AI & Open AI Fabric 2027 开发实施计划

## 1. 开发目标

2027 年完成一个可生产运行的主权控制面和三条开放互操作主链：

1. `Agent Harness → ARR → MMR`：模型能力可替换、可追溯；
2. `Agent Harness → ARR → MCP Gateway → Enterprise Tool`：Tool 能力可发现、受控执行；
3. `Agent Harness → ARR → A2A Gateway → Domain Agent`：Agent 能力可发现、受控委派；
4. `Portable Profile → Placement Plan → Enterprise AI Factory`：至少一个工作负载完成跨区/跨平台迁移演练。

不以模块数量作为完成标准，以关键 Capability 的替代能力、执行证据和真实退出演练作为验收标准。

## 2. 开发组织

### 2.1 建议团队

| 团队 | 建议核心投入 | 责任 |
|---|---:|---|
| 03 Control Plane | 5–7 FTE | Policy、Registry、Hub、ARR、Ops |
| Model Fabric / MMR | 3–5 FTE | 既有 MMR 演进、profile、证据 |
| MCP Fabric | 4–6 FTE | Registry、Gateway、SDK、首批 adapters |
| A2A Fabric | 3–5 FTE | Agent Registry、Gateway、SDK、信任域 |
| Placement/Portability | 3–4 FTE | Profile、Resolver、AI Factory adapters |
| Identity/Trust 联合小组 | 2–3 FTE（跨工程） | workload identity、delegation、PDP、审批 |
| SRE/Quality/Security | 3–4 FTE（共享） | SLO、测试平台、供应链、演练 |

实际投入由现有平台能力评估后调整。不要为每个逻辑模块成立独立团队；一期允许 Policy、Registry、ARR、Ops 由同一 Control Plane 团队建设。

### 2.2 治理

- 03 工程委员会：月度范围、风险和 ADR 决策。
- Contract Council：MMR/MCP/A2A/Context/Identity/Factory 双周契约评审。
- Protocol Owners：跟踪 MCP/A2A/OpenAPI/CloudEvents/OCI 版本与安全公告。
- Vendor Exit Board：架构、采购、法务、安全和业务 Owner 半年度演练。

## 3. 代码与仓库策略

现有 MMR 不因本工程强制迁仓。建议建立一个独立的 **contract/governance repository**，其他服务可以多仓，但必须由契约流水线联动：

```text
sovereign-ai-contracts/
├── capabilities/          # Capability schemas and lifecycle
├── providers/             # Provider snapshot schemas
├── openapi/               # ARR/Registry/Placement APIs
├── events/                # CloudEvents payload schemas
├── protocols/
│   ├── mcp/               # adopted version, enterprise extensions
│   ├── a2a/
│   └── model-profile/
├── portability/           # PortableDeploymentProfile schemas
├── policy-inputs/         # input/output schemas, not policy secrets
├── compatibility/         # N/N-1 matrices
├── examples/              # golden contract examples
└── adr/

sovereign-control-plane/   # modular monolith initially
├── modules/
│   ├── sovereignty-policy/
│   ├── capability-registry/
│   ├── resource-resolver/
│   ├── evidence-index/
│   └── resource-hub-api/
├── adapters/
│   ├── identity-trust/
│   ├── mmr/
│   ├── mcp/
│   ├── a2a/
│   ├── context/
│   └── ai-factory/
└── migrations/
```

MCP Gateway、A2A Gateway、MMR 和 Placement Controller 是独立部署单元。Control Plane 一期保持模块化单体，避免过早拆分。

## 4. 2026 Q4 准备阶段

### 4.1 现状盘点

- MMR 技术栈、API、profile、鉴权、SLO、用量与决策 ID；
- 企业 API Gateway、MCP、Agent Registry、A2A、服务发现和 IAM 现状；
- Context Router、AI Factory、多集群/边缘平台现状；
- 关键供应商、合同导出条款、数据驻留和替代方案；
- 开源依赖、许可证和安全基线。

### 4.2 立项产物

- 03 工程章程、Owner 和资金；
- 本文档三件套批准；
- contract repository；
- 10–20 个首批 Capability 清单；
- 三个首批 Provider：MMR、一个只读 MCP Tool、一个内部 Domain Agent；
- 一个迁移演练工作负载；
- Phase 0 PoC 计划。

## 5. 2027 路线图

### Q1：主权控制面与模型主链

**目标**：先利用已建成 MMR，形成最短生产闭环。

**交付**

1. Sovereignty Policy 数据模型 v1，不自建 PDP；接入 04 工程。
2. Capability Registry v1：Capability、Provider、Binding、Snapshot、生命周期。
3. AI Resource Hub 最小只读/管理界面；可先使用内部管理 UI，Backstage 仅在已有时投影。
4. ARR v1：MODEL/CONTEXT/TOOL 三类计划模型，首期实际启用 MODEL。
5. MMR Logical Profile Publisher、`resource_plan_id/model_route_decision_id` 关联。
6. Decision/Evidence Index v1、OTel 属性规范。
7. OpenAPI/JSON Schema/CloudEvents 和 N-1 契约门禁。

**退出条件**

- 一个生产 Agent 通过 ARR 获取 MMR profile 并完成调用；
- MMR 内部切换具体模型时 ARR/Agent 不改配置；
- Resource Plan、授权决定和模型子决定可关联；
- Q1 Go/No-Go 清单全部通过。

### Q2：MCP Tool & Data Access Fabric

**目标**：完成企业 Tool 的开放接入和受控执行。

**交付**

1. MCP 规范基线和企业扩展 namespace；
2. 企业 MCP Registry 投影和 MCP Gateway v1；
3. workload identity/OAuth、Tool 风险、参数 Schema、幂等、approval ref；
4. `preview/execute/status/cancel` 统一执行契约；
5. ERP/MES/PLM 中至少 3 个只读、1 个低风险写 Tool adapter；
6. ARR 启用 TOOL_PROVIDER；
7. Tool execution evidence 和安全监控；
8. Official MCP Registry/reference implementation 对照 PoC。

**退出条件**

- 同一 Capability 可在不修改 Agent 的情况下切换两个 Tool Provider；
- 高风险/缺审批请求 fail closed；
- Tool 参数和结果不进入 ARR/Registry；
- MCP 标准兼容测试通过。

### Q3：A2A Agent Federation 与 Placement

**目标**：建立跨框架 Agent 委派和可移植放置路径。

**交付**

1. Agent Registry/Agent Card 投影和 A2A Gateway v1；
2. delegation chain、task status/cancel、artifact digest、跨信任域控制；
3. ARR 启用 AGENT_PROVIDER；
4. 两个内部 Domain Agent 通过 A2A 协作；外部 Agent 仅在安全评审后试点；
5. PortableDeploymentProfile 和 ZoneProfile v1；
6. Placement Resolver 与 AI Factory adapter；
7. OCI/SBOM/signature 基线；
8. 一个非关键工作负载完成 private ↔ cloud 或 central ↔ edge 迁移演练。

**退出条件**

- Agent 实现替换不改变上游 capability contract；
- 子 Agent 权限不超过委派范围；
- Placement Plan 与实际 deployment ID 可关联；
- 迁移不依赖手工重写应用配置。

### Q4：规模化、退出保证与生产治理

**目标**：证明主权能力可持续运营，不只是接口演示。

**交付**

1. Sovereignty dashboard：替代覆盖率、供应商集中度、协议兼容、Snapshot、出口完整性；
2. 关键 Capability 分级和替代 Provider 覆盖；
3. Vendor Exit Pack 自动检查；
4. 模型供应商退出/失效演练、MCP Provider 迁移、A2A Agent 替换；
5. DR、Provider suspend、协议版本升级和凭证轮转演练；
6. 性能、容量、成本和安全收敛；
7. 2028 多区域/外部生态扩展决策。

**退出条件**

- 至少一个关键模型 Provider 和一个 Tool Provider 完成实际替代演练；
- 关键 Capability 替代覆盖率达到批准目标；
- 生产 SLO、RTO/RPO 和安全评审通过；
- 所有关键 Vendor Exit Pack 可读取、可验证、可执行。

## 6. Epic 与依赖

| Epic | 模块 | 前置依赖 | 主要验收 |
|---|---|---|---|
| E03-01 Sovereignty Domain Model | 03.1 | 法务/数据/安全规则 | 七类主权可结构化表达 |
| E03-02 Capability Registry | 03.2 | E03-01、领域 Owner | 版本/Owner/Binding/退役闭环 |
| E03-03 Resource Resolver | 03.3 | E03-02、04 PDP | 确定性 Plan、P99、审计 |
| E03-04 MMR Integration | 03.4 | 既有 MMR | logical profile 与子决定关联 |
| E03-05 MCP Fabric | 03.5 | 04 identity、领域 API | 受控 Tool 执行与 provider 替换 |
| E03-06 A2A Fabric | 03.6 | 01 Agent Runtime、04 delegation | 跨 Agent 委派与撤销 |
| E03-07 Portability | 03.7 | AI Factory、制品库 | Placement Plan 和迁移演练 |
| E03-08 Sovereignty Ops | 03.8 | 所有专业域 evidence | 指标、exit pack、演练闭环 |
| E03-09 Resource Hub | 03.2 | Registry、企业门户 | 搜索/申请/Owner/影响分析 |
| E03-10 Contract & Conformance | 横向 | contract repo | MCP/A2A/API/事件 N-1 门禁 |

## 7. 首批 Capability 清单

| 类型 | 建议 Capability | 用途 |
|---|---|---|
| MODEL | `model.reasoning.high` | 高复杂度推理 |
| MODEL | `model.embedding.multilingual` | 多语言向量能力；实际模型由 MMR 决定 |
| CONTEXT | `context.maintenance.manual.retrieve` | 维修手册检索 |
| CONTEXT | `context.work-order.current-state` | 工单实时状态 |
| TOOL | `tool.erp.work-order.read` | 工单查询 |
| TOOL | `tool.mes.production-order.read` | 生产订单查询 |
| TOOL | `tool.plm.part.lookup` | 零部件查询 |
| TOOL | `tool.erp.work-order.update-status` | 低风险写试点，需审批/幂等 |
| AGENT | `agent.supply-risk.analyze` | 供应风险分析委派 |
| AGENT | `agent.quality-nonconformance.analyze` | 质量问题分析委派 |

Capability 命名需由领域 Owner 确认，不能由平台团队单方面定义业务语义。

## 8. PoC 门禁

### 8.1 MCP PoC

- 标准 client/server/gateway 互操作；
- OAuth/workload identity 和最小权限；
- Tool 列表大规模 discovery 的延迟和上下文开销；
- 参数 Schema、流式/长任务、取消、幂等、错误映射；
- Server 升级和 transport 变化；
- Registry 私有可见性、namespace、审批和退役；
- prompt injection/恶意 Tool description 防护。

### 8.2 A2A PoC

- 官方 SDK 与企业 Gateway 互操作；
- Agent Card discovery、任务、流式更新、取消和 artifact；
- 委派凭证缩权、撤销、跨信任域；
- 长任务断线恢复和重复投递；
- Agent 替换、版本兼容和错误归属；
- 恶意/不可信 Agent 的隔离。

### 8.3 Placement PoC

- OCI 制品在两个目标 zone 运行；
- 配置/Secret 引用不绑定供应商实现；
- accelerator/runtime 差异可由 profile 表达；
- 数据依赖和出口可验证；
- SLO、成本、性能和安全差异可比较；
- 回切路径和残留数据清理证明。

### 8.4 Open-source 依赖准入

每个重要依赖必须记录：功能适配、维护活跃度、稳定版本、安全政策、许可证、性能、运维、扩展、集成成本、退出方式。PoC 通过后锁版本/digest，保留 NOTICE，纳入 SBOM 和升级节奏。

## 9. CI/CD 与契约治理

Contract repository 门禁：

1. OpenAPI/JSON Schema/CloudEvents lint；
2. breaking-change detection；
3. MCP/A2A 标准 conformance；
4. N/N-1 producer-consumer tests；
5. 示例与 Schema 双向验证；
6. 扩展 namespace 和 sunset 检查；
7. license/security scan；
8. 文档和兼容矩阵同步。

服务流水线：

1. 复用 MMR/企业技术栈；
2. domain、persistence、contract、integration、security tests；
3. SBOM、镜像签名和 provenance；
4. expand/migrate/contract database migration；
5. canary、自动 SLO 判断和受控回滚；
6. 配置/Binding/Policy 与应用版本独立发布但统一证据。

## 10. 测试矩阵

| 层级 | 重点 |
|---|---|
| Domain | Policy、Binding、稳定解析、版本和状态不变量 |
| Contract | OpenAPI/MCP/A2A/CloudEvents/OCI N-1 |
| Security | 身份伪造、越权、委派提升、重放、参数注入、敏感数据 |
| Interoperability | 不同 SDK、Provider、框架和 transport |
| Performance | ARR、Registry、Gateway 并发和长尾 |
| Resilience | DB/PDP/Provider/消息/网络/区域故障 |
| Portability | 换模型、换 Tool、换 Agent、换 zone |
| Exit | 导出、恢复、替代、撤权、数据删除证明 |

## 11. Go/No-Go 条件

### 控制面

- [ ] 权威边界和数据 Owner 签字。
- [ ] Policy、Registry、ARR 版本和回滚闭环。
- [ ] 不保存禁止业务载荷。
- [ ] SLO、备份、恢复和 on-call 完成。

### 专业数据面

- [ ] MMR/MCP/A2A/Placement 各有独立 SLO、限流、隔离和 Runbook。
- [ ] 04 工程身份/授权/审批集成通过。
- [ ] 执行证据 ID 可与 Resource Plan 关联。
- [ ] Provider suspend/撤权达到目标时间。

### 主权验收

- [ ] 至少两类 Provider 完成真实替换。
- [ ] 一个工作负载完成跨 zone 迁移和回切。
- [ ] 关键供应商 exit pack 经恢复验证。
- [ ] 企业扩展移除后核心标准协议仍互操作。

## 12. 风险与应对

| 风险 | 迹象 | 应对 |
|---|---|---|
| 03 演变成万能平台 | 要求承载 Agent/Context/IAM/Factory 内部能力 | 使用权威矩阵和 ADR 拒绝边界扩张 |
| 协议标准快速变化 | MCP/A2A 版本频繁、SDK 不一致 | Adapter、N/N-1、版本钉住、季度升级窗口 |
| Resource Hub 与 Registry 混为一体 | UI 数据被当运行 SoT | Hub 只通过 API/投影访问 Registry |
| ARR 变成同步总编排 | 同步部署、检索、Tool/Agent 编排需求 | Harness 编排；部署异步；专业数据面直连 |
| 主权只剩“私有部署” | 没有替代/迁移/退出指标 | 以替代覆盖和真实演练验收 |
| 自研协议/网关膨胀 | 复制 MCP/A2A SDK 能力 | 薄 adapter；上游优先；禁止 fork 默认化 |
| 与 04 授权重复 | 03 开始管理角色和策略裁决 | 03 只提供资源属性和消费 decision |
| 与 AI Factory 调度重复 | 03 开始管 GPU 队列/集群 | 03 只产生 Placement Plan |

## 13. 2027 项目级 KPI

| KPI | 建议目标 |
|---|---:|
| 纳管并有 Owner/SLA/版本的关键 Capability | ≥ 50；最终数量按领域确认 |
| 关键 Capability 替代 Provider 覆盖率 | ≥ 80% |
| MMR 内部模型变化导致 Agent 变更 | 0 |
| MCP Tool Provider 替换导致 Agent 业务代码变更 | 0（试点范围） |
| A2A Agent 替换通过契约测试 | ≥ 2 个能力 |
| 实际 Provider/Zone 退出或迁移演练 | ≥ 3 次/年 |
| Resource Plan 与子证据关联率 | 100% |
| 关键协议 N/N-1 兼容测试覆盖 | 100% |
| 控制面敏感业务载荷事件 | 0 |

KPI 在 Q1 由工程委员会根据真实规模批准，避免把未经盘点的数字直接作为预算承诺。
