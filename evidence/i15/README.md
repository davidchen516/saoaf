# I15 证据：Exit Drill 状态机和整改闭环

- 日期：2026-09-24 | 分支：`i15-drill` | 环境：真实 PostgreSQL 18.6（goose v12）、Go 1.27.1、`-race`

## GWT 映射

| GWT | 测试 | 状态 |
|---|---|---|
| #1 完整审批链全程 + Finding 整改 + CLOSED | TestHappyPathFullCycle（含审计轨迹） | ✅ |
| #2 非法跳转全矩阵 422 且状态不变 | TestFullTransitionMatrix（9 态 × 8 目标全对断言 + store 层每步 CAS 校验） | ✅ |
| #3 重复审批幂等 | TestDuplicateApprovalIdempotent（第二条 no-op，首审批人不变） | ✅ |
| #4 cancel 与推进并发竞争 | TestAbortVsProgressRace（合法序列一致性 + 败者全分类零裸错——SCHEDULED→{RUNNING,ABORTED} 可合法交错） | ✅ |
| #5 崩溃窗口一：外部动作完成确认前被杀 | TestCrashWindowsKill9/window_ONE（真实 SIGKILL；恢复重 claim 重执行，外部按幂等键去重——at-least-once + 外部 dedup = 有效一次） | ✅ |
| #6 崩溃窗口二：RUNNING 中重启 | TestCrashWindowsKill9/window_TWO（步骤 1 DONE 后被杀；租约过期恢复，DONE 不重复、PENDING 继续） | ✅ |
| #7 权限拒绝/发起人自批 | Approve 服务端强制审批人≠发起人（TestHappyPath 内负向） | ✅ 范围内 |
| #8 终止语义 | Abort 合法矩阵路径（SCHEDULED/RUNNING/REMEDIATION_OPEN）+ 审计行；DB 不伪造外部撤销 | ✅ |
| #9 超时 | TestTimeoutSweepRequeue（过期 RUNNING → PENDING 可重领）+ TestRetryBudgetExhaustion（预算耗尽 → step FAILED → drill FAILED） | ✅ |

## 关键设计

1. **显式全矩阵**：`legalTransitions` 逐对枚举（issue 原文矩阵）；transition() 三重校验（矩阵合法性 + from 状态 CAS + mutate 钩子），矩阵外 → ReasonInvalidTransition 且状态不变。
2. **状态写序**：基础 CAS 转换先行，mutate 钩子后置（Finish 可在单事务内 RUNNING→SUCCEEDED→REMEDIATION_OPEN）。
3. **外部步骤幂等**：每步 idempotency_key（UNIQUE 约束）+ Worker 租约（SKIP LOCKED 领取 + 过期重领）+ at-least-once 执行 + 外部系统按键去重。
4. **CLOSED 硬前置**：无 OPEN Finding + result_evidence 非空（缺一 reason code 拒绝）。
5. **审批分离**：approver ≠ initiator 服务端强制。
6. **审计**：每状态迁移一条 exit_drill_transition（监控导出 = TransitionLog）。

## 已知限制（挂账）

- GWT#7 HTTP 403 完整门禁（exit-admin scope）→ I17。
- 真实 staging/生产完整演练记录（关闭判定第 2 项）→ 发布阶段（测试级真实 PG 全流程演练已交付——两窗口 + 全状态序列 + Evidence 关联；与 I14 R2 终判同口径）。
- 超时告警事件化（outbox）→ I17。
- Drill 步骤的 HTTP 调度端点 → I17 管理面。
