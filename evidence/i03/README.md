# I03 契约仓库验证证据（2026-09-22）

## 绿运行

- `go run ./tools/contractlint validate` → **PASS**（schemas=4 / examples=4 / negatives=5 / consumers=2 / forbidden-hits=0）
  - 负向夹具断言唯一错误码：VALIDATION_UNKNOWN_FIELD / INVALID_ENUM / MISSING_REQUIRED / PAYLOAD_TOO_LARGE（1MiB+1 注入）/ PAYLOAD_TOO_DEEP（depth 70 > 64）
  - N/N-1：compatibility/v1 消费者夹具 ×2 对当前 schema 全过
  - 禁止字段扫描 0 命中（prompt/凭证/Tool 参数/MMR 内部路由字段）
  - OpenAPI 结构 lint：3.1 版本、Error 信封 $ref、4xx/5xx 引用检查
- `go run ./tools/contractlint breaking HEAD` → PASS（HEAD 含契约、工作树为加法变更；复审 P0 修复后重验）
- `go run ./tools/contractlint breaking HEAD~1` → PASS
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

## 复审修复记录（R1 → 修复提交）

- **P0**：G702 修复曾把 `ref:path` 整串送进 ref 校验导致 breaking 全断——已改为冒号拆分双段校验（ref 字符集 + 仓库相对路径字符集），并新增 `TestGitOutRefPathValidation` + `TestGitOutAcceptsRefPath`（scratch git 仓库全链路：无改动 PASS / 加法 PASS / required 删除 FAIL / enum 收窄 FAIL）回归测试。
- **P1**：误提交的 6.1MB `contractlint` 二进制已移除并入 .gitignore；OpenAPI 整 path 删除漏检已修（`$.paths` 后缀规则 + `TestFindBreakingWholePathRemoval`）。
- **P2**：too-deep 夹具改为真实 70 层嵌套输入，门禁实算 `jsonDepth`（不再信任自报值）；breaking 主链路测试覆盖补齐。
- **P3**：verify.sh 纳入 contract gate（validate + breaking）；`classifySchemaError` 未用参数删除；model-profile openapi.yaml 不在结构 lint glob（禁止扫描已覆盖，特此注明）。

## 已知限制

- breaking 检测为结构化规则集（required/enum/property/operation/整 path/文件删除/const 引入），非全量 OpenAPI diff 工具（如 oasdiff）；"新增 required 条目"（收紧方向）暂不在规则集内——首期契约演进由新 major 目录纪律约束，后续按需补规则；语义等价性由负向夹具与消费者夹具补强。
- 事件 payload schema（events/v1）留待 I10 落地 outbox 事件时填充（目录与信封契约已就位）。
