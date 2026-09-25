# MCP Fabric mock (03.5)

Run `docker compose -f mocks/mcp/compose.yaml up` and call `http://127.0.0.1:4030`.

The contract exposes the Tool/MCP boundary semantics only: the ARR-facing
Tool Snapshot (`GET /control/v1/tool-snapshots/current`) and the
consumer-facing action lifecycle
(`POST /v1/tools/{actionId}/preview|approve|execute`,
`GET|DELETE /v1/tool-executions/{executionId}`). Execute requires the
gateway-identity headers (`X-Resource-Plan-ID`, `X-Resource-Plan-Item-ID`,
`X-Tenant-Ref`, `traceparent`, `X-Policy-Decision-Ref`) and supports
`Idempotency-Key`. Named error examples cover the contract-level negative
fixtures (HIGH-risk without approval reference → 403, idempotency-key
conflict → 409, parameter validation → 400, tool server transport
failure → 503, cancel race lost → 409).

This mock validates the SAOAF/MCP boundary. It does not implement an MCP
Gateway, Registry, Server, proxy, session, or any real Tool invocation —
those remain owned by the professional fabric (03.5 runtime, out of
contract-freeze scope). No deployable unit exists for 03.5: the
`deployable-scan` gate enforces that this stays contract-only.
