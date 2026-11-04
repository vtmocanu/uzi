#!/usr/bin/env bash
# CI-only proof runner for PR #3888 (never merged). One matrix leg = one call.
#
# Inputs (environment, set by .github/workflows/proof-3888.yml):
#   KIND       go | node
#   MODE       fail  (an unfixed variant: pass ONLY if the named tests ran AND failed with the expected text)
#              pass  (the candidate: pass ONLY if the named tests ran AND passed)
#   PATCH      optional file under proofs-3888/variants/ applied (git apply) before the run
#   NAMES      ';'-separated test names. fail mode: each must print a FAIL line. pass mode: each must print a PASS line.
#   PASS_NAMES fail mode only, optional: names that must still PASS under this variant
#   MSG_RE     fail mode: extended regex that must occur in the log (the expected assertion text)
#   PASS_COUNT pass mode, optional: exact number of PASS lines per name (stability runs)
#   go:   GO_PKG (e.g. ./internal/handler), GO_RUN (-run regex), GO_FLAGS (extra flags, word-split)
#   node: NODE_FILES (space-separated paths under agent/)
# The inversion is explicit: a skipped, missing or passing test in fail mode is a FAILED leg.
set -uo pipefail

root=$(git rev-parse --show-toplevel)
cd "$root" || exit 1
log=$(mktemp "${RUNNER_TEMP:-/tmp}/proof-log.XXXXXX")
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
step "test command exit=$rc"

# line_count <fail|pass> <name>: number of result lines for the named test.
line_count() {
  local want=$1 name=$2
  if [ "$KIND" = go ]; then
    local tag=PASS; [ "$want" = fail ] && tag=FAIL
    grep -c -F -- "--- $tag: $name (" "$log"
  else
    local re='^[[:space:]]*ok [0-9]+ - '; [ "$want" = fail ] && re='^[[:space:]]*not ok [0-9]+ - '
    grep -E -- "$re" "$log" | grep -c -F -- "$name"
  fi
}

# A node test name can be a substring of a sibling; the names below are chosen to be unambiguous.
verdict=0
IFS=';' read -r -a names <<<"${NAMES:-}"
[ "${#names[@]}" -gt 0 ] || die "NAMES is empty"

if [ "$MODE" = fail ]; then
  [ "$rc" -ne 0 ] || { echo "--- log tail"; tail -n 40 "$log"; die "the unfixed variant passed (exit 0)"; }
  for n in "${names[@]}"; do
    c=$(line_count fail "$n" || true)
    if [ "${c:-0}" -ge 1 ]; then step "ok   failed as expected: $n"; else step "MISS did not fail (or did not run): $n"; verdict=1; fi
  done
  if [ -n "${PASS_NAMES:-}" ]; then
    IFS=';' read -r -a keep <<<"$PASS_NAMES"
    for n in "${keep[@]}"; do
      c=$(line_count pass "$n" || true)
      if [ "${c:-0}" -ge 1 ]; then step "ok   still passes: $n"; else step "MISS should still pass (and did not): $n"; verdict=1; fi
    done
  fi
  if grep -E -q -- "$MSG_RE" "$log"; then step "ok   expected assertion text found: $MSG_RE"; else step "MISS expected assertion text absent: $MSG_RE"; verdict=1; fi
  echo "--- matching lines"
  grep -E -- "--- FAIL|^[[:space:]]*not ok|$MSG_RE" "$log" | cut -c1-300 | head -60
else
  [ "$rc" -eq 0 ] || { echo "--- log tail"; tail -n 60 "$log"; die "the candidate failed (exit $rc)"; }
  for n in "${names[@]}"; do
    c=$(line_count pass "$n" || true)
    want=${PASS_COUNT:-}
    if [ -n "$want" ]; then
      if [ "${c:-0}" -eq "$want" ]; then step "ok   $n passed $c times"; else step "MISS $n passed ${c:-0} times, want $want"; verdict=1; fi
    elif [ "${c:-0}" -ge 1 ]; then step "ok   passed: $n"; else step "MISS no PASS line: $n"; verdict=1; fi
  done
  if grep -E -q -- '--- FAIL|^[[:space:]]*not ok|--- SKIP|^# skipped [1-9]' "$log"; then step "MISS a failure or skip appears in the log"; verdict=1; fi
fi
step "done verdict=$verdict"
exit "$verdict"
