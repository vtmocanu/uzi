# shellcheck shell=bash
# phase:    plan-reject-verbatim
# title:    PRD #33: live plan reject with a verbatim reason -> verbatim failure_reason back through the worker
# critical: no
# lane:     gitlab
# executor: any
# requires: -
# provides: -
# handoff:  -
# mutates:  -
# restores: -
# =============================================================================
# PRD #33 — deliberate-stop signal: a live-poller plan reject carrying a VERBATIM
# reason must survive the round trip through the worker (issue #15 item 3). A worker
# is online, so the reject is poller-consumed (server_side=false) and the stub runner
# reports `failed` with the reason verbatim.
#
# PRD #97 M4: the stop_kind SQL half was dropped here. `runs.stop_kind` stamping is
# proven against a live Postgres by `api/internal/store/stop_kind_integration_test.go`
# TestCreateRunInputStopKindLiveDB — the two-query split (approve_plan/follow_up leave
# stop_kind NULL; the CreateStopVerdictInput CTE stamps 'plan_rejected'/'cancelled' in
# one statement), both the live and the server-side reject/cancel paths, and the
# out-of-domain CHECK. That test runs in CI on every MR (`test:api-store-it`), a
# stronger gate than this local-only harness. What stays is the full-wire half: the
# reject really went out through the LIVE worker (server_side=false) and the reason
# came back byte-for-byte.
say "PRD #33: live plan reject with a verbatim reason → verbatim failure_reason back through the worker"
IID_R="$(apipost "/api/repos/$REPO_ID/issues" \
  '{"title":"E2E reject","description":"reject me — see prds/4-agent-runtime-workers.md"}' | jq -r '.card.iid')"
RUN_R="$(create_run "$REPO_ID" "$IID_R")" || fail "reject-path run-create failed (non-transient; see stderr)"
[ -n "$RUN_R" ] && [ "$RUN_R" != null ] || fail "reject-path run was not created"
wait_status "$RUN_R" awaiting_approval
# A reason the OLD exact-string heuristic ("run cancelled"/"plan rejected") could
# never recognise — stop_kind must classify it regardless.
REJECT_REASON="this plan skips the migration step; redo it against the new schema"
SS_R="$(apipost "/api/runs/$RUN_R/inputs" \
  "$(jq -cn --arg r "$REJECT_REASON" '{kind:"reject_plan",body:$r}')" | jq -r '.server_side')"
[ "$SS_R" = false ] || fail "a reject against a LIVE worker must be poller-consumed, not server-side (got server_side=$SS_R)"
wait_status "$RUN_R" failed
REJ="$(apiget "/api/runs/$RUN_R")"
[ "$(echo "$REJ" | jq -r '.run.failure_reason')" = "$REJECT_REASON" ] \
  || fail "rejected run must carry the VERBATIM failure_reason (got '$(echo "$REJ" | jq -r '.run.failure_reason')')"
pass "live plan reject: status=failed, failure_reason=verbatim through the worker"

