#!/usr/bin/env bash
# Download, verify fail-closed, and install the pinned Codex native package
# (PRD #1156 M3a) into a stable, immutable, root-owned root outside any writable
# tree. Consumed IDENTICALLY by templates/base and templates/jvm — the ONE place
# the packaging + integrity check lives, so the two images can never drift.
#
# The pin (version, tag, per-arch artifact + SHA256, expected members and expected
# codex-package.json fields) lives in the sibling codex-package.lock; this script
# only enforces it. It installs the COMPLETE package unflattened (bin/codex plus its
# bin/codex-code-mode-host sibling, resources and metadata), because the real
# code-mode path needs the host next to the CLI. It adds NOTHING to PATH: the
# launcher resolves Codex by absolute path only (a bundled codex-path/rg or resource
# shell must never land on Claude's PATH).
#
# Usage:   install-codex.sh <TARGETARCH>        # or TARGETARCH env
#   TARGETARCH        amd64 | arm64 (rejected loudly if missing/unknown)
# Env overrides (defaults are the REAL production install; overrides let a rootless
# test harness exercise the same code path without a network or root):
#   UZI_CODEX_LOCK      path to the lock            (default: sibling codex-package.lock)
#   UZI_CODEX_PREFIX    install root                (default: /opt/uzi-codex)
#   UZI_CODEX_ARTIFACT  pre-downloaded tarball      (default: download from the pin)
# The package installs to ${UZI_CODEX_PREFIX}/${CODEX_VERSION}/ preserving layout.

set -euo pipefail

log()  { printf 'install-codex: %s\n' "$*" >&2; }
die()  { printf 'install-codex: FATAL: %s\n' "$*" >&2; exit 1; }

SCRIPT_DIR="$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)"
LOCK="${UZI_CODEX_LOCK:-${SCRIPT_DIR}/codex-package.lock}"
PREFIX="${UZI_CODEX_PREFIX:-/opt/uzi-codex}"

[ -r "$LOCK" ] || die "cannot read lock at ${LOCK}"

# --- lock accessors ----------------------------------------------------------------
# Extract a KEY=VALUE line's value. A missing key is fatal (fail closed), which is
# exactly how an unknown TARGETARCH is rejected: its arch-suffixed key does not exist.
lock_val() {
  local key="$1" line
  line="$(grep -E "^${key}=" "$LOCK" | head -n1 || true)"
  [ -n "$line" ] || die "missing '${key}' in lock ${LOCK}"
  printf '%s' "${line#*=}"
}

# --- 1. arch mapping (reject missing/unknown loudly) -------------------------------
TARGETARCH="${TARGETARCH:-${1:-}}"
[ -n "$TARGETARCH" ] || die "TARGETARCH is unset — pass amd64|arm64 (env or \$1)"
case "$TARGETARCH" in
  amd64|arm64) : ;;
  *) die "unsupported TARGETARCH '${TARGETARCH}' — expected amd64 or arm64" ;;
esac

CODEX_VERSION="$(lock_val CODEX_VERSION)"
CODEX_TAG="$(lock_val CODEX_TAG)"
BASE_URL="$(lock_val CODEX_BASE_URL)"
MUSL_TARGET="$(lock_val "CODEX_TARGET_${TARGETARCH}")"
ARTIFACT="$(lock_val "CODEX_ARTIFACT_${TARGETARCH}")"
EXPECTED_SHA="$(lock_val "CODEX_SHA256_${TARGETARCH}")"

# The pin must be a real 64-hex digest; an empty/garbage value must FAIL, never pass
# a checksum step by matching nothing.
case "$EXPECTED_SHA" in
  [0-9a-f]*) [ "${#EXPECTED_SHA}" -eq 64 ] || die "SHA256 pin for ${TARGETARCH} is not 64 hex chars" ;;
  *) die "SHA256 pin for ${TARGETARCH} is empty or malformed" ;;
esac

DEST="${PREFIX}/${CODEX_VERSION}"
log "installing Codex ${CODEX_VERSION} (${TARGETARCH} -> ${MUSL_TARGET}) into ${DEST}"

# --- workspace + cleanup -----------------------------------------------------------
WORK="$(mktemp -d)"
DOWNLOADED=""
cleanup() {
  # Remove all temp/probe state; never leave a partial download or extract behind.
  rm -rf "$WORK"
  [ -n "$DOWNLOADED" ] && rm -f "$DOWNLOADED"
  return 0
}
trap cleanup EXIT

# --- 2. obtain the artifact (pre-downloaded override, else pinned download) ---------
if [ -n "${UZI_CODEX_ARTIFACT:-}" ]; then
  [ -r "$UZI_CODEX_ARTIFACT" ] || die "UZI_CODEX_ARTIFACT '${UZI_CODEX_ARTIFACT}' is not readable"
  TARBALL="$UZI_CODEX_ARTIFACT"
  log "using pre-downloaded artifact ${TARBALL}"
else
  TARBALL="${WORK}/${ARTIFACT}"
  DOWNLOADED="$TARBALL"
  URL="${BASE_URL}/${CODEX_TAG}/${ARTIFACT}"
  log "downloading ${URL}"
  curl -fsSL --retry 5 --retry-all-errors --retry-delay 2 --connect-timeout 15 -o "$TARBALL" "$URL" \
    || die "download failed: ${URL}"
fi

# --- 3. verify SHA256 fail-closed --------------------------------------------------
# Explicit compare AND the `sha256sum -c` mirror (Dockerfile:115-122). Both fail on a
# mismatch; neither can pass on empty input (EXPECTED_SHA validated non-empty above).
ACTUAL_SHA="$(sha256sum "$TARBALL" | awk '{print $1}')"
[ -n "$ACTUAL_SHA" ] || die "could not compute SHA256 of ${TARBALL}"
if [ "$ACTUAL_SHA" != "$EXPECTED_SHA" ]; then
  die "SHA256 mismatch for ${ARTIFACT}: expected ${EXPECTED_SHA}, got ${ACTUAL_SHA}"
fi
printf '%s  %s\n' "$EXPECTED_SHA" "$TARBALL" | sha256sum -c - >/dev/null \
  || die "SHA256 verification (sha256sum -c) failed for ${ARTIFACT}"
log "SHA256 OK (${EXPECTED_SHA})"

# --- 4. extract to a staging dir and verify BEFORE installing ----------------------
# --no-same-owner: extract as the current uid deterministically (a root extract would
# otherwise restore the archive's own uid); ownership is normalized to root below.
STAGE="${WORK}/stage"
mkdir -p "$STAGE"
tar -xzf "$TARBALL" -C "$STAGE" --no-same-owner || die "extract failed for ${ARTIFACT}"

# 4a. every expected member must exist.
members="$(lock_val CODEX_MEMBERS)"
read -r -a member_arr <<< "$members"
[ "${#member_arr[@]}" -gt 0 ] || die "lock CODEX_MEMBERS is empty"
for m in "${member_arr[@]}"; do
  [ -e "${STAGE}/${m}" ] || die "expected member missing from package: ${m}"
done

# 4b. the CLI + its code-mode host sibling must be present and executable.
[ -x "${STAGE}/bin/codex" ] || die "bin/codex is missing or not executable"
[ -x "${STAGE}/bin/codex-code-mode-host" ] || die "bin/codex-code-mode-host is missing or not executable"

# 4c. codex-package.json fields must equal the lock's expected values (fail closed).
MANIFEST="${STAGE}/codex-package.json"
[ -r "$MANIFEST" ] || die "codex-package.json missing from package"

# Extract a top-level JSON field value ("KEY": "str" | "KEY": num) without a JSON
# parser (the minimal image ships none). Returns empty if absent -> assert_field dies.
manifest_field() {
  local key="$1" raw
  raw="$(grep -oE "\"${key}\"[[:space:]]*:[[:space:]]*(\"[^\"]*\"|[0-9]+)" "$MANIFEST" | head -n1 || true)"
  # strip  "key" :  prefix, then surrounding quotes if any.
  raw="${raw#*:}"
  raw="${raw#"${raw%%[![:space:]]*}"}"   # ltrim
  raw="${raw%\"}"; raw="${raw#\"}"
  printf '%s' "$raw"
}
assert_field() {
  local key="$1" want="$2" got
  got="$(manifest_field "$key")"
  [ "$got" = "$want" ] || die "codex-package.json ${key}='${got}', expected '${want}'"
}
assert_field layoutVersion "$(lock_val CODEX_MANIFEST_LAYOUT_VERSION)"
assert_field version       "$(lock_val CODEX_MANIFEST_VERSION)"
assert_field variant       "$(lock_val CODEX_MANIFEST_VARIANT)"
assert_field entrypoint    "$(lock_val CODEX_MANIFEST_ENTRYPOINT)"
assert_field resourcesDir  "$(lock_val CODEX_MANIFEST_RESOURCES_DIR)"
assert_field pathDir       "$(lock_val CODEX_MANIFEST_PATH_DIR)"
# target must equal THIS arch's musl target (the artifact is for this architecture).
assert_field target        "$MUSL_TARGET"
log "package verified: ${#member_arr[@]} members, manifest fields match, target=${MUSL_TARGET}"

# --- 5. install unflattened, root-owned, world-readable/executable -----------------
# Fresh, deterministic install root for this version (idempotent re-install). The
# version dir is immutable across versions; we only ever replace the SAME version.
rm -rf "$DEST"
mkdir -p "$DEST"
# Copy each top-level member into DEST preserving its mode (cp -a keeps the 0755 exec
# bits; the whole tree copies unflattened). Copy the CHILDREN, never `cp -a "$STAGE"/.`
# — that reapplies the staging dir's own mode to DEST, which would leak a setgid bit from
# a setgid mktemp parent (e.g. a setgid /tmp) onto the install root.
find "$STAGE" -mindepth 1 -maxdepth 1 -exec cp -a {} "$DEST"/ \;

# Root-owned when we are root (the real build/probe path); a rootless test harness
# keeps its own ownership (chown would EPERM) — the a+rX below is what a runner-uid
# exec actually needs, and it always succeeds on files we own.
if [ "$(id -u)" = "0" ]; then
  chown -R 0:0 "$DEST" || die "chown -R root:root ${DEST} failed"
else
  log "not root — skipping chown (rootless test install)"
fi
# Normalize to a DETERMINISTIC mode: first strip any setuid/setgid/sticky bit tar
# recreated on the staged dirs when the build TMPDIR was itself setgid (node:24-alpine
# /tmp and some CI TMPDIRs are), so the install root does not depend on the builder's
# tmp mode; THEN add the world read/traverse bits a runner-uid exec needs.
find "$DEST" -exec chmod u-s,g-s,o-t {} + || die "chmod (clear setid) ${DEST} failed"
chmod -R a+rX "$DEST" || die "chmod -R a+rX ${DEST} failed"

# --- 6. done -----------------------------------------------------------------------
log "installed Codex ${CODEX_VERSION} at ${DEST} (bin/codex + bin/codex-code-mode-host, not on PATH)"
