#!/bin/sh
# Hermetic coverage: fake Go only, never the real API/controller analyzer.
set -eu
ROOT="$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)"
mkdir -p "$ROOT/.uzi/scratch"
TMP="$(mktemp -d "$ROOT/.uzi/scratch/deadcode-tests.XXXXXX")"
trap 'rm -rf "$TMP"' EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
mkdir -p "$TMP/bin" "$TMP/api" "$TMP/controller" "$TMP/timer programs" "$TMP/temp"
export TMPDIR="$TMP/temp"
GO_LOG="$TMP/go.log"
export GO_LOG
unset ImageOS ImageVersion

fail() {
  printf 'deadcode-gate.test.sh: FAIL: %s\n' "$*" >&2
  exit 1
}
contains() {
  grep -Fq -- "$2" "$1" || fail "$1 lacks: $2"
}

cat > "$TMP/bin/go" <<'EOF'
#!/bin/sh
printf 'called\n' >> "$GO_LOG"
[ "$#" -eq 5 ] && [ "$1" = run ] &&
  [ "$2" = golang.org/x/tools/cmd/deadcode@v0.48.0 ] &&
  [ "$3" = -test ] && [ "$5" = ./... ] &&
  [ "$PWD" = "$EXPECTED_MODULE" ] || exit 99
case "$4" in -f=*) ;; *) exit 99 ;; esac
printf 'fake Go warning\n' >&2
case "$FAKE_RESULT" in
  clean) exit 0 ;;
  finding) printf 'example/module\tDeadProbe\tprobe.go:7:1\n'; exit 0 ;;
  load) exit 1 ;;
  *) exit 99 ;;
esac
EOF
cat > "$TMP/bin/uname" <<'EOF'
#!/bin/sh
printf '%s\n' "$TEST_OS"
EOF
cat > "$TMP/bin/sysctl" <<'EOF'
#!/bin/sh
if [ "$FAKE_MEMORY" = valid ]; then printf '17179869184\n'; else printf 'odd\n'; fi
EOF
cat > "$TMP/bin/vm_stat" <<'EOF'
#!/bin/sh
[ "$FAKE_MEMORY" = valid ] || exit 127
printf 'Mach Virtual Memory Statistics: (page size of 16384 bytes)\nPages free: 10.\nPages inactive: 20.\n'
EOF
cat > "$TMP/timer programs/time fake" <<'EOF'
#!/bin/sh
if [ "$1" = --version ]; then
  case "$TIME_KIND" in
    gnu) printf 'time (GNU Time) 1.9\n'; exit 0 ;;
    busybox) printf 'BusyBox time\n'; exit 1 ;;
    bsd) exit 1 ;;
  esac
fi
case "$1" in
  -v) [ "$2" = -o ] || exit 98; report=$3; shift 3 ;;
  -l) report=; shift ;;
  *) exit 98 ;;
esac
probe=0
[ "$3" != : ] || probe=1
if [ "$probe" -eq 0 ] && [ "$TIME_BEHAVIOR" = no-launch ]; then exit 78; fi
"$@"
command_rc=$?
emit() {
  if [ "$probe" -eq 0 ] && [ "$TIME_BEHAVIOR" = malformed ]; then
    printf 'odd report\n'
  elif [ "$TIME_KIND" = gnu ]; then
    printf 'Elapsed (wall clock) time (h:mm:ss or m:ss): 1:02.50\nMaximum resident set size (kbytes): 2048\n'
  else
    printf '1:02.50 real 0.10 user 0.20 sys\n2097152 maximum resident set size\n'
  fi
}
if [ -n "$report" ]; then emit > "$report"; else emit >&2; fi
if [ "$probe" -eq 0 ] && [ "$TIME_BEHAVIOR" = timer-failure ]; then exit 77; fi
exit "$command_rc"
EOF
chmod +x "$TMP/bin/"* "$TMP/timer programs/time fake"
PATH="$TMP/bin:$PATH"
export PATH
TEST_OS=Linux
FAKE_MEMORY=valid
TIME_BEHAVIOR=normal
export TEST_OS FAKE_MEMORY TIME_BEHAVIOR

run_case() {
  name=$1
  EXPECTED_MODULE="$TMP/$2"
  FAKE_RESULT=$3
  TIME_KIND=$4
  expected=$5
  baseline=${6:-normal}
  export EXPECTED_MODULE FAKE_RESULT TIME_KIND
  : > "$GO_LOG"
  : > "$EXPECTED_MODULE/.deadcode-baseline"
  case "$baseline" in
    stale) printf 'example/module\tStaleProbe\n' > "$EXPECTED_MODULE/.deadcode-baseline" ;;
    missing) rm "$EXPECTED_MODULE/.deadcode-baseline" ;;
  esac
  DEADCODE_TIME_BIN="$TMP/timer programs/time fake"
  if [ "$TIME_KIND" = missing ]; then DEADCODE_TIME_BIN="$TMP/no timer"; fi
  export DEADCODE_TIME_BIN
  out="$TMP/$name.stdout"
  err="$TMP/$name.stderr"
  actual=0
  sh "$ROOT/scripts/deadcode-gate.sh" "$EXPECTED_MODULE" \
    golang.org/x/tools/cmd/deadcode@v0.48.0 >"$out" 2>"$err" || actual=$?
  if [ "$actual" -ne "$expected" ]; then
    cat "$out" "$err" >&2
    fail "$name: expected $expected, got $actual"
  fi
  calls=$(wc -l < "$GO_LOG")
  [ "$calls" -eq 1 ] || fail "$name: expected one Go invocation, got $calls"
  if grep -Fq 'diagnostics:' "$out"; then fail "$name: diagnostics on stdout"; fi
  contains "$err" 'fake Go warning'
  contains "$err" 'wrapper_elapsed_seconds='
  printf 'ok %s\n' "$name"
}

run_case gnu-clean api clean gnu 0
contains "$out" 'clean (0 findings'
contains "$err" 'command_elapsed_seconds=62.500'
contains "$err" 'peak_rss_bytes=2097152'
contains "$err" 'not simultaneous aggregate tree or whole-job memory'
contains "$err" 'runner_image_version=unavailable (ImageVersion unset)'

run_case gnu-finding api finding gnu 1
contains "$out" 'unreachable func: DeadProbe'
run_case stale controller clean gnu 1 stale
contains "$out" 'StaleProbe'
run_case load api load gnu 2
[ ! -s "$out" ] || fail 'load error must have empty stdout'
run_case missing-baseline controller clean gnu 2 missing
contains "$err" 'baseline missing:'

TEST_OS=Darwin
run_case bsd controller clean bsd 0
contains "$err" 'command_elapsed_seconds=62.500'
contains "$err" 'peak_rss_bytes=2097152'
contains "$err" 'total_memory_bytes=17179869184'
contains "$err" 'available_memory_estimate_bytes=491520'
contains "$err" 'page_size_bytes=16384'
FAKE_MEMORY=malformed
run_case bad-memory controller clean bsd 0
contains "$err" 'unavailable (sysctl hw.memsize missing or malformed)'
contains "$err" 'unavailable (vm_stat missing or malformed)'
FAKE_MEMORY=valid
TEST_OS=Linux

run_case absent-timer api clean missing 0
contains "$err" 'unavailable (timer executable missing or not executable)'
run_case busybox api finding busybox 1
contains "$out" 'unreachable func: DeadProbe'
contains "$err" 'unavailable (unsupported BusyBox timer)'

TIME_BEHAVIOR=malformed
run_case malformed-timing api clean gnu 0
contains "$err" 'command_elapsed_seconds=unavailable (timer output missing or malformed)'
contains "$err" 'peak_rss_bytes=unavailable (timer output missing or malformed)'
TIME_BEHAVIOR=timer-failure
run_case timer-failure controller clean gnu 0
contains "$err" 'unavailable (timer exited 77; child verdict retained)'
TIME_BEHAVIOR=no-launch
run_case timer-no-launch controller clean gnu 0
contains "$err" 'timer did not launch command'
printf 'deadcode-gate.test.sh: all contracts passed\n'
