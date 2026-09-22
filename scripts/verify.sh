#!/usr/bin/env bash
# SAOAF local verification suite (I02) — same commands CI runs.
# Exit nonzero on first failure; each stage prints a one-line result.
set -euo pipefail
cd "$(dirname "$0")/.."

GO=${GO:-go}

echo "== 1/8 gofmt =="
command -v gofmt >/dev/null || { echo "gofmt not available"; exit 1; }
unformatted=$(gofmt -l . | grep -v '^web/' || true)
[ -z "$unformatted" ] || { echo "gofmt needed on: $unformatted"; exit 1; }
echo "gofmt: clean"

echo "== 2/8 go vet =="
$GO vet ./...
echo "vet: clean"

echo "== 3/7 unit tests (race) =="
$GO test -race -count=1 ./...
echo "tests: pass"

echo "== 4/8 dependency boundary =="
$GO run ./tools/boundarycheck

echo "== 5/8 license policy =="
$GO run ./tools/licensecheck

echo "== 6/8 build + reproducibility =="
./scripts/build.sh

echo "== 7/8 contract gate =="
$GO run ./tools/contractlint validate
if git rev-parse --verify --quiet "HEAD~1" >/dev/null; then
  $GO run ./tools/contractlint breaking "HEAD~1"
else
  echo "breaking: SKIPPED (no previous commit)"
fi

echo "== 8/8 web build =="
if command -v npm >/dev/null; then
  ( cd web && npm ci --no-fund --no-audit >/dev/null 2>&1 && npm run build >/dev/null )
  echo "web build: pass"
else
  echo "web build: SKIPPED (npm not available; CI runs it)"
fi

echo "VERIFY: ALL PASS"
