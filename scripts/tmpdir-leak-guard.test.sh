#!/bin/sh
# Hermetic contract tests for scripts/tmpdir-leak-guard.sh (issue #2020): a leftover is
# reported with its ledger evidence and a verdict, and the exit codes do not change.
#
# Each case runs the guard around a FAKE command that leaves entries in its TMPDIR and
# writes lines to "$UZI_TMPDIR_GUARD_LEDGER", with the guard's own TMPDIR pointed at a
# directory this test owns so it can prove the scratch dir and the ledger are removed.
set -eu

ROOT="$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)"
GUARD="${UZI_TMPDIR_GUARD_UNDER_TEST:-$ROOT/scripts/tmpdir-leak-guard.sh}"
# Portable template form (check:mktemp-portability): a full path with 6 X's, no -t.
TMP="$(mktemp -d "${TMPDIR:-/tmp}/uzi-tmpdir-guard-test.XXXXXX")"
# shellcheck disable=SC2329 # invoked by the EXIT trap below
cleanup() {
  chmod -R u+rwx "$TMP" 2>/dev/null || true
  rm -rf "$TMP"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

OUTER="$TMP/outer"
mkdir -p "$OUTER"

# The fake commands. `leave NAME MODE` creates NAME/data/x in its TMPDIR; MODE is
# none | created | removed (created and removed lines) | backslash (a created line whose
# test name carries JSON-escaped backslashes) | longer (created and removed
# lines for NAME plus one more character, which must NOT be attributed to NAME). `clean` leaves nothing. `fail RC`
# exits RC. `probe FILE` records the ledger path and the TMPDIR the command saw.
FAKE="$TMP/fake-tests"
cat > "$FAKE" <<'EOS'
#!/bin/sh
case "$1" in
  leave)
    mkdir -p "$TMPDIR/$2/data"
    : > "$TMPDIR/$2/data/x"
    case "$3" in
      created | removed)
        printf '{"entry":"%s","path":"%s/%s","event":"created","pid":1,"at":"t","file":"/w/fake.test.ts","test":"fake test name","site":[]}\n' "$2" "$TMPDIR" "$2" >> "$UZI_TMPDIR_GUARD_LEDGER"
        ;;
    esac
    case "$3" in
      removed)
        printf '{"entry":"%s","path":"%s/%s","event":"removed","pid":1,"at":"t"}\n' "$2" "$TMPDIR" "$2" >> "$UZI_TMPDIR_GUARD_LEDGER"
        ;;
      backslash)
        printf '{"entry":"%s","path":"%s/%s","event":"created","pid":1,"at":"t","file":"/w/bs.test.ts","test":"strips \\\\c and \\\\n","site":[]}\n' "$2" "$TMPDIR" "$2" >> "$UZI_TMPDIR_GUARD_LEDGER"
        ;;
      longer)
        printf '{"entry":"%sX","path":"%s/%sX","event":"created","pid":1,"at":"t","file":"/w/other.test.ts","test":"other","site":[]}\n' "$2" "$TMPDIR" "$2" >> "$UZI_TMPDIR_GUARD_LEDGER"
        printf '{"entry":"%sX","path":"%s/%sX","event":"removed","pid":1,"at":"t"}\n' "$2" "$TMPDIR" "$2" >> "$UZI_TMPDIR_GUARD_LEDGER"
        ;;
    esac
    exit 0
    ;;
  clean) exit 0 ;;
  fail) exit "$2" ;;
  probe)
    printf '%s\n%s\n' "$UZI_TMPDIR_GUARD_LEDGER" "$TMPDIR" > "$2"
    exit 0
    ;;
esac
exit 99
EOS
chmod +x "$FAKE"

cases=0
passed=0
rc=0
ERR="$TMP/stderr"

run_guard() {
  rc=0
  TMPDIR="$OUTER" "$GUARD" "$FAKE" "$@" > /dev/null 2> "$ERR" || rc=$?
}

# check NAME CONDITION...: tally one assertion.
check() {
  name="$1"
  shift
  cases=$((cases + 1))
  if "$@"; then
    passed=$((passed + 1))
  else
    echo "FAIL: $name" >&2
    echo "--- guard stderr ---" >&2
    cat "$ERR" >&2
    echo "--------------------" >&2
  fi
}
rc_is() { [ "$rc" -eq "$1" ]; }
err_has() { grep -qF -- "$1" "$ERR"; }
err_lacks() { ! grep -qF -- "$1" "$ERR"; }
err_line() { grep -qxF -- "$1" "$ERR"; }
outer_empty() { [ -z "$(ls -A "$OUTER")" ]; }

# a. created line only: attribution, honest verdict, listing.
run_guard leave uzi-agent-test-AAAAAA created
check "a: rc=1" rc_is 1
check "a: file shown" err_has "/w/fake.test.ts"
check "a: test shown" err_has "fake test name"
check "a: verdict" err_has "no successful cleanup recorded"
check "a: listing" err_has "uzi-agent-test-AAAAAA/data/x"
check "a: listing is relative to scratch" err_line "      uzi-agent-test-AAAAAA/data/x"
check "a: no recreated verdict" err_lacks "cleanup ran; the directory was recreated afterwards"
check "a: no missing-creator verdict" err_lacks "no creator recorded in the ledger"
check "a: scratch and ledger removed" outer_empty

# b. created + removed, dir still present.
run_guard leave uzi-agent-test-BBBBBB removed
check "b: rc=1" rc_is 1
check "b: verdict" err_has "cleanup ran; the directory was recreated afterwards"
check "b: no created-only verdict" err_lacks "no successful cleanup recorded"
check "b: scratch and ledger removed" outer_empty

# c. no ledger lines at all.
run_guard leave uzi-agent-test-CCCCCC none
check "c: rc=1" rc_is 1
check "c: verdict" err_has "no creator recorded in the ledger"
check "c: no other verdict" err_lacks "cleanup ran; the directory was recreated afterwards"
check "c: scratch and ledger removed" outer_empty

# g. a ledger entry whose name merely starts with the leftover's name is not its creator.
run_guard leave uzi-agent-test-GGGGGG longer
check "g: rc=1" rc_is 1
check "g: verdict" err_has "no creator recorded in the ledger"
check "g: other entry not attributed" err_lacks "/w/other.test.ts"
check "g: scratch and ledger removed" outer_empty

# h. backslashes in a ledger line print verbatim (an echo under dash would eat `\c`).
run_guard leave uzi-agent-test-HHHHHH backslash
check "h: rc=1" rc_is 1
check "h: backslashes verbatim" err_has '"test":"strips \\c and \\n"'
check "h: line not truncated" err_has 'and \\n","site":[]}'
check "h: scratch and ledger removed" outer_empty

# d. clean command.
run_guard clean
check "d: rc=0" rc_is 0
check "d: scratch and ledger removed" outer_empty

# e. failing command keeps its own status.
run_guard fail 7
check "e: rc=7" rc_is 7
check "e: scratch and ledger removed" outer_empty

# f. the ledger variable reaches the command and lives outside the scratch TMPDIR.
SEEN="$TMP/seen"
run_guard probe "$SEEN"
seen_ledger="$(sed -n 1p "$SEEN")"
seen_tmpdir="$(sed -n 2p "$SEEN")"
check "f: rc=0" rc_is 0
check "f: ledger var set" test -n "$seen_ledger"
check "f: ledger outside scratch" test "${seen_ledger#"$seen_tmpdir"/}" = "$seen_ledger"

echo "cases=$cases passed=$passed"
if [ "$cases" -eq 0 ] || [ "$passed" -ne "$cases" ]; then
  exit 1
fi
