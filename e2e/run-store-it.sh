#!/usr/bin/env bash
# Live-DB coverage for the behavior the fake-store unit tests cannot exercise:
# the SQL itself, and the transaction semantics around it.
#
#   * the PRD #24 MR-close watcher's candidate selection (ListMRWatchCandidates)
#   * the PRD #6 pipeline-status cache (ListWatchedRunRefsForRepo window/cap/
#     DISTINCT-ON, the per-card most-recent-run join, default-branch projection,
#     upsert-latest-per-ref, reconcile eviction)
#   * the PRD #58 hosted-worker quota: a guarded insert under a per-user advisory
#     lock. A fake store has no snapshot isolation and no locks, so it cannot
#     exhibit the TOCTOU this gate exists to catch — it would go green against a
#     quota that lets four workers through here.
#
# Spins up a THROWAWAY Postgres, points UZI_TEST_DATABASE_URL at it, applies the
# real goose migrations, seeds fixtures, and runs every *LiveDB test in the seven
# packages that carry one (store, handler, forgesvc, schedsvc, workersvc, recovery, poller -- the
# list on the `go test` line below, mirrored in .github/workflows/ci.yml).
# Isolated: unique container + published loopback port, torn down on exit; never
# touches the user's own stacks or DBs.
#
# Run standalone, or alongside ./e2e/run-e2e.sh as the full store-SQL E2E gate.
set -euo pipefail
# The Go suites and agent fixtures below create host-side repos: they must not read
# host git config, where a global core.fsmonitor=true leaves a detached daemon per repo.
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
NAME="uzi-store-it-$$"
PORT="$(( 20000 + (RANDOM % 20000) ))"
PGPASS="$(openssl rand -hex 16)"
PGIMAGE="${UZI_STORE_IT_PG_IMAGE:-postgres:17}"
# How long to wait for the throwaway Postgres to accept connections. Widened from
# a hard-coded 30 (issue #171): on a daemon busy with mutation containers, 30s
# timed out and produced a RUN=0 PASS=0 FAIL=0 log that reads exactly like the
# false-green .claude/rules/go.md documents. Env-overridable for a very busy host.
PGWAIT="${UZI_STORE_IT_PG_WAIT_SECS:-120}"

say() { printf '\n\033[1m==> %s\033[0m\n' "$1"; }

cleanup() {
  local code=$?
  docker rm -f -v "$NAME" >/dev/null 2>&1 || true
  exit $code
}
trap cleanup EXIT

# Make sure the image is present BEFORE we start the container, so an image-fetch
# fault gets the same loud, framed INFRASTRUCTURE-FAILURE treatment as a readiness
# timeout (issue #171's contract) instead of dying at `docker run` with a raw
# daemon error — both are "no tests ran", not a test result. `docker image inspect`
# is a local no-op when the image is already cached (no registry call), so an
# offline host with the image present is unaffected; the pull only happens when the
# image is genuinely absent, and its progress is now visible rather than a silent
# stall behind the "starting throwaway Postgres" line.
if ! docker image inspect "$PGIMAGE" >/dev/null 2>&1; then
  say "pulling $PGIMAGE (not present locally)"
  if ! docker pull "$PGIMAGE"; then
    printf '\n\033[1;31m==> INFRASTRUCTURE FAILURE: could not pull %s. NO TESTS RAN.\033[0m\n' "$PGIMAGE" >&2
    printf '\033[1;31m==> This is NOT a test result. The throwaway Postgres image is unavailable\033[0m\n' >&2
    printf '\033[1;31m==> (registry unreachable, or an offline host with the image not cached).\033[0m\n' >&2
    exit 1
  fi
fi

say "starting throwaway Postgres ($NAME) on 127.0.0.1:$PORT"
# Give PostgreSQL dynamic shared memory headroom for concurrent readers.
docker run -d --rm --shm-size=1g --name "$NAME" \
  -e POSTGRES_USER=uzi -e POSTGRES_DB=uzi -e POSTGRES_PASSWORD="$PGPASS" \
  --mount "type=bind,source=$ROOT/e2e/test-database-marker.sql,target=/docker-entrypoint-initdb.d/10-uzi-test-database-marker.sql,readonly" \
  -p "127.0.0.1:$PORT:5432" "$PGIMAGE" >/dev/null

DSN="postgres://uzi:$PGPASS@127.0.0.1:$PORT/uzi?sslmode=disable"

say "waiting for Postgres to accept connections (up to ${PGWAIT}s)"
# -h 127.0.0.1 IS LOAD-BEARING (issue #2304): the postgres image's initdb runs a
# temporary server on the Unix socket only, so a socket probe can pass on it and
# the recheck below then lands in its shutdown, a false "never became ready" after
# a few seconds. TCP is up only on the final server.
for _ in $(seq 1 "$PGWAIT"); do
  docker exec "$NAME" pg_isready -h 127.0.0.1 -U uzi -d uzi >/dev/null 2>&1 && break
  sleep 1
done
if ! docker exec "$NAME" pg_isready -h 127.0.0.1 -U uzi -d uzi >/dev/null 2>&1; then
  # LOUD, unmistakable banner: a readiness timeout is an INFRASTRUCTURE fault,
  # not a test result. Without it the run ends with no package times and, to
  # whoever tallies the log, RUN=0 PASS=0 FAIL=0 — indistinguishable from the
  # false-green .claude/rules/go.md documents (unset UZI_TEST_DATABASE_URL, or a
  # -v-less grep). Keep this wording in sync with that passage.
  printf '\n\033[1;31m==> INFRASTRUCTURE FAILURE: throwaway Postgres never became ready within %ss. NO TESTS RAN.\033[0m\n' "$PGWAIT" >&2
  printf '\033[1;31m==> This is NOT a test result. A RUN=0 PASS=0 FAIL=0 tally here means the database never came up\033[0m\n' >&2
  printf '\033[1;31m==> (usually daemon contention), NOT that the suite passed or was skipped. Retry, or raise\033[0m\n' >&2
  printf '\033[1;31m==> UZI_STORE_IT_PG_WAIT_SECS (currently %s) to give a busy host more headroom.\033[0m\n' "$PGWAIT" >&2
  exit 1
fi

say "running the live-DB integration tests"
cd "$ROOT/api"
# -buildvcs=false: this linked worktree confuses go's VCS stamping (git run from
# $HOME); it only affects the embedded commit hash, never compile/test behavior.
#
# ./internal/handler/... joined ./internal/store/... for PRD #58 M2: the hosted
# provision quota is enforced by an advisory lock plus a guarded insert ACROSS one
# transaction, which lives in the handler (Handler.pool/.q are concrete types, so
# there is no fake-store seam) and whose race a fake store could never exhibit. -run
# filters by name, so only the *LiveDB tests run here — the rest of the handler
# suite stays in `go test ./...`.
#
# ./internal/forgesvc/... ./internal/schedsvc/... ./internal/workersvc/... joined
# 2026-09-02: 17 *LiveDB tests in those three packages were swept by NOTHING -- not
# here, not by ci.yml -- while their own skip messages named this script. A *LiveDB
# test in a package this line does not list never runs anywhere, and `go test
# ./...` prints `ok` for it all the same. When a *LiveDB test lands in a new
# package, add the package HERE and in ci.yml's "LiveDB tests" step, in the same
# commit.
#
# -p 1 IS LOAD-BEARING, NOT A SPEED KNOB — do not drop it to parallelize the run.
# go test runs PACKAGE binaries concurrently by default, and these packages share
# one database (there is exactly one throwaway Postgres above). Concurrently: they
# all call store.Migrate, and goose races itself into "relation already exists";
# worse, the handler suite's resetUsers() TRUNCATEs users mid-flight under the store
# suite's fixtures, failing tests that have nothing to do with the change being
# made. Both were observed the first time this line swept two packages. -p 1
# serializes the binaries; tests within a package are already sequential.
#
# The shared database also means fixtures must not reuse literal UNIQUE values
# across packages: nothing truncates `workers` between binaries, so a worker row
# inserted with a fixed token_hash by one package's test is still there when the
# next package's test inserts the same literal (measured 2026-09-02: handler's
# denied_cli_dismiss and workersvc's task_review both used `[]byte{0x2}`, and the
# second one failed on the UNIQUE constraint). Derive token hashes from a fresh
# uuid, the way handler/hosted_provision_livedb_test.go documents.
#
# PKGS is the ONE list; ci.yml's "LiveDB tests" step carries the same seven, and the
# per-package check below is what makes a stale list visible: a listed package that
# contributes zero *LiveDB tests prints `ok <pkg> 0.01s [no tests to run]`, which the
# aggregate exit code and the aggregate PASS count both read as green.
PKGS=(
  ./internal/store/...
  ./internal/handler/...
  ./internal/forgesvc/...
  ./internal/schedsvc/...
  ./internal/workersvc/...
  ./internal/recovery/...
  ./internal/poller/...
)
# EXPLICIT XXXXXX template, NOT `mktemp -t <name>`: GNU coreutils rejects a `-t`
# template with too few X's (it needs >=3 consecutive; this one had none) with "too
# few X's in template", while BSD/macOS accepts it, so that form fails only on the
# Linux worker/CI, never a contributor's mac. Matches the idiom in
# scripts/scan-secrets.sh (commit f0e3c438) and is enforced by check:mktemp-portability.
LOG="$(mktemp "${TMPDIR:-/tmp}/uzi-store-it-log.XXXXXX")" || {
  echo "run-store-it: mktemp failed" >&2
  exit 1
}
# Keep local LiveDB deadlines bounded per package. With artifact admission coverage,
# workersvc takes nearly 18m in isolation and integrated sweeps exceeded 20m (#2866),
# so give it 30m of headroom; handler retains 20m. This changes no test selection or
# race/skip checks and is not a speedup. Reset other packages to 10m each iteration.
for p in "${PKGS[@]}"; do
  timeout=10m
  if [ "$p" = "./internal/handler/..." ]; then
    timeout=20m
  elif [ "$p" = "./internal/workersvc/..." ]; then
    timeout=30m
  fi
  UZI_TEST_DATABASE_URL="$DSN" go test -buildvcs=false -count=1 -v -race -p 1 \
    -timeout "$timeout" -run 'LiveDB$' "$p" 2>&1 | tee -a "$LOG"
done

# Gate on the gate, mirrored from ci.yml. (1) Aggregate: the suite must have RUN
# something and SKIPPED nothing -- with the DSN unset or Postgres unreachable every test
# self-skips and `go test` still prints ok. (2) Per package: each listed package must
# have produced an `ok <pkg> <N>s` line with NO `[no tests to run]` suffix, so a package
# whose LiveDB tests were renamed, moved, or never existed cannot hide behind the others.
# The aggregate check (ran > 0, no skip at ANY indentation) is shared with ci.yml.
if ! "$ROOT/scripts/livedb-skip-guard.sh" "$LOG"; then
  printf '\n\033[1;31m==> FAIL: the live-DB tests did not actually run against Postgres.\033[0m\n' >&2
  exit 1
fi
for p in "${PKGS[@]}"; do
  root="github.com/vtmocanu/uzi/api/${p#./}"
  root="${root%/...}"
  if ! grep -qE "^ok[[:space:]]+${root}[[:space:]]+[0-9.]+s$" "$LOG"; then
    printf '\n\033[1;31m==> FAIL: %s reported no LiveDB test run (no plain "ok <pkg> <N>s" line; a "[no tests to run]" suffix means the package is listed but contributes nothing).\033[0m\n' "$root" >&2
    exit 1
  fi
done
rm -f "$LOG"

# This opt-in leg shares the live throwaway DSN but is outside the LiveDB name
# filter. Existing Go-only CI lanes neither require Node nor prove this composition.
if [ -n "${UZI_CODEX_REFRESH_LOSTREPLY_E2E:-}" ]; then
  E2E_LOG="$(mktemp "${TMPDIR:-/tmp}/codex-refresh-lostreply-log.XXXXXX")"
  UZI_TEST_DATABASE_URL="$DSN" go test -buildvcs=false -count=1 -v -race -p 1 \
    -run '^TestCodexRefreshLostReplyE2E$' ./internal/handler/... 2>&1 | tee "$E2E_LOG"
  # A named RUN and PASS are mandatory; inspect all indentation levels for skips.
  if ! grep -q '^=== RUN   TestCodexRefreshLostReplyE2E$' "$E2E_LOG" ||
    ! grep -q '^--- PASS: TestCodexRefreshLostReplyE2E ' "$E2E_LOG" ||
    grep -q -- '--- SKIP:' "$E2E_LOG"; then
    echo "combined Codex proof did not run and pass without skips: $E2E_LOG" >&2
    exit 1
  fi
  rm -f "$E2E_LOG"
fi

if [ "${UZI_HEARTBEAT_PROTOCOL_REFRESH_E2E:-}" = "1" ]; then
  mkdir -p "$ROOT/.uzi/scratch"
  E2E_LOG="$(mktemp "$ROOT/.uzi/scratch/heartbeat-protocol-refresh-log.XXXXXX")"
  UZI_TEST_DATABASE_URL="$DSN" go test -buildvcs=false -count=1 -v -race -p 1 \
    -run '^TestHeartbeatProtocolRefreshE2E$' ./internal/handler/... 2>&1 | tee "$E2E_LOG"
  if ! grep -q '^=== RUN   TestHeartbeatProtocolRefreshE2E$' "$E2E_LOG" ||
    ! grep -q '^--- PASS: TestHeartbeatProtocolRefreshE2E ' "$E2E_LOG" ||
    grep -q -- '--- SKIP:' "$E2E_LOG"; then
    echo "heartbeat protocol refresh proof did not run and pass without skips: $E2E_LOG" >&2
    exit 1
  fi
  rm -f "$E2E_LOG"
fi

# M1 composition proof: real GitHub collection, sealed storage and both worker
# reader adapters, including an actually reclaimed generation. Keep this log.
if [ "${UZI_CI_ARTIFACTS_E2E:-}" = "1" ]; then
  mkdir -p "$ROOT/.uzi/scratch"
  E2E_LOG="$(mktemp "$ROOT/.uzi/scratch/ci-artifacts-repair-log.XXXXXX")"
  printf 'CI artifact repair proof: revision=%s log=%s\n' "$(git -C "$ROOT" rev-parse HEAD)" "$E2E_LOG"
  UZI_TEST_DATABASE_URL="$DSN" go test -buildvcs=false -count=1 -v -race -p 1 \
    -run '^TestCIArtifactsRepairE2E$' ./internal/handler/... 2>&1 | tee "$E2E_LOG"
  if grep -q -- '--- SKIP:' "$E2E_LOG"; then
    echo "CI artifact repair proof skipped: $E2E_LOG" >&2
    exit 1
  fi
  for proof_case in TestCIArtifactsRepairE2E \
    TestCIArtifactsRepairE2E/claude/fresh TestCIArtifactsRepairE2E/claude/resume \
    TestCIArtifactsRepairE2E/codex/fresh TestCIArtifactsRepairE2E/codex/resume; do
    if ! grep -Fxq "=== RUN   $proof_case" "$E2E_LOG" ||
      ! grep -Eq "^[[:space:]]*--- PASS: $proof_case \\(" "$E2E_LOG"; then
      echo "CI artifact repair proof missing RUN/PASS for $proof_case: $E2E_LOG" >&2
      exit 1
    fi
  done
fi

# Explicit opt-in only: the ordinary Go-only LiveDB sweep does not prove this Node composition.
if [ "${UZI_STALE_WORKER_CANCEL_E2E:-}" = "1" ]; then
  mkdir -p "$ROOT/.uzi/scratch"
  E2E_LOG="$(mktemp "$ROOT/.uzi/scratch/gate-log.XXXXXX")"
  UZI_TEST_DATABASE_URL="$DSN" go test -buildvcs=false -count=1 -v -race -p 1 \
    -run '^TestStaleWorkerCancelE2E$' ./internal/handler/... 2>&1 | tee "$E2E_LOG"
  # Named positive controls, and zero skips at every indentation level.
  if ! grep -q '^=== RUN   TestStaleWorkerCancelE2E$' "$E2E_LOG" ||
    ! grep -q '^--- PASS: TestStaleWorkerCancelE2E ' "$E2E_LOG" ||
    grep -q -- '--- SKIP:' "$E2E_LOG"; then
    echo "stale worker cancel proof did not run and pass without skips: $E2E_LOG" >&2
    exit 1
  fi
  for fixture_case in plan-empty question-empty stale-generation ordinary-outage; do
    if ! grep -q "^=== RUN   TestStaleWorkerCancelE2E/$fixture_case$" "$E2E_LOG" ||
      ! grep -q -- "--- PASS: TestStaleWorkerCancelE2E/$fixture_case " "$E2E_LOG"; then
      echo "stale worker cancel case did not run and pass: $fixture_case ($E2E_LOG)" >&2
      exit 1
    fi
  done
  printf 'Stale worker cancel combined proof log: %s\n' "$E2E_LOG"
fi

printf '\n\033[32mStore integration tests passed.\033[0m\n'
