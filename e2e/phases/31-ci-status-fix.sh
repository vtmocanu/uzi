# shellcheck shell=bash
# phase:    ci-status-fix
# title:    PRD #6: CI status sync + Fix CI + the verification stamp
# critical: no
# lane:     gitlab
# executor: any
# requires: -
# provides: -
# handoff:  -
# mutates:  -
# restores: -
# =============================================================================
# PRD #6 — CI status sync, Fix CI (plan-gated ci_fix run), and verification. The
# poller is already at ~2s (the MR-close phase sped it up), so the pipeline sync
# ticks fast; forge-fake serves the pipeline endpoints and the /_e2e/pipelines
# mutator seeds/flips a ref's status.
say "PRD #6: CI status sync + Fix CI + the verification stamp"

# 1) A red pipeline on main becomes visible on the board header within a tick.
fake_post "/_e2e/pipelines" '{"ref":"main","status":"failed","jobs":[{"name":"unit","stage":"test","status":"failed","trace":"=== RUN TestFoo\n--- FAIL: TestFoo (nil guard removed)\nFAIL\n"}]}' >/dev/null
wait_board_pipeline failed 20
pass "red main pipeline is visible on the board header within a poll interval"

# 2) Fix CI queues a plan-gated ci_fix run; a second one on the same ref is a 409.
FIXRUN="$(apipost "/api/repos/$REPO_ID/ci-fix-runs" '{"ref":"main"}' | jq -r '.run.id')"
{ [ -n "$FIXRUN" ] && [ "$FIXRUN" != null ]; } || fail "ci_fix run was not created"
[ "$(apiget "/api/runs/$FIXRUN" | jq -r '.run.kind')" = ci_fix ] || fail "run kind is not ci_fix"
DUP="$(apipost_code "/api/repos/$REPO_ID/ci-fix-runs" '{"ref":"main"}')"
[ "$DUP" = 409 ] || fail "a second active Fix CI on main should be 409, got $DUP"
pass "Fix CI queued ci_fix run $FIXRUN; a duplicate on the same ref is 409"

# 3) Plan gate → approve → the worker pushes the fix branch + opens an MR.
wait_status "$FIXRUN" awaiting_approval
[ "$(apiget "/api/runs/$FIXRUN" | jq -r '.run.plan_md // empty')" != "" ] || fail "ci_fix run carried no plan"
apipost "/api/runs/$FIXRUN/inputs" '{"kind":"approve_plan","body":""}' >/dev/null
wait_status "$FIXRUN" completed "${UZI_E2E_COMPLETE_TIMEOUT:-$COMPLETE_TIMEOUT_DEFAULT}"
FIXBRANCH="$(apiget "/api/runs/$FIXRUN" | jq -r '.run.branch')"
case "$FIXBRANCH" in ci-fix/pipeline-*) : ;; *) fail "ci_fix fix branch not ci-fix/pipeline-* (got $FIXBRANCH)";; esac
FIXMR="$(apiget "/api/runs/$FIXRUN" | jq -r '.run.mr_iid')"
{ [ "$FIXMR" != null ] && [ "$FIXMR" -gt 0 ]; } || fail "ci_fix run.mr_iid not set (got $FIXMR)"
[ "$(fake_state | jq -r --arg b "$FIXBRANCH" '[.mrs[] | select(.source_branch==$b)] | length')" -ge 1 ] \
  || fail "fake GitLab recorded no MR from $FIXBRANCH"
pass "ci_fix completed on $FIXBRANCH with MR !$FIXMR (default-branch fix)"

# 4) uzi verifies its work: the fix branch's post-fix pipeline passes → verdict.
fake_post "/_e2e/pipelines" "{\"ref\":\"$FIXBRANCH\",\"status\":\"success\"}" >/dev/null
wait_verdict "$FIXRUN" verified 20
pass "fix branch pipeline passed → run $FIXRUN stamped verified"

# 5) not_code path: a red main whose log says it is not a code problem. Flip main
#    GREEN first so the following red is unambiguously the NEW, sentinel-bearing
#    pipeline the sync must cache (a stale "still failed" would let the ci_fix
#    snapshot the previous, clean pipeline).
fake_post "/_e2e/pipelines" '{"ref":"main","status":"success"}' >/dev/null
wait_board_pipeline success 20
fake_post "/_e2e/pipelines" '{"ref":"main","status":"failed","jobs":[{"name":"deploy","stage":"deploy","status":"failed","trace":"runner disk full UZI_STUB_NOT_CODE"}]}' >/dev/null
wait_board_pipeline failed 20
NCRUN="$(apipost "/api/repos/$REPO_ID/ci-fix-runs" '{"ref":"main"}' | jq -r '.run.id')"
wait_status "$NCRUN" awaiting_approval
apipost "/api/runs/$NCRUN/inputs" '{"kind":"approve_plan","body":""}' >/dev/null
wait_status "$NCRUN" completed "${UZI_E2E_COMPLETE_TIMEOUT:-$COMPLETE_TIMEOUT_DEFAULT}"
[ "$(apiget "/api/runs/$NCRUN" | jq -r '.run.fix_verdict')" = not_code ] || fail "not_code run verdict is not not_code"
[ "$(apiget "/api/runs/$NCRUN" | jq -r '.run.mr_iid')" = null ] || fail "a not_code run must open no MR"
pass "not_code path: run $NCRUN completed with fix_verdict=not_code and no MR"

