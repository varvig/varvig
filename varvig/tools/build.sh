#!/usr/bin/env bash
# build.sh — cross-compile one release binary for a target triple.
#
# Usage: tools/build.sh <goos>-<goarch>
#   e.g. tools/build.sh linux-amd64
#
# The binary is static (CGO disabled — the shipping configuration, see the CI
# workflow's "cgo-free" job) and stamped with the release version so
# `varvig --version` reports the tag the marketplace pins against
# (varvig-release-automation §4). Output lands in <repo>/dist/varvig-<target>
# (with a .exe suffix on Windows). VERSION is taken from the environment,
# defaulting to "dev" for local builds.
set -euo pipefail

target="${1:?usage: build.sh <goos>-<goarch>}"
goos="${target%-*}"
goarch="${target##*-}"

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"   # <repo>/varvig/tools
module="$(cd "$here/.." && pwd)"                        # <repo>/varvig  (the Go module)
dist="$(cd "$module/.." && pwd)/dist"                   # <repo>/dist
mkdir -p "$dist"

out="$dist/varvig-$target"
if [ "$goos" = "windows" ]; then
  out="$out.exe"
fi

version="${VERSION:-dev}"

echo "building varvig $version for $goos/$goarch -> $out"
cd "$module"
CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" \
  go build \
  -trimpath \
  -ldflags "-s -w -X main.version=$version" \
  -o "$out" \
  ./cmd/varvig

echo "built $out"
