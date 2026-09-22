#!/usr/bin/env bash
# I04 persistence gate: run the migration tests against a REAL PostgreSQL
# 18.6 container. Locally boots a throwaway container on :5433; CI passes
# SAOAF_TEST_PG_DSN pointing at the service container.
set -euo pipefail
cd "$(dirname "$0")/.."

export PATH="$HOME/go-toolchain/go1.27.1/bin:$PATH"
if [ -z "${GOOSE_BIN:-}" ] && [ -x "$HOME/go-toolchain/bin/goose" ]; then
  export GOOSE_BIN="$HOME/go-toolchain/bin/goose"
fi

if [ -n "${SAOAF_TEST_PG_DSN:-}" ]; then
  echo "using provided SAOAF_TEST_PG_DSN"
  go test -race -count=1 -v ./migrations
  exit $?
fi

CONTAINER=saoaf-pg-i04
docker rm -f "$CONTAINER" >/dev/null 2>&1 || true
docker run -d --name "$CONTAINER" \
  -e POSTGRES_PASSWORD=postgres \
  -p 127.0.0.1:5433:5432 \
  postgres:18.6 >/dev/null

cleanup() { docker rm -f "$CONTAINER" >/dev/null 2>&1 || true; }
trap cleanup EXIT

echo "waiting for postgres health..."
for i in $(seq 1 60); do
  if docker exec "$CONTAINER" pg_isready -U postgres >/dev/null 2>&1; then
    break
  fi
  sleep 1
done

export SAOAF_TEST_PG_DSN="postgres://postgres:postgres@127.0.0.1:5433/postgres"
go test -race -count=1 -v ./migrations
