#!/usr/bin/env bash
# Real-HTTP orchestrator for PRD #1171 milestone m5 (item 7): stand up the minimal
# REAL codex worker-route API test server (api/cmd/codexm3btestserver) on a real
# listening loopback socket, backed by a THROWAWAY, migrated Postgres, so a later
# milestone can point a REAL TypeScript WorkerClient at it and exercise
#
#   POST /api/worker/runs/{id}/codex/release
#   POST /api/worker/runs/{id}/codex/refresh
#
# over real HTTP. It is the real-socket promotion of the in-process coverage in
# api/internal/handler/worker_codex_livedb_test.go.
#
# TWO WAYS TO USE IT:
#
#   1. Sourced by a harness (the intended downstream use):
#          source e2e/codex-m3b/testserver.sh
#          codex_m3b_up            # starts PG + server, exports CODEX_M3B_* env vars
#          ...  drive the WorkerClient against "$CODEX_M3B_BASE_URL" ...
#          codex_m3b_down          # tears down the server process + PG container
#      Sourcing does NOT enable errexit or install an EXIT trap on the caller, and
#      the caller owns the up/down lifecycle.
#
#   2. Executed directly (self-contained smoke proof):
#          e2e/codex-m3b/testserver.sh
#      Brings the stack up, prints the exported env, curls each Bearer codex route to
#      prove the server boots/migrates/seeds/answers over real HTTP, then tears the
#      stack down via an EXIT trap.
#
# ISOLATION (CLAUDE.md destructive-ops rules): the throwaway Postgres is named
# OUTSIDE the uzi- namespace (cdr-codexm3b-pg-$$) and torn down by that EXACT name
# only -- never a uzi-* glob, never `docker compose down`, never `-p uzi`, never -v.
# The server process is stopped by the EXACT PID this script started.

# Exported for a downstream consumer (populated by codex_m3b_up):
#   CODEX_M3B_BASE_URL                 origin of the real server, e.g. http://127.0.0.1:PORT
#                                      (routes live under $CODEX_M3B_BASE_URL/api/worker/...)
#   CODEX_M3B_WORKER_TOKEN             Bearer join token owning both seeded runs
#   CODEX_M3B_SUB_RUN_ID               subscription-mode run id
#   CODEX_M3B_SUB_CAPABILITY           subscription run's per-claim capability
#   CODEX_M3B_SUB_CHATGPT_ACCOUNT_ID   provider-verified account id for the subscription run
#   CODEX_M3B_SUB_GENERATION           committed generation the worker starts observing at
#   CODEX_M3B_API_RUN_ID               api_key-mode run id
#   CODEX_M3B_API_CAPABILITY           api_key run's per-claim capability

# _codex_m3b_say prints a framed status line to stderr (stdout stays reserved for the
# server's machine-readable contract in the sourced case).
_codex_m3b_say() {
  printf '\n==> %s\n' "$1" >&2
}

# codex_m3b_up starts the throwaway Postgres, migrates+seeds via the Go test server,
# captures the server's single JSON contract line and exports it as CODEX_M3B_* vars.
# Returns non-zero (and cleans up what it started) on any failure.
codex_m3b_up() {
  CDR_M3B_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
  CDR_M3B_ROOT="$(cd "$CDR_M3B_DIR/../.." && pwd)"
  CDR_M3B_PG_NAME="cdr-codexm3b-pg-$$"
  CDR_M3B_PG_PORT="$(( 20000 + (RANDOM % 20000) ))"
  CDR_M3B_PG_IMAGE="${CDR_M3B_PG_IMAGE:-postgres:17}"
  CDR_M3B_PG_WAIT="${CDR_M3B_PG_WAIT_SECS:-120}"

  local pgpass
  pgpass="$(openssl rand -hex 16)" || return 1

  # Make sure the image is present before `docker run`, so an image-fetch fault is a
  # loud infrastructure failure rather than a raw daemon error at container start.
  if ! docker image inspect "$CDR_M3B_PG_IMAGE" >/dev/null 2>&1; then
    _codex_m3b_say "pulling $CDR_M3B_PG_IMAGE (not present locally)"
    if ! docker pull "$CDR_M3B_PG_IMAGE"; then
      _codex_m3b_say "INFRASTRUCTURE FAILURE: could not pull $CDR_M3B_PG_IMAGE"
      return 1
    fi
  fi

  _codex_m3b_say "starting throwaway Postgres ($CDR_M3B_PG_NAME) on 127.0.0.1:$CDR_M3B_PG_PORT"
  if ! docker run -d --rm --name "$CDR_M3B_PG_NAME" \
    -e POSTGRES_USER=uzi -e POSTGRES_DB=uzi -e POSTGRES_PASSWORD="$pgpass" \
    -p "127.0.0.1:$CDR_M3B_PG_PORT:5432" "$CDR_M3B_PG_IMAGE" >/dev/null; then
    _codex_m3b_say "INFRASTRUCTURE FAILURE: could not start throwaway Postgres"
    return 1
  fi

  local dsn="postgres://uzi:${pgpass}@127.0.0.1:${CDR_M3B_PG_PORT}/uzi?sslmode=disable"

  _codex_m3b_say "waiting for Postgres to accept connections (up to ${CDR_M3B_PG_WAIT}s)"
  local i
  for (( i = 0; i < CDR_M3B_PG_WAIT; i++ )); do
    if docker exec "$CDR_M3B_PG_NAME" pg_isready -U uzi -d uzi >/dev/null 2>&1; then
      break
    fi
    sleep 1
  done
  if ! docker exec "$CDR_M3B_PG_NAME" pg_isready -U uzi -d uzi >/dev/null 2>&1; then
    _codex_m3b_say "INFRASTRUCTURE FAILURE: throwaway Postgres never became ready within ${CDR_M3B_PG_WAIT}s"
    codex_m3b_down
    return 1
  fi

  # Build the server once (a linked worktree confuses go's VCS stamping, so
  # -buildvcs=false; it only affects the embedded commit hash, never behaviour).
  CDR_M3B_BIN="$(mktemp -t codexm3btestserver.XXXXXX)" || { codex_m3b_down; return 1; }
  _codex_m3b_say "building api/cmd/codexm3btestserver"
  if ! ( cd "$CDR_M3B_ROOT/api" && GOFLAGS=-buildvcs=false go build -o "$CDR_M3B_BIN" ./cmd/codexm3btestserver ); then
    _codex_m3b_say "FAILURE: could not build the test server"
    codex_m3b_down
    return 1
  fi

  CDR_M3B_OUT="$(mktemp -t codexm3btestserver-out.XXXXXX)" || { codex_m3b_down; return 1; }
  CDR_M3B_ERR="$(mktemp -t codexm3btestserver-err.XXXXXX)" || { codex_m3b_down; return 1; }

  _codex_m3b_say "starting the test server (migrate + seed + listen)"
  UZI_TEST_DATABASE_URL="$dsn" "$CDR_M3B_BIN" >"$CDR_M3B_OUT" 2>"$CDR_M3B_ERR" &
  CDR_M3B_SERVER_PID=$!

  # The server prints exactly one JSON line (unbuffered) then blocks. Wait for that
  # line, or for the process to die early.
  local contract=""
  local j
  for (( j = 0; j < 100; j++ )); do
    if [ -s "$CDR_M3B_OUT" ]; then
      IFS= read -r contract <"$CDR_M3B_OUT" || true
      if [ -n "$contract" ]; then
        break
      fi
    fi
    if ! kill -0 "$CDR_M3B_SERVER_PID" 2>/dev/null; then
      _codex_m3b_say "FAILURE: test server exited before printing its contract"
      cat "$CDR_M3B_ERR" >&2 || true
      codex_m3b_down
      return 1
    fi
    sleep 0.1
  done
  if [ -z "$contract" ]; then
    _codex_m3b_say "FAILURE: test server did not print its contract in time"
    cat "$CDR_M3B_ERR" >&2 || true
    codex_m3b_down
    return 1
  fi

  export CODEX_M3B_BASE_URL CODEX_M3B_WORKER_TOKEN
  export CODEX_M3B_SUB_RUN_ID CODEX_M3B_SUB_CAPABILITY CODEX_M3B_SUB_CHATGPT_ACCOUNT_ID CODEX_M3B_SUB_GENERATION
  export CODEX_M3B_API_RUN_ID CODEX_M3B_API_CAPABILITY
  CODEX_M3B_BASE_URL="$(printf '%s' "$contract" | jq -r '.base_url')"
  CODEX_M3B_WORKER_TOKEN="$(printf '%s' "$contract" | jq -r '.worker_token')"
  CODEX_M3B_SUB_RUN_ID="$(printf '%s' "$contract" | jq -r '.subscription.run_id')"
  CODEX_M3B_SUB_CAPABILITY="$(printf '%s' "$contract" | jq -r '.subscription.capability')"
  CODEX_M3B_SUB_CHATGPT_ACCOUNT_ID="$(printf '%s' "$contract" | jq -r '.subscription.chatgpt_account_id')"
  CODEX_M3B_SUB_GENERATION="$(printf '%s' "$contract" | jq -r '.subscription.generation')"
  CODEX_M3B_API_RUN_ID="$(printf '%s' "$contract" | jq -r '.api_key.run_id')"
  CODEX_M3B_API_CAPABILITY="$(printf '%s' "$contract" | jq -r '.api_key.capability')"

  if [ -z "$CODEX_M3B_BASE_URL" ] || [ "$CODEX_M3B_BASE_URL" = "null" ]; then
    _codex_m3b_say "FAILURE: could not parse base_url from server contract"
    codex_m3b_down
    return 1
  fi

  _codex_m3b_say "server up at $CODEX_M3B_BASE_URL (pid $CDR_M3B_SERVER_PID)"
  return 0
}

# codex_m3b_down stops the test server by the EXACT PID this script started and removes
# the throwaway Postgres by its EXACT name. Idempotent and safe to call on any partial
# start. Never uses a uzi-* glob, `docker compose down`, -p uzi or -v.
codex_m3b_down() {
  if [ -n "${CDR_M3B_SERVER_PID:-}" ] && kill -0 "$CDR_M3B_SERVER_PID" 2>/dev/null; then
    kill "$CDR_M3B_SERVER_PID" 2>/dev/null || true
    wait "$CDR_M3B_SERVER_PID" 2>/dev/null || true
  fi
  CDR_M3B_SERVER_PID=""
  if [ -n "${CDR_M3B_PG_NAME:-}" ]; then
    docker rm -f "$CDR_M3B_PG_NAME" >/dev/null 2>&1 || true
  fi
  for f in "${CDR_M3B_BIN:-}" "${CDR_M3B_OUT:-}" "${CDR_M3B_ERR:-}"; do
    [ -n "$f" ] && rm -f "$f" 2>/dev/null || true
  done
}

# _codex_m3b_post POSTs a JSON body to a codex route and prints "<http_code> <body>".
_codex_m3b_post() {
  local path="$1" body="$2"
  curl -sS -o /dev/stdout -w ' %{http_code}' \
    -X POST \
    -H "Authorization: Bearer ${CODEX_M3B_WORKER_TOKEN}" \
    -H "Content-Type: application/json" \
    -d "$body" \
    "${CODEX_M3B_BASE_URL}${path}"
}

# _codex_m3b_smoke drives each Bearer codex route once and asserts a 200, proving the
# real server boots, migrates, seeds and answers over real HTTP. Direct-exec only.
_codex_m3b_smoke() {
  local resp code

  _codex_m3b_say "smoke: POST subscription release"
  resp="$(_codex_m3b_post "/api/worker/runs/${CODEX_M3B_SUB_RUN_ID}/codex/release" \
    "{\"capability\":\"${CODEX_M3B_SUB_CAPABILITY}\"}")"
  code="${resp##* }"
  printf '  -> %s\n' "$resp" >&2
  [ "$code" = "200" ] || { _codex_m3b_say "FAIL: subscription release code=$code"; return 1; }

  _codex_m3b_say "smoke: POST subscription refresh"
  resp="$(_codex_m3b_post "/api/worker/runs/${CODEX_M3B_SUB_RUN_ID}/codex/refresh" \
    "{\"capability\":\"${CODEX_M3B_SUB_CAPABILITY}\",\"operation_id\":\"$(cat /proc/sys/kernel/random/uuid)\",\"observed_generation\":${CODEX_M3B_SUB_GENERATION}}")"
  code="${resp##* }"
  printf '  -> %s\n' "$resp" >&2
  [ "$code" = "200" ] || { _codex_m3b_say "FAIL: subscription refresh code=$code"; return 1; }

  _codex_m3b_say "smoke: POST api_key release"
  resp="$(_codex_m3b_post "/api/worker/runs/${CODEX_M3B_API_RUN_ID}/codex/release" \
    "{\"capability\":\"${CODEX_M3B_API_CAPABILITY}\"}")"
  code="${resp##* }"
  printf '  -> %s\n' "$resp" >&2
  [ "$code" = "200" ] || { _codex_m3b_say "FAIL: api_key release code=$code"; return 1; }

  _codex_m3b_say "smoke: all codex Bearer routes answered 200 over real HTTP"
  return 0
}

# Direct-exec entrypoint: full self-contained smoke with EXIT-trap teardown. When this
# file is sourced instead, none of this runs and the caller owns the lifecycle.
_codex_m3b_main() {
  set -euo pipefail
  trap codex_m3b_down EXIT
  codex_m3b_up
  # Echo the exported contract for a human reading the smoke run.
  {
    printf 'CODEX_M3B_BASE_URL=%s\n' "$CODEX_M3B_BASE_URL"
    printf 'CODEX_M3B_WORKER_TOKEN=%s\n' "$CODEX_M3B_WORKER_TOKEN"
    printf 'CODEX_M3B_SUB_RUN_ID=%s\n' "$CODEX_M3B_SUB_RUN_ID"
    printf 'CODEX_M3B_SUB_CAPABILITY=%s\n' "$CODEX_M3B_SUB_CAPABILITY"
    printf 'CODEX_M3B_SUB_CHATGPT_ACCOUNT_ID=%s\n' "$CODEX_M3B_SUB_CHATGPT_ACCOUNT_ID"
    printf 'CODEX_M3B_SUB_GENERATION=%s\n' "$CODEX_M3B_SUB_GENERATION"
    printf 'CODEX_M3B_API_RUN_ID=%s\n' "$CODEX_M3B_API_RUN_ID"
    printf 'CODEX_M3B_API_CAPABILITY=%s\n' "$CODEX_M3B_API_CAPABILITY"
  } >&2
  _codex_m3b_smoke
  _codex_m3b_say "smoke passed"
}

# Run the smoke only when executed directly; stay a pure library when sourced.
if ! (return 0 2>/dev/null); then
  _codex_m3b_main "$@"
fi
