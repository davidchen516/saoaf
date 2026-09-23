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

## 已知限制

- Capability 表结构与生命周期复用同一状态机（域层矩阵共享）；Owner/scope/审计挂 I05/I08 接线；outbox 事件发布挂 I10。
- Snapshot 版本单调性无 DB 级兜底（P2-3）：SubmitSnapshot 是 check-then-insert，并发下落后事务可能基于陈旧 max 插入更低版本（同版本号有 UNIQUE 兜底）；后续用 advisory lock 或 trigger 补。
- resource_provider.active_revision 为死列：只读不写（恒为 1），语义由 provider_active_pointer 表承载；下一迭代接线或删列。
- VerifyDigest 无生产调用方（仅测试引用）：store 写路径拿不到原始 payload，内容比对由持有 payload 的调用层承担（I09 Resolver / I11 MMR adapter）。
- profiles 列未持久化 Submit 校验的内容：DB 恒为默认 '[]'；内容承诺由 digest 承载，profiles 列后续要么持久化要么标注暂不承载数据。
- 过期 snapshot 不可回滚：回滚路径重跑 ValidateSnapshot，过期即拒绝（ReasonExpired）——恢复需先生成新 snapshot。
