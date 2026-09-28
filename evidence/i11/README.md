# I11 证据：MMR Adapter 与模型最小闭环（Phase 0/Mock 范围）

- 日期：2026-09-24
- 分支：`i11-mmr-adapter`
- 环境：真实 PostgreSQL 18.6、真实 Prism 5.15.10（`mocks/mmr` 契约，`--errors` 示例选择）、mTLS（CA/SPIFFE 证书链）、Go 1.27.1、`-race`
- 关闭口径：~~本 PR 交付 Phase 0/Mock 可判定范围的全部内容；Issue #11 保持 OPEN，唯一剩余关闭条件是**真实 MMR（vllm-semantic-router）端到端证据链**~~ → **2026-09-28 第一方决策改判 Mock 收口**（见文末「Mock 收口批次」：真实 MMR Provider 不可提供，Mock 全范围交付后关闭 #11；残余项按归属转移）。

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

---

# Mock 收口批次（2026-09-28，Issue #11 关闭交付）

- 分支：`i11-mock-closure`
- 环境：真实 PostgreSQL 18.6（每测试一次性库 + goose 迁移）、真实 Prism 5.15.10（`--errors` + `Prefer` 示例选择）、Go 1.27.1、`-race`

## 第一方决策记录

**2026-09-28，David（仓库所有者）**：「对于#11 I11，我无法提供 真实的 MMR Provider，请做Mock」

据此，Issue #11 的关闭口径由「真实 MMR（vllm-semantic-router）端到端证据链」改为 **Mock 全范围收口**：上述 ledger 中一切可在 Mock 判定的项，本批次全部交付；依赖真实环境的残余项按归属转移（见「残余披露」）。与 #5（企业 Identity）/ #13（生产运行记录）同属第一方决策口径。

## 逐 ledger 处置

| Ledger 项 | 处置 | 交付物 |
|---|---|---|
| 真实 MMR 端到端：五结局关联闭环 | **Mock 收口** | `internal/mmr/closure_test.go` `TestMockClosedLoopFiveOutcomes`：真实 Prism mock + 真实 PG 台账，五结局（SUCCEEDED/FALLBACK/QUOTA/TIMEOUT/FAILED）各一行，全部绑定 `plan-mock-001`（mock 回显的计划 ID），QUOTA/FAILED 经 `Prefer: code=429/503` 错误示例 + 错误信封关联；TIMEOUT 无 HTTP 响应可关联——测试直接记账（合成 ref 路径，PR #45 审查通过口径：超时的 decision id 只会出现在 MMR 后续 retry/审计导出中，I12 消费） |
| 真实 MMR 端到端：Plan 不变性实验（MMR 内部变更） | **Mock 收口** | `cmd/control-plane-api/mmr_closure_test.go` `TestClosurePlanInvarianceAcrossSnapshotRepublish`（组合层——boundary 禁止 internal/mmr import resolver/registry）：重发布走**真 registry store 路径**（`Store.SubmitSnapshot` + `Store.ActivateSnapshot`，与 ingest API 驱动同一 store；断言 active pointer 移到 v2 证重发布落地），前后各跑**真 resolver**（fresh idem key → 新 plan 行，非幂等重放）：断言 `registry.capability_binding` revision/is_active 逐位不变 + 选中同一 binding + plan fingerprint 不变 |
| 真实 MMR 端到端：kill switch 联动（GWT#8） | **Mock 收口（ARR 半侧）** | `TestClosureKillSwitchBlocksNewPlans`（同文件）：挂起走**真 I08 路径**（`binding.Store.Suspend`，PUBLISHED→SUSPENDED rev CAS）；阻断断言走**真 resolver**（`Resolve` → `NO_COMPATIBLE_PROVIDER`）；挂起前先以同一 resolver 谓词证该 binding 可选中（可证伪性锚点）；在途请求（挂起前已计划）经 Prism mock 503，数据面结局仍入台账（FAILED 1 行） |
| 真实 MMR 端到端：shadow 与灰度监控对比 | **Mock 收口** | `internal/mmr/closure_test.go` `TestMockShadowAndGrayModeBookkeeping`：SHADOW 模式行**先于调用落位**，断言调用时刻 `Mode()=='SHADOW'`（且调用不翻模式），shadow 调用记账 1 行（对比证据）；SHADOW→GRAY→FULL CAS 链 + `change_record` 审计 ≥2 行（entity_kind='mmr-routing-mode'） |
| R1 追加挂账：digest 内容重算未接线 | **FIXED** | `SnapshotDigest`（内容寻址：sha256 over 规范化 snapshot 内容）+ `validateIngest` 重算比对 → 不符 400 `DIGEST_MISMATCH`；`TestSnapshotIngestDigestRecompute`；既有套件全部迁移到内容寻址 digest（`ingestBody` 空 digest 自动重算） |
| R1 追加挂账：签名仅验非空 | **FIXED** | `SnapshotAPIConfig.PublisherKey`（ed25519）：签名须为 base64 的 64 字节 ed25519 over digest 字符串，否则 400 `SIGNATURE_INVALID`；`TestSnapshotIngestSignatureVerify`（正例 + 两负例：未签名占位串——非法 base64 即拦；**错密钥签名**——合法 base64/64 字节，唯 ed25519.Verify 能拦，单撤 Verify 该负例必红）。密钥未配置时保持 Phase 0 行为（非空即可）——测试密钥属 Mock 配置 |
| R1 追加挂账：snapshot_version 类型冲突（spec string vs DB int） | **FIXED** | 命名类型 `SnapshotVersion`：自定义 `UnmarshalJSON` 同时接受 JSON 数字与数字字符串；`TestSnapshotVersionStringForm`：字符串形态以**原始字节过 HTTP 线**（marshal 后字节级替换、直接 POST 不再 re-marshal），断言落库为 int 1；非数字字符串（"mock-9"）过线 400 `INVALID_SNAPSHOT`（decode 错误路径）。契约 `contractlint validate` PASS（schemas=10 examples=11） |
| 其他挂账：FALLBACK 无 Mock 示例 | **FIXED** | 契约 200 响应保留默认 singular `example`（SUCCEEDED，已发布字段——contractlint breaking 闸判定移除即破坏性变更，不能动）+ 新增命名示例 `fallback`（status FALLBACK + 独立 decision/usage ID）+ status enum 扩为 `[SUCCEEDED, FALLBACK]`；`OutcomeFromBody`（200 + body status FALLBACK → FALLBACK）；五结局测试经 `Prefer: example=fallback` 走通。`example`/`examples` 在同一 media type 共存违反 OAS 3.x 互斥——Prism 5.15/contractlint 容忍，作为已知偏差披露（见「残余披露」），整改需协调的契约变更（删已发布字段）另立批次 |
| 其他挂账：ARR 线程池指标 | **不在本批次** | 原挂账归属不变（I12/I13 台账消费与指标） |
| 其他挂账：SnapshotPublisher 拉取备选 | **不在本批次** | 推送为首选的口径不变 |
| 其他挂账：endpoint 解析 | **不在本批次** | Harness 侧解析口径不变 |

## PR #58 审查 R1 整改（Findings → 修复 → 回归映射）

| Finding | 修复 | 回归 |
|---|---|---|
| P1-1 签名验证钉失效（单撤 `ed25519.Verify` 套件仍绿）+ README「错密钥负例」声称失实（系列第 9 次声称/事实不符） | 补错密钥负例：另一 keypair 签 digest（合法 base64、64 字节）→ 400 `SIGNATURE_INVALID`；README 该行改为与测试实况一致 | `TestSnapshotIngestSignatureVerify`；蓝军单撤 Verify → 红（`wrong-key signature = 200, want 400 SIGNATURE_INVALID`） |
| P1-2 Plan 不变性恒绿假钉（republish 走裸 INSERT、registry 包零命中 binding、两读之间无产品代码）+ 注释「drives the actual registry snapshot store」失实 | 测试迁至 cmd 组合层（boundary 禁 internal/mmr import resolver/registry/binding），走真 `Store.SubmitSnapshot`+`ActivateSnapshot` 重发布 + 断言 active pointer 移到 v2 + 前后真 resolver 解析（同 binding、同 fingerprint）+ binding 行逐位不变；失实注释随测试迁移消除 | `TestClosurePlanInvarianceAcrossSnapshotRepublish`；蓝军注入「ActivateSnapshot 清空该 provider 全部绑定」→ 红（`binding row read: no rows in result set`） |
| P1-3 kill switch「不可解析」半侧恒绿（裸 UPDATE 后用同谓词 count 自证自己的写入；binding 级挂起阻断全仓库零可证伪覆盖） | 迁至 cmd 组合层：真 I08 `binding.Store.Suspend`（rev CAS）+ 真 resolver `Resolve` → `NO_COMPATIBLE_PROVIDER` 断言；挂起前先以同一谓词证可选（该锚点使砍谓词探针必红） | `TestClosureKillSwitchBlocksNewPlans`；蓝军砍 resolver binding 谓词（`cb.state='PUBLISHED' AND cb.is_active`）→ 红（`resolve after suspend succeeded (kill switch failed)`） |
| P2-1 shadow 半侧无模式耦合（调用发生在 SHADOW 行插入之前、无代码消费 mode） | SHADOW 行前置到调用之前；断言调用时刻 `Mode()=='SHADOW'` 且调用不翻模式；shadow 记账行断言 | `TestMockShadowAndGrayModeBookkeeping` |
| P3-1 StringForm 字符串形态从未过 HTTP 线（re-marshal 还原为数字形态，「三种形态」声称过头） | 字节级替换后直接 POST 原始字节（不再 re-marshal）；补落库断言（`provider_snapshot.snapshot_version` 为 int 1） | `TestSnapshotVersionStringForm` |
| P3-2 TIMEOUT「适配器错误路径」措辞失实（status==0 分支零适配器代码参与） | README 与测试注释改为「测试直接记账（合成 ref 路径，PR #45 口径）」 | 本表 + `closure_test.go` 注释 |
| P3-3 OAS 互斥违规（同一 media type 上 `example` 与 `examples` 共存） | **尝试折叠被 contractlint breaking 闸否决**（singular `example` 为已发布字段，移除判破坏性变更、无豁免机制）——恢复共存并如实披露为已知 OAS 偏差（Prism/contractlint 均容忍）；整改需协调的契约变更，另立批次 | contractlint validate + breaking（vs f54d725）双 PASS + mmr/cmd 全套件 + 「残余披露」 |
| P3-4 `OutcomeFromBody` fail-open 记账（200 + 不可解析 body → SUCCEEDED） | 残余披露如实记载（见下） | 见「残余披露」 |
| OBS WORM `TestArchiveConcurrentSingleLogicalPack` flake（首轮 `links=0 want 8`、重跑绿、与 diff 无关） | `docs/memory/backlog.md` 立项（与 I23 sweep/claim 双驱动窗口观察同域） | `docs/memory/backlog.md` |

## 残余披露（Mock 收口后仍不声称的）

- **MMR 数据面 kill switch**：本仓库交付的是 ARR 半侧（binding 挂起阻断新解析 + 在途结局记账）；MMR 自身的后端摘除开关按基线归 MMR 所有（issue 原文 External closure dependency）——Mock 无从证明 MMR 侧行为，不声称。
- **GWT#7 回退演练计时**：`ROLLED_BACK` 的状态机语义、旧静态配置引用、审计（`TestRoutingModeTransitions`/`TestModeStoreScopingAndCAS`）已覆盖；**真实系统的挂钟计时演练**属生产运行记录（#13 ledger），Mock 收口不含。
- **ed25519 发布方密钥**：测试用生成的密钥对证明验证逻辑；企业真实发布方密钥分发/轮换属 #5（企业 Identity）范畴。
- **OAS 互斥偏差（R1 P3-3 未整改）**：200 响应 media type 上 singular `example`（默认 SUCCEEDED，已发布字段）与命名示例 `examples.fallback` 共存，违反 OAS 3.x 互斥——Prism 5.15 与 contractlint 均容忍，行为无影响；删除已发布字段过不了 breaking 闸，整改需与消费方协调的契约变更（届时 `example` 折叠进 `examples` 并走破坏性变更评审）。
- **`OutcomeFromBody` 记账取向（fail-open）**：200 + body 不可解析时按状态码记 SUCCEEDED（无法判读 FALLBACK 时倾向 200 的字面语义）——R1 P3-4 如实披露，不改判。
- **TIMEOUT 结局的 decision id**：Mock 收口中为合成（`mrd-e2e-4`）——真实超时的 decision id 只在 MMR 后续 retry/审计导出中出现（I12 消费），Mock 无从取回。

## 回归与 CI

- 全仓 `go build ./... && go vet ./...` + `go test -race -count=1 ./...` + contractlint（PASS：schemas=10 examples=11 negatives=25 consumers=8）+ boundarycheck（31 包）+ licensecheck 全绿。
- 新增 CI job `mmr-mock-closure`（quality.yml，exit-drill 同模式）：PG 18.6 service + goose，依次跑 `./internal/mmr/`（docker-run Prism mock + 真 PG -race）→ `./internal/registry/` 快照 ingest 套件 → `./cmd/control-plane-api/ -run TestClosure`（组合层真路径）——本批次的全部新钉子进 CI 门禁。
- 蓝军探针（/tmp 克隆 @ 本批次 HEAD，R2 前自证）：砍 resolver binding 谓词 → kill switch 红；注入 ActivateSnapshot 清空绑定 → Plan 不变性红；单撤 `ed25519.Verify` → 签名负例红。三支全真红。
