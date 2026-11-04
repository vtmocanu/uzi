#!/usr/bin/env bash
# CI-only proof runner for PR #3888 (never merged). One matrix leg = one call.
#
# Inputs (environment, set by .github/workflows/proof-3888.yml):
#   LEG        leg name (the full log is saved as proof-logs/<LEG>.log)
#   KIND       go | node
#   MODE       fail  (an unfixed variant: pass ONLY if the named tests ran AND failed with the expected text)
#              pass  (the candidate: pass ONLY if the named tests ran AND passed)
#   PATCH      optional file under proofs-3888/variants/ applied (git apply) before the run
#   NAMES      ';'-separated entries 'test name' or 'test name@@regex'. A go subtest is 'Parent/child'.
#              fail mode: the test must print its own FAIL line, and THAT test's block (the result line plus the
#              lines indented under it) must be a real assertion failure (go: no panic or timeout; node:
#              failureType testCodeFailure or subtestsFailed, never a cancellation or timeout) and match the
#              entry's regex (default MSG_RE). pass mode: each name must print a PASS line.
#   PASS_NAMES fail mode, optional: ';'-separated names that must still PASS under this variant
#   MSG_RE     default per-entry regex (extended)
#   PASS_COUNT pass mode, optional: exact number of PASS lines per name (stability runs)
#   go:   GO_PKG, GO_RUN (-run regex), GO_FLAGS (extra flags, word-split)
#   node: NODE_FILES (space-separated paths under agent/)
# The inversion is explicit: a skipped, missing or passing test in fail mode is a FAILED leg.
set -uo pipefail

root=$(git rev-parse --show-toplevel)
cd "$root" || exit 1
mkdir -p proof-logs
log=proof-logs/${LEG:-leg}.log
t0=$(date +%s)
step() { printf '[%4ss] %s\n' "$(( $(date +%s) - t0 ))" "$*"; }
die() { step "FAIL: $*"; exit 1; }

: "${KIND:?}" "${MODE:?}"
if [ -n "${PATCH:-}" ]; then
  step "apply proofs-3888/variants/$PATCH"
  git apply --check "proofs-3888/variants/$PATCH" || die "patch does not apply"
  git apply "proofs-3888/variants/$PATCH" || die "patch apply"
  git diff --stat
fi

if [ "$KIND" = node ]; then
  # A mutant must fail its named tests, not the production typecheck (validate-agent runs this gate).
  step "typecheck the (patched) agent"
  (cd agent && npm run typecheck) >"proof-logs/${LEG:-leg}.typecheck.log" 2>&1 || { tail -n 20 "proof-logs/${LEG:-leg}.typecheck.log"; die "the variant does not typecheck"; }
fi

step "run tests ($KIND, mode=$MODE)"
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null GOFLAGS=-buildvcs=false
rc=0
case "$KIND" in
  go)
    read -r -a flags <<<"${GO_FLAGS:-}"
    (cd api && go test "$GO_PKG" -run "$GO_RUN" -v "${flags[@]}") >"$log" 2>&1 || rc=$?
    ;;
  node)
    read -r -a files <<<"$NODE_FILES"
    (cd agent && node --import tsx --test --test-reporter=tap --test-timeout=120000 "${files[@]}") >"$log" 2>&1 || rc=$?
    ;;
  *) die "unknown KIND $KIND" ;;
esac
step "test command exit=$rc (full log: $log)"

# shellcheck disable=SC1091
. "$root/proofs-3888/verdict.sh"
judge
exit $?
