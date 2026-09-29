#!/usr/bin/env bash
# Hermetic regression for e2e/codex-git-trust/run.sh (issue #1769), driven by a FAKE `docker` on
# PATH: no daemon, no image, no container. The stub records every argv, its own pid/pgid and the
# signals it receives, and is steered per case by env (STUB_RUN_EXIT, STUB_BLOCK=run|build,
# STUB_IGNORE_TERM, STUB_PS_LISTED). It asserts:
#   a. a missing CODEX_GIT_TRUST_SRC_DIR / CODEX_GIT_TRUST_BIN_DIR source exits 2 and docker is
#      never invoked;
#   b. `docker run` and `docker build` stay in the script's process group (timeout --foreground),
#      so a process-group SIGINT (Ctrl-C) ends the script with 130 promptly, well under the
#      30s CODEX_GIT_TRUST_TIMEOUT / CODEX_GIT_TRUST_BUILD_TIMEOUT; a group SIGTERM that the
#      docker CLI ignores ends with 143 within the 10s kill grace. Without --foreground, GNU
#      timeout setpgid's away and these cases time out red;
#   c. an unverified cleanup (container still listed) turns run exit 0 or 77 into 3 and keeps 1;
#   d. CODEX_GIT_TRUST_BUILD_NETWORK=host forwards `--network host`; any other value warns and
#      forwards nothing;
#   e. every bind is `--mount type=bind,...,readonly` and no `-v`/`--volume` appears.
# Linux only (reads /proc/<pid>/stat; needs GNU timeout --foreground): the Task target is
# `platforms: [linux]`. Prints PASS:/FAIL: per check and a MANDATORY `cases=N passed=N` tally, and
# exits nonzero unless every check passed and at least MIN_CASES ran, so a gutted run cannot read
# green. run.sh is started with `set -m` (its own process group) under GNU
# `env --default-signal=INT,TERM`, so an INT/TERM ignore inherited from a backgrounded caller
# (`task gate:repo &`) is reset in the child rather than reddening the signal cases.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SCRIPT="$HERE/run.sh"
MIN_CASES=43

[ -f "$SCRIPT" ] || { echo "run.sh not found at $SCRIPT" >&2; exit 2; }
[ -r /proc/self/stat ] || { echo "ERROR: needs /proc (Linux only)" >&2; exit 2; }
timeout --foreground 5s true 2>/dev/null || { echo "ERROR: needs GNU timeout --foreground" >&2; exit 2; }
env --default-signal=INT,TERM true 2>/dev/null \
  || { echo "ERROR: needs GNU env --default-signal (coreutils >= 8.31) to reset inherited INT/TERM ignores" >&2; exit 2; }
REAL_TIMEOUT="$(command -v timeout)"
export REAL_TIMEOUT

# Hermetic: no inherited knob may steer run.sh.
for v in ${!CODEX_GIT_TRUST_@} ${!STUB_@}; do unset "$v"; done

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
FAKEBIN="$TMP/bin"
mkdir -p "$FAKEBIN"
ST="$TMP/state"

# Fake docker. Quoted heredoc: everything expands at stub run time. The stub never execs
# anything but coreutils; a signal trap records the signal and (unless STUB_IGNORE_TERM=1 for
# TERM) re-raises it with the default disposition, so the stub dies by the signal as a CLI would.
cat > "$FAKEBIN/docker" <<'EOF'
#!/usr/bin/env bash
set -u
st="${STUB_STATE:?}"
printf '%s\n' "$*" >> "$st/calls.log"
block() {
  local label="$1" stat rest
  trap 'echo INT >> "$st/$label.signals"; trap - INT; kill -INT $$' INT
  if [ "${STUB_IGNORE_TERM:-0}" = 1 ]; then
    trap 'echo TERM >> "$st/$label.signals"' TERM
  else
    trap 'echo TERM >> "$st/$label.signals"; trap - TERM; kill -TERM $$' TERM
  fi
  stat="$(cat "/proc/$$/stat")"
  rest="${stat##*) }"
  # Fields after "(comm) ": state ppid pgrp ...
  # shellcheck disable=SC2086
  set -- $rest
  printf '%s %s %s\n' "$$" "$3" "$2" > "$st/$label.pid.tmp"
  mv "$st/$label.pid.tmp" "$st/$label.pid"
  local i=0
  while [ "$i" -lt 600 ]; do sleep 0.1; i=$((i + 1)); done
  exit 124
}
case "${1:-}" in
  build)
    printf '%s\n' "$@" > "$st/build.argv"
    [ "${STUB_BLOCK:-}" = build ] && block build
    exit 0
    ;;
  run)
    printf '%s\n' "$@" > "$st/run.argv"
    [ "${STUB_BLOCK:-}" = run ] && block run
    exit "${STUB_RUN_EXIT:-0}"
    ;;
  ps)
    [ "${STUB_PS_LISTED:-0}" = 1 ] && echo 0123456789ab
    exit 0
    ;;
  *) exit 0 ;;
esac
EOF
chmod +x "$FAKEBIN/docker"

# Keep the production 10s kill grace visible to the test. Only the blocking docker
# run/build calls use a short grace here; the image-inspect timeout passes through.
cat > "$FAKEBIN/timeout" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
if [ "${1:-}" = --foreground ] && [ "${2:-}" = --kill-after=10s ] &&
   [ "${4:-}" = docker ]; then
  case "${5:-}" in
    run|build)
      printf 'requested=%s forwarded=%s docker=%s\n' "$2" --kill-after=0.2s "$5" >> "${STUB_STATE:?}/timeout.log"
      shift 2
      exec "$REAL_TIMEOUT" --foreground --kill-after=0.2s "$@"
      ;;
  esac
fi
if [ "${1:-}" = 10s ] && [ "${2:-}" = docker ] &&
   [ "${3:-}" = image ] && [ "${4:-}" = inspect ]; then
  printf 'passthrough=10s docker image inspect\n' >> "${STUB_STATE:?}/timeout.log"
fi
exec "$REAL_TIMEOUT" "$@"
EOF
chmod +x "$FAKEBIN/timeout"
PATH="$FAKEBIN:$PATH"
export PATH

cases=0
passed=0
check() { # NAME CONDITION-RESULT(0/1) [DETAIL]
  cases=$((cases + 1))
  if [ "$2" -eq 0 ]; then
    passed=$((passed + 1))
    echo "PASS: $1"
  else
    echo "FAIL: $1${3:+ -- $3}"
  fi
}
t() { if "$@"; then echo 0; else echo 1; fi; }

reset_state() { rm -rf "$ST"; mkdir -p "$ST"; }

# run_sync ACTION [VAR=VALUE ...]: runs run.sh synchronously; sets RC and PID-less OUT.
run_sync() {
  local action="$1"
  shift
  reset_state
  set +e
  env STUB_STATE="$ST" "$@" bash "$SCRIPT" "$action" > "$ST/out" 2>&1
  RC=$?
  set -e
}

# signal_case LABEL ACTION STUB-LABEL SIGNAL WANT-RC DEADLINE-SECS [VAR=VALUE ...]
# Starts run.sh as its own process-group leader, waits for the stub to block, checks the stub's
# pgid equals the script's, signals the whole group, and requires exit WANT-RC within DEADLINE.
signal_case() {
  local label="$1" action="$2" stub="$3" sig="$4" want="$5" deadline="$6"
  shift 6
  reset_state
  set -m
  env --default-signal=INT,TERM STUB_STATE="$ST" STUB_BLOCK="$stub" "$@" bash "$SCRIPT" "$action" > "$ST/out" 2>&1 &
  local pid=$!
  set +m
  local i=0
  while [ ! -f "$ST/$stub.pid" ] && [ "$i" -lt 300 ]; do sleep 0.05; i=$((i + 1)); done
  if [ ! -f "$ST/$stub.pid" ]; then
    check "$label: stub docker $stub started" 1 "no pid file; output: $(tr '\n' '|' < "$ST/out")"
    kill -KILL "$pid" 2>/dev/null || true
    wait "$pid" 2>/dev/null || true
    return
  fi
  local spid spgid sppid
  read -r spid spgid sppid < "$ST/$stub.pid"
  check "$label: docker $stub is in the script's process group" "$(t [ "$spgid" = "$pid" ])" \
    "script pid/pgid=$pid, docker $stub pgid=$spgid"
  local start=$SECONDS
  kill "-$sig" -- "-$pid"
  i=0
  while kill -0 "$pid" 2>/dev/null && [ "$i" -lt $((deadline * 10)) ]; do sleep 0.1; i=$((i + 1)); done
  local prompt=0
  if kill -0 "$pid" 2>/dev/null; then
    prompt=1
    # Unblock by exact saved PIDs only: the stub, then its timeout parent, then the script.
    kill -KILL "$spid" 2>/dev/null || true
    i=0
    while kill -0 "$pid" 2>/dev/null && [ "$i" -lt 50 ]; do sleep 0.1; i=$((i + 1)); done
    kill -KILL "$sppid" 2>/dev/null || true
    kill -KILL "$pid" 2>/dev/null || true
  fi
  local rc=0
  wait "$pid" 2>/dev/null || rc=$?
  check "$label: group SIG$sig ends the script within ${deadline}s" "$prompt" \
    "still running after ${deadline}s (timeout 30s); a group signal did not reach docker $stub"
  check "$label: exit status is $want" "$(t [ "$rc" = "$want" ])" "got $rc after $((SECONDS - start))s"
  check "$label: docker $stub itself received SIG$sig" "$(t grep -sqx "$sig" "$ST/$stub.signals")" \
    "signals recorded: $(cat "$ST/$stub.signals" 2>/dev/null | tr '\n' ' ')"
  check "$label: production requested 10s kill grace" \
    "$(t grep -qxF "requested=--kill-after=10s forwarded=--kill-after=0.2s docker=$stub" "$ST/timeout.log")"
  check "$label: only docker $stub grace was shortened" \
    "$(t [ "$(grep -c '^requested=' "$ST/timeout.log")" = 1 ])"
  if [ "$action" = fixture ]; then
    check "$label: image-inspect timeout kept its 10s duration" \
      "$(t grep -qxF 'passthrough=10s docker image inspect' "$ST/timeout.log")"
    check "$label: container removed by exact name on the signal path" \
      "$(t grep -qx "rm -f codex-git-trust-$pid" "$ST/calls.log")"
  fi
}

# ---- a. missing bind sources: exit 2, docker never invoked ----------------------------------
mkdir -p "$TMP/src" "$TMP/sbin" "$TMP/emptybin"
for b in uzi-codex-supervisor uzi-codex-fileop uzi-codex-command-sandbox; do : > "$TMP/sbin/$b"; done

for spec in "SRC_DIR missing|CODEX_GIT_TRUST_SRC_DIR=$TMP/missing" \
  "SRC_DIR relative|CODEX_GIT_TRUST_SRC_DIR=relative/src" \
  "BIN_DIR missing|CODEX_GIT_TRUST_BIN_DIR=$TMP/missing" \
  "BIN_DIR without binaries|CODEX_GIT_TRUST_BIN_DIR=$TMP/emptybin"; do
  run_sync fixture "${spec#*|}"
  check "a: ${spec%%|*} exits 2" "$(t [ "$RC" = 2 ])" "got $RC: $(tr '\n' '|' < "$ST/out")"
  check "a: ${spec%%|*} never invokes docker" "$(t [ ! -e "$ST/calls.log" ])"
done

# ---- b. process-group signals reach docker run / docker build -------------------------------
signal_case "b: fixture INT" fixture run INT 130 5 CODEX_GIT_TRUST_TIMEOUT=30
signal_case "b: build INT" build build INT 130 5 CODEX_GIT_TRUST_BUILD_TIMEOUT=30
signal_case "b: fixture TERM (docker ignores TERM)" fixture run TERM 143 15 \
  CODEX_GIT_TRUST_TIMEOUT=30 STUB_IGNORE_TERM=1

# ---- c. exit-3 mapping for an unverified cleanup --------------------------------------------
for spec in "0 0 0" "77 0 77" "1 0 1" "0 1 3" "77 1 3" "1 1 1"; do
  read -r run_rc listed want <<< "$spec"
  run_sync fixture STUB_RUN_EXIT="$run_rc" STUB_PS_LISTED="$listed"
  check "c: run exit $run_rc, still listed=$listed -> $want" "$(t [ "$RC" = "$want" ])" \
    "got $RC: $(tr '\n' '|' < "$ST/out")"
done
# The removal and the verification both name the container exactly (run.sh's own $$).
run_sync fixture
name="$(grep -m1 '^run ' "$ST/calls.log" | grep -o 'codex-git-trust-[0-9]*' || true)"
check "c: run names the container codex-git-trust-<pid>" "$(t [ -n "$name" ])"
check "c: cleanup runs docker rm -f on that exact name" "$(t grep -qx "rm -f $name" "$ST/calls.log")"
check "c: cleanup verifies with an anchored exact-name filter" \
  "$(t grep -qxF "ps -a --filter name=^/$name\$ -q" "$ST/calls.log")"

# ---- d. build network opt-in ------------------------------------------------------------------
# network_is FILE VALUE: the argv line after `--network` is VALUE. lacks FILE ERE: no line matches.
network_is() { grep -A1 -x -- '--network' "$1" | grep -qx -- "$2"; }
lacks() { [ -f "$1" ] && ! grep -qE -- "$2" "$1"; }
run_sync build CODEX_GIT_TRUST_BUILD_NETWORK=host
check "d: BUILD_NETWORK=host exits 0" "$(t [ "$RC" = 0 ])" "got $RC"
check "d: BUILD_NETWORK=host forwards --network host" "$(t network_is "$ST/build.argv" host)"
run_sync build CODEX_GIT_TRUST_BUILD_NETWORK=yes
check "d: BUILD_NETWORK=yes exits 0" "$(t [ "$RC" = 0 ])" "got $RC"
check "d: BUILD_NETWORK=yes warns" "$(t grep -q '^WARN: CODEX_GIT_TRUST_BUILD_NETWORK=yes' "$ST/out")"
check "d: BUILD_NETWORK=yes forwards no --network" "$(t lacks "$ST/build.argv" '^--network$')"
run_sync build
check "d: unset BUILD_NETWORK forwards no --network" "$(t lacks "$ST/build.argv" '^--network$')"

# ---- e. bind hygiene --------------------------------------------------------------------------
run_sync fixture CODEX_GIT_TRUST_SRC_DIR="$TMP/src" CODEX_GIT_TRUST_BIN_DIR="$TMP/sbin"
check "e: fixture with every bind exits 0" "$(t [ "$RC" = 0 ])" "got $RC: $(tr '\n' '|' < "$ST/out")"
mounts="$(grep -A1 -x -- '--mount' "$ST/run.argv" | grep -vx -- '--mount' | grep -vx -- '--' || true)"
mount_count="$(printf '%s\n' "$mounts" | grep -c . || true)"
check "e: five binds (fixture dir, src, three binaries)" "$(t [ "$mount_count" = 5 ])" "got $mount_count"
bad="$(printf '%s\n' "$mounts" | grep -vE '^type=bind,source=/[^,]*,target=/[^,]*,readonly$' || true)"
check "e: every bind is --mount type=bind,...,readonly" "$(t [ -z "$bad" ])" "bad: $bad"
check "e: the src override is bound read-only at /app/src" \
  "$(t grep -qx "type=bind,source=$TMP/src,target=/app/src,readonly" "$ST/run.argv")"
check "e: no -v/--volume in the run argv" "$(t lacks "$ST/run.argv" '^(-v.*|--volume(=.*)?)$')"
check "e: the fixture container runs with --network none" \
  "$(t network_is "$ST/run.argv" none)"

echo "cases=$cases passed=$passed"
if [ "$cases" -lt "$MIN_CASES" ]; then
  echo "FAIL: only $cases cases ran (expected >= $MIN_CASES)" >&2
  exit 1
fi
[ "$passed" -eq "$cases" ]
