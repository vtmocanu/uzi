#!/usr/bin/env bash
# Live-DB coverage for the PRD #24 MR-close watcher's candidate-selection query
# (ListMRWatchCandidates) — the SQL the fake-store unit tests cannot exercise.
#
# Spins up a THROWAWAY Postgres, points UZI_TEST_DATABASE_URL at it, and runs the
# store integration test (TestListMRWatchCandidatesLiveDB), which applies the real
# goose migrations, seeds fixtures, and asserts candidate selection — including
# rework suppression (a non-completed latest run yields no candidate) and the
# no-superseded-MR-fallback rule (a latest completed run with NULL mr_iid yields
# none). Isolated: unique container + published loopback port, torn down on exit;
# never touches the user's own stacks or DBs.
#
# Run standalone, or alongside ./e2e/run-e2e.sh as the full PRD #24 E2E gate.
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

say "running the store candidate-selection integration test"
cd "$ROOT/api"
# -buildvcs=false: this linked worktree confuses go's VCS stamping (git run from
# $HOME); it only affects the embedded commit hash, never compile/test behavior.
UZI_TEST_DATABASE_URL="$DSN" go test -buildvcs=false -count=1 -v \
  -run TestListMRWatchCandidatesLiveDB ./internal/store/...

printf '\n\033[32mStore integration test passed.\033[0m\n'
