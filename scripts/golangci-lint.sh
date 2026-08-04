#!/bin/sh
# Acquire the PINNED golangci-lint RELEASE BINARY (sha256-verified) and exec it.
# PRD #230 M5.
#
# WHY THIS REPLACES `go run .../golangci-lint@vX.Y.Z`. That form COMPILES the
# linter from source on a cold GOCACHE (~51s, measured — the pipeline's cold long
# pole, lint:controller 564.8s on MR !157). A downloaded release binary skips the
# compile entirely. It stays byte-identical local and in CI (PRD #103 SC-1):
# BOTH acquire the SAME pinned artifact at the SAME version, so `task lint:api` /
# `lint:controller` produce identical findings either place — the linter VERSION,
# not its build-Go, decides which rules fire (see the D6 guard below for the one
# thing build-Go does decide).
#
# Usage:  scripts/golangci-lint.sh <version> <golangci-lint args...>
#   e.g.  scripts/golangci-lint.sh v2.12.2 run ./...
#         scripts/golangci-lint.sh v2.12.2 config verify
#         scripts/golangci-lint.sh v2.12.2 cache clean
#
# The version is an ARGUMENT, not a script default, so `task`'s echo still shows
# the pin — the same convention scripts/deadcode-gate.sh and validate:api's sqlc
# pin use, and the mechanism the Taskfile header calls out for noticing a pin
# going missing.
set -eu

VERSION="${1:-}"
if [ -z "$VERSION" ]; then
  echo "usage: scripts/golangci-lint.sh <version> <args...>" >&2
  echo "  e.g. scripts/golangci-lint.sh v2.12.2 run ./..." >&2
  exit 2
fi
shift
VER_NO_V="${VERSION#v}"

# --- Per-arch pinned artifacts + INLINED sha256 (TARBALL and BINARY) ---------
# TWO checksums per arch, both INLINED, never a variable (a variable is
# displaceable by a manual pipeline; .task_setup makes exactly this argument for
# TASK_SHA256):
#   * tar_sha256 — the release TARBALL GitHub serves, verified BEFORE extraction
#     (this is what .task_setup does; it re-downloads every CI job and caches
#     nothing, so a tarball check is all it needs);
#   * bin_sha256 — the EXTRACTED binary, verified on EVERY run below so a cache
#     HIT re-verifies rather than trusting whatever sits at $BIN. .task_setup has
#     no cache and therefore no cache-hit path; this script does (for local
#     reuse), so an unverified hit would exec a corrupt or PLANTED binary — the
#     cache-poisoning shape M6b was dropped to avoid. Pinning the binary hash here
#     makes safety a SCRIPT property (this literal), not an env property (the
#     cache dir an attacker could write). This is the "exceed" half of matching
#     .task_setup's precedent.
# Each value was derived by hashing the artifact GitHub serves; the tarball values
# are additionally cross-checked against golangci-lint-<ver>-checksums.txt (a
# checksum FILE from the same release is not an independent check, so the artifact
# is the source of truth).
#
# 🔴 A SECOND ARCH NEEDS A SECOND PAIR derived against ITS OWN artifacts — one sha
# across two arches is a guaranteed mismatch whose tempting "fix" is to delete the
# check, which is the whole control. Only darwin/arm64 (the dev host) and
# linux/amd64 (both shared runners; kaniko cannot cross-build either) are pinned;
# any other host fails LOUDLY below rather than silently.
os="$(uname -s)"
arch="$(uname -m)"
case "$os/$arch" in
  Darwin/arm64)
    asset="darwin-arm64"
    tar_sha256="a9c54498731b3128f79e090be6110f3e5fffccc617b08142ed244d4126c73f29"
    bin_sha256="691b9100ce968ff0009b6b7757ef6a585e31ae9ab11dfe0340ebb6e8e21fdc3d"
    ;;
  Linux/x86_64 | Linux/amd64)
    asset="linux-amd64"
    tar_sha256="8df580d2670fed8fa984aac0507099af8df275e665215f5c7a2ae3943893a553"
    bin_sha256="e26335d9bd381a60e5769a13b0ccc7967db5b6fb9c39a896a1f6fd0befe0a661"
    ;;
  *)
    echo "golangci-lint.sh: unsupported host $os/$arch" >&2
    echo "  Only darwin/arm64 and linux/amd64 are pinned. Adding one needs a" >&2
    echo "  tarball+binary checksum pair derived against that arch's artifacts." >&2
    exit 2
    ;;
esac

# sha256 verification with a macOS FALLBACK. coreutils `sha256sum` is not on a
# stock macOS (which ships `shasum`), and this script — unlike .task_setup, which
# runs only in linux CI — ALSO runs on the dev mac. Both tools accept the
# identical "<hex>  <path>" line on stdin under `-c -` and exit nonzero on
# mismatch, so the discriminator is which one exists. (openssl dgst is not used:
# its output format differs and would need reformatting to feed `-c`.)
verify_sha256() {  # $1 = expected hex, $2 = file; returns the check's status
  if command -v sha256sum >/dev/null 2>&1; then
    printf '%s  %s\n' "$1" "$2" | sha256sum -c - >/dev/null
  elif command -v shasum >/dev/null 2>&1; then
    printf '%s  %s\n' "$1" "$2" | shasum -a 256 -c - >/dev/null
  else
    echo "golangci-lint.sh: no sha256 tool found (need sha256sum or shasum)" >&2
    return 2
  fi
}

# The pinned checksums are for 2.12.2 ONLY. Refuse any other version rather than
# run an old checksum against a new artifact (a guaranteed sha256sum failure, but
# fail here naming the reason). Bumping the pin means replacing the version in
# Taskfile.yml AND both checksum literals above, together.
if [ "$VER_NO_V" != "2.12.2" ]; then
  echo "golangci-lint.sh: pinned checksums are for 2.12.2, got '$VER_NO_V'." >&2
  echo "  Update the version pin (Taskfile.yml) AND both per-arch tar+bin sha256" >&2
  echo "  pairs in this script together — they are one pinned set." >&2
  exit 2
fi

# --- Cache location: DELIBERATELY OUTSIDE $CI_PROJECT_DIR ---------------------
# Under $HOME/.cache, NOT the project tree, so GitLab's per-lockfile `cache:`
# (which only persists paths under $CI_PROJECT_DIR) never carries this binary
# across the MR/protected trust boundary. That is one half of the issue #211
# poisoning vector M6b was dropped to avoid; the OTHER half — a cache HIT serving
# an unverified binary — is closed by re-verifying $BIN against bin_sha256 on
# EVERY run below, not just on a miss. So CI re-downloads+verifies once per job
# (~1-2s, vs the 51s compile it replaces) and a local reuse re-verifies (~0.2s to
# hash the ~40 MB binary) before every gate.
CACHE_ROOT="${UZI_GOLANGCI_LINT_DIR:-${XDG_CACHE_HOME:-$HOME/.cache}/uzi-golangci-lint}"
DIR="$CACHE_ROOT/$VER_NO_V/$asset"
BIN="$DIR/golangci-lint"

# (Re)install when $BIN is missing OR fails its pinned binary checksum. A cached
# binary that does not match — corrupt, truncated, or PLANTED — is removed and
# never exec'd; the block below rebuilds it from the verified tarball. Offline,
# the re-download fails loudly (curl -f under `set -e`) rather than the bad binary
# running. The `verify_sha256` call is in an `if` condition, so its nonzero status
# on a mismatch drives the branch instead of aborting under errexit.
if [ ! -x "$BIN" ] || ! verify_sha256 "$bin_sha256" "$BIN"; then
  rm -f "$BIN"
  mkdir -p "$DIR"
  TMP="$(mktemp -d)"
  # shellcheck disable=SC2064  # expand TMP now so the trap survives its unset.
  trap "rm -rf '$TMP'" EXIT INT TERM
  url="https://github.com/golangci/golangci-lint/releases/download/v${VER_NO_V}/golangci-lint-${VER_NO_V}-${asset}.tar.gz"
  # curl on golang:1.26 and darwin; wget as a fallback for a curl-less image
  # (mirrors .task_setup's discriminator). The lint jobs run on golang:1.26,
  # which has curl.
  if command -v curl >/dev/null 2>&1; then
    curl -fsSLo "$TMP/gcl.tar.gz" "$url" || { echo "golangci-lint.sh: download failed: $url" >&2; exit 2; }
  else
    wget -qO "$TMP/gcl.tar.gz" "$url" || { echo "golangci-lint.sh: download failed: $url" >&2; exit 2; }
  fi
  # Verify the TARBALL before extraction (matches .task_setup), then the extracted
  # BINARY before it is moved into place — so a bad artifact never reaches $BIN.
  verify_sha256 "$tar_sha256" "$TMP/gcl.tar.gz" \
    || { echo "golangci-lint.sh: tarball checksum mismatch for $asset (expected $tar_sha256)" >&2; exit 2; }
  # The tarball extracts to golangci-lint-<ver>-<asset>/golangci-lint.
  tar -xzf "$TMP/gcl.tar.gz" -C "$TMP"
  verify_sha256 "$bin_sha256" "$TMP/golangci-lint-${VER_NO_V}-${asset}/golangci-lint" \
    || { echo "golangci-lint.sh: extracted binary checksum mismatch for $asset (expected $bin_sha256)" >&2; exit 2; }
  mv "$TMP/golangci-lint-${VER_NO_V}-${asset}/golangci-lint" "$BIN"
  chmod +x "$BIN"
fi

# --- D6 build-Go GUARD (PRD #230 M5 / D6) ------------------------------------
# golangci-lint can only lint code whose Go LANGUAGE version is <= the Go it was
# built with (golangci-lint #5641/#6272, FAQ). A downloaded release binary carries
# a FROZEN build-Go (2.12.2 => go1.26.2, language 1.26). If EITHER Go module's
# `go` directive language rises above it, the linter emits typecheck errors
# instead of findings — a DIFFERENT result, which SC-1 forbids. golangci-lint's
# own failure on this is already loud (exit 3), so this guard is the CHEAP,
# earlier form D6 asks for: it names the pin and the directive before lint runs.
# The risk is BIDIRECTIONAL (a module bumping up trips it just as a linter
# regressing down would), so it is checked at gate time against the module in the
# current directory — each lint recipe runs with `dir:` set to its module.
if [ -f go.mod ]; then
  # Read build-Go from the BINARY'S OWN `version` output ("... built with go1.26.2
  # ..."), NOT `go version <binary>`. The latter is the `go` COMMAND, which honors
  # the CWD go.mod's toolchain directive: in a module whose directive is a Go the
  # host lacks, it tries to DOWNLOAD that toolchain, fails, prints nothing, and the
  # guard skips silently — the exact case this guard exists to catch. The binary
  # reporting itself is unaffected by any toolchain selection.
  build_go="$("$BIN" version 2>/dev/null | awk '{for (i = 1; i <= NF; i++) if ($i ~ /^go[0-9]/) { sub(/^go/, "", $i); print $i; exit }}')"
  mod_go="$(awk '$1 == "go" { print $2; exit }' go.mod)"
  # Compare LANGUAGE version (major.minor) numerically; patch is ignored, because
  # the gate golangci-lint itself applies is on language (1.26.4's language is
  # 1.26). If either value fails to parse, skip rather than fail spuriously — the
  # intrinsic exit-3 still covers the real mismatch.
  case "$build_go.$mod_go" in
    [0-9]*.[0-9]*.[0-9]* | [0-9]*.[0-9]*)
      b_maj="${build_go%%.*}"
      b_rest="${build_go#*.}"
      b_min="${b_rest%%.*}"
      m_maj="${mod_go%%.*}"
      m_rest="${mod_go#*.}"
      m_min="${m_rest%%.*}"
      if [ "$b_maj" -lt "$m_maj" ] || { [ "$b_maj" -eq "$m_maj" ] && [ "$b_min" -lt "$m_min" ]; }; then
        echo "golangci-lint.sh: pinned binary build-Go language ${b_maj}.${b_min} (go${build_go}) is BELOW" >&2
        echo "  this module's go directive language ${m_maj}.${m_min} (go ${mod_go}, $(pwd)/go.mod)." >&2
        echo "  golangci-lint would emit typecheck errors instead of findings" >&2
        echo "  (golangci-lint #5641/#6272), diverging local/CI from this pin." >&2
        echo "  Bump the pin to a release built with go>=${m_maj}.${m_min}: update the" >&2
        echo "  version and both per-arch tar+bin sha256 pairs in this script (PRD #230 D6)." >&2
        exit 2
      fi
      ;;
  esac
fi

exec "$BIN" "$@"
