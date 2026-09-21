---
title: AI Resource Resolver 与 Multi-Model Router 集成设计
version: 1.0.0
status: proposed
owner: TODO-ARR与MMR联合负责人
reviewers:
  - TODO-MMR架构负责人
  - TODO-Agent-Harness负责人
  - TODO-SRE负责人
created: 2026-09-21
updated: 2026-09-21
classification: internal
related:
  - ../programs/03-sovereign-ai-open-ai-fabric/architecture.md
  - ../programs/03-sovereign-ai-open-ai-fabric/module-contracts.md
  - ../architecture/ai-routing-platform-4a.md
  - ./resource-resolver-api.md
---

# AI Resource Resolver 与 Multi-Model Router 集成设计

## 1. 集成结论

MMR 是 `MODEL_PROVIDER` 域的唯一权威服务。ARR 只持有 MMR 的稳定逻辑 profile Snapshot，并在 Resource Plan 中返回 MMR 的服务引用和 profile。Agent Harness 直接调用 MMR；模型请求、流式响应和 Token 计量不经过 ARR。

```text
Capability requirement
  -> ARR: choose MMR + logical profile
  -> Harness: invoke MMR directly
  -> MMR: choose concrete model/provider/deployment/fallback
  -> Harness/Telemetry: correlate resource_plan_id + model_route_decision_id
```

### 1.1 MMR 实现基线

已建成 MMR 主要基于 [vLLM Semantic Router](https://github.com/vllm-project/semantic-router) 构建。该项目采用 Apache-2.0，定位为异构模型推理前的可编程 Mixture-of-Models 决策层。其官方拓扑由 Envoy 承载数据面流量，通过 ExtProc 调用 Semantic Router；后端 vLLM 或其他 OpenAI-compatible Provider 负责实际推理。Semantic Router 不加载模型权重，也不替代企业 API Gateway、模型 Serving、GPU 调度或容量管理。

SAOAF 与 vLLM Semantic Router 对象的映射如下：

| SAOAF / MMR 对象 | vLLM Semantic Router 对象 | 集成规则 |
|---|---|---|
| MMR logical profile | stable Entrypoint + Recipe | ARR 只看稳定 profile ID 和契约摘要 |
| 任务/风险/模态/复杂度信号 | Signal / Projection | MMR 内部持有，不复制到 ARR |
| 硬约束与路由策略 | Decision | MMR 执行；主权和授权约束仍来自 03.1/04 |
| 单模型、级联或受控多模型路径 | Algorithm + Plugins | MMR 负责执行和证据，ARR 不编排 |
| 具体模型或供应商 endpoint | Provider model | MMR 权威，禁止进入 ARR Snapshot |
| `model_route_decision_id` | 企业证据扩展 | 必须由 MMR 生成并跨响应/事件保持唯一 |

上游版本必须按不可变 tag/digest 锁定。当前架构评审以 v0.3.0 能力和官方文档为参考；生产采用版本、企业 fork 差异、Canonical YAML/Recipe 管理、升级和回滚路径仍由 MMR Phase 0 盘点确认。SAOAF 不 fork 或内嵌 Semantic Router，仅维护面向 MMR 稳定契约的薄 adapter。

## 2. 权威边界

| 对象/决策 | ARR | MMR |
|---|---|---|
| `model.reasoning.high` 等企业能力语义 | 权威 | 映射支持 |
| 能力到 MMR profile 的跨域 Binding | 权威 | 提供可绑定 profile |
| profile 到具体模型、供应商、部署 | 不知情 | 权威 |
| 价格、配额、负载、健康、回退链 | 不知情 | 权威 |
| Resource Plan ID | 权威 | 透传/记录 |
| Model route decision ID | 仅保存引用 | 权威 |
| 模型请求/响应/流式数据 | 不接收 | 处理 |
| 模型使用量与成本明细 | 不复制 | 权威，向 FinOps/观测输出 |

## 3. MMR 必须提供的控制面契约

### 3.1 Logical Profile Snapshot

最低字段：

```json
{
  "provider_id": "multi-model-router",
  "snapshot_version": "2026-09-21T02:00:00Z-17",
  "contract_version": "2026-06",
  "generated_at": "2026-09-21T02:00:00Z",
  "valid_until": "2026-09-21T02:30:00Z",
  "profiles": [
    {
      "profile_id": "reasoning-high-v2",
      "capability_keys": ["model.reasoning.high"],
      "regions": ["cn-east"],
      "data_classification_max": "CONFIDENTIAL",
      "features": {
        "streaming": true,
        "structured_output": true,
        "tool_calling": true
      },
      "constraint_schema_version": "1.2",
      "status": "AVAILABLE"
    }
  ],
  "digest": "sha256:...",
  "signature": "TODO-enterprise-signature"
}
```

禁止字段：

- 具体模型或供应商名称；
- endpoint、密钥、账号和部署 ID；
- 流量权重、价格规则、回退顺序；
- 单实例或单模型健康；
- Prompt 模板和 Guardrail 内容。

如果某些模型名称因合规必须显式声明，由 MMR 自己在执行 API 和审计中处理，不能把内部路由目录复制到 ARR。

### 3.2 Snapshot 传输

首选方式：MMR 在 profile 变化、契约变化或周期心跳时调用 ARR Snapshot API。备选方式：ARR 定时拉取 MMR 控制面。不得同时启用推送和拉取作为双权威。

建议：

- 变化即推送，至少每 10 分钟刷新一次 `valid_until`；
- `valid_until` 建议为 30 分钟，具体由故障预算确认；
- ARR 校验 MMR workload identity、digest、签名和单调版本；
- 相同 `snapshot_version + digest` 幂等；版本相同而 digest 不同必须拒绝并告警；
- profile 状态只允许 `AVAILABLE/DEGRADED/UNAVAILABLE/RETIRED`。

## 4. MMR 运行时契约要求

ARR 不重新定义现有 MMR API，只要求 MMR 在现有请求中支持以下字段或等价 metadata：

```json
{
  "model_profile": "reasoning-high-v2",
  "resource_plan_id": "0199...",
  "resource_plan_item_id": "0199...",
  "task_ref": "task-...",
  "constraints": {
    "streaming_required": true
  }
}
```

同时传递：

- W3C `traceparent/tracestate`；
- 已验证 caller/tenant 身份上下文；
- MMR 自己要求的幂等键、配额和预算引用。

MMR 响应至少增加：

```json
{
  "model_route_decision_id": "mrd-...",
  "resource_plan_id": "0199...",
  "status": "SUCCEEDED",
  "usage_ref": "usage-..."
}
```

流式响应可在 headers/trailers 或最终事件中返回 decision ID。具体模型名称是否返回给 Harness 由 MMR 的合规策略决定，与 ARR 无关。

## 5. 失败语义

| 场景 | 责任方 | 行为 |
|---|---|---|
| ARR 找不到可用 MMR Binding | ARR | `NO_COMPATIBLE_PROVIDER`，不调用 MMR |
| MMR Snapshot 过期 | ARR/MMR控制面 | 新 Plan fail closed；告警 Snapshot age |
| MMR profile 不存在 | MMR | 返回 `PROFILE_NOT_FOUND`；视为契约漂移，禁止 ARR 自动换 profile |
| 模型不可用/配额不足/成本超限 | MMR | 按 MMR 规则回退或返回其原始错误 |
| MMR 超时 | Harness | 按 MMR 客户端规则重试；不重新 Resolve 以规避配额 |
| Plan 已过期但调用已开始 | MMR/Harness | 由 MMR 接受策略决定；记录开始时间，禁止 ARR 修改历史 Plan |
| Provider 被紧急 suspend | ARR/MMR/Harness | 停止生成新 Plan；MMR 数据面按自身 kill switch 处理在途/新请求 |

ARR 不把 MMR 错误转换成自己的“模型选择错误”，以免丢失语义。Harness 应保留 `resource_plan_id` 和 `model_route_decision_id` 两层错误证据。

## 6. 兼容性与变更

| 变化 | ARR 是否变更 | 处理方式 |
|---|---|---|
| MMR 内部模型 A → B | 否 | MMR 自己发布和审计 |
| MMR 权重/价格/回退规则 | 否 | MMR 自己变更 |
| profile 增加可选 feature | 通常否 | Snapshot 新 minor，ARR 忽略未知可选字段 |
| profile 删除或破坏性语义变化 | 是 | 新 profile ID/contract major，并行迁移 Binding |
| MMR API 破坏性变化 | 是 | 新 contract major，Harness/MMR adapter 双版本运行 |
| endpoint/service discovery 变化 | 可能 | 更新 Provider `endpoint_ref`，不改 Capability |

MMR 必须至少提前一个发布窗口公告 profile 退役，并给出替代 profile 和 sunset 时间。

## 7. 联合契约测试

### 7.1 Provider 侧测试包

MMR CI 在每次 profile/API 变更时运行：

1. Snapshot JSON Schema 校验；
2. 禁止字段扫描；
3. digest 和签名验证；
4. profile capability 映射完整性；
5. 执行 API 接受 `resource_plan_id/traceparent`；
6. 成功、回退、配额、超时均返回 `model_route_decision_id`；
7. N-1 ARR adapter 兼容性。

### 7.2 ARR 侧测试

1. MMR 内部模型变化而 profile 不变时，Resource Plan 不变；
2. 未知可选字段不破坏 Snapshot ingest；
3. Snapshot 乱序、重复、篡改、过期均按规则处理；
4. profile 被 RETIRED 后不产生新 Plan；
5. ARR 不保存被注入的模型名、权重、密钥等禁止字段；
6. Semantic Router 内部 entrypoint/recipe 的候选模型、级联和 plugin 变化不改变 ARR Resource Plan；
7. 生产 Harness 只使用批准的 virtual entrypoint；未批准的物理模型直选被拒绝或隔离在管理/诊断路径；
8. MMR 故障不会拖垮 Resolve 线程池，因为运行调用不经过 ARR。

## 8. 上线切换

1. 盘点 MMR 当前 vLLM Semantic Router 版本、fork 差异、Envoy/ExtProc 拓扑、entrypoint/recipe、API、鉴权、错误码、trace 和发布流程。
2. 在 MMR 外围增加 `ProfileSnapshotPublisher` 薄适配器，不改核心路由算法。
3. 先把现有稳定模型入口映射为 1–3 个逻辑 profile，避免复制整个模型目录。
4. ARR 以 shadow 模式解析；Harness 比较 ARR profile 与旧静态配置。
5. 对单一低风险 Agent 开启 Resource Plan，但模型调用仍走原 MMR endpoint。
6. 达标后按 Agent/租户灰度，旧配置保留一个回滚窗口。
7. 冻结 Agent 直接配置新 MMR profile 的路径，统一从 Capability Binding 获取。

## 9. Phase 0 必答问题

- [ ] 当前 vLLM Semantic Router 的版本/tag/digest、企业 fork 和上游差异是什么？
- [ ] MMR 的 Envoy/ExtProc、控制面、模型 backend 和企业 Gateway 部署边界是什么？
- [ ] 当前逻辑 profile 如何映射到 Entrypoint/Recipe，是否仍存在业务方直配物理模型的路径？
- [ ] MMR API 使用 HTTP、gRPC 还是兼有？流式协议是什么？
- [ ] workload identity、租户和授权上下文如何传递？
- [ ] 当前 error taxonomy 和 retry contract 是什么？
- [ ] `model_route_decision_id` 是否已存在且全局唯一？
- [ ] profile 变更、健康和退役事件如何发布？
- [ ] 当前 OTel semantic attributes 和日志脱敏规范是什么？
- [ ] MMR 的 SLO、容量、灾备和发布窗口是什么？

这些答案缺失不妨碍 4A 评审，但不允许进入实现承诺和最终技术栈冻结。

## 10. 官方实现参考

- [vLLM Semantic Router repository](https://github.com/vllm-project/semantic-router)
- [Introduction: stable API and Mixture-of-Models](https://github.com/vllm-project/semantic-router/blob/main/website/docs/intro.md)
- [System Overview: Envoy, Router, Recipes and Provider Models](https://github.com/vllm-project/semantic-router/blob/main/website/docs/overview/semantic-router-overview.md)
- [vLLM Semantic Router releases](https://github.com/vllm-project/semantic-router/releases)
