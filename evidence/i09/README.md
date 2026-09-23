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

## 已知限制（挂账）

- GWT#8 的 2 倍峰值 60 分钟稳态归 #21（issue 明示）。
- 监控仪表盘（resolve 延迟/错误率/fingerprint 命中）与降级告警导出：挂 I12/I13（监控面）；本轮负载报告为本地跑批产物。
- Resolve 审计：plan 本身即审计载体（trace_id/request_id 可关联）；resolve 未另写 saoaf.change_record（runtime 读路径 vs 管理写路径的审计口径——如需可挂 I12/I16 裁决）。
- 无 Policy set 配置时 pass-through（记录空 revision）——已披露；生产建议配置 SAOAF_RESOLVER_POLICY_SET。
- OpenAPI 3.1 契约文件与消费者驱动契约测试（specs §7/§8 清单）：挂 contract 冻结议题（与 I18-20 fabric 契约同批）。
- 每租户/每 caller 限流池细分、cursor 分页管理 API：挂 I16。
