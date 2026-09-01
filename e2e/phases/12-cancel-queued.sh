# shellcheck shell=bash
# phase:    cancel-queued
# title:    cancel path: a queued run is cancelled server-side (no live poller)
# critical: no
# lane:     gitlab
# executor: any
# requires: -
# provides: -
# handoff:  -
# mutates:  -
# restores: -
# --- cancel path (server-side, before any worker is online) ------------------
say "cancel path: a queued run is cancelled server-side (no live poller)"
IID_C="$(apipost "/api/repos/$REPO_ID/issues" \
  '{"title":"E2E cancel","description":"cancel me — see prds/4-agent-runtime-workers.md"}' | jq -r '.card.iid')"
RUN_C="$(create_run "$REPO_ID" "$IID_C")" || fail "cancel-path run-create failed (non-transient; see stderr)"
# NOT a race, despite looking exactly like the one PRD #95 had to fix with a vault lock:
# no worker EXISTS yet at this point in the suite (the join token is minted and the agent
# started immediately below), so nothing can claim this run and `queued` is stable by
# construction. Left as a bare read deliberately — do not "harden" it (PRD #97 M9 swept
# the suite for this class; this is the one instance that is already safe).
[ "$(apiget "/api/runs/$RUN_C" | jq -r '.run.status')" = queued ] || fail "cancel-path run should start queued"
SS="$(apipost "/api/runs/$RUN_C/inputs" '{"kind":"cancel","body":""}' | jq -r '.server_side')"
[ "$SS" = true ] || fail "cancel of a queued run should be applied server-side (got server_side=$SS)"
[ "$(apiget "/api/runs/$RUN_C" | jq -r '.run.status')" = cancelled ] || fail "queued run did not transition to cancelled"
pass "queued run transitioned to cancelled server-side"

