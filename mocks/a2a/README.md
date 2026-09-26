# A2A Fabric mock (03.6)

Run `docker compose -f mocks/a2a/compose.yaml up` and call `http://127.0.0.1:4040`.

The contract exposes the agent/delegation boundary semantics only: the
ARR-facing Agent Snapshot (`GET /control/v1/agent-snapshots/current`) and
the consumer-facing task lifecycle (`POST /v1/agent-tasks`,
`GET|DELETE /v1/agent-tasks/{taskId}`). Submit requires the 04-issued
delegation reference + scope and supports `Idempotency-Key` (same key →
same at- task). Named error examples cover the contract-level negative
fixtures (delegation scope exceeded → 403, unknown/malicious agent → 404,
task not found → 404, cancel race lost → 409, runtime transport
failure/timeout → 503).

**Semantics caveat (disclosed since I18 review):** Prism is a stateless
contract mock. The state-level negative responses are CONTRACT EXAMPLES
selected via `Prefer: code=…`; the mock does not statefully REJECT an
out-of-delegation-scope submit (a plain submit returns the 200 example).
Request-side validation (required headers, path patterns, body shapes)
IS enforced by Prism. Stateful enforcement — delegation subset
arbitration, cancel/status race resolution, trust-domain validation — is
03.6 runtime / 04 Identity responsibility, out of contract-freeze scope
(issue non-goals: no Gateway, remote agent, orchestration, credential
issuance).

This mock validates the SAOAF/agent boundary. It does not implement an
A2A Gateway, remote agent runtime, orchestration, or delegation credential
signing — those remain owned by the professional fabric. No deployable
unit exists for 03.6: the `deployable-scan` gate enforces that this stays
contract-only.
