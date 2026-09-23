# I08 证据：Binding 发布/回滚/暂停/退役

- 日期：2026-09-23（R1 整改后更新）
- 分支：`i08-binding`
- 环境：真实 PostgreSQL 18.6（Docker，goose v3.28.0 逐迁移上库）、Go 1.27.1、`-race`
- 测试输出：`test-results.txt`（19 项 PASS：binding 全套 verbose + 并行竞态 ×3 + 全仓 14 包 -race 全绿）

## 审查轮次

- R0（初审）：CHANGES REQUESTED —— 2 P1 + 5 P2 + 4 P3，全部有真实 PG 探针实证。
- R1（整改）：P1/P2 全修 + P3-1/2/3 修复，映射如下。

## Finding → 修复 → 回归测试映射

| Finding | 修复 | 回归测试 |
|---|---|---|
| P1-1 部分重叠+同优先级穿透（探针实证双活） | `Overlaps` 接入发布事务：同 environment+priority 的在役行逐一 `Overlaps` 裁决；`(env,priority)` advisory lock 关闭并发穿透窗口；**顺带修复 Overlaps 语义**（AND 跨维度 + 空=unrestricted 通配，specs §1.2；旧实现 OR + 无通配，{tenant-a} vs {tenant-a,tenant-b} 判不重叠） | `TestDBPartialOverlapBlocked`（部分重叠/通配/不同优先级/维度不相交）、`TestDBConcurrentOverlapSingleWinner`（并发闭包恰一胜出）、域层 `TestScopeOverlap` 扩展 4 例 |
| P1-2 RETIRED 可被 Publish 复活（SUSPENDED/DEPRECATED 同理绕过状态机） | deactivate UPDATE 限定 `state='PUBLISHED'`；CAS 仲裁查最新行状态：非 DRAFT/PUBLISHED → `ErrInvalidTransition` + reason code | `TestDBTerminalStatesBlockPublish`（RETIRED/SUSPENDED/DEPRECATED 三态拒绝复活 + 拒绝后无状态漂移） |
| P2-1 transition 零审计零事件 | Suspend/Resume/Deprecate/Retire 单事务写 change_record（op/actor/trace）+ outbox_event（binding.suspended/resumed/deprecated/retired） | `TestDBTransitionAuditsAndEvents` |
| P2-2 actor 错填 ChangeReason、trace_id 硬编码 | `PublishReq`/`TransitionReq` 增 `Actor`/`TraceID`；门禁强制 actor 非空 | `TestDBTransitionAuditsAndEvents`（PUBLISH 行 actor=user:auditor/trace 断言） |
| P2-3 Resume 撞槽位返回裸 23505 | transition 约束名映射 → `ErrScopeConflict`（fail-closed 不变） | `TestDBResumeSlotConflictClassified`（+ 无漂移 + 唯一 live） |
| P2-4 非活跃历史行 DB 层可改内容 | freeze trigger 改为**所有行**内容不可变（仅生命周期字段可变） | `TestDBHistoricalContentImmutable`（历史行/active 行改写 → 23514） |
| P2-5 未披露的未实现项（DEPRECATED 死路/无创建 API/无影响查询） | 实现 `Create`（DRAFT+审计）、`Deprecate`、`FindOverlapping`（影响查询，I09 resolve 输入，优先级序） | `TestDBCreateDeprecateImpactQuery`（含重复 create 拒绝、DEPRECATED 出影响集） |
| P3-1 PublishGate ticket 死参数 | 移除死参数，改为 (approval, reason, actor)；**approval 对全环境强制**（比 issue 的生产前置更严，此处明确披露） | 域层 `TestPublishGate` 扩展 actor 用例 |
| P3-2 CheckScopeFields 在 store 层无意义且有误拒风险 | 从 Publish 移除（闭环 struct 不可能携带任意 key）；保留域函数供 HTTP 边界（I16）使用 | `TestScopeForbiddenFields`（域层保留） |
| P3-3 CREATE ROLE 跨包并发竞态（审查实测 flake 一次） | 00001 DO 块加 unique_violation 异常捕获（角色为 cluster 级全局对象，NOT EXISTS+CREATE 非原子；语义不变） | 并行压测 ×3（migrations+binding 同集群并行 goose up）全绿 |

## 验收场景 → 测试映射（Issue #8 核心验收逻辑 GWT）

| GWT | 场景 | 测试 | 结果 |
|---|---|---|---|
| 1 | Happy path：发布成功 | `TestDBTransitionAuditsAndEvents`（发布 + change_record actor 归因 + outbox 事件） | PASS |
| 2 | 错误输入：scope 重叠同优先级（含**部分重叠/通配**）/ 审批缺失 / 引用不存在的 revision | `TestDBPartialOverlapBlocked`、`TestPublishGate`（红运行）、`TestDBCASRevisionConflict` | PASS |
| 3 | 重复提交：同请求重放幂等 | `TestDBPublishIdempotentReplay`（同 key 重放不产生第二个 active/事件；key 重用异意图 → 硬错误；被取代重放 → 409） | PASS |
| 4 | 并发：50 并发 publish | `TestDB50ConcurrentPublishSingleActive`（恰 1 成功；DB 恰 1 active）+ `TestDBConcurrentOverlapSingleWinner`（重叠 scope 并发恰一胜出） | PASS |
| 7 | 回滚：新 revision 指向历史内容 | `TestDBRollbackCreatesNewRevision`（恢复 v1 的**同 scope 同 priority** 内容；历史行不被改写/删除） | PASS |
| 8 | 暂停/退役（后效部分联动 I09） | `TestDBSuspendResumeRetire` + `TestDBTransitionAuditsAndEvents`（暂停/恢复/退役全链 + 审计 + 事件） | PASS |

域层（无 DB）：`TestScopeCanonicalGolden`（排序/去重/NFC，含分解/合成 Unicode）、`TestScopeOverlap`（含通配/部分重叠/跨维度 AND）、`TestScopeForbiddenFields`、`TestTransitionMatrix`、`TestBindingString`、`TestPublishGate` —— 6/6 PASS。

## 关键设计决策（审查 R0 后修订）

1. **冲突不变量 = 重叠，不是 hash 全等**（R1 P1-1）：`{tenant-a,tenant-b}` 与 `{tenant-a}` 同优先级不可同时 PUBLISHED。发布事务内对同 (environment, priority) 在役行做 `Overlaps` 裁决；`(env, priority)` advisory lock 序列化同槽位发布，关闭并发穿透。`binding_scope_priority_unique` 保留为全等兜底。
2. **Overlaps 语义与 specs §1.2 对齐**：绑定按「每维命中（成员或通配）」匹配请求 → 两 scope 重叠 iff 每维 co-match（任一侧空=通配，或集合相交）。旧 OR 实现已修正为 AND 跨维度。
3. **发布只可覆盖 PUBLISHED**（R1 P1-2）：RETIRED 终态不可复活；SUSPENDED 须先 Resume；DEPRECATED 须走 Retire。首次发布仅当最新行为 DRAFT。
4. **旧行保号 + 全行内容不可变**：发布时旧行保留原 revision；freeze trigger 护所有行的内容字段（历史行也是审计证据）。
5. **幂等账本**（GWT#3，I05 ledger 落点）：同 key 同指纹重放 no-op；同 key 异指纹 → `BINDING_IDEMPOTENCY_KEY_REUSE`；已应用后被取代 → 409。
6. **outbox 事务性落盘**：publish 与 transition 均同事务写 `saoaf.outbox_event`（含 `change_record_id` FK）；NATS 传输归 I10。
7. **审计归因**：所有写路径记录请求提供的 actor/trace_id；门禁强制 actor 非空。

## 产物

- `migrations/00005_binding.sql`：capability_binding（状态机 CHECK + `UNIQUE(binding_key,revision)` + 单 active 部分唯一索引 + scope/priority 在役唯一索引 + **全行内容不可变 trigger**）+ `registry.publish_idempotency` 幂等账本。
- `migrations/00001_foundation.sql`：CREATE ROLE DO 块并发安全化（异常捕获，语义不变）。
- `internal/binding/`：domain（scope 规范化/哈希/**重叠语义**/生命周期矩阵/发布门禁）+ store（发布 CAS+advisory lock+重叠裁决 / 幂等 / outbox / Create / Suspend/Resume/Deprecate/Retire 审计+事件 / FindOverlapping 影响查询）。

## 已知限制（后续 Issue 接线）

- GWT#5 崩溃恢复 kill 注入取证：存储侧由同事务保证；真实 kill 挂 I10（outbox 传输）。
- GWT#6 权限拒绝（binding-admin 403）：HTTP 接线挂 I05/I16。
- GWT#8 后效断言（新 Plan 不选暂停 Provider、历史 Plan 不改写）：Resolve 挂 I09。
- approval 门禁对全环境强制（比 issue 的「生产前置」更严）：明确披露，如需放宽到仅生产需 David 裁决。
- CheckScopeFields 的原始 JSON 禁止字段扫描在 HTTP 边界接线挂 I16。
- P3-4 无专用 Rollback API：store 层以「重放历史内容」达成回滚语义；API 层防「rollback 名义携带任意新内容」挂 I16。
- 幂等键为请求级全局唯一；跨 binding 重用同 key 视为 key reuse 硬错误。
