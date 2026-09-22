---
adr: ADR-PHASE0-0002
title: Phase 0 冻结：PostgreSQL 18.6 + CloudNativePG 唯一权威持久化
status: accepted
owner: David
decided: 2026-09-22
supersedes: -
classification: public
---

# ADR-PHASE0-0002：PostgreSQL 18.6 + CloudNativePG 1.30.0 唯一权威持久化

## 决策

Registry、Resolver、Ops 的唯一权威状态库为 PostgreSQL 18.6，由 CloudNativePG 1.30.0 管理：三实例跨三个故障域、一主两副本、异步流复制、自动 failover；WAL 连续归档独立 S3-compatible bucket（RPO ≤ 5 分钟），每日 base backup 保留 35 天。缓存、搜索与消息平台均不成为权威来源。

## 依据

- 契约/状态/审计一体的控制面数据模型适合关系模型 + 强事务。
- 单地域三故障域 HA 与 RTO ≤ 60 分钟 / RPO ≤ 5 分钟的 MVP 目标可由该组合满足。
- 单容器本地开发不用于证明 HA、PITR 或 RTO。

## 证据

- [mvp-baseline.md 第 5 节 PostgreSQL、HA 与备份](../../architecture/mvp-baseline.md)
- [Issue #1 已确认基线](https://github.com/davidchen516/saoaf/issues/1)
- [PostgreSQL 18 backup and recovery](https://www.postgresql.org/docs/18/backup.html)
- [CloudNativePG high availability](https://cloudnative-pg.io/info/high-availability/)

## 影响

- I04 按 expand/migrate/contract 建立 Schema 与迁移基础；生产收缩需单独窗口。
- 后续数据库版本/拓扑变更走新 ADR。
