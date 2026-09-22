---
title: 03 Sovereign AI & Open AI Fabric 架构交付包索引
version: 1.2.0
status: proposed
owner: TODO-AI平台负责人
created: 2026-09-21
updated: 2026-09-21
classification: public
---

# 03 Sovereign AI & Open AI Fabric 架构交付包

建议按以下顺序评审：

1. [03 工程总体架构与模块边界](./programs/03-sovereign-ai-open-ai-fabric/architecture.md)
2. [03 工程模块交互与契约](./programs/03-sovereign-ai-open-ai-fabric/module-contracts.md)
3. [03 工程 2027 开发实施计划](./programs/03-sovereign-ai-open-ai-fabric/development-plan.md)
4. [03 工程架构一致性审计](./programs/03-sovereign-ai-open-ai-fabric/architecture-audit.md)
5. [03 工程技术栈设计](./architecture/technology-stack.md)
6. [AI Resource Router 子项目 4A 架构](./architecture/ai-routing-platform-4a.md)
7. [AI Resource Router 边界与资源能力](./architecture/ai-resource-router.md)
8. [ARR Runtime/Admin API](./specs/resource-resolver-api.md)
9. [ARR 数据与事件设计](./specs/resource-resolver-events-and-data.md)
10. [ARR 与 MMR 集成](./specs/multi-model-router-integration.md)
11. [ARR 实施计划](./plans/resource-resolver-implementation-plan.md)
12. [ARR 部署与运行手册](./runbooks/resource-resolver-operations.md)
13. [ARR 架构一致性审计](./architecture/ai-routing-platform-audit-report.md)
14. [身份认证、登录与授权 Mock](./specs/authentication-authorization-mock.md)
15. [当前未决项清单](./plans/open-decisions.md)
16. [Phase 0 最小 MVP 基线](./architecture/mvp-baseline.md)

当前状态为 `proposed`。完成 Phase 0 的 MMR 盘点、容量基线、IAM 与保留期确认后，主文档可升级为 `approved`，接口文档可进入实现基线。

## 发布分级

本目录中的架构、契约、计划和运行手册按 `public` 分级，可在公开仓库中审阅。文档中的租户、服务、域名、ID、组织和数据均为 Mock 示例，不代表真实生产环境。

仓库尚未加入开源许可证，因此公开可见不自动授予复制、修改或分发权。每次发布仍须检查凭证、内部地址、人员信息、供应商合同、未公开漏洞和受限业务数据；后续采用开源许可证需由项目 Owner 单独批准。

## 对外开源主页

[GitHub Pages homepage](../gh-pages/README.md) 提供面向开源社区的中英双语项目介绍、架构、接入步骤、标准、治理和贡献指引。发布工作流位于 `../.github/workflows/deploy-pages.yml`。
