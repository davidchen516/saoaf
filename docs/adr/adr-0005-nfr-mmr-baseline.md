---
adr: ADR-PHASE0-0005
title: Phase 0 冻结：容量/SLO/保留期 NFR 与 MMR 契约基线
status: accepted
owner: David
decided: 2026-09-22
supersedes: -
classification: public
---

# ADR-PHASE0-0005：容量/SLO/保留期 NFR 与 MMR 契约基线

## 决策一：容量与 SLO

峰值 100 Resolve QPS（持续 15 分钟）、P50 ≤ 25 ms / P95 ≤ 60 ms / P99 ≤ 100 ms、99.9% 月可用性；部署为 1 个主权地域、3 个故障域、≥2 应用副本；RTO ≤ 60 分钟、数据库 RPO ≤ 5 分钟；单实例逻辑多租户 ≤ 10 租户，`tenant_ref` 只能来自已验证 Token/受信网关。

## 决策二：Evidence 保留与 WORM

在线明细按数据类型保留（Plan/Item 90 天、Decision 1 年、高风险变更 2 年）；每日 Evidence Pack 1 年、高风险 2 年，S3 Object Lock COMPLIANCE、主权地域内保存。生产对象存储必须通过 WORM 准入测试（Mock S3 或普通 versioning 不作为证据）。

## 决策三：MMR 契约基线

MMR 以 vLLM Semantic Router 为决策核心（Apache-2.0）。Phase 0 契约参考其 v0.3.0 公开能力：上游 tag `v0.3.0`（commit `ee6dd0d1486450c174b0554b0f22e2e7d615cbc2`，发布于 2026-06-05）。`mocks/mmr` 按稳定 profile/decision 契约模拟。真实部署的镜像 digest、fork 差异与 endpoint 尚未核验，作为 OPEN-02 跟踪——不阻塞 Mock 与 ARR 开发，阻塞真实联调。

## 依据

- 项目发起人已按 [open-decisions.md](../plans/open-decisions.md) B 节关闭容量/数据库/消息/Evidence 基线。
- [mvp-baseline.md](../../architecture/mvp-baseline.md)（approved-for-mock）。
- vLLM Semantic Router v0.3.0 release 与仓库均为公开可核查。

## 证据

- [mvp-baseline.md 第 2、7、8 节](../../architecture/mvp-baseline.md)
- [vLLM Semantic Router releases](https://github.com/vllm-project/semantic-router/releases)
- [open-decisions.md OPEN-02](../plans/open-decisions.md)
- [Issue #1 已确认基线](https://github.com/davidchen516/saoaf/issues/1)

## 影响

- I09 的负载测试以此 NFR 为批准阈值；I23 承接 WORM 生产准入。
- 真实 MMR digest 核验完成前，联调证据只能引用 Mock；OPEN-02 关闭后须在台账补记。
