#!/usr/bin/env sh
set -eu

ROOT="$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)"
OUT="${1:-dist}"
mkdir -p "$ROOT/$OUT"

build_one() {
  goos="$1"
  goarch="$2"
  name="$3"
  CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" \
    go build -trimpath -ldflags="-s -w" -o "$ROOT/$OUT/$name" ./cmd/sqlon
  echo "built $ROOT/$OUT/$name"
}

cd "$ROOT"
build_one windows amd64 sqlon-windows-amd64.exe
build_one linux amd64 sqlon-linux-amd64
build_one linux arm64 sqlon-linux-arm64
