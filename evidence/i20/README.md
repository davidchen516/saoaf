# I20 证据：Placement Fabric 契约、Mock 和测试包冻结

- 日期：2026-09-27 | 分支：`i20-placement-contract` | 范围：contract-only（03.7，无运行时）

## 交付物

| 产物 | 位置 | 内容 |
|---|---|---|
| JSON Schema ×2 | `contracts/schemas/v1/placement/` | **portable-profile**（profile_id/OCI digest 架寻址/architecture/accelerator_class 枚举/runtime+configuration 引用式/**secret 仅 name+ref（additionalProperties:false 结构性拒绝明文 value）**/data_dependencies digest/health_slo/export_procedure+license_sbom 引用+digest/**profile_digest 双 zone 同字节锚点**）+ **placement-plan**（pp- ID/policy_decision_ref/**zones_evaluated 逐 zone eligible⇒reason_code（四类枚举：INSUFFICIENT_CAPACITY/NOT_COMPLIANT_ZONE/ARTIFACT_INCOMPATIBLE/FAILBACK_FAILED）**/状态机 {PROPOSED→APPROVED→APPLYING→{APPLIED\|FAILED\|FAILED_BACK}}/deployment_ref） |
| 不变量（if/then 冻结） | 两个 schema | portable-profile：secret items 只允许 name+ref（结构性）；placement-plan root ×3（终态⇒finished_at、FAILED/FAILED_BACK⇒error 信封、APPLIED⇒deployment_ref）+ items ×1（eligible=false⇒reason_code）——**蓝军 4/4 红**（逐条删 → 各对应负向夹具 unexpectedly PASSED） |
| OpenAPI 3.1 | `contracts/protocols/placement/openapi.yaml` | `GET /control/v1/zones`（双示例 zone）+ `POST /v1/profiles/validate?zone=`（**纯校验——同字节同 digest 同判定**）+ `POST /v1/placement-plans`（**异步**控制面计划，幂等 Idempotency-Key）+ `GET /v1/placement-plans/{id}`（断线恢复收敛）；负向响应 7 类（四类负向 + secret 引用拒绝 + 校验失败 + 计划不存在，全部冻结枚举内） |
| **sync-infra-scan 新门禁** | `contractlint sync-infra-scan`（CI contract-gate job） | **I20 边界不变量：契约体系内不存在同步建基础设施接口**——扫描全部 openapi+protocols 操作的 path/operationId/description，动词（create/provision/deploy/allocate/resize/scale-out/spin-up/launch）×名词（gpu/node/cluster/instance/machine/vm/infrastructure）命中即红（async 计划语义 allowlist）；**红运行**：注入 `POST /v1/zones/{zoneId}/gpus` createGpuNode → 红 |
| Mock | `mocks/placement/compose.yaml`（Prism :4050） | 契约语义 mock（example-selection 披露同 I18 先例）；不调度任何东西 |
| 夹具 | valid/placement ×2、negative ×5 | golden（profile/plan）+ 明文 secret value（UNKNOWN_FIELD）/终态缺 finished_at（MISSING_REQUIRED）/FAILED 缺 error（MISSING_REQUIRED）/APPLIED 缺 deployment_ref（MISSING_REQUIRED）/ineligible zone 缺 reason_code（MISSING_REQUIRED）——**每条不变量各配一夹具** |
| 消费者样例 | `compatibility/v1/consumer-fixtures/placement-*.json` ×2 | 钉版本 N/N-1 |
| 消费者测试 | `scripts/placement-contract-test.sh` | 14/14 组 24 断言（见下） |

## 消费者测试结果（Mock 实测）

**14/14 组 PASS（24 断言：14 check + 10 内容断言）**：
- **GWT#1 双 zone 同 digest 验证**：同一 profile 字节在 zone-a 与 zone-b 均验证通过且 `profile_digest` 逐字节一致（同字节证明——issue 关闭判定第 1 项）
- **GWT#2 四类负向各一**：容量不足 409 CONFLICT / 无合规 zone 403 SEMANTIC_INVALID / 制品不兼容 400 SEMANTIC_INVALID / 回切失败 503 INTERNAL + secret 引用拒绝 403（GWT#6）
- **GWT#3 幂等**：同 Idempotency-Key 重放返回同一 pp- 计划
- **GWT#5 断线恢复收敛**：status 返回权威终态 APPLIED + deployment_ref + finished_at
- **secret 引用制**：明文 value 键被契约（additionalProperties:false）拒绝 → 400（Mock 请求侧真校验）
- 缺身份头拒绝；双示例 zone 元数据可读

## 红运行与蓝军反验

| 注入/反验 | 门禁 | 结果 |
|---|---|---|
| `POST /v1/zones/{zoneId}/gpus` createGpuNode（同步建 GPU） | **sync-infra-scan** | **红**；恢复绿 |
| `internal/placementfabric/` 运行时包 | deployable-scan | **红**（marker placementfabric） |
| 明文 `prompt` 类注入 placement golden | validate 禁止扫描 | 由 forbidden 扫描族覆盖（placement 夹具纳入全仓扫描） |
| 删 portable-profile secret items 结构性拒绝 | 负向夹具 | **蓝军红**（plaintext-secret 夹具 unexpectedly PASSED） |
| 删 plan 终态⇒finished_at | 负向夹具 | **蓝军红** |
| 删 plan APPLIED⇒deployment_ref | 负向夹具 | **蓝军红** |
| 删 items eligible=false⇒reason_code | 负向夹具 | **蓝军红** |

## ARR 边界不变量（issue AC 第 3 项）

**ARR 契约中不存在同步创建基础设施的接口**：sync-infra-scan 扫描全部契约操作（openapi/v1 控制面 + protocols/ 四模块）——placement 的 plan 提交是异步控制面操作（PROPOSED → Owner apply → Factory 异步执行），validate 是纯只读校验；红运行证明门禁有牙齿。CI contract-gate job 常驻。

## 门禁基线

`CONTRACT GATE: PASS (schemas=10 examples=11 negatives=24 consumers=8 forbidden-hits=0)`；`DEPLOYABLE SCAN: PASS`；`SYNC-INFRA SCAN: PASS`；breaking 对 main 纯加法；CI module-contracts job（MCP+A2A+Placement 三 Mock 常驻）。

## 挂账

- 外部反馈（Enterprise AI Factory/制品库/基础设施平台接口）：start rule 只约束后续兼容变更/新 major
- zone 评估/Policy 检查/部署执行：03.7 运行时批次
- 信封 v2 403 码收敛：维持挂账（冻结枚举 major 批次）
