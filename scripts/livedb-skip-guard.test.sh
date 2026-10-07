#!/usr/bin/env bash
# Hermetic exit-code contract for scripts/livedb-skip-guard.sh over synthetic `go test -v`
# logs: a clean pass, a top-level skip, a nested (indented) subtest skip, a deeper nested
# skip, and a log where nothing ran. Prints a `cases=N passed=N` tally and fails below its
# case floor, so a gutted run cannot read green.
set -euo pipefail

ROOT="$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)"
GUARD="$ROOT/scripts/livedb-skip-guard.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
FLOOR=6
cases=0
passed=0

# check NAME WANT_RC LOG_CONTENT
check() {
  local name="$1" want="$2" log="$TMP/$1.log" rc=0
  printf '%s\n' "$3" > "$log"
  "$GUARD" "$log" > /dev/null 2>&1 || rc=$?
  cases=$((cases + 1))
  if [ "$rc" -eq "$want" ]; then
    passed=$((passed + 1))
  else
    echo "FAIL $name: rc=$rc want=$want" >&2
  fi
}

check clean 0 '=== RUN   TestALiveDB
=== RUN   TestALiveDB/case
    --- PASS: TestALiveDB/case (0.01s)
--- PASS: TestALiveDB (0.02s)
ok  	example/pkg	0.10s'

check top-level-skip 1 '=== RUN   TestALiveDB
--- PASS: TestALiveDB (0.02s)
=== RUN   TestBLiveDB
--- SKIP: TestBLiveDB (0.00s)
ok  	example/pkg	0.10s'

check nested-skip 1 '=== RUN   TestALiveDB
=== RUN   TestALiveDB/case
    store_test.go:10: UZI_TEST_DATABASE_URL unset
    --- SKIP: TestALiveDB/case (0.00s)
--- PASS: TestALiveDB (0.02s)
ok  	example/pkg	0.10s'

check deep-nested-skip 1 '=== RUN   TestALiveDB
=== RUN   TestALiveDB/a/b
        --- SKIP: TestALiveDB/a/b (0.00s)
    --- PASS: TestALiveDB/a (0.01s)
--- PASS: TestALiveDB (0.02s)'

check nothing-ran 1 'ok  	example/pkg	0.01s [no tests to run]'

# Usage error: a missing log file.
rc=0
"$GUARD" "$TMP/does-not-exist.log" > /dev/null 2>&1 || rc=$?
cases=$((cases + 1))
if [ "$rc" -eq 2 ]; then passed=$((passed + 1)); else echo "FAIL missing-log: rc=$rc want=2" >&2; fi

echo "cases=$cases passed=$passed"
if [ "$cases" -lt "$FLOOR" ] || [ "$passed" -ne "$cases" ]; then
  exit 1
fi
