# shellcheck shell=bash
# phase:    restart-agent
# title:    PRD #966: restart the agent worker before schedule phases 60-62
# critical: no
# lane:     gitlab
# executor: any
# requires: UZI_WORKER_TOKEN
# provides: SCHED_AGENT_UP
# handoff:  -
# mutates:  compose:agent(restarted)
# restores: -
# ---------------------------------------------------------------------------
# Phase 50 `compose stop`ped the agent (its binding assertions need the live worker
# out of the way) and phase 51 depends on it staying down. The schedule phases 60-62
# `wait_status … completed` and so need the live worker back. Restart it here — after
# 51, before 60 — so those runs have a claimant again.
#
# The restart is ENFORCED, not merely hoped-for: this phase provides SCHED_AGENT_UP,
# which 60-62 `requires:`. If `wait_worker_online` fails, the phase body aborts under
# `set -euo pipefail` before the assignment below, so the token is never provided and
# 60-62 FAIL fast on requires-validation (naming restart-agent) instead of timing out
# with runs stuck in `queued`.
say "restart the agent worker (stopped by 50; 51 needed it down) before the schedule phases 60-62"
"${COMPOSE[@]}" up -d --wait agent
wait_worker_online
pass "agent worker back online for the schedule phases"
# shellcheck disable=SC2034  # consumed cross-phase: the driver round-trips this provides var to 60-62 (declare -p), it is not read in-body
SCHED_AGENT_UP=1   # success token required by 60-62 (unset if the restart above aborted the body)
