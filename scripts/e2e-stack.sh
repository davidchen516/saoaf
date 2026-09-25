#!/bin/bash
# I17 E2E stack: real control-plane-api + vite dev server against a real
# seeded PostgreSQL, with stub OIDC/PDP/approval. Used by CI (E2E job) and
# locally. Brings the stack up, runs the browser suite, tears everything
# down. Requires: goose CLI, node + npm (web deps installed), playwright
# (web devDependency), a reachable PostgreSQL (env SAOAF_TEST_PG_DSN).
#
# CI runs Playwright's bundled chromium; local runs prefer system Chrome
# (channel:'chrome') and fall back automatically (the suite tries both).
set -u
cd "$(dirname "$0")/.."

PGDSN="${SAOAF_TEST_PG_DSN:-postgres://postgres:postgres@127.0.0.1:5432/postgres}"
PGHOST=$(echo "$PGDSN" | sed -E 's|.*@([^:/]+):.*|\1|')
PGPORT=$(echo "$PGDSN" | sed -E 's|.*:([0-9]+)/.*|\1|')
PGUSER=$(echo "$PGDSN" | sed -E 's|.*://([^:]+):.*|\1|')
PGPASS=$(echo "$PGDSN" | sed -E 's|.*://[^:]+:([^@]+)@.*|\1|')
DB=e2e_$$_$RANDOM
# derive the per-run DSN: swap the path's database name, preserving any
# query string (e.g. ?sslmode=disable)
DSN=$(echo "$PGDSN" | sed -E "s|(postgres://[^/]*)/[^?]*|\1/$DB|")
API_PORT=${E2E_API_PORT:-8099}
VITE_PORT=${E2E_VITE_PORT:-5199}

# psql access: prefer the local client; when absent, find the docker
# container publishing $PGPORT and exec psql inside it (the local dev
# setup runs PG in docker with no host psql installed).
PSQLContainer=""
psql_exec() {
  if command -v psql >/dev/null 2>&1; then
    PGPASSWORD="$PGPASS" psql -h "$PGHOST" -p "$PGPORT" -U "$PGUSER" "$@"
  else
    if [ -z "$PSQLContainer" ]; then
      PSQLContainer=$(docker ps --format '{{.ID}} {{.Ports}}' | grep ":$PGPORT->" | head -1 | cut -d' ' -f1)
      [ -n "$PSQLContainer" ] || { echo "E2E-STACK: no local psql and no docker container on port $PGPORT"; return 1; }
    fi
    docker exec -i "$PSQLContainer" psql -U "$PGUSER" "$@"
  fi
}

cleanup() {
  [ -n "${API_PID:-}" ] && kill "$API_PID" 2>/dev/null
  [ -n "${VITE_PID:-}" ] && kill "$VITE_PID" 2>/dev/null
  [ -n "${ISS_PID:-}" ] && kill "$ISS_PID" 2>/dev/null
  [ -n "${PDP_PID:-}" ] && kill "$PDP_PID" 2>/dev/null
  [ -n "${APPR_PID:-}" ] && kill "$APPR_PID" 2>/dev/null
  sleep 1
  psql_exec -c "DROP DATABASE IF EXISTS $DB WITH (FORCE)" >/dev/null 2>&1
  rm -rf "$ISS_DIR" /tmp/e2e-tok-*.txt /tmp/e2e-api-bin /tmp/e2e-api.log /tmp/e2e-vite.log /tmp/e2e-vite.pid 2>/dev/null
  # vite runs under an npx wrapper — kill the port listener tree too
  pkill -f "vite --host 127.0.0.1 --port $VITE_PORT" 2>/dev/null
}
trap cleanup EXIT INT TERM

# 1) fresh DB + migrations (create/drop via psql; the goose run uses the
# DSN over the wire)
psql_exec -c "DROP DATABASE IF EXISTS $DB WITH (FORCE)" >/dev/null 2>&1
psql_exec -c "CREATE DATABASE $DB" || { echo "E2E-STACK: cannot create $DB"; exit 1; }
if ! goose -dir migrations postgres "$DSN" up >/dev/null 2>&1; then
  echo "E2E-STACK: goose migration failed"; exit 1
fi

# 2) seed the ops views (metrics incl. UNKNOWN + broken ref, evidence,
# alert, expired pack, drill + finding + transitions — same shape as
# internal/ops/ops_test.go)
psql_exec -d "$DB" -v ON_ERROR_STOP=1 <<'SQL'
INSERT INTO saoaf.metric_result (metric_key, dimensions, value, status, status_reason, dataset_revision, formula_version, evidence_ref) VALUES
 ('substitution_coverage', '{"tenant":"tenant-a","environment":"production","capability":"cap-1"}', 0.75, 'OK', '', 42, 'v1', 'ev-resolved-1'),
 ('substitution_coverage', '{"tenant":"tenant-a","environment":"staging","capability":"cap-1"}', NULL, 'UNKNOWN', 'no active binding for capability', 42, 'v1', ''),
 ('substitution_coverage', '{"tenant":"tenant-b","environment":"production","capability":"cap-1"}', 0.10, 'OK', '', 42, 'v1', 'ev-resolved-1'),
 ('evidence_completeness', '{"tenant":"tenant-a","environment":"production"}', NULL, 'INSUFFICIENT_DATA', 'empty evidence', 42, 'v1', 'ev-missing-9');
INSERT INTO saoaf.evidence_record (event_id, source_topic, aggregate_kind, aggregate_id, tenant_ref, occurred_at, payload_digest, content, plan_id, retention_class, worm_status, state) VALUES ('ev-resolved-1', 'binding.published', 'binding', 'b-1', 'tenant-a', now(), 'sha256:0000000000000000000000000000000000000000000000000000000000000000', '{}', '', 'STANDARD', 'ARCHIVED', 'LINKED');
INSERT INTO saoaf.risk_alert (rule_key, entity_kind, entity_id, dataset_revision, severity, detail, state) VALUES ('single-provider', 'capability', 'cap-1', 42, 'HIGH', '{}', 'OPEN');
INSERT INTO saoaf.exit_pack (pack_key, vendor, revision, state, owner_ref, substitute_provider, recovery_steps, evidence_refs, checklist, valid_until, created_by) VALUES ('vendor-x', 'vendor-x', 1, 'ACTIVE', 'user:ops', 'prov-y', '["step1"]', '["ev-resolved-1"]', '{}', now() - interval '1 day', 'user:ops');
INSERT INTO saoaf.exit_drill (drill_key, vendor, initiator, state) VALUES ('drill-1', 'vendor-x', 'user:init', 'SUCCEEDED');
INSERT INTO saoaf.exit_drill_finding (drill_key, finding_key, description, severity, state, remediation) VALUES ('drill-1', 'f-1', 'fallback too slow', 'HIGH', 'OPEN', '');
INSERT INTO saoaf.exit_drill_transition (drill_key, from_state, to_state, actor) VALUES ('drill-1', 'DRAFT', 'APPROVED', 'user:approver'), ('drill-1', 'APPROVED', 'SUCCEEDED', 'user:worker');
SQL

# 3) stubs (PDP allow, approval APPROVED, OIDC with JWKS) — background
# python servers reading their port from argv
PDP_PORT=$((25000 + RANDOM % 4000)); APPR_PORT=$((PDP_PORT + 1)); ISS_PORT=$((APPR_PORT + 1))
ISS_DIR=$(mktemp -d)
openssl genrsa -out "$ISS_DIR/jwt.pem" 2048 2>/dev/null
N=$(openssl rsa -in "$ISS_DIR/jwt.pem" -noout -modulus 2>/dev/null | cut -d'=' -f2 | xxd -r -p | base64 | tr '+/' '-_' | tr -d '=' | tr -d '\n')
python3 /dev/stdin "$PDP_PORT" <<'EOF' & PDP_PID=$!
import sys, http.server, json
class H(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        self.send_response(200); self.send_header('Content-Type','application/json'); self.end_headers()
        self.wfile.write(json.dumps({"decision": True, "context": {"policy_decision_id": "d1"}}).encode())
    def log_message(self, *a): pass
http.server.HTTPServer(('127.0.0.1', int(sys.argv[1])), H).serve_forever()
EOF
python3 /dev/stdin "$APPR_PORT" <<'EOF' & APPR_PID=$!
import sys, http.server, json, time
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        self.send_response(200); self.send_header('Content-Type','application/json'); self.end_headers()
        self.wfile.write(json.dumps({"approval_ref":"approval:1","status":"APPROVED","requester_ref":"user:ops","approver_refs":["user:bob"],"decided_at":time.strftime('%Y-%m-%dT%H:%M:%SZ'),"object_digest":"sha256:mock"}).encode())
    def log_message(self, *a): pass
http.server.HTTPServer(('127.0.0.1', int(sys.argv[1])), H).serve_forever()
EOF
python3 /dev/stdin "$ISS_PORT" "$N" <<'EOF' & ISS_PID=$!
import sys, http.server, json
port, n = sys.argv[1], sys.argv[2]
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        body = b'{}'
        if self.path == '/.well-known/openid-configuration':
            body = json.dumps({"issuer": f"http://127.0.0.1:{port}", "jwks_uri": f"http://127.0.0.1:{port}/keys"}).encode()
        elif self.path == '/keys':
            body = json.dumps({"keys": [{"kty":"RSA","kid":"k1","alg":"RS256","n":n,"e":"AQAB"}]}).encode()
        self.send_response(200); self.send_header('Content-Type','application/json'); self.end_headers(); self.wfile.write(body)
    def log_message(self, *a): pass
http.server.HTTPServer(('127.0.0.1', int(port)), H).serve_forever()
EOF
sleep 1

# 4) mint tokens (ops + scope-less) — pyjwt for RS256 signing
pip3 install --quiet --user pyjwt cryptography >/dev/null 2>&1 || true
python3 /dev/stdin "$ISS_DIR/jwt.pem" "http://127.0.0.1:$ISS_PORT" "ops.read ops.all-tenants" "user:ops" > /tmp/e2e-tok-ops.txt <<'EOF'
import sys, time, jwt
key = open(sys.argv[1]).read()
iss, scope, sub = sys.argv[2], sys.argv[3], sys.argv[4]
print(jwt.encode({"iss": iss, "aud": "saoaf-control-plane", "sub": sub, "jti": "j-ops",
                  "scope": scope, "tenant_ref": "tenant-a", "exp": int(time.time()) + 3600},
                 key, algorithm="RS256", headers={"kid": "k1"}))
EOF
python3 /dev/stdin "$ISS_DIR/jwt.pem" "http://127.0.0.1:$ISS_PORT" "resource.read" "user:limited" > /tmp/e2e-tok-noscope.txt <<'EOF'
import sys, time, jwt
key = open(sys.argv[1]).read()
iss, scope, sub = sys.argv[2], sys.argv[3], sys.argv[4]
print(jwt.encode({"iss": iss, "aud": "saoaf-control-plane", "sub": sub, "jti": "j-lim",
                  "scope": scope, "tenant_ref": "tenant-a", "exp": int(time.time()) + 3600},
                 key, algorithm="RS256", headers={"kid": "k1"}))
EOF
TOK_OPS=$(cat /tmp/e2e-tok-ops.txt)
TOK_NOSCOPE=$(cat /tmp/e2e-tok-noscope.txt)
[ -n "$TOK_OPS" ] && [ -n "$TOK_NOSCOPE" ] || { echo "E2E-STACK: token mint failed"; exit 1; }

# 5) the API (all surfaces: admin + hub + ops). Pre-build to keep the
# readiness loop short (a cold `go run` compile can exceed it).
go build -o /tmp/e2e-api-bin ./cmd/control-plane-api || { echo "E2E-STACK: api build failed"; exit 1; }
SAOAF_OIDC_ISSUER="http://127.0.0.1:$ISS_PORT" \
SAOAF_PDP_URL="http://127.0.0.1:$PDP_PORT" \
SAOAF_APPROVAL_URL="http://127.0.0.1:$APPR_PORT" \
SAOAF_DB_DSN="$DSN" \
CONTROL_PLANE_API_ADDR="127.0.0.1:$API_PORT" \
/tmp/e2e-api-bin >/tmp/e2e-api.log 2>&1 & API_PID=$!

# 6) the UI dev server (proxy → API)
# setsid: the npx wrapper spawns a node child; killing the session id
# takes the whole tree down (a bare wrapper kill orphans the vite server
# and --strictPort fails the NEXT run's readiness)
(cd web && SAOAF_API_TARGET="http://127.0.0.1:$API_PORT" nohup setsid npx vite --host 127.0.0.1 --port "$VITE_PORT" --strictPort >/tmp/e2e-vite.log 2>&1 & echo $! > /tmp/e2e-vite.pid)
VITE_PID=$(cat /tmp/e2e-vite.pid)

# 7) wait for readiness (hard-fail when the loops time out — a silent
# fallthrough would hit dead ports at the sanity gates)
ready=0
for i in $(seq 1 60); do
  if curl -sf "http://127.0.0.1:$API_PORT/healthz" >/dev/null 2>&1; then ready=1; break; fi
  sleep 1
done
[ "$ready" = "1" ] || { echo "E2E-STACK: API did not become ready"; tail -5 /tmp/e2e-api.log; exit 1; }
ready=0
for i in $(seq 1 60); do
  if curl -sf "http://127.0.0.1:$VITE_PORT/" >/dev/null 2>&1 || curl -sf "http://localhost:$VITE_PORT/" >/dev/null 2>&1; then ready=1; break; fi
  sleep 1
done
[ "$ready" = "1" ] || { echo "E2E-STACK: vite did not become ready"; tail -5 /tmp/e2e-vite.log; exit 1; }

# sanity gates before the browser runs
B="http://127.0.0.1:$API_PORT"
code() { curl -s -o /dev/null -w '%{http_code}' "$@"; }
[ "$(code "$B/admin/v1/ops/overview" -H "Authorization: Bearer $TOK_OPS")" = "200" ] || { echo "E2E-STACK: ops overview not 200"; exit 1; }
[ "$(code "$B/admin/v1/ops/metrics" -H "Authorization: Bearer $TOK_NOSCOPE")" = "403" ] || { echo "E2E-STACK: scope-less not 403"; exit 1; }
[ "$(code "$B/admin/v1/ops/overview")" = "401" ] || { echo "E2E-STACK: anonymous not 401"; exit 1; }

# 8) run the browser suite (local: system Chrome; CI: bundled chromium —
# the suite auto-falls-back when channel:'chrome' is unavailable).
# E2E_HOLD=1 keeps the stack up (debugging) without running the suite.
if [ "${E2E_HOLD:-0}" = "1" ]; then
  echo "E2E-HOLD: stack live (API :$API_PORT, vite :$VITE_PORT, tokens /tmp/e2e-tok-*.txt); Ctrl-C tears down"
  wait
  exit 0
fi
cd web
E2E_UI_URL="http://127.0.0.1:$VITE_PORT" node --test test/ops.e2e.mjs
RC=$?
exit $RC