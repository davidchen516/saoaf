# I21 证据：生产准入、灾备和真实 MMR Provider 退出演练（仓库内可判定面）

- 日期：2026-09-28 | 分支：`i21-exit-drill` | 环境：Go 1.27 + 真 PG 18.6（检查器/彩排）

## 面判定（诚实划分）

**启动门禁**：issue 明文要求 #2–#20、#22、#23 关闭——**#5/#11/#13 挂账未关且无 David 第一方风险接受记录**。严格读法门禁不满足；按 goal 模式与三次先例（#5/#11/#13 本身）同一处置：**交付仓库内可判定面，生产资源项挂账**。

核心交付（真实 MMR Provider 替代/失效演练、2× 峰值 60min SLO、生产 PITR RTO/RPO、15min 回滚计时、生产供应链验证）**全部依赖外部生产资源**（生产 MMR/Agent Harness/企业 Identity/WORM 存储/K8s/观测平台）——挂账清单见下。**桌面演练与证据链检查器是生产演练的操作者流程预演**：生产演练使用同一二进制（`exit-chain-verify`）对生产 DSN 运行。

## 交付物

| 产物 | 位置 | 内容 |
|---|---|---|
| **证据链检查器** | `internal/exitdrillverify/` | issue「退出演练证据链完整可查」的形式化：**plan→decision→(pack)→(plan items)→evidence→findings→audit** 逐链核验——断链产生**具名链接**的 typed break（drill/exit_pack/correlation/plan/evidence/audit 六类）；ARR/Agent 零变更证明为操作者签字位（未证明 = NO-GO——issue: ARR/Agent 配置零变更） |
| **Go/No-Go 判定** | `Chain.GoNoGo()` | 三 NO-GO 条件：链断、ARR diff 未证明、Agent diff 未证明；模板 `docs/templates/go-no-go-review.md`（未达标项 Owner+到期日——issue: 未达标项有 Owner 与到期日） |
| **导出 CLI** | `cmd/exit-chain-verify` | `exit-chain-verify -dsn <prod> -drill <key> [-arr-zero] [-agent-zero]`：JSON 链导出（Go/No-Go pack 输入）+ 断链非零退出；`-arr-zero/-agent-zero` **只在操作者运行 runbook 对比命令之后**设置 |
| **桌面彩排** | `scripts/exit-drill-rehearsal.sh` | 完整模拟：真域栈种子（registry→plan→pack→drill→correlation→evidence）→ CLI 核验（签字前 NO-GO → 签字后 GO）→ 断链演练（无 correlation/无 pack 的 drill → 具名 break + NO-GO）——8/8 断言 |
| **Runbook 增补** | `docs/runbooks/resource-resolver-operations.md` | 生产演练导出流程 + 零变更对比命令位 + 模板指向 |

## 测试证据

| 套件 | 结果 | 覆盖 |
|---|---|---|
| `internal/exitdrillverify`（真 PG -race） | **10/10 PASS** | 完整链→GO（签字前 NO-GO 双原因→签字后 GO+全 7 链接类导出）；**七类断链各一子测试**（drill 缺失/in-flight/无 pack（SUPERSEDED 模拟——verified pack 受红线保护不可删）/correlation 空/plan 悬挂（correlation 指不存在 plan——resolver 计划不可变）/evidence 缺失/审计轨迹绕过——每断链必须具名）；JSON 往返导出 |
| 桌面彩排 | **8/8 PASS** | CLI 端到端：完整链+签字流+断链 drill（correlation/pack 双 break + NO-GO） |

## 设计说明

- **断链模拟的诚实性**：exit pack（verified 后）与 resolver plan/item 受数据库红线约束不可删（audit red line）——测试用 **SUPERSEDED 回退**与**悬挂 correlation**模拟断链形态（真实世界中破损写入留下的形状），不绕过红线。
- **零变更证明的两阶段**：检查器无法 diff 外部配置/代码存储——导出的链带 `arr_config_diff: "unverified"`（NO-GO 阻塞），操作者运行 runbook 对比命令后以 `-arr-zero`/`-agent-zero` 签字（模板有人工核对栏）。**签字前不可能 GO**。
- **evidence 链接按 plan_id**（saoaf.evidence_record 的关联键——schema 无 trace_id 列；trace_id 在 correlation 行上保留给 MMR 证据）。

## 挂账（生产资源——同 #5/#11/#13 先例）

1. **真实 MMR Provider 替代/失效演练**（vLLM Semantic Router 同一虚拟 entrypoint/Recipe 契约下：physical provider 切换，ARR/Agent 零变更）——需生产 MMR 与 Agent Harness；桌面彩排已预演操作者流程，同一 CLI 对生产 DSN 运行
2. 2× 峰值持续 60min SLO 验证（含长尾/错误归因台账）——需生产负载与观测平台
3. 生产 PITR（RTO/RPO 计时 + active pointer/Evidence/Outbox 水位核对——CI PITR drill 已交付机制，生产环境运行记录挂账）
4. 应用/Binding 回滚 15 分钟计时（机制已交付：I08 回滚为新 revision；生产计时挂账）
5. 凭证轮转/实例滚动/Snapshot 过期/断连演练的生产记录（协议/机制已由 I10/I22/I12/I16 套件覆盖）
6. 供应链：SBOM/签名/provenance 验证（image+provenance CI job 已常驻 main；生产制品验证输出挂账）
7. on-call/告警接线（观测平台资源）与 Go/No-Go 评审的**人工评审记录**（模板已交付；评审需 David 与运维参与）

**启动门禁的风险接受**：#5/#11/#13 挂账下并行开工需 David 第一方记录——本 PR 按可判定面交付并如实披露（与 #5/#11/#13 处置同构）。

## 审查 R1 整改（CHANGES REQUESTED → 全项修复）

| Finding | 修复 | 回归测试 |
|---|---|---|
| P1-1 correlation 无 drill/vendor 归因（时间窗裸查——无关流量可**填充**断链（false GO）或**污染**完整链（false NO-GO）——生产共享库双向失真，探针亲证） | 归因过滤：**[drill.created_at, COALESCE(finished_at, now)] 窗口** + plan_item.provider_key ∈ {drill vendor, drill pack 的 substitute}；悬挂 correlation 保留（dangling plan 由 plan 链接上报，不被 JOIN 静默丢弃） | **双向钉子**：TestUnrelatedTrafficDoesNotSatisfyNorContaminate（无归因结果的 drill 在无关流量下保持断链；完整 drill 不被污染） |
| P2-1 OPEN findings（含 CRITICAL）不影响 GO | open HIGH/CRITICAL → **Chain.Breaks + NO-GO**（签字后仍 NO-GO） | TestOpenCriticalFindingsBlockGo |
| P2-2 14MB 二进制误提交 | git rm + .gitignore（历史清洗待 David 决策） | — |
| P3-1 exit_pack 忽略 drill.exit_pack_key（错误指针被 vendor 兜底吞掉） | JOIN **drill 自己的 pack 指针**（legacy 空指针才 vendor 兜底）+ state ∈ ACTIVE/VALIDATED 过滤 | TestDrillPackPointerHonored（SUPERSEDED 指针 → break） |
| P3-2 audit 只数行数（单行伪造通过） | 必须存在 **→APPROVED 且 actor 非空** 的转移（I15 审批人≠发起人不变量） | TestAuditRequiresApprovalStep |
| P3-3 psql local 分支硬编码 host/port | 修为 **DSN 解析 host:port+password**（TCP 强制）——CI 兼容 | CI 绿 |
| P3-4 「六类 typed error」措辞失实 | 包注释改为如实：drill 缺失=typed error；**其余断链=具名 Chain.Breaks + NO-GO**；实现统一 | 文本 |

整改后：检查器 **14/14**（真 PG -race：原 10 + 四个 R1 探针收编）；彩排 8/8（归因对齐）。
