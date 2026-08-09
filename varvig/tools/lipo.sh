#!/usr/bin/env bash
# lipo.sh — combine the two macOS slices into a universal binary.
#
# The release matrix builds darwin-amd64 and darwin-arm64 separately; this fuses
# them into dist/varvig-darwin-universal so a single macOS artifact runs on both
# Apple Silicon and Intel (varvig-release-automation §4). It uses the in-repo
# makefat helper rather than Apple's `lipo`, so it works on the Linux runner the
# publish job uses.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"   # <repo>/varvig/tools
module="$(cd "$here/.." && pwd)"                        # <repo>/varvig
dist="$(cd "$module/.." && pwd)/dist"                   # <repo>/dist

amd64="$dist/varvig-darwin-amd64"
arm64="$dist/varvig-darwin-arm64"
universal="$dist/varvig-darwin-universal"

for f in "$amd64" "$arm64"; do
  if [ ! -f "$f" ]; then
    echo "lipo.sh: missing input $f (build darwin-amd64 and darwin-arm64 first)" >&2
    exit 1
  fi
done

echo "fusing darwin slices -> $universal"
cd "$module"
go run ./tools/makefat -o "$universal" "$amd64" "$arm64"
