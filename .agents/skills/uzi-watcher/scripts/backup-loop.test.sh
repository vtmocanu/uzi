#!/usr/bin/env bash
# Select one case with TEST_CASE or argv[1]; SCRIPT overrides the loop under test.
set -eu

HERE="$(cd "$(dirname "$0")" && pwd)"
REPO="$(cd "$HERE/../../../.." && pwd)"
SCRIPT="${SCRIPT:-$HERE/backup-loop.sh}"
# macOS launchd uses Bash 3.2; avoid associative arrays and empty-array expansions.
BASH_BIN="${BASH_BIN:-/bin/bash}"
mkdir -p "$REPO/.uzi/scratch"
WORK="$(mktemp -d "$REPO/.uzi/scratch/backup-loop-test.XXXXXX")"
trap 'rm -rf "$WORK"' EXIT
fail(){ echo "FAIL [$CASE]: $*" >&2; exit 1; }
line_count(){ awk 'END {print NR+0}' "$1"; }
contains(){ grep -Fq -- "$2" "$1" || fail "missing '$2' in $1"; }

make_stubs(){
  cat > "$CASE_DIR/kubectl" <<'STUB'
#!/usr/bin/env bash
set -eu
printf '<%s>' "$@" >> "$CASE_DIR/kubectl.calls"
printf '\n' >> "$CASE_DIR/kubectl.calls"
case "$CASE" in
  reject-unknown) printf '%s\n' other-ctx ;;
  reject-prefix) printf '%s\n' test-ctx-extra ;;
  reject-multiline) printf '%s\n' test-ctx ;;
  reject-kubectl-error) echo "fixture config failure" >&2; exit 7 ;;
  reject-kubectl-error-matching)
    echo test-ctx; echo "fixture config failure" >&2; exit 7 ;;
  *) printf '%s\n' other-ctx test-ctx ;;
esac
STUB
  cat > "$CASE_DIR/uzi" <<'STUB'
#!/usr/bin/env bash
set -eu
# The backup stub increments this before retirement probes. Zero means startup.
cycle=0
[ ! -f "$CASE_DIR/backup-count" ] || cycle="$(cat "$CASE_DIR/backup-count")"
phase=startup
[ "$cycle" -eq 0 ] || phase="cycle-$cycle"
printf '%s <%s><%s><%s><%s><%s>\n' "$phase" "$@" >> "$CASE_DIR/uzi.calls"
[ "$#" -eq 5 ] && [ "$1" = run ] && [ "$2" = get ] &&
  [ "$4" = --field ] && [ "$5" = status ] || exit 9
case "$CASE" in
  baseline)
    if [ "$3" = run-b ] && [ "$cycle" -lt 2 ]; then
      echo running
    else
      echo completed
    fi ;;
  advisory-terminal)
    case "$3" in
      run-a) echo completed ;;
      run-b) echo failed ;;
      run-c) echo cancelled ;;
    esac ;;
  advisory-empty|retain-empty)
    if [ "$3" = run-a ] && [ "$cycle" -lt 2 ]; then exit 0; fi
    echo completed ;;
  advisory-failing|retain-failing)
    if [ "$3" = run-a ] && [ "$cycle" -lt 2 ]; then exit 7; fi
    echo completed ;;
  advisory-unrecognized|retain-unrecognized)
    if [ "$3" = run-a ] && [ "$cycle" -lt 2 ]; then echo mystery; else echo completed; fi ;;
  advisory-active)
    if [ "$3" = run-a ] && [ "$cycle" -lt 2 ]; then echo running; else echo completed; fi ;;
  advisory-failing-terminal)
    echo completed
    [ "$cycle" -ne 0 ] || exit 7 ;;
  *) echo completed ;;
esac
STUB
  cat > "$CASE_DIR/backup" <<'STUB'
#!/usr/bin/env bash
set -eu
printf '%s\n' "$*" >> "$CASE_DIR/backup.calls"
printf 'ctx=%s\n' "${UZI_CTX-<unset>}" >> "$CASE_DIR/backup.context"
n=0
[ ! -f "$CASE_DIR/backup-count" ] || n="$(cat "$CASE_DIR/backup-count")"
n=$((n + 1))
echo "$n" > "$CASE_DIR/backup-count"
# At most four cycles, independently of sibling targets' probe results.
# STOP makes unexpected retention fail assertions without an hour-long loop.
[ "$n" -lt 4 ] || touch "$UZI_BACKUP_DIR/STOP"
if [ "$CASE" = baseline ] && [ "$n" -eq 1 ]; then exit 1; fi
STUB
  chmod +x "$CASE_DIR/kubectl" "$CASE_DIR/uzi" "$CASE_DIR/backup"
}

assert_flow(){
  [ "$RC" -eq 0 ] || fail "loop exit=$RC: $(cat "$CASE_DIR/stderr")"
  [ ! -e "$CASE_DIR/out/STOP" ] || fail "four-cycle safety bound reached"
  [ ! -e "$CASE_DIR/out/backup-loop.pid" ] || fail "pid survived exit"
  contains "$CASE_DIR/out/backup-loop.state" "status=ended"
  contains "$CASE_DIR/out/backup-loop.state" "namespaces=ns-a ns-b"
  contains "$CASE_DIR/out/backup-loop.state" "ends_at="
  [ -z "$(awk -F= '$1=="runs"{print $2}' "$CASE_DIR/out/backup-loop.state")" ] ||
    fail "retired runs remained in final state"
  if grep -q '^status=running$' "$CASE_DIR/out/backup-loop.state"; then
    fail "stale running state survived exit"
  fi
  contains "$CASE_DIR/stdout" "all runs terminal; exiting"
}

assert_startup(){
  for rid in "$@"; do
    expected="startup <run><get><$rid><--field><status>"
    count="$(awk -v want="$expected" '$0==want{n++} END{print n+0}' "$CASE_DIR/uzi.calls")"
    [ "$count" -eq 1 ] || fail "$rid startup probes=$count (expected 1)"
  done
  [ "$(awk '$1=="startup"{n++} END{print n+0}' "$CASE_DIR/uzi.calls")" -eq "$#" ] ||
    fail "unexpected startup targets"
}

assert_backups(){
  [ "$(line_count "$CASE_DIR/backup.calls")" -eq "$1" ] || fail "unexpected backup cycle count"
  shift
  expected="$*"
  while IFS= read -r actual; do
    [ "$actual" = "$expected" ] || fail "backup targets '$actual', expected '$expected'"
  done < "$CASE_DIR/backup.calls"
}

assert_kubectl(){
  [ "$(line_count "$CASE_DIR/kubectl.calls")" -eq 1 ] || fail "expected exactly one context lookup"
  [ "$(cat "$CASE_DIR/kubectl.calls")" = '<config><get-contexts><-o><name>' ] ||
    fail "unexpected kubectl arguments: $(cat "$CASE_DIR/kubectl.calls")"
}

run_case(){
  CASE="$1"
  CASE_DIR="$WORK/$CASE"
  mkdir -p "$CASE_DIR"
  export CASE CASE_DIR
  : > "$CASE_DIR/kubectl.calls"
  : > "$CASE_DIR/uzi.calls"
  : > "$CASE_DIR/backup.calls"
  make_stubs
  set -- run-a run-b
  [ "$CASE" != advisory-terminal ] || set -- run-a run-b run-c
  RC=0
  (
    export UZI_BIN="$CASE_DIR/uzi" UZI_KUBECTL="$CASE_DIR/kubectl"
    export UZI_BACKUP_RUNS_SCRIPT="$CASE_DIR/backup" UZI_BACKUP_DIR="$CASE_DIR/out"
    export UZI_BACKUP_INTERVAL=1 UZI_BACKUP_MAX_HOURS=1
    export UZI_BACKUP_RETENTION_DAYS=14 UZI_WORKER_NS='ns-a ns-b'
    # Also shadow PATH so an accidental default kubectl cannot reach a cluster.
    export PATH="$CASE_DIR:$PATH"
    export UZI_CTX=test-ctx
    case "$CASE" in
      context-unset) unset UZI_CTX ;;
      context-empty) export UZI_CTX= ;;
      reject-multiline) export UZI_CTX=$'test-ctx\nmissing-context' ;;
    esac
    "$BASH_BIN" "$SCRIPT" "$@"
  ) > "$CASE_DIR/stdout" 2> "$CASE_DIR/stderr" || RC=$?
  echo "OBSERVED [$CASE] loop_exit=$RC kubectl_calls=$(line_count "$CASE_DIR/kubectl.calls") status_calls=$(line_count "$CASE_DIR/uzi.calls") backup_calls=$(line_count "$CASE_DIR/backup.calls")"
  echo "STATUS CALLS [$CASE]"
  cat "$CASE_DIR/uzi.calls"
  echo "BACKUP CALLS [$CASE]"
  cat "$CASE_DIR/backup.calls"
  case "$CASE" in
    reject-*)
      # Check every rejection property even when the old loop returned zero.
      bad=0
      [ "$RC" -eq 2 ] || { echo "FAIL [$CASE]: expected exit 2, got $RC"; bad=1; }
      if ! awk 'tolower($0) ~ /error/ && tolower($0) ~ /context/ && /test-ctx/ {found=1} END {exit !found}' "$CASE_DIR/stderr"; then
        echo "FAIL [$CASE]: context error absent from stderr"; bad=1
      fi
      if [ "$CASE" = reject-multiline ] && ! grep -Fq -- missing-context "$CASE_DIR/stderr"; then
        echo "FAIL [$CASE]: rejected context component missing from stderr"; bad=1
      fi
      [ ! -e "$CASE_DIR/out" ] || { echo "FAIL [$CASE]: output root/state/pid writes occurred"; bad=1; }
      [ ! -s "$CASE_DIR/uzi.calls" ] || { echo "FAIL [$CASE]: status probes occurred"; bad=1; }
      [ ! -s "$CASE_DIR/backup.calls" ] || { echo "FAIL [$CASE]: backups occurred"; bad=1; }
      [ ! -e "$CASE_DIR/backup-count" ] || { echo "FAIL [$CASE]: backup count written"; bad=1; }
      [ "$bad" -eq 0 ] || fail "rejection contract violated"
      assert_kubectl ;;
    *)
      assert_flow
      case "$CASE" in
        baseline)
          assert_backups 2 run-a run-b
          contains "$CASE_DIR/out/backup-loop.state" "context=test-ctx"
          contains "$CASE_DIR/stdout" "backup cycle incomplete rc=1"
          contains "$CASE_DIR/stdout" "keeping terminal run run-a after incomplete backup cycle"
          contains "$CASE_DIR/stdout" "retired terminal run run-a status=completed"
          # Works both before and after M2: startup must not advance cycle statuses.
          contains "$CASE_DIR/uzi.calls" "cycle-1 <run><get><run-b><--field><status>"
          contains "$CASE_DIR/uzi.calls" "cycle-2 <run><get><run-b><--field><status>" ;;
        context-unset|context-empty)
          [ ! -s "$CASE_DIR/kubectl.calls" ] || fail "implicit/current context lookup occurred"
          value='<unset>'
          [ "$CASE" != context-empty ] || value=
          [ "$(cat "$CASE_DIR/backup.context")" = "ctx=$value" ] ||
            fail "backup context changed: $(cat "$CASE_DIR/backup.context")"
          contains "$CASE_DIR/out/backup-loop.state" "context=<unset: current kube context>"
          assert_backups 1 run-a run-b
          assert_startup "$@" ;;
        *)
          assert_kubectl
          if [ "$CASE" = context-known ] || [ "$CASE" = advisory-terminal ] ||
            [ "$CASE" = advisory-failing-terminal ]; then
            assert_backups 1 "$@"
          else
            [ "$(line_count "$CASE_DIR/backup.calls")" -eq 2 ] || fail "expected two backup cycles"
            [ "$(awk 'NR==1{print}' "$CASE_DIR/backup.calls")" = "run-a run-b" ] ||
              fail "first backup lost an original target"
            [ "$(awk 'NR==2{print}' "$CASE_DIR/backup.calls")" = run-a ] ||
              fail "unknown/active run-a not retained, or terminal run-b not retired"
            # Unknown results at cycle 1 must survive into backup 2 and be retried.
            for rid in "$@"; do
              contains "$CASE_DIR/uzi.calls" "cycle-1 <run><get><$rid><--field><status>"
            done
            contains "$CASE_DIR/uzi.calls" "cycle-2 <run><get><run-a><--field><status>"
          fi
          assert_startup "$@" ;;
      esac
      if [ "$CASE" != baseline ]; then
        warnings="$(awk '/WARN/ && /all target runs already terminal/{n++} END{print n+0}' "$CASE_DIR/stdout" "$CASE_DIR/stderr")"
        expected=0
        case "$CASE" in context-known|context-unset|context-empty|advisory-terminal) expected=1 ;; esac
        [ "$warnings" -eq "$expected" ] || fail "terminal advisory count=$warnings (expected $expected)"
      fi ;;
  esac
  echo "PASS [$CASE]"
}

CASES='baseline reject-unknown reject-prefix reject-multiline reject-kubectl-error reject-kubectl-error-matching context-known context-unset context-empty advisory-terminal advisory-empty advisory-failing advisory-unrecognized advisory-active advisory-failing-terminal retain-empty retain-failing retain-unrecognized'
SELECTED="${1:-${TEST_CASE:-all}}"
if [ "$SELECTED" != all ]; then
  case " $CASES " in
    *" $SELECTED "*) run_case "$SELECTED" ;;
    *) echo "unknown TEST_CASE: $SELECTED (choose: $CASES)" >&2; exit 2 ;;
  esac
else
  # Each case runs in a subshell so a failure cannot skip independent cases.
  failures=0
  for selected in $CASES; do
    (run_case "$selected") || failures=$((failures + 1))
  done
  [ "$failures" -eq 0 ] || { echo "FAIL: $failures cases failed" >&2; exit 1; }
fi
