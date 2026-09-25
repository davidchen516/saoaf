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

## 已知限制（挂账）

- **真实浏览器 E2E**：issue 要求 Playwright 或等价——当前交付为 node:test 源码/构建产物/契约断言（8/8）+ CI 的 `web build + npm audit` 门禁。真实浏览器 E2E 需要 Playwright/CDP 依赖引入与浏览器二进制——挂 I17 管理面批次（届时连带 I17 的管理界面一起做跨页面 E2E）。
- 绕过 UI 的 API 鉴权负向测试：**已在 I05/I06/I08 交付**（PDP fail-closed/scope 403/审批门禁——`middleware_test.go` 全套）；本 PR 的测试引用服务端已验证的语义。
- 回滚演练（旧制品部署 + 后台零变更 diff）：静态制品替换是部署操作——测试级证据为「UI 无直连数据库路径」（源码断言）；生产演练挂发布阶段。
- 前端错误率/成功率仪表盘 → I13/I17（监控面）。
- I16 范围内的 Binding/Plan 列表/详情/发布/回滚操作面已交付；创建/校验/影响分析 → I17（Sovereignty Operations 需要同一操作面 + Drill/Finding 管理视图）。
