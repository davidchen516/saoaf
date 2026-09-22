---
title: Phase 0 必答问题台账
version: 1.1.0
status: answered
owner: David
created: 2026-09-22
updated: 2026-09-22
classification: public
---

# Phase 0 必答问题台账（Issue #1）

状态机：`OPEN → ANSWERED（证据链接 + Owner + 日期）` 或 `OPEN → RISK-ACCEPTED（风险接受人 + 到期日）`。P0 问题不允许停留在中间态；台账由 `tools/phase0/baseline_check.py` 门禁强制校验。

## 台账

| ID | 优先级 | 主题 | 状态 | Owner | 证据 | 日期 |
|---|---|---|---|---|---|---|
| Q-LANG-01 | P0 | 控制面主语言 | ANSWERED | David | [adr-0001](../adr/adr-0001-language-go.md) | 2026-09-22 |
| Q-DB-01 | P0 | 权威数据库与 HA/备份 | ANSWERED | David | [adr-0002](../adr/adr-0002-postgres.md) | 2026-09-22 |
| Q-MSG-01 | P0 | 事件平台与交付语义 | ANSWERED | David | [adr-0003](../adr/adr-0003-nats.md) | 2026-09-22 |
| Q-IDN-01 | P0 | 身份/授权/审批 Phase 0 方案 | ANSWERED | David | [adr-0004](../adr/adr-0004-identity-mock.md) | 2026-09-22 |
| Q-NFR-01 | P0 | 容量/SLO/RTO/RPO 基线 | ANSWERED | David | [adr-0005](../adr/adr-0005-nfr-mmr-baseline.md) | 2026-09-22 |
| Q-EVI-01 | P0 | Evidence 保留期与 WORM | ANSWERED | David | [adr-0005](../adr/adr-0005-nfr-mmr-baseline.md) | 2026-09-22 |
| Q-MMR-01 | P0 | MMR 上游契约基线 | ANSWERED | David | [adr-0005](../adr/adr-0005-nfr-mmr-baseline.md) | 2026-09-22 |
| Q-LIC-01 | P0 | 公开仓库 LICENSE 决策 | ANSWERED | David | [license-decision](license-decision.md) | 2026-09-22 |
| Q-OWN-01 | P0 | 工程治理 Owner | ANSWERED | David | [open-decisions B 节](../plans/open-decisions.md) | 2026-09-22 |
| Q-CLS-01 | P0 | 文档 classification | ANSWERED | David | [docs/README.md 发布分级](../README.md) | 2026-09-22 |
| Q-MMR-02 | P0 | 真实 MMR 镜像 digest、fork 差异、endpoint | RISK-ACCEPTED | David | [open-decisions OPEN-02](../plans/open-decisions.md) | 2026-10-31 |
| Q-MMR-03 | P1 | Envoy/ExtProc 拓扑与 backend pool | RISK-ACCEPTED | David | [open-decisions OPEN-02](../plans/open-decisions.md) | 2026-10-31 |
| Q-CAP-01 | P1 | 首批 10–20 个 Capability 清单 | RISK-ACCEPTED | David | [open-decisions OPEN-03](../plans/open-decisions.md) | 2026-10-31 |
| Q-FIX-01 | P1 | MMR Profile Snapshot 夹具可重放 | ANSWERED | David | [fixtures/mmr](../../fixtures/mmr) | 2026-09-22 |
| Q-TRC-01 | P1 | 一次含身份/entrypoint/决策 ID 的运行调用记录 | ANSWERED | David | [fixtures/mmr/recorded-calls.json](../../fixtures/mmr/recorded-calls.json) | 2026-09-22 |

## 风险接受说明（RISK-ACCEPTED 项）

- **Q-MMR-02 / Q-MMR-03（OPEN-02）**：真实 MMR 的镜像 digest、fork 差异、endpoint、Envoy/ExtProc 拓扑尚未核验。接受理由：Phase 0 契约已按 v0.3.0 公开能力冻结，Mock 与 ARR 开发不受阻；仅真实联调（I11/I21 相关部分）被阻塞。到期日 2026-10-31，逾期未关闭须升级工程委员会（Owner: David）重新裁决。
- **Q-CAP-01（OPEN-03）**：首批 Capability 业务清单待领域 Owner 确认，阻塞 I07 首批数据与业务验收，不阻塞骨架与契约。到期日 2026-10-31。

### 风险接受确认记录

- 2026-09-22：David（工程总负责人 / Owner）在 Issue #1 实现证据评审中**确认上述三条风险接受生效**（Q-MMR-02、Q-MMR-03、Q-CAP-01），到期日维持 2026-10-31。真实 MMR 调用 / Trace / SLO 证据并入 I11 联调范围。决策同步记录于 Issue #1 评论与 PR #24。

## 冲突升级路径

同一问题出现两个来源冲突的回答时：登记冲突、升级工程委员会（Owner: David）裁决，不得静默覆盖任一方（Issue #1 验收逻辑场景 3）。
