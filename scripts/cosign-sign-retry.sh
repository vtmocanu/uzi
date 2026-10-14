#!/usr/bin/env bash
# Retry keyless signing only for explicit secondary-rate-limit / HTTP 429 / 5xx
# failures. Five attempts, with 5/10/20/30-second backoff (65 seconds total).
# This bounds retries, not the duration of an individual cosign request.
set -euo pipefail
[ "$#" -eq 1 ] || { echo "usage: $0 <reference@digest>" >&2; exit 2; }
work_dir="$(mktemp -d)"
trap 'rm -rf "$work_dir"' EXIT
attempt=1
delay=5
while :; do
  rc=0
  cosign sign --yes "$1" > "$work_dir/out" 2> "$work_dir/err" || rc=$?
  cat "$work_dir/out"
  cat "$work_dir/err" >&2
  [ "$rc" -ne 0 ] || exit 0
  if ! grep -qiF 'secondary rate limit' "$work_dir/out" "$work_dir/err" &&
     ! grep -qiE '(http(/[0-9.]+)?( status( code)?)?|unexpected status( code)?|status( code)?)[[:space:]:=]+(429|5[0-9][0-9])([[:space:][:punct:]]|$)' "$work_dir/out" "$work_dir/err"; then
    exit "$rc"
  fi
  [ "$attempt" -lt 5 ] || exit "$rc"
  echo "cosign-sign-retry: transient signing failure; attempt $attempt/5, retrying in ${delay}s" >&2
  sleep "$delay"
  attempt=$((attempt + 1))
  delay=$((delay * 2))
  [ "$delay" -le 30 ] || delay=30
done
