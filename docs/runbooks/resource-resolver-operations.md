---
title: AI Resource Resolver 部署与运行手册
version: 1.0.0
status: proposed
owner: TODO-ARR-SRE负责人
reviewers:
  - TODO-ARR技术负责人
  - TODO-数据库负责人
  - TODO-安全负责人
created: 2026-09-21
updated: 2026-09-21
classification: internal
related:
  - ../architecture/ai-routing-platform-4a.md
  - ../plans/resource-resolver-implementation-plan.md
---

# AI Resource Resolver 部署与运行手册

## 1. 服务清单

| 部署单元 | 副本 | 状态 | 扩缩容指标 |
|---|---:|---|---|
| `resource-resolver` | 生产至少 3 | 无状态 | CPU、并发、P99、DB pool 等待 |
| `resource-resolver-outbox` | 2（单事件用 DB 领取锁） | 无状态 worker | backlog、publish latency |
| PostgreSQL | 企业 HA 标准 | 权威状态 | connections、locks、replication lag、disk |
| OTel Collector | 企业共享 | 遥测转发 | queue、drop、export errors |

若 Outbox Worker 与应用同进程，仍应使用独立线程池、连接池配额和开关，避免影响 Resolve。

## 2. 配置与密钥

| 配置 | 来源 | 轮转/变更 |
|---|---|---|
| 数据库连接 | 企业 Secret/KMS | 凭证轮转演练；不写镜像/ConfigMap |
| OIDC issuer/audience | 企业配置中心 | 双版本重叠窗口 |
| mTLS/workload identity | 企业身份平台 | 自动轮转，过期前告警 |
| 服务发现 endpoint_ref resolver | MMR 同一 SDK | 按平台发布 |
| 缓存 TTL/上限 | 版本化配置 | 灰度变更 |
| 限流和请求上限 | API Gateway + 应用 | 双层保护 |
| break-glass | IAM/ITSM 决定引用 | 有时限、双人复核、全审计 |

## 3. 标准部署流程

1. 确认变更单、镜像 digest、SBOM、许可证和安全扫描通过。
2. 检查数据库备份、复制延迟、磁盘余量和当前长事务。
3. 先执行向后兼容的 expand migration；验证 migration checksum。
4. 部署 1 个 canary，流量 1–5%；运行 health、Resolve smoke 和管理只读检查。
5. 观察至少 15 分钟及足够请求量：错误率、P99、DB pool、CPU、outbox lag、cache miss。
6. 按 25% → 50% → 100% 推广；每步保持可回滚。
7. 完成发布后验证三域样例和 trace 关联。
8. 收缩 migration 必须在旧版本全部退出且经过一个回滚窗口后单独执行。

禁止在发布期间同时大批量修改 Binding 或 Provider Snapshot。

## 4. 回滚流程

### 4.1 应用回滚

1. 停止进一步推广，保留证据和 trace。
2. 将流量切回上一镜像 digest。
3. 不回滚 expand schema；旧应用必须与其兼容。
4. 验证 Resolve、MMR 调用关联和 Outbox。
5. 记录影响范围、起止时间和失败版本。

### 4.2 Binding 回滚

1. 找到上一 `PUBLISHED` revision 和变更单。
2. 调用 `:rollback` 创建新的 active revision，不直接改数据库。
3. 观察 `BindingActivated` 和缓存生效；目标 60 秒内。
4. 使用固定验收请求验证 `decision_fingerprint`。
5. 旧 Plan 仍在 TTL 内不可变；必要时通过 Provider suspend/执行端 kill switch 阻断。

## 5. 告警基线

| 告警 | 初始阈值 | 严重性 | 首要动作 |
|---|---|---|---|
| Resolve 可用性 | 5 分钟成功率 < 99.5% | P1 | 检查 DB/发布快照/最近发布 |
| Resolve P99 | 10 分钟 > 100 ms | P2 | 分解 DB、cache、queue、CPU |
| DB pool wait | P95 > 20 ms 或池耗尽 | P1/P2 | 限流、查慢查询/连接泄漏 |
| Snapshot age | 距 `valid_until` < 5 分钟 | P2 | 联系 Provider；验证发布链 |
| Snapshot expired | 任一生产 active Provider | P1 | 新 Plan 受阻，恢复 Snapshot |
| Outbox lag | > 60 秒持续 5 分钟 | P2 | 检查总线/worker/DLQ |
| Ambiguous binding | 生产出现 > 0 | P1 | 阻止相关 Capability，回滚配置 |
| 管理越权/异常发布 | > 0 | P1 安全 | 冻结管理写，通知安全 |
| Prohibited data detected | > 0 | P1 安全 | 隔离日志/事件，停止相关入口 |

阈值在基线压测和两周生产数据后调整，并记录变更原因。

## 6. 事件处置

### 6.1 PostgreSQL 不可用

**表现**：DB health 失败，管理写 503；Resolver 可能使用内存发布快照。

1. 冻结管理写和发布任务。
2. 确认各实例加载的 revision、缓存 TTL 和快照未过期。
3. 仅允许已验证的只读降级窗口；超过窗口后 Resolve fail closed。
4. 数据库团队恢复主库/故障切换；检查 timeline 和复制一致性。
5. ARR 恢复后先只读校验 active pointers、outbox、最近 Plan，再开放写入。

### 6.2 Provider Snapshot 即将/已经过期

1. 查看 `provider_id`、最后成功版本和发布错误。
2. 联系专业 Provider Owner；不要人工延长数据库时间戳。
3. 若 Provider 服务健康但 Publisher 故障，重发同一 digest 的新有效 Snapshot。
4. 如需 break-glass，必须由审批系统签发有时限的例外并记录风险；到期自动关闭。
5. 恢复后检查间隙期间失败 Plan 和是否需要业务补偿。

### 6.3 Binding 冲突或错误解析

1. 用 request digest、plan ID 还原 Capability/Binding/Snapshot revisions。
2. 暂停相关 Binding 或回滚到上一稳定 revision。
3. 清理缓存只通过事件或受控管理操作，不重启全部实例。
4. 使用固定输入重放 Resolve，核对 fingerprint。
5. 添加回归案例并修复发布时冲突检查。

### 6.4 MMR 数据面不可用

ARR Resolve 可能仍成功，因为它不是数据面健康探针。处置由 MMR Runbook 负责：

1. Harness/MMR 依据 MMR 错误和 retry contract 处理；
2. ARR 不重新选择具体模型或绕过 MMR；
3. 若 MMR 整体不可用，MMR Owner 将 Provider 状态置为 UNAVAILABLE/SUSPENDED；
4. 关联 `resource_plan_id` 与 `model_route_decision_id` 评估影响。

### 6.5 Outbox 堵塞

1. 检查消息平台、认证、网络、DLQ 和单事件 payload。
2. 保持业务写，但当 lag 超过变更安全阈值时暂停批量发布。
3. 修复后从最早未发布记录恢复；依赖 event ID 和 revision 幂等。
4. 不手工将 `published_at` 批量置值来“清空告警”。

### 6.6 疑似敏感数据泄露

1. 停止相关 API/Publisher 或日志出口，保全访问审计。
2. 通知安全与数据 Owner；轮转可能受影响的凭证。
3. 定位进入路径、存储、日志、Trace 和事件消费者。
4. 按合规流程隔离/清理，禁止无审计的直接 SQL 删除。
5. 增加 Schema 拒绝、日志脱敏和回归测试后再恢复。

## 7. 备份恢复演练

季度演练步骤：

1. 选择恢复点并在隔离环境执行 PITR。
2. 校验 migration checksum、表/分区、行数和 active revision。
3. 运行固定 Resolve corpus，比较 decision fingerprint。
4. 检查未发布 Outbox，防止恢复环境向生产总线发送事件。
5. 重新发布测试 Snapshot，验证正常推进。
6. 记录 RPO、RTO、人工步骤和改进项。

## 8. 日常运维

每日：可用性/P99、Snapshot age、DB/Outbox、错误码分布、证书期限。  
每周：Binding 变更、冲突失败、缓存命中、慢查询、容量趋势、依赖漏洞。  
每月：SLO/Error Budget、权限复核、退役资源、成本、分区与保留任务。  
每季度：恢复、回滚、Provider suspend、安全事件桌面演练和开源依赖升级评审。

## 9. 值班交接信息

每次事件/交接必须包含：

- 影响的环境、租户/工厂、Capability 和 Provider；
- 开始时间、当前状态、用户影响；
- 最近应用版本、Binding revision、Snapshot version；
- 代表性 `trace_id/resource_plan_id`；
- 已执行动作及结果；
- 下一步、Owner 和预计更新时间。

不得在工单中复制 Token、Prompt、工具参数或模型响应。

