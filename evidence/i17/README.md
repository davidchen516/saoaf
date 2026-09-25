# I17 证据：Sovereignty Operations 管理界面

- 日期：2026-09-25 | 分支：`i17-ops` | 环境：Go 1.27.1 + React 19 + TS 5.9 + Playwright 1.63（系统 Chrome）

## 交付架构（I16 模式复用 + 授权层）

- **`internal/ops`（module 03.8 读投影）**：10 个 GET 路由挂载在 `MountAdmin` 身份门控子树内（相对路径，经 R3 验证的 extend 机制）：
  - `GET /admin/v1/ops/overview` — 总览聚合（未决告警/过期 Pack/进行中 Drill/未闭环整改/隔离深度/**显式未知指标数**）
  - `GET /admin/v1/ops/metrics` — 最新指标（DISTINCT ON per metric×dimensions）+ **下钻五轴过滤**（capability/provider/vendor/environment/tenant）+ 时间窗
  - `GET /admin/v1/ops/metrics/history` — 单指标全历史（公式版本并存，回滚不改写历史）
  - `GET /admin/v1/ops/metrics/broken` — **证据断链视图**（evidence_ref 空/不可解析 → broken_reason）
  - `GET /admin/v1/ops/alerts` / `/ops/evidence` / `/ops/exit-packs` / `/ops/drills` / `/ops/drills/{id}` / `/ops/drills/{id}/log`
- **授权**：`ops.read` scope（`RequireScope`）；`ops.all-tenants` 决定租户可见域——**无此 scope 强制 tenant_ref 过滤，显式跨租户请求 403**（绝不静默收窄）；token 受信 `environments` claim（空=不限）收窄环境轴，越轴显式请求 403。`authn.Identity` 增加 `Environments` 字段（服务端声明权威不变）。
- **显式状态**：指标 `value: null` + `status/status_reason` 原样透传（未知 ≠ 0）；Exit Pack 过期**读取时判定**（`state: EXPIRED` + `declared_state` 保留）；`digest: null` 未知不折算为空串。
- **服务端分页**：`limit` 默认 50、**硬上限 200**（超限钳制）；响应携带 `limit/offset/count`；浏览器从不加载全量。
- **Drill 非法操作**：ops 面是**纯读控制台**（无任何写方法挂载）；状态机裁决在 I15 API（负向测试见下）。
- **ADR-0006**：ops 包只 SQL 共享 schema + 导入 platform；boundarycheck 27 包 PASS；接线在 composition root（`cmd/control-plane-api`：hub + ops 双 extend 挂载）。

## 测试证据

| 套件 | 结果 | 覆盖 |
|---|---|---|
| `internal/ops/ops_test.go`（真 PG 18.6 + `-race`） | **8/8 PASS** | 三元组 100%（OK 行必带 evidence_ref）/ 未知≠0（null+reason 断言）/ 租户隔离（隐藏 + 显式 403 + all-tenants 对照）/ 环境隔离（403 + 强制过滤 + 无 claim 对照）/ 分页钳制 + 无 scope 403 + 匿名 401 + 404 信封 / alerts-packs-drills 视图（EXPIRED 读取时判定）/ 断链视图（2 行 + broken_reason）/ environments claim 解析 |
| `web/test/ops.test.mjs`（node:test） | **13/13 PASS** | 敏感零落地 / 唯一基址 / 服务端分页参数 / 未知徽标 / 状态着色 / 三元组渲染 / 断链视图 / EXPIRED 显式 / 只读控制台（无 POST/PUT/DELETE）/ findings+审计轨迹 / 无本地缓存伪造 / 导航 / flat 信封 |
| `web/test/ops.e2e.mjs`（**Playwright + 系统 Chrome**，真实二进制 + 真 PG + stub OIDC/PDP/审批） | **3/3 PASS** | 全视图权威数据渲染（overview 计数 + 指标行 + 三元组逐行断言 + 未知徽标 + 断链行 + 风险行 + EXPIRED 读取时判定 + Drill 详情/findings/审计轨迹）/ **无 scope 操作者显式 403 错误态**（非空成功）/ 分页服务端参数 + **载荷 1243 字节**（上限 256KiB） |
| `scripts/e2e-stack.sh`（栈编排，CI 同款） | sanity gates + 3/3 | 建库→迁移→种子→stub→API（预编译）→vite→readiness 硬失败→sanity（200/403/401）→浏览器套件→trap 清理 |

## 关闭判定对照（issue 核心验收逻辑）

- [x] **下钻抽样 100%**：TestOpsDrillDownTripleCompleteness（每行 formula_version + dataset_revision；OK 行 evidence_ref 非空）+ E2E 逐行断言
- [x] **越权负向（绕过 UI 直调 API）**：ops_test（403 + 租户隐藏 + 对照组）+ 栈 sanity gates（scope-less 403 / 匿名 401）+ E2E（浏览器内无 scope 显式 403 错误态）
- [x] **分页载荷上限量化**：E2E 实测 **1243 字节/页**（limit=50 种子集）；上限断言 256KiB；服务端 limit 硬钳 200
- [x] **显式状态（未知 ≠ 零风险）**：null value + status_reason 全链路（API 原样透传 → UI 徽标）断言；overview unknown_metrics 计数
- [x] **Drill 非法操作由 API 拒绝**：ops 面零写路由（web 测试断言无 POST/PUT/DELETE）；状态机负向已在 I15 交付（`TestAdminRouteShadowingPanics` 保证扩展不能再注册写路由）

### GWT 对照

| # | 证据 |
|---|---|
| 1 happy path | E2E 测试 1（六视图 + 三元组 + Evidence 引用渲染） |
| 2 错误/缺失 | 未知徽标 + 断链视图 + 后端 5xx 时 ops-error 显式（scope-less E2E 同构） |
| 3 重复提交 | 只读面无写操作；写幂等在 I16 发布链路已证（Idempotency-Key） |
| 4 并发 Drill | I15 状态机 CAS（TestDBRepublishConcurrentSingleWinner 同构证据；ops 面无写） |
| 5 崩溃恢复 | 视图切换重置 + API 重拉（web 测试「无本地缓存伪造」）；E2E 全程 API 权威 |
| 6 权限拒绝 | 三层：401 匿名 / 403 无 scope / 403 跨租户·跨环境（显式） |
| 7 回滚 | 静态制品替换；ops 面零写 → 后台状态零变更（结构性） |
| 8 分页性能 | 服务端分页 + 1243B 实测 + 200 硬上限 + 下一页禁用态由服务端 count 驱动 |

## 监控挂账

前端错误率/核心视图加载 SLO/下钻成功率仪表盘 → I13 主权指标生产接线批次（与 #13 ledger 第 1 项「生产数据集完整聚合运行记录 + 监控曲线导出」同批——ops 面已消费 metric_result/risk_alert 表，生产调度器接通后仪表盘即有数据源）。

## I16 台账清偿

- **真实浏览器 E2E**：✅ 本 PR 交付（Playwright + 系统 Chrome 3/3；I16 hub 视图在同一 App 内，导航与 api() 基座不变）
- routeRecorder `Group()` 覆盖：非阻塞观察（当前无使用方），随下次 admin 面变更处理

## 审查 R1 整改（CHANGES REQUESTED → 全项修复）

| Finding | 修复 | 回归 |
|---|---|---|
| P1-1 history/broken 未应用 environments 收窄（production-scoped token 可读 staging 行；显式 environment 过滤被静默忽略） | 两 handler 接住 `envs` 追加 `dimensions->>'environment' IN (...)`（与 listMetrics 同构）；history 同时应用显式 environment 过滤 | `TestOpsEnvironmentConfinementHistoryAndBroken`（staging 不可见 + in-scope 可见 + off-scope 403 + 无 claim 对照组） |
| P2-1 overview unknown_metrics 查询失败静默折 0 | 失败进 `unknown_fields` + 输出 null（与 quarantine_depth 语义对齐：显式未知，绝不当 0） | `TestOpsOverviewUnknownSemantics`（wire shape：number/list 断言）+ UI `UnknownBadge` |
| P3-1 drills/{missing}/log 200 空列表 | 先查存在性 → 404 NOT_FOUND（与 getDrill 一致） | `TestOpsDrillLogNotFound` |
| P3-2 getDrill findings 查询失败静默空成功 | 失败 → 500 INTERNAL（部分失败不呈现为成功空态） | 代码路径 |
| P3-3 e2e-stack.sh 卫生成组 | trap 覆盖 EXIT/INT/TERM；删除死代码 stub 启动；cleanup 覆盖全部临时文件 + vite 进程树（setsid + pkill）；种子 SQL `-v ON_ERROR_STOP=1`；E2E_HOLD 调试模式 | 运行后孤儿/残留检查通过 |
| P3-4 /ops/evidence?since= 非法值静默忽略 | 与 metrics 一致 → 400 VALIDATION_INVALID_ENUM | 代码路径 |
| P3-5 跨租户/环境 403 错误码 FORBIDDEN_FIELD | 统一为 `FORBIDDEN`（与 RequireScope 同码；FORBIDDEN_FIELD 保留原写面语义） | 4 处替换 |
| P3-6 UI overview unknown_fields 命中字段渲染 0 | `UnknownBadge` 显式「未知」徽标 | UI 代码 |
| P3-7 risk_alert 无租户维度 | 挂观察（当前全平台级规则；租户实体告警出现时需补收窄）——审查员同判 |
| P3-8 ValueCell 将 NOT_APPLICABLE 误标「数据不足」 | 三态：未知/不适用/数据不足 | UI 代码 |

**E2E 断言竞态修复**（整改过程中暴露）：drill-detail 容器渲染先于 log 响应——E2E 改为等待审计轨迹内容（`ol li`）而非容器。drillLog 的存在性检查使该竞态必现，属测试缺陷非产品缺陷。

整改后：ops 11/11（真 PG -race，含 3 个新回归）、web 24/24、E2E 3/3、payload 1243B。
