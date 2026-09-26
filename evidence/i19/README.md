# I19 证据：A2A Fabric 契约、Mock 和测试包冻结

- 日期：2026-09-27 | 分支：`i19-a2a-contract` | 范围：contract-only（03.6，无运行时）

## 交付物

| 产物 | 位置 | 内容 |
|---|---|---|
| JSON Schema ×2 | `contracts/schemas/v1/a2a/` | **agent-snapshot**（Agent Card 逻辑投影：agent_id/capability_key/skills/interface_version/endpoint_ref/trust_domain 枚举/max_delegation_scope/status + **signature 必填**——恶意/未签名 Card 被契约拒绝）+ **agent-task**（任务对象与最小证据：delegation_ref/delegation_scope/requested_scopes/granted_scopes/状态机 {SUBMITTED→RUNNING→{COMPLETED\|FAILED\|CANCELED}}/artifact 引用制） |
| OpenAPI 3.1 | `contracts/protocols/a2a-agent/openapi.yaml` | Agent Snapshot 端点 + submit/status/cancel 全生命周期；幂等 submit（同 key → 同 at- 任务）；委派引用必填；负向响应 7 类（冻结信封枚举内） |
| 不变量（if/then 冻结） | agent-task schema allOf ×4 | 终态⇒finished_at；FAILED/CANCELED⇒error 信封；RUNNING 起必带 granted_scopes；COMPLETED 必带 artifact_ref（引用+digest）——**蓝军反验**：删除不变量 → 负向夹具变「unexpectedly PASSED」→ 红 |
| Mock | `mocks/a2a/compose.yaml`（Prism :4040） | 契约语义 mock；**不实现** A2A Gateway/远程 Agent/编排/委派凭证签发（example-selection 披露同 I18 先例） |
| 夹具 | valid/a2a ×2、negative ×6 | golden + 恶意未签名 Card（MISSING_REQUIRED）/未知信任域（INVALID_ENUM）/缺委派引用（MISSING_REQUIRED）/artifact 内联正文（UNKNOWN_FIELD——引用制结构性拒绝）/终态缺 finished_at（MISSING_REQUIRED） |
| 消费者样例 | `compatibility/v1/consumer-fixtures/a2a-*.json` ×2 | 钉版本 N/N-1 |
| 消费者测试 | `scripts/a2a-contract-test.sh` | 11/11 组 22 断言（见下） |

## 契约语义状态机（issue 核心验收逻辑）

`SUBMITTED → RUNNING → {COMPLETED｜FAILED｜CANCELED}`；重复 submit 以 Idempotency-Key 幂等（同 key → 同 at- 任务）；cancel/status 竞争的终态由 Gateway 仲裁；**断线恢复经 status 查询语义收敛**（消费者测试 GWT#5：status 返回权威终态 + finished_at + artifact 引用）。

**委派边界**（module-contracts §8.2）：delegation_ref 由 04 工程签发、Gateway 不自造身份（契约必填性冻结）；子任务权限 ⊆ 委派范围——requested_scopes 越界 → 403 SEMANTIC_INVALID 信封（契约级负向例 + 消费者测试断言；**有状态子集仲裁属 03.6 运行时**，mock 披露同 I18 P2-3 口径）。**artifact 引用制**：仅 uri+digest，additionalProperties:false 使内联正文结构性被拒（负向夹具 + 消费者测试双证）。

## 消费者测试结果（Mock 实测）

**11/11 组 PASS**：GWT#1 submit→status→artifact（delegation/granted_scopes/artifact_ref 语义断言）/ GWT#3 幂等重放（同 key → 同任务）/ GWT#5 断线恢复收敛（权威终态 + artifact 引用制）/ GWT#4 cancel 竞争 409 CONFLICT / GWT#6 委派越界 403 SEMANTIC_INVALID / GWT#2 恶意未知 Agent 404 / 缺委派字段 400 / 运行时 transport failure 超时 503 / 未知任务 404 / 缺契约身份头拒绝 / snapshot 语义（trust_domain/max_delegation_scope/signature）。

## 红运行与蓝军反验

| 注入/反验 | 门禁 | 结果 |
|---|---|---|
| `cmd/a2a-gateway/` 二进制（03.6 运行时产物） | deployable-scan | **红**（marker a2a-gateway） |
| `prompt` 字段写入 a2a golden 夹具 | validate 禁止扫描 | **红** |
| 删除 agent-task 终态⇒finished_at 不变量 | 负向夹具 | **蓝军红**（unexpectedly PASSED）；恢复绿 |
| 删除 agent-snapshot signature 必填 | 负向夹具 | **蓝军红**（unexpectedly PASSED）；恢复绿 |

## I18 挂账清偿（本 PR 内完成）

- **OBS-1**：HIGH⇒approval 不变量镜像进 mcp-tool OpenAPI ToolSnapshot actions items（if/then allOf）——避免 canonical JSON Schema 与 OpenAPI 双源漂移
- **OBS-2**：validate 增加**覆盖断言**——examples/valid 下任意深度每个 .json 必须被发现 glob 命中（P1-1 同类静默跳过被结构性关闭）
- **OBS-3**：valid/mcp/tool-snapshot.json 末尾换行
- **R1-P3-1**：**OpenAPI 结构 lint 扩展至 contracts/protocols/**（mcp-tool/model-profile/a2a-agent 三文件全部纳入：3.1 版本/paths 非空/components.responses.Error 必在/4xx-5xx 信封引用（$ref 一层解析））；model-profile 补 Error 响应
- **P3-3**（信封 v2 403 码收敛）：维持挂账（冻结枚举变更需 major，跨模块收敛随信封 v2 批次）

## 门禁基线

`CONTRACT GATE: PASS (schemas=8 examples=9 negatives=16 consumers=6 forbidden-hits=0)`；`DEPLOYABLE SCAN: PASS`；breaking 对 main 纯加法；CI module-contracts job（MCP+A2A 双 Mock 常驻）。

## 挂账

- 外部反馈（01 Agent Runtime/04 delegation 接口）：start rule 只约束后续兼容变更/新 major
- 有状态委派子集仲裁/cancel 竞争仲裁/trust-domain 验证：03.6 运行时批次

## 审查 R1 整改（CHANGES REQUESTED → 全项修复）

| Finding | 修复 | 验证 |
|---|---|---|
| P2-1 Four invariants but only one negative fixture + breaking.go tail-deletion blind spot (removing the COMPLETED⇒artifact_ref tail item, validate/breaking both green) | ① Add 3 negative fixtures (failed-no-error / running-no-granted / completed-no-artifact)——each invariant has one negative fixture; ② breaking.go object arrays `len(cur)<len(base)` means breaking (tail-deletion detection) | Blue-team replay: delete any one of the 4 invariants → validate all red (corresponding fixture unexpectedly PASSED); shrinkage detection has unit regression `TestObjectArrayShrinkageIsBreaking`; gate negatives 16→19 |
| P3-1 OBS-1 claim ahead of code (mcp-tool only added description, no if/then) | **Truly add if/then allOf** to ToolSnapshot actions items (risk HIGH ⇒ approval_required const true) | grep allOf present at file line 443; Mock 13/13 (snapshot example passes through new validation); this round's patch has assert guards——silent no longer possible |
| P3-2 Count disclosure errors (negative ×6 actually 5; 22 assertions actually 21) | README corrected (now ×8/21 assertions, consistent with the numbers after adding fixtures) | Text |
| P3-3 Invalid taskId → Prism 422 bare error | mocks/a2a/README explicitly disclosed (mock-layer limitation, contract patterns are mandatory) | Text |

After remediation: `CONTRACT GATE: PASS (schemas=8 examples=9 negatives=19 consumers=6 forbidden-hits=0)`; breaking vs main purely additive (changed=3 deleted=0); MCP 13/13 / A2A 11/11; contractlint unit tests all green (including new shrinkage regression).
