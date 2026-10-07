#!/usr/bin/env sh
# Assemble a versioned release: cross-platform binaries (mcp/eval/goldgen),
# packaged tarballs + windows zip, the offline docker image, SHA256SUMS.
# Usage: sh scripts/release.sh v0.22.0
# Steps and the checks behind them: docs/RELEASE_CHECKLIST.md.
#
# SQLON_VERIFY_ORACLE='<docker-network> <host:port/service> <user> <password>'
# makes the image check open a real Oracle connection (see verify-image.sh).
set -eu
V="${1:?usage: release.sh vX.Y.Z}"
ROOT="$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)"
cd "$ROOT"
D="dist/$V"; P="$D/pkg"
mkdir -p "$P"

# inject the release version (strip leading v) so the running server, MCP
# serverInfo, and web UI all report exactly this tag — no manual const drift.
VER="${V#v}"
LDFLAGS="-s -w -X sqlon/internal/mcp.Version=$VER"

for spec in "windows amd64 .exe" "linux amd64 " "linux arm64 "; do
  set -- $spec; goos=$1; goarch=$2; ext=${3:-}
  CGO_ENABLED=0 GOOS=$goos GOARCH=$goarch go build -trimpath -ldflags="$LDFLAGS" \
    -o "$D/sqlon-$goos-$goarch$ext" ./cmd/sqlon
  CGO_ENABLED=0 GOOS=$goos GOARCH=$goarch go build -trimpath -ldflags="$LDFLAGS" \
    -o "$D/sqlon-eval-$goos-$goarch$ext" ./cmd/jamypg-eval
  CGO_ENABLED=0 GOOS=$goos GOARCH=$goarch go build -trimpath -ldflags="$LDFLAGS" \
    -o "$D/sqlon-goldgen-$goos-$goarch$ext" ./cmd/jamypg-goldgen
done
echo "built binaries in $D"

for arch in amd64 arm64; do
  tar -czf "$P/sqlon-$V-linux-$arch.tar.gz" -C "$D" "sqlon-linux-$arch" -C "$ROOT" data docs README.md scripts/sqlon-disk-report.sh
done

python3 - "$D" "$V" <<'PY'
import sys, zipfile, os
D, V = sys.argv[1], sys.argv[2]
out = os.path.join(D, "pkg", f"sqlon-{V}-windows-amd64.zip")
with zipfile.ZipFile(out, "w", zipfile.ZIP_DEFLATED) as z:
    z.write(os.path.join(D, "sqlon-windows-amd64.exe"), "sqlon-windows-amd64.exe")
    for base in ("data", "docs"):
        for root, _, files in os.walk(base):
            for f in files:
                p = os.path.join(root, f); z.write(p, p)
    z.write("README.md", "README.md")
    z.write("scripts/sqlon-disk-report.sh", "scripts/sqlon-disk-report.sh")
print("wrote", out)
PY

# The docker image is the deliverable for offline sites: sqlon:vX.Y.Z, saved
# as sqlon-vX.Y.Z.tar.gz with a .sha256 sidecar. It is the Oracle build
# (godror + Instant Client), which also links the PostgreSQL/MySQL/MariaDB
# drivers, so one image serves every engine.
IMG="sqlon:$V"; TAR="sqlon-$V.tar.gz"
docker build -q -f Dockerfile.oracle --build-arg VERSION="$VER" -t "$IMG" . >/dev/null
docker save "$IMG" | gzip > "$P/$TAR"
( cd "$P" && sha256sum "$TAR" > "$TAR.sha256" )
echo "saved docker image $IMG -> $P/$TAR"

# load the saved tarball and exercise it before anything is published
sh scripts/verify-image.sh "$P/$TAR" "$IMG" 1

# the disk-report agent ships on its own too, for DB hosts that only need it
cp scripts/sqlon-disk-report.sh "$P/sqlon-disk-report.sh"

( cd "$P" && sha256sum ./*.tar.gz ./*.zip ./sqlon-disk-report.sh > SHA256SUMS.txt && cat SHA256SUMS.txt )
