#!/usr/bin/env bash
# capture.sh — screenshot a desktop browser window at native RETINA resolution
# using macOS `screencapture -R` on the window's on-screen rectangle. This grabs
# the screen region directly, so it does NOT suffer Orca's low-res desktop-region
# fallback (which downsamples to ~1280px whenever the window is not the frontmost
# app). It only needs the window brought forward and unoccluded.
#
# Optionally navigates to a URL first (in a new tab, via Orca computer-use) and
# resizes the window to a fixed size, so framing is reproducible. Pair with
# crop-ui.sh to strip the browser chrome.
#
# Usage:
#   capture.sh --app <selector> <output.png> [--url URL] [--size WxH] [--crop] [--width N]
#
#   --app <selector>  App bundle id (app.zen-browser.zen, com.google.Chrome,
#                     com.apple.Safari, com.microsoft.edgemac) or app name. Required.
#   --url URL         open URL in a NEW tab first (via Orca), then capture. The uzi
#                     instance URL goes here, never hardcoded. Needs Orca computer-use.
#   --size WxH        resize the window to WxH points before capturing (osascript),
#                     for reproducible framing. e.g. --size 1728x1080.
#   --crop            run crop-ui.sh on the result (in-place) to strip chrome.
#   --width N         with --crop, downscale the crop to N px wide (e.g. 1800).
#
# macOS only. Needs Screen Recording permission for the terminal (screencapture)
# and Automation permission for System Events (window rect + resize). Orca is only
# required when --url is used; its binary is $ORCA_CLI_COMMAND if set, else `orca`.
#
# Exit: 0 ok; 2 usage; 4 tool missing; 5 could not read the window rectangle.
set -euo pipefail

die() { echo "capture: $2" >&2; exit "$1"; }
command -v screencapture >/dev/null 2>&1 || die 4 "screencapture not found (macOS only)"
command -v osascript >/dev/null 2>&1 || die 4 "osascript not found (macOS only)"

APP=""; OUT=""; URL=""; SIZE=""; DOCROP=0; WIDTH=""
while [ $# -gt 0 ]; do
  case "$1" in
    --app)   APP="${2:-}"; shift 2 ;;
    --url)   URL="${2:-}"; shift 2 ;;
    --size)  SIZE="${2:-}"; shift 2 ;;
    --crop)  DOCROP=1; shift ;;
    --width) WIDTH="${2:-}"; shift 2 ;;
    -*) die 2 "unknown flag: $1" ;;
    *) if [ -z "$OUT" ]; then OUT="$1"; else die 2 "too many args"; fi; shift ;;
  esac
done
[ -n "$APP" ] || die 2 "missing --app <selector>"
[ -n "$OUT" ] || die 2 "usage: capture.sh --app <selector> <output.png> [--url URL] [--size WxH]"

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"

# Resolve the System Events process name from an app bundle id or plain name.
proc_name() {
  case "$APP" in
    *.*) osascript -e "tell application \"System Events\" to get name of first process whose bundle identifier is \"$APP\"" 2>/dev/null || echo "$APP" ;;
    *)   echo "$APP" ;;
  esac
}

activate_app() {
  case "$APP" in
    *.*) osascript -e "tell application id \"$APP\" to activate" 2>/dev/null || true ;;
    *)   osascript -e "tell application \"$APP\" to activate" 2>/dev/null || true ;;
  esac
}

# Open URL in a new tab via Orca (leaves existing tabs untouched).
navigate() {
  local ORCA="${ORCA_CLI_COMMAND:-orca}"
  command -v "$ORCA" >/dev/null 2>&1 || die 4 "--url needs the Orca CLI '$ORCA' (set ORCA_CLI_COMMAND or install Orca)"
  # A failed nav step can leave the old tab in place, so warn loudly rather than
  # swallow it; the caller still visually verifies the shot before publishing.
  "$ORCA" computer get-app-state --app "$APP" --restore-window --no-screenshot --json >/dev/null 2>&1 || true
  "$ORCA" computer hotkey --app "$APP" --key "CmdOrCtrl+T" --restore-window --no-screenshot --json >/dev/null 2>&1 \
    || echo "capture: new-tab step failed; the captured page may be the wrong one" >&2
  sleep 0.8
  "$ORCA" computer type-text --app "$APP" --text "$URL" --restore-window --no-screenshot --json >/dev/null 2>&1 \
    || echo "capture: typing the URL failed; the captured page may be the wrong one" >&2
  "$ORCA" computer press-key --app "$APP" --key Return --restore-window --no-screenshot --json >/dev/null 2>&1 \
    || echo "capture: submitting the URL failed; the captured page may be the wrong one" >&2
  sleep 2.5
}

resize_window() {
  local w="${SIZE%x*}" h="${SIZE#*x}" proc
  case "$SIZE" in *x*) ;; *) echo "capture: --size must be WxH (e.g. 1728x1080); skipping resize" >&2; return 0 ;; esac
  proc="$(proc_name)"
  osascript -e "tell application \"System Events\" to tell process \"$proc\" to set size of front window to {$w, $h}" 2>/dev/null \
    || echo "capture: resize failed (grant the terminal Automation permission for System Events)" >&2
}

crop_here() {
  [ "$DOCROP" -eq 1 ] || return 0
  local args=("$OUT" "$OUT"); [ -n "$WIDTH" ] && args+=(--width "$WIDTH")
  "$SCRIPT_DIR/crop-ui.sh" "${args[@]}"
}

# Use if-blocks, not `cond && func`: the `&&` form disables `set -e` inside the
# called function for the rest of its body (a bash errexit footgun).
if [ -n "$URL" ]; then navigate; fi
if [ -n "$SIZE" ]; then resize_window; fi
activate_app
sleep 0.5

# Read the front window's on-screen rectangle (points) and capture just that region.
PROC="$(proc_name)"
rect="$(osascript -e "tell application \"System Events\" to tell process \"$PROC\" to get {position, size} of front window" 2>/dev/null || true)"
# rect looks like: "0, 30, 1728, 1080"  (x, y, w, h)
rect="${rect//,/}"
read -r X Y W H <<<"$rect"
{ [ -n "${X:-}" ] && [ -n "${Y:-}" ] && [ -n "${W:-}" ] && [ -n "${H:-}" ]; } \
  || die 5 "could not read the '$PROC' front-window rectangle (grant Automation permission, and make sure it has a window)"

screencapture -x -R "${X},${Y},${W},${H}" "$OUT"
crop_here
read -r OW OH < <(magick identify -format '%w %h\n' "$OUT" 2>/dev/null || echo "? ?")
echo "capture: ${PROC} window ${W}x${H}@${X},${Y} -> ${OUT##*/} ${OW}x${OH}"
# Privacy fail-safe: the script cannot read the browser's demo-mode flag, so it
# cannot prove masking is active. Require a human/agent to confirm before publishing.
echo "capture: VERIFY demo-mode masking in ${OUT##*/} before publishing — no real email, repo owner, or forge host." >&2
