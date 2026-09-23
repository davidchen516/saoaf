# I07 Registry 验证证据（2026-09-23）

环境：真实 PostgreSQL 18.6（docker）；goose v3.28.0。12/12 全绿（-race，registry-suite-results.txt）。

| 测试 | GWT/AC |
|---|---|
| TestFullTransitionMatrix | 全矩阵（6 状态×每对，非法全拒 + Transition() reason code） |
| TestForbiddenFields | 禁止字段负向（physical_model/price/weight/fallback_chain/credentials/prompt） |
| TestSnapshotValidationNegatives | GWT#2 各类 reason code（digest 格式/签名/工作负载/契约 major/有效期） |
| TestDBConcurrentCAS | GWT#4 并发 CAS：恰一成功一 ErrRevisionConflict |
| TestDBSnapshotOrderAndDuplicate | 乱序拒绝 + 同版本不同 digest/同 digest 重放均拒 + 直接 INSERT 23505 |
| TestDBPublishedImmutableAndCandidates | trigger 冻结已发布内容列 + SUSPENDED/RETIRED 不在候选 + PUBLISHED 回到候选 |
| TestDBActivePointerSwitch | GWT#1 pointer 唯一 + v1→v2 切换 |
| TestFullLifecycleViaStoreAPI | P1-1/P1-2 回归：经 store API 走完 VALIDATED→PUBLISHED→SUSPENDED→PUBLISHED→DEPRECATED→RETIRED 全链 + DRAFT 跳转拒绝 |
| TestActivePointerRollback | P1-3 回归：pointer 回滚到已发布 v1；v2 仍 PUBLISHED |
| TestForbiddenFieldsRecursive | P2-1 回归：嵌套 JSONB 禁止字段递归扫描 |
| TestVerifyDigest | P2-2 回归：digest 内容比对（篡改检测） |
| TestDBSubmitSnapshotForbiddenProfiles | P2-1 回归：Submit 侧拒绝嵌套禁止字段 |

产物：migration 00004（capability_definition/resource_provider/provider_snapshot/active_pointer + CHECK 状态机 + 唯一约束 + 不可变 trigger）；internal/registry（domain 状态机/校验 + store CAS/submit/activate/candidates）。
已知限制：Capability 表结构与生命周期复用同一状态机（域层矩阵共享）；Owner/scope/审计挂 I05/I08 接线；outbox 事件发布挂 I10。
