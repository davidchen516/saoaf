---
title: 03 Sovereign AI & Open AI Fabric 工程索引
version: 1.0.0
status: proposed
owner: TODO-03工程负责人
reviewers:
  - TODO-企业AI架构委员会
  - TODO-AI平台负责人
  - TODO-安全与IAM负责人
  - TODO-基础设施负责人
created: 2026-09-21
updated: 2026-09-21
classification: internal
---

# 03 Sovereign AI & Open AI Fabric

本工程是制造企业 2027 AI 技术架构八大战略工程中的第 03 工程，目标是让企业在模型、工具、Agent、Context、算力和供应商变化时，持续拥有选择权、控制权、迁移能力和运营连续性。

交付文档：

1. [总体架构与模块边界](./architecture.md)
2. [模块交互与契约设计](./module-contracts.md)
3. [2027 开发实施计划](./development-plan.md)
4. [AI Resource Router 子项目架构](../../architecture/ai-routing-platform-4a.md)
5. [AI Resource Router 与 Multi-Model Router 集成](../../specs/multi-model-router-integration.md)
6. [架构一致性审计](./architecture-audit.md)

工程内一级模块：

| 编号 | 模块 | 主要主权 |
|---|---|---|
| 03.1 | Sovereignty Governance & Policy Plane | Operation、Data、Context、Learning |
| 03.2 | Capability Registry & AI Resource Hub | Operation、Agent、Context |
| 03.3 | AI Resource Router | 跨域资源选择权 |
| 03.4 | Model Fabric / Multi-Model Router | Model |
| 03.5 | Tool & Data Access Fabric / MCP | Data、Tool、Context 接入 |
| 03.6 | Agent Federation Fabric / A2A | Agent |
| 03.7 | Placement & Portability Fabric | Compute、Cloud/Private/Edge |
| 03.8 | Sovereignty Operations & Exit Assurance | Operation、Learning、供应商退出 |
