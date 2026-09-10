#!/bin/sh
# PRD #1171 m5 — in-container PACKAGING proof for the Codex adapter's baked binaries.
#
# This is the deterministic half of the m3b image proof (the lifecycle suite is the other
# half, run by run-lifecycle.sh). It asserts the things the m5 Dockerfile change installs and
# that the packaged CodexExecutor resolves by ABSOLUTE path at runtime:
#
#   * the static process supervisor  /usr/local/bin/uzi-codex-supervisor  root-owned 0555
#   * the static openat2 fileop helper /usr/local/bin/uzi-codex-fileop     root-owned 0555
#   * the static Landlock command wrapper /usr/local/bin/uzi-codex-command-sandbox root-owned 0555
#   * node + tsx + the pinned Codex app-server are present
#   * the packaged CodexExecutor source's FILEOP_BIN constant == the installed fileop path
#
# It never edits or bypasses anything; every check reads real on-disk state. Run through the
# real root entrypoint (writable-root posture) so the ownership it proves is the true posture.
set -u

SUP=/usr/local/bin/uzi-codex-supervisor
FILEOP=/usr/local/bin/uzi-codex-fileop
SANDBOX=/usr/local/bin/uzi-codex-command-sandbox
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
  # dependency would break the trust-anchor posture. This control must FAIL (not `note`)
  # without positive static-link evidence: a non-static result, and the absence of BOTH
  # `file` and `ldd`, each increment FAIL so the proof cannot read green unproven.
  if command -v file >/dev/null 2>&1; then
    _f="$(file -b "$_p" 2>/dev/null || echo '?')"
    case "$_f" in
      *statically\ linked*) ok "$_p is statically linked" ;;
      *) bad "$_p is not statically linked: $_f" ;;
    esac
  elif command -v ldd >/dev/null 2>&1; then
    # ldd on a STATIC binary: glibc prints "not a dynamic executable"; musl prints
    # "Not a valid dynamic program" (or errors). A DYNAMIC binary lists a loader / "=>".
    _l="$(ldd "$_p" 2>&1 || true)"
    case "$_l" in
      # Alpine's ldd prefixes its static-binary verdict with the musl loader path, so
      # match that positive verdict BEFORE the generic dynamic-loader indicators.
      *"not a dynamic executable"*|*"Not a valid dynamic program"*) ok "$_p is statically linked (ldd)" ;;
      *"=>"*|*ld-musl*|*ld-linux*) bad "$_p is not statically linked (ldd reported a dynamic loader)" ;;
      *) bad "$_p static-link unproven by ldd: $_l" ;;
    esac
  else
    bad "cannot prove $_p is statically linked: neither file nor ldd is present"
  fi
}

hdr "PRD 1171 m5 packaging controls START (uid=$(id -u) gid=$(id -g))"

hdr "Control P1: the static process supervisor"
check_static_bin "$SUP"

hdr "Control P2: the static openat2 fileop helper (the m5 Dockerfile install)"
check_static_bin "$FILEOP"

hdr "Control P2b: the static Landlock command wrapper"
check_static_bin "$SANDBOX"

hdr "Control P3: node + tsx + the pinned Codex app-server"
if [ -x "$NODE" ]; then ok "node present ($NODE)"; else bad "node missing at $NODE"; fi
if [ -x "$TSX" ]; then ok "tsx present ($TSX)"; else bad "tsx missing at $TSX"; fi
if [ -x "$CODEX" ]; then ok "codex present ($CODEX)"; else bad "codex missing at $CODEX"; fi

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

if grep -q 'COMMAND_SANDBOX_BIN = "/usr/local/bin/uzi-codex-command-sandbox"' "$EXECUTOR_SRC"; then
  ok "packaged codex-executor.ts pins COMMAND_SANDBOX_BIN to $SANDBOX"
else
  bad "packaged codex-executor.ts COMMAND_SANDBOX_BIN does not match $SANDBOX"
fi

hdr "Control P5: dedicated managed-auth session reader group"
_runner_groups=" $(id -Gn runner 2>/dev/null || true) "
_worker_groups=" $(id -Gn worker 2>/dev/null || true) "
_command_groups=" $(id -Gn runner-cmd 2>/dev/null || true) "
case "$_runner_groups" in
  *" codex-session "*) ok "provider runner belongs to the dedicated session group" ;;
  *) bad "provider runner is missing the dedicated session group" ;;
esac
case "$_worker_groups" in
  *" codex-session "*) ok "worker belongs to the dedicated session group" ;;
  *) bad "worker is missing the dedicated session group" ;;
esac
case "$_command_groups" in
  *" codex-session "*) bad "command runner must not belong to the dedicated session group" ;;
  *) ok "command runner is excluded from the dedicated session group" ;;
esac
case "$_runner_groups" in
  *" worker "*) bad "provider runner must not belong to the broad worker group" ;;
  *) ok "provider runner is excluded from the broad worker group" ;;
esac

hdr "SUMMARY packaging PASS=$PASS FAIL=$FAIL"
[ "$FAIL" -eq 0 ]
