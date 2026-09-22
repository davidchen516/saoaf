# I02 工程骨架验证证据索引（v2，2026-09-22）

本地：macOS darwin/arm64 + Go 1.27.1（官方 tarball sha256 校验）+ Node 24/npm 11 + Docker 28；CI：GitHub Actions ubuntu-24.04。所有红运行探针（PR #27–#30）已取证后关闭并删除分支，仓库无残留。

## 绿运行

| 检查 | 结果 |
|---|---|
| `./scripts/verify.sh`（fmt/vet/race-test×4 包/boundary/license/build+双构建对比/web） | **ALL PASS** |
| PR #26 CI（8 必检 job，run 35712798418/35717962100） | **全 pass**（image+provenance 为 main-only，PR 正确 skip） |
| 进程探针真实运行 | 本地 + CI probes job：healthz/readyz 200、SIGTERM 优雅退出 exit 0 |
| 容器探针真实运行 | distroless 镜像 docker build ×2 + 运行 healthz 200 |
| **跨环境 digest 一致（GWT#1）** | 本地 darwin 交叉编译 linux/amd64 与 CI 原生构建**逐字节一致**：api `91a4d6a1…cb2d9`、worker `8dadf79c…4fb1c`（`-buildvcs=false` + `-trimpath` + ldflags 版本注入） |
| 同树双构建（GWT#3 机器内） | build.sh 内置 cmp 断言 + CI digest-reproducibility job 双构建对比 PASS；无关文件变更后 digest 不变（54bd6a7e 实测） |

## 四类红运行（全部 CI 流水线级证据 → `red-run-ci-probes.md`）

| # | 注入 | 门禁 | CI 证据 |
|---|---|---|---|
| ① 密钥泄露 | 随机 AWS 格式键 | gitleaks（SCA job） | PR #27 run 35706492628：`aws-access-token` 检出，exit 2 |
| ② 高危依赖 | x/net@v0.17.0 | govulncheck -scan module（SCA job） | PR #28 run 35713097546：GO-2026-5942 等 CVE 群检出 |
| ③ 未白名单依赖 | 白名单移除 chi | licensecheck（verify job） | PR #30 run 35713150687：`not on allowlist: go-chi/chi/v5` |
| ④ 失败单测 | TestInjectedFailure | go test -race（verify job） | PR #29 run 35713109388：`--- FAIL: TestInjectedFailure` |

**拒绝合并记录（GWT#5）**：PR #29 转 ready 后尝试合并 → `the base branch policy prohibits the merge`，mergeStateStatus **BLOCKED**（8 context strict 必检 + enforce_admins，API GET 确认）。

**_test.go 走私盲区（审查 P2-1）**：已修——`go list -f` 模板提取 TestImports/XTestImports 合并检查；注入 `registry→resolver` 测试导入 → FAIL exit 1（rsync 副本验证）。

## GWT 场景证据

- **#2 错误输入**：破坏锁定（go.sum 删除 / go.mod 条目移除）→ 构建失败且错误可定位（`no required module provides package… / import lookup disabled by -mod=readonly`，见 red-run-ci-probes.md 附录记录）。
- **#3 重复提交**：同 commit 两次 CI 构建 digest 一致（buildvcs=false 使 merge-ref 与 head digest 无关）；GWT#4 演练中的 rerun 复证。
- **#4 崩溃恢复**：run 35717962100 真实演练：in_progress → cancel → rerun → **completed/success**，无半合并（strict 必检保证）；PR #26 状态保持 CLEAN。
- **#5 权限拒绝**：如上 BLOCKED 记录。
- **#6 回滚**：I02 未合并前的回滚目标 = main@fce7dc2（pre-I02，phase0-baseline **success** 已验证可构建）；已发布制品/SBOM/证据保留（run 工件 + evidence/ 不删除）。合并后将打 `i02-scaffold-v0.1.0` tag 作为后续回滚锚点。

## 监控

`pipeline-monitor.md`：Actions API 导出的 30 条运行记录（含 4 次红运行注入的 failure 与恢复），持续视图 = GitHub Actions/Insights。

## 已知限制（如实披露）

- govulncheck 源码 symbol 模式在 Go 1.27 触发 x/vuln v1.1.4 上游 panic（SSA 路径）；采用 module 级（依赖存在即红）+ binary 级（制品符号可达性）双扫描替代，workflow 注释记录 revisit 条件。
- 签名/provenance：本仓可用形态 = digest-addressable 镜像推 GHCR（main-only job）+ SBOM digest 绑定记录 + run 工件；无 KMS/cosign 密钥，升级属 I21 安全供应链范围。
- npm SCA = `npm audit --audit-level=high`（lockfile 锁定解析）；web 组件级测试随 I16。
