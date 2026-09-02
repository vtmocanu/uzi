# shellcheck shell=bash
# phase:    restart-agent
# title:    PRD #966: restart the agent worker before schedule phases 60-62
# critical: no
# lane:     gitlab
# executor: any
# requires: -
# provides: -
# handoff:  -
# mutates:  compose:agent(restarted)
# restores: -
# ---------------------------------------------------------------------------
# Phase 50 `compose stop`ped the agent (its binding assertions need the live worker
# out of the way) and phase 51 depends on it staying down. The schedule phases 60-62
# `wait_status … completed` and so need the live worker back. Restart it here — after
# 51, before 60 — so those runs have a claimant again.
say "restart the agent worker (stopped by 50; 51 needed it down) before the schedule phases 60-62"
"${COMPOSE[@]}" up -d --wait agent
wait_worker_online
pass "agent worker back online for the schedule phases"
