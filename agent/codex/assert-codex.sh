#!/usr/bin/env bash
# Build-time runner-uid execution guard for the pinned Codex native package
# (PRD #1156 M3a). Runs INDEPENDENTLY of install-codex.sh (the install already
# happened) and proves the installed binaries actually EXECUTE under the exact
# unprivileged uid a run uses — not merely that files exist.
#
# It reproduces the run-time launch posture: the credential-less `runner` uid
# (setpriv --reuid, all caps cleared — modelled on agent/src/runner-uid.ts:74-94),
# a fresh throwaway HOME, a replaced environment (env -i) and a bounded timeout.
# Codex is a static-pie musl ELF, so no libc sniffing is needed; the fake-GNU-`ldd`
# PATH case (mirroring templates/base/Dockerfile:294-297) proves the pick stays
# correct even when a glibc `ldd` sits first on PATH, the condition a provisioned
# nix glibc.bin creates at run time.
#
# Direct runner-uid `codex --version` MUST print EXACTLY `codex-cli <version>` on
# stdout and exit 0; the matching code-mode host sibling must be present and
# executable. A corrupted binary, a missing host or a broken loader path fails HERE,
# at build time, instead of shipping a silent breakage into every run.
#
# Env overrides (defaults are the REAL production guard):
#   UZI_CODEX_LOCK          path to the lock  (default: sibling codex-package.lock)
#   UZI_CODEX_PREFIX        install root      (default: /opt/uzi-codex)
#   UZI_CODEX_RUNNER_USER   uid to drop to    (default: runner)

set -euo pipefail

log()  { printf 'assert-codex: %s\n' "$*" >&2; }
die()  { printf 'assert-codex: FATAL: %s\n' "$*" >&2; exit 1; }

SCRIPT_DIR="$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)"
LOCK="${UZI_CODEX_LOCK:-${SCRIPT_DIR}/codex-package.lock}"
PREFIX="${UZI_CODEX_PREFIX:-/opt/uzi-codex}"
RUNNER_USER="${UZI_CODEX_RUNNER_USER:-runner}"
SETPRIV="/bin/setpriv"
TIMEOUT=30

[ -r "$LOCK" ] || die "cannot read lock at ${LOCK}"

lock_val() {
  local key="$1" line
  line="$(grep -E "^${key}=" "$LOCK" | head -n1 || true)"
  [ -n "$line" ] || die "missing '${key}' in lock ${LOCK}"
  printf '%s' "${line#*=}"
}

CODEX_VERSION="$(lock_val CODEX_VERSION)"
DEST="${PREFIX}/${CODEX_VERSION}"
CODEX="${DEST}/bin/codex"
CODEX_HOST="${DEST}/bin/codex-code-mode-host"
EXPECTED_VERSION="codex-cli ${CODEX_VERSION}"

# --- 1. the CLI and its code-mode host sibling must exist and be executable --------
[ -e "$CODEX" ]       || die "codex not installed at ${CODEX}"
[ -x "$CODEX" ]       || die "codex at ${CODEX} is not executable"
[ -e "$CODEX_HOST" ]  || die "code-mode host sibling missing at ${CODEX_HOST}"
[ -x "$CODEX_HOST" ]  || die "code-mode host at ${CODEX_HOST} is not executable"

# --- throwaway probe state (fresh HOME per run, fake-ldd dir), cleaned on exit ------
PROBE="$(mktemp -d)"
cleanup() { rm -rf "$PROBE"; return 0; }
trap cleanup EXIT
# Make the probe root traversable so the runner uid can actually REACH the fresh HOME and
# the fake-ldd dir underneath it (mktemp -d is 0700 root-owned; a non-traversable parent
# would leave the "usable HOME" aspiration untrue). Throwaway, removed on exit.
chmod 0755 "$PROBE"

# Run `codex --version` as RUNNER_USER with a replaced env, a fresh world-usable HOME
# and an explicit PATH prefix, capturing stdout. Prints nothing to our stdout; sets
# the globals GUARD_OUT / GUARD_RC for the caller to assert on.
run_version_as_runner() {
  local path_prefix="$1" home
  home="$(mktemp -d "${PROBE}/home.XXXXXX")"
  # Fresh throwaway HOME any uid can use (root creates it 0700; the runner uid drops
  # into it). codex --version tolerates an inaccessible HOME, but a usable one keeps
  # the probe honest and leaves no cross-run residue.
  chmod 0777 "$home"
  GUARD_RC=0
  GUARD_OUT="$(
    "$SETPRIV" --reuid "$RUNNER_USER" --regid "$RUNNER_USER" --init-groups \
      --inh-caps -all --ambient-caps -all -- \
      env -i "HOME=${home}" "PATH=${path_prefix}/usr/bin:/bin" \
      timeout "$TIMEOUT" "$CODEX" --version 2>/dev/null
  )" || GUARD_RC=$?
}

# --- 2. positive control: plain runner-uid execution -------------------------------
run_version_as_runner ""
[ "$GUARD_RC" -eq 0 ] || die "runner-uid \`codex --version\` exited ${GUARD_RC} (expected 0)"
[ "$GUARD_OUT" = "$EXPECTED_VERSION" ] \
  || die "runner-uid \`codex --version\` printed '${GUARD_OUT}', expected '${EXPECTED_VERSION}'"
log "positive control OK: runner-uid codex --version -> '${GUARD_OUT}'"

# --- 3. fake-GNU-ldd PATH case (mirror Dockerfile:294-297) -------------------------
# A throwaway `ldd` that lies "GNU libc" first on PATH; the musl static-pie codex must
# still be picked and print the same version. Reddens the guard if selection ever
# becomes PATH/ldd-dependent.
FAKE_LDD_DIR="${PROBE}/fake-glibc"
mkdir -p "$FAKE_LDD_DIR"
printf '#!/bin/sh\necho "ldd (GNU libc) 2.42"\n' > "${FAKE_LDD_DIR}/ldd"
chmod 0755 "${FAKE_LDD_DIR}/ldd"
run_version_as_runner "${FAKE_LDD_DIR}:"
[ "$GUARD_RC" -eq 0 ] || die "runner-uid \`codex --version\` (fake glibc ldd) exited ${GUARD_RC} (expected 0)"
[ "$GUARD_OUT" = "$EXPECTED_VERSION" ] \
  || die "runner-uid \`codex --version\` (fake glibc ldd) printed '${GUARD_OUT}', expected '${EXPECTED_VERSION}'"
log "fake-ldd control OK: pick stays correct with a glibc ldd first on PATH"

log "guard passed: Codex ${CODEX_VERSION} executes as ${RUNNER_USER} (CLI + code-mode host present)"
