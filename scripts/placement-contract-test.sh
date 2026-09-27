#!/bin/bash
# I20 placement contract consumer run: dual-zone same-digest validation,
# async plan lifecycle and every contract-level negative. Run:
# ./scripts/placement-contract-test.sh (mock on :4050).
set -u
B="${PLACEMENT_MOCK_URL:-http://127.0.0.1:4050}"
PASS=0; FAIL=0
check() { if [ "$2" = "$3" ]; then PASS=$((PASS+1)); echo "PASS $1"; else FAIL=$((FAIL+1)); echo "FAIL $1: got [$2] want [$3]"; fi }
TP="00-00000000000000000000000000000000-0000000000000000-01"
DIG='sha256:1111111111111111111111111111111111111111111111111111111111111111'
PROFILE=$(cat <<'EOF'
{
  "profile_id": "profile.reasoning.serve",
  "profile_version": "v1",
  "artifact": {"oci_digest": "sha256:0000000000000000000000000000000000000000000000000000000000000000", "architecture": "amd64", "accelerator_class": "gpu-a100-80g"},
  "runtime_contract_ref": "schema://runtime/serve-v1",
  "configuration_schema_ref": "schema://configuration/serve-v1",
  "secrets": [{"name": "model-weights-key", "ref": "secret://tenant-a/model-weights"}],
  "data_dependencies": [{"name": "weights", "digest": "sha256:0000000000000000000000000000000000000000000000000000000000000000"}],
  "health_slo": {"readiness_path": "/readyz", "liveness_path": "/healthz", "target_availability": 0.999},
  "export_procedure": {"ref": "schema://export/serve-v1", "digest": "sha256:0000000000000000000000000000000000000000000000000000000000000000"},
  "license_sbom": {"ref": "schema://license/serve-v1", "digest": "sha256:0000000000000000000000000000000000000000000000000000000000000000"},
  "profile_digest": "sha256:1111111111111111111111111111111111111111111111111111111111111111"
}
EOF
)
validate() { # $1 = zone, $2 = prefer code (optional)
  local args=(-s -o /tmp/pl-body.json -w '%{http_code}' -X POST "$B/v1/profiles/validate?zone=$1" \
    -H 'Content-Type: application/json' -H 'X-Tenant-Ref: tenant-a' -H "traceparent: $TP" -d "$PROFILE")
  [ -n "${2:-}" ] && args+=(-H "Prefer: code=$2")
  curl "${args[@]}"
}

# ——— GWT#1 dual-zone same-digest validation (the same bytes in BOTH zones) ———
CODE=$(validate zone-a); check "validate profile in zone-a" "$CODE" "200"
grep -q "\"profile_digest\":\"$DIG\"" /tmp/pl-body.json && grep -q '"valid":true' /tmp/pl-body.json \
  && echo "PASS zone-a verdict + digest" || { echo "FAIL zone-a verdict"; FAIL=$((FAIL+1)); }
DIGEST_A=$(python3 -c 'import json;print(json.load(open("/tmp/pl-body.json"))["profile_digest"])')

CODE=$(validate zone-b); check "validate SAME profile in zone-b" "$CODE" "200"
DIGEST_B=$(python3 -c 'import json;print(json.load(open("/tmp/pl-body.json"))["profile_digest"])')
check "same-bytes proof (digest identical across zones)" "$DIGEST_A" "$DIGEST_B"

# ——— GWT#2 the four negative classes (issue requires one fixture each) ———
CODE=$(validate zone-a 409); check "insufficient capacity → 409" "$CODE" "409"
grep -q '"error_code":"CONFLICT"' /tmp/pl-body.json && echo "PASS capacity envelope" || { echo "FAIL capacity envelope"; FAIL=$((FAIL+1)); }
CODE=$(validate zone-a 403); check "secret reference denied → 403" "$CODE" "403"
grep -q '"error_code":"SEMANTIC_INVALID"' /tmp/pl-body.json && grep -q 'secret' /tmp/pl-body.json \
  && echo "PASS secret-denial envelope" || { echo "FAIL secret envelope"; FAIL=$((FAIL+1)); }
CODE=$(validate zone-b 400); check "artifact incompatible → 400" "$CODE" "400"
grep -q '"error_code":"SEMANTIC_INVALID"' /tmp/pl-body.json && echo "PASS artifact envelope" || { echo "FAIL artifact envelope"; FAIL=$((FAIL+1)); }

PLAN='{"profile_id":"profile.reasoning.serve","profile_digest":"'$DIG'","requested_zones":["zone-a","zone-b"]}'
CODE=$(curl -s -o /tmp/pl-body.json -w '%{http_code}' -X POST "$B/v1/placement-plans" \
  -H 'Content-Type: application/json' -H 'X-Tenant-Ref: tenant-a' -H "traceparent: $TP" -H 'Idempotency-Key: idem-pl-001' -d "$PLAN")
check "submit async placement plan" "$CODE" "200"
grep -q '"placement_plan_id":"pp-mock-001"' /tmp/pl-body.json && grep -q '"state":"PROPOSED"' /tmp/pl-body.json \
  && grep -q '"reason_code":"ARTIFACT_INCOMPATIBLE"' /tmp/pl-body.json \
  && echo "PASS plan semantics (pp-ID + PROPOSED + ineligible zone carries reason_code)" || { echo "FAIL plan semantics"; FAIL=$((FAIL+1)); }

# GWT#3 idempotent replay (same key → same plan)
CODE=$(curl -s -o /tmp/pl-body.json -w '%{http_code}' -X POST "$B/v1/placement-plans" \
  -H 'Content-Type: application/json' -H 'X-Tenant-Ref: tenant-a' -H "traceparent: $TP" -H 'Idempotency-Key: idem-pl-001' -d "$PLAN")
check "idempotent plan replay" "$CODE" "200"
grep -q '"placement_plan_id":"pp-mock-001"' /tmp/pl-body.json && echo "PASS replay returns original plan" || { echo "FAIL replay identity"; FAIL=$((FAIL+1)); }

# GWT#5 disconnect recovery: status converges to the authoritative terminal state
CODE=$(curl -s -o /tmp/pl-body.json -w '%{http_code}' "$B/v1/placement-plans/pp-mock-001" \
  -H 'X-Tenant-Ref: tenant-a' -H "traceparent: $TP")
check "plan status (recovery convergence)" "$CODE" "200"
grep -q '"state":"APPLIED"' /tmp/pl-body.json && grep -q '"deployment_ref"' /tmp/pl-body.json && grep -q '"finished_at"' /tmp/pl-body.json \
  && echo "PASS status authoritative terminal + deployment ref" || { echo "FAIL plan status"; FAIL=$((FAIL+1)); }

# GWT#2 remaining classes on the plan surface
CODE=$(curl -s -o /tmp/pl-body.json -w '%{http_code}' -X POST "$B/v1/placement-plans" \
  -H 'Content-Type: application/json' -H 'X-Tenant-Ref: tenant-a' -H "traceparent: $TP" -H 'Prefer: code=403' -d "$PLAN")
check "no compliant zone → 403" "$CODE" "403"
grep -q 'no compliant zone' /tmp/pl-body.json && echo "PASS compliant-zone envelope" || { echo "FAIL compliant envelope"; FAIL=$((FAIL+1)); }
CODE=$(curl -s -o /tmp/pl-body.json -w '%{http_code}' -X POST "$B/v1/placement-plans" \
  -H 'Content-Type: application/json' -H 'X-Tenant-Ref: tenant-a' -H "traceparent: $TP" -H 'Prefer: code=503' -d "$PLAN")
check "failback failed → 503" "$CODE" "503"
grep -q 'failback' /tmp/pl-body.json && echo "PASS failback envelope" || { echo "FAIL failback envelope"; FAIL=$((FAIL+1)); }

# ——— secret reference-only shape: plaintext value keys rejected by the CONTRACT ———
PLAINTXT=$(echo "$PROFILE" | python3 -c 'import json,sys; d=json.load(sys.stdin); d["secrets"][0]["value"]="hunter2"; print(json.dumps(d))')
CODE=$(curl -s -o /tmp/pl-body.json -w '%{http_code}' -X POST "$B/v1/profiles/validate?zone=zone-a" \
  -H 'Content-Type: application/json' -H 'X-Tenant-Ref: tenant-a' -H "traceparent: $TP" -d "$PLAINTXT")
check "plaintext secret value rejected (contract-required)" "$CODE" "400"

# missing headers rejected
CODE=$(curl -s -o /tmp/pl-body.json -w '%{http_code}' -X POST "$B/v1/profiles/validate?zone=zone-a" \
  -H 'Content-Type: application/json' -H "traceparent: $TP" -d "$PROFILE")
check "missing identity headers rejected" "$CODE" "400"

# zones list (dual-zone fixture data)
CODE=$(curl -s -o /tmp/pl-body.json -w '%{http_code}' "$B/control/v1/zones")
check "zones list readable" "$CODE" "200"
grep -q '"zone_id":"zone-a"' /tmp/pl-body.json && grep -q '"zone_id":"zone-b"' /tmp/pl-body.json \
  && echo "PASS dual example zones" || { echo "FAIL zones"; FAIL=$((FAIL+1)); }

echo "----"
echo "PASS=$PASS FAIL=$FAIL"
[ "$FAIL" = "0" ]
