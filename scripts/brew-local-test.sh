#!/usr/bin/env bash
# Local Homebrew test for the uzi-cli formula, no published tag / no remote needed.
#
# The real formula installs by cloning vtmocanu/uzi over SSH at a vX.Y.Z tag and
# building from source. Here we mimic that without a remote or a real tag: build a
# throwaway local git repo holding the CURRENT source (the api/ module), point the
# formula's `url` at it (file:// git url) at a matching tag, drop the formula into a
# throwaway local tap, then install / run / assert / test / uninstall. Cleaned up
# on exit. Unlike the reference harness this COMPILES the CLI, so it is the M4 spike that
# settles whether a from-source Go formula installs under Homebrew's sandbox.
set -euo pipefail
export HOMEBREW_NO_AUTO_UPDATE=1

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "$here/.." && pwd)"
formula="$repo_root/Formula/uzi-cli.rb"
# Version comes from the formula's `tag: "vX.Y.Z"` (Homebrew strips the leading v
# to derive the version). The throwaway repo is tagged vX.Y.Z to match.
version="$(sed -n 's/.*tag: "v\([^"]*\)".*/\1/p' "$formula")"
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

# 1. throwaway git repo holding the CURRENT source, tagged vX.Y.Z. This stands in
#    for the vtmocanu/uzi repo that Homebrew clones over SSH in production. Only the
#    api/ module is needed to build; a second top-level entry (README) keeps
#    Homebrew from descending into a sole subdirectory (which would break the
#    formula's `cd "api"`, mirroring the real repo having many top-level entries).
src="$work/src"
mkdir -p "$src"
# Copy tracked working-tree source so an uncommitted edit under api/ is exercised.
git -C "$repo_root" archive --format=tar HEAD -- api | tar -x -C "$src"
# Layer any uncommitted (but tracked) api/ changes on top of the HEAD snapshot.
if ! git -C "$repo_root" diff --quiet -- api; then
  git -C "$repo_root" diff HEAD -- api | git -C "$src" apply --unsafe-paths --directory="$src" 2>/dev/null \
    || echo "(note: could not overlay uncommitted api/ changes; testing HEAD source)"
fi
printf 'throwaway uzi brew-spike source\n' >"$src/README.md"
git -C "$src" init -q
git -C "$src" -c user.email=t@t -c user.name=t add -A
git -C "$src" -c user.email=t@t -c user.name=t commit -q -m "spike $version"
git -C "$src" tag "v$version"

# 2. local tap with the formula, url repointed from git@... to the local repo.
mkdir -p "$tap_dir/Formula"
sed 's|url "git@[^"]*"|url "file://'"$src"'"|' "$formula" >"$tap_dir/Formula/uzi-cli.rb"
chmod 644 "$tap_dir/Formula/uzi-cli.rb"

# 3. install, run, assert, brew-test, uninstall (uninstall via cleanup trap).
brew uninstall --force uzi-cli >/dev/null 2>&1 || true
# Homebrew's git download strategy caches the clone by FORMULA name
# (~/Library/Caches/Homebrew/uzi-cli--git), keyed on the formula+resource, NOT the
# url. Left alone it reuses a PRIOR run's commit and silently builds stale source.
# Clear it so this run compiles the current api/.
rm -rf "$(brew --cache)/uzi-cli--git" 2>/dev/null || true
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
