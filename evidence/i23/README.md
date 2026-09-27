# I23 证据：Evidence WORM 对象存储准入与归档链路

- 日期：2026-09-27 | 分支：`i23-worm-archive` | 环境：Go 1.27 + 真 PG 18.6 + MinIO（RELEASE.2025-01-24，S3 Object Lock COMPLIANCE）

## 交付范围（仓库内可判定面）

**域模块 `internal/evidencepack`**（module 03.8 归档链路，SQL 共享 schema 边界，ADR-0006）：

- **Pack 状态机**（迁移 00013）：`PENDING → WRITING → LOCKED → VERIFIED`；失败分类 `RETRYABLE`（存储瞬态）/`QUARANTINED`（digest 不符/数据级）。**未锁定对象绝不标记 VERIFIED**（MarkVerified 只在 LOCKED 状态 + 已记录 object_version 上转移）。
- **幂等逻辑 pack**：`pack_id` 内容寻址（窗口记录排序规范化 + policy version 的 SHA-256 派生）——同窗口永远同 pack；并发 worker 竞争同一窗口 → **单逻辑结果**（唯一 pack_id + ON CONFLICT + record 级 UNIQUE link）。
- **归档编排**（archive.go）：claim（FOR UPDATE SKIP LOCKED + 前向 checkpoint）→ 写 manifest（仅允许元数据：record id/digest/租户别名/policy 版本/retention）→ **PutLocked（COMPLIANCE 保留）** → 服务端 digest 核对 → 锁定账本状态 → **VERIFIED 转移与 index link 原子提交**（同一事务——link 永不指向未锁定/未验证版本）。
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
- manifest 的租户标识为别名形（`t-<tenant_ref>`）——与 I12 索引红线口径一致
