# I11 证据：MMR Adapter 与模型最小闭环（Phase 0/Mock 范围）

- 日期：2026-09-24
- 分支：`i11-mmr-adapter`
- 环境：真实 PostgreSQL 18.6、真实 Prism 5.15.10（`mocks/mmr` 契约，`--errors` 示例选择）、mTLS（CA/SPIFFE 证书链）、Go 1.27.1、`-race`
- 关闭口径：**本 PR 交付 Phase 0/Mock 可判定范围的全部内容**；Issue #11 保持 OPEN，唯一剩余关闭条件是**真实 MMR（vllm-semantic-router）端到端证据链**——外部依赖（`blocked::external-dependency`：既有 Multi-Model Router 与 Agent Harness 由外部交付），与 #5（I05）ledger 同口径挂账。

## 验收场景 → 测试映射（核心验收逻辑，按 Mock 可判定范围）

| GWT/不变量 | 测试 | 结果 |
|---|---|---|
| GWT#1 Happy path：契约头 + 双 ID 关联 + trace 贯穿 | `TestMockSucceededCorrelation`（Prism 200：X-Model-Route-Decision-ID + 计划 ID 回显一致） | PASS |
| GWT#2 profile 不存在/契约漂移：明确失败、不自动切换 | `TestMockProfileNotFoundNoAutoSwitch`（400；decision id 仍可关联——specs §5 五结局关联性；adapter 不改写 profile） | PASS |
| 错误透传（specs §5：MMR 错误不被翻译成 ARR「模型选择错误」） | `TestMockErrorPassthrough`（429 QUOTA 从错误信封提取 decision id；OutcomeForStatus 语义映射） | PASS |
| 双 ID 五结局关联完整性 | `TestCorrelationFiveOutcomes`（SUCCEEDED/FALLBACK/QUOTA/TIMEOUT/FAILED 各一行 + 重放幂等吸收 + 无 ID 拒绝） | PASS |
| 数据面隔离（模型字节不经过 ARR） | `TestForbidFieldScan`（禁止字段代码扫描：canonical YAML/Signals/Decisions/candidates/cascade/backend health 在 internal/+cmd/ 可执行代码零命中——引号内拒绝模式除外）+ adapter 只构造契约头，无模型载荷路径 | PASS |
| GWT#4 线程隔离 | `TestStreamingDoesNotBlockControlPlane`（长/流式调用进行中控制面读取并行完成；adapter 为 Harness 侧库，ARR 进程无模型请求处理器——扫描证明） | PASS |
| 灰度状态机（shadow → 灰度 → 全量；回退保留 Plan/Evidence） | `TestRoutingModeTransitions` + `TestModeStoreScopingAndCAS`（Agent>租户>全局作用域、CAS 并发、`ROLLED_BACK` 带旧静态配置引用、change_record 审计历史） | PASS |
| Snapshot ingest §3.2 | `TestSnapshotIngestFlow`（mTLS 身份 → 校验 → submit+activate → **provider.snapshot-changed 事件同事务**；同 (version,digest) 幂等 no-op；同 version 异 digest → 409 契约漂移；版本回退拒绝） | PASS |
| 校验负向 | `TestSnapshotIngestValidation`（坏 digest/空签名/非法 profile 状态/provider 不匹配/未知 provider 404） | PASS |
| mTLS 工作负载身份 | `TestSnapshotIngestWorkloadIdentity`（无客户端证书在 TLS 握手层即拒——I05 workload verifier 首个 HTTP 接线点）+ 身份错配 403 分支 | PASS |
| Plan 不变性（机制侧） | 幂等 ingest（同 version+digest → 零状态变化）正是「MMR 内部变更不触碰 ARR 契约 → Plan 不变」的实现机制；MMR 内部换 candidates 不产生新 ARR-facing snapshot → 无 ingest → Plan 指纹不变。**端到端实验（真实 MMR 内部变更）属真实联调 ledger** | PASS（机制）|

## 产物

- `migrations/00008_mmr.sql`：`saoaf.model_route_correlation`（五结局双 ID 关联台账，I12 消费）+ `saoaf.mmr_routing_mode`（灰度状态机，变更经 change_record 审计）。
- `internal/mmr/`（module 03.4）：Invocation（契约头构造——X-Resource-Plan-ID/Item-ID/X-Tenant-Ref/traceparent；无模型载荷）、ValidateResponse/DecisionFromError（双 ID 校验 + 错误信封关联）、Correlator（台账写入，(item,decision) 幂等）、ModeStore（灰度状态机 + CAS + 审计）。
- `internal/registry/snapshotapi.go`：`POST /providers/{key}/snapshots`（mTLS SPIFFE 身份 + §3.2 校验 + I07 存储路径 + 幂等/漂移/单调语义）；`ActivateSnapshot` 补 `provider.snapshot-changed` 事件 + 审计行（同事务，事件对同 (provider,version) 幂等——回滚重指不重复通告）。
- 契约微调：200 响应头补 example（Prism 兜底字面量问题），400 例保留 decision id（specs §5 五结局关联性）。

## 审查 R1 整改（Findings → 修复 → 回归映射）

| Finding | 修复 | 回归 |
|---|---|---|
| P1 幂等判定基于「行存在」而非「已激活」——半提交后重放假报 idempotent，version 永久卡死 | CheckSnapshotIngest 幂等要求 state=PUBLISHED；同 digest 未发布 → IngestResumable（跳过 submit、重试 activate）；过期窗口重放诚实 400 | `TestSnapshotIngestHalfCommitReplayResumes`（审查探针 E 正名：DRAFT 重放 → 激活收敛 + 过期重放 400 不假成功） |
| P2-1 SetMode 模式行与审计行不同事务 | SetMode 单事务（CAS upsert + change_record 同 tx 提交） | `TestModeStoreScopingAndCAS` |
| P2-2 竞态 loser 裸 400（丢失幂等 200/漂移 409 分类） | submit 唯一冲突后重跑 CheckSnapshotIngest 重新分类（同 digest→200 / 异 digest→409 / 可恢复→续激活） | `TestSnapshotIngestConcurrentSameKeyConverges`（双并发均 200、恰一次 activate） |
| P3-3 契约 major 校验自引用恒真 | ExpectedContractMajor 入配置（空=未配置跳过；配置后错配 400 CONTRACT_MAJOR_REJECTED） | `TestSnapshotIngestContractMajorConfig` |
| P3-5 仓内测试缺口 | 真版本回退 409（无行 v2 < max v3）与异 SAN 403 分支补测（审查探针 F/G 正名） | `TestSnapshotIngestVersionRegression409` / `TestSnapshotIngestWrongSAN403` |
| P3-7 Mode() 字符串判 no-rows + forbid_test 死代码 | errors.Is(pgx.ErrNoRows) + 死代码清除 | 既有套件 |

## 已知限制 / Ledger（Issue #11 保持 OPEN 的原因）

**真实 MMR 端到端证据链（非 mock）——外部依赖不可用**：
- 生产 Agent → ARR → 真实 MMR 调用记录 + Trace 导出；shadow 与灰度监控对比；真实 kill switch 联动（GWT#8 联动演示）；真实内部模型变更的 Plan 不变性端到端实验；回退演练计时（GWT#7）。
- 这些依赖「既有 Multi-Model Router 与 Agent Harness」（issue 自述 External closure dependency）——与 #5（I05）ledger 同口径：本 PR 把 Mock 可判定的全部范围交付并入 main，真实联调证据待外部环境就绪后补齐关闭。

审查 R1 裁决追加挂账（原 ledger 遗漏）：
- **digest 内容重算未接线**：ingest 幂等/漂移检测建立在发布方自报 digest 上；`registry.VerifyDigest` 存在但未在 ingest 路径调用（mTLS 单一可信发布方下 Phase 0 可接受；真实签名验证与 digest 篡改检测一并挂真实联调批次）。
- **签名仅验非空**：真实签名验证（spec §3.2 签名校验）未实现——同上挂真实联调（依赖企业签名规范确定）。
- **snapshot_version 类型冲突**：spec §3.1 与 mock 契约为 string（"mock-1"），registry 链（00004/I07）为 int——真实联调须解决（涉及 00004 schema 与 I07 接口，不在本 PR 范围内静默改类型）。
- forbid 扫描的引号豁免有理论盲区（string-key 形式）——多层防御（CheckForbiddenFieldsRecursive + DisallowUnknownFields + 表无对应列）为主证据，扫描为辅（README 原文已如此声明）。

其他挂账：
- FALLBACK 结局在 Mock 无对应示例——台账五结局经 Correlator 直接覆盖，Mock 覆盖 SUCCEEDED/QUOTA/FAILED/TIMEOUT 四路径。
- ARR 线程池占用指标（GWT#4 的指标证据）：进程内无代理路径已证；指标导出挂 I12/I13。
- SnapshotPublisher 的拉取备选（spec §3.2 备选）：未实现（推送为首选；拉取留待真实 MMR 的能力盘点）。
- MMR 调用方的 endpoint 解析（企业服务发现 adapter）：Plan item 的 endpoint_ref 即 MMR 服务引用，Harness 侧解析——真实 Harness 联调范围。
