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

## 审查 R1 整改（Findings → 修复 → 回归映射）

| Finding | 修复 | 回归 |
|---|---|---|
| P0 exitpack_active 非 UNIQUE（pg_indexes 实锤；barrier 三方并发 15/15 双活，最多 3 行 ACTIVE 零报错） | 改 **CREATE UNIQUE INDEX**（照 binding_single_active 范式）+ Activate 在 UPDATE 与 COMMIT 两处把 23505 映射为 EXITPACK_ALREADY_ACTIVE | `TestConcurrentThreeWayActivateSingleActive`（barrier 三方：恰一 ACTIVE + 败者全部分类化零裸错——last-writer-wins 交替合法，不变量是唯一 ACTIVE） |
| P1-1 scanPack 丢 checklist（导出 null + 红线扫描被架空 + digest 不覆盖） | 全部 scanPack/SELECT 补 checklist 列 | `TestChecklistForbiddenBlockedInRealValidate`（真实 Validate 路径 api_key 被拒）+ `TestChecklistIncludedInExport`（manifest 含 checklist 且 digest 覆盖） |
| P1-2 历史不可变可绕过（vendor/pack_key/revision/valid_until 不冻结；VALIDATED→DRAFT 降级绕过；DELETE 静默 no-op） | trigger 冻结集补全（vendor/pack_key/revision/valid_until/created_by）+ 禁止已验证态回退 DRAFT + DELETE 对已验证行显式 RAISE（DRAFT 行可删） | `TestImmutabilityBypassPathsClosed`（四条绕过路径全拒 + DRAFT 删除合法） |
| P1-3 证据断链无实现且注释虚假 | RiskView 联 `saoaf.evidence_record`：ACTIVE pack 引用不存在/LINKED 之外的证据 → 风险视图 | `TestBrokenEvidenceRiskView` |
| P2-1 SweepExpired 两段式窗口脆弱 | 单语句 `UPDATE ... RETURNING` | 既有 TestExpirySweepAndRiskView |
| P2-2 supersede 维度 | 按 issue 状态机原文「ACTIVE 每 vendor 唯一」收敛（注释明确）+ UNIQUE 索引按 vendor | — |
| P3-1 未用 sentinel + CreateDraft 裸 23505 | 清理未用；CreateDraft 冲突映射 EXITPACK_REVISION_CONFLICT | 探针路径 |
| P3-3 README 与实现不符 | 修正（本表即修正记录） | — |

## 已知限制（挂账）

- HTTP API（查询/写入端点）与 exit-admin 403 门禁 → I17 管理面批次。
- I13 exit_pack_completeness 指标的数据面接入（metrics 框架已就位）→ I13 ledger 项。
- 到期告警的事件化（outbox）与监控导出 → I17。
- WORM 归档链路（Object Lock）→ I23。
