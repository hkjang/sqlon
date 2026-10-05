#!/bin/sh
# sqlon-disk-report.sh — report the database host's disk usage to SQLON early
# warning, so forecasts see what SQL cannot: everything else on the volume
# (dump backups, external logs, other programs) and its real size.
#
# Run it on the DB host every minute, e.g. from cron:
#   * * * * * SQLON_URL=https://sqlon.example.com SQLON_TOKEN=... \
#     SQLON_PROFILE=orders-prod /usr/local/bin/sqlon-disk-report.sh /var/lib/postgresql /pgwal
#
#   SQLON_URL      SQLON base URL
#   SQLON_TOKEN    admin token, or an MCP key (ssk_...) allowed to use the profile
#   SQLON_PROFILE  DB profile id (comma-separated for several databases on this host)
#   SQLON_HOST     name shown in alerts (default: hostname)
#   arguments      directories on the volumes to report (data directory, WAL, backups)
#
# Needs only POSIX sh, df, awk, and curl or wget.
set -eu

: "${SQLON_URL:?set SQLON_URL, e.g. https://sqlon.example.com}"
: "${SQLON_TOKEN:?set SQLON_TOKEN (admin token or MCP key)}"
: "${SQLON_PROFILE:?set SQLON_PROFILE to the DB profile id}"
if [ "$#" -eq 0 ]; then
  echo "usage: sqlon-disk-report.sh <data-dir> [<wal-dir> ...]" >&2
  exit 2
fi
host="${SQLON_HOST:-$(hostname)}"

# df -P: one line per filesystem, sizes in 1024-byte blocks. The mount point
# is the last column and may contain spaces; a volume named twice is sent once.
volumes=$(df -Pk "$@" | awk '
  NR > 1 {
    mount = $6; for (i = 7; i <= NF; i++) mount = mount " " $i
    if (seen[mount]++) next
    fs = $1
    gsub(/\\/, "\\\\", mount); gsub(/"/, "\\\"", mount)
    gsub(/\\/, "\\\\", fs); gsub(/"/, "\\\"", fs)
    printf "%s{\"mount\":\"%s\",\"filesystem\":\"%s\",\"total_bytes\":%.0f,\"used_bytes\":%.0f,\"avail_bytes\":%.0f}", (n++ ? "," : ""), mount, fs, $2 * 1024, $3 * 1024, $4 * 1024
  }')
profiles=$(printf '%s' "$SQLON_PROFILE" | awk -F, '{
  for (i = 1; i <= NF; i++) { gsub(/^ +| +$/, "", $i); if ($i != "") printf "%s\"%s\"", (n++ ? "," : ""), $i }
}')
body="{\"profiles\":[${profiles}],\"host\":\"${host}\",\"volumes\":[${volumes}]}"
url="${SQLON_URL%/}/api/early-warning/disk"

if command -v curl >/dev/null 2>&1; then
  curl -fsS --max-time 20 -X POST "$url" \
    -H "Authorization: Bearer ${SQLON_TOKEN}" -H "Content-Type: application/json" \
    --data "$body" >/dev/null
else
  wget -q -T 20 -O /dev/null \
    --header="Authorization: Bearer ${SQLON_TOKEN}" --header="Content-Type: application/json" \
    --post-data="$body" "$url"
fi
