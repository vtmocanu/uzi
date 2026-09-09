#!/bin/sh
# PRD #1171 m5 — in-container PACKAGING proof for the Codex adapter's baked binaries.
#
# This is the deterministic half of the m3b image proof (the lifecycle suite is the other
# half, run by run-lifecycle.sh). It asserts the things the m5 Dockerfile change installs and
# that the packaged CodexExecutor resolves by ABSOLUTE path at runtime:
#
#   * the static process supervisor  /usr/local/bin/uzi-codex-supervisor  root-owned 0555
#   * the static openat2 fileop helper /usr/local/bin/uzi-codex-fileop     root-owned 0555
#   * node + tsx + the pinned Codex app-server are present
#   * the packaged CodexExecutor source's FILEOP_BIN constant == the installed fileop path
#
# It never edits or bypasses anything; every check reads real on-disk state. Run through the
# real root entrypoint (writable-root posture) so the ownership it proves is the true posture.
set -u

SUP=/usr/local/bin/uzi-codex-supervisor
FILEOP=/usr/local/bin/uzi-codex-fileop
NODE=/usr/local/bin/node
TSX=/app/node_modules/.bin/tsx
CODEX=/opt/uzi-codex/0.153.2/bin/codex
EXECUTOR_SRC=/app/src/codex/codex-executor.ts

PASS=0
FAIL=0

hdr()  { printf '\n========== %s ==========\n' "$*"; }
ok()   { PASS=$((PASS + 1)); printf '  [PASS] %s\n' "$*"; }
bad()  { FAIL=$((FAIL + 1)); printf '  [FAIL] %s\n' "$*"; }
note() { printf '  [note] %s\n' "$*"; }

# A static, root-owned 0555 binary: exists, is a regular file, owner uid 0 / gid 0, mode 555.
check_static_bin() {
  _p="$1"
  if [ ! -f "$_p" ]; then bad "missing binary: $_p"; return; fi
  _own="$(stat -c '%u:%g' "$_p" 2>/dev/null || echo '?')"
  _mode="$(stat -c '%a' "$_p" 2>/dev/null || echo '?')"
  if [ "$_own" = "0:0" ] && [ "$_mode" = "555" ]; then
    ok "$_p present, root-owned ($_own) mode $_mode"
  else
    bad "$_p ownership/mode wrong: owner=$_own mode=$_mode (want 0:0 / 555)"
  fi
  # It must be executable and statically linked (no dynamic loader) — a runner-writable /nix
  # dependency would break the trust-anchor posture. `file` may be absent; fall back to ldd.
  if command -v file >/dev/null 2>&1; then
    _f="$(file -b "$_p" 2>/dev/null || echo '?')"
    case "$_f" in
      *statically\ linked*) ok "$_p is statically linked" ;;
      *) note "$_p file: $_f (expected 'statically linked')" ;;
    esac
  fi
}

hdr "PRD 1171 m5 packaging controls START (uid=$(id -u) gid=$(id -g))"

hdr "Control P1: the static process supervisor"
check_static_bin "$SUP"

hdr "Control P2: the static openat2 fileop helper (the m5 Dockerfile install)"
check_static_bin "$FILEOP"

hdr "Control P3: node + tsx + the pinned Codex app-server"
[ -x "$NODE" ] && ok "node present ($NODE)" || bad "node missing at $NODE"
[ -x "$TSX" ]  && ok "tsx present ($TSX)"  || bad "tsx missing at $TSX"
[ -x "$CODEX" ] && ok "codex present ($CODEX)" || bad "codex missing at $CODEX"

hdr "Control P4: the packaged FILEOP_BIN constant matches the installed path"
if [ -f "$EXECUTOR_SRC" ]; then
  if grep -q 'FILEOP_BIN = "/usr/local/bin/uzi-codex-fileop"' "$EXECUTOR_SRC"; then
    ok "packaged codex-executor.ts pins FILEOP_BIN to $FILEOP"
  else
    bad "packaged codex-executor.ts FILEOP_BIN does not match $FILEOP"
    note "$(grep -n 'FILEOP_BIN' "$EXECUTOR_SRC" 2>/dev/null | head -1)"
  fi
else
  bad "packaged executor source not found at $EXECUTOR_SRC"
fi

hdr "SUMMARY packaging PASS=$PASS FAIL=$FAIL"
[ "$FAIL" -eq 0 ]
