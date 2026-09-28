#!/bin/sh
# Hermetic contract tests for scripts/real-procfs-skip-check.sh (issue #1863).
#
# No agent tests and no real proc root: each case runs the wrapper around a FAKE command
# (one that appends skip records to "$UZI_REAL_PROCFS_SKIP_LOG", or exits non-zero) with
# the probe pointed at a throwaway directory (UZI_REAL_PROCFS_PROBE_DIR): a plain mkdtemp
# dir enumerates, a chmod-000 one is denied. That proves the verdict table fires as
# designed, so an edit that guts the zero-skip assertion reddens here instead of shipping a
# gate that stays green while real-procfs coverage vanishes. CI may export CI, so every
# non-CI case runs under `env -u CI`.
set -eu

ROOT="$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)"
CHECK="$ROOT/scripts/real-procfs-skip-check.sh"
# Portable template form (check:mktemp-portability): a full path with 6 X's, no -t.
TMP="$(mktemp -d "${TMPDIR:-/tmp}/uzi-real-procfs-check-test.XXXXXX")"
# shellcheck disable=SC2329 # invoked by the EXIT trap below
cleanup() {
  chmod -R u+rwx "$TMP" 2>/dev/null || true
  rm -rf "$TMP"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

ENUMERABLE="$TMP/enumerable"
DENIED="$TMP/denied"
mkdir -p "$ENUMERABLE" "$DENIED"
chmod 000 "$DENIED"

# The fake commands. `skip N` appends N records and exits 0; `fail RC` exits RC after
# appending one record (the list must still print); `rmlog RC` deletes the skip log and exits RC.
FAKE="$TMP/fake-tests"
cat > "$FAKE" <<'EOF'
#!/bin/sh
case "$1" in
  skip)
    i=0
    while [ "$i" -lt "$2" ]; do
      i=$((i + 1))
      printf 'fake label %s\treal-procfs-denied: fake reason %s\n' "$i" "$i" >> "$UZI_REAL_PROCFS_SKIP_LOG"
    done
    exit 0
    ;;
  fail)
    printf 'failing label\treal-procfs-denied: failing reason\n' >> "$UZI_REAL_PROCFS_SKIP_LOG"
    exit "$2"
    ;;
  rmlog)
    rm -f "$UZI_REAL_PROCFS_SKIP_LOG"
    exit "$2"
    ;;
esac
echo "fake-tests: unknown mode '$1'" >&2
exit 99
EOF
chmod +x "$FAKE"

failures=0
OUT="$TMP/out"

# run_case NAME EXPECTED_RC PROBE_DIR CI_VALUE COMMAND...
# CI_VALUE "-" runs with CI unset (`env -u CI`); anything else exports CI=<value>.
run_case() {
  name="$1" want="$2" probe="$3" ci="$4"
  shift 4
  got=0
  if [ "$ci" = "-" ]; then
    env -u CI UZI_REAL_PROCFS_PROBE_DIR="$probe" "$@" > "$OUT" 2>&1 || got=$?
  else
    env CI="$ci" UZI_REAL_PROCFS_PROBE_DIR="$probe" "$@" > "$OUT" 2>&1 || got=$?
  fi
  if [ "$got" -eq "$want" ]; then
    echo "PASS: $name (exit $got)"
    return 0
  fi
  echo "FAIL: $name: expected exit $want, got $got; output:"
  sed 's/^/    /' "$OUT"
  failures=$((failures + 1))
  return 1
}

expect_output() {
  name="$1" needle="$2"
  if grep -Fq -- "$needle" "$OUT"; then
    echo "PASS: $name"
  else
    echo "FAIL: $name: output lacks: $needle"
    sed 's/^/    /' "$OUT"
    failures=$((failures + 1))
  fi
}

# (a) enumerable, nothing skipped: green.
run_case "enumerable + 0 skips" 0 "$ENUMERABLE" - "$CHECK" "$FAKE" skip 0 &&
  expect_output "enumerable + 0 skips reports the count" "real-procfs skips: 0"

# (b) enumerable, one skip: coverage vanished.
run_case "enumerable + 1 skip" 1 "$ENUMERABLE" - "$CHECK" "$FAKE" skip 1 &&
  expect_output "enumerable + 1 skip names the vanished coverage" "coverage vanished: 1 real-procfs"

# (c) denied, skips allowed with a banner that lists every record. Root reads a chmod-000
# directory anyway, so this case cannot build a denied probe dir under uid 0.
if [ "$(id -u)" -eq 0 ]; then
  echo "SKIP: denied + skips: running as uid 0, which enumerates a chmod-000 dir, so no denied probe dir can be built"
else
  run_case "denied + 2 skips" 0 "$DENIED" - "$CHECK" "$FAKE" skip 2 && {
    expect_output "denied lists the first label and reason" "real-procfs skip: fake label 1 — real-procfs-denied: fake reason 1"
    expect_output "denied lists the second label and reason" "real-procfs skip: fake label 2 — real-procfs-denied: fake reason 2"
    expect_output "denied prints the banner with the count" "2 real-procfs test(s) SKIPPED"
  }
fi

# (d) CI forbids any skip, even where the probe says denied (and on uid 0, where it cannot).
run_case "CI=1 + 1 skip (denied probe)" 1 "$DENIED" 1 "$CHECK" "$FAKE" skip 1 &&
  expect_output "CI names the rule" "real-procfs tests must run in CI"
run_case "CI=1 + 0 skips" 0 "$ENUMERABLE" 1 "$CHECK" "$FAKE" skip 0 || true

# (e) the command's own failure status wins, and the list still prints.
run_case "command rc 7 preserved" 7 "$ENUMERABLE" - "$CHECK" "$FAKE" fail 7 &&
  expect_output "a failed command still lists its skips" "real-procfs skip: failing label — real-procfs-denied: failing reason"

# (f) a probe error other than EACCES/EPERM does not excuse a skip.
run_case "missing probe dir + 1 skip" 1 "$TMP/does-not-exist" - "$CHECK" "$FAKE" skip 1 || true

# (g) a vanished skip log fails closed; a non-zero command status still wins.
run_case "skip log deleted, command rc 0" 1 "$ENUMERABLE" - "$CHECK" "$FAKE" rmlog 0 &&
  expect_output "a deleted skip log is named" "is missing (the command removed it)"
run_case "skip log deleted, command rc 5 preserved" 5 "$ENUMERABLE" - "$CHECK" "$FAKE" rmlog 5 || true

# (h) end to end through the real TypeScript recorder: agent/test/real-procfs.ts's
# realProcfsSkip, run under node + tsx with a forced denied value, must land its record in
# the log this wrapper names, so the enumerable probe dir reports the vanished coverage. A
# renamed env var on either side reddens here. node and tsx are required, never skipped.
AGENT="$ROOT/agent"
if ! command -v node > /dev/null 2>&1; then
  echo "FAIL: end-to-end recorder: node is not on PATH"
  failures=$((failures + 1))
elif [ ! -e "$AGENT/node_modules/tsx" ]; then
  echo "FAIL: end-to-end recorder: tsx is not installed under $AGENT/node_modules (install the agent deps)"
  failures=$((failures + 1))
else
  E2E="$TMP/e2e-recorder.mts"
  cat > "$E2E" <<EOF
import { realProcfsSkip } from "$AGENT/test/real-procfs.ts";
realProcfsSkip("e2e forced", "real-procfs-denied: forced by the e2e case");
EOF
  # shellcheck disable=SC2016 # the single-quoted program is expanded by the inner sh
  run_case "end-to-end recorder + enumerable probe" 1 "$ENUMERABLE" - "$CHECK" \
    sh -c 'cd "$1" && exec node --import tsx "$2"' sh "$AGENT" "$E2E" && {
    expect_output "end-to-end lists the recorded skip" "real-procfs skip: e2e forced — real-procfs-denied: forced by the e2e case [e2e forced]"
    expect_output "end-to-end names the vanished coverage" "coverage vanished: 1 real-procfs"
  }
fi

# (i) usage.
run_case "no command is a usage error" 2 "$ENUMERABLE" - "$CHECK" || true

if [ "$failures" -gt 0 ]; then
  echo "real-procfs-skip-check.test.sh: $failures failure(s)" >&2
  exit 1
fi
echo "real-procfs-skip-check.test.sh: all cases passed"
