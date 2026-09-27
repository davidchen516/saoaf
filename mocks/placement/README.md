# Placement Fabric mock (03.7)

Run `docker compose -f mocks/placement/compose.yaml up` and call `http://127.0.0.1:4050`.

The contract exposes the placement boundary semantics only: the example
zone list (`GET /control/v1/zones`), pure profile validation
(`POST /v1/profiles/validate?zone=…` — the same profile bytes yield the
same digest and verdict in every zone), and the ASYNC control-plane plan
lifecycle (`POST /v1/placement-plans` with Idempotency-Key,
`GET /v1/placement-plans/{planId}`). Named error examples cover the
four issue negative classes (insufficient capacity → 409, no compliant
zone → 403, artifact incompatible → 400, failback failed → 503) plus
the secret-reference denial (403).

**Semantics caveat (disclosed since I18):** Prism is a stateless
contract mock. The negative responses are CONTRACT EXAMPLES selected
via `Prefer: code=…`; the mock does not statefully evaluate zones.
Request-side validation (required headers, path patterns, body shapes
including the reference-only secrets structure) IS enforced by Prism.
Zone evaluation, policy checks and deployments belong to the 03.7
runtime / AI Factory — out of contract-freeze scope (issue non-goals:
no Kubernetes/GPU/cross-cluster scheduling, cloud resource creation or
Placement Controller). The `sync-infra-scan` gate guarantees the
contract corpus never grows a synchronous infrastructure-creation
interface.

Prism returns the EXAMPLE body for validation responses: the
`profile_digest` equality assertion in the consumer test therefore
cannot FAIL against this mock (an example-selection echo, not a
server-side computation). The same-bytes/digest semantics live in the
CONTRACT (request/response schemas + the profile_digest pattern); the
runtime-zone equality is 03.7 runtime responsibility.

This mock validates the SAOAF/placement boundary. It does not schedule
anything. No deployable unit exists for 03.7: the `deployable-scan`
gate enforces that this stays contract-only.
