# Phase 0 Issue #1 验证证据索引

所有证据于 2026-09-22 在本机（macOS, Docker 28.0.1, Python 3.13.2）采集，命令与结果原样保存。

## 门禁红运行（故障注入：无效输入被拒绝）

| 文件 | 场景 | 结果 |
|---|---|---|
| `red-run-gate-rejects-invalid-ledger.txt` | 台账含中间态（OPEN/IN-PROGRESS）、缺 Owner、缺证据链接 | FAIL，exit=1 |
| `red-run-gate-rejects-invalid-fixture.txt` | Snapshot 夹具 digest 非 sha256 前缀 | FAIL，exit=1 |
| `red-run-unit-fixture-validation.txt` | 调用记录缺 model_route_decision_id、缺 4 种结局 | FAIL（fixture 校验单元级捕获） |
| `red-run-gate-rejects-missing-decision-id.txt` | 全量门禁在无效夹具下的完整红运行 | FAIL，exit=1 |

## 权限拒绝证据（验收逻辑场景 5）

| 文件 | 场景 | 结果 |
|---|---|---|
| `scenario5-branch-protection-rejects-direct-push.txt` | 管理员直接 push main（空提交探针）被 GH006 拒绝：必须走 PR + 2/2 status checks | 拒绝成功，main 未变 |

## 绿运行（正式基线通过）

| 文件 | 场景 | 结果 |
|---|---|---|
| `green-run-fixture-replay.txt` | 真实 E2E：docker compose 启动 Prism 5.15.10 Mock，重放 5 个结局（SUCCEEDED / PROFILE_NOT_FOUND / QUOTA_EXCEEDED / MODEL_UNAVAILABLE / TIMEOUT-class），每种结局均断言 `resource_plan_id` 与 `model_route_decision_id` 双关联、错误码正确、Snapshot 夹具与活体 Mock 交叉核对 | PASS，exit=0 |

## 可重复执行

```bash
python3 tools/phase0/baseline_check.py       # 台账/ADR/夹具/LICENSE 门禁
python3 tools/phase0/replay_mmr_fixtures.py  # 真实 E2E 重放（需要 Docker）
```

CI 在 `.github/workflows/phase0-baseline.yml` 中以相同命令执行（Mock E2E 通过 docker compose 起在 CI runner 上）。

## 已知限制

- Prism 为静态示例 Mock：返回 example 中的固定关联 ID，不回显请求头中的 plan ID。因此重放断言"双 ID 存在且互异"，不断言"回显一致"——回显一致是真实 MMR 集成检查，被 OPEN-02（Q-MMR-02 风险接受，到期 2026-10-31）阻塞。
- TIMEOUT 结局以 503 transport 失败类别表达（Prism 不模拟超时）；真实超时语义留待 I11 联调。
- Snapshot 夹具的 `signature` 为 `mock-only-not-cryptographic`，符合 Mock 契约；真实签名验证属 I07/I11 范围。
