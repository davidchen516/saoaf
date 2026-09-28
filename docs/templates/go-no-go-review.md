# Go/No-Go Review — MMR Provider Exit Drill (I21)

- Drill key: `<drill-key>`　|　Date: `<date>`　|　Reviewer: `<name/role>`
- Chain export: `exit-chain-verify -dsn <prod-dsn> -drill <drill-key>` 输出（附 JSON）

## 证据链核对（逐链接，工具判定 + 人工签字）

| 链接 | 工具判定 | 人工核对 |
|---|---|---|
| drill 存在且为完成态（SUCCEEDED/REMEDIATION_OPEN/CLOSED） | ☐ | ☐ |
| exit pack（ACTIVE/VALIDATED，含替代 provider） | ☐ | ☐ |
| plan → decision（correlation 行：pre/post outcome + usage + trace） | ☐ | ☐ |
| plan 有 items（ARR 选择记录） | ☐ | ☐ |
| evidence 索引覆盖（plan_id 关联记录存在） | ☐ | ☐ |
| findings/remediation（open/resolved 台账） | ☐ | ☐ |
| 审计轨迹（transition log 非空——状态机未绕过） | ☐ | ☐ |

## 零变更证明（issue: ARR/Agent 配置零变更——操作者运行 runbook 导出对比后签字）

- ARR 逻辑 profile 与 Agent 配置 diff = 0：☐（附导出对比命令输出）
- Agent 业务代码 diff = 0：☐（附 `git diff` / 制品 digest 对比输出）
- 物理模型细节仅在 MMR 证据中（ARR 状态无物理细节）：☐

## 未达标项（NO-GO 必填）

| 项 | Owner | 到期日 |
|---|---|---|
| | | |

## 判定

☐ GO　☐ NO-GO（含未解释长尾/错误、断链、零变更未证明、回滚超时、高危安全问题）

签字：＿＿＿＿＿＿
