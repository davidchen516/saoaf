# I12 证据：Evidence Index 与事件消费恢复

- 日期：2026-09-24
- 分支：`i12-evidence`
- 环境：真实 PostgreSQL 18.6（goose 逐迁移上库 v9）、Go 1.27.1、`-race`
- 测试输出：`test-results.txt`（9 项 evidence 测试 + 全仓 19/19 摘要）

## 验收场景 → 测试映射（Issue #12 核心验收逻辑）

| GWT | 场景 | 测试 | 结果 |
|---|---|---|---|
| 1 | Happy path：Evidence 关联成功、按 Plan/租户/时间查询 | `TestIngestHappyPathAndIdempotency`（plan.resolved → resolver 行链路 + binding 链路 + Query API 断言） | PASS |
| 2 | 畸形/未知事件类型 → 隔离告警、循环不中断 | `TestIngestUnknownTopicQuarantine` + `TestIngestMalformedPayloadQuarantine`（非对象 payload） | PASS |
| 3 | 重复投递 → 恰一条 Evidence | `TestIngestHappyPathAndIdempotency`（checkpoint 回拨重放 → 幂等吸收 0 新建） | PASS |
| 4 | 多消费者并发 → 无重复 Evidence | `TestCheckpointMonotonic`（4 worker 并发扫批 + 只前进 checkpoint + 跨 worker 去重） | PASS |
| 5 | **两窗口崩溃注入**（DB 提交前 / 提交后·下轮扫描前） | `TestEvidenceCrashMatrix/window_{A,B}`：子进程真实 SIGKILL（`signal: killed` × 2），恢复消费循环收敛，恰 1 条 LINKED Evidence（零重复） | PASS |
| 6 | 乱序/缺失父事件/延迟 24 小时/重放 → 收敛 | `TestIngestOutOfOrderDelayedMissingParent`（乱序 revision 2→1、断链隔离→父出现→Repair 修复 LINKED、24h 延迟事件仍消费、零重复断言） | PASS |
| 7 | 跨租户查询 403 | 查询 API 按 tenant 过滤（调用方隔离——API 层接线沿用 I09 计划查询的 caller-scoped 模式；完整 403 门禁挂 I17 管理面） | PASS（范围内） |
| 8 | 回滚：停止 → 恢复安全 checkpoint → 重放 | checkpoint 只前进不回退 + 重放依赖幂等（GWT#3 路径复证；checkpoint 回拨测试显式证明重放零新建） | PASS |
| 9 | **敏感扫描红/绿** | 红：`TestIngestForbiddenContentRedRun`——含 `model_prompt` 的负向事件被隔离（FORBIDDEN_CONTENT）且**正文不落库**（content 仅存 stub）；绿：分类层 `forbiddenContent` 扫描（prompt/model_response/tool_param/agent_message/credential/secret/password/api_key）+ 既有全仓防线 | PASS |

## 关键设计（预披露）

1. **消费源 = saoaf.outbox_event 的 PUBLISHED 水位**：Evidence Index 在同一控制面 DB 内直接按 `published_seq > checkpoint` 扫描（I10 已发布事件的权威记录）；NATS 为传输层、非权威——与 issue Non-goals（不保存业务正文）一致，Evidence 从不触碰消息平台。
2. **幂等消费 = event_id UNIQUE**（issue 数据不变量）：重复投递被唯一约束吸收；checkpoint 在**同批事务**内单调推进（`ON CONFLICT ... WHERE last_seq <= $2` 只前进守卫）。
3. **两窗口收敛机制**：窗口 A（ingest 事务未提交被杀）→ 重启从旧 checkpoint 重扫全批重做；窗口 B（事务已提交）→ 重扫经 event_id 唯一约束吸收零新建——重放依赖幂等而非回退。
4. **分类顺序固定**：红线（forbidden/malformed → QUARANTINED 且 payload 换 stub 不落库）→ 未知类型 → 断链检测（plan.resolved → resolver 行；binding.* → registry 行；provider.snapshot-changed → provider 行）→ LINKED。
5. **隔离 = 状态而非删除**（审计不变量）：QUARANTINED 行带 reason + 告警日志；`RepairQuarantined` 在父实体修复后重链（QUARANTINED → LINKED），原始事件与 Evidence 永不删除。
6. **保留策略表达**：retention_class 按已批准基线映射（Plan 90d/Change 2y/Snapshot digest 1y/Event 14d）+ retention_until 落列；WORM 归档状态字段（NONE/PENDING/ARCHIVED）为独立 WORM Issue（I23）预留关联，本 Issue 只做状态承载。
7. **Evidence content = 最小事件载荷**：digest（sha256）+ 引用；不落 Prompt/响应/参数/正文（红线在 ingest 分类层强制）。

## 产物

- `migrations/00009_evidence.sql`：saoaf.evidence_record（event_id UNIQUE + 状态 CHECK + 保留/归档列 + 断链原因）+ saoaf.evidence_checkpoint（只前进守卫）。
- `internal/evidence/`：Index（ScanBatch/IngestBatch 单事务/classify/RepairQuarantined/Query/QuarantineDepth/Checkpoint）+ Consumer 循环（含两窗口注入钩子）。

## 已知限制（挂账）

- GWT#7 跨租户 403 的完整 API 门禁（scope + PDP）：查询 API 已按 tenant 过滤；HTTP 接线与 403 门禁挂 I17（管理面）批次。
- 监控仪表盘（消费延迟/隔离深度/checkpoint 滞后）：`QuarantineDepth`/`Checkpoint` 导出接口已备，dashboard 导出挂 I13。
- 真实 ARR/MMR 事件流的**非全 mock**消费运行记录（关闭判定第 2 项）：依赖真实 MMR 环境——与 #11 ledger 同批（本 Issue 的消费运行在 CI 全绿 + 本地记录下以仓库自身真实 outbox 事件驱动，非注入 mock 数据；「真实 MMR 事件」待外部环境）。
- WORM 归档链路：I23（状态字段已就位）。
