# I18 证据：MCP Fabric 契约、Mock 和测试包冻结

- 日期：2026-09-25 | 分支：`i18-mcp-contract` | 范围：contract-only（03.5，无运行时）

## 交付物

| 产物 | 位置 | 内容 |
|---|---|---|
| JSON Schema ×2 | `contracts/schemas/v1/mcp/` | **tool-snapshot**（ARR-facing 逻辑 action 投影：action_id/capability_key/server_ref/transport/risk_level/idempotency{supported,key_scope,ttl}/approval_required/parameter_schema_ref/status）+ **execution-evidence**（te- ID/state 机{PREVIEWED,APPROVED,RUNNING,SUCCEEDED,FAILED,CANCELLED}/policy_decision_ref/approval_ref/idempotency_key/result_ref{uri,digest}/flat error 信封） |
| OpenAPI 3.1 | `contracts/protocols/mcp-tool/openapi.yaml` | Tool Snapshot 端点 + preview/approve/execute/status/cancel 全生命周期；网关身份头（X-Resource-Plan-ID/Item-ID/X-Tenant-Ref/traceparent/X-Policy-Decision-Ref）+ Idempotency-Key；负向响应 8 类（冻结信封枚举内） |
| Mock | `mocks/mcp/compose.yaml`（Prism 5.15.10 :4030） | 契约语义 mock；**不实现** Gateway/Registry/Server/proxy/真实 Tool 调用 |
| 夹具 | `contracts/examples/valid/mcp/` ×2、`contracts/examples/negative/` ×3 | golden（tool-snapshot/execution-evidence）+ 负向（unknown-transport→INVALID_ENUM、missing-plan→MISSING_REQUIRED、bad-state→INVALID_ENUM） |
| 消费者样例 | `contracts/compatibility/v1/consumer-fixtures/mcp-*.json` ×2 | 钉版本 N/N-1 矩阵（contractlint validate 第 3 类检查） |
| 消费者测试 | `scripts/mcp-contract-test.sh` | 13 组 24 断言（见下） |
| 门禁 | `contractlint deployable-scan`（新子命令，CI contract-gate job）+ `mcp-contract` CI job | 契约-only 不变量 + Mock 生命周期/负向常驻验证 |

## 契约语义状态机（issue 核心验收逻辑）

`preview → approve（高风险必经，approval_ref 绑定 preview 参数摘要）→ execute → {succeeded｜failed}`；`execute → status（崩溃恢复收敛点）｜ cancel（竞争窗口，Gateway 仲裁终态）`。幂等：同 Idempotency-Key 重放返回原执行（te- ID 不变）；键冲突（不同参数摘要复用同键）→ 409 CONFLICT。

## 消费者测试结果（scripts/mcp-contract-test.sh，Mock 实测）

**13/13 组全 PASS**（对 Prism mock，无真实 MCP 运行时）：
- GWT#1 happy path：preview（HIGH+approval_required+next_step 语义断言）→ approve（next_step=execute）→ execute（te- ID/SUCCEEDED/result_ref 证据）→ status（权威状态）
- GWT#3 幂等：同键重放返回原执行
- GWT#4 cancel 竞争：terminal 执行上 cancel → 409 CONFLICT（execute 赢仲裁）
- GWT#5 崩溃恢复（语义）：status 查询收敛到权威终态
- GWT#6 审批拒绝：HIGH 风险 execute 无 approval_ref → **403 + SEMANTIC_INVALID 信封**（冻结枚举内语义码）
- GWT#2 错误输入：参数校验 400 VALIDATION_MISSING_REQUIRED / 未知 action 404 NOT_FOUND / 幂等键冲突 409 CONFLICT / Tool Server transport failure 503 INTERNAL / 缺契约必填身份头被拒绝
- ARR-facing snapshot：risk/idempotency/approval_required 全携带

## 红运行注入证据（issue 要求的两类）

| 注入 | 门禁 | 结果 |
|---|---|---|
| 真实 Tool 参数（`api_key: sk-live-real` 写入 valid 夹具） | contractlint validate 禁止字段扫描 | **红**（'api_key' present）；恢复后绿 |
| `internal/mcpfabric/` 运行时包 | deployable-scan | **红** |
| `cmd/mcp-gateway/` 二进制 | deployable-scan | **红** |
| `build/*mcp*.Dockerfile` 引用 fabric 二进制 | deployable-scan | **红** |
| 根目录 k8s Deployment（mcp-gateway） | deployable-scan | **红** |

恢复后全绿：`DEPLOYABLE SCAN: PASS` / `CONTRACT GATE: PASS (schemas=6 examples=4 negatives=8 consumers=4 forbidden-hits=0)`。

## N/N-1 矩阵

contractlint validate 第 3 类：`compatibility/v1/consumer-fixtures/mcp-tool-snapshot.json`、`mcp-execution-evidence.json` 对**当前** schema 验证通过（N）；CI 每次运行记录。破坏性变更走新 major（contracts README 版本规则；`contractlint breaking` 对 PR base 强制）。

## 挂账

- 外部系统反馈（领域 Tool Owner / MCP 真实接口）：issue start rule 明确只阻塞**最终 freeze**，不阻塞 Schema/Mock/负向夹具/消费者样例——本 PR 交付全部仓库内可判定内容；外部反馈到达后如有调整走兼容变更或新 major。
- Mock 与真实 MCP 协议的对齐审计：随 03.5 运行时批次。

## 审查 R1 整改（CHANGES REQUESTED → 全项修复）

| Finding | 修复 | 验证 |
|---|---|---|
| P1-1 valid/mcp golden 未被 validate 覆盖（examples=4 旁证；篡改 golden 门禁不红） | validate 递归发现 valid/**/*.json + 子目录路径映射（examples/valid/mcp/x.json → schemas/v1/mcp/x.json） | **红运行**：transport 改非法值 → 门禁红（enum 违反消息）；恢复绿。examples=6→7（含 RUNNING golden） |
| P2-1 HIGH⇒approval_required=true 未冻结进 schema（HIGH+false 快照过校验） | ToolAction item 内 allOf if/then 约束（const true）；配负向夹具 mcp-tool-snapshot-high-no-approval | 夹具红（原快照过校验的证据被门禁拒绝）；分类映射对齐（const 违反 → VALIDATION_INVALID_ENUM，classify 关键字映射，schema $comment 如实记录） |
| P2-2 execution-evidence 状态机矛盾（RUNNING 必填 finished_at 无法合规表达） | state 枚举收敛为 te- 生命周期 {RUNNING, SUCCEEDED, FAILED, CANCELLED}（PREVIEWED/APPROVED 归 tp-/ta- 对象，schema description 明示）；finished_at 降为可选 + if/then：终态必填 finished_at、FAILED/CANCELLED 必带 error 信封 | 负向夹具 ×2（terminal-no-finished→MISSING_REQUIRED / failed-no-error→MISSING_REQUIRED）+ RUNNING golden（无 finished_at 合法）；OpenAPI ExecutionResult 同步（allOf 镜像） |
| P2-3 Mock「拒绝」仅为 Prefer 选例（AC 表述与实测不符） | mocks/mcp/README **显式披露**：状态级负向为 example-selection 非有状态拒绝；请求侧校验（头/路径/体）真实强制；有状态 enforcement（risk 门禁/幂等台账/cancel 仲裁）属 03.5 运行时（issue non-goals） | 披露文本（审查员最低修复口径） |
| P3-1 protocols/ 未纳入 OpenAPI 结构 lint | 挂账（model-profile 同先例；扩 lint+响应命名约定随 I19 契约批次统一） | — |
| P3-2 cancel 例 error_code=CANCELLED 不在冻结枚举 | 执行证据 error 字段措辞修正：**flat 信封形状，error_code 为域分类**（非冻结 HTTP 枚举）——OpenAPI description 如实 | 文本 |
| P3-3 403 语义码跨模块分叉（FORBIDDEN 枚举外 vs SEMANTIC_INVALID 枚举内） | 挂账：信封 v2 收敛或运行时迁移（I18 守住冻结枚举自洽） | — |
| P3-4 杂项 | hdrs() 死函数删除；CI mcp-contract 健康等待循环替代固定 sleep；RETIRED prose 明确「新快照不再出现」 | 文本/CI |

整改后：`CONTRACT GATE: PASS (schemas=6 examples=7 negatives=11 consumers=4 forbidden-hits=0)`、`DEPLOYABLE SCAN: PASS`、消费者测试 13/13（Mock 重启实测）、breaking 对 main 纯加法。
