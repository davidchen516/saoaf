# I02 工程骨架验证证据索引

日期：2026-09-22。本地环境：macOS darwin/arm64，Go 1.27.1（官方 tarball，sha256 校验通过），Node 24.13.0 / npm 11.6.2。

## 绿运行（正式基线）

| 检查 | 命令 | 结果 |
|---|---|---|
| 格式/静态 | `gofmt -l`（空）+ `go vet ./...` | PASS |
| 单元测试（race） | `go test -race -count=1 ./...` | 4 包全 ok |
| 依赖边界 | `go run ./tools/boundarycheck` | PASS（13 packages） |
| License 策略 | `go run ./tools/licensecheck` | PASS（chi v5 唯一第三方依赖，MIT，白名单内） |
| 构建+可复现性 | `./scripts/build.sh`（双构建 digest 逐一 cmp） | PASS：`control-plane-api-darwin-arm64` = `4e4fb120…f8be4`，`control-plane-worker-darwin-arm64` = `7667335f…5bb4b`，两次构建逐字节一致 |
| Web 构建（TS strict） | `npm run build`（tsc --noEmit + vite） | PASS |
| 进程探针（真实运行） | 起 API(:18080)/Worker(:18081) → curl healthz/readyz → SIGTERM | 全 200；`{"status":"alive"}`/`{"status":"ready"}`；两进程 exit 0 优雅退出；JSON 日志含 listening/stopped 事件 |

全套合一入口：`./scripts/verify.sh` → `VERIFY: ALL PASS`。

## 红运行注入（门禁有效性证明，四选四）

在 /tmp/i02_red 完整副本上逐项注入（未触碰仓库本体）：

| # | 注入 | 门禁 | 结果 |
|---|---|---|---|
| 1 | 失败单测 `TestInjectedFailure` | `go test -race` | `--- FAIL: TestInjectedFailure` → exit 非零；移除后恢复 PASS |
| 2 | 反向模块依赖 `registry → resolver` | `boundarycheck` | `FAIL: internal:registry may not import internal:resolver` exit 1；移除后 PASS |
| 3 | 未白名单依赖 `github.com/google/uuid`（真实 go get 解析） | `licensecheck` | `FAIL: go module not on allowlist: github.com/google/uuid` exit 1；tidy 移除后 PASS |
| 4 | 密钥泄露（AWS AKIA…特征文件） | gitleaks-action（CI） | 见 red-run-4-secret-scan.txt |

## CI 证据

- PR 绿运行、同 commit 重跑 digest 一致性、SBOM 产物：合并后回填（见 Issue #2 关闭评论）。

## 已知限制

- gitleaks 红运行为 CI 侧证据（gitleaks-action 扫全历史）；本地无 gitleaks 二进制时以规则特征等效说明，CI 上以真实运行为准。
- 镜像签名/provenance（cosign）在 main 推送阶段执行；PR 阶段产出 SBOM + digest 工件（I02 验收要求"可按 digest 识别并附带 SBOM"）。
- web 仅有 tsc+vite 构建门禁；组件测试随 I16 引入（本 Issue 范围外）。
