#!/usr/bin/env bash
# SAOAF local build (I02). CI runs the same script so "works locally" and
# "works in CI" cannot drift apart.
#
# Produces, under ./dist/:
#   control-plane-api-<os>-<arch>        binary
#   control-plane-worker-<os>-<arch>     binary
#   control-plane-api-<os>-<arch>.sha256 digest files
#   control-plane-worker-<os>-<arch>.sha256
#
# Digest rule (issue acceptance): build twice from the same tree → identical
# digests. We enforce this by rebuilding into dist2/ and comparing. The
# binaries embed no timestamps/paths: -trimpath + fixed main module version.
set -euo pipefail
cd "$(dirname "$0")/.."

GO=${GO:-go}
TARGETS=${TARGETS:-"$(go env GOOS)-$(go env GOARCH)"}
VERSION=${VERSION:-"0.1.0-i02"}

rm -rf dist dist2
mkdir -p dist

build_into() {
  out=$1
  for target in $TARGETS; do
    os=${target%%-*}; arch=${target##*-}
    for bin in control-plane-api control-plane-worker; do
      # -buildvcs=false: VCS stamping would embed the build's git state,
      # breaking digest reproducibility across environments (local checkout
      # vs PR merge ref vs main checkout). The version is injected via
      # ldflags instead. -trimpath removes local path leakage.
      CGO_ENABLED=0 GOOS=$os GOARCH=$arch $GO build \
        -trimpath \
        -buildvcs=false \
        -ldflags="-s -w -X main.version=$VERSION" \
        -o "$out/$bin-$os-$arch" \
        "./cmd/$bin"
      ( cd "$out" && shasum -a 256 "$bin-$os-$arch" > "$bin-$os-$arch.sha256" )
    done
  done
}

build_into dist
# Reproducibility assertion: same tree, second build must match byte-for-byte.
build_into dist2
for f in dist/*.sha256; do
  base=$(basename "$f" .sha256)
  if ! cmp -s "dist/$base" "dist2/$base"; then
    echo "REPRODUCIBILITY FAIL: dist/$base differs from dist2/$base" >&2
    exit 1
  fi
done
rm -rf dist2

echo "BUILD: PASS"
cat dist/*.sha256
