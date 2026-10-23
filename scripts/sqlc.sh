#!/bin/sh
# Acquire a pinned sqlc archive and execute a verified private snapshot.
set -eu

VERSION="${1:-}"
if [ -z "$VERSION" ] || [ "$#" -lt 2 ]; then
  echo "usage: scripts/sqlc.sh <version> <args...>" >&2
  echo "  e.g. scripts/sqlc.sh v1.31.1 generate" >&2
  exit 2
fi
shift

# --- Per-arch pinned release archives -----------------------------------------
# github-release-attachments maps each current tarball sha256 to the SAME asset
# in a newer release. Distinct depName aliases prevent Renovate from deduplicating
# the two matches in this one file; packageName keeps both on the real upstream.
# renovate-tarball: depName=sqlc-darwin-arm64 packageName=sqlc-dev/sqlc
SQLC_DARWIN_ARM64_VERSION="v1.31.1"
SQLC_DARWIN_ARM64_SHA256="21602158c99eb1f2bae197a66abfb1941d1e9e50b23125bb193349c6b1acc71e"
# renovate-tarball: depName=sqlc-linux-amd64 packageName=sqlc-dev/sqlc
SQLC_LINUX_AMD64_VERSION="v1.31.1"
SQLC_LINUX_AMD64_SHA256="497ae4fcdfa64c5b0c311ffe4c2bd991e43991e82e5367792ed78bc2dca27354"
# renovate-tarball: depName=sqlc-linux-arm64 packageName=sqlc-dev/sqlc
SQLC_LINUX_ARM64_VERSION="v1.31.1"
SQLC_LINUX_ARM64_SHA256="b7cae247740d0c51a1e657479e5b2d21e6fef428f596682a01bc55bf4ab8a23d"

# One sha across two arches is a guaranteed mismatch whose tempting "fix" is to
# delete the check. Darwin/arm64 and both Linux worker architectures are pinned;
# any other host fails loudly. The versions are duplicated
# so Renovate can update each asset and digest atomically, then checked here so a
# partial update cannot pass on whichever architecture happens to run first.
if [ "$SQLC_DARWIN_ARM64_VERSION" != "$SQLC_LINUX_AMD64_VERSION" ] \
  || [ "$SQLC_DARWIN_ARM64_VERSION" != "$SQLC_LINUX_ARM64_VERSION" ]; then
  echo "sqlc.sh: per-arch versions disagree:" >&2
  echo "  darwin/arm64=$SQLC_DARWIN_ARM64_VERSION" >&2
  echo "  linux/amd64=$SQLC_LINUX_AMD64_VERSION" >&2
  echo "  linux/arm64=$SQLC_LINUX_ARM64_VERSION" >&2
  echo "  Renovate must update all version+sha256 pairs in one change." >&2
  exit 2
fi

os="$(uname -s)"
arch="$(uname -m)"
case "$os/$arch" in
  Darwin/arm64)
    asset="darwin-arm64"
    tar_sha256="$SQLC_DARWIN_ARM64_SHA256"
    ;;
  Linux/x86_64 | Linux/amd64)
    asset="linux-amd64"
    tar_sha256="$SQLC_LINUX_AMD64_SHA256"
    ;;
  Linux/aarch64 | Linux/arm64)
    asset="linux-arm64"
    tar_sha256="$SQLC_LINUX_ARM64_SHA256"
    ;;
  *)
    echo "sqlc.sh: unsupported host $os/$arch" >&2
    echo "  Only darwin/arm64, linux/amd64 and linux/arm64 are pinned. Adding one needs its" >&2
    echo "  own version+tarball-sha256 pair derived from that arch's artifact." >&2
    exit 2
    ;;
esac

pinned_version="$SQLC_DARWIN_ARM64_VERSION"
if [ "$VERSION" != "$pinned_version" ]; then
  echo "sqlc.sh: pinned archives are for $pinned_version, got '$VERSION'." >&2
  echo "  Update ci.yml's SQLC_VERSION and all per-arch version+sha256 pairs together." >&2
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
    echo "sqlc.sh: no sha256 tool found (need sha256sum or shasum)" >&2
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
CACHE_ROOT="${UZI_SQLC_DIR:-${XDG_CACHE_HOME:-$HOME/.cache}/uzi-sqlc}"
DIR="$CACHE_ROOT/$VER_NO_V/$asset"
ARCHIVE="$DIR/sqlc_${VER_NO_V}_$(printf '%s' "$asset" | tr '-' '_').tar.gz"
# `ulimit -f` units differ (512 bytes on Linux sh, 1024 on macOS sh). Divide the
# byte caps by the larger unit so neither platform can transiently exceed them.
# Bound both network/cache inputs and the complete decompressed tar stream.
MAX_ARCHIVE_BYTES=33554432   # 32 MiB; inspected release archives are below 16 MiB.
MAX_ARCHIVE_BLOCKS=32768
MAX_TAR_BYTES=134217728  # 128 MiB; the conservative Linux limit is 64 MiB.
MAX_TAR_BLOCKS=131072
mkdir -p "$DIR" || { echo "sqlc.sh: cannot create cache directory" >&2; exit 2; }
TMP="$(mktemp -d)" || exit 2
STAGE=""
# shellcheck disable=SC2329 # Invoked by the POSIX trap 0 handler below.
cleanup() {
  rm -rf "$TMP"
  if [ -n "$STAGE" ]; then rm -rf "$STAGE"; fi
}
trap cleanup 0
trap 'exit 130' INT
trap 'exit 143' TERM
CANDIDATE="$TMP/sqlc.tar.gz"

# Copy first and verify the private snapshot. A concurrent mutation of the cache
# after this copy cannot change the bytes later extracted.
candidate_ready=0
if [ -f "$ARCHIVE" ]; then
  if (ulimit -f "$MAX_ARCHIVE_BLOCKS" && cp "$ARCHIVE" "$CANDIDATE") \
    && verify_sha256 "$tar_sha256" "$CANDIDATE"; then
    candidate_ready=1
  else
    rm -f "$CANDIDATE"
  fi
fi

downloaded=0
if [ "$candidate_ready" -ne 1 ]; then
  rm -f "$ARCHIVE"
  url="https://github.com/sqlc-dev/sqlc/releases/download/v${VER_NO_V}/sqlc_${VER_NO_V}_$(printf '%s' "$asset" | tr '-' '_').tar.gz"
  # The file-size resource limit stops tools that ignore or never receive a
  # Content-Length. curl and wget remain transport alternatives, not size guards.
  if command -v curl >/dev/null 2>&1; then
    (ulimit -f "$MAX_ARCHIVE_BLOCKS" && curl -fsSL --retry 3 --retry-delay 2 \
      --retry-all-errors --retry-connrefused -o "$CANDIDATE" "$url") \
      || { echo "sqlc.sh: bounded download failed: $url" >&2; exit 2; }
  else
    attempt=1
    while :; do
      rm -f "$CANDIDATE"
      if (ulimit -f "$MAX_ARCHIVE_BLOCKS" && wget -qO "$CANDIDATE" "$url"); then
        break
      fi
      if [ "$attempt" -ge 4 ]; then
        echo "sqlc.sh: bounded download failed: $url" >&2
        exit 2
      fi
      attempt=$((attempt + 1))
      sleep 2
    done
  fi
  verify_sha256 "$tar_sha256" "$CANDIDATE" \
    || { echo "sqlc.sh: tarball checksum mismatch for $asset (expected $tar_sha256)" >&2; exit 2; }
  downloaded=1
fi
archive_bytes="$(wc -c < "$CANDIDATE" | tr -d ' ')"
[ "$archive_bytes" -le "$MAX_ARCHIVE_BYTES" ] \
  || { echo "sqlc.sh: archive exceeds $MAX_ARCHIVE_BYTES bytes" >&2; exit 2; }

# Decompress into a bounded ordinary tar before inspecting or extracting it.
# This caps cumulative member bytes even when the gzip ratio is extreme.
UNPACKED="$TMP/sqlc.tar"
(ulimit -f "$MAX_TAR_BLOCKS" && gzip -dc "$CANDIDATE" > "$UNPACKED") \
  || { echo "sqlc.sh: archive exceeds or cannot produce a $MAX_TAR_BYTES-byte tar" >&2; exit 2; }
tar_bytes="$(wc -c < "$UNPACKED" | tr -d ' ')"
[ "$tar_bytes" -le "$MAX_TAR_BYTES" ] \
  || { echo "sqlc.sh: decompressed tar exceeds $MAX_TAR_BYTES bytes" >&2; exit 2; }

# The pinned release archives contain exactly one regular file named sqlc.
EXPECTED_MEMBERS="$TMP/expected-members"
MEMBERS="$TMP/members"
MEMBERS_SORTED="$TMP/members-sorted"
VERBOSE_MEMBERS="$TMP/members-verbose"
printf '%s\n' sqlc > "$EXPECTED_MEMBERS"
tar -tf "$UNPACKED" > "$MEMBERS" \
  || { echo "sqlc.sh: could not list verified archive for $asset" >&2; exit 2; }
LC_ALL=C sort "$MEMBERS" > "$MEMBERS_SORTED"
cmp -s "$EXPECTED_MEMBERS" "$MEMBERS_SORTED" \
  || { echo "sqlc.sh: verified archive has unexpected member paths" >&2; exit 2; }
tar -tvf "$UNPACKED" > "$VERBOSE_MEMBERS" \
  || { echo "sqlc.sh: could not inspect verified archive member types" >&2; exit 2; }
awk 'NF == 0 || substr($1, 1, 1) != "-" { bad = 1 } END { exit(bad || NR != 1) }' "$VERBOSE_MEMBERS" \
  || { echo "sqlc.sh: verified archive contains a non-regular or extra member" >&2; exit 2; }

# Apply the same conservative limit during extraction. This catches PAX/GNU
# sparse members whose logical size can exceed the complete tar-stream size.
(ulimit -f "$MAX_TAR_BLOCKS" && tar -xf "$UNPACKED" -C "$TMP") \
  || { echo "sqlc.sh: extraction failed or exceeded the per-file size cap" >&2; exit 2; }
BIN="$TMP/sqlc"
if [ ! -x "$BIN" ]; then
  echo "sqlc.sh: verified archive did not contain an executable at the expected path" >&2
  exit 2
fi
binary_bytes="$(wc -c < "$BIN" | tr -d ' ')"
[ "$binary_bytes" -gt 0 ] && [ "$binary_bytes" -le "$MAX_TAR_BYTES" ] \
  || { echo "sqlc.sh: extracted binary has invalid size $binary_bytes" >&2; exit 2; }
# Populate the cache only after validation. This invocation uses the private
# verified snapshot; later invocations reverify the cached bytes.
if [ "$downloaded" -eq 1 ]; then
  STAGE="$(mktemp -d "$DIR/.stage.XXXXXX")" || { echo "sqlc.sh: cache staging failed" >&2; exit 2; }
  if ! (ulimit -f "$MAX_ARCHIVE_BLOCKS" && cp "$CANDIDATE" "$STAGE/archive") \
    || ! verify_sha256 "$tar_sha256" "$STAGE/archive" \
    || ! mv -f "$STAGE/archive" "$ARCHIVE"; then
    echo "sqlc.sh: cache publication failed" >&2
    exit 2
  fi
  rmdir "$STAGE"
  STAGE=""
fi

# Keep the child in the foreground so its status is preserved. POSIX shells defer
# signal traps while waiting; cleanup runs when that foreground command returns.
rc=0
"$BIN" "$@" || rc=$?
exit "$rc"
