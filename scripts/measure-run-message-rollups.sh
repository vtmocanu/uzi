#!/usr/bin/env bash
# M0 measurement for issue #2755: can a TERMINAL run's run_messages rows be rolled up into
# compressed chunks, losslessly and without a read regression? This script starts a THROWAWAY
# Postgres, points the api/cmd/rollupbench harness at it, and leaves results.json,
# settings.json and summary.md in OUT_DIR. The harness creates and drops its own databases on
# that server; nothing here ever touches a database you run yourself.
#
#   scripts/measure-run-message-rollups.sh [--scale tiny|full] [--corpus-ndjson DIR] OUT_DIR
#
#   --scale tiny   a synthetic corpus that finishes in a few minutes (the default, a smoke run)
#   --scale full   about 200 synthetic runs and a few hundred MB, budgeted for roughly 90
#                  minutes on 4 CPUs
#   --corpus-ndjson DIR
#                  measure a real export instead: one file per run in the
#                  `uzi run logs <run> --json` line format. THE FILES HOLD REAL PAYLOADS.
#                  Keep them outside the repository, and never commit them.
#
# Safety:
#   * The container is named cdr-rollup-m0-<pid>, listens on 127.0.0.1 only, uses a random
#     password and port, and is removed by that exact name when the script exits. No other
#     container is created, inspected or removed.
#   * results.json carries no DSN, no run id and no payload content: run ids are ordinals and
#     the harness scrubs uuid- and DSN-shaped text before writing.
#   * The backfill pauses UZI_M0_BACKFILL_SLEEP (default 50ms) between runs, as a throttled
#     production backfill would.
#   * Postgres runs with its stock settings (the harness tunes nothing); SHOW ALL is recorded
#     in settings.json.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
NAME="cdr-rollup-m0-$$"
PORT="$(( 20000 + (RANDOM % 20000) ))"
PGPASS="$(openssl rand -hex 16)"
PGIMAGE="${UZI_ROLLUP_M0_PG_IMAGE:-postgres:17}"
PGWAIT="${UZI_ROLLUP_M0_PG_WAIT_SECS:-120}"
# Pause between runs during the backfill. The live-reader bar (C5) is judged on a throttled
# backfill, so the default is not zero; the value is recorded in results.json meta.
SLEEP="${UZI_M0_BACKFILL_SLEEP:-50ms}"

usage() {
  echo "usage: $0 [--scale tiny|full] [--corpus-ndjson DIR] OUT_DIR" >&2
  exit 2
}

SCALE=tiny
NDJSON=""
OUT=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    --scale) [ "$#" -ge 2 ] || usage; SCALE="$2"; shift 2 ;;
    --corpus-ndjson) [ "$#" -ge 2 ] || usage; NDJSON="$2"; shift 2 ;;
    -h | --help) usage ;;
    -*) usage ;;
    *) [ -z "$OUT" ] || usage; OUT="$1"; shift ;;
  esac
done
[ -n "$OUT" ] || usage
case "$SCALE" in tiny | full) ;; *) usage ;; esac
if [ -n "$NDJSON" ] && [ ! -d "$NDJSON" ]; then
  echo "measure-run-message-rollups: --corpus-ndjson is not a directory" >&2
  exit 2
fi

mkdir -p "$OUT"
OUT="$(cd "$OUT" && pwd)"

say() { printf '\n==> %s\n' "$1" >&2; }

cleanup() {
  local code=$?
  docker rm -f -v "$NAME" >/dev/null 2>&1 || true
  exit "$code"
}
trap cleanup EXIT

# Pulling is best effort: an offline host with the image already cached must still run.
if ! docker image inspect "$PGIMAGE" >/dev/null 2>&1; then
  say "pulling $PGIMAGE"
  docker pull "$PGIMAGE" >&2 || {
    echo "measure-run-message-rollups: could not pull $PGIMAGE and it is not cached" >&2
    exit 1
  }
fi

say "starting throwaway Postgres ($NAME) on 127.0.0.1:$PORT"
docker run -d --rm --name "$NAME" \
  -e POSTGRES_USER=uzi -e POSTGRES_DB=uzi -e POSTGRES_PASSWORD="$PGPASS" \
  -p "127.0.0.1:$PORT:5432" "$PGIMAGE" >/dev/null

# -h 127.0.0.1 matters: initdb's temporary server listens on the socket only, so a socket
# probe can pass on it and then fail in its shutdown. TCP is up only on the final server.
for _ in $(seq 1 "$PGWAIT"); do
  docker exec "$NAME" pg_isready -h 127.0.0.1 -U uzi -d uzi >/dev/null 2>&1 && break
  sleep 1
done
if ! docker exec "$NAME" pg_isready -h 127.0.0.1 -U uzi -d uzi >/dev/null 2>&1; then
  echo "measure-run-message-rollups: Postgres never became ready within ${PGWAIT}s" >&2
  exit 1
fi

DSN="postgres://uzi:$PGPASS@127.0.0.1:$PORT/uzi?sslmode=disable"

ARGS=(--dsn "$DSN" --i-own-this-db --out "$OUT" --scale "$SCALE" --backfill-sleep "$SLEEP" --text-root "$ROOT")
if [ -n "$NDJSON" ]; then
  ARGS+=(--corpus-ndjson "$NDJSON")
fi

say "running the harness (scale $SCALE); results go to $OUT"
cd "$ROOT/api"
# -buildvcs=false: a linked worktree confuses go's VCS stamping; it only drops the embedded commit hash.
GOFLAGS="${GOFLAGS:--buildvcs=false}" go run ./cmd/rollupbench "${ARGS[@]}"

say "done: $OUT/summary.md"
