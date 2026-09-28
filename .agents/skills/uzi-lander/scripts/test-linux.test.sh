#!/usr/bin/env bash
# test-linux.test.sh — test-linux.sh keeps a broken harness (exit 2) apart from a failed test
# (exit 1). Hermetic: a stub `docker` on PATH, no container runs.
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SCRIPT="$HERE/test-linux.sh"
WORK=$(mktemp -d); trap 'rm -rf "$WORK"' EXIT
fail() { echo "FAIL: $*" >&2; exit 1; }
mkdir -p "$WORK/bin"
cat > "$WORK/bin/docker" <<'STUB'
#!/usr/bin/env bash
case "$1" in
  info) [ "$MODE" = daemon_down ] && exit 1; exit 0 ;;
  rm) exit 0 ;;
  run)
    case "$MODE" in
      refused) echo "docker: error during connect" >&2; exit 1 ;;
      tests_fail) echo "FAIL x.test.sh"; echo "LANDER_TESTS_RC=1"; exit 0 ;;
      tests_pass) echo "PASS x.test.sh"; echo "LANDER_TESTS_RC=0"; exit 0 ;;
      spoof) echo "FAIL x.test.sh"; echo "    LANDER_TESTS_RC=0"; echo "LANDER_TESTS_RC=1"; exit 0 ;;
    esac ;;
esac
exit 0
STUB
chmod +x "$WORK/bin/docker"
export PATH="$WORK/bin:$PATH"

run() { MODE="$1"; export MODE; set +e; "$SCRIPT" x.test.sh > "$WORK/$1.out" 2>&1; rc=$?; set -e; }

run daemon_down; [ "$rc" -eq 2 ] || fail "daemon down: want 2, got $rc: $(cat "$WORK/daemon_down.out")"
run refused;     [ "$rc" -eq 2 ] || fail "docker run refused: want 2, got $rc: $(cat "$WORK/refused.out")"
run tests_fail;  [ "$rc" -eq 1 ] || fail "a failing test: want 1, got $rc"
run tests_pass;  [ "$rc" -eq 0 ] || fail "passing tests: want 0, got $rc"
run spoof;       [ "$rc" -eq 1 ] || fail "an indented test-output marker was read as the result, rc=$rc"
echo "PASS test-linux: daemon down and refused run exit 2, failing tests 1, passing 0, indented marker ignored"
