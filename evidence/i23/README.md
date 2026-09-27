# I23 证据：Evidence WORM 对象存储准入与归档链路

- 日期：2026-09-27 | 分支：`i23-worm-archive` | 环境：Go 1.27 + 真 PG 18.6 + MinIO（RELEASE.2025-01-24，S3 Object Lock COMPLIANCE）

## 交付范围（仓库内可判定面）

**域模块 `internal/evidencepack`**（module 03.8 归档链路，SQL 共享 schema 边界，ADR-0006）：

- **Pack 状态机**（迁移 00013）：`PENDING → WRITING → LOCKED → VERIFIED`；失败分类 `RETRYABLE`（存储瞬态）/`QUARANTINED`（digest 不符/数据级）。**未锁定对象绝不标记 VERIFIED**（MarkVerified 只在 LOCKED 状态 + 已记录 object_version 上转移）。
- **幂等逻辑 pack**：`pack_id` 内容寻址（窗口记录排序规范化 + policy version 的 SHA-256 派生）——同窗口永远同 pack；并发 worker 竞争同一窗口 → **单逻辑结果**（唯一 pack_id + ON CONFLICT + record 级 UNIQUE link）。
- **归档编排**（archive.go）：claim（pack_id ON CONFLICT + record 级 link 去重 + 前向 checkpoint）→ 写 manifest（仅允许元数据：record id/digest/租户别名/policy 版本/retention）→ **PutLocked（COMPLIANCE 保留）** → 服务端 digest 核对 → 锁定账本状态 → **VERIFIED 转移与 index link 原子提交**（同一事务——link 永不指向未锁定/未验证版本）。
- **四窗口崩溃恢复**（issue 核心场景）：W1 manifest 上传后/MarkWritten 前、W2 MarkWritten 后/锁定前、W3 锁定后/link 前、W4 link 后/VERIFIED 前（结构性消除——MarkVerified 原子）。`Recover` 从任意在途状态收敛到 VERIFIED，无丢失/无重复/无未验证伪成功——**四窗口各一测试**（真 PG）。
- **S3 对象存储抽象**（ObjectStore 接口 + MinIO 实现）：版本化写入、COMPLIANCE 保留、digest 读回、**retention 只可延长**（ledger 与对象存储双层拒绝缩短）、legal hold set/query/clear。
- **worker 接线**（env 门控）：`SAOAF_WORM_ENDPOINT` 等配置齐备才启用归档循环；失败不崩溃 worker（pack 账本记录 RETRYABLE/QUARANTINED，下轮 tick 重试）。

## 测试证据

| 套件 | 结果 | 覆盖 |
|---|---|---|
| `pack_test.go`（真 PG -race，内存 WORM fake） | **7/7 PASS** | happy path（VERIFIED+links+retention 覆盖窗口）/ **并发 4 worker 单逻辑 pack**（同 pack_id、单 manifest 版本、8 records 8 links 无双归档）/ **四窗口崩溃恢复**（W1-W4 各一子测试）/ **digest 不符 → QUARANTINED**（0 links）/ 存储故障 → RETRYABLE → 恢复重试成功 / **retention extend-only**（双层 shrink 拒绝）/ **link 只在 VERIFIED 提交**（WRITING 中途 0 links；未锁定 MarkVerified 被拒）/ pack_id 确定性（排序规范化、policy 敏感） |
| `minio_store_test.go`（docker 门控：SAOAF_TEST_MINIO=1） | CI worm-archive job（PG+MinIO services） | **版本化 COMPLIANCE 写入 + digest 读回 + 删除被服务端拒绝**（WORM 服务器语义）/ retention extend-only（服务器侧）/ legal hold set/query/clear / **端到端**（索引窗口 → VERIFIED pack → 真实锁定版本 + links） |

## 负向与不变量矩阵（issue 验收逻辑）

- 删除绕过：COMPLIANCE 锁定版本删除 → **服务器拒绝**（MinIO 集成测试断言错误返回）
- 期限缩短：retention shrink → **ledger 层（RETENTION_SHRINK reason）+ 对象存储层（服务器拒绝）双层拒绝**
- delete marker 不影响锁定版本：版本化桶语义（MinIO 集成覆盖版本寻址）
- 未锁定对象不得 VERIFIED / index 不得引用未锁定版本：**MarkVerified 事务原子 + no-link-before-verified 测试**
- 两个 worker 并发归档同一 pack → 单逻辑结果：**并发测试（4 worker）**
- 中断恢复无半完成 manifest：**四窗口恢复测试**
- 存储不可用：待归档窗口保留（checkpoint 不前移），pack 账本 RETRYABLE，在线索引不伪报已归档（link 未提交）
- 禁止内容：manifest 仅允许元数据（BuildManifest 白名单字段 + 租户别名化）；归档正文是索引已存储的最小事件载荷（红线分类在 I12 消费时已执行）

## 挂账（WORM 生产准入——外部依赖，同 #5/#11/#13 先例）

issue 明文「Mock S3 API 或普通 versioning 不作为 WORM 合规证据」。本 PR 交付：
- 完整归档链路域模块 + 不变量 + 四窗口恢复 + 并发幂等（真 PG 验证）
- S3 协议机械化验证（MinIO Object Lock COMPLIANCE：版本化写入/digest/删除拒绝/extend-only/legal hold——CI 常驻）
- **生产准入证据挂账**：企业 WORM 存储选型/准入报告（存储产品版本、许可证/合同约束、主权地域与跨故障域冗余、SSE-KMS、审计日志、KMS 撤销/恢复演练、跨故障域恢复 RTO/RPO）——需要企业对象存储、KMS、安全/存储/合规团队资源（issue External 依赖）。到达后：`NewMinIOStore` 换生产 endpoint 即接线（接口已抽象）。

## 已知边界

- MinIO 集成测试在本地无 MinIO 时 SKIP（SAOAF_TEST_MINIO 门控）；CI worm-archive job（PG+MinIO services）常驻执行
- `PutLocked` 不带 Expires（生命周期由 retention 治理——WORM 语义）
- manifest 的租户标识为可配口径（当前 `t-<tenant_ref>` 带租户引用——**非匿名化**，企业准入时按合规要求定正式别名规则；与 I12 索引红线口径一致）
- **legal hold / 权限分离**：`Store.SetHold`（HoldGate 授权接口——clear 需显式授权拒绝 unauthorized）+ ledger `legal_hold` 列双写；**归档写入者/验证者/hold 解除者的角色分离接入企业 IdP 后实装**（企业准入挂账项的一部分）

## 存储后端说明（SeaweedFS 替换 MinIO）

集成测试的 S3 Object Lock 后端使用 **SeaweedFS 4.47**（开源、活跃维护、S3 COMPLIANCE retention/legal hold/版本化齐全），原因：MinIO 开源服务器项目已归档下架（docker hub/quay/dl.min.io 均返回 410 Gone——社区版停止分发）。`minio_store_test.go` 的 4 项集成测试对 SeaweedFS 全绿：版本化 COMPLIANCE 写入+digest 读回+**删除被服务器拒绝**、retention extend-only（服务器拒绝缩短）、legal hold set/query/clear、端到端归档。与生产准入的关系不变：**任何开源/自建 S3 的协议验证都只是机械化验证**，WORM 合规证据以企业存储准入报告为准（挂账）。

## 审查 R1 整改（CHANGES REQUESTED → 全项修复）

| Finding | 修复 | 回归测试 |
|---|---|---|
| P1-1 稳态约一半记录永久跳过（checkpoint 只在 existing>0 分支推进，成功路径从不推进） | ClaimWindow **按 link 存在性过滤窗口**（只把未归档记录组包；已归档前缀只推进 checkpoint——绝不整窗跳过）；归档成功后 **AdvanceCheckpoint** 推进到 last_record_id | TestSteadyStateArchivesEveryRecord：三轮 tick（5+5+0 记录）→ 10/10 ARCHIVED、2 个 VERIFIED pack、0 跳过 |
| P1-2 Recover 零生产调用方（崩溃/RETRYABLE pack 永不恢复；注释宣称「下轮 tick 重试」与事实相反） | **ArchiveOnce 开头接入 PendingPacks sweep**：每个 tick 先把全部非终态 pack 驱动到终态再取新窗口；worker 注释改为如实描述机制 | TestWorkerLoopRecoversCrashedPack（WRITING 崩溃 + 10 tick 无新记录 → VERIFIED）+ TestWorkerLoopRetriesRetryablePack（存储故障→RETRYABLE→恢复→VERIFIED） |
| P1-3 W3 真实时钟下永久卡死（锁定后恢复用恢复时钟重算 retention 窗口——晚 1 分钟即失败） | 锁定校验改为 **pack 行记录的 retention_until（锁定时承诺）** 为基准；对象锁低于承诺时 **ExtendRetention 补足**（retention 只增，WORM 合法）后再验 | TestW3RecoveryWithClockDrift（锁定时钟 T0、恢复时钟 T0+1h → VERIFIED） |
| P2-1 权限分离/legal hold 未交付 | **Store.SetHold**：HoldGate 授权接口（clear 需显式授权）+ ledger legal_hold 列 + 对象存储双写；未授权 clear → EVIDENCEPACK_HOLD_UNAUTHORIZED 且 ledger 不变 | TestHoldGateAuthorizesAndDenies（set 过、未授权 clear 拒绝+ledger 不变） |
| P3-1 「FOR UPDATE SKIP LOCKED」虚假声称 | 注释与 evidence 改为如实机制（pack_id ON CONFLICT + link 去重 + drive-lock 仲裁） | 文本 |
| P3-2 状态矩阵与 SQL 漂移 | ValidTransitions 对齐 SQL WHERE 单一执法源（PENDING→RETRYABLE/QUARANTINED 补入、LOCKED→RETRYABLE 移除） | 文本 |
| P3-3 RetentionUntil 吞掉非「无 retention」错误 | 仅 NoSuchObjectLockConfiguration 归 nil；auth/网络错误传播 | 代码路径 |
| P3-4 worm_status 不更新 | **MarkVerified 同事务**把窗口内 evidence_record.worm_status 置 ARCHIVED | TestSteadyStateArchivesEveryRecord 断言 worm_status=ARCHIVED |
| P3-5 CI weed 无校验和 | md5sum -c release 资产校验 | CI |
| ——（整改中新发现）sweep 重开双驱动窗口 | **TryDriveLock**（pg_try_advisory_lock per pack）：sweep 与新 claim 路径都先取驱动锁，另一 worker 驱动时跳过——多进程 HA 亦成立 | TestArchiveConcurrentSingleLogicalPack 维持 4-worker 单 manifest 版本 |

整改后：`internal/evidencepack` 13/13（真 PG -race，7 原有 + 6 新回归含四探针场景）；CI worm-archive（SeaweedFS 4.47 + md5 校验）。

## 审查 R2 整改（CHANGES REQUESTED → 全项修复）

R1 全部 findings 亲证 FIXED（四探针重放绿）；R2 在整改新面上命中 4 缺陷 + 1 测试死分支，全部修复：

| Finding | 修复 | 回归测试 |
|---|---|---|
| R2-P1-A TryDriveLock 解锁 context 在获取时起算 5s——慢驱动（真实 S3 上传常态）泄漏 advisory lock，pack 全局不可驱动 | release 时**新建**短超时 context | TestDriveLockReleasedAfterSlowDrive（6s 驱动后独立 session 可重取锁） |
| R2-P1-B MarkVerified 的 worm_status 用 BETWEEN 裸范围——窗口内 QUARANTINED/外部记录被翻成 ARCHIVED（合规台账对审计撒谎） | UPDATE 范围改为 **IN (SELECT record_id FROM link WHERE pack_id)**——精确到本 pack 的 linked 记录集 | TestQuarantinedRecordNotArchivedFlag（隔离记录保持 NONE、4 linked → ARCHIVED） |
| R2-P2-A Recover 用 BETWEEN 重载与 claim 过滤窗口口径分叉——过滤窗口崩溃 pack 永久 WRITING 僵尸（每 tick 报错） | 重载镜像 claim 语义（BETWEEN + LINKED + NOT-EXISTS-link）；计数漂移时 **QUARANTINED 终态化**（诚实 reason；存活记录经正常循环重打包——覆盖无损失）；PENDING 崩溃行转 RETRYABLE（sweep 不再永扫僵尸，R2-P3-A 同修） | TestFilteredWindowPackRecoverConverges（过滤窗口+中途修复 → 终态 + unarchived=0） |
| R2-P2-B TestW3RecoveryWithClockDrift 死分支（archivePack 直通 VERIFIED 后提前 return，延迟恢复分支从未执行——插桩 panic 不触发） | **重写**：手工驱动到 LOCKED（断言 setup 状态），再以 T0+1h 时钟 Recover | 新版真实到达延迟分支（setup 断言 LOCKED，不走 VERIFIED 短路） |
| R2-P3-B checkpoint 越过慢事务提交间隙（当前单 consumer 顺序事务不可达成；ingest 并发化即升级丢档） | store.go checkpoint 注释 + evidence 本行**显式记录前置依赖**：**checkpoint 语义依赖 ingest 单 writer 顺序提交**——并发化 ingest 时必须重设计扫描边界 | 注释+本表 |

整改后：`internal/evidencepack` **16/16**（真 PG -race：13 R1 后 + 3 R2 探针收编）。
