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
# real goose migrations, seeds fixtures, and runs every *LiveDB test in the five
# packages that carry one (store, handler, forgesvc, schedsvc, workersvc -- the
# list on the `go test` line below, mirrored in .github/workflows/ci.yml).
# Isolated: unique container + published loopback port, torn down on exit; never
# touches the user's own stacks or DBs.
#
# Run standalone, or alongside ./e2e/run-e2e.sh as the full store-SQL E2E gate.
set -euo pipefail

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
  docker rm -f "$NAME" >/dev/null 2>&1 || true
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
docker run -d --rm --name "$NAME" \
  -e POSTGRES_USER=uzi -e POSTGRES_DB=uzi -e POSTGRES_PASSWORD="$PGPASS" \
  -p "127.0.0.1:$PORT:5432" "$PGIMAGE" >/dev/null

DSN="postgres://uzi:$PGPASS@127.0.0.1:$PORT/uzi?sslmode=disable"

say "waiting for Postgres to accept connections (up to ${PGWAIT}s)"
for _ in $(seq 1 "$PGWAIT"); do
  docker exec "$NAME" pg_isready -U uzi -d uzi >/dev/null 2>&1 && break
  sleep 1
done
if ! docker exec "$NAME" pg_isready -U uzi -d uzi >/dev/null 2>&1; then
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
UZI_TEST_DATABASE_URL="$DSN" go test -buildvcs=false -count=1 -v -race -p 1 \
  -run 'LiveDB$' ./internal/store/... ./internal/handler/... \
  ./internal/forgesvc/... ./internal/schedsvc/... ./internal/workersvc/...

printf '\n\033[32mStore integration tests passed.\033[0m\n'
