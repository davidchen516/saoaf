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

## 审查 R1 整改（CHANGES REQUESTED → 全项修复）

| Finding | 修复 | 验证 |
|---|---|---|
| P1-1 TestTenantTrustedClaimSourceOnly 恒绿（字面子串伪正则——注入 body 租户赋值仍 PASS） | 重写为**行为断言**（真 issuer 令牌 validate → tenant_ref 必须等于 token claim）+ **真 regex** 结构断言（`TenantRef:\s*id\.TenantRef` 必在；`TenantRef:\s*(body\|input\|payload\|req\|json)` 必不在——admin.go + ops.go） | **红运行验证**：注入合法语法 `TenantRef: bodyTenant.TenantRef` → 测试红（"request-body tenant assignment found"）；恢复绿 |
| P2-1 GateOrder 403 腿从未行使（宽 client 带 resource.publish → 200 逃生口） | **窄 scope client**（resource.read only + fullScopeAllowed:false）专用 mint（kcTokenClient）→ 断言**硬 403**，逃生口删除 | e2e 双测真栈绿（含 fresh-client 传播重试） |
| P2-2 SatisfiedFor 空 requester_ref fail-open | `d.RequesterRef == "" → false`（畸形/降级审批响应拒绝——同 ApproverRefs 空先例） | 契约套件 |
| P2-3 旧钥拒收声称失实（rotation 测试只断言新钥过） | TestValidateFollowsKeyRotation 补断言 **旧令牌必须拒**（rotateKey 换钥集：JWKS handler 改为请求时读当前钥——旧 kid 消失）；rotateSeq 计数器 | authn 套件 -race 绿 |
| P3-1 根目录 0 字节 ARCHIVED 误提交 | 删除 | — |
| P3-2 e2e 头注释轮转条目自相矛盾 | 头注释改为与正文一致的弱性质披露（validator 级钉住） | 文本 |
| P3-3 narrow/GateOrder client 无 cleanup；头注释 "removed afterwards" 过宽 | narrow client t.Cleanup 删除；注释按实收窄 | 代码 |
| P3-4 死代码（errDenier/containsErr/matchSimple/grepFiles/var _=fmt） | 全部删除；errors2 用 strings.Contains | 编译器 |
| 附带：RateLimit(0,0) 除零（MountAdmin 零配置 panic——行为测试新触发面） | rate 0 = 禁用限流（守卫） | 测试通过 |

整改后：`internal/platform` 全套 -race 绿（7 包 + 根包）；e2e 双测真栈绿（含窄 scope 硬 403）。

**挂账（R1 OBS）**：wildcardDigest "sha256:mock" 生产放行面（I05 预留——真实审批服务不得回传该字面量；随企业审批服务接入时收口）；CI identity job Prism 就绪等待已补。

## 审查 R2 整改（残余 1×P2 + 3×P3 → 全项修复）

| Finding | 修复 | 验证 |
|---|---|---|
| NEW-P2-1 空 requester fail-open 修复无回归钉（蓝军 revert 全套绿） | **TestSatisfiedForEmptyRequesterRefFailsClosed**（approval 包）：空 requester 对任意主体 false + 空 subject false + 对照腿（有 requester 通过） | **蓝军反验**：还原 `!= "" &&` 短路 → 测试红；恢复绿 |
| NEW-P3-1 P1-1 重写的行为腿是死代码剧场（mount 未 drive；fakePublish 记录器全弃用） | **删除全部死支架**（fakePublish/gotTenant/gotActor/req/adminCfg）；注释改为如实：行为腿=validator 层 claim 断言 + regex 辅助 + 行为保护网指向 admin_test.go 的 TestPublishHandlerSuccessPassthrough（该测试亲证能抓间接绕过探针） | 代码 + 注释 |
| NEW-P3-2 regex 可被间接化绕过（tenantOverride 别名）+ 正向 pin 被 audit 行满足 | 测试注释如实声明 regex 仅防直引形态；**行为安全网**（admin_test 捕获 input.TenantRef 断言）在注释中显式指向——R2 亲证该网抓住间接探针 | 注释 |
| NEW-P3-3 RateLimit rate=0 注释称"disable"实为 deny-all（refill 停但 token 检查在，(0,0) 全 429） | **真 pass-through**：refill==0 → next.ServeHTTP 直通（无桶记账） | middleware 套件绿（含既有限流测试不回归） |

整改后：`internal/platform` 8 包 -race 全绿；e2e 双测真栈绿（17.9s 冷栈首次 mint + 0.2s）；regex 门禁红运行复验（bodyTenant 注入 → 红）。
