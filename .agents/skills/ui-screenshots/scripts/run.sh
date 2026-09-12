#!/usr/bin/env bash
# run.sh — provision + run the capture engine. Ensures playwright-core is installed
# in this scripts dir (node_modules is gitignored), resolves the instance URL from
# `uzi auth status` when --url is omitted, then runs capture-ui.mjs against the
# manifest, staging PNGs for review. Everything else is passed through.
#
# Usage:
#   run.sh [--url <url>] [--stage <dir>] [--only slugs] [--repo id] [--run id] [passthrough...]
# Needs a Chrome already up via launch-chrome.sh (CDP on --port, default 9222).
set -euo pipefail
DIR="$(cd "$(dirname "$0")" && pwd)"
SKILL="$(cd "$DIR/.." && pwd)"

# args: capture --url / --stage if the caller set them, else fill defaults.
URL=""; STAGE=""; pass=()
while [ $# -gt 0 ]; do
  case "$1" in
    --url) URL="${2:-}"; shift 2 ;;
    --stage) STAGE="${2:-}"; shift 2 ;;
    *) pass+=("$1"); shift ;;
  esac
done
[ -n "$URL" ] || URL="$(uzi auth status 2>/dev/null | awk '/^URL/{print $2}')"
[ -n "$URL" ] || { echo "no --url and could not read it from 'uzi auth status'" >&2; exit 2; }
[ -n "$STAGE" ] || STAGE="${TMPDIR:-/tmp}/uzi-ui-shots"

command -v node >/dev/null 2>&1 || { echo "node not found on PATH" >&2; exit 4; }
# Provision playwright-core into a cache dir OUTSIDE the skill (keeps the skill dir
# small); capture-ui.mjs loads it from UZI_SHOTS_DEPS via createRequire.
DEPS="${UZI_SHOTS_DEPS:-${HOME}/.cache/uzi-ui-screenshots}"
if [ ! -d "$DEPS/node_modules/playwright-core" ]; then
  echo "provisioning playwright-core into $DEPS (one-time)…"
  mkdir -p "$DEPS"
  ( cd "$DEPS" && npm i --silent playwright-core >/dev/null 2>&1 )
fi
export UZI_SHOTS_DEPS="$DEPS"

echo "instance: $URL"
echo "stage:    $STAGE"
node "$DIR/capture-ui.mjs" --url "$URL" --manifest "$SKILL/shots.json" --stage "$STAGE" "${pass[@]}"
