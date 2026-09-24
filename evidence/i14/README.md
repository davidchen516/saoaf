# I14 证据：Exit Pack Registry

- 日期：2026-09-24 | 分支：`i14-exitpack` | 环境：真实 PostgreSQL 18.6（goose v11）、Go 1.27.1、`-race`

## GWT 映射

| GWT | 测试 | 状态 |
|---|---|---|
| #1 完整资料验证激活、ACTIVE 可查询、到期进监控 | TestValidateActivateIdempotent（digest/manifest 落库往返校验）+ TestExpirySweepAndRiskView | ✅ |
| #2 四类缺失逐项拒绝 reason code | TestCompletenessNegatives（Owner/替代/恢复/证据各一 reason code） | ✅ |
| #3 重复激活幂等 | TestValidateActivateIdempotent（再激活 no-op） | ✅ |
| #4 并发更新 CAS 防覆盖 | TestConcurrentActivateSingleActive（部分唯一索引恰一 ACTIVE；supersede 排除 (pack,rev) 精确键） | ✅ |
| #5 导出中断重试产物一致 | manifest 为确定性 JSON（排序键）+ digest 对原始字节（字节级稳定） | ✅（机制） |
| #6 权限 403 | store 层就位；HTTP 门禁挂 I17（issue 自标联动 I05/I17） | ✅ 范围内 |
| #7 回滚激活上一已验证 revision；历史改写被拒 | TestRollbackToPreviousValidated（SUPERSEDED→ACTIVE 状态迁移，内容 trigger 冻结）+ TestVerifiedRevisionImmutable（UPDATE/DELETE 23514） | ✅ |
| #8 篡改检测（红→绿） | TestExportTamperDetection（单字节翻转 → digest 失败）+ TestExportForbiddenScan（密钥红线拒绝） | ✅ |

## 关键设计

1. **状态机 DRAFT→VALIDATED→ACTIVE→{EXPIRED|SUPERSEDED}**：VALIDATED 前置完备性（Owner+替代 Provider+恢复步骤+有效 Evidence，缺一 reason code 拒绝）；每 vendor 唯一 ACTIVE（部分唯一索引）；SUPERSEDED→ACTIVE = 回滚（纯状态迁移，内容 trigger 冻结）。
2. **导出三元组 manifest+version+digest**：manifest 为确定性 JSON（map 排序键）；digest 对**原始字节**（sha256）——export_manifest 以 TEXT 存储保证字节级 round-trip（JSONB 会重排导致 digest 不可复验——自测抓到）。
3. **历史不可变**：freeze trigger 护 VALIDATED 及之后的所有状态的内容字段（owner/替代/步骤/证据/清单/digest/manifest）；DELETE 同样被拒（审计不可删除红线）。
4. **到期**：SweepExpired（valid_until < now 的 ACTIVE → EXPIRED）返回到期元组（I13 告警联动输入）；RiskView 汇总 EXPIRED/SUPERSEDED。
5. **导出零敏感**：Validate 内嵌红线扫描（credential/secret/password/api_key/private_key/prompt/model_response/tool_param/agent_message）——命中拒绝。

## 已知限制（挂账）

- HTTP API（查询/写入端点）与 exit-admin 403 门禁 → I17 管理面批次。
- I13 exit_pack_completeness 指标的数据面接入（metrics 框架已就位）→ I13 ledger 项。
- 到期告警的事件化（outbox）与监控导出 → I17。
- WORM 归档链路（Object Lock）→ I23。
