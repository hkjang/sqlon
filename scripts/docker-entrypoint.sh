#!/bin/sh
# Seed an empty data directory from the metadata baked into the image, then
# exec sqlon. A named volume receives those files from the image on first
# mount; a bind-mounted host directory does not, and sqlon then exits at boot
# with "load SQLON catalog: ... no such file or directory".
set -eu
DATA=/app/data/sqlon
prev=
for a in "$@"; do
  case "$prev" in -data|--data) DATA="$a" ;; esac
  case "$a" in -data=*|--data=*) DATA="${a#*=}" ;; esac
  prev="$a"
done
if [ ! -e "$DATA/meta_physical_models.json" ]; then
  if ! { mkdir -p "$DATA" && cp -Rn /usr/share/sqlon/seed/. "$DATA/"; } 2>/dev/null; then
    echo "sqlon: cannot write $DATA as uid $(id -u); on the host run: chown -R $(id -u):$(id -g) <mounted dir>" >&2
    exit 1
  fi
  echo "sqlon: seeded $DATA from the image's metadata" >&2
fi
exec sqlon "$@"
