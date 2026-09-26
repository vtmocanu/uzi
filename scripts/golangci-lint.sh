#!/bin/sh
# Acquire the PINNED golangci-lint RELEASE BINARY (sha256-verified) and run it.
# PRD #230 M5.
#
# WHY THIS REPLACES `go run .../golangci-lint@vX.Y.Z`. That form COMPILES the
# linter from source on a cold GOCACHE (~51s, measured — the pipeline's cold long
# pole, lint:controller 564.8s on MR !157). A downloaded release archive skips the
# compile entirely. Local and CI acquire their platform's artifact from the SAME
# pinned release, and the findings were verified identical (PRD #103 SC-1). The
# linter VERSION, not its build-Go, decides which rules fire (see the D6 guard
# below for the one thing build-Go does decide).
#
# Usage:  scripts/golangci-lint.sh <version> <golangci-lint args...>
#   e.g.  scripts/golangci-lint.sh vX.Y.Z run ./...
#         scripts/golangci-lint.sh vX.Y.Z config verify
#         scripts/golangci-lint.sh vX.Y.Z cache clean
#
# The version is an ARGUMENT, not a script default, so `task`'s echo still shows
# the resolved static pin. This matches scripts/deadcode-gate.sh and
# validate:api's sqlc pin, and makes a missing pin visible in gate output.
#
# The download RETRIES transient failures (issue #1144: a CDN connection reset
# reddened lint jobs). curl needs --retry-all-errors because plain --retry does
# not retry a connection reset. wget loops explicitly (4 attempts, 3 retries)
# instead of --tries, which stays portable to BusyBox and removes any partial
# output before each attempt. The
# tarball sha256 check still runs before extraction, so a retry cannot
# substitute a different artifact.
set -eu

VERSION="${1:-}"
if [ -z "$VERSION" ]; then
  echo "usage: scripts/golangci-lint.sh <version> <args...>" >&2
  echo "  e.g. scripts/golangci-lint.sh vX.Y.Z run ./..." >&2
  exit 2
fi
shift

# --- Per-arch pinned release archives -----------------------------------------
# github-release-attachments maps each current tarball sha256 to the SAME asset
# in a newer release. Distinct depName aliases prevent Renovate from deduplicating
# the two matches in this one file; packageName keeps both on the real upstream.
# renovate-tarball: depName=golangci-lint-darwin-arm64 packageName=golangci/golangci-lint
GOLANGCI_LINT_DARWIN_ARM64_VERSION="v2.13.2"
GOLANGCI_LINT_DARWIN_ARM64_SHA256="f4bf83f0b64f055c42b28fc9a38861839f69c096e61c788e72dfaae412011789"
# renovate-tarball: depName=golangci-lint-linux-amd64 packageName=golangci/golangci-lint
GOLANGCI_LINT_LINUX_AMD64_VERSION="v2.13.2"
GOLANGCI_LINT_LINUX_AMD64_SHA256="2277d43b98ec0054280f2ac26b53268bae97682444678a59a657dd565da021d6"

# One sha across two arches is a guaranteed mismatch whose tempting "fix" is to
# delete the check. Only darwin/arm64 (the dev host) and linux/amd64 (both shared
# runners) are pinned; any other host fails loudly. The versions are duplicated
# so Renovate can update each asset and digest atomically, then checked here so a
# partial update cannot pass on whichever architecture happens to run first.
if [ "$GOLANGCI_LINT_DARWIN_ARM64_VERSION" != "$GOLANGCI_LINT_LINUX_AMD64_VERSION" ]; then
  echo "golangci-lint.sh: per-arch versions disagree:" >&2
  echo "  darwin/arm64=$GOLANGCI_LINT_DARWIN_ARM64_VERSION" >&2
  echo "  linux/amd64=$GOLANGCI_LINT_LINUX_AMD64_VERSION" >&2
  echo "  Renovate must update both version+sha256 pairs in one change." >&2
  exit 2
fi

os="$(uname -s)"
arch="$(uname -m)"
case "$os/$arch" in
  Darwin/arm64)
    asset="darwin-arm64"
    tar_sha256="$GOLANGCI_LINT_DARWIN_ARM64_SHA256"
    ;;
  Linux/x86_64 | Linux/amd64)
    asset="linux-amd64"
    tar_sha256="$GOLANGCI_LINT_LINUX_AMD64_SHA256"
    ;;
  *)
    echo "golangci-lint.sh: unsupported host $os/$arch" >&2
    echo "  Only darwin/arm64 and linux/amd64 are pinned. Adding one needs its" >&2
    echo "  own version+tarball-sha256 pair derived from that arch's artifact." >&2
    exit 2
    ;;
esac

pinned_version="$GOLANGCI_LINT_DARWIN_ARM64_VERSION"
if [ "$VERSION" != "$pinned_version" ]; then
  echo "golangci-lint.sh: pinned archives are for $pinned_version, got '$VERSION'." >&2
  echo "  Update Taskfile.yml's pin and both per-arch version+sha256 pairs together." >&2
  exit 2
fi
VER_NO_V="${VERSION#v}"

# sha256 verification with a macOS fallback. coreutils `sha256sum` is not on a
# stock macOS (which ships `shasum`). Both accept the identical
# "<hex>  <path>" line on stdin under `-c -` and exit nonzero on mismatch.
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

# --- Cache the verified archive, never an independently trusted binary --------
# The cache stays outside the project tree, so CI never carries it across an
# untrusted/protected boundary. Every invocation copies the cached archive into
# its private temp dir, verifies that snapshot, and extracts a fresh binary from
# the same snapshot. A cache entry changed after the copy cannot change what this
# run executes. A corrupt or planted entry is removed and never extracted; this
# preserves per-run cache integrity without a second, non-upstream binary hash.
CACHE_ROOT="${UZI_GOLANGCI_LINT_DIR:-${XDG_CACHE_HOME:-$HOME/.cache}/uzi-golangci-lint}"
DIR="$CACHE_ROOT/$VER_NO_V/$asset"
ARCHIVE="$DIR/golangci-lint-${VER_NO_V}-${asset}.tar.gz"
# `ulimit -f` units differ (512 bytes on Linux sh, 1024 on macOS sh). Divide the
# byte caps by the larger unit so neither platform can transiently exceed them.
# Bound both network/cache inputs and the complete decompressed tar stream.
MAX_ARCHIVE_BYTES=134217728  # 128 MiB; current archives are far smaller.
MAX_ARCHIVE_BLOCKS=131072
MAX_TAR_BYTES=268435456      # 256 MiB total decompressed tar stream.
MAX_TAR_BLOCKS=262144
mkdir -p "$DIR"
TMP="$(mktemp -d)"
cleanup() {
  rm -rf "$TMP"
}
trap cleanup EXIT
CANDIDATE="$TMP/gcl.tar.gz"

# Copy first and verify the private snapshot. A concurrent mutation of the cache
# after this copy cannot change the bytes later extracted.
candidate_ready=0
if [ -f "$ARCHIVE" ]; then
  if (ulimit -f "$MAX_ARCHIVE_BLOCKS"; cp "$ARCHIVE" "$CANDIDATE") \
    && verify_sha256 "$tar_sha256" "$CANDIDATE"; then
    candidate_ready=1
  else
    rm -f "$CANDIDATE"
  fi
fi

downloaded=0
if [ "$candidate_ready" -ne 1 ]; then
  rm -f "$ARCHIVE"
  url="https://github.com/golangci/golangci-lint/releases/download/v${VER_NO_V}/golangci-lint-${VER_NO_V}-${asset}.tar.gz"
  # The file-size resource limit stops tools that ignore or never receive a
  # Content-Length. curl and wget remain transport alternatives, not size guards.
  if command -v curl >/dev/null 2>&1; then
    (ulimit -f "$MAX_ARCHIVE_BLOCKS"; curl -fsSL --retry 3 --retry-delay 2 \
      --retry-all-errors --retry-connrefused -o "$CANDIDATE" "$url") \
      || { echo "golangci-lint.sh: bounded download failed: $url" >&2; exit 2; }
  else
    attempt=1
    while :; do
      rm -f "$CANDIDATE"
      if (ulimit -f "$MAX_ARCHIVE_BLOCKS"; wget -qO "$CANDIDATE" "$url"); then
        break
      fi
      if [ "$attempt" -ge 4 ]; then
        echo "golangci-lint.sh: bounded download failed: $url" >&2
        exit 2
      fi
      attempt=$((attempt + 1))
      sleep 2
    done
  fi
  verify_sha256 "$tar_sha256" "$CANDIDATE" \
    || { echo "golangci-lint.sh: tarball checksum mismatch for $asset (expected $tar_sha256)" >&2; exit 2; }
  downloaded=1
fi
archive_bytes="$(wc -c < "$CANDIDATE" | tr -d ' ')"
[ "$archive_bytes" -le "$MAX_ARCHIVE_BYTES" ] \
  || { echo "golangci-lint.sh: archive exceeds $MAX_ARCHIVE_BYTES bytes" >&2; exit 2; }

# Decompress into a bounded ordinary tar before inspecting or extracting it.
# This caps cumulative member bytes even when the gzip ratio is extreme.
UNPACKED="$TMP/gcl.tar"
(ulimit -f "$MAX_TAR_BLOCKS"; gzip -dc "$CANDIDATE" > "$UNPACKED") \
  || { echo "golangci-lint.sh: archive exceeds or cannot produce a $MAX_TAR_BYTES-byte tar" >&2; exit 2; }
tar_bytes="$(wc -c < "$UNPACKED" | tr -d ' ')"
[ "$tar_bytes" -le "$MAX_TAR_BYTES" ] \
  || { echo "golangci-lint.sh: decompressed tar exceeds $MAX_TAR_BYTES bytes" >&2; exit 2; }

# The upstream archive contract is exactly three regular files below one
# versioned directory. Exact names reject absolute/traversal paths and extra
# members; the verbose type check rejects links, devices and other special files.
prefix="golangci-lint-${VER_NO_V}-${asset}"
EXPECTED_MEMBERS="$TMP/expected-members"
MEMBERS="$TMP/members"
MEMBERS_SORTED="$TMP/members-sorted"
VERBOSE_MEMBERS="$TMP/members-verbose"
printf '%s\n' "$prefix/LICENSE" "$prefix/README.md" "$prefix/golangci-lint" > "$EXPECTED_MEMBERS"
tar -tf "$UNPACKED" > "$MEMBERS" \
  || { echo "golangci-lint.sh: could not list verified archive for $asset" >&2; exit 2; }
LC_ALL=C sort "$MEMBERS" > "$MEMBERS_SORTED"
cmp -s "$EXPECTED_MEMBERS" "$MEMBERS_SORTED" \
  || { echo "golangci-lint.sh: verified archive has unexpected member paths" >&2; exit 2; }
tar -tvf "$UNPACKED" > "$VERBOSE_MEMBERS" \
  || { echo "golangci-lint.sh: could not inspect verified archive member types" >&2; exit 2; }
awk 'NF == 0 || substr($1, 1, 1) != "-" { bad = 1 } END { exit(bad || NR != 3) }' "$VERBOSE_MEMBERS" \
  || { echo "golangci-lint.sh: verified archive contains a non-regular or extra member" >&2; exit 2; }

# Apply the same conservative limit during extraction. This catches PAX/GNU
# sparse members whose logical size can exceed the complete tar-stream size.
(ulimit -f "$MAX_TAR_BLOCKS"; tar -xf "$UNPACKED" -C "$TMP") \
  || { echo "golangci-lint.sh: extraction failed or exceeded the per-file size cap" >&2; exit 2; }
BIN="$TMP/$prefix/golangci-lint"
if [ ! -x "$BIN" ]; then
  echo "golangci-lint.sh: verified archive did not contain an executable at the expected path" >&2
  exit 2
fi
binary_bytes="$(wc -c < "$BIN" | tr -d ' ')"
[ "$binary_bytes" -gt 0 ] && [ "$binary_bytes" -le "$MAX_TAR_BYTES" ] \
  || { echo "golangci-lint.sh: extracted binary has invalid size $binary_bytes" >&2; exit 2; }
# Cache only after the fully bounded structure has validated. `mv` replaces a
# hostile destination symlink rather than following it.
if [ "$downloaded" -eq 1 ]; then
  mv "$CANDIDATE" "$ARCHIVE"
fi

# --- D6 build-Go guard (PRD #230 M5 / D6) -------------------------------------
# golangci-lint can only lint code whose selected Go LANGUAGE version is <= the
# Go it was built with (golangci-lint #5641/#6272, FAQ). The selected version is
# the newest of the module's `go` directive, optional `toolchain go...` directive,
# and `go env GOVERSION` under the caller's effective GOTOOLCHAIN policy. PR #1275
# proved the directive distinction; a newer ambient Go produces the same failure.
# The intrinsic failure is loud, but this guard names the selecting source first.
parse_language() {  # $1 = Go version; prints "major:minor"
  value="$1"
  major="${value%%.*}"
  rest="${value#*.}"
  [ "$rest" != "$value" ] || return 1
  minor="${rest%%.*}"
  case "$major" in '' | *[!0-9]*) return 1 ;; esac
  case "$minor" in '' | *[!0-9]*) return 1 ;; esac
  printf '%s:%s\n' "$major" "$minor"
}

language_is_newer() {  # $1 and $2 are "major:minor"
  left_major="${1%%:*}"
  left_minor="${1#*:}"
  right_major="${2%%:*}"
  right_minor="${2#*:}"
  [ "$left_major" -gt "$right_major" ] \
    || { [ "$left_major" -eq "$right_major" ] && [ "$left_minor" -gt "$right_minor" ]; }
}

if [ -f go.mod ]; then
  # Read build-Go from the binary's own `version` output. `go version <binary>`
  # would instead run through the caller's toolchain selection, conflating the
  # linter's build provenance with the ambient Go measured separately below.
  build_go="$("$BIN" version 2>/dev/null | awk '{for (i = 1; i <= NF; i++) if ($i ~ /^go[0-9]/) { sub(/^go/, "", $i); print $i; exit }}')"
  mod_go="$(awk '$1 == "go" { print $2; exit }' go.mod)"
  toolchain_go="$(awk '$1 == "toolchain" { sub(/^go/, "", $2); print $2; exit }' go.mod)"
  ambient_go=""
  if command -v go >/dev/null 2>&1; then
    ambient_go="$(go env GOVERSION 2>/dev/null)" || ambient_go=""
    ambient_go="${ambient_go#go}"
  fi
  build_language="$(parse_language "$build_go")" || build_language=""
  target_language="$(parse_language "$mod_go")" || target_language=""
  target_source="go directive $mod_go"

  if [ -n "$target_language" ] && [ -n "$toolchain_go" ]; then
    toolchain_language="$(parse_language "$toolchain_go")" || toolchain_language=""
    if [ -n "$toolchain_language" ] && language_is_newer "$toolchain_language" "$target_language"; then
      target_language="$toolchain_language"
      target_source="toolchain directive go$toolchain_go"
    fi
  fi
  if [ -n "$target_language" ] && [ -n "$ambient_go" ]; then
    ambient_language="$(parse_language "$ambient_go")" || ambient_language=""
    if [ -n "$ambient_language" ] && language_is_newer "$ambient_language" "$target_language"; then
      target_language="$ambient_language"
      target_source="go env GOVERSION go$ambient_go"
    fi
  fi

  # If either value fails to parse, skip rather than fail spuriously;
  # golangci-lint's own exit still covers a real mismatch.
  if [ -n "$build_language" ] && [ -n "$target_language" ] \
    && language_is_newer "$target_language" "$build_language"; then
    b_maj="${build_language%%:*}"
    b_min="${build_language#*:}"
    t_maj="${target_language%%:*}"
    t_min="${target_language#*:}"
    echo "golangci-lint.sh: pinned binary build-Go language ${b_maj}.${b_min} (go${build_go}) is BELOW" >&2
    echo "  this module's selected Go language ${t_maj}.${t_min} ($target_source, $(pwd)/go.mod)." >&2
    echo "  golangci-lint would emit typecheck errors instead of findings" >&2
    echo "  (golangci-lint #5641/#6272), diverging local/CI from this pin." >&2
    echo "  Bump to a release built with go>=${t_maj}.${t_min}; Renovate must update" >&2
    echo "  Taskfile.yml and both per-arch version+sha256 pairs together." >&2
    exit 2
  fi
fi

# Preserve the original exec semantics so signals and the exact linter status
# reach the caller directly. A watcher ignores catchable group cancellation and
# removes the private extraction after the exec'd process exits. Direct SIGKILL to
# the owner is covered too; no process can clean up a whole-group SIGKILL.
owner_pid="$$"
(
  trap '' HUP INT QUIT TERM
  while kill -0 "$owner_pid" 2>/dev/null; do
    sleep 1
  done
  rm -rf "$TMP"
) </dev/null >/dev/null 2>&1 &
trap - EXIT
exec "$BIN" "$@"
