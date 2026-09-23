# I07 Registry 验证证据（2026-09-23）

环境：真实 PostgreSQL 18.6（docker）；goose v3.28.0。7/7 全绿（-race，registry-suite-results.txt）。

| 测试 | GWT/AC |
|---|---|
| TestFullTransitionMatrix | 全矩阵（6 状态×每对，非法全拒 + Transition() reason code） |
| TestForbiddenFields | 禁止字段负向（physical_model/price/weight/fallback_chain/credentials/prompt） |
| TestSnapshotValidationNegatives | GWT#2 各类 reason code（digest 格式/签名/工作负载/契约 major/有效期） |
| TestDBConcurrentCAS | GWT#4 并发 CAS：恰一成功一 ErrRevisionConflict |
| TestDBSnapshotOrderAndDuplicate | 乱序拒绝 + 同版本不同 digest/同 digest 重放均拒 + 直接 INSERT 23505 |
| TestDBPublishedImmutableAndCandidates | trigger 冻结已发布内容列 + SUSPENDED/RETIRED 不在候选 + PUBLISHED 回到候选 |
| TestDBActivePointerSwitch | GWT#1/#7 pointer 唯一 + v1→v2 切换 |

产物：migration 00004（capability_definition/resource_provider/provider_snapshot/active_pointer + CHECK 状态机 + 唯一约束 + 不可变 trigger）；internal/registry（domain 状态机/校验 + store CAS/submit/activate/candidates）。
已知限制：Capability 表结构与生命周期复用同一状态机（域层矩阵共享）；Owner/scope/审计挂 I05/I08 接线；outbox 事件发布挂 I10。
