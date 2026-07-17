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
# real goose migrations, seeds fixtures, and runs every store integration test
# (the *LiveDB set). Isolated: unique container + published loopback port, torn
# down on exit; never touches the user's own stacks or DBs.
#
# Run standalone, or alongside ./e2e/run-e2e.sh as the full store-SQL E2E gate.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
NAME="uzi-store-it-$$"
PORT="$(( 20000 + (RANDOM % 20000) ))"
PGPASS="$(openssl rand -hex 16)"
PGIMAGE="${UZI_STORE_IT_PG_IMAGE:-postgres:17}"

say() { printf '\n\033[1m==> %s\033[0m\n' "$1"; }

cleanup() {
  local code=$?
  docker rm -f "$NAME" >/dev/null 2>&1 || true
  exit $code
}
trap cleanup EXIT

say "starting throwaway Postgres ($NAME) on 127.0.0.1:$PORT"
docker run -d --rm --name "$NAME" \
  -e POSTGRES_USER=uzi -e POSTGRES_DB=uzi -e POSTGRES_PASSWORD="$PGPASS" \
  -p "127.0.0.1:$PORT:5432" "$PGIMAGE" >/dev/null

DSN="postgres://uzi:$PGPASS@127.0.0.1:$PORT/uzi?sslmode=disable"

say "waiting for Postgres to accept connections"
for _ in $(seq 1 30); do
  docker exec "$NAME" pg_isready -U uzi -d uzi >/dev/null 2>&1 && break
  sleep 1
done
docker exec "$NAME" pg_isready -U uzi -d uzi >/dev/null 2>&1 \
  || { echo "postgres never became ready"; exit 1; }

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
# -p 1 IS LOAD-BEARING, NOT A SPEED KNOB — do not drop it to parallelize the run.
# go test runs PACKAGE binaries concurrently by default, and these two packages
# share one database (there is exactly one throwaway Postgres above). Concurrently:
# both call store.Migrate, and goose races itself into "relation already exists";
# worse, the handler suite's resetUsers() TRUNCATEs users mid-flight under the store
# suite's fixtures, failing tests that have nothing to do with the change being
# made. Both were observed the first time this line swept two packages. -p 1
# serializes the binaries; tests within a package are already sequential.
UZI_TEST_DATABASE_URL="$DSN" go test -buildvcs=false -count=1 -v -race -p 1 \
  -run 'LiveDB$' ./internal/store/... ./internal/handler/...

printf '\n\033[32mStore integration tests passed.\033[0m\n'
