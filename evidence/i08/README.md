# I08 证据：Binding 发布/回滚/暂停/退役

- 日期：2026-09-23
- 分支：`i08-binding`
- 环境：真实 PostgreSQL 18.6（Docker，goose v3.28.0 逐迁移上库）、Go 1.27.1、`-race`
- 测试输出：`test-results.txt`（binding 详单 + 红运行 + 全仓 14 包 -race 全绿）

## 验收场景 → 测试映射（Issue #8 核心验收逻辑 GWT）

| GWT | 场景 | 测试 | 结果 |
|---|---|---|---|
| 1 | Happy path：发布成功 | `TestDBSuspendResumeRetire`（发布 + change_record + outbox 事件） | PASS |
| 2 | 错误输入：scope 重叠同优先级 / 审批缺失 / 引用不存在的 revision | `TestDBScopeConflictBlocked`、`TestPublishGate`（红运行）、`TestDBCASRevisionConflict` | PASS |
| 3 | 重复提交：同请求重放幂等 | `TestDBPublishIdempotentReplay`（同 key 重放不产生第二个 active/事件；key 重用于不同意图 → 硬错误；过期重放 → 409） | PASS |
| 4 | 并发：50 并发 publish | `TestDB50ConcurrentPublishSingleActive`（恰 1 成功；DB 恰 1 active；败者 ErrRevisionConflict，修改不丢失） | PASS |
| 7 | 回滚：新 revision 指向历史内容 | `TestDBRollbackCreatesNewRevision`（恢复 v1 的**同 scope 同 priority** 内容；历史 4 行不被改写/删除） | PASS |
| 8 | 暂停/退役（后效部分联动 I09） | `TestDBSuspendResumeRetire`（PUBLISHED→SUSPENDED→PUBLISHED→SUSPENDED→RETIRED 全链 + CAS 拒绝后状态无漂移） | PASS |

域层（无 DB）：`TestScopeCanonicalGolden`（排序/去重/NFC 规范化 golden，含 Unicode 用例）、`TestScopeOverlap`、`TestScopeForbiddenFields`、`TestTransitionMatrix`、`TestBindingString`、`TestPublishGate` —— 6/6 PASS。

## 关键设计决策（审查关注点预披露）

1. **revision 唯一性修复**：发布时旧行**保留原 revision**（历史不可变），新行取 `expected+1`；此前实现把旧行 bump 到 `expected+1` 与新行碰撞，任何第二次发布必炸。
2. **scope 冲突索引收窄**：`binding_scope_priority_unique` 仅约束 `state='PUBLISHED' AND is_active` 的在役行。原因：(a) 被取代的历史行参与索引会挡住「回滚到同 scope+priority 的历史内容」——回滚是 Issue 明确要求的场景；(b) SUSPENDED 释放槽位（GWT#8：暂停即不在役，替代 Binding 可发布）。Resume 若与后发布者冲突，由唯一索引兜底拒绝（fail-closed）。
3. **幂等账本**（GWT#3，I05 ledger 计划「storage-level idempotency with I08」落点）：`registry.publish_idempotency(key PK, binding, resulting_revision, request_fingerprint)` 与发布同事务写入。同 key+同指纹重放 → no-op 返回；同 key+异指纹 → `BINDING_IDEMPOTENCY_KEY_REUSE` 硬错误；已应用后被取代 → 409。
4. **outbox 事务性落盘**：发布事务内写 `saoaf.outbox_event`（含 `change_record_id` FK），保证「域状态与 outbox 一致」（GWT#5 的存储侧）；NATS 传输归 I10。
5. **约束名精确映射**：23505 按 `ConstraintName` 区分 → revision 唯一=ErrRevisionConflict / scope 唯一=ErrScopeConflict / single_active=ErrDuplicateActive，并发败者的 409 语义不再误报为 scope 冲突。
6. **CAS 收紧到首次发布**：无 active 行时对最新行（DRAFT seed）的 revision 做 CAS（防 revision 跳号）；不存在的 binding → ErrNotFound。

## 产物

- `migrations/00005_binding.sql`：capability_binding（状态机 CHECK + `UNIQUE(binding_key,revision)` + 单 active 部分唯一索引 + scope/priority 在役唯一索引 + active 内容不可变 trigger）+ `registry.publish_idempotency` 幂等账本。
- `internal/binding/`：domain（scope 规范化/哈希/重叠/生命周期矩阵/发布门禁/禁止字段）+ store（发布 CAS + 幂等 + outbox + 暂停/恢复/退役）。

## 已知限制（后续 Issue 接线）

- GWT#5 崩溃恢复（publish 事务中断→outbox 一致性验证）：存储侧由同事务保证；真实 kill 注入取证挂 I10（outbox 传输）。
- GWT#6 权限拒绝（binding-admin 403）：HTTP 接线挂 I05/I16。
- GWT#8 后效断言（新 Plan 不选暂停 Provider、历史 Plan 不改写）：Resolve 挂 I09。
- scope 冲突的「重叠」判定当前用 canonical hash 精确相等（同 scope 集）；跨 scope 部分重叠 + 同优先级的语义重叠检测由域层 `Overlaps` 提供，接入写路径挂 I09（Resolve 需要遍历候选时统一裁决）。
