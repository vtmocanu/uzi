#!/usr/bin/env bash
# capture.sh — screenshot a desktop browser window at RETINA (2x) via Orca
# computer-use, retrying until a real window capture (not the low-res
# desktop-region fallback) lands. Solves the flaky-scale problem where a window
# in native fullscreen, on a non-Retina display, or occluded is captured at
# scale < 1 and comes out soft.
#
# Optionally navigates to a URL (in a new tab, non-destructive) and resizes the
# window to a fixed size first, so framing is reproducible. It captures whatever
# the window then shows; pair with crop-ui.sh to strip the browser chrome.
#
# Usage:
#   capture.sh --app <selector> <output.png> [--url URL] [--size WxH] \
#              [--min-scale F] [--tries N] [--crop] [--width N]
#
#   --app <selector>  Orca app selector: a browser bundle id (app.zen-browser.zen,
#                     com.google.Chrome, com.apple.Safari, com.microsoft.edgemac),
#                     a unique app name, or pid:<n>. Required.
#   --url URL         open URL in a NEW tab first (non-destructive to existing tabs),
#                     then capture. The uzi instance URL is passed here, never hardcoded.
#   --size WxH        resize the browser window to WxH points before capturing
#                     (via osascript). e.g. --size 1600x1000. Reproducible framing.
#   --min-scale F     minimum capture scale to accept as retina (default 1.5).
#   --tries N         attempts before giving up and saving the best shot (default 8).
#   --crop            also run crop-ui.sh on the result (in-place) to strip chrome.
#   --width N         with --crop, downscale the crop to N px wide (e.g. 1800).
#
# The Orca binary is $ORCA_CLI_COMMAND if set, else `orca`. See the computer-use
# skill for selector discovery (`orca computer list-apps --json`). Resize needs
# macOS Automation permission for the terminal (System Events).
#
# Exit: 0 retina ok; 1 saved a low-res shot (best effort — fix the window); 2 usage; 4 tool missing.
set -euo pipefail

die() { echo "capture: $2" >&2; exit "$1"; }
ORCA="${ORCA_CLI_COMMAND:-orca}"
command -v "$ORCA" >/dev/null 2>&1 || die 4 "Orca CLI '$ORCA' not found (set ORCA_CLI_COMMAND or install Orca)"

APP=""; OUT=""; URL=""; SIZE=""; MIN_SCALE="1.5"; TRIES=8; DOCROP=0; WIDTH=""
while [ $# -gt 0 ]; do
  case "$1" in
    --app)       APP="${2:-}"; shift 2 ;;
    --url)       URL="${2:-}"; shift 2 ;;
    --size)      SIZE="${2:-}"; shift 2 ;;
    --min-scale) MIN_SCALE="${2:-}"; shift 2 ;;
    --tries)     TRIES="${2:-}"; shift 2 ;;
    --crop)      DOCROP=1; shift ;;
    --width)     WIDTH="${2:-}"; shift 2 ;;
    -*) die 2 "unknown flag: $1" ;;
    *) if [ -z "$OUT" ]; then OUT="$1"; else die 2 "too many args"; fi; shift ;;
  esac
done
[ -n "$APP" ] || die 2 "missing --app <selector>"
[ -n "$OUT" ] || die 2 "usage: capture.sh --app <selector> <output.png> [--url URL] [--size WxH]"

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
o() { "$ORCA" computer "$@" --app "$APP" --no-screenshot --json >/dev/null 2>&1 || true; }

# Resize the front window to WxH points via System Events (bundle id -> process name).
resize_window() {
  local w="${SIZE%x*}" h="${SIZE#*x}" proc
  case "$SIZE" in *x*) ;; *) echo "capture: --size must be WxH (e.g. 1600x1000); skipping resize" >&2; return 0 ;; esac
  proc="$(osascript -e "tell application \"System Events\" to get name of first process whose bundle identifier is \"$APP\"" 2>/dev/null || true)"
  [ -n "$proc" ] || proc="$APP"
  osascript -e "tell application \"System Events\" to tell process \"$proc\" to set size of front window to {$w, $h}" 2>/dev/null \
    || echo "capture: resize failed (grant the terminal Automation permission for System Events)" >&2
}

# Open URL in a new tab (leaves existing tabs untouched).
navigate() {
  o get-app-state --restore-window
  o hotkey --key "CmdOrCtrl+T" --restore-window
  sleep 0.8
  o type-text --text "$URL" --restore-window
  o press-key --key Return --restore-window
  sleep 2.5
}

crop_here() {
  [ "$DOCROP" -eq 1 ] || return 0
  local args=("$OUT" "$OUT"); [ -n "$WIDTH" ] && args+=(--width "$WIDTH")
  "$SCRIPT_DIR/crop-ui.sh" "${args[@]}"
}

[ -n "$URL" ] && navigate
[ -n "$SIZE" ] && resize_window
[ -n "$SIZE" ] && sleep 0.5

TMP="$(mktemp -t uzi-capture-XXXX.json)"
trap 'rm -f "$TMP"' EXIT

best_path=""; best_scale="0"
for try in $(seq 1 "$TRIES"); do
  "$ORCA" computer get-app-state --app "$APP" --restore-window --json > "$TMP" 2>/dev/null || true
  path="$(grep -oE '/[^"]*screenshot\.png' "$TMP" | head -1 || true)"
  scale="$(grep -oE '"scale"[[:space:]]*:[[:space:]]*[0-9.]+' "$TMP" | head -1 | grep -oE '[0-9.]+$' || true)"
  [ -n "$path" ] || { echo "capture: no screenshot (try $try) — is the window visible?" >&2; sleep 1; continue; }
  if awk -v a="${scale:-0}" -v b="$best_scale" 'BEGIN{exit !(a>b)}'; then best_scale="${scale:-0}"; best_path="$path"; fi
  if awk -v s="${scale:-0}" -v m="$MIN_SCALE" 'BEGIN{exit !(s>=m)}'; then
    cp "$path" "$OUT"
    echo "capture: retina ok (scale $scale) try $try -> ${OUT##*/}"
    crop_here
    exit 0
  fi
  echo "capture: low-res (scale ${scale:-?}) try $try — refocusing…" >&2
  sleep 1
done

[ -n "$best_path" ] || die 1 "no capture at all — check Orca screen-recording permission and that the window is visible"
cp "$best_path" "$OUT"
echo "capture: gave up after $TRIES tries; saved best (scale $best_scale). Un-fullscreen the window, move it to the Retina display, and dismiss the screensaver, then retry." >&2
exit 1
