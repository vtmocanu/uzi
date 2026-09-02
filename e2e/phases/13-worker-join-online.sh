# shellcheck shell=bash
# phase:    worker-join-online
# title:    issue a worker join token and bring the worker online
# critical: yes
# lane:     gitlab
# executor: any
# requires: -
# provides: WTOKEN UZI_WORKER_TOKEN
# handoff:  -
# mutates:  -
# restores: -
# --- issue the worker join token + bring the worker online -------------------
say "issue a worker join token and bring the worker online"
WTOKEN="$(apipost /api/workers '{"name":"e2e-worker"}' | jq -r '.token')"
[ -n "$WTOKEN" ] && [ "$WTOKEN" != null ] || fail "no worker token minted"
# Hand the minted token to the base `worker_token` Docker secret (env source). A shell
# export overrides the --env-file placeholder for THIS phase's `up` below. To reach a
# LATER phase's `up`/recreate (15 restart, 45 force-recreate, 59 restart), the export
# must ALSO be a declared provide: under PRD #966's per-phase subshells a bare export
# dies with this subshell, but a `provides:` var is round-tripped to the top-level shell
# (declare -p keeps the -x, so it re-exports) and inherited by every later phase. Hence
# `provides: … UZI_WORKER_TOKEN` above, and `requires: UZI_WORKER_TOKEN` on the consumers.
# The entrypoint hardens /run/secrets/worker_token to 0400 worker on each start.
export UZI_WORKER_TOKEN="$WTOKEN"
"${COMPOSE[@]}" up -d --wait agent
wait_worker_online
pass "worker registered and is online"

