#!/usr/bin/env sh
set -eu

ROOT="$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)"
OUT="${1:-dist}"
mkdir -p "$ROOT/$OUT"

cd "$ROOT"
CGO_ENABLED=1 go build -tags oracle -trimpath -ldflags="-s -w" \
  -o "$ROOT/$OUT/sqlon-oracle-linux-amd64" ./cmd/sqlon
echo "built $ROOT/$OUT/sqlon-oracle-linux-amd64"
echo "runtime requires Oracle Instant Client; use Dockerfile.oracle for a bundled image"
