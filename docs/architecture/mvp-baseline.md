---
title: SAOAF Phase 0 最小 MVP 基线
version: 1.0.0
status: approved-for-mock
owner: David
created: 2026-09-22
updated: 2026-09-22
classification: public
---

# SAOAF Phase 0 最小 MVP 基线

## 1. 目标和部署边界

MVP 验证一条完整、可审计、可替换的模型能力路径：Capability 注册 → ARR Resolve → MMR Mock 调用 → Evidence 关联 → Provider 退出演练。只部署一个主权地域，跨三个独立故障域；不做跨地域双活，不承载 Prompt、模型响应、工具参数或业务正文。

## 2. 多租户与容量

| 项目 | MVP 基线 | 升级触发器 |
|---|---|---|
| 租户 | 单实例逻辑多租户，最多 10 个租户 | 强监管租户要求独立密钥、独立备份或物理隔离 |
| 隔离键 | `tenant_ref` 只能来自已验证 Token/受信网关 | 任何自由文本覆盖均拒绝 |
| Registry 规模 | 100 Capability、50 Provider、500 Binding | 达 70% 后做容量复测 |
| Resolve 峰值 | 100 QPS，持续 15 分钟 | 连续 7 天超过 70 QPS |
| Resolve 延迟 | P50 ≤ 25 ms，P95 ≤ 60 ms，P99 ≤ 100 ms | P99 连续两个窗口超标 |
| 可用性 | 每月 99.9%，计划维护另行公告 | 关键生产 Agent 纳管后评审 99.95% |
| 部署 | 1 个地域、3 个故障域、2 个以上应用副本 | 第二主权地域或区域级 RTO 要求出现 |
| RTO/RPO | RTO ≤ 60 分钟；数据库 RPO ≤ 5 分钟 | 合规要求零数据丢失时引入同步复制/第二地域 |

租户隔离必须在数据访问层集中注入，数据库唯一键和索引包含 `tenant_ref` 或其不可逆内部引用。缓存 key、幂等 key、NATS subject、指标访问和审计导出均带租户边界；指标标签不得直接包含敏感租户名称。

## 3. MMR Mock

`mocks/mmr` 模拟 vLLM Semantic Router 的稳定 Entrypoint/Recipe 边界：

- OpenAI-compatible `POST /v1/chat/completions` 和 `GET /v1/models`；
- 仅接受逻辑 model/profile `reasoning-high-v1`；
- 透传 `resource_plan_id`、`resource_plan_item_id`、`traceparent`；
- 返回唯一 `model_route_decision_id`、`usage_ref` 和受控 usage；
- 提供成功、profile 不存在、配额不足和服务不可用示例；
- 不声称模拟真实 signal、decision、algorithm、级联、质量、成本或模型健康。

真实 MMR 接入时只替换 base URL 和认证配置，契约测试保持不变。

## 4. 身份、MFA、Workload Identity、PDP 和审批

| 项目 | Mock 固定值/行为 |
|---|---|
| OIDC issuer | `http://127.0.0.1:8080/realms/saoaf-dev` |
| API audience | `saoaf-control-plane` |
| 人员 client | `saoaf-local-pkce`，Authorization Code + PKCE `S256` |
| scopes | `resource.read/write/publish/suspend`、`sovereignty.read`、`exitpack.write`、`drill.approve` |
| MFA | Keycloak TOTP；新用户首次登录必须配置；生产由企业 MFA/条件访问替换 |
| Workload Identity | 运行时生成测试 CA 和短期 X.509 证书，URI SAN 为 `spiffe://saoaf.test/ns/default/sa/resource-resolver`；生产切换 SPIFFE/SPIRE 或企业 mTLS |
| PDP | AuthZEN `POST /access/v1/evaluation`；失败、超时、未知响应全部 fail closed |
| 审批 | `POST /approvals/v1/requests`、`GET /approvals/v1/requests/{id}`；申请人不能批准自己的高风险请求 |

Mock 不预置密码、私钥、client secret 或真实人员。浏览器生产形态仍采用 BFF 和安全 Cookie；本地 public client 只用于协议调试。

## 5. PostgreSQL、HA 与备份

主路径选择 PostgreSQL 18.6 和 CloudNativePG 1.30.0：

- 三实例跨三个故障域，一主两副本，自动 failover；MVP 使用异步流复制；
- 应用只访问 operator 管理的读写 Service，连接池总量先限制为 100；
- 开启 checksums、TLS、SCRAM-SHA-256、最小权限数据库角色和迁移专用角色；
- WAL 连续归档到独立 S3-compatible bucket，目标 RPO ≤ 5 分钟；
- 每日一次 base backup，保留 35 天；月度恢复演练连续三次通过后改为季度；
- 恢复验收包含 schema version、active revision、outbox 水位、Evidence digest 和 API smoke test；
- 跨地域灾备延后；MVP 只保证单地域内跨故障域高可用。

本地开发可使用单容器 PostgreSQL，但它不用于证明 HA、PITR 或 RTO。

## 6. 企业消息平台

选择 NATS JetStream 2.14.7 作为 MVP 事件平台：

- 相比 Kafka，当前事件量和团队规模不需要分区级大吞吐及更重运维；
- 生产为三节点 JetStream，stream replicas=3；本地 Mock 为单节点、只绑定 loopback；
- 交付语义为 at-least-once，CloudEvent `id` 去重，aggregate revision 拒绝乱序回退；
- stream `SAOAF_EVENTS`，subjects `saoaf.>`，默认 7 天/10 GiB 上限，DLQ 14 天；
- Outbox 是数据库事务事实；NATS 不成为权威状态，也不保存敏感业务载荷；
- 若持续吞吐超过 5,000 events/s、需要长期 replay 或企业已有 Kafka 托管标准，再做替换 ADR。

选择 2.14.7 而非刚发布的 2.15.0，以减少 MVP 首轮升级风险；补丁升级需通过重复、乱序、leader 切换和恢复测试。

## 7. Evidence、WORM 和数据驻留

| 数据 | 在线保留 | WORM/归档 |
|---|---:|---:|
| Resource Plan/Item | 90 天 | 每日摘要 Evidence Pack 1 年 |
| Decision Record | 1 年 | 每日 Evidence Pack 1 年 |
| Change Record/高风险审批 | 2 年 | COMPLIANCE Object Lock 2 年 |
| Provider Snapshot 正文 | 90 天 | digest 和引用 1 年 |
| Outbox/NATS event | DB 14 天 / NATS 7 天 | 只归档审计需要的摘要 |
| Exit Pack/Drill 报告 | 在用期 + 2 年 | COMPLIANCE Object Lock 2 年 |

生产对象存储必须支持 S3 API、versioning、Object Lock COMPLIANCE、legal hold、SSE-KMS、独立写入/读取/保留管理权限、审计日志和同一主权地域内的跨故障域冗余。原始业务载荷禁止进入 Evidence Pack；每个 Pack 含 manifest、SHA-256 digest、签名引用、生成时间、租户别名、策略版本和对象 retention 信息。

候选存储必须通过以下准入测试：锁定版本在到期前任何管理员都无法删除或缩短期限；delete marker 不影响锁定版本；legal hold 生效；KMS 密钥撤销和恢复流程可审计；跨故障域恢复后 digest 一致。Mock S3 API 或普通 versioning 不能作为 WORM 证据。

开源评估结论：MinIO 社区仓库已归档且为 AGPLv3，不进入主路径；SeaweedFS 虽为 Apache-2.0 并声明 Object Lock，但公开问题显示 COMPLIANCE 删除语义存在争议，只有在指定版本通过上述准入测试后才可试验。MVP 生产默认对接企业已有、经过 WORM 认证的 S3-compatible 存储。

## 8. 验收门槛

1. 100 QPS、15 分钟压测下 P99 ≤ 100 ms，错误率 < 0.1%。
2. 任一应用副本或 PostgreSQL 主实例故障不破坏已提交 revision；恢复达到 RTO/RPO。
3. NATS 重复、乱序、断连和 leader 切换不产生重复状态迁移。
4. Token 过期、错误 audience、缺 scope、PDP 超时、审批过期和伪造 tenant 全部 fail closed。
5. MMR Mock 的成功与负向场景均保留 `resource_plan_id` 和 `model_route_decision_id`。
6. WORM 删除绕过测试失败即阻止 Evidence 生产准入。

## 9. 开源选择与退出条件

| 组件 | 选择 | 许可证 | 退出条件 |
|---|---|---|---|
| MMR 核心 | vLLM Semantic Router v0.3.x 契约 | Apache-2.0 | 真实 MMR 保持相同 profile/decision 契约 |
| OIDC Mock | Keycloak 26.7.4 | Apache-2.0 | 生产直接接企业 IdP |
| API Mock | Prism 5.15.10 | Apache-2.0 | 契约治理扩大时迁移 Microcks |
| 数据库 | PostgreSQL 18.6 | PostgreSQL License | 企业不支持 18 时退到仍受支持的相邻主版本 |
| PostgreSQL Operator | CloudNativePG 1.30.0 | Apache-2.0 | 非 Kubernetes 环境切企业托管 PostgreSQL |
| 事件平台 | NATS JetStream 2.14.7 | Apache-2.0 | 吞吐、长期 replay 或企业标准触发 Kafka ADR |
| WORM 存储 | 企业 S3-compatible Object Lock | 产品依赖 | 未通过不可删除和恢复测试即拒绝采用 |

## 10. 官方依据

- [vLLM Semantic Router system overview](https://github.com/vllm-project/semantic-router/blob/main/website/docs/overview/semantic-router-overview.md)
- [PostgreSQL 18 backup and recovery](https://www.postgresql.org/docs/18/backup.html)
- [CloudNativePG high availability](https://cloudnative-pg.io/info/high-availability/)
- [CloudNativePG backup and PITR](https://cloudnative-pg.io/docs/devel/operator_capability_levels/)
- [NATS Server releases](https://github.com/nats-io/nats-server/releases)
- [S3 Object Lock](https://docs.aws.amazon.com/AmazonS3/latest/userguide/object-lock.html)
