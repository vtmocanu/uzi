#!/usr/bin/env bash
# launch-chrome.sh — launch an app-mode Chrome with CDP enabled, logged in to the
# uzi instance, for the ui-screenshots tool. App mode (--app=) drops the tab strip
# and URL bar; capture itself uses CDP Page.captureScreenshot (viewport only), so
# no window chrome is ever in a shot. A persistent profile keeps the SSO session so
# you only log in once. The instance URL is an argument, never hardcoded.
#
# Usage: launch-chrome.sh <url> [profile-dir] [cdp-port]
set -euo pipefail
URL="${1:?usage: launch-chrome.sh <url> [profile-dir] [port]}"
PROFILE="${2:-${HOME}/.cache/uzi-ui-screenshots-profile}"
PORT="${3:-9222}"
CHROME="/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"
[ -x "$CHROME" ] || { echo "Chrome not found at: $CHROME" >&2; exit 1; }
mkdir -p "$PROFILE"
"$CHROME" --app="$URL" \
  --user-data-dir="$PROFILE" \
  --remote-debugging-port="$PORT" \
  --window-size=1500,1120 --window-position=40,40 \
  --no-first-run --no-default-browser-check >/dev/null 2>&1 &
echo "app-mode Chrome launched: pid $!, CDP http://localhost:$PORT, profile $PROFILE"
echo "If the uzi login/SSO page shows, log in once in that window; the profile persists it."
