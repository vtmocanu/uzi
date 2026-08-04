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

# --- Per-arch pinned artifact + INLINED sha256 -------------------------------
# The checksum is the WHOLE control on this fetch and is INLINED, never a
# variable: a variable is displaceable by a manual pipeline, and .task_setup
# makes exactly this argument for TASK_SHA256. Each value was derived by hashing
# the artifact GitHub serves (shasum -a 256) and cross-checked against the
# release's golangci-lint-<ver>-checksums.txt — a checksum FILE from the same
# release is not an independent check, so the artifact is the source of truth.
#
# 🔴 A SECOND ARCH NEEDS A SECOND CHECKSUM LITERAL derived against ITS OWN
# artifact — one sha across two arches is a guaranteed mismatch whose tempting
# "fix" is to delete the check, which is the whole control. Only darwin/arm64
# (the dev host) and linux/amd64 (both shared runners; kaniko cannot cross-build
# either) are pinned; any other host fails LOUDLY below rather than silently.
os="$(uname -s)"
arch="$(uname -m)"
case "$os/$arch" in
  Darwin/arm64)
    asset="darwin-arm64"
    sha256="a9c54498731b3128f79e090be6110f3e5fffccc617b08142ed244d4126c73f29"
    ;;
  Linux/x86_64 | Linux/amd64)
    asset="linux-amd64"
    sha256="8df580d2670fed8fa984aac0507099af8df275e665215f5c7a2ae3943893a553"
    ;;
  *)
    echo "golangci-lint.sh: unsupported host $os/$arch" >&2
    echo "  Only darwin/arm64 and linux/amd64 are pinned. Adding one needs a" >&2
    echo "  checksum literal derived against that arch's artifact." >&2
    exit 2
    ;;
esac

# The pinned checksums are for 2.12.2 ONLY. Refuse any other version rather than
# run an old checksum against a new artifact (a guaranteed sha256sum failure, but
# fail here naming the reason). Bumping the pin means replacing the version in
# Taskfile.yml AND both checksum literals above, together.
if [ "$VER_NO_V" != "2.12.2" ]; then
  echo "golangci-lint.sh: pinned checksums are for 2.12.2, got '$VER_NO_V'." >&2
  echo "  Update the version pin (Taskfile.yml) AND the per-arch sha256 literals" >&2
  echo "  in this script together — they are one pinned set." >&2
  exit 2
fi

# --- Cache location: DELIBERATELY OUTSIDE $CI_PROJECT_DIR ---------------------
# Under $HOME/.cache, NOT the project tree, so GitLab's per-lockfile `cache:`
# (which only persists paths under $CI_PROJECT_DIR) never carries this binary
# across the MR/protected trust boundary. That is the issue #211 poisoning vector
# PRD #230 M6b was dropped to avoid: a cache HIT that serves an unverified binary.
# Here CI re-downloads + re-verifies once per job (~1-2s, vs the 51s compile it
# replaces); locally the binary is reused across runs from the same cache.
CACHE_ROOT="${UZI_GOLANGCI_LINT_DIR:-${XDG_CACHE_HOME:-$HOME/.cache}/uzi-golangci-lint}"
DIR="$CACHE_ROOT/$VER_NO_V/$asset"
BIN="$DIR/golangci-lint"

if [ ! -x "$BIN" ]; then
  mkdir -p "$DIR"
  TMP="$(mktemp -d)"
  # shellcheck disable=SC2064  # expand TMP now so the trap survives its unset.
  trap "rm -rf '$TMP'" EXIT INT TERM
  url="https://github.com/golangci/golangci-lint/releases/download/v${VER_NO_V}/golangci-lint-${VER_NO_V}-${asset}.tar.gz"
  # curl on golang:1.26 and darwin; wget as a fallback for a curl-less image
  # (mirrors .task_setup's discriminator). The lint jobs run on golang:1.26,
  # which has curl.
  if command -v curl >/dev/null 2>&1; then
    curl -fsSLo "$TMP/gcl.tar.gz" "$url"
  else
    wget -qO "$TMP/gcl.tar.gz" "$url"
  fi
  echo "$sha256  $TMP/gcl.tar.gz" | sha256sum -c -
  # The tarball extracts to golangci-lint-<ver>-<asset>/golangci-lint.
  tar -xzf "$TMP/gcl.tar.gz" -C "$TMP"
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
        echo "  version and BOTH per-arch sha256 literals in this script (PRD #230 D6)." >&2
        exit 2
      fi
      ;;
  esac
fi

exec "$BIN" "$@"
