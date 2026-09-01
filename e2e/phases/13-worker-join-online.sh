# shellcheck shell=bash
# phase:    worker-join-online
# title:    issue a worker join token and bring the worker online
# critical: yes
# lane:     gitlab
# executor: any
# requires: -
# provides: WTOKEN
# handoff:  -
# mutates:  -
# restores: -
# --- issue the worker join token + bring the worker online -------------------
say "issue a worker join token and bring the worker online"
WTOKEN="$(apipost /api/workers '{"name":"e2e-worker"}' | jq -r '.token')"
[ -n "$WTOKEN" ] && [ "$WTOKEN" != null ] || fail "no worker token minted"
# Hand the minted token to the base `worker_token` Docker secret (env source). A shell
# export overrides the --env-file placeholder and persists for every later `up` /
# recreate / restart in this run, so no per-start re-delivery is needed. The entrypoint
# hardens /run/secrets/worker_token to 0400 worker on each start.
export UZI_WORKER_TOKEN="$WTOKEN"
"${COMPOSE[@]}" up -d --wait agent
wait_worker_online
pass "worker registered and is online"

