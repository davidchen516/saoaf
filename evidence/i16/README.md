# I16 证据：AI Resource Hub 管理界面

- 日期：2026-09-25 | 分支：`i16-hub` | 环境：React 19 + TypeScript 5.9 + Vite 7（tsc --noEmit 严格检查）

## GWT 映射

| GWT | 实现/测试 | 状态 |
|---|---|---|
| #1 有权用户搜索/过滤/发布 | Bindings 视图（搜索框 + 过滤 + canPublish 发布按钮）；whoami scope 驱动 | ✅ |
| #2 非法表单 | 前端 strict TS + API 信封（error.code/message 统一展示） | ✅ |
| #3 重复提交 | 发布走 POST + expected_revision（幂等键一致 → 服务端幂等，联动 I08） | ✅ 契约 |
| #4 并发编辑 409 | 409 → 显式冲突提示 + 「未覆盖」+ 刷新到权威状态 | ✅ |
| #5 崩溃恢复 | UI 无本地状态伪造（每次进入从 API 重载） | ✅ |
| #6 无权用户 | whoami scopes 驱动写入口隐藏 + 服务端 403（双防线） | ✅ |
| #7 回滚 | 部署上一静态制品（feature flag / dist 替换）不影响后台状态 | ✅ 机制 |

## AC 映射

| AC | 测试 | 状态 |
|---|---|---|
| 所有写操作通过 Admin API | 唯一 fetch 基址 `/api/admin/v1`（源码断言无 DB/内部服务直连） | ✅ |
| 无权用户不渲染写入口 + 服务端 403 | `who?.canPublish` 门控；服务端 scope 链（I05/I06 已交付）为最终防线 | ✅ |
| 并发编辑 revision 冲突不覆盖 | 409 → conflict 提示 + 刷新（绝不 silently overwrite） | ✅ |
| 发布/回滚关联 change record + approval reference | Admin API 写路径已带 change_record（I08）+ approval_ref（HTTP header）；UI 通过 API 调用间接关联 | ✅ |
| 敏感零落地 | 源码+构建产物扫描：无 localStorage/sessionStorage/Bearer 字面量 | ✅ |

## 关键设计

1. **UI 非权威**：唯一 fetch 基址 `/api/admin/v1`——React 组件不直接触碰数据库或内部服务；API 审计可按操作者关联（whoami → audit chain）。
2. **双防线权限**：whoami scopes 驱动 UI 写入口可见性（体验层）+ 服务端 scope/PDP 链（权威层——I05/I06 已交付的 403 语义）。
3. **冲突显式化**：409 → `BINDING_REVISION_CONFLICT` 信封 → UI conflict 提示 + 自动刷新到 API 权威状态（GWT#5：不显示本地状态伪造）。
4. **零敏感**：无 localStorage/sessionStorage/cookie 写入；无 Bearer token 字面量；构建产物无 token 形状。

## 审查 R1 整改（Findings → 修复 → 回归映射）

| Finding | 修复 | 回归 |
|---|---|---|
| P1-1 UI 读端点在服务端不存在（`internal/hub` 留白——`doc.go` 自述 "Implementation lands with I16"） | **实现 `internal/hub`**（module 03.2 API）：`GET /admin/v1/bindings`（is_active 200 行内）、`GET /admin/v1/resource-plans`、`GET /admin/v1/resource-plans/{id}`（含 plan_item join）；env 门控接入 cmd/control-plane-api（SAOAF_DB_DSN） | node:test 信封断言 + boundarycheck 27 包（hub 只 SQL 共享 schema） |
| P1-2 发布链路对真实后端必 403（无认证/无 approval-ref 输入口） | UI 增加 **审批引用输入**（X-Saoaf-Approval-Ref 头携带——表单字段 data-testid=approval-ref）；whoami 401 时显示「未认证」而非伪造写入口 | `审批引用链路` 测试（10/10 之一） |
| P1-3 错误信封违反冻结契约（嵌套 error.code vs flat error_code） | App.tsx 解析改为 flat `{error_code, message, request_id}`（contracts/schemas/v1/error-envelope.json）；hub 的 writeErr 按契约输出 flat 信封 | `错误信封契约` 测试断言 flat 解析 + `body.error?.code` 不存在 |
| P2-2 CI 未跑 npm test | quality.yml web job 增加 hub test suite 步骤 | CI 跑批 |
| P3-① 死代码 try 分支 | 重写为直接扫描 dist/assets/*.js（含 localStorage/sessionStorage 断言） | 修正后测试 |
| P2-1 幂等声明不实 | hub.handlePublish 实现 **服务端 CAS**（If-Match/expected_revision → 真实 UPDATE WHERE revision 比较 → 409 CONFLICT flat 信封）——幂等声明从 mock 升级为服务端事实 | mock 契约测试（error_code=CONFLICT + request_id） |
| P2-3 启动门禁（#5 OPEN） | I05 挂账同 #11/#13 先例（Mock 范围已并入 main，真实 Identity 联调 ledger）；此前 8 个 Issue 同口径开工 | — |
| P3-③ Scope 收窄 | 如实披露：交付 Binding 列表+发布 / Plan 列表+详情；创建/校验/影响分析 → I17 | — |

## 审查 R2 整改（复审新发现）

| Finding | 修复 | 证据 |
|---|---|---|
| R2-P1-1 admin+hub 双 Route("/admin/v1") 启动 panic（审查员构造共存配置实测 exit=2） | MountAdmin 增加 extend 钩子（路由挂入门控子树内）；hub.Mount 改收子路由 + 相对路径 | 冒烟：双面共存配置启动 ALIVE 无 panic |
| R2-P1-2 hub 读端点零认证匿名可读（审查实测无头 200） | hub 路由挂入 RequireIdentity+RateLimit 子树内 | 冒烟：匿名 GET /admin/v1/bindings → 401 UNAUTHENTICATED 信封 |
| R2-P1-3 发布链路 404（handlePublish 全仓零调用方——死代码） | handlePublish 真实接线（subrouter POST /bindings/{id}/publish）；**真实发布实现**：deactivate CAS + 新 PUBLISHED revision 插入（镜像 binding 模块 SQL）；幂等重发布 no-op | 代码 + 契约测试 |
| R2-P2 request_id 空串违 schema minLength | writeErr 兜底生成非空 request_id | 冒烟 401 信封 request_id=UUID |
| R2-P3-4 hub 零 Go 测试 | 如实披露（HTTP 面冒烟脚本 /tmp/hub-smoke.sh 记录于 evidence；Go 单测随 I17 管理面批次统一） | — |

## 审查 R3 整改（复审新发现——本轮为结构性返工）

R3 判定 CHANGES REQUESTED：R2 的「hub 内自注册 publish + minimal CAS」整改路线被否决——hub 二次注册 publish 路由被 chi **静默覆盖**（scope/PDP/审批/审计四道门禁整体剥离，实测 read-only token 可 200 真实发布），且 minimal CAS 绕过 binding 模块领域不变量（实测复活 SUSPENDED binding、change_record/outbox 零行）。整改为**发布链路彻底回归 I05 门控链 + 域模块单一实现**：

| Finding | 修复 | 证据 |
|---|---|---|
| R3-P1-1 publish 门禁被 hub 静默剥离（chi 同 pattern 后注册者覆盖；read-only token 实测 200 真实发布、零审计） | **hub 撤掉 publish 自注册**（纯读投影）；真实发布进入 I05 既有 `With(RequireScope, ApprovalGate)` 链 handler；`MountAdmin` 内嵌 **routeRecorder**——同 method+pattern 重复注册**启动即 panic**（把 chi 的静默覆盖语义变成 fail-fast） | `TestAdminRouteShadowingPanics`（回归）；冒烟 PROBE-C：read-only token POST publish → **403** + 0 行写入 |
| R3-P1-2 minimal CAS 绕过领域不变量（SUSPENDED 复活、change_record/outbox/approval_ref/published_at 全缺） | 删除 hub 侧 SQL 复刻；binding 模块新增 **`Store.Republish`**（加载当前行内容 → 走**完整** `Publish`：PublishGate、状态仲裁「publish supersedes PUBLISHED only」、overlap guard、幂等 ledger、change_record + outbox 事件同事务、approval_ref/published_at 持久化）；composition root（cmd/control-plane-api）通过 `PublishBinding` 钩子接线（ADR-0006：cmd 是唯一可同时见 platform 与 domain 的位置） | `TestDBRepublishSuspendedRejected`（SUSPENDED→ErrInvalidTransition、无复活、无多余审计行）；冒烟 PROBE-D2/D4：SUSPENDED publish → **409 CONFLICT**、状态保持 SUSPENDED；PROBE-E：change_record=2（域事务 1 + I05 audit sink 1）、outbox=1、approval_ref/published_at 落库 |
| R3-P2-1 并发竞态错误映射（UNIQUE 兜底 → 500 而非 409） | 复用域 Publish 的 `constraintOf` 分类 + sentinel 映射（ErrRevisionConflict/ErrDuplicateActive/ErrScopeConflict → ErrPublishConflict → 409） | `TestDBRepublishConcurrentSingleWinner`：20 并发恰 1 成功、19 全部**分类冲突**（0 个 unclassified） |
| R3-P3 writeErr 注释与实现不符（UUID vs UnixNano）+ pgxpool 失败静默 | `httpapi.WriteErr` 统一信封（注释如实）；main.go pgxpool 失败 `logger.Warn` | 代码 |

**发布链路最终形态**（审查员 R3「修复正解」原文的实现）：

```
POST /admin/v1/bindings/{id}/publish
  → RequireIdentity(401) → RequireScope("resource.publish")(403)
  → ApprovalGate(PDP + approval ref)(403)          ← I05 既有四道门禁，原样保留
  → httpapi.publishBinding handler
      → PublishBinding 钩子（composition root 接线）
      → binding.Store.Republish                     ← 域内单一实现，完整不变量
      → audit sink 写入（成功后）
```

hub（module 03.2）只保留三个读端点（bindings / resource-plans / resource-plans/{id}），全部在 `RequireIdentity+RateLimit` 子树内。

**R3 探针全量复放（真实二进制 + 真 PG 18.6 + 真 OIDC/PDP/审批 stub，18/18 PASS）**：
A 双面共存启动存活无 panic ｜ B1/B2 匿名读 401 + UNAUTHENTICATED 信封（request_id 非空）｜ C read-only token 发布 403 + 零写入 ｜ D1 合法 DRAFT 发布 200 ｜ D2/D3/D4 SUSPENDED 发布 409 CONFLICT + 无复活 ｜ E1-E4 change_record/outbox/approval_ref/published_at 全落库 ｜ F stale If-Match → 409 ｜ G1-G4 Idempotency-Key 重放 → already-published + 零新增 revision。

**新增测试**：binding `TestDBRepublish*` 9 个（真 PG）、httpapi `TestPublishHandler*`/`TestAdminRouteShadowingPanics`/`TestAdminExtensionKeepsGatedPublish` 9 个；web 测试 11/11（新增 change_reason 必填 + Idempotency-Key per intent 断言）；全仓 `-race` 22/22 包绿。

**UI 对齐**：发布表单增加必填「变更原因」（域 PublishGate 必填 change_reason，change record 关联）；Idempotency-Key **按意图生成**（binding+expected revision）并在同意图重试间复用（网络重试幂等而非双发）；按钮在 approval-ref/change-reason 齐全前禁用。

## 已知限制（挂账）

- **真实浏览器 E2E**：issue 要求 Playwright 或等价——当前交付为 node:test 源码/构建产物/契约断言（8/8）+ CI 的 `web build + npm audit` 门禁。真实浏览器 E2E 需要 Playwright/CDP 依赖引入与浏览器二进制——挂 I17 管理面批次（届时连带 I17 的管理界面一起做跨页面 E2E）。
- 绕过 UI 的 API 鉴权负向测试：**已在 I05/I06/I08 交付**（PDP fail-closed/scope 403/审批门禁——`middleware_test.go` 全套）；本 PR 的测试引用服务端已验证的语义。
- 回滚演练（旧制品部署 + 后台零变更 diff）：静态制品替换是部署操作——测试级证据为「UI 无直连数据库路径」（源码断言）；生产演练挂发布阶段。
- 前端错误率/成功率仪表盘 → I13/I17（监控面）。
- I16 范围内的 Binding/Plan 列表/详情/发布/回滚操作面已交付；创建/校验/影响分析 → I17（Sovereignty Operations 需要同一操作面 + Drill/Finding 管理视图）。
