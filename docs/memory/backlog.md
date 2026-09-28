# SAOAF backlog（非阻塞观察项）

审查轮次中裁定不阻塞、但值得后续跟进的观察项（OBS）。每条注明来源轮次与领域。

## reliability

- **WORM 归档 `TestArchiveConcurrentSingleLogicalPack` 竞态 flake**（2026-09-28，PR #58 R1 OBS）：
  CI 首轮出现 `pack_test.go:297: links = 0, want 8 (no double-archive)` 单项失败、重跑即绿；本
  diff 未触碰 evidencepack，main 连续 8 次全绿，本地 `-race -count=5` 复跑通过。与 I23 R4 的
  sweep/claim 双驱动窗口观察项同域（evidence/i23/README.md「sweep 重开双驱动窗口」行，
  TryDriveLock per-pack 锁已落地）。属并发时序敏感的偶发失稳，待复现样本更多时排查
  4-worker 会合点（links 断言处）的等待边界。
