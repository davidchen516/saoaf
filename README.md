# Sovereign AI & Open AI Fabric (SAOAF)

SAOAF is an open reference architecture for enterprises that need to choose, connect, govern, replace, and prove control over AI capabilities across models, tools, data, agents, and infrastructure.

SAOAF 是面向企业主权 AI 的开放参考架构，用于统一管理模型、工具、数据、Agent 与算力资源的选择、连接、治理、替换和审计证明。

## Architecture / 架构

The program is divided into eight bounded modules:

| Module | Responsibility |
| --- | --- |
| 03.1 Sovereignty Governance & Policy Plane | Defines sovereignty constraints and governance policy |
| 03.2 Capability Registry & AI Resource Hub | Maintains stable capability identities, metadata, ownership, bindings, and lifecycle |
| 03.3 AI Resource Router | Resolves capability requirements into deterministic, short-lived Resource Plans |
| 03.4 Model Fabric / Multi-Model Router | Selects and invokes concrete models; manages quota, cost, retries, and fallback |
| 03.5 Tool & Data Access Fabric / MCP | Provides governed access to enterprise tools and data services |
| 03.6 Agent Federation Fabric / A2A | Supports agent discovery, delegation, and cross-agent work |
| 03.7 Placement & Portability Fabric | Converts placement intent into portable deployment profiles |
| 03.8 Sovereignty Operations & Exit Assurance | Links evidence, measures portability, and validates exit paths |

Current delivery scope: 03.3 and 03.8 are implemented in this project, with the minimum 03.1/03.2 control-plane capabilities they require. 03.4 is an existing system and is integrated through its published contracts. 03.5, 03.6, and 03.7 are contract-only in this phase; their runtime implementations are deferred.

The control plane carries metadata, constraints, plans, and evidence references. Prompts, tool parameters, model responses, and other business payloads travel directly through the specialist execution fabrics.

AI Resource Router and Multi-Model Router are sibling modules. The Resource Router selects a logical provider and contract profile; the Multi-Model Router selects and invokes a concrete model endpoint.

## Documentation / 文档

Start with the [architecture delivery package](docs/README.md). The main documents cover:

- overall architecture, module boundaries, and interaction contracts;
- 4A architecture for the AI Resource Router;
- Runtime and Admin APIs, data model, and lifecycle events;
- integration contract with the existing Multi-Model Router;
- implementation plan, operations runbook, and architecture audits.

## Website / 网站

The bilingual project homepage is published through GitHub Pages from [`gh-pages/`](gh-pages/). The deployment workflow runs automatically when homepage files change on `main`.

## Project status / 项目状态

The architecture is currently **proposed / pre-release**. This delivery implements AI Resource Router and Sovereignty Operations & Exit Assurance, integrates the existing Multi-Model Router, and freezes interfaces for MCP, A2A, and Placement without implementing those three execution fabrics.

The repository is currently maintained as closed-source design material. Public repository visibility does not grant reuse, modification, or redistribution rights; access and publication policy must be approved separately.

Please use [GitHub Issues](https://github.com/davidchen516/saoaf/issues) for scoped architecture proposals, contract questions, and implementation feedback.
