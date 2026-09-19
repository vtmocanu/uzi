#!/bin/sh
# Hermetic contract tests for scripts/golangci-lint.sh.
set -eu

ROOT="$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)"
WRAPPER="$ROOT/scripts/golangci-lint.sh"
TMP="$(mktemp -d)"
CANCEL_LAUNCHER_PID=""
CANCEL_WRAPPER_PID=""
CANCEL_CHILD_PID=""
CANCEL_WATCHDOG_PID=""
cleanup() {
  if [ -n "$CANCEL_WATCHDOG_PID" ]; then
    kill -TERM "$CANCEL_WATCHDOG_PID" 2>/dev/null || true
    wait "$CANCEL_WATCHDOG_PID" 2>/dev/null || true
  fi
  if [ -n "$CANCEL_WRAPPER_PID" ]; then
    kill -TERM "$CANCEL_WRAPPER_PID" 2>/dev/null || true
    wait "$CANCEL_WRAPPER_PID" 2>/dev/null || true
  fi
  if [ -n "$CANCEL_CHILD_PID" ]; then
    kill -TERM "$CANCEL_CHILD_PID" 2>/dev/null || true
  fi
  if [ -n "$CANCEL_LAUNCHER_PID" ]; then
    kill -TERM "$CANCEL_LAUNCHER_PID" 2>/dev/null || true
    wait "$CANCEL_LAUNCHER_PID" 2>/dev/null || true
  fi
  rm -rf "$TMP"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

fail() {
  echo "golangci-lint.test.sh: FAIL: $*" >&2
  exit 1
}

assert_eq() {
  expected="$1"
  actual="$2"
  label="$3"
  [ "$actual" = "$expected" ] || fail "$label: expected '$expected', got '$actual'"
}

assert_contains() {
  file="$1"
  literal="$2"
  grep -Fq -- "$literal" "$file" || fail "$file does not contain: $literal"
}

assert_empty() {
  file="$1"
  [ ! -s "$file" ] || fail "$file should be empty"
}

TASK_PIN="$(awk '$1 == "GOLANGCI_LINT_VERSION:" { print $2; exit }' "$ROOT/Taskfile.yml")"
DARWIN_PIN="$(awk -F\" '$1 == "GOLANGCI_LINT_DARWIN_ARM64_VERSION=" { print $2; exit }' "$WRAPPER")"
LINUX_PIN="$(awk -F\" '$1 == "GOLANGCI_LINT_LINUX_AMD64_VERSION=" { print $2; exit }' "$WRAPPER")"
DARWIN_SHA="$(awk -F\" '$1 == "GOLANGCI_LINT_DARWIN_ARM64_SHA256=" { print $2; exit }' "$WRAPPER")"
LINUX_SHA="$(awk -F\" '$1 == "GOLANGCI_LINT_LINUX_AMD64_SHA256=" { print $2; exit }' "$WRAPPER")"
VERSION_NO_V="${TASK_PIN#v}"

[ -n "$TASK_PIN" ] || fail "Taskfile golangci-lint pin is missing"
assert_eq "$TASK_PIN" "$DARWIN_PIN" "Taskfile/Darwin pin"
assert_eq "$TASK_PIN" "$LINUX_PIN" "Taskfile/Linux pin"

FAKE_BIN="$TMP/bin"
mkdir -p "$FAKE_BIN"

cat > "$FAKE_BIN/uname" <<'EOF'
#!/bin/sh
case "${1:-}" in
  -s) printf '%s\n' "$FAKE_UNAME_S" ;;
  -m) printf '%s\n' "$FAKE_UNAME_M" ;;
  *) exit 2 ;;
esac
EOF

cat > "$FAKE_BIN/mktemp" <<'EOF'
#!/bin/sh
dir="$FAKE_MKTEMP_ROOT/run.$$"
mkdir -p "$dir"
printf '%s\n' "$dir" >> "$FAKE_MKTEMP_LOG"
printf '%s\n' "$dir"
EOF

cat > "$FAKE_BIN/sha256sum" <<'EOF'
#!/bin/sh
IFS= read -r line || exit 1
expected="${line%%  *}"
file="${line#*  }"
[ "$expected" = "$FAKE_EXPECTED_SHA" ] || exit 1
[ "$(cat "$file" 2>/dev/null || true)" = "archive:$FAKE_ASSET:$FAKE_VERSION" ]
EOF

cat > "$FAKE_BIN/curl" <<'EOF'
#!/bin/sh
[ "${FAKE_DOWNLOAD_FAIL:-0}" != "1" ] || exit 22
if [ "${FAKE_DOWNLOAD_DELAY:-0}" != "0" ]; then
  sleep "$FAKE_DOWNLOAD_DELAY"
fi
out=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    -*o)
      shift
      out="${1:-}"
      ;;
  esac
  shift
done
[ -n "$out" ] || exit 2
printf 'archive:%s:%s\n' "$FAKE_ASSET" "$FAKE_VERSION" > "$out"
printf '%s\n' "$FAKE_ASSET" >> "$FAKE_DOWNLOAD_LOG"
EOF

cat > "$FAKE_BIN/gzip" <<'EOF'
#!/bin/sh
[ "${1:-}" = "-dc" ] || exit 2
cat "${2:-}"
EOF

cat > "$FAKE_BIN/go" <<'EOF'
#!/bin/sh
[ "${1:-}" = "env" ] && [ "${2:-}" = "GOVERSION" ] || exit 2
printf 'go%s\n' "$FAKE_AMBIENT_GO"
EOF

cat > "$FAKE_BIN/tar" <<'EOF'
#!/bin/sh
prefix="golangci-lint-$FAKE_VERSION-$FAKE_ASSET"
case "${1:-}" in
  -tf)
    printf '%s\n' "$prefix/LICENSE" "$prefix/README.md" "$prefix/golangci-lint"
    if [ "${FAKE_TAR_EXTRA:-0}" = "1" ]; then
      printf '%s\n' "../escape"
    fi
    ;;
  -tvf)
    binary_mode="-rwxr-xr-x"
    if [ "${FAKE_TAR_LINK:-0}" = "1" ]; then
      binary_mode="lrwxr-xr-x"
    fi
    printf '%s\n' \
      "-rw-r--r-- user/group 1 2026-01-01 00:00 $prefix/LICENSE" \
      "-rw-r--r-- user/group 1 2026-01-01 00:00 $prefix/README.md" \
      "$binary_mode user/group 1 2026-01-01 00:00 $prefix/golangci-lint"
    ;;
  -xf)
    out=""
    shift 2
    while [ "$#" -gt 0 ]; do
      case "$1" in
        -C)
          shift
          out="${1:-}"
          ;;
      esac
      shift
    done
    [ -n "$out" ] || exit 2
    dest="$out/$prefix"
    mkdir -p "$dest"
    : > "$dest/LICENSE"
    : > "$dest/README.md"
    if [ "${FAKE_TAR_EMPTY_BINARY:-0}" = "1" ]; then
      : > "$dest/golangci-lint"
    else
      cp "$FAKE_LINTER_TEMPLATE" "$dest/golangci-lint"
    fi
    chmod +x "$dest/golangci-lint"
    ;;
  *) exit 2 ;;
esac
EOF

FAKE_LINTER_TEMPLATE="$TMP/fake-golangci-lint"
cat > "$FAKE_LINTER_TEMPLATE" <<'EOF'
#!/bin/sh
if [ "${1:-}" = "version" ]; then
  printf 'golangci-lint has version fake built with go%s\n' "$FAKE_BUILD_GO"
  exit 0
fi
printf '%s\n' "$*" >> "$FAKE_EXEC_LOG"
if [ "${FAKE_CHILD_BLOCK:-0}" = "1" ]; then
  # The PID file is the test's readiness handshake. Install handlers before publishing it,
  # otherwise the caller can signal after seeing the PID but before the traps exist.
  trap 'printf "TERM\n" >> "$FAKE_SIGNAL_LOG"; exit 143' TERM
  trap 'printf "INT\n" >> "$FAKE_SIGNAL_LOG"; exit 130' INT
  printf '%s\n' "$$" > "$FAKE_CHILD_PID_FILE"
  while :; do sleep 1; done
fi
exit "${FAKE_CHILD_STATUS:-0}"
EOF

# The blocking tests signal immediately after this PID appears, so the marker must mean
# both handlers are installed. This static guard makes that readiness contract deterministic.
awk '
  /trap .* TERM$/ { term_ready = 1 }
  /trap .* INT$/ { int_ready = 1 }
  /FAKE_CHILD_PID_FILE/ { exit(term_ready && int_ready ? 0 : 1) }
  END { if (!(term_ready && int_ready)) exit 1 }
' "$FAKE_LINTER_TEMPLATE" \
  || fail "fake linter publishes readiness before installing signal handlers"

# Shell `cmd &` starts cmd with SIGINT ignored. Spawn through Node so the wrapper
# enters with normal signal dispositions, matching foreground Task execution.
SPAWN_HELPER="$TMP/spawn-wait.mjs"
cat > "$SPAWN_HELPER" <<'EOF'
import { spawn } from "node:child_process";
import { writeFileSync } from "node:fs";

const [pidFile, cwd, command, ...args] = process.argv.slice(2);
const child = spawn(command, args, {
  cwd,
  env: process.env,
  stdio: "inherit",
  detached: process.env.FAKE_DETACHED === "1",
});
child.once("spawn", () => writeFileSync(pidFile, `${child.pid}\n`));
child.once("error", () => process.exit(2));
child.once("exit", (code, signal) => {
  if (code !== null) process.exit(code);
  const statuses = { SIGINT: 130, SIGTERM: 143, SIGKILL: 137 };
  process.exit(statuses[signal] ?? 1);
});
EOF
chmod +x "$FAKE_BIN/uname" "$FAKE_BIN/mktemp" "$FAKE_BIN/sha256sum" "$FAKE_BIN/curl" \
  "$FAKE_BIN/gzip" "$FAKE_BIN/go" "$FAKE_BIN/tar" "$FAKE_LINTER_TEMPLATE"

DOWNLOAD_LOG="$TMP/downloads.log"
EXEC_LOG="$TMP/executions.log"
WRAPPER_TMP_ROOT="$TMP/wrapper-tmp"
MKTEMP_LOG="$TMP/mktemp.log"
CHILD_PID_FILE="$TMP/child.pid"
WRAPPER_PID_FILE="$TMP/wrapper.pid"
SIGNAL_LOG="$TMP/signals.log"
mkdir -p "$WRAPPER_TMP_ROOT"
: > "$DOWNLOAD_LOG"
: > "$EXEC_LOG"
: > "$MKTEMP_LOG"
: > "$SIGNAL_LOG"
RUN_RC=0

run_wrapper() {
  output="$1"
  os="$2"
  arch="$3"
  cache="$4"
  cwd="$5"
  build_go="$6"
  child_status="$7"
  download_fail="$8"
  shift 8

  case "$os/$arch" in
    Darwin/arm64)
      asset="darwin-arm64"
      expected_sha="$DARWIN_SHA"
      ;;
    Linux/x86_64 | Linux/amd64)
      asset="linux-amd64"
      expected_sha="$LINUX_SHA"
      ;;
    *)
      asset="unsupported"
      expected_sha="unsupported"
      ;;
  esac

  RUN_RC=0
  (
    cd "$cwd"
    env \
      PATH="$FAKE_BIN:$PATH" \
      FAKE_MKTEMP_ROOT="$WRAPPER_TMP_ROOT" \
      FAKE_MKTEMP_LOG="$MKTEMP_LOG" \
      UZI_GOLANGCI_LINT_DIR="$cache" \
      FAKE_UNAME_S="$os" \
      FAKE_UNAME_M="$arch" \
      FAKE_ASSET="$asset" \
      FAKE_VERSION="$VERSION_NO_V" \
      FAKE_EXPECTED_SHA="$expected_sha" \
      FAKE_DOWNLOAD_FAIL="$download_fail" \
      FAKE_DOWNLOAD_DELAY="${FAKE_DOWNLOAD_DELAY:-0}" \
      FAKE_DOWNLOAD_LOG="$DOWNLOAD_LOG" \
      FAKE_TAR_EXTRA="${FAKE_TAR_EXTRA:-0}" \
      FAKE_TAR_LINK="${FAKE_TAR_LINK:-0}" \
      FAKE_TAR_EMPTY_BINARY="${FAKE_TAR_EMPTY_BINARY:-0}" \
      FAKE_LINTER_TEMPLATE="$FAKE_LINTER_TEMPLATE" \
      FAKE_BUILD_GO="$build_go" \
      FAKE_AMBIENT_GO="${FAKE_AMBIENT_GO:-1.26.6}" \
      FAKE_CHILD_STATUS="$child_status" \
      FAKE_CHILD_BLOCK="${FAKE_CHILD_BLOCK:-0}" \
      FAKE_CHILD_PID_FILE="$CHILD_PID_FILE" \
      FAKE_SIGNAL_LOG="$SIGNAL_LOG" \
      FAKE_EXEC_LOG="$EXEC_LOG" \
      "$WRAPPER" "$TASK_PIN" "$@"
  ) > "$output" 2>&1 || RUN_RC=$?
}

START_PID=""
start_concurrent_wrapper() {
  output="$1"
  cache="$2"
  (
    cd "$TMP"
    exec env \
      PATH="$FAKE_BIN:$PATH" \
      FAKE_MKTEMP_ROOT="$WRAPPER_TMP_ROOT" \
      FAKE_MKTEMP_LOG="$MKTEMP_LOG" \
      UZI_GOLANGCI_LINT_DIR="$cache" \
      FAKE_UNAME_S=Darwin \
      FAKE_UNAME_M=arm64 \
      FAKE_ASSET=darwin-arm64 \
      FAKE_VERSION="$VERSION_NO_V" \
      FAKE_EXPECTED_SHA="$DARWIN_SHA" \
      FAKE_DOWNLOAD_FAIL=0 \
      FAKE_DOWNLOAD_DELAY=0.2 \
      FAKE_DOWNLOAD_LOG="$DOWNLOAD_LOG" \
      FAKE_TAR_EXTRA=0 \
      FAKE_TAR_LINK=0 \
      FAKE_TAR_EMPTY_BINARY=0 \
      FAKE_LINTER_TEMPLATE="$FAKE_LINTER_TEMPLATE" \
      FAKE_BUILD_GO=1.26.2 \
      FAKE_AMBIENT_GO=1.26.6 \
      FAKE_CHILD_STATUS=0 \
      FAKE_CHILD_BLOCK=0 \
      FAKE_CHILD_PID_FILE="$CHILD_PID_FILE" \
      FAKE_SIGNAL_LOG="$SIGNAL_LOG" \
      FAKE_EXEC_LOG="$EXEC_LOG" \
      "$WRAPPER" "$TASK_PIN" version
  ) > "$output" 2>&1 &
  START_PID=$!
}

start_blocking_wrapper() {
  output="$1"
  cache="$2"
  detached="${3:-0}"
  (
    exec env \
      PATH="$FAKE_BIN:$PATH" \
      FAKE_MKTEMP_ROOT="$WRAPPER_TMP_ROOT" \
      FAKE_MKTEMP_LOG="$MKTEMP_LOG" \
      UZI_GOLANGCI_LINT_DIR="$cache" \
      FAKE_UNAME_S=Darwin \
      FAKE_UNAME_M=arm64 \
      FAKE_ASSET=darwin-arm64 \
      FAKE_VERSION="$VERSION_NO_V" \
      FAKE_EXPECTED_SHA="$DARWIN_SHA" \
      FAKE_DOWNLOAD_FAIL=1 \
      FAKE_DOWNLOAD_DELAY=0 \
      FAKE_DOWNLOAD_LOG="$DOWNLOAD_LOG" \
      FAKE_TAR_EXTRA=0 \
      FAKE_TAR_LINK=0 \
      FAKE_TAR_EMPTY_BINARY=0 \
      FAKE_LINTER_TEMPLATE="$FAKE_LINTER_TEMPLATE" \
      FAKE_BUILD_GO=1.26.2 \
      FAKE_AMBIENT_GO=1.26.6 \
      FAKE_CHILD_STATUS=0 \
      FAKE_CHILD_BLOCK=1 \
      FAKE_CHILD_PID_FILE="$CHILD_PID_FILE" \
      FAKE_SIGNAL_LOG="$SIGNAL_LOG" \
      FAKE_EXEC_LOG="$EXEC_LOG" \
      FAKE_DETACHED="$detached" \
      node "$SPAWN_HELPER" "$WRAPPER_PID_FILE" "$TMP" \
        "$WRAPPER" "$TASK_PIN" run ./...
  ) > "$output" 2>&1 &
  START_PID=$!
}

seed_archive() {
  cache="$1"
  asset="$2"
  dir="$cache/$VERSION_NO_V/$asset"
  mkdir -p "$dir"
  printf 'archive:%s:%s\n' "$asset" "$VERSION_NO_V" \
    > "$dir/golangci-lint-$VERSION_NO_V-$asset.tar.gz"
}

# Cold and warm Darwin runs: one download, then a valid offline cache hit.
DARWIN_CACHE="$TMP/cache-darwin"
run_wrapper "$TMP/darwin-cold.out" Darwin arm64 "$DARWIN_CACHE" "$TMP" 1.26.2 0 0 version
assert_eq 0 "$RUN_RC" "Darwin cold run"
assert_contains "$TMP/darwin-cold.out" "built with go1.26.2"
run_wrapper "$TMP/darwin-warm.out" Darwin arm64 "$DARWIN_CACHE" "$TMP" 1.26.2 0 1 version
assert_eq 0 "$RUN_RC" "Darwin warm offline run"
assert_eq 1 "$(grep -Fxc 'darwin-arm64' "$DOWNLOAD_LOG")" "Darwin download count"

# Linux uses its own archive and digest rather than reusing Darwin's.
LINUX_CACHE="$TMP/cache-linux"
run_wrapper "$TMP/linux-cold.out" Linux x86_64 "$LINUX_CACHE" "$TMP" 1.26.2 0 0 version
assert_eq 0 "$RUN_RC" "Linux cold run"
assert_eq 1 "$(grep -Fxc 'linux-amd64' "$DOWNLOAD_LOG")" "Linux download count"

# Two cold callers, and two callers recovering one corrupt cache, each use a
# private verified snapshot. Atomic cache publication cannot break either peer.
CONCURRENT_COLD="$TMP/cache-concurrent-cold"
start_concurrent_wrapper "$TMP/concurrent-cold-a.out" "$CONCURRENT_COLD"
pid_a="$START_PID"
start_concurrent_wrapper "$TMP/concurrent-cold-b.out" "$CONCURRENT_COLD"
pid_b="$START_PID"
rc_a=0
rc_b=0
wait "$pid_a" || rc_a=$?
wait "$pid_b" || rc_b=$?
assert_eq 0 "$rc_a" "concurrent cold caller A"
assert_eq 0 "$rc_b" "concurrent cold caller B"

CONCURRENT_CORRUPT="$TMP/cache-concurrent-corrupt"
seed_archive "$CONCURRENT_CORRUPT" darwin-arm64
printf 'corrupt\n' > "$CONCURRENT_CORRUPT/$VERSION_NO_V/darwin-arm64/golangci-lint-$VERSION_NO_V-darwin-arm64.tar.gz"
start_concurrent_wrapper "$TMP/concurrent-corrupt-a.out" "$CONCURRENT_CORRUPT"
pid_a="$START_PID"
start_concurrent_wrapper "$TMP/concurrent-corrupt-b.out" "$CONCURRENT_CORRUPT"
pid_b="$START_PID"
rc_a=0
rc_b=0
wait "$pid_a" || rc_a=$?
wait "$pid_b" || rc_b=$?
assert_eq 0 "$rc_a" "concurrent corrupt caller A"
assert_eq 0 "$rc_b" "concurrent corrupt caller B"

# A corrupt cache plus a failed replacement download executes no binary.
CORRUPT_CACHE="$TMP/cache-corrupt"
seed_archive "$CORRUPT_CACHE" darwin-arm64
printf 'corrupt\n' > "$CORRUPT_CACHE/$VERSION_NO_V/darwin-arm64/golangci-lint-$VERSION_NO_V-darwin-arm64.tar.gz"
: > "$EXEC_LOG"
run_wrapper "$TMP/corrupt.out" Darwin arm64 "$CORRUPT_CACHE" "$TMP" 1.26.2 0 1 run ./...
assert_eq 2 "$RUN_RC" "corrupt offline cache"
assert_empty "$EXEC_LOG"
assert_contains "$TMP/corrupt.out" "bounded download failed"

# A digest-valid archive with an extra traversal member is rejected before
# extraction, even though its compressed bytes passed verification.
STRUCTURE_CACHE="$TMP/cache-structure"
seed_archive "$STRUCTURE_CACHE" darwin-arm64
: > "$EXEC_LOG"
FAKE_TAR_EXTRA=1
run_wrapper "$TMP/structure.out" Darwin arm64 "$STRUCTURE_CACHE" "$TMP" 1.26.2 0 1 run ./...
unset FAKE_TAR_EXTRA
assert_eq 2 "$RUN_RC" "unexpected archive member"
assert_empty "$EXEC_LOG"
assert_contains "$TMP/structure.out" "unexpected member paths"

# Expected names are insufficient if the executable itself is a symlink.
LINK_CACHE="$TMP/cache-link"
seed_archive "$LINK_CACHE" darwin-arm64
: > "$EXEC_LOG"
FAKE_TAR_LINK=1
run_wrapper "$TMP/link.out" Darwin arm64 "$LINK_CACHE" "$TMP" 1.26.2 0 1 run ./...
unset FAKE_TAR_LINK
assert_eq 2 "$RUN_RC" "non-regular archive member"
assert_empty "$EXEC_LOG"
assert_contains "$TMP/link.out" "non-regular or extra member"

# Extraction-time size validation rejects an empty executable before launch.
EMPTY_CACHE="$TMP/cache-empty"
seed_archive "$EMPTY_CACHE" darwin-arm64
: > "$EXEC_LOG"
FAKE_TAR_EMPTY_BINARY=1
run_wrapper "$TMP/empty.out" Darwin arm64 "$EMPTY_CACHE" "$TMP" 1.26.2 0 1 run ./...
unset FAKE_TAR_EMPTY_BINARY
assert_eq 2 "$RUN_RC" "empty extracted binary"
assert_empty "$EXEC_LOG"
assert_contains "$TMP/empty.out" "invalid size 0"

# Unsupported hosts fail before download or execution.
: > "$EXEC_LOG"
run_wrapper "$TMP/unsupported.out" FreeBSD amd64 "$TMP/cache-unsupported" "$TMP" 1.26.2 0 0 run ./...
assert_eq 2 "$RUN_RC" "unsupported host"
assert_empty "$EXEC_LOG"
assert_contains "$TMP/unsupported.out" "unsupported host FreeBSD/amd64"

# A child's exact nonzero status survives wrapper cleanup.
STATUS_CACHE="$TMP/cache-status"
seed_archive "$STATUS_CACHE" darwin-arm64
: > "$EXEC_LOG"
run_wrapper "$TMP/status.out" Darwin arm64 "$STATUS_CACHE" "$TMP" 1.26.2 3 1 run ./...
assert_eq 3 "$RUN_RC" "child exit propagation"
assert_contains "$EXEC_LOG" "run ./..."

# Direct INT and TERM reach the exec'd linter process and preserve conventional
# shell statuses. A watchdog makes a lost signal fail, never hang.
CANCEL_CACHE="$TMP/cache-cancel"
seed_archive "$CANCEL_CACHE" darwin-arm64
assert_signal_forwarded() {
  cancel_signal="$1"
  expected_status="$2"
  cancel_output="$TMP/cancel-$cancel_signal.out"
  : > "$CHILD_PID_FILE"
  : > "$WRAPPER_PID_FILE"
  : > "$SIGNAL_LOG"
  start_blocking_wrapper "$cancel_output" "$CANCEL_CACHE"
  CANCEL_LAUNCHER_PID="$START_PID"
  tries=0
  while [ ! -s "$CHILD_PID_FILE" ] || [ ! -s "$WRAPPER_PID_FILE" ]; do
    tries=$((tries + 1))
    [ "$tries" -le 100 ] || fail "blocking child did not start for $cancel_signal"
    sleep 0.02
  done
  CANCEL_WRAPPER_PID="$(cat "$WRAPPER_PID_FILE")"
  CANCEL_CHILD_PID="$(cat "$CHILD_PID_FILE")"
  (
    sleep 5
    kill -KILL "$CANCEL_WRAPPER_PID" 2>/dev/null || true
    kill -KILL "$CANCEL_CHILD_PID" 2>/dev/null || true
    kill -KILL "$CANCEL_LAUNCHER_PID" 2>/dev/null || true
  ) &
  CANCEL_WATCHDOG_PID=$!
  kill -s "$cancel_signal" "$CANCEL_WRAPPER_PID"
  cancel_rc=0
  wait "$CANCEL_LAUNCHER_PID" || cancel_rc=$?
  CANCEL_LAUNCHER_PID=""
  CANCEL_WRAPPER_PID=""
  kill -TERM "$CANCEL_WATCHDOG_PID" 2>/dev/null || true
  wait "$CANCEL_WATCHDOG_PID" 2>/dev/null || true
  CANCEL_WATCHDOG_PID=""
  assert_eq "$expected_status" "$cancel_rc" "wrapper $cancel_signal propagation"
  if kill -0 "$CANCEL_CHILD_PID" 2>/dev/null; then
    kill -KILL "$CANCEL_CHILD_PID" 2>/dev/null || true
    fail "wrapper $cancel_signal left child $CANCEL_CHILD_PID alive"
  fi
  CANCEL_CHILD_PID=""
  # The child records the forwarded signal from its own trap, a separate process
  # whose append can lag the wrapper's exit (which returns 128+signum without
  # waiting on the child). cancel_rc above already proved the signal propagated;
  # poll the log for the child's record rather than reading it once and racing.
  tries=0
  while ! grep -Fq -- "$cancel_signal" "$SIGNAL_LOG" 2>/dev/null; do
    tries=$((tries + 1))
    [ "$tries" -le 150 ] || fail "$SIGNAL_LOG never recorded forwarded $cancel_signal"
    sleep 0.02
  done
}
assert_signal_forwarded INT 130
assert_signal_forwarded TERM 143

# A catchable process-group cancellation also reaches the linter, while the
# watcher survives long enough to remove the private extraction directory.
: > "$CHILD_PID_FILE"
: > "$WRAPPER_PID_FILE"
: > "$MKTEMP_LOG"
: > "$SIGNAL_LOG"
start_blocking_wrapper "$TMP/cancel-group.out" "$CANCEL_CACHE" 1
CANCEL_LAUNCHER_PID="$START_PID"
tries=0
while [ ! -s "$CHILD_PID_FILE" ] || [ ! -s "$WRAPPER_PID_FILE" ] || [ ! -s "$MKTEMP_LOG" ]; do
  tries=$((tries + 1))
  [ "$tries" -le 100 ] || fail "process-group cancellation child did not start"
  sleep 0.02
done
CANCEL_WRAPPER_PID="$(cat "$WRAPPER_PID_FILE")"
CANCEL_CHILD_PID="$(cat "$CHILD_PID_FILE")"
wrapper_tmp="$(awk 'END { print }' "$MKTEMP_LOG")"
(
  sleep 5
  kill -KILL "-$CANCEL_WRAPPER_PID" 2>/dev/null || true
  kill -KILL "$CANCEL_LAUNCHER_PID" 2>/dev/null || true
) &
CANCEL_WATCHDOG_PID=$!
kill -TERM "-$CANCEL_WRAPPER_PID"
group_rc=0
wait "$CANCEL_LAUNCHER_PID" || group_rc=$?
CANCEL_LAUNCHER_PID=""
CANCEL_WRAPPER_PID=""
kill -TERM "$CANCEL_WATCHDOG_PID" 2>/dev/null || true
wait "$CANCEL_WATCHDOG_PID" 2>/dev/null || true
CANCEL_WATCHDOG_PID=""
assert_eq 143 "$group_rc" "process-group TERM propagation"
if kill -0 "$CANCEL_CHILD_PID" 2>/dev/null; then
  kill -KILL "$CANCEL_CHILD_PID" 2>/dev/null || true
  fail "process-group TERM left child $CANCEL_CHILD_PID alive"
fi
CANCEL_CHILD_PID=""
# Same cross-process append lag as the single-signal case above: poll, don't race.
tries=0
while ! grep -Fq -- "TERM" "$SIGNAL_LOG" 2>/dev/null; do
  tries=$((tries + 1))
  [ "$tries" -le 150 ] || fail "$SIGNAL_LOG never recorded group-forwarded TERM"
  sleep 0.02
done
tries=0
while [ -e "$wrapper_tmp" ]; do
  tries=$((tries + 1))
  [ "$tries" -le 150 ] || fail "cleanup watcher left $wrapper_tmp after group TERM"
  sleep 0.02
done

# Regression: toolchain, not only go, selects the effective target language.
D6_BAD="$TMP/d6-bad"
mkdir -p "$D6_BAD"
cat > "$D6_BAD/go.mod" <<'EOF'
module example.com/d6

go 1.26.4
toolchain go1.27.1
EOF
D6_CACHE="$TMP/cache-d6"
seed_archive "$D6_CACHE" darwin-arm64
: > "$EXEC_LOG"
run_wrapper "$TMP/d6-bad.out" Darwin arm64 "$D6_CACHE" "$D6_BAD" 1.26.2 0 1 run ./...
assert_eq 2 "$RUN_RC" "toolchain language guard"
assert_empty "$EXEC_LOG"
assert_contains "$TMP/d6-bad.out" "selected Go language 1.27"
assert_contains "$TMP/d6-bad.out" "toolchain directive go1.27.1"

D6_GOOD="$TMP/d6-good"
mkdir -p "$D6_GOOD"
cat > "$D6_GOOD/go.mod" <<'EOF'
module example.com/d6

go 1.26.4
toolchain go1.26.6
EOF

# A newer ambient Go under GOTOOLCHAIN=auto also selects the target language.
: > "$EXEC_LOG"
FAKE_AMBIENT_GO=1.27.1
run_wrapper "$TMP/d6-ambient.out" Darwin arm64 "$D6_CACHE" "$D6_GOOD" 1.26.2 0 1 run ./...
unset FAKE_AMBIENT_GO
assert_eq 2 "$RUN_RC" "ambient Go language guard"
assert_empty "$EXEC_LOG"
assert_contains "$TMP/d6-ambient.out" "go env GOVERSION go1.27.1"

# Control: same-language directives and ambient Go allow the child to run.
: > "$EXEC_LOG"
run_wrapper "$TMP/d6-good.out" Darwin arm64 "$D6_CACHE" "$D6_GOOD" 1.26.2 0 1 run ./...
assert_eq 0 "$RUN_RC" "compatible toolchain control"
assert_contains "$EXEC_LOG" "run ./..."

printf 'golangci-lint wrapper tests: PASS\n'
