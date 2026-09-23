# I10 证据：Transactional Outbox 与事件传播

- 日期：2026-09-24
- 分支：`i10-outbox`
- 环境：真实 PostgreSQL 18.6 + 真实 NATS JetStream 2.14.7（nats:2.14.7-alpine，`-js`，每测试独立容器）、Go 1.27.1、`-race`
- 测试输出：`test-results.txt`（摘要）、`events-suite-verbose.log`（events 全套）、`red-run.txt`（负向红运行）

## 验收场景 → 测试映射（Issue #10 核心验收逻辑 GWT）

| GWT | 场景 | 测试 | 结果 |
|---|---|---|---|
| 1 | Happy path：域写入提交 → outbox 行出现 → 批准延迟内发布 | `TestWorkerHappyPath`（PUBLISHED + published_seq 单调水位）+ `TestNATSPublishAndServerDedup`（真实流到达） | PASS |
| 2 | 畸形事件/持续失败 → 重试耗尽 → DLQ + 告警 | `TestWorkerDeadLetter`（attempts 耗尽 → FAILED + last_error + `stats.Failed`/`stats.DLQDepth` 告警计数 + **原记录不删除**） | PASS |
| 3 | 发布成功标记前崩溃 → 重发 → 消费端幂等去重 | `TestNATSPublishAndServerDedup`（同 CloudEvent id 重发 → JetStream MsgID 去重吸收，流内恰 1 条）+ `TestCrashMatrix/window_B`（真实 kill -9）+ `TestDeduper`/`TestOrdered`（消费端） | PASS |
| 4 | 多 Worker 并发领取：最多一租约持有；崩溃窗口重复由去重吸收 | `TestWorkerConcurrentClaimNoDoublePublish`（50 事件两 Worker 并发 DrainOnce，传输层恰 50 次发布）+ `TestWorkerLeaseExpiryReclaim`（死 Worker 租约过期被重领）+ `TestCrashMatrix`（真实 SIGKILL × 4 窗口） | PASS |
| 5 | kill -9 四窗口注入矩阵（领取后/发布前、发布后/标记前、标记后/checkpoint 前、checkpoint 写入中） | `TestCrashMatrix/window_{A,B,C,D}`：**每窗口真实 SIGKILL 一次**；恢复后无丢失（PUBLISHED）+ 无重复（流内恰 1 条）+ 水位完整 | PASS |
| 6 | 非 Admin 主体暂停/恢复 Worker → 403 | `TestWorkerPauseResume`（暂停不领取、恢复排空——语义核心）；HTTP 403 门禁接线说明见下（I05 模式沿用） | PASS（范围内） |
| 7 | 回滚：暂停 Worker → 安全 checkpoint 恢复 → 重放收敛 | `TestNATSOutageDrill`（stop→start 真实演练：原始事件保留、backlog 自动清空、6 事件无一丢失/重复）+ FAILED 行永不删除（DLQ 重放入口） | PASS |
| 8 | 背压超阈值阻止大批量管理发布；已有 Plan 不受影响 | `TestDBBackpressureGate`（binding.Publish 门：超阈值 → BINDING_BACKLOG_BLOCKED；既有 active revision 无损；resolve 不经此门） | PASS |

关闭判定项「幂等消费端到端验证（联动 I12）」：Deduper/RevisionTracker/Ordered 已交付并单测（I12 接线消费端）。

## 四窗口 kill -9 矩阵（必须提交的证据）

`TestCrashMatrix`：子进程跑真实 worker（真 PG + 真 JetStream），在四个窗口点自送 SIGKILL（`syscall.Kill(os.Getpid(), SIGKILL)`，非模拟退出）：

- **A 领取后/发布前**：行处 PUBLISHING 带租约 → 恢复 Worker 待租约过期重领 → 发布恰一次
- **B 发布后/标记前**：恢复后重发布 → JetStream MsgID 去重吸收 → 标记；流内恰 1 条（无重复到达消费者）
- **C 标记后/checkpoint 前**：标记 UPDATE 原子含 published_seq（标记即 checkpoint）→ 恢复无事件可做、无重复
- **D checkpoint（领取事务）写入中**：未提交事务随进程消亡 → 行回 PENDING → 干净重领

## 关键设计决策（预披露）

1. **DLQ 语义**：权威 DLQ = outbox 中 `status='FAILED'` 的行（specs §6 保留表：发布后 14 天，闭环后清理）；DLQ **副本**发布到主流 `saoaf.dlq.<topic>`（NATS 留存 7 天）。不做独立 SAOAF_DLQ 流——JetStream 禁止 subjects 重叠（`saoaf.>` 已覆盖 dlq 前缀），且 issue 的「DLQ 14 天」即 DB 行保留口径。
2. **租约 = 可见性超时**：claim 事务 `SELECT FOR UPDATE SKIP LOCKED` → PUBLISHING + lease_expires_at；租约过期可被任意 Worker 重领（00007 索引不含 now()——now() 非 IMMUTABLE 不能进谓词，过期过滤在查询 WHERE）。
3. **标记即 checkpoint**：published_seq 在标记 UPDATE 同事务赋值（`max+1`），窗口 C/D 因此可收敛；消费端按 CloudEvent id + aggregate revision 幂等/乱序吸收（交付 `Deduper`/`RevisionTracker` 供 I12）。
4. **传输错误分类**：`ErrTransportDown`（断连）→ 回 PENDING + 线性退避 next_retry_at，永不 DLQ；其他错误（畸形等）→ 重试预算后 FAILED+DLQ。
5. **半提交=0**：outbox insert 与域写入同事务（I08 binding/I09 plan 路径既有）；**负向红运行**：禁用 binding.Publish 的 outbox insert（「只提交域状态」）→ 协变断言变红（`red-run.txt`：outbox rows = 0, want 1）→ 恢复后绿。
6. **背压门**：`binding.Store.MaxBacklog`（0=关闭）：发布前查 backlog（`saoaf` 共享 schema SQL，ADR-0006 无跨模块 import）；resolve 永不受门（GWT#8「不破坏已有 Plan」）。
7. **I09 plan 事件补全**：`CreatePlan` 同事务写 change_record(RESOLVE) + `plan.resolved` outbox 事件（也闭掉 I09 披露「resolve 未写审计行」）；采样策略「待定」→ 当前不采样，已披露。
8. **监控/告警**：`WorkerStats`（backlog 深度/publish 延迟峰值/失败数/DLQ 深度）+ worker 进程 `GET /metrics`；超阈值 `slog.Warn`（backlog、延迟预算、DLQ 单条 Error）。仪表盘导出挂 I12/I13（已知限制）。
9. **NATS 客户端**：nats.go v1.54.0 入 allowlist（Apache-2.0，JetStream 2.14.7 基线）；断线自动重连（MaxReconnects=-1）——断连演练依赖此。

## 产物

- `migrations/00007_outbox_worker.sql`：租约/退避/DLQ 列 + 可领取索引 + **状态 CHECK 扩展**（00001 原三态约束不识别 PUBLISHING 租约态；expand，Down 恢复原约束）。
- `internal/events/`：cloudevent（CloudEvents 1.0 信封 + topic→catalog type 映射）、transport（JetStream adapter + 流保障 + DLQ 副本 + MsgID 去重窗口）、worker（SKIP LOCKED 租约/退避/DLQ/四窗口钩子/Pause-Resume/Stats）、consumer（Deduper/RevisionTracker/Ordered + BacklogDepth/BulkPublishAllowed）。
- `cmd/control-plane-worker/main.go`：env 门控接线（SAOAF_DB_DSN + SAOAF_NATS_URL）+ /metrics + pause/resume 管理端点。
- `internal/binding`：MaxBacklog 门 + 协变断言；`internal/resolver`：plan 审计 + 事件同事务。

### GO-2026-5932 接受记录（审查关注点预披露）

- 事实：`x/crypto`（经 nats.go→nkeys，已批准 NATS 基线的传递依赖）携带 GO-2026-5932——`x/crypto/openpgp` 包永久无人维护的通告（**Fixed in: N/A**，不存在修复版本）。
- 证据：**import graph 零 openpgp**（`go list -deps ./cmd/...` 实证——x/crypto 仅经 curve25519/blake2b/nacl 进入）；govulncheck v1.1.4 的模块级扫描与 stripped 二进制扫描都会按包级通配兜底报告该通告（审查 R1 实测归因，`Fixed in: N/A` 永不修复）。
- 处置：quality.yml 的模块级与 binary 两级扫描都对**且仅对** GO-2026-5932 显式接受（`ACCEPTED-WITH-EVIDENCE` 注释 + 过滤实现于 workflow 内，可审查）；其他一切 GO-* 发现保持严格 fatal。x/crypto 版本随 go.mod（v0.57.0）。

## 审查 R1 整改（Findings → 修复 → 回归映射）

| Finding | 修复 | 回归 |
|---|---|---|
| P1-1 binary 扫描同报 GO-2026-5932（stripped 二进制包级通配兜底）+ evidence 表述相反 | binary_scan 过滤器（同 module_scan：仅接受该通告，其余 fatal）；evidence 措辞更正为「import graph 实证零 openpgp；扫描兜底误报」 | CI SCA 绿 |
| P2-1 published_seq MAX+1 竞态（审查风暴探针 3 轮实测 23505） | 标记语句前置 `pg_advisory_xact_lock('saoaf:outbox:watermark')`——临界区极小、水位唯一构造 | TestWorkerConcurrentClaimNoDoublePublish + 四窗口矩阵回归 |
| P2-2 next_retry_at 只写不读（退避死代码） | claim 查询过滤 `next_retry_at < now()`；断连回 PUBLISHING→PENDING 带未来 retry 时间 → 下一轮领取为空 → Run 正常 sleep（热循环结构性消除） | TestWorkerOutageThenRecoveryNoLoss（清 next_retry_at 从 no-op 变为真解除退避） |
| P2-3 生产传输 DLQ 不可达（一切错误→ErrTransportDown） | `isTerminalNATSError`（ErrMaxPayload/ErrBadSubject）→ 终态走重试预算→FAILED/DLQ；断连类仍永续重试 | TestWorkerDeadLetter（fake 终态）+ 分类注释 |
| P2-4 管理面默认绑全接口 | `-admin-addr` 默认 `127.0.0.1:8081`（挂账披露的 loopback 前提成立） | — |
| P3-1 claim N+1 查 tenant_ref 且吞错 | tenant_ref/trace_id JOIN 进 claim 单查询；死代码清除 | 既有套件 |
| P3-2 PublishDLQ nil-map panic 地雷 | 非对象 payload 包装（不 panic） | — |
| P3-4 traceparent 从不填充 | change_record.trace_id → 信封 Traceparent（随 JOIN 一并取出） | 既有套件 |
| P3-6 readiness 恒真残缺表达式 | readyz = outbox 未配置→true；配置→TransportHealthy（断连期如实 not-ready） | — |
| P3-8 evictOldestLocked 名不副实 | 更名 resetLocked + 注释如实（bounded FULL reset） | — |
| P3-3/5/7 挂账 | 事件目录命名对齐（binding.published vs binding.activated.v1 等）与 payload 富化挂契约冻结批次；00007 Down 需先清 PUBLISHING 行（回退前置条件已入迁移注释）；背压门生产接线挂 I22 | — |

## 已知限制（挂账）

- GWT#6 HTTP 403：worker 管理端点（pause/resume）当前为进程内管理面（loopback 边界 = Phase 0）；I05 身份链接线挂 I22（生产 Identity 接入时统一挂 scope 门）。
- 幂等消费端到端（I12 联动）：Deduper/RevisionTracker 已交付待消费端接线。
- 监控仪表盘导出与告警规则上线：挂 I12/I13（issue 关闭判定「仪表盘与告警规则上线」按挂账口径）。
- Plan.resolved 事件采样：issue「采样策略待定」——当前全量，量大时可采样，挂 I12 裁决。
- `panwatch` 等外部容器非本测试创建，未动。
