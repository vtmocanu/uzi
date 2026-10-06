#!/usr/bin/env bash
# The ONE definition of "a dependency bump that is cited in bulk at cut time". SOURCE this
# file; it defines functions and runs nothing on its own.
#
#   is_dependency_manifest <path>  -> 0 if a SHIPPING path may change in a dependency bump
#                                     without its own CHANGELOG line (basename go.mod, go.sum,
#                                     Dockerfile or devbox.lock; never deploy/chart/**).
#   is_dependency_subject <subject> -> 0 if a commit subject is dependency-typed
#                                     (`chore(deps):` / `fix(deps):` / `build(deps):`, `!` ok).
#
# Two consumers share this so the exemption and the citation cannot drift:
#   - scripts/check-changelog-entry.sh: spares a dependency-only PR its per-PR entry.
#   - .agents/skills/uzi-release/scripts/release-cut.sh: cites exactly those merges in one
#     bullet at cut time, so the coverage oracle (unchanged) sees them accounted for.

is_dependency_manifest() {
  case "$1" in deploy/chart/*) return 1 ;; esac
  case "${1##*/}" in
    go.mod|go.sum|Dockerfile|devbox.lock) return 0 ;;
    *) return 1 ;;
  esac
}

# A bash regex, not `printf | grep -q`: grep -q exits on the first match, and under the
# callers' `set -o pipefail` the SIGPIPEd printf would turn a match into a failure.
DEPENDENCY_SUBJECT_ERE='^(chore|fix|build)\(deps\)!?:'
is_dependency_subject() {
  [[ "$1" =~ $DEPENDENCY_SUBJECT_ERE ]]
}
