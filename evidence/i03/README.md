# I03 契约仓库验证证据（2026-09-22）

## 绿运行

- `go run ./tools/contractlint validate` → **PASS**（schemas=4 / examples=4 / negatives=5 / consumers=2 / forbidden-hits=0）
  - 负向夹具断言唯一错误码：VALIDATION_UNKNOWN_FIELD / INVALID_ENUM / MISSING_REQUIRED / PAYLOAD_TOO_LARGE（1MiB+1 注入）/ PAYLOAD_TOO_DEEP（depth 70 > 64）
  - N/N-1：compatibility/v1 消费者夹具 ×2 对当前 schema 全过
  - 禁止字段扫描 0 命中（prompt/凭证/Tool 参数/MMR 内部路由字段）
  - OpenAPI 结构 lint：3.1 版本、Error 信封 $ref、4xx/5xx 引用检查
- `go run ./tools/contractlint breaking HEAD` → PASS（首发布：新增=加法）
- `./scripts/verify.sh` → ALL PASS；`go test -race ./tools/contractlint` 全绿（深度/分类器/映射/禁止扫描/夹具）
- MMR 契约单源化迁移后 `replay_mmr_fixtures.py` → PASS（mock compose 挂载 ../../contracts/…）

## 红运行①②（CI 级，探针 PR 取证后回填链接）

① breaking change（enum 收窄 / required 删除 / property 删除）→ contract-gate job 红
② 示例注入禁止字段（prompt）→ validate 禁止扫描红
③ 超深/超大 → 负向夹具在 contract-gate job 中持续断言（每个 PR 的 CI 日志即记录）

## 红运行①的本地三形态记录（修复标量数组 set-diff bug 后）

```
enum 收窄: FAIL - contracts/schemas/v1/error-envelope.json: enum value "CONFLICT" removed at $.properties.error_code.enum (exit 1)
required 删除: FAIL - required entry "request_id" removed at $.required (exit 1)
property 删除: FAIL - pagination.json: property "next_offset" removed at $.properties.page.properties (exit 1)
恢复后: PASS (changed=0 deleted=0)
```

## 已知限制

- breaking 检测为结构化规则集（required/enum/property/operation/文件删除/const 引入），非全量 OpenAPI diff 工具（如 oasdiff）；语义等价性由负向夹具与消费者夹具补强。
- 事件 payload schema（events/v1）留待 I10 落地 outbox 事件时填充（目录与信封契约已就位）。
