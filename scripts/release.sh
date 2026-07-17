#!/usr/bin/env sh
# Assemble a versioned release: cross-platform binaries (mcp/eval/goldgen),
# packaged tarballs + windows zip, docker image tar, SHA256SUMS.
# Usage: sh scripts/release.sh v0.22.0
set -eu
V="${1:?usage: release.sh vX.Y.Z}"
ROOT="$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)"
cd "$ROOT"
D="dist/$V"; P="$D/pkg"
mkdir -p "$P"

# inject the release version (strip leading v) so the running server, MCP
# serverInfo, and web UI all report exactly this tag — no manual const drift.
VER="${V#v}"
LDFLAGS="-s -w -X jamypg/internal/mcp.Version=$VER"

for cmd in mcp eval goldgen; do
  for spec in "windows amd64 .exe" "linux amd64 " "linux arm64 "; do
    set -- $spec; goos=$1; goarch=$2; ext=${3:-}
    CGO_ENABLED=0 GOOS=$goos GOARCH=$goarch go build -trimpath -ldflags="$LDFLAGS" \
      -o "$D/jamypg-$cmd-$goos-$goarch$ext" ./cmd/jamypg-$cmd
  done
done
echo "built binaries in $D"

for arch in amd64 arm64; do
  tar -czf "$P/jamypg-mcp-$V-linux-$arch.tar.gz" -C "$D" "jamypg-mcp-linux-$arch" -C "$ROOT" data docs README.md
done

python3 - "$D" "$V" <<'PY'
import sys, zipfile, os
D, V = sys.argv[1], sys.argv[2]
out = os.path.join(D, "pkg", f"jamypg-mcp-{V}-windows-amd64.zip")
with zipfile.ZipFile(out, "w", zipfile.ZIP_DEFLATED) as z:
    z.write(os.path.join(D, "jamypg-mcp-windows-amd64.exe"), "jamypg-mcp-windows-amd64.exe")
    for base in ("data", "docs"):
        for root, _, files in os.walk(base):
            for f in files:
                p = os.path.join(root, f); z.write(p, p)
    z.write("README.md", "README.md")
print("wrote", out)
PY

docker build -q --build-arg VERSION="$VER" -t "jamypg-mcp:$V" . >/dev/null
docker save "jamypg-mcp:$V" | gzip > "$P/jamypg-mcp-$V-docker.tar.gz"
echo "saved docker image"

( cd "$P" && sha256sum ./*.tar.gz ./*.zip > SHA256SUMS.txt && cat SHA256SUMS.txt )
