---
adr: ADR-PHASE0-0004
title: Phase 0 冻结：身份与授权使用 Phase 0 Mock，生产由 04 Identity & Trust 持有
status: accepted
owner: David
decided: 2026-09-22
supersedes: -
classification: public
---

# ADR-PHASE0-0004：身份与授权使用 Phase 0 Mock

## 决策

Phase 0 全部使用本地 Mock：Keycloak 26.7.4（OIDC issuer `http://127.0.0.1:8080/realms/saoaf-dev`、audience `saoaf-control-plane`、TOTP MFA）、AuthZEN PDP Mock（fail closed）、审批 API Mock（申请人不能自批）、短期 X.509 workload identity（SPIFFE URI SAN）。生产不自建 IAM/PDP，由 04 Identity & Trust 提供；Mock→生产切换由 Issue #22 验收，契约保持不变。

## 依据

- 03 工程不承担身份权威职责（模块边界已固定）。
- Mock 契约与生产适配器共享同一 OpenAPI 契约，切换只替换配置。

## 证据

- [mvp-baseline.md 第 4 节身份、MFA、Workload Identity、PDP 和审批](../../architecture/mvp-baseline.md)
- [authentication-authorization-mock.md](../../specs/authentication-authorization-mock.md)
- [mocks/identity](../../../mocks/identity)
- [Issue #1 已确认基线](https://github.com/davidchen516/saoaf/issues/1)

## 影响

- I05 按 Mock 契约实现适配器；生产接入证据由 I22 单独验收，不阻塞 Phase 0。
