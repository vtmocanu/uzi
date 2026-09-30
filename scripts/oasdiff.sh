#!/bin/sh
# Acquire the PINNED oasdiff RELEASE BINARY (sha256-verified) and run it.
# PRD #1908 M5 (Decision 11), following scripts/golangci-lint.sh.
#
# oasdiff is the OpenAPI diff tool behind `task check:api-v1-compat`: it compares the
# api/openapi/v1.yaml on origin/main with the working tree's copy and fails on any
# breaking change. It is acquired as a downloaded release archive, not built from
# source and not listed in devbox.json (that file is worker config).
#
# Usage:  scripts/oasdiff.sh <version> <oasdiff args...>
#   e.g.  scripts/oasdiff.sh vX.Y.Z breaking --fail-on ERR base.yaml revision.yaml
#
# The version is an ARGUMENT, not a script default, so `task`'s echo shows the
# resolved static pin and a missing pin is visible in gate output. It must equal the
# version pinned below; the Taskfile and this file are bumped together.
#
# The download RETRIES transient failures the way golangci-lint.sh does (issue #1144):
# curl needs --retry-all-errors because plain --retry does not retry a connection
# reset, and wget loops explicitly (4 attempts) so each attempt starts from no partial
# output. The tarball sha256 check runs before extraction, so a retry cannot
# substitute a different artifact.
#
# Exit status: the oasdiff binary's own status when it ran; 2 for every failure of
# this wrapper itself (bad usage, unsupported host, download, checksum, archive
# structure), so a gate can tell "the tool found a breaking change" (oasdiff's own
# nonzero) from "the tool never ran".
set -eu

VERSION="${1:-}"
if [ -z "$VERSION" ]; then
  echo "usage: scripts/oasdiff.sh <version> <oasdiff args...>" >&2
  echo "  e.g. scripts/oasdiff.sh vX.Y.Z breaking base.yaml revision.yaml" >&2
  exit 2
fi
shift

# --- Per-arch pinned release archives -----------------------------------------
# The version+sha256 pairs are updated together, from the release's checksums.txt.
# darwin ships ONE universal (arm64+x86_64) archive; linux ships one per arch.
# Only these three hosts are pinned; any other host fails loudly rather than
# skipping the check.
# renovate-tarball: depName=oasdiff-darwin-all packageName=oasdiff/oasdiff
OASDIFF_DARWIN_ALL_VERSION="v1.32.1"
OASDIFF_DARWIN_ALL_SHA256="e4d74b7e2dfb9d4819e7fc720c905ec86547e4637ac270a2b0187c0f1fb7187e"
# renovate-tarball: depName=oasdiff-linux-amd64 packageName=oasdiff/oasdiff
OASDIFF_LINUX_AMD64_VERSION="v1.32.1"
OASDIFF_LINUX_AMD64_SHA256="7c8939fc49b75ee11fec66a5b83b37a2fca6aee109fed85013b1ba2ac2a1ee7f"
# renovate-tarball: depName=oasdiff-linux-arm64 packageName=oasdiff/oasdiff
OASDIFF_LINUX_ARM64_VERSION="v1.32.1"
OASDIFF_LINUX_ARM64_SHA256="32fff58a120f75a723d6c2422444691c37fa6813fed61d23f53dbcb604b30f6d"

if [ "$OASDIFF_DARWIN_ALL_VERSION" != "$OASDIFF_LINUX_AMD64_VERSION" ] \
  || [ "$OASDIFF_LINUX_AMD64_VERSION" != "$OASDIFF_LINUX_ARM64_VERSION" ]; then
  echo "oasdiff.sh: per-arch versions disagree:" >&2
  echo "  darwin/all=$OASDIFF_DARWIN_ALL_VERSION" >&2
  echo "  linux/amd64=$OASDIFF_LINUX_AMD64_VERSION" >&2
  echo "  linux/arm64=$OASDIFF_LINUX_ARM64_VERSION" >&2
  echo "  Update every version+sha256 pair in one change." >&2
  exit 2
fi

os="$(uname -s)"
arch="$(uname -m)"
case "$os/$arch" in
  Darwin/arm64 | Darwin/x86_64)
    asset="darwin_all"
    tar_sha256="$OASDIFF_DARWIN_ALL_SHA256"
    ;;
  Linux/x86_64 | Linux/amd64)
    asset="linux_amd64"
    tar_sha256="$OASDIFF_LINUX_AMD64_SHA256"
    ;;
  Linux/aarch64 | Linux/arm64)
    asset="linux_arm64"
    tar_sha256="$OASDIFF_LINUX_ARM64_SHA256"
    ;;
  *)
    echo "oasdiff.sh: unsupported host $os/$arch" >&2
    echo "  Only darwin (arm64, x86_64), linux/amd64 and linux/arm64 are pinned. Adding" >&2
    echo "  one needs its own version+tarball-sha256 pair from the release's checksums.txt." >&2
    exit 2
    ;;
esac

pinned_version="$OASDIFF_DARWIN_ALL_VERSION"
if [ "$VERSION" != "$pinned_version" ]; then
  echo "oasdiff.sh: pinned archives are for $pinned_version, got '$VERSION'." >&2
  echo "  Update Taskfile.yml's pin and every per-arch version+sha256 pair together." >&2
  exit 2
fi
VER_NO_V="${VERSION#v}"

# sha256 verification with a macOS fallback (see golangci-lint.sh): both tools accept
# the identical "<hex>  <path>" line on stdin under `-c -` and exit nonzero on mismatch.
verify_sha256() {  # $1 = expected hex, $2 = file; returns the check's status
  if command -v sha256sum >/dev/null 2>&1; then
    printf '%s  %s\n' "$1" "$2" | sha256sum -c - >/dev/null
  elif command -v shasum >/dev/null 2>&1; then
    printf '%s  %s\n' "$1" "$2" | shasum -a 256 -c - >/dev/null
  else
    echo "oasdiff.sh: no sha256 tool found (need sha256sum or shasum)" >&2
    return 2
  fi
}

# --- Cache the verified archive, never an independently trusted binary --------
# The cache stays outside the project tree. Every invocation copies the cached archive
# into its private temp dir, verifies THAT snapshot and extracts a fresh binary from it,
# so a cache entry changed after the copy cannot change what this run executes. A
# corrupt or planted entry is removed and never extracted.
CACHE_ROOT="${UZI_OASDIFF_DIR:-${XDG_CACHE_HOME:-$HOME/.cache}/uzi-oasdiff}"
DIR="$CACHE_ROOT/$VER_NO_V/$asset"
ARCHIVE="$DIR/oasdiff_${VER_NO_V}_${asset}.tar.gz"
# `ulimit -f` units differ (512 bytes on Linux sh, 1024 on macOS sh). Divide the byte
# caps by the larger unit so neither platform can transiently exceed them. Bound both
# the network/cache inputs and the complete decompressed tar stream.
MAX_ARCHIVE_BYTES=134217728  # 128 MiB; the current archives are 7-14 MB.
MAX_ARCHIVE_BLOCKS=131072
MAX_TAR_BYTES=268435456      # 256 MiB total decompressed tar stream.
MAX_TAR_BLOCKS=262144
mkdir -p "$DIR"
TMP="$(mktemp -d)"
cleanup() {
  rm -rf "$TMP"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
CANDIDATE="$TMP/oasdiff.tar.gz"

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
  url="https://github.com/oasdiff/oasdiff/releases/download/v${VER_NO_V}/oasdiff_${VER_NO_V}_${asset}.tar.gz"
  # The file-size resource limit stops tools that ignore or never receive a
  # Content-Length. curl and wget remain transport alternatives, not size guards.
  if command -v curl >/dev/null 2>&1; then
    (ulimit -f "$MAX_ARCHIVE_BLOCKS"; curl -fsSL --retry 3 --retry-delay 2 \
      --retry-all-errors --retry-connrefused -o "$CANDIDATE" "$url") \
      || { echo "oasdiff.sh: bounded download failed: $url" >&2; exit 2; }
  else
    attempt=1
    while :; do
      rm -f "$CANDIDATE"
      if (ulimit -f "$MAX_ARCHIVE_BLOCKS"; wget -qO "$CANDIDATE" "$url"); then
        break
      fi
      if [ "$attempt" -ge 4 ]; then
        echo "oasdiff.sh: bounded download failed: $url" >&2
        exit 2
      fi
      attempt=$((attempt + 1))
      sleep 2
    done
  fi
  verify_sha256 "$tar_sha256" "$CANDIDATE" \
    || { echo "oasdiff.sh: tarball checksum mismatch for $asset (expected $tar_sha256)" >&2; exit 2; }
  downloaded=1
fi
archive_bytes="$(wc -c < "$CANDIDATE" | tr -d ' ')"
[ "$archive_bytes" -le "$MAX_ARCHIVE_BYTES" ] \
  || { echo "oasdiff.sh: archive exceeds $MAX_ARCHIVE_BYTES bytes" >&2; exit 2; }

# Decompress into a bounded ordinary tar before inspecting or extracting it, so the
# cumulative member bytes are capped even when the gzip ratio is extreme.
UNPACKED="$TMP/oasdiff.tar"
(ulimit -f "$MAX_TAR_BLOCKS"; gzip -dc "$CANDIDATE" > "$UNPACKED") \
  || { echo "oasdiff.sh: archive exceeds or cannot produce a $MAX_TAR_BYTES-byte tar" >&2; exit 2; }
tar_bytes="$(wc -c < "$UNPACKED" | tr -d ' ')"
[ "$tar_bytes" -le "$MAX_TAR_BYTES" ] \
  || { echo "oasdiff.sh: decompressed tar exceeds $MAX_TAR_BYTES bytes" >&2; exit 2; }

# The upstream archive contract is exactly two regular files at the top level: LICENSE
# and oasdiff. Exact names reject absolute/traversal paths and extra members; the
# verbose type check rejects links, devices and other special files.
EXPECTED_MEMBERS="$TMP/expected-members"
MEMBERS="$TMP/members"
MEMBERS_SORTED="$TMP/members-sorted"
VERBOSE_MEMBERS="$TMP/members-verbose"
printf '%s\n' "LICENSE" "oasdiff" > "$EXPECTED_MEMBERS"
tar -tf "$UNPACKED" > "$MEMBERS" \
  || { echo "oasdiff.sh: could not list verified archive for $asset" >&2; exit 2; }
LC_ALL=C sort "$MEMBERS" > "$MEMBERS_SORTED"
cmp -s "$EXPECTED_MEMBERS" "$MEMBERS_SORTED" \
  || { echo "oasdiff.sh: verified archive has unexpected member paths" >&2; exit 2; }
tar -tvf "$UNPACKED" > "$VERBOSE_MEMBERS" \
  || { echo "oasdiff.sh: could not inspect verified archive member types" >&2; exit 2; }
awk 'NF == 0 || substr($1, 1, 1) != "-" { bad = 1 } END { exit(bad || NR != 2) }' "$VERBOSE_MEMBERS" \
  || { echo "oasdiff.sh: verified archive contains a non-regular or extra member" >&2; exit 2; }

# The same conservative size limit during extraction catches sparse members whose
# logical size can exceed the tar stream's.
EXTRACT="$TMP/extract"
mkdir -p "$EXTRACT"
(ulimit -f "$MAX_TAR_BLOCKS"; tar -xf "$UNPACKED" -C "$EXTRACT") \
  || { echo "oasdiff.sh: extraction failed or exceeded the per-file size cap" >&2; exit 2; }
BIN="$EXTRACT/oasdiff"
if [ ! -x "$BIN" ]; then
  echo "oasdiff.sh: verified archive did not contain an executable at the expected path" >&2
  exit 2
fi
binary_bytes="$(wc -c < "$BIN" | tr -d ' ')"
[ "$binary_bytes" -gt 0 ] && [ "$binary_bytes" -le "$MAX_TAR_BYTES" ] \
  || { echo "oasdiff.sh: extracted binary has invalid size $binary_bytes" >&2; exit 2; }
# Cache only after the fully bounded structure has validated. `mv` replaces a hostile
# destination symlink rather than following it.
if [ "$downloaded" -eq 1 ]; then
  mv "$CANDIDATE" "$ARCHIVE"
fi

# Run as a child (not exec) so the EXIT trap removes the private extraction, and hand
# back oasdiff's own status. `|| rc=$?` records it under `set -e`.
rc=0
"$BIN" "$@" || rc=$?
exit "$rc"
