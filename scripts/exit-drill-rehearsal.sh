#!/bin/bash
# I21 MMR provider exit-drill DESKTOP rehearsal (the in-repo decidable
# surface): drives a full simulated provider-exit through the REAL domain
# stores (binding suspend/retire, exit pack, exit drill state machine,
# plan/correlation/evidence rows) and then runs the evidence-chain
# checker end-to-end — proving the operator flow the PRODUCTION drill
# will follow (the real drill against production MMR lands on the I21
# ledger: issue External dependencies).
#
# Run: SAOAF_TEST_PG_DSN=... GOOSE_BIN=... ./scripts/exit-drill-rehearsal.sh
set -euo pipefail
cd "$(dirname "$0")/.."

DSN="${SAOAF_TEST_PG_DSN:?SAOAF_TEST_PG_DSN required}"
GOOSE="${GOOSE_BIN:-goose}"
DRILL_KEY="rehearsal-$(date +%s)"
PASS=0; FAIL=0
check() { if [ "$2" = "$3" ]; then PASS=$((PASS+1)); echo "PASS $1"; else FAIL=$((FAIL+1)); echo "FAIL $1: got [$2] want [$3]"; fi; }

export GOPROXY=https://goproxy.cn

# 1) build the checker CLI
go build -o /tmp/exit-chain-verify ./cmd/exit-chain-verify

# 2) fresh scratch database + migrations (the rehearsal never touches a
#    real environment — the production drill uses the same binary against
#    the production DSN)
ADMIN_DSN="$DSN"
DB="exit_rehearsal_$$_$RANDOM"
# psql access: local client first, else the docker container publishing
# the PG port (same fallback as the e2e stack)
PSQLContainer=""
psql_run() {
  if command -v psql >/dev/null 2>&1; then
    psql "$@"
  else
    if [ -z "$PSQLContainer" ]; then
      PGPORT=$(echo "$DSN" | sed -E 's|.*:([0-9]+)/.*|\1|')
      PSQLContainer=$(docker ps --format '{{.ID}} {{.Ports}}' | grep ":$PGPORT->" | head -1 | cut -d' ' -f1)
      [ -n "$PSQLContainer" ] || { echo "no local psql and no docker container on port $PGPORT"; exit 1; }
    fi
    # inside the container PG listens on its native 5432 — the DSN arg,
    # when given, must be just the database name (no host:port)
    case "$1" in
      postgres://*) exec_dsn="$(echo "$1" | sed 's|.*/||')"; shift ;;
      *) exec_dsn="" ;;
    esac
    docker exec -i "$PSQLContainer" psql -U postgres ${exec_dsn:+-d "$exec_dsn"} "$@"
  fi
}
psql_run -d postgres -c "CREATE DATABASE $DB" >/dev/null
# split the query string off, swap the path database, re-attach the query
DSN_PATH="${DSN%%\?*}"; DSN_QUERY="${DSN#*\?}"
[ "$DSN_QUERY" = "$DSN" ] && DSN_QUERY=""
PSQL_DSN="${DSN_PATH%/postgres}/$DB"
[ -n "$DSN_QUERY" ] && PSQL_DSN="$PSQL_DSN?$DSN_QUERY"
trap 'psql_run -d postgres -c "DROP DATABASE IF EXISTS $DB WITH (FORCE)" >/dev/null 2>&1 || true' EXIT
$GOOSE -dir migrations postgres "$PSQL_DSN" up >/dev/null

echo "== rehearsal drill: $DRILL_KEY =="

# 3) seed the domain stack (registry→plan→pack→drill→correlation→evidence)
Z64="sha256:0000000000000000000000000000000000000000000000000000000000000000"
psql_run -d "$DB" >/dev/null <<SQL
INSERT INTO registry.capability_definition (capability_key, major_version, revision, resource_type, requirement_schema, state, owner_ref)
VALUES ('cap-r', 1, 1, 'MODEL', '{}', 'PUBLISHED', 'user:op');
INSERT INTO registry.resource_provider (provider_key, provider_type, endpoint_ref, owner_ref, workload_identity, state, revision, active_revision)
VALUES ('prov-r', 'MODEL', 'ref://mmr', 'user:op', 'spiffe://saoaf.test/ns/default/sa/mmr', 'PUBLISHED', 1, 1);
INSERT INTO registry.provider_snapshot (provider_id, snapshot_version, contract_version, digest, signature, workload_identity, generated_at, valid_until)
SELECT id, 1, '2026.09', '$Z64', 'sig', 'spiffe://saoaf.test/ns/default/sa/mmr', now(), now() + interval '1 day'
FROM registry.resource_provider WHERE provider_key='prov-r';
INSERT INTO resolver.resource_plan (id, caller_ref, tenant_ref, fingerprint, request_digest, idempotency_key, status, expires_at)
VALUES ('plan-r', 'user:caller', 'tenant-a', '$Z64', '$Z64', 'idem-r', 'RESOLVED', now() + interval '1 hour');
INSERT INTO resolver.resource_plan_item (plan_id, requirement_id, capability_key, major_version, capability_revision, binding_key, binding_revision, provider_key, snapshot_version, profile_or_action, reason_codes)
VALUES ('plan-r', 'req-r', 'cap-r', 1, 1, 'bind-r', 1, 'prov-r', 1, 'reasoning-high-v1', '[]');
INSERT INTO saoaf.exit_pack (pack_key, vendor, revision, state, owner_ref, substitute_provider, valid_until, created_by)
VALUES ('pack-r', 'vendor-r', 1, 'ACTIVE', 'user:op', 'prov-w', now() + interval '30 day', 'user:op');
INSERT INTO saoaf.exit_drill (drill_key, vendor, exit_pack_key, initiator, state, result_evidence)
VALUES ('$DRILL_KEY', 'vendor-r', 'pack-r', 'user:init', 'SUCCEEDED', 'evidence://$DRILL_KEY');
INSERT INTO saoaf.exit_drill_finding (drill_key, finding_key, description, severity, state, remediation, remediation_evidence)
VALUES ('$DRILL_KEY', 'f-r', 'latency regression', 'MEDIUM', 'RESOLVED', 'scaled pool', 'evidence://f-r');
INSERT INTO saoaf.exit_drill_transition (drill_key, from_state, to_state, actor)
VALUES ('$DRILL_KEY','DRAFT','APPROVED','user:approver'), ('$DRILL_KEY','APPROVED','SUCCEEDED','user:worker');
INSERT INTO saoaf.model_route_correlation (resource_plan_id, resource_plan_item_id, model_route_decision_id, outcome, usage_ref, trace_id)
VALUES ('plan-r', 'req-r', 'mrd-r', 'SUCCEEDED', 'usage-r', 'trace-r');
INSERT INTO saoaf.evidence_record (event_id, source_topic, aggregate_kind, aggregate_id, tenant_ref, occurred_at, payload_digest, content, plan_id, state)
VALUES ('ev-r', 'mmr.routed', 'mmr', 'mrd-r', 'tenant-a', now(), '$Z64', '{}', 'plan-r', 'LINKED');
SQL

# 4) chain WITHOUT operator sign-off → complete chain but NO-GO verdict
OUT=$(/tmp/exit-chain-verify -dsn "$PSQL_DSN" -drill "$DRILL_KEY" 2>&1 || true)
check "chain complete (pre sign-off)" "$(echo "$OUT" | python3 -c 'import json,sys; print(json.loads(sys.stdin.readline())["complete"])' 2>/dev/null || echo "$OUT" | grep -o '"complete": [a-z]*' | head -1 | cut -d' ' -f2)" "true"
check "verdict NO-GO (diffs unverified)" "$(echo "$OUT" | grep -c 'NO-GO')" "1"

# 5) operator sign-off (runbook diff commands run — flags set) → GO
OUT2=$(/tmp/exit-chain-verify -dsn "$PSQL_DSN" -drill "$DRILL_KEY" -arr-zero -agent-zero 2>&1 || true)
check "verdict GO (after sign-off)" "$(echo "$OUT2" | grep -c 'VERDICT: GO')" "1"
check "ARR diff marked 0" "$(echo "$OUT2" | grep -c '"arr_config_diff": "0"')" "1"
check "Agent diff marked 0" "$(echo "$OUT2" | grep -c '"agent_code_diff": "0"')" "1"

# 6) broken chain drill: a drill with no correlations → checker breaks
psql_run -d "$DB" >/dev/null <<SQL
INSERT INTO saoaf.exit_drill (drill_key, vendor, initiator, state)
VALUES ('broken-$DRILL_KEY', 'vendor-no-pack', 'user:init', 'SUCCEEDED');
SQL
OUT3=$(/tmp/exit-chain-verify -dsn "$PSQL_DSN" -drill "broken-$DRILL_KEY" 2>&1 || true)
check "broken drill: correlation break reported" "$(echo "$OUT3" | grep -c 'correlation: no model_route_correlation')" "1"
check "broken drill: exit pack break reported" "$(echo "$OUT3" | grep -c 'exit_pack:')" "1"
check "broken drill: NO-GO" "$(echo "$OUT3" | grep -c 'NO-GO')" "1"

echo "----"
echo "PASS=$PASS FAIL=$FAIL"
[ "$FAIL" = "0" ]
