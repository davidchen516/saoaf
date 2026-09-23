# I09 证据：确定性 Resource Resolver 和 Resource Plan API

- 日期：2026-09-23
- 分支：`i09-resolver`
- 环境：真实 PostgreSQL 18.6（Docker，goose v3.28.0 逐迁移上库）、Go 1.27.1、`-race`
- 测试输出：`test-results.txt`（全套 verbose）+ `load-full.log`（15 分钟 100 QPS 负载报告）

## 验收场景 → 测试映射（Issue #9 核心验收逻辑 GWT）

| GWT | 场景 | 测试 | 结果 |
|---|---|---|---|
| 1 | Happy path：确定性 Plan，fingerprint 可复验 | `TestDBResolveHappyPath`（fingerprint 与 Fingerprint() 独立复算一致；同输入不同幂等键 → 同 fingerprint）+ `TestResolverHTTPHappyPathAndIdempotency` | PASS |
| 2 | 错误输入按路由优先级返回唯一错误码 | 域层 `TestValidateRequestMatrix`/`TestErrorRoutingOrder` + `TestDBResolveCapabilityNotFound`（404）/`TestDBResolvePolicyDenied`（422/POLICY_DENIED）/`TestDBResolveAmbiguousBinding`（409）/`TestDBResolveSnapshotExpired`、`TestDBResolveSnapshotNotActive`（424）/`TestDBResolveNoCompatibleRegion`、`TestDBResolveScopeFiltered`、`TestDBResolveSuspendedProviderFiltered`（422） | PASS |
| 3 | 100 次相同幂等请求并发 → 仅 1 个 Plan，全部同 Plan | `TestDBPlan100ConcurrentSameKey`（存储层）+ `TestDBResolve100ConcurrentOnePlan`（端到端） | PASS |
| 4 | 相同 key 不同请求体并发 → 恰一成功一个 409 | `TestDBPlanConcurrentDifferentBodies` + `TestDBResolveIdempotencyEndToEnd` | PASS |
| 5 | 崩溃恢复：无半写 Plan | 幂等键唯一索引裁决 + `CreatePlan` 败者回滚后重读分类（25P02 修复）+ Plan/Item 全行 trigger 不可变 | PASS |
| 6 | Runtime/Admin 隔离 403 | `TestResolverAdminIsolation`（runtime→admin 403；admin→resolve 403） | PASS |
| 7 | 回滚：已生成 Plan 保留不删除 | Plan 状态仅 RESOLVED→{EXPIRED\|REVOKED}（`TestDBPlanLifecycle` + trigger 终态拒绝 + 决策字段不可变） | PASS |
| 8 | 性能：MVP 数据集 100 QPS | 冒烟（CI）：`TestLoadSmoke` 30s@100QPS → **P99 6.2ms、0 错误**；完整（证据）：`TestLoadFull`（SAOAF_LOADTEST_FULL=1）15 分钟 @100 QPS → 见 `load-full.log` | PASS |
| 9 | 降级读：DB 断连 → 未过期 Snapshot 继续可读、写关闭 | `TestResolveDBOutageDegradation`（缓存降级读继续 + degraded 计数 + resolve 503 fail-closed）+ `TestCacheDegradedReadOnOutage`/`TestCacheExpiredRefusedDuringOutage`/`TestCacheColdMissOutage` | PASS |

确定性语料（AC-1）：`TestFingerprintDeterminismCorpus`（同输入+revision set+policy revision → 同 fingerprint；requirement 顺序规范化；binding revision / policy revision / task_ref 变化 → fingerprint 变化）。

快照缓存：`TestCacheSingleflight`（50 并发 Get → 恰 1 次回源）、`TestCacheRevisionInvalidation`（版本失效换新）。

## 关键设计决策（审查关注点预披露）

1. **错误路由顺序固定**（issue 原文）：Schema 校验 → 禁止字段（HTTP 边界 RAW body 扫描——闭环 struct 无法携带未声明 key，扫 remarshal 值会有子串误报，同 I08 P3-2 结论）→ Capability 查找（404）→ Policy 过滤（携带 policy revision；无配置 set 时为**已披露的 pass-through**）→ Binding 消歧（409 同优先级并列）→ Snapshot 有效性（424）→ 候选过滤（422 带逐项 reason codes）。
2. **消歧先于候选过滤**：按 issue 固定顺序，同优先级两个匹配 binding 即 409（即使其中之一的 profile 也不合格）——fail-loud 配置错误检测；I08 的 overlap guard 使经 API 无法造出该状态，测试用 SQL 直插构造。
3. **Snapshot 有效性 = binding 钉住的 snapshot 仍为 provider active pointer 且未过 valid_until**：新 snapshot 发布后旧 binding 必须重发布，不静默用旧 provider 内容（确定性与新鲜度的取舍，保守方向）。
4. **连接池化**：resolver 全路径 + policy store（可选 Pool 字段，增量改动）使用 pgxpool——100 QPS 预算下逐请求 TCP connect 不可行；100 并发幂等测试曾打爆 max_connections 证实必要。
5. **幂等键裁决**：唯一索引为并发仲裁者；INSERT 败者先回滚事务再重读分类（否则 25P02）。
6. **policy revision 进 fingerprint**：policy 允许时 Plan 记录 set_id+version（`TestDBResolvePolicyRevisionCarried`）。
7. **健康探针**不鉴权（specs §3.3）；ready = DB 可读 + ≥1 快照已加载（cmd 接线）。
8. **TTL**：默认 300s，options.max_plan_ttl_seconds 可缩短（`TestDBResolveTTL`）；过期读为派生状态（审计保留期内可查 EXPIRED，不删除）。

## 产物

- `migrations/00006_resolver.sql`：resolver.resource_plan（状态机 CHECK + 幂等唯一 + 决策字段不可变 trigger）+ resource_plan_item（全行不可变 trigger，UPDATE/DELETE 均 23514）。
- `internal/resolver/`：domain（校验/路由 codes/fingerprint）、cache（单航班+soft refresh+降级读）、loader（registry 快照 + active pointer 校验）、service（固定路由管线）、store（幂等 plan 存储）。
- `internal/platform/httpapi/resolver.go` + `cmd/control-plane-api/main.go`：runtime API（resolve/GET plan/health）+ 环境变量门控接线（SAOAF_OIDC_ISSUER + SAOAF_DB_DSN + 可选 SAOAF_RESOLVER_POLICY_SET）。
- `internal/policy/store.go`：可选 pgxpool（增量，非池化路径不变）。

## CI 整改（R1，PR 首轮红→修）

1. **boundarycheck FAIL → 架构合规重构**（ADR-0006：模块间禁依赖）：
   - HTTP 适配器移入 resolver 模块本体（internal/resolver/httpapi.go；internal→platform 是唯一允许方向）；httpapi 仅保留 WriteJSON/NewHealth 等平台设施。
   - resolver 不再 import policy：resolver 定义 `PolicyEngine` 接口（PolicyRevision/PolicyInput 为 resolver 本地类型；`ErrNoActivePolicy` 显式语义），**组合根（cmd/control-plane-api）提供 policyEngine 适配器**；跨模块集成测试落 cmd（TestPolicyEngineAdapterIntegration：真实 policy store + CEL evaluator 的 deny/allow + revision 携带全链）。
   - resolver 包内 policy 相关测试改用 fakePolicy（fake 引擎）；policy↔resolver 的行为等价性由 cmd 集成测试证明。
2. **govulncheck GO-2026-6094（cel-go v0.26.0）**：升级 cel-go → v0.30.0（policy 沙箱操作符表全测通过）；x/exp/protobuf 传递升级。
3. 迁移后 HTTP 测试 goose 路径层级修正。

## 审查 R1 整改（Findings → 修复 → 回归映射）

| Finding | 修复 | 回归测试 |
|---|---|---|
| P1-1 降级读分支吞掉 ErrSnapshotUnavailable（热缓存在 pointer 切换后持续供给已失效快照至 valid_until；探针实测复现） | cache.Get 的 loader 失败分支先判 `errors.Is(lerr, ErrSnapshotUnavailable)` → **驱逐条目并原样上抛**（424 分类），仅其余错误进入降级读；degraded 计数不再被非断连场景污染 | `TestDBResolveWarmCachePointerFlip`（热缓存+pointer 切换 → 424 + 计数零污染） |
| P2-1 readiness 死锁（新实例在 readiness 门控后永远收不到首个 resolve 来预热缓存） | `SnapshotCache.WarmupActive`（启动时预载全部 active published 快照）+ cmd 接线；warmup 失败非致命（readiness 如实反映） | 启动路径（cmd 日志）；readiness 语义不变 |
| P2-2 HTTP 禁止字段负向测试死代码（构造未断言） | 真实 POST + 400 断言 + envelope 不回显 payload 内容断言 | `TestResolverHTTPInputHygiene` 扩展 |
| P3-1 路由顺序文档与实现不一致 | 文档改为与 issue 原文一致的「Schema → 禁止字段 → **Policy → Capability** → 消歧 → Snapshot → 候选」；HTTP 边界 RAW 扫描先于 JSON decode 的次序差异（共用 INVALID_REQUIREMENT、外部不可观察）已在 service.go 注释与本文说明 | — |
| P3-3 provider.contract_version 硬编码空串 | PlanItem 增 ContractVersion（migration 00006 列 + 落库 + 响应输出） | 既有套件 + happy path 字段断言 |
| P3-4 CreatePlan 一切 23505 均视为幂等冲突 | 按约束名分流：`(caller_ref, idempotency_key)` 唯一 → 幂等重读；PK 碰撞 → 500 要求新请求 | 既有幂等套件回归 |
| P3-2/3-5/3-6 | 值子串 fail-closed 语义（调用方命名约束）写入本文已知限制；`X-Saoaf-Environment` vs spec `X-Environment`、traceparent 强制、环境枚举校验挂契约冻结批次；消歧 fail-loud 的多 region 配置指南挂平台文档 | — |
| P3-7 load-full.log 版本口径 | 在最终 HEAD 重录 15 分钟跑批（见下） | 重录替换 evidence |

### 审查 R1 结论摘要（完整报告回填于 PR #43 评论）
- 39/39 resolver 测试 + 全仓 16/16 包 -race 由审查员在 8fd75fb 亲自复跑全绿；负载冒烟复跑一致（p99 6.39ms）。
- 8 项对抗探针：确定性反证/幂等风暴/路由优先级/越权矩阵全 PASS；探针 E（热缓存 pointer 切换）复现 P1-1 → 已修。
- 审计角色（resolver.audit）读取路径由审查员首次实证放行语义（200）。

## 已知限制（挂账）

- GWT#8 的 2 倍峰值 60 分钟稳态归 #21（issue 明示）。
- 监控仪表盘（resolve 延迟/错误率/fingerprint 命中）与降级告警导出：挂 I12/I13（监控面）；本轮负载报告为本地跑批产物。
- Resolve 审计：plan 本身即审计载体（trace_id/request_id 可关联）；resolve 未另写 saoaf.change_record（runtime 读路径 vs 管理写路径的审计口径——如需可挂 I12/I16 裁决）。
- 无 Policy set 配置时 pass-through（记录空 revision）——已披露；生产建议配置 SAOAF_RESOLVER_POLICY_SET。
- OpenAPI 3.1 契约文件与消费者驱动契约测试（specs §7/§8 清单）：挂 contract 冻结议题（与 I18-20 fabric 契约同批）。
- 每租户/每 caller 限流池细分、cursor 分页管理 API：挂 I16。
