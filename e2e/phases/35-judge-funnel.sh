# shellcheck shell=bash
# phase:    judge-funnel
# title:    PRD #46: run judge (stub) — funnel enqueue -> claim -> review -> persist-first notification
# critical: no
# lane:     gitlab
# executor: any
# requires: -
# provides: J_RUN
# handoff:  -
# mutates:  settings:judge_enabled=true
# restores: -
# =============================================================================
# PRD #46 — run judge + notifications inbox, end to end. UZI_E2E_EXECUTOR=stub
# selects the STUB judge queryFn (judge-runner-stub.ts): the judge model call makes
# NO network request and returns an error result, so JudgeRunner deterministically
# posts its command-not-found FALLBACK — a real review with a dummy token and ZERO
# Anthropic spend. What remains here is the funnel (formerly "Phase A"), which is
# genuinely full-wire: enable the judge (global kill-switch + admin opt-in), finish an
# issue run → the committed-terminal funnel enqueues a `judge` run → the worker claims
# it (repo-less, Anthropic-only claim: no forge PAT) → fetches the trace via the
# judge-scoped endpoint → posts a review → a PERSIST-FIRST inbox notification lands
# for the reviewed run.
# Phase B (plant `jq: command not found` → re-judge → the replacement review UPSERTs
# the same single row naming install_worker_tool 'jq') was DROPPED by PRD #97 M4. Every
# link of that chain is proven at a cheaper layer that runs in CI on every MR:
#   - the trace scan that turns `bash: jq: command not found` into a missing-tool
#     signal — `api/internal/workersvc/judge_m3_test.go` TestScanCommandNotFound
#     (four shell dialects + dedupe), TestScanCommandNotFoundFiltersNoise,
#     TestScanCommandNotFoundEmptyWhenClean, and TestJudgeClaimCarriesModelAndSignal
#     (the signal reaches the worker's claim);
#   - the signal → `install_worker_tool` recommendation mapping and the fact that a
#     FAILED model call still lands the review — `agent/test/judge-runner.test.ts`
#     (`fallbackReview` maps missing tools; the UZI_E2E_EXECUTOR=stub queryFn drives
#     the deterministic fallback; a hung/timed-out query still posts it);
#   - the review persisting its recommendations — `judge_m3_test.go`
#     TestPostReviewPersistsVerdictAndRecs;
#   - the re-judge UPSERT ("one review row per target, never a second") — live
#     Postgres, `api/internal/store/recommendation_dispositions_integration_test.go`
#     asserts `reviewID2 == reviewID` after a second
#     UpsertRunReviewWithRecommendations on the same target (UNIQUE target_run_id).
# Consequence: the PRD #68 phase below can no longer read a judge-produced
# recommendation, so it seeds its own coordinate directly (see there).
# Judge is turned OFF again at the end so the later concurrency section's capacity math
# is unaffected.
# =============================================================================
say "PRD #46: run judge (stub) — funnel enqueue -> claim -> review -> persist-first notification"

login   # fresh admin session; login also unlocks the admin's vault (the dummy token is DEK-sealed)
[ "$(apiget /api/auth/me | jq -r '.vault.unlocked')" = true ] \
  || fail "PRD #46: the admin vault must be unlocked so the worker can open the token at judge claim"
apiput /api/admin/settings '{"settings":{"judge_enabled":"true","judge_model":"haiku"}}' >/dev/null
[ "$(apiput /api/me/judge '{"enabled":true}' | jq -r '.user.judge_enabled')" = true ] \
  || fail "PRD #46: PUT /api/me/judge did not enable the per-user opt-in"
pass "judge enabled (global kill-switch + admin opt-in); dummy token present; vault unlocked"

# --- the funnel: a finished run is auto-judged; a review + notification land ---
J_IID="$(apipost "/api/repos/$REPO_ID/issues" \
  '{"title":"E2E judge target","description":"judge e2e — implements prds/46-run-judge-self-improvement.md"}' \
  | jq -r '.card.iid')"
J_RUN="$(create_run "$REPO_ID" "$J_IID")" || fail "judge-target run-create failed (non-transient; see stderr)"
wait_status "$J_RUN" awaiting_approval 90
apipost "/api/runs/$J_RUN/inputs" '{"kind":"approve_plan","body":"","selection":{"source":"repo","exclusions":[]}}' >/dev/null
wait_status "$J_RUN" completed 120
pass "target issue run $J_RUN completed (the run the judge reviews)"

# wait_review RUN [TIMEOUT] — poll the M4 owner-scoped review endpoint until a review lands.
wait_review() {
  local run="$1" timeout="${2:-120}"
  local start=$SECONDS deadline=$((SECONDS + timeout))
  while [ $SECONDS -lt $deadline ]; do
    if [ -n "$(apiget "/api/runs/$run/review" | jq -r '.review.id // empty')" ]; then
      record_margin "judge review landed" "$((SECONDS - start))" "$timeout"; return 0
    fi
    sleep 0.3
  done
  fail "PRD #46: no judge review ever landed for run $run"
}
wait_review "$J_RUN" 120
[ "$(apiget "/api/runs/$J_RUN/review" | jq -r '.review.status')" = failed ] \
  || fail "PRD #46: the stub judge must post a fallback review (status=failed)"
pass "funnel: the finished run was auto-judged; a review landed on the run-page endpoint (stub fallback, status=failed)"

# The judge run is repo-less — the observable proxy for its Anthropic-only/no-PAT claim
# (the wire-level no-PAT assertion is TestClaimJudgeWireCarriesNoPATValue).
J_JUDGE="$(db_psql "SELECT id FROM runs WHERE kind='judge' AND target_run_id='$J_RUN' ORDER BY created_at DESC LIMIT 1")"
[ -n "$J_JUDGE" ] || fail "PRD #46: no judge run row for target $J_RUN"
[ "$(db_psql "SELECT repo_id IS NULL FROM runs WHERE id='$J_JUDGE'")" = t ] \
  || fail "PRD #46: the judge run must be repo-less (no repo join, no forge PAT in its claim)"
pass "judge run $J_JUDGE is repo-less (Anthropic-only claim; no forge PAT)"

# Persist-first: the review POST created an inbox notification anchored to the reviewed run.
[ "$(apiget /api/notifications | jq --arg r "$J_RUN" '[.notifications[] | select(.run_id==$r and .kind=="judge_review")] | length')" -ge 1 ] \
  || fail "PRD #46: no judge_review inbox notification for the reviewed run (persist-first delivery)"
pass "persist-first: a judge_review inbox notification landed for the reviewed run"

