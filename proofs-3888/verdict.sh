#!/usr/bin/env bash
# shellcheck disable=SC2154  # inputs come from the caller (run.sh or run_parser_test.sh)
# Verdict logic of run.sh, kept in a file so proofs-3888/run_parser_test.sh can feed it synthetic logs.
# judge needs: log KIND MODE rc NAMES MSG_RE PASS_NAMES PASS_COUNT and a step() function. Returns 0 only for a good verdict.
judge() {
  # block <fail|pass> <name>: the named test's result line and the lines indented under it (exact name).
  block() {
    awk -v kind="$KIND" -v want="$1" -v n="$2" '
      function ind(l) { match(l, /^ */); return RLENGTH }
      function hit(l,   t) {
        if (kind == "go") { t = (want == "fail") ? "--- FAIL: " : "--- PASS: "; return index(l, t n " (") == ind(l) + 1 }
        t = l; sub(/^ *(not )?ok [0-9]+ - /, "", t)
        if ((want == "fail") != (l ~ /^ *not ok /)) return 0
        if (want == "pass" && t ~ / # (SKIP|TODO)/) return 0
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
  [ "${#entries[@]}" -gt 0 ] || { step "FAIL: NAMES is empty"; return 1; }

  if [ "$MODE" = fail ]; then
    [ "$rc" -ne 0 ] || { tail -n 40 "$log"; step "FAIL: the unfixed variant passed (exit 0)"; return 1; }
    if [ "$KIND" = go ] && grep -q -E '^panic: |^fatal error: |test timed out after' "$log"; then
      step "MISS the log holds a panic, fatal error or test timeout; no failure is credited"; grep -n -E '^panic: |^fatal error: |test timed out after' "$log" | head -3; return 1
    fi
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
    [ "$rc" -eq 0 ] || { tail -n 60 "$log"; step "FAIL: the candidate failed (exit $rc)"; return 1; }
    for e in "${entries[@]}"; do
      n=${e%%@@*}
      c=$(block pass "$n" | grep -c -E "^ *(--- PASS: |ok [0-9]+ - )" || true)
      if [ -n "${PASS_COUNT:-}" ]; then
        if [ "${c:-0}" -eq "$PASS_COUNT" ]; then step "ok   $n passed $c times"; else step "MISS $n passed ${c:-0} times, want $PASS_COUNT"; verdict=1; fi
      elif [ "${c:-0}" -ge 1 ]; then step "ok   passed: $n"; else step "MISS no PASS line: $n"; verdict=1; fi
    done
    if grep -E -q -- '--- FAIL|^ *not ok|--- SKIP|^# skipped [1-9]|^ *(not )?ok [0-9]+ - .* # (SKIP|TODO)' "$log"; then step "MISS a failure or skip appears in the log"; verdict=1; fi
  fi
  step "done verdict=$verdict"
  return "$verdict"
}
