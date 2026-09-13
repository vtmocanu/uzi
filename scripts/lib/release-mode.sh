#!/usr/bin/env bash
# Shared release-mode helpers for the release-train scripts (PRD 1265). PURE string
# functions — no git, no network — so the fixture test (scripts/release-scripts-test.sh)
# covers them offline and release-watch.sh / release-verify.sh agree on what "an RC" is.
# SOURCE this file; it defines functions and runs nothing on its own.
#
#   release_mode <version>   -> "rc" if the version carries a prerelease suffix, else
#                               "stable". A leading v is tolerated.
#   release_base <version>   -> the stable base X.Y.Z (strips any prerelease suffix).
#
# One shape only (D2): a prerelease is X.Y.Z-rc.N, so any `-` means a prerelease. Keeping
# this in one place is the point — the watch (which workflows to wait for) and the verify
# (whether releases/latest may equal the tag) must never disagree about a given tag.

release_mode() {
  local v="${1#v}"
  case "$v" in
    *-*) printf 'rc\n' ;;
    *)   printf 'stable\n' ;;
  esac
}

release_base() {
  local v="${1#v}"
  printf '%s\n' "${v%%-*}"
}
