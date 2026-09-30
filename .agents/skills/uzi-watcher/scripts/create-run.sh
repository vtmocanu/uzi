#!/usr/bin/env bash
# Create a uzi run, retrying ONLY the label-sync rejections, then print its JSON.
#
# A label added on the forge reaches uzi's issue cache on the next poller sync, so a
# create right after labelling can be refused with "not marked as uzi's work" (or, for
# an issue filed moments ago, "issue not found on this repo's board"). Those refusals
# create nothing, so retrying them is safe. Every other error stops at once: a generic
# failure is never turned into repeated create attempts.
#
# stdout carries only uzi's --json document; uzi's stderr (the version-skew warning, the
# refusal text) stays on stderr, so `create-run.sh … | jq` is safe.
#
# Usage: create-run.sh <repo-id> <issue> [extra `uzi run create` flags...]
#   e.g. create-run.sh "$REPO_ID" 1909 --mr-rework
# Env: UZI_BIN (default uzi), CREATE_RETRY_SECS (default 90: one default poll interval
#   plus sync margin), CREATE_RETRY_INTERVAL (default 15).
# Exit: 0 created; otherwise uzi's own exit code from the last attempt. When the retry
#   window runs out on a label-sync refusal, stderr ends with RETRY_EXHAUSTED.
set -u
REPO="${1:?usage: create-run.sh <repo-id> <issue> [uzi run create flags...]}"
ISSUE="${2:?usage: create-run.sh <repo-id> <issue> [uzi run create flags...]}"
shift 2
UZI="${UZI_BIN:-uzi}"
LIMIT="${CREATE_RETRY_SECS:-90}"
INT="${CREATE_RETRY_INTERVAL:-15}"

err="$(mktemp)"
trap 'rm -f "$err"' EXIT
start=$SECONDS
while :; do
  rc=0
  out="$("$UZI" run create --repo "$REPO" --issue "$ISSUE" ${1+"$@"} --json 2>"$err")" || rc=$?
  cat "$err" >&2
  if [ "$rc" -eq 0 ]; then
    printf '%s\n' "$out"
    exit 0
  fi
  if ! grep -q -F -e "not marked as uzi's work" -e "issue not found on this repo's board" "$err"; then
    exit "$rc"
  fi
  if [ $((SECONDS - start + INT)) -gt "$LIMIT" ]; then
    echo "RETRY_EXHAUSTED after $((SECONDS - start))s: uzi still refuses; check the label on the forge" >&2
    exit "$rc"
  fi
  echo "label not synced yet; retrying in ${INT}s" >&2
  sleep "$INT"
done
