# shellcheck shell=bash
# phase:    uzi-eligibility-gate
# title:    PRD #764: uzi run-eligibility gate (no PRD link required + Promote)
# critical: no
# lane:     gitlab
# executor: any
# requires: -
# provides: -
# handoff:  -
# mutates:  -
# restores: -
# =============================================================================
# PRD #764 — `uzi` is the SINGLE run-eligibility gate. An issue carrying the `uzi`
# label runs with no prds/*.md link; an issue WITHOUT `uzi` is refused (422). The
# label is applied from uzi's own UI (forge-first) via POST .../promote, which makes
# a previously non-runnable issue runnable.
say "PRD #764: uzi run-eligibility gate (no PRD link required + Promote)"

# Stage a uzi-labelled issue with NO prds/*.md link, then FullSync so uzi caches it
# (has_prd_link=false). The uzi gate reads the cached labels — a link is optional now.
IID_UZ="$(fake_post /_e2e/issues \
  "$(jq -nc '{title:"E2E uzi run",description:"tiny fix, no plan file here",labels:["uzi"]}')" | jq -r '.iid')"
[ -n "$IID_UZ" ] && [ "$IID_UZ" != null ] || fail "could not stage the uzi issue on the fake"
apipost "/api/repos/$REPO_ID/sync" '' >/dev/null
apiget "/api/repos/$REPO_ID/board" | jq -e --argjson iid "$IID_UZ" \
  '.board.cards[] | select(.iid==$iid) | .has_prd_link==false' >/dev/null \
  || fail "staged uzi issue #$IID_UZ not cached with has_prd_link=false"
pass "staged uzi issue #$IID_UZ (no PRD link), cached"

# Stage a NON-uzi issue (a bare selector) for the refusal + Promote path.
IID_NU="$(fake_post /_e2e/issues \
  "$(jq -nc '{title:"E2E non-uzi",description:"no plan file here either",labels:["bug"]}')" | jq -r '.iid')"
[ -n "$IID_NU" ] && [ "$IID_NU" != null ] || fail "could not stage the non-uzi issue on the fake"
apipost "/api/repos/$REPO_ID/sync" '' >/dev/null

# --- a NON-uzi issue is refused ------------------------------------------------
C="$(apipost_code "/api/repos/$REPO_ID/runs" "{\"issue_iid\":$IID_NU}")"
[ "$C" = 422 ] || fail "non-uzi issue: run-create should 422, got $C"
pass "a non-uzi issue is refused with 422 (add the uzi label to run it)"

# --- a uzi issue with no PRD link runs the normal lifecycle --------------------
RUN_UZ="$(create_run "$REPO_ID" "$IID_UZ")" || fail "uzi run-create failed (non-transient; see stderr)"
[ -n "$RUN_UZ" ] && [ "$RUN_UZ" != null ] || fail "uzi run was not created (gate failed)"
wait_status "$RUN_UZ" awaiting_approval
pass "run $RUN_UZ started with no PRD link and reached the plan gate"
apipost "/api/runs/$RUN_UZ/inputs" '{"kind":"approve_plan","body":""}' >/dev/null
wait_status "$RUN_UZ" completed "${UZI_E2E_COMPLETE_TIMEOUT:-$COMPLETE_TIMEOUT_DEFAULT}"
[ "$(apiget "/api/runs/$RUN_UZ" | jq -r '.run.branch')" = "agent/issue-$IID_UZ" ] \
  || fail "uzi run did not push agent/issue-$IID_UZ"
MR_UZ="$(apiget "/api/runs/$RUN_UZ" | jq -r '.run.mr_iid')"
{ [ "$MR_UZ" != null ] && [ "$MR_UZ" -gt 0 ]; } || fail "uzi run opened no MR (got $MR_UZ)"
pass "uzi run completed the normal lifecycle (branch agent/issue-$IID_UZ, MR !$MR_UZ)"

# UI Promote: the uzi label lands on the fake forge and the returned card reflects it,
# making the previously non-uzi issue runnable.
CARD="$(apipost "/api/repos/$REPO_ID/issues/$IID_NU/promote" '')"
echo "$CARD" | jq -e '.card.labels | index("uzi") != null' >/dev/null \
  || fail "promote: returned card labels missing uzi: $(echo "$CARD" | jq -c '.card.labels')"
wait_eq yes 20 "promote: uzi written to the fake forge issue #$IID_NU" \
  fake_has_label "$IID_NU" uzi
pass "promote: uzi label on the fake forge + reflected in the card, issue now runnable"

