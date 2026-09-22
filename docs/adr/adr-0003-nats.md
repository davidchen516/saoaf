---
adr: ADR-PHASE0-0003
title: Phase 0 冻结：NATS JetStream 2.14.7 事件传输
status: accepted
owner: David
decided: 2026-09-22
supersedes: -
classification: public
---

# ADR-PHASE0-0003：NATS JetStream 2.14.7 事件传输

## 决策

MVP 事件平台为 NATS JetStream 2.14.7：本地 Mock 单节点仅绑定 loopback；生产三节点、stream replicas=3。交付语义 at-least-once，CloudEvent `id` 去重，aggregate revision 拒绝乱序回退。Outbox 是数据库事务事实；NATS 不成为权威状态，也不保存敏感业务载荷。

## 依据

- 当前事件量与团队规模不需要 Kafka 分区级大吞吐及更重运维。
- 选择 2.14.7 而非刚发布的 2.15.0，降低 MVP 首轮升级风险；补丁升级需通过重复、乱序、leader 切换和恢复测试。

## 证据

- [mvp-baseline.md 第 6 节企业消息平台](../architecture/mvp-baseline.md)
- [Issue #1 已确认基线](https://github.com/davidchen516/saoaf/issues/1)
- [NATS Server releases](https://github.com/nats-io/nats-server/releases)

## 影响

- I10 的 EventTransport 端口以 NATS JetStream 为参考实现；企业已有 Kafka 托管标准或吞吐超 5,000 events/s 时做替换 ADR。
