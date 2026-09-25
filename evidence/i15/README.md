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

## 审查 R1 整改（Findings → 修复 → 回归映射）

| Finding | 修复 | 回归 |
|---|---|---|
| P1-1 审计断链（SUCCEEDED→REMEDIATION_OPEN 两条内联写路径绕 transition()——审计链 tail ≠ 实际状态；AddFinding 两语句非事务） | 提取 `txTransition`（CAS + 审计行的事务内构件）；Finish 的 mutate 钩子与 AddFinding 均改走矩阵转换带审计；AddFinding 整体单事务；transition() 审计行紧跟基础 CAS（日志序=边序） | `TestAddFindingWritesAuditRow` + `TestHappyPathFullCycle` 审计链 6 边逐位断言（软化断言已硬化） |
| P2-1 ABORTED 后 worker 照常执行外部步骤 | `ClaimNext` 前置 drill 状态检查（非 RUNNING 不领取）——人工终止立即停止外部副作用 | `TestWorkerStopsOnAbortedDrill`（calls=0） |
| P2-2 CLOSED 可事后加 Finding | AddFinding 状态守卫：CLOSED/未启动态拒绝（reason code） | `TestClosedDrillRejectsFindings` |
| P2-3 矩阵收窄（FAILED/ABORTED 无整改闭环） | **按 issue 字面矩阵仲裁**：{SUCCEEDED\|FAILED\|ABORTED} → REMEDIATION_OPEN 全部开放（issue 原文即此；前实现的收窄是未记录的自行决定） | `TestFailedDrillOpensRemediation`（含审计行 + 闭环 + Close） |
| P2-4 矩阵测试自证（纯函数测自己的副本） | 吸收审查探针 A2：store 层每对状态真实转换尝试（合法边成功+恰 1 审计行；非法边 reason+状态不变）——`Transition` 通用命令面导出 | `TestFullTransitionMatrix`（store 层 72 对） |
| P2-5 RUNNING→ABORTED 人工中断无确定性测试 | 新增确定性测试（含审计行 + 双 abort 拒绝） | `TestRunningAbortDeterministic` |
| P3 顺带 | RunStep claimed_by 守卫；死列写入（scheduled_at/started_at 等）；A1 探针（直接 SQL 62/62 非法边放行）按全仓口径挂账——DB 层无转换 trigger 为既有模式（binding/policy 同），应用层唯一防线 | — |

## 审查 R2 整改（复审新发现）

| Finding | 修复 | 回归 |
|---|---|---|
| R2-P2-1 AddFinding/Close TOCTOU（200 轮 19 命中「CLOSED drill + OPEN finding」——AC4 直接违反） | AddFinding 的状态读加 `SELECT ... FOR UPDATE`——与 Close 的计数查询在行锁上串行化，任何交错下守卫可见全部已提交 Finding | `TestAddFindingCloseTOCTOUInvariant`（20 轮并发交错；不变量断言 CLOSED 永不带 OPEN finding） |
| R2-P2-2 导出的 Transition 通用面绕过全部守卫（REM→CLOSED 带 OPEN finding 成功、自批成功、零 finding 开 REM） | **去导出**（transition 保持包内原语）；矩阵测试的守卫边改走真实命令（Approve/Close）——守卫语义成为矩阵断言的一部分 | `TestGuardedEdgesEnforced`（open-finding 拒 + 空证据拒 + 自批拒）+ TestFullTransitionMatrix 改造 |
| R2-P3-2 审计排序无 tiebreaker | `ORDER BY at, id` | — |
| R2-P3-1 ABORTED→REM 半闭环 | 挂账：ABORTED drill 的 result_evidence 无供给口（仅 Finish 从 RUNNING 可写）——tech-lead 裁决给证据供给口或接受 REM(aborted)→ABORTED 为唯一出口（后者为当前行为，已记录） | — |

## 已知限制（挂账）

- GWT#7 HTTP 403 完整门禁（exit-admin scope）→ I17。
- 真实 staging/生产完整演练记录（关闭判定第 2 项）→ 发布阶段（测试级真实 PG 全流程演练已交付——两窗口 + 全状态序列 + Evidence 关联；与 I14 R2 终判同口径）。
- 超时告警事件化（outbox）→ I17。
- Drill 步骤的 HTTP 调度端点 → I17 管理面。
