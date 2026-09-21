---
title: AI Resource Resolver API 契约
version: 1.0.0
status: proposed
owner: TODO-ARR技术负责人
reviewers:
  - TODO-Agent-Harness负责人
  - TODO-MMR负责人
  - TODO-IAM负责人
created: 2026-09-21
updated: 2026-09-21
classification: internal
related:
  - ../programs/03-sovereign-ai-open-ai-fabric/module-contracts.md
  - ../architecture/ai-routing-platform-4a.md
  - ./resource-resolver-events-and-data.md
  - ./multi-model-router-integration.md
---

# AI Resource Resolver API 契约

## 1. 目的与约束

本文定义一期可实现的 HTTP/JSON 契约。实现前应据此生成 OpenAPI 3.1 文档和 JSON Schema；若 MMR 已统一使用 gRPC，可在保持相同语义的前提下生成 protobuf adapter。

- Base path：`/api/ai-resource-resolver/v1`
- Content-Type：`application/json`
- 时间：RFC 3339 UTC
- ID：UUIDv7 或企业统一有序 ID
- 金额不在本项目契约中；不得以浮点数表示配额或成本约束
- 未声明的字段在 v1 默认拒绝，除 `extensions` 对象外
- Runtime API 与 Management API 使用独立 route、权限和限流池

## 2. 通用请求头

| Header | 必填 | 说明 |
|---|---|---|
| `Authorization` | 是 | 企业签发的 Bearer Token；服务间可叠加 mTLS |
| `traceparent` | 是 | W3C Trace Context；缺失时入口生成 |
| `tracestate` | 否 | W3C Trace Context |
| `X-Request-Id` | 是 | 调用方请求 ID；仅用于追踪，不作为幂等键 |
| `Idempotency-Key` | 管理写/Resolve重试时是 | 同一 caller + endpoint 下 24 小时唯一 |
| `X-Tenant-Ref` | 是 | 已验证租户引用，不接受自由文本身份 |
| `X-Environment` | 是 | `dev/test/staging/prod` 或企业规范值 |

服务从已验证 Token/网关注入获得 caller、tenant、factory、region 等声明；请求体中的同名字段不得覆盖身份声明。

## 3. Runtime API

### 3.1 解析资源计划

`POST /resource-plans:resolve`

#### 请求

```json
{
  "contract_version": "1.0",
  "task_ref": "task-01K5...",
  "requirements": [
    {
      "requirement_id": "req-model-1",
      "capability_id": "model.reasoning.high",
      "capability_major_version": 1,
      "resource_type": "MODEL_PROVIDER",
      "constraints": {
        "data_classification_max": "CONFIDENTIAL",
        "region": "cn-east",
        "streaming_required": true
      }
    },
    {
      "requirement_id": "req-tool-1",
      "capability_id": "tool.erp.work-order.read",
      "capability_major_version": 1,
      "resource_type": "TOOL_PROVIDER",
      "constraints": {
        "risk_level_max": "LOW",
        "idempotency_required": true
      }
    }
  ],
  "options": {
    "max_plan_ttl_seconds": 300,
    "include_explanations": true
  },
  "extensions": {}
}
```

规则：

- `requirements` 1–20 项，`requirement_id` 在请求内唯一；
- `capability_id` 必须是 Registry 中的稳定 key，不接受自然语言；
- `constraints` 必须通过该 Capability 的 `requirement_schema`；
- 请求总大小不超过 256 KiB；
- 同一 `Idempotency-Key` 和相同规范化请求返回同一 Plan；相同 key 不同请求返回 409。

#### 成功响应 `200`

```json
{
  "contract_version": "1.0",
  "resource_plan_id": "0199...",
  "task_ref": "task-01K5...",
  "status": "RESOLVED",
  "created_at": "2026-09-21T02:10:00Z",
  "expires_at": "2026-09-21T02:15:00Z",
  "decision_fingerprint": "sha256:...",
  "items": [
    {
      "requirement_id": "req-model-1",
      "capability": {
        "id": "model.reasoning.high",
        "major_version": 1,
        "revision": 12
      },
      "binding": {
        "id": "0199...",
        "revision": 7
      },
      "provider": {
        "id": "multi-model-router",
        "type": "MODEL_PROVIDER",
        "endpoint_ref": "svc://ai-routing/mmr-runtime",
        "contract_version": "2026-06"
      },
      "invocation": {
        "protocol": "HTTP_JSON",
        "profile_or_action": "reasoning-high-v2"
      },
      "reason_codes": ["CAPABILITY_MATCH", "REGION_MATCH", "DATA_CLASS_MATCH"]
    }
  ],
  "trace_id": "4bf92f3577b34da6a3ce929d0e0e4736"
}
```

`endpoint_ref` 必须由 Harness 的企业服务发现 adapter 解析，不返回含密钥的完整 URL。`invocation` 只含调用描述符，不包含凭证、工具参数、Prompt 或上下文内容。

#### 错误

| HTTP | code | 含义 | 是否可重试 |
|---:|---|---|---|
| 400 | `INVALID_REQUIREMENT` | Schema 或语义无效 | 否 |
| 401 | `UNAUTHENTICATED` | 身份无效 | 刷新身份后 |
| 403 | `CALLER_NOT_ALLOWED` | 无权调用 Resolve | 否；由 IAM 决定 |
| 404 | `CAPABILITY_NOT_FOUND` | 能力或 major 不存在 | 否 |
| 409 | `IDEMPOTENCY_CONFLICT` | 幂等键重用但内容不同 | 否 |
| 409 | `AMBIGUOUS_BINDING` | 同优先级存在冲突 | 否；平台修复配置 |
| 422 | `NO_COMPATIBLE_PROVIDER` | 无满足约束的 Binding | 修改需求或配置 |
| 424 | `PROVIDER_SNAPSHOT_EXPIRED` | 专业 Snapshot 过期 | 平台刷新后 |
| 429 | `RATE_LIMITED` | 超出调用方限制 | 按 `Retry-After` |
| 503 | `RESOLVER_UNAVAILABLE` | 无可用发布快照/数据库故障 | 指数退避 |

错误信封：

```json
{
  "error": {
    "code": "NO_COMPATIBLE_PROVIDER",
    "message": "No active binding satisfies requirement req-tool-1",
    "request_id": "...",
    "trace_id": "...",
    "retryable": false,
    "details": [
      {"requirement_id": "req-tool-1", "reason_codes": ["REGION_MISMATCH"]}
    ]
  }
}
```

消息不得泄露候选 Provider 的敏感配置或调用者无权查看的资源名称。

### 3.2 查询资源计划

`GET /resource-plans/{resource_plan_id}`

只允许原 caller、同任务的受信服务或审计角色读取。返回值与 Resolve 响应一致；过期后在审计保留期内返回 `status=EXPIRED`，不代表仍可执行。

### 3.3 解析健康检查

- `GET /health/live`：进程存活，不检查依赖。
- `GET /health/ready`：数据库可读且至少一个发布快照加载完成。
- `GET /health/startup`：迁移完成、配置校验通过。

健康接口不得返回连接串、版本漏洞信息或 Provider 目录。

## 4. Management API

### 4.1 Capability

| 方法 | 路径 | 权限 | 说明 |
|---|---|---|---|
| `POST` | `/capabilities` | `resource.write` | 创建 DRAFT major/revision |
| `GET` | `/capabilities/{key}/versions/{major}` | `resource.read` | 读取指定版本 |
| `GET` | `/capabilities` | `resource.read` | 游标分页与过滤 |
| `POST` | `/capabilities/{id}:publish` | `resource.publish` | 发布不可变 revision |
| `POST` | `/capabilities/{id}:deprecate` | `resource.publish` | 设置退役窗口 |

创建请求关键字段：

```json
{
  "capability_key": "context.maintenance.manual.retrieve",
  "major_version": 1,
  "resource_type": "CONTEXT_PROVIDER",
  "description": "Retrieve approved maintenance manuals",
  "owner_ref": "group:ai-context-platform",
  "requirement_schema": {},
  "default_plan_ttl_seconds": 300,
  "data_classification_max": "CONFIDENTIAL",
  "extensions": {}
}
```

### 4.2 Provider

| 方法 | 路径 | 权限 | 说明 |
|---|---|---|---|
| `POST` | `/providers` | `resource.write` | 创建 Provider |
| `GET` | `/providers/{provider_key}` | `resource.read` | 查看逻辑 Provider |
| `POST` | `/providers/{id}:certify` | `resource.publish` | 认证 Provider 契约 |
| `POST` | `/providers/{id}:suspend` | `resource.suspend` | 紧急停止新解析 |
| `POST` | `/providers/{id}:resume` | `resource.suspend` | 恢复前需重新校验 Snapshot |

Provider 创建请求：

```json
{
  "provider_key": "multi-model-router",
  "provider_type": "MODEL_PROVIDER",
  "owner_ref": "group:mmr-platform",
  "endpoint_ref": "svc://ai-routing/mmr-runtime",
  "control_endpoint_ref": "svc://ai-routing/mmr-control",
  "supported_regions": ["cn-east", "cn-north"],
  "lifecycle_state": "DRAFT"
}
```

### 4.3 Provider Snapshot

`POST /providers/{provider_id}/snapshots`

由专业平台身份调用。请求：

```json
{
  "snapshot_version": "2026-09-21T02:00:00Z-17",
  "contract_version": "2026-06",
  "generated_at": "2026-09-21T02:00:00Z",
  "valid_until": "2026-09-21T02:30:00Z",
  "digest": "sha256:...",
  "profiles": [
    {
      "profile_id": "reasoning-high-v2",
      "capability_keys": ["model.reasoning.high"],
      "regions": ["cn-east"],
      "data_classification_max": "CONFIDENTIAL",
      "status": "AVAILABLE",
      "constraints_schema_version": "1.2"
    }
  ],
  "signature": "TODO-由企业签名规范确定"
}
```

ARR 校验 Provider 身份、时间窗口、digest、契约 major 和重复版本。它不接受具体模型名、供应商、权重、价格、密钥、实例健康或回退链字段。

### 4.4 Binding

| 方法 | 路径 | 权限 | 说明 |
|---|---|---|---|
| `POST` | `/bindings` | `resource.write` | 创建 DRAFT Binding |
| `POST` | `/bindings/{id}:validate` | `resource.write` | 对 Snapshot 和规则校验 |
| `POST` | `/bindings/{id}:publish` | `resource.publish` | 激活新 revision |
| `POST` | `/bindings/{id}:rollback` | `resource.publish` | 指向前一已发布 revision |
| `POST` | `/bindings/{id}:retire` | `resource.publish` | 停止新 Plan 使用 |

```json
{
  "capability_ref": {"key": "model.reasoning.high", "major_version": 1},
  "provider_ref": "multi-model-router",
  "profile_or_action": "reasoning-high-v2",
  "environment": "prod",
  "scope": {
    "tenant_refs": ["tenant-a"],
    "regions": ["cn-east"]
  },
  "constraints": {
    "data_classification_max": "CONFIDENTIAL"
  },
  "priority": 100,
  "selection_mode": "SINGLE",
  "change_reason": "Enable production reasoning profile",
  "ticket_ref": "CHG-12345"
}
```

生产 publish 必须携带外部审批决定引用，或由入口网关注入：

```json
{
  "expected_revision": 4,
  "approval_decision_ref": "authz-decision-...",
  "effective_at": "2026-09-21T03:00:00Z"
}
```

## 5. 过滤、分页与并发

- 列表使用 opaque cursor，不使用 offset：`?limit=100&cursor=...`。
- `limit` 默认 50，最大 200。
- 管理资源返回 `ETag`；更新必须用 `If-Match` 或 `expected_revision`。
- 过滤只开放白名单字段，如 `state`、`owner_ref`、`resource_type`、`updated_after`。
- 所有排序必须稳定并包含 ID 作为最后排序键。

## 6. 幂等与重试

| 操作 | 幂等策略 |
|---|---|
| Resolve | `caller + Idempotency-Key + request_digest`；24 小时 |
| Snapshot 发布 | `provider_id + snapshot_version + digest` |
| Binding publish/rollback | `binding_id + target_revision + Idempotency-Key` |
| suspend/resume | 目标状态幂等 |

客户端只对 429、502、503、504 重试，使用带抖动指数退避，最多 3 次；管理写在不确定结果时必须先按幂等键查询，不得生成新键重复提交。

## 7. 契约兼容性

- URL 中的 `/v1` 是 API major；破坏性变化发布 `/v2`。
- 同 major 内只新增可选字段、枚举值需使用 `UNKNOWN` 安全处理策略。
- Capability `requirement_schema` 破坏性变化升 Capability major。
- Provider `contract_version` 采用 major/minor；ARR 至少支持当前和前一 major 的 adapter，期限由平台政策确定。
- CI 必须运行消费者驱动契约测试：Harness、MMR、Context、Tool 各维护最小消费者样例。

## 8. 实现验收清单

- [ ] OpenAPI lint 通过，示例可被 Schema 校验。
- [ ] 同一幂等键并发 100 次只创建一个 Plan/版本。
- [ ] 未知字段、超大请求、深层 JSON、重复 requirement 均被拒绝。
- [ ] 管理写乐观锁冲突返回 409，不丢失更新。
- [ ] 错误不暴露候选资源、内部 SQL、密钥或调用载荷。
- [ ] N-1 消费者契约测试通过。
- [ ] API 网关、应用和数据库审计能用 `trace_id/request_id` 关联。
