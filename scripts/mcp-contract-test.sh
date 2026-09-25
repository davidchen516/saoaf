#!/bin/bash
# I18 MCP contract consumer run: walks the full action lifecycle against
# the Prism mock (no real MCP runtime) and asserts every contract-level
# negative fixture. Run: ./scripts/mcp-contract-test.sh (mock on :4030).
set -u
B="${MCP_MOCK_URL:-http://127.0.0.1:4030}"
PASS=0; FAIL=0
check() { if [ "$2" = "$3" ]; then PASS=$((PASS+1)); echo "PASS $1"; else FAIL=$((FAIL+1)); echo "FAIL $1: got [$2] want [$3]"; fi }
post() { curl -s -o /tmp/mcp-body.json -w '%{http_code}' -X POST "$B$1" -H 'Content-Type: application/json' "${@:3}" -d "$2"; }
TP="00-00000000000000000000000000000000-0000000000000000-01"

# ——— GWT#1 happy path: preview → approve → execute → status ———
CODE=$(post /v1/tools/tool.erp.work-order.write/preview '{"parameters":{"work_order_id":"WO-mock-001"}}' \
  -H "X-Resource-Plan-ID: plan-mock-001" -H "X-Resource-Plan-Item-ID: item-mock-001" -H "X-Tenant-Ref: tenant-a" -H "X-Policy-Decision-Ref: pd-mock-001" -H "traceparent: $TP")
check "preview HIGH-risk write" "$CODE" "200"
grep -q '"risk_level":"HIGH"' /tmp/mcp-body.json && grep -q '"approval_required":true' /tmp/mcp-body.json && grep -q '"next_step":"approve"' /tmp/mcp-body.json \
  && echo "PASS preview semantics (HIGH + approval_required + next_step)" || { echo "FAIL preview semantics"; FAIL=$((FAIL+1)); }

CODE=$(post /v1/tools/tool.erp.work-order.write/approve '{"preview_id":"tp-mock-001","approval_ref":"approval:mock-erp-write"}' \
  -H "X-Resource-Plan-ID: plan-mock-001" -H "X-Tenant-Ref: tenant-a" -H "traceparent: $TP")
check "approve with reference" "$CODE" "200"
grep -q '"next_step":"execute"' /tmp/mcp-body.json && echo "PASS approve semantics" || { echo "FAIL approve semantics"; FAIL=$((FAIL+1)); }

CODE=$(post /v1/tools/tool.erp.work-order.write/execute '{"approval_ref":"approval:mock-erp-write","parameters":{"work_order_id":"WO-mock-001","quantity":5}}' \
  -H "X-Resource-Plan-ID: plan-mock-001" -H "X-Resource-Plan-Item-ID: item-mock-001" -H "X-Tenant-Ref: tenant-a" -H "X-Policy-Decision-Ref: pd-mock-001" \
  -H "traceparent: $TP" -H 'Idempotency-Key: idem-mock-001')
check "execute with approval + idempotency key" "$CODE" "200"
grep -q '"tool_execution_id":"te-mock-001"' /tmp/mcp-body.json && grep -q '"state":"SUCCEEDED"' /tmp/mcp-body.json \
  && grep -q '"result_ref"' /tmp/mcp-body.json && echo "PASS execute evidence (id/state/result_ref)" || { echo "FAIL execute evidence"; FAIL=$((FAIL+1)); }

# GWT#3 idempotent replay: same key → same execution result
CODE=$(post /v1/tools/tool.erp.work-order.write/execute '{"approval_ref":"approval:mock-erp-write","parameters":{"work_order_id":"WO-mock-001","quantity":5}}' \
  -H "X-Resource-Plan-ID: plan-mock-001" -H "X-Resource-Plan-Item-ID: item-mock-001" -H "X-Tenant-Ref: tenant-a" -H "X-Policy-Decision-Ref: pd-mock-001" \
  -H "traceparent: $TP" -H 'Idempotency-Key: idem-mock-001')
check "idempotent replay (same key)" "$CODE" "200"
grep -q '"tool_execution_id":"te-mock-001"' /tmp/mcp-body.json && echo "PASS replay returns original execution" || { echo "FAIL replay identity"; FAIL=$((FAIL+1)); }

# GWT#5 crash recovery convergence: status query returns authoritative state
CODE=$(curl -s -o /tmp/mcp-body.json -w '%{http_code}' "$B/v1/tool-executions/te-mock-001" -H "X-Tenant-Ref: tenant-a" -H "traceparent: $TP")
check "status query (recovery convergence)" "$CODE" "200"
grep -q '"state":"SUCCEEDED"' /tmp/mcp-body.json && echo "PASS status authoritative state" || { echo "FAIL status state"; FAIL=$((FAIL+1)); }

# GWT#4 cancel race: cancel on a terminal execution → 409 (execute won)
CODE=$(curl -s -o /tmp/mcp-body.json -w '%{http_code}' -X DELETE "$B/v1/tool-executions/te-mock-001" -H "X-Tenant-Ref: tenant-a" -H "traceparent: $TP" -H 'Idempotency-Key: idem-cancel-001' -H 'Prefer: code=409')
check "cancel on terminal execution → 409 (race lost)" "$CODE" "409"
grep -q '"error_code":"CONFLICT"' /tmp/mcp-body.json && echo "PASS cancel race lost envelope" || { echo "FAIL cancel race envelope"; FAIL=$((FAIL+1)); }

# ——— GWT#6 negative: HIGH-risk execute WITHOUT approval reference → 403 ———
CODE=$(post /v1/tools/tool.erp.work-order.write/execute '{"parameters":{"work_order_id":"WO-mock-001"}}' \
  -H "X-Resource-Plan-ID: plan-mock-001" -H "X-Resource-Plan-Item-ID: item-mock-001" -H "X-Tenant-Ref: tenant-a" -H "X-Policy-Decision-Ref: pd-mock-001" \
  -H "traceparent: $TP" -H 'Prefer: code=403')
check "HIGH-risk execute without approval → 403" "$CODE" "403"
grep -q '"error_code":"SEMANTIC_INVALID"' /tmp/mcp-body.json && grep -q 'approval reference' /tmp/mcp-body.json \
  && echo "PASS approval-required envelope (SEMANTIC_INVALID)" || { echo "FAIL approval-required envelope"; FAIL=$((FAIL+1)); }

# ——— GWT#2 negative fixtures via Prism example selection ———
CODE=$(post /v1/tools/tool.erp.work-order.write/preview '{"parameters":{}}' \
  -H "X-Resource-Plan-ID: plan-mock-001" -H "X-Resource-Plan-Item-ID: item-mock-001" -H "X-Tenant-Ref: tenant-a" -H "X-Policy-Decision-Ref: pd-mock-001" \
  -H "traceparent: $TP" -H 'Prefer: code=400')
check "parameter validation failure → 400" "$CODE" "400"
grep -q '"error_code":"VALIDATION_MISSING_REQUIRED"' /tmp/mcp-body.json && echo "PASS parameter error envelope" || { echo "FAIL parameter envelope"; FAIL=$((FAIL+1)); }

CODE=$(post /v1/tools/tool.erp.work-order.write/preview '{"parameters":{"work_order_id":"WO-1"}}' \
  -H "X-Resource-Plan-ID: plan-mock-001" -H "X-Resource-Plan-Item-ID: item-mock-001" -H "X-Tenant-Ref: tenant-a" -H "X-Policy-Decision-Ref: pd-mock-001" \
  -H "traceparent: $TP" -H 'Prefer: code=404')
check "unknown action → 404" "$CODE" "404"

CODE=$(post /v1/tools/tool.erp.work-order.write/execute '{"approval_ref":"approval:mock-erp-write","parameters":{"work_order_id":"WO-1"}}' \
  -H "X-Resource-Plan-ID: plan-mock-001" -H "X-Resource-Plan-Item-ID: item-mock-001" -H "X-Tenant-Ref: tenant-a" -H "X-Policy-Decision-Ref: pd-mock-001" \
  -H "traceparent: $TP" -H 'Prefer: code=409')
check "idempotency conflict → 409" "$CODE" "409"
grep -q '"error_code":"CONFLICT"' /tmp/mcp-body.json && echo "PASS idempotency conflict envelope" || { echo "FAIL conflict envelope"; FAIL=$((FAIL+1)); }

CODE=$(post /v1/tools/tool.erp.work-order.write/execute '{"approval_ref":"approval:mock-erp-write","parameters":{"work_order_id":"WO-1"}}' \
  -H "X-Resource-Plan-ID: plan-mock-001" -H "X-Resource-Plan-Item-ID: item-mock-001" -H "X-Tenant-Ref: tenant-a" -H "X-Policy-Decision-Ref: pd-mock-001" \
  -H "traceparent: $TP" -H 'Prefer: code=503')
check "tool server transport failure → 503" "$CODE" "503"
grep -q '"error_code":"INTERNAL"' /tmp/mcp-body.json && echo "PASS transport failure envelope" || { echo "FAIL transport envelope"; FAIL=$((FAIL+1)); }

# missing required header (gateway identity) → Prism rejects the request
# (5.x uses 400 for its own validation errors; the CONTRACT requires the
# header — the mock proves the requirement is enforced)
CODE=$(post /v1/tools/tool.erp.work-order.write/preview '{"parameters":{"work_order_id":"WO-1"}}' -H "traceparent: $TP")
check "missing identity headers rejected (contract-required)" "$CODE" "400"

# ——— ARR-facing snapshot ———
CODE=$(curl -s -o /tmp/mcp-body.json -w '%{http_code}' "$B/control/v1/tool-snapshots/current")
check "tool snapshot readable" "$CODE" "200"
grep -q '"risk_level":"HIGH"' /tmp/mcp-body.json && grep -q '"approval_required":true' /tmp/mcp-body.json \
  && grep -q '"idempotency"' /tmp/mcp-body.json && echo "PASS snapshot carries risk/idempotency/approval" || { echo "FAIL snapshot semantics"; FAIL=$((FAIL+1)); }

echo "----"
echo "PASS=$PASS FAIL=$FAIL"
[ "$FAIL" = "0" ]