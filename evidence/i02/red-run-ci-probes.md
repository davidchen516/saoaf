# I02 红运行 CI 证据（探针 PR，全部已关闭、分支已删）

> 四类红运行（Issue 要求"缺一不可"）现全部有 CI 流水线级记录 + 拒绝合并记录（GWT#5）。
> 密钥探针见 red-run-4-secret-scan.txt（PR #27，gitleaks aws-access-token）。

## ② 高危依赖 → SCA 红（PR #28，run 35713097546）

注入：`golang.org/x/net@v0.17.0`（blank import）。结果：SCA job **fail**（31s）。
归因（run 日志原文）：
```
=== Module Results ===
Vulnerability #1: GO-2026-5942
    Parsing an invalid SVCB or HTTPS RR can panic in golang.org/x/net/dns/dnsmessage
  Module: golang.org/x/net
    Found in: golang.org/x/net@v0.17.0
    Fixed in: golang.org/x/net@v0.56.0
Vulnerability #2: GO-2026-5030 (XSS in x/net/html) …
```
注：探针同时使 build job 红（go.sum 更新导致 reproducibility 断言变化）——两 job 独立红，SCA 归因如上。

## ④ 失败单测 → 合并阻断（PR #29，run 35713109388）

注入：`TestInjectedFailure`（t.Fatal）。结果：verify job **fail**（50s）。
归因：`--- FAIL: TestInjectedFailure (0.00s)`。
**拒绝合并记录（GWT#5）**：PR 转 ready 后尝试合并：
```
X Pull request davidchen516/saoaf#29 is not mergeable: the base branch policy prohibits the merge.
mergeStateStatus: BLOCKED
```

## ③ 未白名单依赖 → license 红（PR #30，run 35713150687）

注入：从 allow-go.txt 移除 chi v5（实际依赖仍在 go.mod）。结果：verify job **fail**（51s）。
归因：`LICENSE CHECK: FAIL - go module not on allowlist: github.com/go-chi/chi/v5`。

## 必检清单（P1-1 修复，API GET 确认）

baseline-gate / mmr-fixture-replay / build·test·boundary·license / web build+npm audit /
probes / SAST gosec / SCA·secret·SBOM / digest reproducibility —— 8 context 全部 strict 必检，
enforce_admins=true。任一红 → BLOCKED（如上实证）。
