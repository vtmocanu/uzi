#!/usr/bin/env bash
# Behavioral regression tests for run-store-it.sh (teardown and the #2304 TCP readiness probe),
# driven by a FAKE `docker` on PATH. Proves the EXIT-trap cleanup removes the
# container's ANONYMOUS DATA VOLUME (docker rm -v), not just the container -- the
# leak that filled the worker's Docker data-root before this fix. No real docker
# daemon, no Postgres and no real `go test`: fakes exercise readiness failures
# and the ready-server package sweep, including timeout and aggregate-log guards.
# Exits non-zero on any failed assertion.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SCRIPT="$HERE/run-store-it.sh"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

FAKEBIN="$TMP/bin"
mkdir -p "$FAKEBIN"
LOG="$TMP/docker.log"

# Fake docker. Every invocation appends its argv to $LOG (a FILE, so it survives
# the real cleanup's `>/dev/null 2>&1` redirect), then returns per-subcommand:
#   image -> 0  (image present; the pull block is skipped)
#   run   -> 0  (container "started")
#   exec  -> 1  (pg_isready never ready -> readiness timeout -> exit 1 -> trap)
#   rm    -> 0  (record the teardown)
# UNquoted heredoc: $LOG expands now (test time); the fake's own \$* / \$1 stay
# literal for run time.
cat > "$FAKEBIN/docker" <<EOF
#!/usr/bin/env bash
printf '%s\n' "\$*" >> '$LOG'
case "\$1" in
  image) exit 0 ;;
  run)   exit 0 ;;
  exec)  exit 1 ;;
  rm)    exit 0 ;;
  *)     exit 0 ;;
esac
EOF
chmod +x "$FAKEBIN/docker"

PATH="$FAKEBIN:$PATH"
export PATH

fails=0

# Drive the real script: readiness times out in ~1s (PGWAIT=1), so it exits 1 down
# the infrastructure-failure path, firing the EXIT trap (cleanup). A non-zero exit
# is expected; `|| true` keeps `set -e` from aborting the test on it.
UZI_STORE_IT_PG_WAIT_SECS=1 bash "$SCRIPT" >/dev/null 2>&1 || true

rm_count="$(grep -c '^rm ' "$LOG" || true)"
rm_line="$(grep -m1 '^rm ' "$LOG" || true)"

if [ "$rm_count" = "1" ]; then
  printf 'PASS: cleanup ran exactly one `docker rm`\n'
else
  printf 'FAIL: expected exactly one `docker rm`, got %s\n' "$rm_count"
  fails=$((fails + 1))
fi

# THE REGRESSION: the teardown must remove the anonymous volume (docker rm -v),
# not just the container. RED on the pre-fix `docker rm -f "$NAME"`.
case " $rm_line " in
  *" -v "*) printf 'PASS: `docker rm` carries -v (anonymous volume removed)\n' ;;
  *)        printf 'FAIL: `docker rm` is missing -v -> anonymous volume leaks: %s\n' "$rm_line"
            fails=$((fails + 1)) ;;
esac

case "$rm_line" in
  *uzi-store-it-*) printf 'PASS: teardown targets the throwaway container by name\n' ;;
  *)               printf 'FAIL: teardown did not target uzi-store-it-<pid>: %s\n' "$rm_line"
                   fails=$((fails + 1)) ;;
esac

# Issue #2304: the readiness probe must use TCP. The fake models the postgres
# image's startup timeline on a shared tick (one per `docker exec`): tick 1 is
# initdb's temporary server (Unix socket only), ticks 2-3 its shutdown (nothing
# answers), tick 4+ the final server (socket and TCP). A socket-only probe passes on
# tick 1 and its recheck fails on tick 2: the false timeout. A TCP probe waits for
# tick 4. A fake `go` records that the script got past readiness.
LOG2="$TMP/docker2.log"
TICK="$TMP/tick"
echo 0 > "$TICK"
FAKEBIN2="$TMP/bin2"
mkdir -p "$FAKEBIN2"
cat > "$FAKEBIN2/docker" <<EOF
#!/usr/bin/env bash
printf '%s\n' "\$*" >> '$LOG2'
case "\$1" in
  exec)
    t=\$((\$(cat '$TICK') + 1)); echo "\$t" > '$TICK'
    tcp=0; case " \$* " in *" -h 127.0.0.1 "*) tcp=1 ;; esac
    if [ "\$t" -ge 4 ]; then exit 0; fi
    if [ "\$t" -eq 1 ] && [ "\$tcp" -eq 0 ]; then exit 0; fi
    exit 1 ;;
  *) exit 0 ;;
esac
EOF
cat > "$FAKEBIN2/go" <<EOF
#!/usr/bin/env bash
printf 'go %s\n' "\$*" >> '$LOG2'
EOF
chmod +x "$FAKEBIN2/docker" "$FAKEBIN2/go"

# The fake go prints no test output, so the script fails later at its skip guard;
# only whether it reached go matters here.
out="$(PATH="$FAKEBIN2:$PATH" UZI_STORE_IT_PG_WAIT_SECS=10 bash "$SCRIPT" 2>&1 || true)"
if grep -q '^go test' "$LOG2" && ! printf '%s' "$out" | grep -q 'never became ready'; then
  printf 'PASS: readiness waits for the final TCP server, not initdb'"'"'s temporary socket server\n'
else
  printf 'FAIL: readiness accepted initdb'"'"'s temporary server and timed out on its shutdown (#2304)\n'
  fails=$((fails + 1))
fi

# Issue #2378: execute the CI service's actual health command against the same
# socket-only init/final-TCP distinction. No Docker startup or wall-clock wait.
health_cmd="$(awk -F '"' '/^[[:space:]]*--health-cmd / { print $2; n++ } END { if (n != 1) exit 1 }' \
  "$HERE/../.github/workflows/ci.yml")"
cat > "$FAKEBIN2/pg_isready" <<'STUB'
#!/bin/sh
printf '%s\n' "$*" >> "$PROBE_LOG"
if [ "$PG_STAGE" = final ]; then exit 0; fi
case " $* " in *" -h 127.0.0.1 "*) exit 1 ;; *) exit 0 ;; esac
STUB
chmod +x "$FAKEBIN2/pg_isready"
init_rc=0
PATH="$FAKEBIN2:$PATH" PG_STAGE=init PROBE_LOG="$TMP/ci-probe.log" \
  sh -c "$health_cmd" || init_rc=$?
final_rc=0
PATH="$FAKEBIN2:$PATH" PG_STAGE=final PROBE_LOG="$TMP/ci-probe.log" \
  sh -c "$health_cmd" || final_rc=$?
if [ "$init_rc" -ne 0 ] && [ "$final_rc" -eq 0 ] && [ -s "$TMP/ci-probe.log" ]; then
  printf 'PASS: CI health command rejects socket-only initdb and accepts the final server\n'
else
  printf 'FAIL: CI health command accepted initdb or rejected the final server (#2378)\n'
  fails=$((fails + 1))
fi

# Issue #2665: a ready server exposes the public Docker/Go seams without waiting
# for real LiveDB tests. Each scenario is bounded to seven package calls plus the
# optional handler call; a failed scenario is recorded without blocking siblings.
READYBIN="$TMP/ready-bin"
mkdir -p "$READYBIN"
cat > "$READYBIN/docker" <<'STUB'
#!/usr/bin/env bash
printf '%s\n' "$*" >> "$CASE_DIR/docker.log"
exit 0
STUB
cat > "$READYBIN/go" <<'STUB'
#!/usr/bin/env bash
printf '%s' "${UZI_TEST_DATABASE_URL:-}" >> "$CASE_DIR/go.log"
printf '\t%s' "$@" >> "$CASE_DIR/go.log"
printf '\n' >> "$CASE_DIR/go.log"
pkg="${!#}"
root="github.com/vtmocanu/uzi/api/${pkg#./}"
root="${root%/...}"
name=TestSweepLiveDB
case " $* " in
  *" ^TestCodexRefreshLostReplyE2E$ "*) name=TestCodexRefreshLostReplyE2E ;;
esac
if [ "$SCENARIO" = go-fail ] && [ "$pkg" = ./internal/forgesvc/... ]; then
  echo "simulated Go failure" >&2
  exit 7
fi
if [ "$pkg" = ./internal/store/... ]; then
  case "$SCENARIO" in
    no-tests) printf 'ok\t%s\t0.01s [no tests to run]\n' "$root"; exit 0 ;;
    missing-ok) echo "=== RUN   $name"; echo "--- PASS: $name (0.01s)"; exit 0 ;;
    top-skip) echo "--- SKIP: $name (0.01s)" ;;
    sub-skip) echo "    --- SKIP: $name/child (0.01s)" ;;
  esac
fi
echo "=== RUN   $name"
echo "--- PASS: $name (0.01s)"
printf 'ok\t%s\t0.01s\n' "$root"
STUB
chmod +x "$READYBIN/docker" "$READYBIN/go"

assert_case() {
  local message="$1"
  shift
  if "$@"; then
    printf 'PASS: %s (%s)\n' "$message" "$scenario"
  else
    printf 'FAIL: %s (%s)\n' "$message" "$scenario"
    fails=$((fails + 1))
  fi
}

# Explicit expected order, independent of the runner's PKGS array.
expected_pkgs=(store handler forgesvc schedsvc workersvc recovery poller)
for scenario in success optional go-fail no-tests missing-ok top-skip sub-skip; do
  case_dir="$TMP/$scenario"
  mkdir -p "$case_dir"
  optional=""
  if [ "$scenario" != success ]; then optional=1; fi
  rc=0
  PATH="$READYBIN:$PATH" CASE_DIR="$case_dir" SCENARIO="$scenario" TMPDIR="$case_dir" \
    UZI_STORE_IT_PG_WAIT_SECS=1 UZI_CODEX_REFRESH_LOSTREPLY_E2E="$optional" \
    bash "$SCRIPT" > "$case_dir/output" 2>&1 || rc=$?

  dsn="$(cut -f1 "$case_dir/go.log" | head -n 1)"
  assert_case "Go receives a nonempty throwaway DSN" test -n "$dsn"
  case "$dsn" in
    postgres://uzi:*@127.0.0.1:*/uzi?sslmode=disable) ;;
    *) printf 'FAIL: unexpected DSN (%s)\n' "$scenario"; fails=$((fails + 1)) ;;
  esac
  : > "$case_dir/expected"
  for pkg in "${expected_pkgs[@]}"; do
    limit=10m
    if [ "$pkg" = workersvc ]; then limit=15m; fi
    printf '%s\ttest\t-buildvcs=false\t-count=1\t-v\t-race\t-p\t1\t-timeout\t%s\t-run\tLiveDB$\t./internal/%s/...\n' \
      "$dsn" "$limit" "$pkg" >> "$case_dir/expected"
    if [ "$scenario" = go-fail ] && [ "$pkg" = forgesvc ]; then break; fi
  done
  if [ "$scenario" = optional ]; then
    # The optional leg retains its own selector and Go's default timeout policy.
    printf '%s\ttest\t-buildvcs=false\t-count=1\t-v\t-race\t-p\t1\t-run\t^TestCodexRefreshLostReplyE2E$\t./internal/handler/...\n' \
      "$dsn" >> "$case_dir/expected"
  fi
  assert_case "ordered calls, exact flags, timeout reset and shared DSN" \
    cmp -s "$case_dir/expected" "$case_dir/go.log"

  rm_count="$(grep -c '^rm ' "$case_dir/docker.log" || true)"
  assert_case "cleanup runs once" test "$rm_count" -eq 1
  assert_case "cleanup targets the started container and anonymous volume" \
    awk '/^run / { for (i=1; i<NF; i++) if ($i=="--name") name=$(i+1) }
         /^rm / { if ($0 == "rm -f -v " name && name != "") found=1 }
         END { exit !found }' "$case_dir/docker.log"

  case "$scenario" in
    success|optional)
      assert_case "successful sweep exits zero" test "$rc" -eq 0
      assert_case "aggregate log contains all seven passes" \
        grep -qF 'LiveDB: 7 passed, 0 skipped' "$case_dir/output"
      assert_case "success banner appears" grep -qF 'Store integration tests passed.' "$case_dir/output"
      assert_case "successful log files removed" \
        test -z "$(find "$case_dir" -name '*-log.*' -print)"
      ;;
    *)
      assert_case "failed sweep exits nonzero" test "$rc" -ne 0
      assert_case "failure suppresses success banner" \
        test "$(grep -cF 'Store integration tests passed.' "$case_dir/output" || true)" -eq 0
      # Exact go.log comparison above also proves no optional call after failure.
      case "$scenario" in
        go-fail)
          assert_case "Go's nonzero exit survives tee" test "$rc" -eq 7
          assert_case "aggregate log retains early output and failing stderr" \
            grep -qF 'simulated Go failure' "$case_dir"/uzi-store-it-log.*
          assert_case "aggregate log retains prior package output" \
            grep -qF 'github.com/vtmocanu/uzi/api/internal/store' "$case_dir"/uzi-store-it-log.*
          ;;
        no-tests|missing-ok)
          assert_case "per-package guard rejects absent plain ok despite later passes" \
            grep -qF 'internal/store reported no LiveDB test run' "$case_dir/output"
          ;;
        top-skip|sub-skip)
          assert_case "aggregate guard retains and rejects an early skip at any depth" \
            grep -qF 'LiveDB: 7 passed, 1 skipped' "$case_dir/output"
          ;;
      esac
      ;;
  esac
done

if [ "$fails" -eq 0 ]; then
  printf '\nrun-store-it teardown test: all assertions passed\n'
  exit 0
fi
printf '\nrun-store-it teardown test: %s assertion(s) failed\n' "$fails" >&2
exit 1
