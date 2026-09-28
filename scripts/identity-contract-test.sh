#!/bin/bash
# I22 identity contract end-to-end run: the REAL Keycloak 26 issuer + the
# AuthZEN/approval contract mocks, driving the production adapters
# (authn.Validator / authz.PDPClient / approval.Client) through the full
# chain: token issuance → signature/issuer/audience/expiry validation →
# scope gate → tenant-claim trust → PDP evaluation → approval gate →
# audit. Also proves JWKS key rotation: after the realm rotates its keys,
# a token signed by the OLD key stops validating (old-certificate
# revocation semantics for the signing keys).
#
# The Go harness (internal/platform/identity_e2e_test.go) does the work;
# this script only brings the stack up and sets SAOAF_E2E_IDENTITY=1.
# Run: SAOAF_TEST_PG_DSN unset is fine (no DB needed) — the identity
# stack alone is enough:
#   docker compose -f mocks/identity/compose.yaml up -d
#   KC_BOOTSTRAP_ADMIN_USERNAME=admin KC_BOOTSTRAP_ADMIN_PASSWORD=admin \
#     SAOAF_E2E_IDENTITY=1 go test -race -count=1 -run TestIdentityE2E ./internal/platform/
set -euo pipefail

cd "$(dirname "$0")/.."

export KC_BOOTSTRAP_ADMIN_USERNAME="${KC_BOOTSTRAP_ADMIN_USERNAME:-admin}"
export KC_BOOTSTRAP_ADMIN_PASSWORD="${KC_BOOTSTRAP_ADMIN_PASSWORD:-admin}"

docker compose -f mocks/identity/compose.yaml up -d

# readiness: Keycloak discovery + both Prism mocks
ISSUER="http://127.0.0.1:8080/realms/saoaf-dev"
for i in $(seq 1 60); do
  curl -sf "$ISSUER/.well-known/openid-configuration" >/dev/null 2>&1 \
    && curl -sf -X POST "http://127.0.0.1:4010/access/v1/evaluation" \
         -H 'Content-Type: application/json' \
         -d '{"subject":{"type":"identity","id":"probe"},"action":{"name":"probe"}}' >/dev/null 2>&1 \
    && break
  sleep 2
done
curl -sf "$ISSUER/.well-known/openid-configuration" >/dev/null || { echo "identity stack not ready"; exit 1; }

echo "identity stack ready:"
echo "  issuer     : $ISSUER"
echo "  authzen pdp: http://127.0.0.1:4010"
echo "  approvals  : http://127.0.0.1:4011"

# minting harness: a confidential client (created via the admin API by the
# Go test) with direct grants enabled lets the test obtain tokens
# programmatically — the realm's own clients are deliberately PKCE-only.
export SAOAF_E2E_IDENTITY=1
export SAOAF_E2E_ISSUER="$ISSUER"
export SAOAF_E2E_PDP_URL="http://127.0.0.1:4010"
export SAOAF_E2E_APPROVAL_URL="http://127.0.0.1:4011"
export KC_URL="http://127.0.0.1:8080"

go test -race -count=1 -v -run "TestIdentityE2E" ./internal/platform/
