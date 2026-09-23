# I06 Sovereignty Policy 验证证据（2026-09-23）

环境：真实 PostgreSQL 18.6（docker）；goose CLI v3.28.0；cel-go v0.26.0。

## 域层 10/10（-race）+ DB 层 4/4（-race，真实 PG）= 14/14 全绿（policy-suite-results.txt）

| 测试 | GWT/不变量 |
|---|---|
| TestStateMachineFullMatrix | 全转换矩阵：非法路径全拒（SUPERSEDED 仅允许→ACTIVATED 回滚） |
| TestPublishActivateLifecycle | GWT#1 happy path |
| TestPublishedImmutable | 已发布不可回 DRAFT；SUPERSEDED→DRAFT/PUBLISHED 非法 |
| TestPublishActivateIdempotent | GWT#3 幂等（重复发布/激活 no-op） |
| TestRollbackActivatesPrevious | GWT#7 回滚=激活上一 revision；历史 digest 不变 |
| TestConcurrentActivationSingleWinner | GWT#4（域层）终态恰一 ACTIVE |
| TestEvaluationDeterministicCorpus | 关闭判定①：50 轮×5 输入固定语料，结果/revision 引用完全一致 |
| TestSandboxNegatives + TestNonBoolResultRejectedAtEvaluate | 关闭判定①：语法/类型/超长(4096)/未授权函数(duration/timestamp/自定义)/非 bool 各自 reason code |
| TestSandboxAllowsWhitelisted | contains/size 白名单照常 |
| TestDBSingleActiveInvariant | **数据库唯一部分索引实证**：第二个 ACTIVATED INSERT 被 23505 拒 |
| TestDBConcurrentActivationSingleActive | GWT#4 数据库级：两 goroutine 并发激活→恰一成功一 ErrUniqueActive |
| TestDBRollbackAndImmutablePlanRef | GWT#7 数据库级：回滚后 plan_policy_ref 仍指 v1；重复写入被唯一键拒（引用不可变） |
| TestDBActivationTransactionAbortIsAtomic | GWT#5 数据库级：锁阻塞→释放后收敛，无双 ACTIVE |

## 产物

- internal/policy：Set/Revision（内容寻址 canonical digest）+ CEL 求值器（长度上限/超时/函数白名单/中断检查）+ Store（事务内 supersede→activate，唯一索引并发裁决）
- migrations/00003：policy.policy_revision（CHECK 状态机 + 唯一 (set_id,version)）+ policy_single_active_per_set 部分唯一索引 + plan_policy_ref（引用不可变）

## 已知限制

- Policy 求值暴露为库 API；HTTP 端点随 I09 Resolver 的 plan 组装接线（本 Issue scope 为模块与约束）
- 沙箱内存上限：cel-go v0.26 无程序级内存配额 API；以长度上限+中断检查+白名单承载（Upstream 功能跟踪）
