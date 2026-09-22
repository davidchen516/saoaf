#!/usr/bin/env bash
# SAOAF local verification suite (I02) — same commands CI runs.
# Exit nonzero on first failure; each stage prints a one-line result.
set -euo pipefail
cd "$(dirname "$0")/.."

GO=${GO:-go}

echo "== 1/7 gofmt =="
command -v gofmt >/dev/null || { echo "gofmt not available"; exit 1; }
unformatted=$(gofmt -l . | grep -v '^web/' || true)
[ -z "$unformatted" ] || { echo "gofmt needed on: $unformatted"; exit 1; }
echo "gofmt: clean"

echo "== 2/7 go vet =="
$GO vet ./...
echo "vet: clean"

echo "== 3/7 unit tests (race) =="
$GO test -race -count=1 ./...
echo "tests: pass"

echo "== 4/7 dependency boundary =="
$GO run ./tools/boundarycheck

echo "== 5/7 license policy =="
$GO run ./tools/licensecheck

echo "== 6/7 build + reproducibility =="
./scripts/build.sh

echo "== 7/7 web build =="
if command -v npm >/dev/null; then
  ( cd web && npm ci --no-fund --no-audit >/dev/null 2>&1 && npm run build >/dev/null )
  echo "web build: pass"
else
  echo "web build: SKIPPED (npm not available; CI runs it)"
fi

echo "VERIFY: ALL PASS"
