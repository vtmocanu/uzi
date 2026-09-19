#!/usr/bin/env bash
# The ONE definition of "does this path ship" for the release train. SOURCE this file;
# it defines a function and runs nothing on its own.
#
#   is_shipping <path>   -> 0 if the path is release-shipping code (api, agent/src,
#                           controller, web app, chart, docs), 1 otherwise (tests, e2e,
#                           fixtures, and everything else — build glue, skills, prds, root
#                           files). It classifies by PATH only; message-based exemptions
#                           (`docs(...):`, `chore(release):`, `Changelog: none`) live in
#                           scripts/assert-changelog-covers-release.sh, the coverage oracle.
#
# Two consumers share this so "shipping" cannot drift between them:
#   - scripts/assert-changelog-covers-release.sh: which merges must be cited.
#   - .agents/skills/uzi-release/scripts/release-cut.sh: whether a --promote has anything
#     worth cutting a next candidate for (promote-only fires when NO shipping commit and an
#     empty [Unreleased]) — a docs-only commit after the RC must not force an empty next RC.

is_shipping() {
  case "$1" in
    *_test.go|*.test.ts|*.test.tsx|*/testdata/*|*/test/*|e2e/*|fixtures/*) return 1 ;;
    api/*|agent/src/*|controller/*|web/src/*|deploy/chart/*|docs/*) return 0 ;;
    *) return 1 ;;
  esac
}
