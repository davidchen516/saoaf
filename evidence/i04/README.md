# I04 持久化基础验证证据（2026-09-22）

环境：真实 PostgreSQL 18.6（docker postgres:18.6 镜像，Debian 18.6-1.pgdg13+2），goose CLI v3.28.0（pinned），pgx v5.11.0。迁移套件 `go test -race -count=1 -v ./migrations`（scripts/pgtest.sh 编排，每测试独立 CREATE DATABASE）。

## 迁移套件 9/9 全绿（`migration-suite-results.txt`）

| # | 测试 | GWT 对应 | 断言要点 |
|---|---|---|---|
| 1 | TestApplyFromEmptyDatabase | GWT#1 空库路径 | goose up → version=2；change_record+outbox 原子写入 |
| 2 | TestUpgradePathFromPreviousRelease | GWT#1 升级路径 + GWT#7 | up-to 1 → v1 应用正常 → up → v2 后旧形态写入兼容（published_seq 对旧行全 NULL） |
| 3 | TestMigrationIdempotentRerun | GWT#3 | 二次 up 无重复版本行 |
| 4 | TestConcurrentRunnersConverge | GWT#4 | advisory lock 781927001：第二 runner 必然拿不到 |
| 5 | TestInterruptedMigrationRecoversSafely | GWT#5 | **确定性中断**：ACCESS EXCLUSIVE 锁住目标表使 v2 ALTER 阻塞 → kill 迁移进程 → 无半变更（version 仍 1、无 published_seq 列）→ 解锁重试 → v2 完整收敛 |
| 6 | TestAppRoleCannotRunMigrations | GWT#6 | 无 DDL 角色跑 goose → permission denied；迁移后 saoaf_app DML 正常、CREATE TABLE 被拒 |
| 7 | TestConstraintEnforcement | 数据不变量负向 | 唯一(23505)/CHECK(23514)/FK(23503) 全部数据库层强制 |
| 8 | TestTransactionalAtomicityNoHalfCommit | 半提交=0 | 显式回滚与连接中途死亡（pgx Close 回滚未决事务）两形态：两表计数均 0 |
| 9 | TestPoolConcurrentWritesInvariant | 连接池压测+并发约束 | 20 goroutine/5 连接池：无半提交（计数=20/20）；20 并发重放同 event_id → 恰 1 成功 19 拒绝(23505) |

## PITR 演练（`pitr-drill-run.txt`，scripts/pitr-drill.sh 原样输出）

WAL 归档（archive_mode=on）→ T0 基线+basebackup → T1 WAL-only 提交（含 published_seq=1 水位）→ pg_switch_wal → **DROP TABLE 灾难** → 灾难段强制归档 → base backup + recovery.signal + recovery_target_time=PITR 目标 → promote → 核对清单：

```
schema version : 2 (want 2)
change_record   : 2 rows (want 2 — T0+T1, disaster excluded)
outbox_event    : 2 rows (want 2)
outbox watermark: 1 (want 1 — evt-t1 published_seq)
entities intact : 2 (want 2)
constraint probe: ERROR: new row ... violates check constraint "outbox_event_status_check"
PITR DRILL: PASS
```

active pointer 与 Evidence digest 的核对项随 I07/I12 的业务表落地后并入本清单（本期无该对象，见 scripts/pitr-drill.sh 注释）；API smoke test 以恢复库上的 SQL 探针代替（healthz 探针在 I02 已有独立门禁）。

## 已知限制

- 三实例/CloudNativePG 生产拓扑与跨故障域 HA 属生产准入（I21/I22 范围），本期以单容器验证行为语义；RTO/RPO 达标测量在 I21。
- expand/migrate/contract 三阶段的中断注入：本期有 expand（ALTER 加列）中断证据；migrate（数据回填）与 contract（收缩）阶段尚未有对应迁移（I06+ 产生后按需补）。
- 监控（迁移进度/锁等待/schema drift 告警）：CI 迁移门禁执行历史承担（GitHub Actions 记录），生产监控告警随 I21。
