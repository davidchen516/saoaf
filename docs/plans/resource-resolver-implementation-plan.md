---
title: AI Resource Resolver 落地实施计划
version: 1.0.0
status: proposed
owner: TODO-AI平台项目负责人
reviewers:
  - TODO-ARR技术负责人
  - TODO-MMR负责人
  - TODO-安全负责人
  - TODO-SRE负责人
created: 2026-09-21
updated: 2026-09-21
classification: internal
related:
  - ../programs/03-sovereign-ai-open-ai-fabric/development-plan.md
  - ../architecture/ai-routing-platform-4a.md
  - ../specs/resource-resolver-api.md
  - ../specs/multi-model-router-integration.md
  - ../runbooks/resource-resolver-operations.md
---

# AI Resource Resolver 落地实施计划

## 1. 交付策略

以一个 Agent、三个能力薄切片落地：

- `model.reasoning.high` → 已建成 MMR 的稳定逻辑 profile；
- `context.maintenance.manual.retrieve` → Context Router 的只读能力；
- `tool.erp.work-order.read` → Tool/MCP Gateway 的低风险只读 action。

先完成 MMR 模型链路，再接 Context 和 Tool。任何阶段都不把专业数据面流量代理进 ARR。

建议周期为 14 周，实际日期在 MMR Phase 0 盘点后确定。建议核心团队 6–8 人：产品/架构 1、后端 2–3、平台/SRE 1、安全/IAM 0.5、测试/质量 1、MMR/Context/Tool 各兼职接口人。

## 2. 阶段计划

### Phase 0：边界、契约与 PoC（第 1–2 周）

**工作项**

1. 盘点 MMR 代码库、技术栈、profile、API、错误、鉴权、OTel、SLO 和部署方式。
2. 确认 Context Router、Tool Gateway、IAM 的现状和一期可用接口。
3. 锁定容量、数据保留、RTO/RPO、地域和租户隔离要求。
4. 完成 OpenAPI、JSON Schema、CloudEvents 初稿和 error taxonomy。
5. PostgreSQL PoC：发布并发、Resolve 查询、Outbox、月分区、备份恢复。
6. xRegistry PoC 只验证数据模型映射与替换成本；不进入生产主路径。
7. 输出依赖清单、许可证审查和初始 SBOM。

**必须通过的困难点 PoC**

| PoC | 基线 | 通过条件 |
|---|---|---|
| 确定性解析 | 10k Capability、100k Binding 的合成数据；实际规模确认后调整 | P99 ≤ 100 ms，结果 100% 可重复 |
| 发布并发 | 50 个并发 publish 同一 Binding | 只能有一个 active revision，无丢失更新 |
| MMR 契约 | profile Snapshot + 运行调用 | 内部模型替换不改变 ARR Plan |
| 故障读取 | 断开 PostgreSQL | 未过期发布快照继续读；管理写关闭 |
| Outbox | 消息端故障与恢复 | 无双写丢失，重复可幂等 |
| 敏感数据 | 注入密钥/Prompt/参数 | API/Schema/日志均阻止或脱敏 |

**退出条件**

- 4A、边界、OpenAPI、MMR 集成和 ADR 评审通过；
- 关键 TODO 有 Owner 和日期；
- PoC 数据证明单体 + PostgreSQL 可行；
- 未通过的开源候选有明确否决或延后记录。

### Phase 1：MMR 最小闭环（第 3–6 周）

**Epic A：工程骨架**

- 复用 MMR 的语言、框架、构建、镜像、鉴权 SDK、OTel、CI/CD；
- 模块边界：domain/application/adapters/api；
- PostgreSQL schema 与 migration；
- SBOM、依赖锁定、SAST/SCA、镜像签名。

**Epic B：Registry 与发布**

- Capability、Provider、Snapshot、Binding CRUD；
- 状态机、乐观锁、审批引用、Change Record；
- 发布/回滚/暂停；
- Outbox Worker 和事件 Schema。

**Epic C：Resolve 与 Plan**

- Schema 校验、约束过滤、稳定排序、冲突检测；
- Plan、Decision Ledger、幂等；
- 应用内缓存、revision 失效；
- runtime/admin 限流和线程池隔离。

**Epic D：MMR Adapter**

- Snapshot Publisher/Ingestor；
- Harness 传递 `resource_plan_id`；
- MMR 返回 `model_route_decision_id`；
- 联合消费者契约测试。

**退出条件**

- 模型薄切片端到端完成；
- 同一输入同一版本产生相同 fingerprint；
- MMR 内部模型切换不影响 ARR；
- 单元、集成、契约、安全基线测试通过。

### Phase 2：Context、Tool 与治理（第 7–10 周）

**Context 接入**

- 逻辑 context capability Snapshot；
- data classification、region、freshness 约束；
- `retrieval_evidence_id` 关联；
- 禁止文档和检索结果进入 ARR。

**Tool 接入**

- action、risk、idempotency、approval 元数据；
- Authz decision reference；
- preview/execute/status/cancel 由 Tool Gateway 持有；
- `execution_id` 关联。

**治理与门户**

- IAM 权限、生产双人审批和 break-glass；
- Resource Hub/Backstage 只读投影（已有 Backstage 时）；
- 退役依赖查询和通知；
- 审计报表与数据保留任务。

**退出条件**

- 三域契约测试通过；
- 高风险工具缺少授权时 fail closed；
- 数据分类和区域负向测试通过；
- 安全威胁建模和隐私评审关闭高风险问题。

### Phase 3：生产化与切换（第 11–14 周）

**可靠性**

- 3 副本、PDB/反亲和、HPA 或企业等价机制；
- PostgreSQL HA、PITR、恢复演练；
- 超时、连接池、限流、缓存上限；
- outbox backlog 和 DLQ 运行流程。

**验证**

- 目标规模 2 倍峰值的 60 分钟稳态压测；
- 数据库故障、实例滚动、事件总线故障、Snapshot 过期演练；
- 回滚、Provider suspend、凭证轮转、证书过期预警；
- 日志/Trace/事件敏感信息扫描。

**上线**

1. 影子决策 1–2 周；
2. 单 Agent/单租户 5%；
3. 25% → 50% → 100%，每阶段至少覆盖一个业务高峰；
4. 旧静态配置保留一个发布窗口；
5. 达标后冻结旧路径新配置并安排退役。

**退出条件**

- SLO dashboard、告警、Runbook、值班和 Owner 完整；
- 性能、安全、灾备、回滚演练通过；
- 生产准入评审签字；
- 旧路径退役日期确认。

## 3. 工作分解结构

| ID | 工作包 | 主要产物 | 依赖 |
|---|---|---|---|
| WP-01 | MMR 现状盘点 | 接口/技术栈/差距清单 | MMR团队 |
| WP-02 | 契约仓库 | OpenAPI、JSON Schema、事件 Schema | WP-01 |
| WP-03 | Domain Core | 状态机、Resolver、Plan | WP-02 |
| WP-04 | Persistence | migrations、repository、outbox | WP-03 |
| WP-05 | Runtime API | resolve/get plan、幂等、错误 | WP-03/04 |
| WP-06 | Admin API | registry/publish/rollback/suspend | WP-03/04 |
| WP-07 | MMR Adapter | Snapshot、执行关联 | WP-01/02/05 |
| WP-08 | Context Adapter | capability/evidence | WP-02/05 |
| WP-09 | Tool Adapter | action/authz/execution evidence | WP-02/05 |
| WP-10 | Security | IAM、mTLS、RBAC、审批、审计 | WP-05/06 |
| WP-11 | Observability | OTel、dashboard、alerts | WP-05/06 |
| WP-12 | Production Readiness | load/chaos/DR/runbook/canary | 全部 |

## 4. 测试策略

| 层级 | 重点 | 工具/方法 |
|---|---|---|
| Domain unit | 约束、排序、冲突、状态机、fingerprint | 表驱动/参数化测试 |
| Schema | 请求、Snapshot、事件正负例 | JSON Schema/OpenAPI lint |
| Persistence | 事务、锁、分区、Outbox | PostgreSQL Testcontainers |
| Contract | Harness/MMR/Context/Tool N-1 | Consumer-driven examples |
| Integration | IAM、服务发现、OTel、事件 | 预生产真实依赖或受控替身 |
| Performance | Resolve、发布、缓存击穿、DB故障 | 企业压测工具；固定数据集 |
| Security | 越权、注入、重放、敏感日志、供应链 | 企业安全流水线 + 人工评审 |
| Resilience | DB/消息/Pod/网络/Snapshot 故障 | 故障注入与恢复演练 |

核心业务规则的 mutation/branch coverage 目标由团队规范确定；不能用总覆盖率替代不变量测试。

## 5. CI/CD 门禁

Pull Request：

1. format/lint/compile；
2. domain + schema + persistence tests；
3. API breaking-change detection；
4. SAST、secret scan、SCA、license policy；
5. 生成 SBOM；
6. 架构依赖规则；
7. migration backward-compatibility check。

主干/发布：

1. 构建一次、镜像签名、以 digest 推广；
2. 预生产契约和 smoke test；
3. migration expand 阶段先行；
4. canary 观察错误率、P99、DB pool、outbox lag；
5. 自动或人工门禁按企业发布规范推进；
6. 回滚应用时保证 schema 仍向后兼容。

## 6. 上线 Go/No-Go 清单

- [ ] MMR/Context/Tool Owner 对边界和契约签字。
- [ ] 所有 Phase 0 TODO 已关闭或有接受风险记录。
- [ ] 生产容量 2 倍峰值压测通过，无未解释的长尾。
- [ ] SLO、告警、Dashboard、On-call 和升级路径可用。
- [ ] 数据备份恢复、Binding 回滚、Provider suspend 演练通过。
- [ ] 安全高危问题为 0，中危有 Owner 和期限。
- [ ] SBOM、许可证清单、镜像签名和依赖版本锁定完成。
- [ ] 日志、Trace、事件无禁止数据。
- [ ] 旧路径回退已演练，回滚 ≤ 15 分钟。
- [ ] 业务验收案例和故障预期已签字。

## 7. 风险与缓解

| 风险 | 早期信号 | 缓解 |
|---|---|---|
| MMR 没有稳定逻辑 profile | 只能导出具体模型列表 | 在 MMR 外围建立最小 profile adapter；不复制内部目录 |
| ARR 演化成总网关 | 要求代理模型/工具流量 | 架构门禁：数据面 API 不进入 ARR；ADR 评审 |
| 约束字段无限扩张 | JSONB 中出现大量查询字段 | 每个字段需要 Owner、Schema、索引和退役政策 |
| 绑定冲突导致随机路由 | 同优先级多候选 | 发布时阻止 + 运行时 `AMBIGUOUS_BINDING` |
| 门户成为事实源 | Backstage 修改直接影响运行 | 单向只读投影；写入只走 ARR Admin API |
| 事件基础设施过度建设 | 一期先上 Kafka/CDC 集群 | Outbox Worker 起步，门槛触发后再复用平台 |
| 新技术栈增加运维成本 | 与 MMR 不同语言/框架 | Phase 0 强制对齐；架构例外需 ADR |

## 8. Phase 4 采用门槛

| 候选 | 触发条件 | 必须比较的基线 |
|---|---|---|
| xRegistry server | 需要跨平台标准 Registry API，且规范已稳定 | 当前 PostgreSQL 领域实现的性能、扩展、迁移和运维 |
| OPA | 多团队独立管理复杂策略，代码发布阻碍治理 | 当前 IAM/PDP + 显式领域校验 |
| Kafka/Pulsar | ≥3 个独立消费者、需要重放、Outbox polling 达到瓶颈 | 当前 Outbox Worker |
| Redis | DB 负载或缓存生效 SLO 经压测不达标 | 应用内缓存 |
| Agent Provider | 至少两个可替换 Agent 服务且有稳定委派契约 | Harness 直接委派 |
