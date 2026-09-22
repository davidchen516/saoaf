# SAOAF 契约仓库（I03）

机器可读契约的唯一权威来源。目录结构（对齐 development-plan §3）：

| 目录 | 内容 |
|---|---|
| `schemas/v1/` | JSON Schema 2020-12 公共信封：统一错误信封（唯一错误码路由）、分页、幂等、CloudEvents 事件信封 |
| `openapi/v1/` | OpenAPI 3.1 控制面 API 契约（I02 端点 + 公共约定；扩展命名空间 `x-saoaf-*`） |
| `events/v1/` | 事件载荷 schema（预留：I10 落地 outbox 事件时填充） |
| `protocols/model-profile/` | MMR 逻辑 profile 契约（原 mocks/mmr/openapi.yaml 单源化迁入；Mock 挂载指向此处） |
| `examples/` | golden examples（valid，必须通过 schema）与负向夹具（negative，必须以预期错误码失败） |
| `compatibility/` | N/N-1 兼容矩阵与钉版本消费者夹具 |

## 门禁

`go run ./tools/contractlint validate`（CI job：contract gate）：
1. 尺寸（>1 MiB）→ 深度（>64）→ JSON Schema 校验 → 禁止字段扫描 → OpenAPI 结构 lint；
2. 负向夹具断言唯一错误码（统一错误信封）；
3. 消费者夹具对当前版本验证（N/N-1）。

`go run ./tools/contractlint breaking <base-ref>`（CI：PR 对 base、main 对 HEAD~1）：
破坏性变更（删必填/收窄枚举/删属性/删操作/删文件）→ 红；加法变更放行。

## 版本规则

`DRAFT → PUBLISHED（兼容变更）｜ NEW-MAJOR（破坏性）`。已发布版本目录不可原地破坏性修改、
不可删除；破坏性变更在新 major 目录并行迁移（issue 回滚场景 7）。

## 禁止内容

契约与示例中不得出现：Prompt、模型响应、Tool 参数、Agent 消息、凭证、
MMR 内部路由字段（candidate models/cascade/backend health 等）——门禁强制扫描。
