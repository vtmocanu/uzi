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

# block <fail|pass> <name>: the named test's result line and the lines indented under it (exact name).
block() {
  awk -v kind="$KIND" -v want="$1" -v n="$2" '
    function ind(l) { match(l, /^ */); return RLENGTH }
    function hit(l,   t) {
      if (kind == "go") { t = (want == "fail") ? "--- FAIL: " : "--- PASS: "; return index(l, t n " (") == ind(l) + 1 }
      t = l; sub(/^ *(not )?ok [0-9]+ - /, "", t)
      if ((want == "fail") != (l ~ /^ *not ok /)) return 0
      return t == n || index(t, n " #") == 1
    }
    on { if (ind($0) > base || $0 ~ /^ *$/) { print; next } on = 0 }
    hit($0) { print; base = ind($0); on = 1 }
  ' "$log"
}

# detail <name>: go -v prints a test's t.Fatal/t.Error lines under its own "=== RUN" (or "=== CONT") header,
# before the summary, so the evidence for a go test is those segments; node keeps it in its block.
detail() {
  if [ "$KIND" != go ]; then block fail "$1"; return; fi
  block fail "$1"
  awk -v n="$1" '
    /^=== / { on = ($0 == "=== RUN   " n || $0 == "=== CONT  " n); next }
    /^--- |^(ok|FAIL|PASS)/ { on = 0 }
    on { print }
  ' "$log"
}

verdict=0
IFS=';' read -r -a entries <<<"${NAMES:-}"
[ "${#entries[@]}" -gt 0 ] || die "NAMES is empty"

if [ "$MODE" = fail ]; then
  [ "$rc" -ne 0 ] || { tail -n 40 "$log"; die "the unfixed variant passed (exit 0)"; }
  for e in "${entries[@]}"; do
    n=${e%%@@*}; re=$MSG_RE; [ "$e" != "$n" ] && re=${e#*@@}
    r=$(block fail "$n")
    if [ -z "$r" ]; then step "MISS no FAIL result for: $n"; verdict=1; continue; fi
    b=$(detail "$n")
    if [ "$KIND" = go ]; then
      if grep -q -E '^panic: |test timed out after' <<<"$b"; then step "MISS $n: panic or timeout, not an assertion"; verdict=1; continue; fi
    elif ! grep -q -E "failureType: '(testCodeFailure|subtestsFailed)'" <<<"$b"; then
      step "MISS $n: not a testCodeFailure/subtestsFailed (cancellation or timeout?)"; verdict=1; continue
    fi
    if grep -q -E -- "$re" <<<"$b"; then step "ok   $n failed with /$re/"; else step "MISS $n failed without /$re/"; verdict=1; fi
    printf '%s\n' "$b" | grep -v -E 'duration_ms|^ *(stack|at )|location:' | head -8 | cut -c1-220 | sed 's/^/       | /'
  done
  if [ -n "${PASS_NAMES:-}" ]; then
    IFS=';' read -r -a keep <<<"$PASS_NAMES"
    for n in "${keep[@]}"; do
      if [ -n "$(block pass "$n")" ]; then step "ok   still passes: $n"; else step "MISS should still pass (and did not): $n"; verdict=1; fi
    done
  fi
else
  [ "$rc" -eq 0 ] || { tail -n 60 "$log"; die "the candidate failed (exit $rc)"; }
  for e in "${entries[@]}"; do
    n=${e%%@@*}
    c=$(block pass "$n" | grep -c -E "^ *(--- PASS: |ok [0-9]+ - )" || true)
    if [ -n "${PASS_COUNT:-}" ]; then
      if [ "${c:-0}" -eq "$PASS_COUNT" ]; then step "ok   $n passed $c times"; else step "MISS $n passed ${c:-0} times, want $PASS_COUNT"; verdict=1; fi
    elif [ "${c:-0}" -ge 1 ]; then step "ok   passed: $n"; else step "MISS no PASS line: $n"; verdict=1; fi
  done
  if grep -E -q -- '--- FAIL|^ *not ok|--- SKIP|^# skipped [1-9]' "$log"; then step "MISS a failure or skip appears in the log"; verdict=1; fi
fi
step "done verdict=$verdict"
exit "$verdict"
