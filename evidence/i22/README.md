# I22 证据：生产 Identity & Trust 接入（仓库内可判定面）

- 日期：2026-09-28 | 分支：`i22-identity` | 环境：Keycloak 26.7.4（真 realm 导入）+ Prism AuthZEN/approval 契约 mock + Go adapters（authn/authz/approval）

## 交付范围（仓库内可判定面 + 挂账划分）

issue 核心是「对接企业服务并保持契约不变」。**Mock 与生产 adapter 是同一代码**（endpoint 配置切换——issue AC「切换不要求修改领域模块」结构性成立）；企业测试环境端到端运行记录挂账（见下）。

### 真栈端到端（internal/platform/identity_e2e_test.go，CI identity-contract job 常驻）

- **令牌全链**：Keycloak 26 password grant → authn.Validator（签名/issuer/audience/expiry fail-closed）→ claims（sub/aud/scope/tenant_ref 全断言）
- **门序不变量**：401（匿名）→ 403（缺 scope）固定顺序（TestIdentityE2EGateOrder，真实签发令牌驱动）
- **跨租户声明信任**：outsider 令牌携带 tenant-b（用户属性映射）——租户声明只来自受信 token
- **PDP/审批真栈**：AuthZEN Prism evaluation + approval Prism 决策获取 + SatisfiedFor 链
- **Keycloak 26 无 admin 轮转 API 的如实披露**（POST /keys 405；KeyProvider 组件重配会损毁密钥栈——开发期亲证）：旧钥撤销语义由 validator 级 TestValidateFollowsKeyRotation 钉住（kid 消失→强制刷新→旧令牌拒）；e2e 断言活 issuer 的稳定验证 + RS256 轮转基底存在

### 契约故障注入套件（identity_contract_test.go，6/6）

| 测试 | 场景 | 断言 |
|---|---|---|
| TestPDPEveryFailureClassFailsClosed | 超时/断连/5xx/畸形体 四类 | 全部 ErrDenied——fail-closed 优先 |
| TestPDPFlappingNeverDegradesToAllow | PDP 抖动下 40 并发 | 0 降级放行、0 丢失 |
| TestApprovalRetryIdempotent | 崩溃重试 5 次 | 同引用同决策（幂等）、审批状态无漂移 |
| TestApprovalSelfApprovalRefused | requester=sole approver | 拒绝 + **其他主体也不能骑用该审批** |
| TestApprovalOutageFailsClosed | 审批服务宕机 | ErrRejected（门 403） |
| TestTenantTrustedClaimSourceOnly | 结构断言 | TenantRef 无任何 request-body 赋值路径 |

### 修复的真实缺陷

- **审批未绑定 requester**（SatisfiedFrom 忽略 Decision.RequesterRef——任何持有 approval ref 的主体可骑用他人审批；e2e 契约测试命中）→ `d.RequesterRef != requesterRef → false`
- **PDPClient 无超时注入面**（测试无法验证超时类 fail-closed）→ SetTimeout

### Mock 栈增强（realm json）

- e2e 用户预置（PBKDF2 哈希口令、requiredActions 空、tenant_ref 属性——**导入路径绕过 Keycloak 26 声明式 profile 的属性剥离**，admin API 路径会静默丢弃自定义属性）
- profile client scope 补建（sub/preferred_username/email mappers——realm 原缺，导致签发令牌无 sub）
- 契约测试专用 client 由测试经 admin API 自举（direct grants + realm scope mappers；409 幂等：secret+scopes 重置、requiredActions 清空）——生产用户仍走 PKCE 浏览器流（mock README 既有约定不变）

## 挂账（企业测试环境——外部依赖，同 #5/#11/#13 先例）

- 企业 OIDC issuer/audience/MFA/条件访问端到端运行记录（企业 IdP 对接后：adapter 同码、endpoint 切换）
- 企业 workload identity/mTLS 信任链 + 证书签发/轮转/撤销真实演练（证书生命周期属企业 CA；协议面由 mocks/identity/workload 的 SPIFFE 测试 SVID 覆盖解析与 mTLS wiring）
- AuthZEN-compatible PDP 与企业审批服务的生产实例联调记录
- 生产 break-glass/职责分离实装（HoldGate 先例在 I23；角色源接入企业 IdP 后接线）
- 回滚计时、旧证书撤销的生产演练记录

**回滚不变量（结构性）**：生产不得回退 Phase 0 Mock——adapter 切换是 endpoint 配置，无代码回滚面；管理写 fail-closed（PDP/审批断连即 403）由故障注入套件钉住。

## 测试基线

`internal/platform` 全套 -race 绿（8 包）；CI identity-contract job（Keycloak+Prism 容器）常驻；boundary 29 包 PASS；license PASS。
