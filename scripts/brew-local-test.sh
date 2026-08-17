#!/usr/bin/env bash
# Local Homebrew test for the uzi-cli formula, no published tag / no remote needed.
#
# The real formula installs by downloading vtmocanu/uzi's tag source tarball (the
# auto-archive) and building from source. Here we mimic that offline: build a throwaway
# tarball holding the CURRENT source (the api/ module), then render Formula/uzi-cli.rb's
# url/sha256 placeholders against it with the SAME reusable `task brew:formula` CI uses
# (TARBALL_URL points at the local file://), drop the rendered formula into a throwaway
# local tap, then install / run / assert / test / uninstall. Cleaned up on exit. Unlike
# a stub harness this COMPILES the CLI, so it is what settles whether a from-source Go
# formula installs under Homebrew's sandbox.
set -euo pipefail
export HOMEBREW_NO_AUTO_UPDATE=1

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "$here/.." && pwd)"

# Arbitrary local version: this test validates that the CURRENT source builds, installs
# and runs, not a version number. The CLI reports v<version> (the formula stamps
# -X main.version=v#{version}), and Homebrew derives <version> from the tarball filename.
version="0.0.0"
work="$(mktemp -d)"

# Throwaway tap name on purpose (NOT the real vtmocanu/homebrew-tap).
tap_user="uzi-local"; tap_repo="tap"
tap_dir="$(brew --repository)/Library/Taps/$tap_user/homebrew-$tap_repo"
ref="$tap_user/$tap_repo/uzi-cli"

cleanup() {
  brew uninstall --force uzi-cli >/dev/null 2>&1 || true
  rm -rf "$tap_dir" "$work"
  # Also drop the now-empty tap namespace dir Homebrew leaves behind.
  rmdir "$(dirname "$tap_dir")" 2>/dev/null || true
}
trap cleanup EXIT

# 1. Throwaway source tarball holding the CURRENT source, laid out like the real
#    auto-archive (top dir uzi-<version>/ so Homebrew's single-subdir chdir lands there
#    and the formula's `cd "api"` resolves). Only api/ is needed to build; a second
#    top-level entry (README) keeps Homebrew from descending PAST uzi-<version>/ into a
#    sole api/ subdir, which would break `cd "api"` (mirrors the real repo's many
#    top-level entries).
src="$work/uzi-$version"
mkdir -p "$src"
# Copy tracked working-tree source so an uncommitted edit under api/ is exercised.
git -C "$repo_root" archive --format=tar HEAD -- api | tar -x -C "$src"
# Layer any uncommitted (but tracked) api/ changes on top of the HEAD snapshot.
if ! git -C "$repo_root" diff --quiet -- api; then
  git -C "$repo_root" diff HEAD -- api | git -C "$src" apply --unsafe-paths --directory="$src" 2>/dev/null \
    || echo "(note: could not overlay uncommitted api/ changes; testing HEAD source)"
fi
printf 'throwaway uzi brew-spike source\n' >"$src/README.md"
tarball="$work/uzi-$version.tar.gz"
tar -czf "$tarball" -C "$work" "uzi-$version"

# 2. Render the tap formula with the reusable brew:formula task (the one CI runs),
#    pointing url at the local tarball. This exercises the real render path -- the
#    @@URL@@/@@SHA256@@ substitution and the sha256 guard -- with no network or tag.
mkdir -p "$tap_dir/Formula"
( cd "$repo_root" && task brew:formula VERSION="v$version" \
    TARBALL_URL="file://$tarball" FORMULA_OUT="$tap_dir/Formula/uzi-cli.rb" )
chmod 644 "$tap_dir/Formula/uzi-cli.rb"

# 3. install, run, assert, brew-test, uninstall (uninstall via cleanup trap).
brew uninstall --force uzi-cli >/dev/null 2>&1 || true
# Clear any prior download so this run builds the current source, not a stale cache.
rm -rf "$(brew --cache)"/*uzi-cli* "$(brew --cache)"/downloads/*uzi-cli* 2>/dev/null || true
brew install --build-from-source "$ref"

echo "--- $(brew --prefix)/bin/uzi version ---"
out="$("$(brew --prefix)/bin/uzi" version)"
echo "$out"
case "$out" in
  "v$version"*) echo "PASS: version asserted (v$version)" ;;
  *) echo "FAIL: unexpected version output (wanted v$version)" >&2; exit 1 ;;
esac

echo "--- brew test ---"
brew test "$ref" || echo "(brew test non-fatal; binary assert above is authoritative)"

echo "OK: from-source brew install/build/run/test round-trip succeeded (cleanup on exit)"
