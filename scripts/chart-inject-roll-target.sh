#!/usr/bin/env bash
# Mutate only an untracked packaging copy. Never invent identity from Chart.appVersion.
set -euo pipefail
[ "$#" -eq 2 ] || { echo "usage: $0 <chart-copy> <verified-stamp>" >&2; exit 2; }
chart="$1" stamp="$2"
[[ "$stamp" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-rc\.[0-9]+)?\+g[0-9a-f]{7}$ ]] || { echo "invalid stamp" >&2; exit 2; }
[ -f "$chart/values.yaml" ] || { echo "chart values missing" >&2; exit 2; }
source_chart="$(cd "$(dirname "$0")/../deploy/chart" && pwd -P)"
chart_path="$(cd "$chart" && pwd -P)"
if [ "$chart_path" = "$source_chart" ] || git -C "$chart" ls-files --error-unmatch values.yaml >/dev/null 2>&1; then
  echo "refusing tracked chart; use a packaging copy" >&2; exit 1
fi
pin="$(awk '
  /^workers:/ { inw=1; next }
  inw && /^[^[:space:]]/ { inw=0 }
  inw && /^  image:/ { inimg=1; next }
  inw && inimg && /^  [^[:space:]]/ { inimg=0 }
  inw && inimg && /^    tag:[[:space:]]/ { v=$2; gsub(/"/, "", v); print v; exit }
' "$chart/values.yaml")"
[ "${stamp%+g*}" = "$pin" ] || { echo "stamp does not match chart worker pin" >&2; exit 1; }
tmp="$(mktemp "$chart/.roll-target.XXXXXX")"
trap 'rm -f "$tmp"' EXIT
awk -v stamp="$stamp" '
  /^workers:/ { workers++; inw=1 }
  inw && /^[^[:space:]]/ && $0 !~ /^workers:/ { inw=0 }
  inw && /^  rollTargetVersion:/ {
    count++; old=$0; sub(/^  rollTargetVersion:[[:space:]]*/, "", old)
    sub(/[[:space:]]+#.*$/, "", old); sub(/[[:space:]]*$/, "", old)
    if (old != "\"\"" && old != "\"" stamp "\"" && old != stamp) {
      print "committed roll target differs or is unsupported" > "/dev/stderr"; bad=1
    }
    if (old == "\"\"") print "  rollTargetVersion: \"" stamp "\""
    else print
    next
  }
  { print }
  END { if (bad || workers != 1 || count != 1) exit 1 }
' "$chart/values.yaml" > "$tmp"
mv "$tmp" "$chart/values.yaml"
