# I13 证据：主权指标、风险台账和查询 API

- 日期：2026-09-24 | 分支：`i13-metrics` | 环境：真实 PostgreSQL 18.6（goose v10）、Go 1.27.1、`-race`
- 测试输出：`test-results.txt`

## GWT 映射

| GWT | 测试 | 状态 |
|---|---|---|
| #1 固定数据集计算+查询，与上次完全一致，下钻三元组 | TestReproducibilityDiffZero（含输出确定性序）+ TestQueryDrilldownTriple | ✅ |
| #2 空分母/未知实体/失效 Binding/过期 Snapshot → 显式分类绝不折叠为 0 | TestSemanticClassification（NOT_APPLICABLE/UNKNOWN/INSUFFICIENT_DATA） | ✅ |
| #3 同一聚合任务重复触发 → 指标不变、告警不重复 | TestRecomputeIdempotency（重算×3） | ✅ |
| #4 双 Worker 并发 → 结果一致 | TestConcurrentAggregation（4 worker + 唯一约束幂等合并） | ✅ |
| #5 聚合中断 → 无半更新窗口 | PersistResults 单事务（记录+告警同 commit）；崩溃 → 全批重做经幂等吸收（同 I12 模式） | ✅（机制） |
| #6 跨租户/越权查询 403 | 查询 API 维度过滤就位；HTTP 403 门禁挂 I17（issue 自标联动 I05/I17） | ✅（范围内） |
| #7 公式 v2 缺陷切回 v1 → 新计算用 v1、历史不改写 | TestFormulaRollbackNoHistoryRewrite（v1/v2 历史并存 + Latest 按版本服务） | ✅ |

## 关键设计

1. **指标纯函数**：`ComputeV1(Dataset) → []MetricResult`——无副作用；输出按 (metric, dims) 确定性排序（map 迭代序不可依赖——审查前自测抓到并在 -race 下显形）。固定数据集重跑 diff = 0 由 mustJSON 全序比较强制。
2. **语义分类表**：Value 为 `*float64`——非 OK 状态为 nil（未知 ≠ 0 由类型与 DB 双重承载）；空分母→NOT_APPLICABLE、未知供应商→UNKNOWN（含告警）、重复 Provider 行→reason 标注、失效 Binding/过期 Snapshot→INSUFFICIENT_DATA。
3. **追溯三元组**：dataset_revision + formula_version + evidence_ref（`dataset:<rev>@<formula>`）逐结果落列，查询 API 返回。
4. **告警幂等键 (rule, entity, revision)**：UPSERT DO NOTHING——重算/Worker 重启只可能触碰 last_seen_at，永不新增重复告警（测试重算×3 验证）。
5. **公式回滚**：UNIQUE (metric, dims, revision, formula_version)——不同公式版本历史并存；Latest 按版本过滤——回滚即切查询版本，历史行零改写。
6. **风险规则 v1**：替代覆盖 < 0.5 → MEDIUM；未知供应商 → HIGH；Binding 健康不足 → LOW。规则随公式版本化。

## 已知限制（挂账）

- 证据完整率/Exit Pack 完整率的完整聚合（依赖 I14 Exit Pack Registry 数据面）→ 挂 I14 就绪后接入（本 PR 表结构与 ComputeV1 框架已就位）。
- 聚合任务调度（定期跑批）→ worker 进程接线按 I17 管理面批次决定（Store/PersistResults 幂等使任何调度器安全）。
- 监控仪表盘导出（聚合成功率/延迟/告警触发）→ I13 交付 Store 查询面，dashboard 导出挂 I13 后续/I17。
- GWT#5 kill 注入：与 I12 同模式（单事务 + 幂等吸收）——专门的 kill 矩阵在 I12 已交付同构证据；如审查要求本 Issue 独立注入矩阵可补。
- GWT#6 跨租户 403 HTTP 门禁 → I17。
