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
# appending one record (the list must still print); `rmlog RC` deletes the skip log and exits RC;
# `unreadable RC` appends one record, makes the skip log unreadable (mode 000) and exits RC;
# `concurrency FILE` writes UZI_AGENT_TEST_CONCURRENCY (or `<unset>`) to FILE and exits 0.
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
  unreadable)
    printf 'unreadable label\treal-procfs-denied: unreadable reason\n' >> "$UZI_REAL_PROCFS_SKIP_LOG"
    chmod 000 "$UZI_REAL_PROCFS_SKIP_LOG"
    exit "$2"
    ;;
  concurrency)
    printf '%s\n' "${UZI_AGENT_TEST_CONCURRENCY-<unset>}" > "$2"
    exit 0
    ;;
esac
echo "fake-tests: unknown mode '$1'" >&2
exit 99
EOF
chmod +x "$FAKE"

failures=0
OUT="$TMP/out"

# Control parallelism in this test's node processes without a production env seam.
# The probe still enumerates the real throwaway directory and gets real EACCES.
PARALLELISM_PRELOAD="$TMP/parallelism.cjs"
cat > "$PARALLELISM_PRELOAD" <<'EOF'
require("node:os").availableParallelism = () => Number(process.env.TEST_AVAILABLE_PARALLELISM);
EOF

# run_case NAME EXPECTED_RC PROBE_DIR CI_VALUE COMMAND...
# CI_VALUE "-" runs with CI unset (`env -u CI`); anything else exports CI=<value>.
# UZI_AGENT_TEST_CONCURRENCY is always unset, so a caller's value cannot leak into a case;
# a case that needs one sets it inside COMMAND (`env UZI_AGENT_TEST_CONCURRENCY=3 ...`).
run_case() {
  name="$1" want="$2" probe="$3" ci="$4"
  shift 4
  got=0
  if [ "$ci" = "-" ]; then
    env -u CI -u UZI_AGENT_TEST_CONCURRENCY NODE_OPTIONS="--require=\"$PARALLELISM_PRELOAD\"" TEST_AVAILABLE_PARALLELISM=4 UZI_REAL_PROCFS_PROBE_DIR="$probe" "$@" > "$OUT" 2>&1 || got=$?
  else
    env -u UZI_AGENT_TEST_CONCURRENCY NODE_OPTIONS="--require=\"$PARALLELISM_PRELOAD\"" TEST_AVAILABLE_PARALLELISM=4 CI="$ci" UZI_REAL_PROCFS_PROBE_DIR="$probe" "$@" > "$OUT" 2>&1 || got=$?
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

# (g2) a skip log that exists but cannot be read cannot be counted: fail closed (the "cannot
# count the skip log" branch), never read as 0 skips. Root reads a mode-000 file anyway, so
# this case cannot build an unreadable log under uid 0.
if [ "$(id -u)" -eq 0 ]; then
  echo "SKIP: unreadable skip log: running as uid 0, which reads a mode-000 file, so no unreadable log can be built"
else
  run_case "unreadable skip log, command rc 0" 1 "$ENUMERABLE" - "$CHECK" "$FAKE" unreadable 0 &&
    expect_output "an unreadable skip log is named" "cannot count the skip log"
fi

# (j) the pre-run probe caps agent test concurrency where enumeration is denied, and only
# there. SEEN is what the fake command read from UZI_AGENT_TEST_CONCURRENCY.
SEEN="$TMP/seen-concurrency"
expect_seen() {
  name="$1" want="$2"
  got="$(cat "$SEEN" 2>/dev/null || echo '<no file>')"
  if [ "$got" = "$want" ]; then
    echo "PASS: $name"
  else
    echo "FAIL: $name: the command saw UZI_AGENT_TEST_CONCURRENCY=$got, expected $want"
    failures=$((failures + 1))
  fi
}
rm -f "$SEEN"
run_case "enumerable: concurrency left unset" 0 "$ENUMERABLE" - "$CHECK" "$FAKE" concurrency "$SEEN" &&
  expect_seen "enumerable: the command sees no concurrency cap" "<unset>"
if [ "$(id -u)" -eq 0 ]; then
  echo "SKIP: denied concurrency cap: running as uid 0, which enumerates a chmod-000 dir, so no denied probe dir can be built"
else
  rm -f "$SEEN"
  run_case "denied: concurrency capped" 0 "$DENIED" - "$CHECK" "$FAKE" concurrency "$SEEN" && {
    expect_seen "denied: the command sees UZI_AGENT_TEST_CONCURRENCY=1" "1"
    expect_output "denied: the cap is explained" "running agent test files serially (UZI_AGENT_TEST_CONCURRENCY=1)"
  }
  rm -f "$SEEN"
  run_case "denied + preset 3: concurrency kept" 0 "$DENIED" - \
    env UZI_AGENT_TEST_CONCURRENCY=3 "$CHECK" "$FAKE" concurrency "$SEEN" &&
    expect_seen "denied + preset 3: the command sees the caller's 3" "3"
  # The npm script splices the value into node's argv verbatim, so a non-positive-integer
  # preset is replaced by 1 (with a warning) where the cap matters.
  rm -f "$SEEN"
  run_case "denied + preset 'x2': concurrency replaced" 0 "$DENIED" - \
    env UZI_AGENT_TEST_CONCURRENCY=x2 "$CHECK" "$FAKE" concurrency "$SEEN" && {
    expect_seen "denied + preset 'x2': the command sees 1" "1"
    expect_output "denied + preset 'x2': the replacement is warned" "UZI_AGENT_TEST_CONCURRENCY='x2' is not a positive integer"
  }
fi

# Issue #2240: caller > denied-procfs cap > two-CPU floor > Node default.
for parallelism in 1 2 4; do
  case "$parallelism" in
    1) seen='<unset>'; effective=1; source=node-default ;;
    2) seen=2; effective=2; source=floor ;;
    4) seen='<unset>'; effective=3; source=node-default ;;
  esac
  run_case "parallelism $parallelism without caller" 0 "$ENUMERABLE" - \
    env TEST_AVAILABLE_PARALLELISM="$parallelism" "$CHECK" "$FAKE" concurrency "$SEEN" && {
    expect_seen "parallelism $parallelism selection" "$seen"
    expect_output "parallelism $parallelism diagnostics" "availableParallelism=$parallelism"
    expect_output "parallelism $parallelism effective/source" "--test-concurrency=$effective source=$source"
    expect_output "runtime node version" "agent-tests: node="
    expect_output "runtime libuv version" "libuv="
    expect_output "quota readable or explicitly unavailable" "cpu.max="
    expect_output "unit stage duration" "agent-stage: unit duration_seconds="
  }
done
for caller in 1 3 x2 ''; do
  run_case "two CPUs keep caller '$caller'" 0 "$ENUMERABLE" - \
    env TEST_AVAILABLE_PARALLELISM=2 UZI_AGENT_TEST_CONCURRENCY="$caller" "$CHECK" "$FAKE" concurrency "$SEEN" &&
    expect_seen "two CPUs caller '$caller' preserved" "$caller"
done
run_case "caller override diagnosed" 0 "$ENUMERABLE" - \
  env TEST_AVAILABLE_PARALLELISM=2 UZI_AGENT_TEST_CONCURRENCY=3 "$CHECK" "$FAKE" concurrency "$SEEN" &&
  expect_output "caller effective/source" "--test-concurrency=3 source=caller-override"
run_case "failure still timed" 7 "$ENUMERABLE" - "$CHECK" "$FAKE" fail 7 &&
  expect_output "failed unit stage retains its exit" "exit=7"
if [ "$(id -u)" -ne 0 ]; then
  run_case "denied cap beats two-CPU floor" 0 "$DENIED" - \
    env TEST_AVAILABLE_PARALLELISM=2 "$CHECK" "$FAKE" concurrency "$SEEN" && {
    expect_seen "denied cap stays serial" "1"
    expect_output "denied cap effective/source" "--test-concurrency=1 source=procfs-denied-cap"
  }
  run_case "caller beats denied cap and floor" 0 "$DENIED" - \
    env TEST_AVAILABLE_PARALLELISM=2 UZI_AGENT_TEST_CONCURRENCY=3 "$CHECK" "$FAKE" concurrency "$SEEN" && {
    expect_seen "caller beats both selections" "3"
    expect_output "denied caller effective/source" "--test-concurrency=3 source=caller-override"
  }
  run_case "invalid caller still replaced on denied procfs" 0 "$DENIED" - \
    env TEST_AVAILABLE_PARALLELISM=2 UZI_AGENT_TEST_CONCURRENCY=x2 "$CHECK" "$FAKE" concurrency "$SEEN" && {
    expect_seen "invalid denied caller sees cap" "1"
    expect_output "invalid denied caller source" "--test-concurrency=1 source=procfs-denied-cap"
  }
fi

# (k) the other half of the cap: agent/package.json's test script must turn
# UZI_AGENT_TEST_CONCURRENCY into --test-concurrency. The exact script string runs through
# `sh -c` (as npm runs it) with a fake `node` first on PATH that prints its argv one per
# line. UZI_AGENT_PACKAGE_JSON overrides the file, for mutation-testing this case only.
# node (to parse the JSON) is required, never skipped.
PKG="${UZI_AGENT_PACKAGE_JSON:-$ROOT/agent/package.json}"
if ! command -v node > /dev/null 2>&1; then
  echo "FAIL: package.json concurrency expansion: node is not on PATH"
  failures=$((failures + 1))
elif ! test_script="$(node -e '
const pkg = JSON.parse(require("node:fs").readFileSync(process.argv[1], "utf8"));
const s = pkg && pkg.scripts && pkg.scripts.test;
if (typeof s !== "string" || s === "") { console.error("no scripts.test in " + process.argv[1]); process.exit(1); }
process.stdout.write(s);' -- "$PKG")"; then
  echo "FAIL: package.json concurrency expansion: cannot read scripts.test from $PKG"
  failures=$((failures + 1))
else
  FAKEBIN="$TMP/fake-node-bin"
  mkdir -p "$FAKEBIN"
  # shellcheck disable=SC2016 # the single-quoted line is the fake node's own script
  printf '%s\n' '#!/bin/sh' 'for a in "$@"; do printf "%s\n" "$a"; done' > "$FAKEBIN/node"
  chmod +x "$FAKEBIN/node"
  ARGV="$TMP/node-argv"
  if env UZI_AGENT_TEST_CONCURRENCY=1 PATH="$FAKEBIN:$PATH" sh -c "$test_script" > "$ARGV" 2>&1 &&
    grep -qx -- '--test-concurrency=1' "$ARGV"; then
    echo "PASS: package.json test script: UZI_AGENT_TEST_CONCURRENCY=1 passes --test-concurrency=1"
  else
    echo "FAIL: package.json test script: UZI_AGENT_TEST_CONCURRENCY=1 did not pass --test-concurrency=1 to node ($PKG); argv:"
    sed 's/^/    /' "$ARGV"
    failures=$((failures + 1))
  fi
  # `--test` present proves the fake node was reached with the real argv.
  if env -u UZI_AGENT_TEST_CONCURRENCY PATH="$FAKEBIN:$PATH" sh -c "$test_script" > "$ARGV" 2>&1 &&
    grep -qx -- '--test' "$ARGV" && ! grep -q -- '^--test-concurrency' "$ARGV"; then
    echo "PASS: package.json test script: unset UZI_AGENT_TEST_CONCURRENCY passes no --test-concurrency"
  else
    echo "FAIL: package.json test script: with UZI_AGENT_TEST_CONCURRENCY unset, node was not reached or still got --test-concurrency ($PKG); argv:"
    sed 's/^/    /' "$ARGV"
    failures=$((failures + 1))
  fi
fi

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
