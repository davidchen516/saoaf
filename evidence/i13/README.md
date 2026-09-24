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

## 审查 R1 整改（Findings → 修复 → 回归映射）

| Finding | 修复 | 回归 |
|---|---|---|
| P1-1 聚合输入管道缺失且未诚实挂账 | **提取器交付**：`Store.ExtractDataset` 从真实表（capability_definition ⋈ capability_binding ⋈ resource_provider）拉取 DimRow，含替代可用性 EXISTS 检测、重复 Provider 计数、未知供应商标记、租户归因；数据集 revision = 迁移版本 ×10⁹ + binding 行指纹（输入不变 → revision 不变 → 固定快照可重算） | `TestExtractDatasetFromLiveTables`（提取→计算→持久→维度过滤查询全链闭环 + 未变输入同 revision） |
| P2-1 查询 API「维度过滤就位」失实 | Latest 增加 dimsFilter（JSONB 包含过滤：capability/provider/vendor/environment/tenant）+ since/computedUntil 时间窗；死变量 byEnv 移除 | 同上 + 既有查询测试 |
| P2-2 协议兼容率二值退化 | 改为按 capability 的真实比率（healthy/live）；完全受损 → INSUFFICIENT_DATA；混合人口给比率 | `TestProtocolCompatibilityIsARatio`（0.5）+ TestSemanticClassification 扩展（比率 + 全受损） |
| P2-3 evidence_ref 自指退化 | **挂账**（如实）：evidence_ref 当前为 `dataset:<rev>@<formula>` 合成引用（不指 I12 索引真实条目）；接入 evidence_record 真实引用挂 I14/I17 批次（需证据完整率聚合同批设计） | — |
| P2-4 last_seen_at 三处宣称失实 | SQL 改为 `DO UPDATE SET last_seen_at = now()`——重算刷新但永不新增重复行 | `TestAlertLastSeenRefreshesWithoutDuplicates`（刷新断言 + 计数不变断言） |
| P3-1/4 | 并发测试错误显式传播（atomic.CAS）；死代码清除 | 既有套件 |
| P3-2/5 | Latest 语义（最近计算时间序）文档化；「当前生效公式」为调用方约定——挂 I17 管理面批次给系统状态 | — |

## 审查 R2 整改（复审新发现）

| Finding | 修复 | 回归 |
|---|---|---|
| R2-P1 指纹对原地 UPDATE 失明（生产 suspend 原句 SQL 下同 revision 静默改写历史——审查探针端到端复现） | revision 改为**内容寻址指纹**：`SUM(hashtext(id \| state \| is_active \| revision))`——任何原地变更 bump revision；未变输入同 revision | `TestRevisionBumpsOnInPlaceUpdate`（生产 suspend 原句 → revision 变化 + 双 revision 行并存历史保留） |
| R2-P2 过期 Snapshot 管道不可达（仅 fixture 语义） | 提取器 JOIN `provider_snapshot`：binding 钉住的 snapshot 过期/未发布 → 提取为 issue 行（active=0, issues=1） | `TestExpiredSnapshotReachableInPipeline` |
| R2-P3-1 跨 capability 复用 provider 误报 duplicate | duplicate 语义改为同 (capability, provider) 多行（真重复）；跨 capability 服务合法 | `TestCrossCapabilityProviderNotDuplicate`（正负双向） |
| R2-P3-2 维度过滤对 tenant/vendor 恒空 | 如实修正：公式当前只发射 {capability} 与 {} 维度——README 表述降为「公式发射的维度（capability；全局）」；tenant/vendor/provider/environment 维度发射挂证据完整率批次（I14/I17） | 文档修正 |
| R2-P3-3 README 陈旧「DO NOTHING」文案 | 更正为 DO UPDATE SET last_seen_at | 文档修正 |

## 审查 R3 整改（终轮复审新发现）

| Finding | 修复 | 回归 |
|---|---|---|
| R3-P1 指纹对 snapshot 维度失明（R2-P2 接线后暴露：valid_until 越界是零写入时钟事件——分类翻转指纹不变，同 revision DO UPDATE 改写 OK 历史；探针两条路径端到端复现） | 指纹改为**提取输出的内容寻址哈希**（Go 侧：行序列化稳定排序 + 迁移版本 → sha256 → 非负折叠）——按构造覆盖所有输入源（binding 原地变更、snapshot 过期/状态翻转、未来新增维度），杜绝第三次复发（审查员方案 b） | `TestSnapshotExpiryBumpsRevision`（零写入时钟越界 → bump + 双 revision 并存）、`TestSnapshotStateFlipBumpsRevision` |
| R3-P3 revision「monotonic」注释失实 + History 按哈希排序无时间语义 | 注释改「content-addressed hash — NOT monotonic」；History 改 ORDER BY computed_at | — |

## 已知限制（挂账，审查 R1/R2/R3 裁决口径）

- **真实运行记录与监控导出**（关闭判定第 3 项的运行面）：提取器已交付使管道闭环可行，但**生产数据集的一次完整聚合运行 + 监控曲线导出**需调度器接线与生产数据——挂 I17 批次；按审查口径，Issue 关闭在补齐前为 **NOT PROVEN 保留 OPEN**（与 #5/#11 同先例）。
- 证据完整率/Exit Pack 完整率的完整聚合（依赖 I14 Exit Pack Registry 数据面）→ 挂 I14 就绪后接入（表结构与 ComputeV1 框架已就位）。
- evidence_ref 接入 I12 真实索引条目 → 与证据完整率同批（I14/I17）。
- 聚合任务调度（定期跑批）+ 「当前生效公式」系统状态 → I17 管理面批次。
- GWT#5 kill 注入：与 I12 同模式（单事务 + 幂等吸收）——审查接受引 I12 同构证据。
- GWT#6 跨租户 403 HTTP 门禁 → I17。
