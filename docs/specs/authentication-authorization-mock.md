---
title: SAOAF 身份认证、登录与授权 Mock 设计
version: 1.1.0
status: proposed
owner: TODO-安全与IAM负责人
created: 2026-09-22
updated: 2026-09-22
classification: public
---

# SAOAF 身份认证、登录与授权 Mock 设计

## 1. 结论与边界

SAOAF 03 工程不是身份或授权权威。生产环境必须接入 04 Identity & Trust：人员、Agent、Workload、租户、组织和角色由企业 IAM 管理，PDP 作最终授权裁决，审批系统管理人工批准。03 工程只验证身份凭据、传递受信声明、请求授权决定，并在自己的 API 和数据面执行决定。

本 Mock 只服务本地开发、CI、契约测试和演示：

- Keycloak 提供真实 OIDC discovery、Authorization Code + PKCE、Token 和 JWKS 行为；
- Prism 根据 OpenAPI 示例模拟 OpenID AuthZEN Authorization API 1.0 的 PDP 响应；
- Mock 不进入生产，不保存真实人员，不代表企业角色模型，也不签发生产凭据；
- Resource Plan、`policy_decision_id`、`approval_ref` 都只是关联引用，不是授权令牌。

## 2. 标准基线

| 场景 | 强制基线 | 本项目处理方式 |
|---|---|---|
| 人员登录 | OpenID Connect Core 1.0；OAuth Authorization Code；PKCE `S256` | 本地 Keycloak；生产由企业 IdP 承担 |
| 浏览器会话 | BFF；`Secure`、`HttpOnly`、`SameSite=Lax/Strict` Cookie；CSRF 防护 | Token 不进入浏览器 `localStorage` |
| API Token | RFC 9700；RFC 8725；RFC 8414 metadata | 固定允许算法，校验 `iss/aud/exp/nbf`，使用 discovery/JWKS 并支持轮转 |
| 服务身份 | OAuth client authentication 的非对称方式；mTLS（RFC 8705）或 SPIFFE X.509-SVID | Mock 只验证接口；生产由企业 Workload Identity 签发短期身份 |
| 授权 | OpenID AuthZEN Authorization API 1.0 | PEP 调用 `/access/v1/evaluation`；默认拒绝、失败关闭 |
| 委派 | OAuth Token Exchange（RFC 8693），仅在 04 工程批准后启用 | 03 不自行签发 delegation token |

禁止 Resource Owner Password Credentials、Implicit Flow、通配 redirect URI、长期 bearer token、共享服务账号、把 ID Token 当 Access Token，以及把角色或 `tenant_ref` 从请求体当成受信信息。

## 3. 三类调用

### 3.1 人员访问管理界面

1. 浏览器访问 BFF；BFF 生成一次性的 `state`、`nonce` 和 PKCE verifier。
2. IdP 完成 MFA/企业条件访问并返回 authorization code。
3. BFF 用 code + verifier 换取 Token，验证 issuer、audience、签名、时间和 nonce。
4. BFF 仅向浏览器写入不透明会话 Cookie；后端 Token 留在服务器端。
5. 每个管理写操作向 PDP 请求授权；发布、暂停、恢复和退出演练还要验证 `approval_ref`。

本地 Mock 直接提供公共 PKCE client，方便没有 BFF 代码时调试协议；这不是生产部署模板。

### 3.2 服务到服务

入口或 service mesh 先验证 mTLS/Workload Identity，再将不可伪造的 principal 传给应用。应用只接受受信代理注入或经签名 Token 验证得到的 `sub`、`tenant_ref`、`client_id`、`scope`；来自普通请求头或请求体的同名字段必须丢弃。生产优先使用短期 X.509-SVID/mTLS 或 `private_key_jwt`，不使用仓库内静态 client secret。

### 3.3 授权决策

PEP 使用 AuthZEN 的 Subject、Action、Resource、Context 模型。示例：

```json
{
  "subject": {"type": "identity", "id": "user:alice"},
  "action": {"name": "resource.publish"},
  "resource": {"type": "capability-binding", "id": "binding:mmr-default"},
  "context": {
    "tenant_ref": "tenant:demo",
    "environment": "production",
    "approval_ref": "approval:chg-1001"
  }
}
```

允许响应带 `decision=true` 和 `policy_decision_id`；拒绝、超时、格式错误、未知决定或缺少必须 obligation 时全部失败关闭。调用方必须把 `X-Request-ID`、`policy_decision_id` 和执行结果写入审计关联，禁止记录 Token、Cookie、授权码或敏感业务载荷。

## 4. Mock 资产

| 资产 | 用途 |
|---|---|
| `mocks/identity/compose.yaml` | 启动 Keycloak 和 AuthZEN Prism |
| `mocks/identity/keycloak/realm-saoaf-dev.json` | 导入 realm、scope、role 和 PKCE client；不含用户和凭据 |
| `mocks/identity/authzen/openapi.yaml` | 可导入 Prism/Microcks 的 AuthZEN 契约及 allow/deny 示例 |
| `mocks/identity/approval/openapi.yaml` | 审批请求、查询和批准/拒绝状态契约 |
| `mocks/identity/workload/generate-test-svid.sh` | 运行时生成带 SPIFFE URI SAN 的短期测试证书；不提交私钥 |

Mock 不预置密码。开发者在 Keycloak 管理界面创建临时用户，或通过 CI 的密钥注入机制在运行时创建；凭据不得提交到仓库。

固定 Mock 参数如下：

- issuer：`http://127.0.0.1:8080/realms/saoaf-dev`；
- audience：`saoaf-control-plane`；
- MFA：TOTP，所有新建测试用户首次登录配置；
- workload principal：`spiffe://saoaf.test/ns/default/sa/resource-resolver`；
- PDP：`http://127.0.0.1:4010/access/v1/evaluation`；
- approval：`http://127.0.0.1:4011/approvals/v1/requests`。

## 5. 最小权限模型

| Scope / Action | 说明 |
|---|---|
| `resource.read` | 浏览 Capability、Provider、Binding 和 Plan 摘要 |
| `resource.write` | 创建草稿和修改非生产元数据 |
| `resource.publish` | 发布生产 Binding；必须有授权决定和审批引用 |
| `resource.suspend` | 暂停 Provider/Binding；安全事件可以走受审计的紧急流程 |
| `sovereignty.read` | 读取主权约束和证据摘要 |
| `exitpack.write` | 管理 Exit Pack，不代表有权执行退出动作 |
| `drill.approve` | 批准退出演练；申请人与批准人必须可分离 |

Role 仅用于 Mock 演示，生产授权不能只依赖 Token 中的粗粒度角色；资源环境、租户、Owner、风险等级和审批状态由 PDP 综合裁决。

## 6. 必须通过的负向验收

1. 缺失、过期、尚未生效、错误 issuer/audience、未知算法或签名失败的 Token 返回 401。
2. 身份有效但 scope/授权不足返回 403；不得混淆为 404 以外的成功路径。
3. redirect URI 非精确匹配、PKCE 非 `S256`、state/nonce 重放必须失败。
4. 请求体/普通 Header 伪造 `subject`、`tenant_ref`、`role` 不得覆盖受信声明。
5. PDP 超时、5xx、无 `decision`、响应 request ID 不匹配或 obligation 不受支持时 fail closed。
6. 同一发布请求重试不得绕过审批或产生重复状态迁移；审批过期或对象摘要变化后必须重新审批。
7. Key/JWKS 轮转期间双版本验证；撤销后旧凭据不得继续建立新会话。
8. 日志和 trace 中不得出现 Authorization header、Cookie、code、refresh token 或 client secret。

## 7. 生产替换门槛

进入生产前必须用企业事实替换 Mock：OIDC issuer/audience、注册 client、MFA/条件访问、用户与组生命周期、workload trust domain、PDP metadata/endpoint、obligation profile、审批实现、密钥轮转、撤销传播、审计保留期、RTO/RPO 和 break-glass 流程。替换只修改适配器和配置，不改变 SAOAF 的身份/授权边界。

## 8. 开源选择记录

| 候选 | 结论 | 原因与退出条件 |
|---|---|---|
| Keycloak 26.7.4 | 选择作 OIDC Mock | Apache-2.0、标准协议完整、可导入 realm；生产仍由 04 工程选型和运维 |
| Stoplight Prism 5.15.10 | 选择作轻量 AuthZEN Mock | Apache-2.0、直接读取 OpenAPI、启动成本低；复杂状态/测试治理成熟后可切 Microcks |
| Microcks 1.14.x | 保留为契约测试平台 | CNCF、Apache-2.0、能力完整，但单个 Mock 的本地依赖更重 |
| 自写 JWT/PDP Mock | 否决 | 容易掩盖算法、issuer、audience、轮转和失败关闭问题 |

## 9. 官方参考

- [OpenID Connect Core 1.0](https://openid.net/specs/openid-connect-core-1_0.html)
- [OAuth 2.0 Security Best Current Practice, RFC 9700](https://datatracker.ietf.org/doc/html/rfc9700)
- [JWT Best Current Practices, RFC 8725](https://datatracker.ietf.org/doc/html/rfc8725)
- [OAuth mTLS, RFC 8705](https://datatracker.ietf.org/doc/html/rfc8705)
- [DPoP, RFC 9449](https://datatracker.ietf.org/doc/html/rfc9449)
- [OpenID AuthZEN Authorization API 1.0](https://openid.net/specs/authorization-api-1_0.html)
- [SPIFFE Standard](https://spiffe.io/docs/latest/spiffe-specs/)
- [Keycloak documentation](https://www.keycloak.org/documentation)
