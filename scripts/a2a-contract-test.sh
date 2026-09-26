#!/bin/bash
# I19 A2A contract consumer run: walks the task lifecycle against the Prism
# mock (no real A2A runtime) and asserts every contract-level negative
# fixture. Run: ./scripts/a2a-contract-test.sh (mock on :4040).
set -u
B="${A2A_MOCK_URL:-http://127.0.0.1:4040}"
PASS=0; FAIL=0
check() { if [ "$2" = "$3" ]; then PASS=$((PASS+1)); echo "PASS $1"; else FAIL=$((FAIL+1)); echo "FAIL $1: got [$2] want [$3]"; fi }
TP="00-00000000000000000000000000000000-0000000000000000-01"
BODY() { cat /tmp/a2a-body.json; }
SUB='{"agent_id":"agent.supply-risk.analyze","delegation_ref":"delegation:mock-001","delegation_scope":["analysis.read","report.write"],"requested_scopes":["analysis.read"],"input_ref":{"uri":"evidence://task-inputs/ti-mock-001","digest":"sha256:0000000000000000000000000000000000000000000000000000000000000000"}}'

# ——— GWT#1 happy path: submit → status → artifact ———
CODE=$(curl -s -o /tmp/a2a-body.json -w '%{http_code}' -X POST "$B/v1/agent-tasks" \
  -H 'Content-Type: application/json' -H "X-Resource-Plan-ID: plan-mock-001" -H "X-Tenant-Ref: tenant-a" \
  -H "traceparent: $TP" -H 'Idempotency-Key: idem-a2a-001' -d "$SUB")
check "submit delegated task" "$CODE" "200"
grep -q '"agent_task_id":"at-mock-001"' /tmp/a2a-body.json && grep -q '"delegation_ref":"delegation:mock-001"' /tmp/a2a-body.json \
  && grep -q '"granted_scopes":\["analysis.read"\]' /tmp/a2a-body.json && grep -q '"state":"RUNNING"' /tmp/a2a-body.json \
  && echo "PASS submit semantics (at-ID + delegation + granted scopes + state)" || { echo "FAIL submit semantics"; FAIL=$((FAIL+1)); }

# GWT#3 idempotent replay: same Idempotency-Key → same task
CODE=$(curl -s -o /tmp/a2a-body.json -w '%{http_code}' -X POST "$B/v1/agent-tasks" \
  -H 'Content-Type: application/json' -H "X-Resource-Plan-ID: plan-mock-001" -H "X-Tenant-Ref: tenant-a" \
  -H "traceparent: $TP" -H 'Idempotency-Key: idem-a2a-001' -d "$SUB")
check "idempotent replay (same key → same at- task)" "$CODE" "200"
grep -q '"agent_task_id":"at-mock-001"' /tmp/a2a-body.json && echo "PASS replay returns original task" || { echo "FAIL replay identity"; FAIL=$((FAIL+1)); }

# GWT#5 disconnect recovery: status query converges to the authoritative terminal state
CODE=$(curl -s -o /tmp/a2a-body.json -w '%{http_code}' "$B/v1/agent-tasks/at-mock-001" \
  -H "X-Tenant-Ref: tenant-a" -H "traceparent: $TP")
check "status query (recovery convergence)" "$CODE" "200"
grep -q '"state":"COMPLETED"' /tmp/a2a-body.json && grep -q '"finished_at"' /tmp/a2a-body.json \
  && grep -q '"artifact_ref"' /tmp/a2a-body.json \
  && echo "PASS status authoritative terminal + artifact reference" || { echo "FAIL status state"; FAIL=$((FAIL+1)); }
# artifact is reference + digest ONLY (no inline content key anywhere)
grep -q '"content"' /tmp/a2a-body.json && { echo "FAIL artifact content leaked inline"; FAIL=$((FAIL+1)); } || echo "PASS artifact reference-only shape"

# ——— GWT#4 cancel/status race: cancel on a terminal task → 409 ———
CODE=$(curl -s -o /tmp/a2a-body.json -w '%{http_code}' -X DELETE "$B/v1/agent-tasks/at-mock-001" \
  -H "X-Tenant-Ref: tenant-a" -H "traceparent: $TP" -H 'Idempotency-Key: idem-a2a-cancel' -H 'Prefer: code=409')
check "cancel on terminal task → 409 (race lost)" "$CODE" "409"
grep -q '"error_code":"CONFLICT"' /tmp/a2a-body.json && echo "PASS cancel race lost envelope" || { echo "FAIL cancel race envelope"; FAIL=$((FAIL+1)); }

# ——— GWT#6 delegation scope exceeded → 403 (contract example selection) ———
OVER='{"agent_id":"agent.supply-risk.analyze","delegation_ref":"delegation:mock-001","delegation_scope":["analysis.read"],"requested_scopes":["database.admin"]}'
CODE=$(curl -s -o /tmp/a2a-body.json -w '%{http_code}' -X POST "$B/v1/agent-tasks" \
  -H 'Content-Type: application/json' -H "X-Resource-Plan-ID: plan-mock-001" -H "X-Tenant-Ref: tenant-a" \
  -H "traceparent: $TP" -H 'Prefer: code=403' -d "$OVER")
check "delegation scope exceeded → 403" "$CODE" "403"
grep -q '"error_code":"SEMANTIC_INVALID"' /tmp/a2a-body.json && grep -q 'delegation reference' /tmp/a2a-body.json \
  && echo "PASS delegation-exceeded envelope (SEMANTIC_INVALID)" || { echo "FAIL delegation envelope"; FAIL=$((FAIL+1)); }

# ——— GWT#2 malicious/unknown agent card → 404 ———
EVIL='{"agent_id":"agent.evil.exfiltrate","delegation_ref":"delegation:mock-001","delegation_scope":["analysis.read"],"requested_scopes":["analysis.read"]}'
CODE=$(curl -s -o /tmp/a2a-body.json -w '%{http_code}' -X POST "$B/v1/agent-tasks" \
  -H 'Content-Type: application/json' -H "X-Resource-Plan-ID: plan-mock-001" -H "X-Tenant-Ref: tenant-a" \
  -H "traceparent: $TP" -H 'Prefer: code=404' -d "$EVIL")
check "unknown/malicious agent → 404" "$CODE" "404"
grep -q '"error_code":"NOT_FOUND"' /tmp/a2a-body.json && echo "PASS unknown-agent envelope" || { echo "FAIL unknown-agent envelope"; FAIL=$((FAIL+1)); }

# ——— negative fixtures via example selection ———
CODE=$(curl -s -o /tmp/a2a-body.json -w '%{http_code}' -X POST "$B/v1/agent-tasks" \
  -H 'Content-Type: application/json' -H "X-Resource-Plan-ID: plan-mock-001" -H "X-Tenant-Ref: tenant-a" \
  -H "traceparent: $TP" -H 'Prefer: code=400' -d '{"agent_id":"agent.supply-risk.analyze"}')
check "missing delegation fields → 400" "$CODE" "400"
grep -q '"error_code":"VALIDATION_MISSING_REQUIRED"' /tmp/a2a-body.json && echo "PASS validation envelope" || { echo "FAIL validation envelope"; FAIL=$((FAIL+1)); }

CODE=$(curl -s -o /tmp/a2a-body.json -w '%{http_code}' -X POST "$B/v1/agent-tasks" \
  -H 'Content-Type: application/json' -H "X-Resource-Plan-ID: plan-mock-001" -H "X-Tenant-Ref: tenant-a" \
  -H "traceparent: $TP" -H 'Prefer: code=503' -d "$SUB")
check "agent runtime transport failure/timeout → 503" "$CODE" "503"
grep -q '"error_code":"INTERNAL"' /tmp/a2a-body.json && echo "PASS runtime-unavailable envelope" || { echo "FAIL runtime envelope"; FAIL=$((FAIL+1)); }

CODE=$(curl -s -o /tmp/a2a-body.json -w '%{http_code}' "$B/v1/agent-tasks/at-no-such-task" \
  -H "X-Tenant-Ref: tenant-a" -H "traceparent: $TP" -H 'Prefer: code=404')
check "unknown task → 404" "$CODE" "404"

# missing required headers rejected (contract-required identity)
CODE=$(curl -s -o /tmp/a2a-body.json -w '%{http_code}' -X POST "$B/v1/agent-tasks" \
  -H 'Content-Type: application/json' -H "traceparent: $TP" -d "$SUB")
check "missing identity headers rejected (contract-required)" "$CODE" "400"

# ——— ARR-facing snapshot ———
CODE=$(curl -s -o /tmp/a2a-body.json -w '%{http_code}' "$B/control/v1/agent-snapshots/current")
check "agent snapshot readable" "$CODE" "200"
grep -q '"trust_domain":"partner"' /tmp/a2a-body.json && grep -q '"max_delegation_scope"' /tmp/a2a-body.json \
  && grep -q '"signature"' /tmp/a2a-body.json && echo "PASS snapshot carries trust_domain/delegation scope/signature" || { echo "FAIL snapshot semantics"; FAIL=$((FAIL+1)); }

echo "----"
echo "PASS=$PASS FAIL=$FAIL"
[ "$FAIL" = "0" ]
