# shellcheck shell=bash
# phase:    lane-github
# title:    PRD #238 M8: the GitHub lane (UZI_E2E_FORGE=github)
# critical: no
# lane:     github
# executor: any
# requires: -
# provides: -
# handoff:  -
# mutates:  -
# restores: -
# =============================================================================
# PRD #238 M8 — the GitHub lane (UZI_E2E_FORGE=github). A FOCUSED lifecycle
# against the same fake's /api/v3 table, run INSTEAD of the GitLab suite. Skipped
# entirely when FORGE=gitlab, so the GitLab lane below is byte-identical.
#
# Connection mechanism (same shape as the Forgejo lane): the harness flips the
# boot-seeded connection's forge_type to 'github' directly in the THROWAWAY test DB
# rather than creating a github connection through the product. This is a NETWORK-
# ALIASING workaround, not a gate one: post-M10 CreateConnection accepts github, but
# the driver derives its API host as api.<web-host>, which for the fake would be
# api.forge-fake.e2e — a name the e2e overlay does not alias — whereas the seeded
# base_url (https://forge-fake.e2e) plus go-github's /api/v3 enterprise mount lands
# the driver on the fake's shared table directly. migration 00102's CHECK admits
# 'github', the sealed PAT is forge-agnostic (a classic-PAT shape — no github_pat_
# prefix), and the base_url is unchanged. Test state only — no api/seed change.
#
# SCOPE (deliberate): this lane exercises the api-side GitHub DRIVER end to end
# (github.go / github_pipelines.go) — VerifyToken + TokenInfo (X-OAuth-Scopes),
# ProjectRole + branch-protection rulesets (D6), issue create + R4 PR filter, the
# Actions two-field status/conclusion collapse (D8), and the CI-fix trigger gate.
# It does NOT drive a worker run: the worker's GitHubClient (agent/src/forge.ts) IS
# wired into runner.ts (the 3-way forge pick), but its api.github.com host derivation
# has no network alias here — so the worker PR path is validated at the FAKE (direct
# /api/v3 /pulls create+dup smoke) and by the runner-push-mr unit test, rather than
# through a live claim in this lane. See the run report.
# =============================================================================
say "PRD #238 M8 (GitHub lane): flip the seeded connection to github in the test DB"
GHPGPW="$(grep '^POSTGRES_PASSWORD=' "$ENVFILE" | cut -d= -f2-)"
gh_psql() { "${COMPOSE[@]}" exec -T -e PGPASSWORD="$GHPGPW" db psql -U uzi -d uzi -tAc "$1" | tr -d '\r\n'; }
GHFLIP="$(gh_psql "UPDATE forge_connections SET forge_type='github' WHERE forge_type='gitlab' RETURNING id")"
[ -n "$GHFLIP" ] || fail "github flip updated no connection row"
[ "$(gh_psql "SELECT forge_type FROM forge_connections")" = github ] || fail "connection is not github after the flip"
pass "connection flipped to forge_type=github (test DB only; production dark-landing intact)"

# Speed the reconcile poller and recreate the api so it rebuilds the GitHub driver
# (ForgeForConnection is per-call, but the fast poll interval is what the pipeline-
# cache + issue-sync assertions below depend on). Same knob the Forgejo/GitLab CI
# phases use. Overlay-only; production defaults untouched.
printf 'E2E_FORGE_POLL_INTERVAL=2s\nFORGE_RECONCILE_EVERY=2\n' >> "$ENVFILE"
"${COMPOSE[@]}" up -d --no-deps --force-recreate api >/dev/null
wait_http
login

CONN_ID="$(apiget /api/forge/connections | jq -r '.connections[0].id // empty')"
[ -n "$CONN_ID" ] || fail "no connection after the flip"

# 1) Privilege sweep against github: the on-demand check re-runs VerifyToken (GET
#    /user), TokenInfo (X-OAuth-Scopes header → {repo}, least-privilege), ProjectRole
#    (GET /repositories/{id} permissions → write, not admin), and
#    DefaultBranchProtection (GET /branches/main → protected; GET
#    /rules/branches/main → an enforced pull_request ruleset → the bot cannot push
#    or merge to main). NB: the driver must NEVER call the admin-gated
#    /branches/main/protection (the fake 403s + logs it if it does).
say "github privilege check: the compliant flipped connection reports least-privilege"
GHPRIV="$(apipost "/api/forge/connections/$CONN_ID/privilege-check" '')"
echo "$GHPRIV" | jq -e '.report.status == "ok"' >/dev/null 2>&1 \
  || fail "github privilege-check not ok (VerifyToken + TokenInfo + D6 against /api/v3): $GHPRIV"
pass "github connection reports least-privilege ✓ (VerifyToken + TokenInfo scopes + D6 rulesets)"

# 2) Issue lifecycle via the api's github CreateIssue (POST /api/v3/.../issues):
#    the card appears on the board (synchronous create).
say "github issue create: a PRD issue lands as a board card via /api/v3"
GHIID="$(apipost "/api/repos/$REPO_ID/issues" \
  '{"title":"E2E github","description":"implements prds/4-agent-runtime-workers.md"}' | jq -r '.card.iid')"
{ [ -n "$GHIID" ] && [ "$GHIID" != null ]; } || fail "github issue not created via /api/v3"
pass "issue #$GHIID created and carded"

# 3) Worker-facing PR endpoints, validated directly at the fake (the runner.ts wiring
#    gap keeps this off the live claim path — see the lane header). CreatePullRequest
#    returns 201 with number + html_url; a duplicate open PR for the same head/base
#    returns 422 (GitHub's generic-validation status), which the worker's
#    driver-declared duplicate set {409,422} catches (R6). This also seeds a PR-shaped
#    entry the R4 filter must drop below.
say "github PR endpoints (fake): create → 201 (number+html_url); duplicate → 422 (R6 dup set)"
GHPR_BODY="$RUNROOT/gh-pr.json"
GHPR_CODE="$(curl -sSk -o "$GHPR_BODY" -w '%{http_code}' -X POST "$FAKE_BASE/api/v3/repos/group/repo/pulls" \
  -H 'Content-Type: application/json' -H "Authorization: Bearer $DUMMY_FORGE_PAT" \
  -d '{"head":"agent/issue-238-e2e","base":"main","title":"E2E github PR","body":"b"}')"
[ "$GHPR_CODE" = 201 ] || fail "github PR create expected 201, got $GHPR_CODE ($(cat "$GHPR_BODY"))"
{ [ "$(jq -r '.number // empty' "$GHPR_BODY")" != "" ] && [ "$(jq -r '.html_url // empty' "$GHPR_BODY")" != "" ]; } \
  || fail "github PR create response missing number/html_url: $(cat "$GHPR_BODY")"
GHPR_DUP="$(curl -sSk -o /dev/null -w '%{http_code}' -X POST "$FAKE_BASE/api/v3/repos/group/repo/pulls" \
  -H 'Content-Type: application/json' -H "Authorization: Bearer $DUMMY_FORGE_PAT" \
  -d '{"head":"agent/issue-238-e2e","base":"main","title":"E2E github PR","body":"b"}')"
[ "$GHPR_DUP" = 422 ] || fail "duplicate github PR expected 422 (the driver-declared dup set), got $GHPR_DUP"
pass "PR create returns 201 (number+html_url); a duplicate returns 422 ✓"

# 4) Actions CI status: seed 2 runs on main — an older SUCCESS and a newer FAILURE.
#    Each run carries BOTH status and conclusion (D8); LatestPipeline takes runs[0]
#    of the id-DESC list and the driver COLLAPSES status:"completed"+conclusion:
#    "failure" into "failure", so the board must cache the NEWEST run's failure
#    (proving newest-of-2 AND that the two-field collapse yields a failure, not a
#    dropped/neutral status — R2/D8 at cache time).
say "github CI status + D8 collapse: newest of 2 runs on main wins (failure over older success)"
fake_post /_e2e/github-actions-runs '{"branch":"main","sha":"sha-main","status":"completed","conclusion":"success","jobs":[{"name":"build","status":"completed","conclusion":"success","log":"ok"}]}' >/dev/null
fake_post /_e2e/github-actions-runs '{"branch":"main","sha":"sha-main","status":"completed","conclusion":"failure","jobs":[{"name":"build","status":"completed","conclusion":"failure","log":"boom at line 5\nFAIL"}]}' >/dev/null
wait_board_pipeline failure 30
pass "board cached the NEWEST run (failure) over the older success — id-DESC[0] + D8 status/conclusion collapse ✓"

# 5) R4: a pull request must NOT appear on the board as a card. The reconcile that
#    cached the pipeline above also ran ListIssues, which the fake answers with the
#    real issue AND the PR-shaped entry (pull_request != null, number 10000+iid); the
#    driver must filter it. A regressed filter would surface card #(10000+iid).
say "github R4: a pull request must NOT appear on the board as a card"
BADCARD="$(apiget "/api/repos/$REPO_ID/board" | jq '[.board.cards[] | select(.iid >= 10000)] | length')"
[ "$BADCARD" = 0 ] || fail "a pull request leaked onto the board as a card (R4 regression)"
pass "no PR on the board — R4 holds ✓"

# 6) CI-fix TRIGGER gate (D8, the CI-fix loop's silent-failure surface): main is at
#    "failure" in the cache (step 4), so the trigger must ACCEPT it (ci_fix.go:95
#    IsFailed("failure")), snapshot the failed pipeline (ListWorkflowJobs +
#    GetWorkflowJobLogs), and create a ci_fix run. A duplicate on the same ref is 409.
#    NOTE: the job-log 302 → text/plain second hop is unit-tested with the SSRF guard
#    relaxed; in-network the driver's production guard rejects the private fake host,
#    which ci_fix.go swallows (tail=""), so the fix run is created regardless. The
#    302 → text/plain SHAPE is proven at the fake in step 7.
say "github Fix-CI gate: a 'failure' Actions run drives Fix CI → ci_fix run"
GHFIX="$(apipost "/api/repos/$REPO_ID/ci-fix-runs" '{"ref":"main"}' | jq -r '.run.id')"
{ [ -n "$GHFIX" ] && [ "$GHFIX" != null ]; } \
  || fail "Fix CI not triggered on a GitHub 'failure' run (the ci_fix.go:95 gate must accept 'failure')"
[ "$(apiget "/api/runs/$GHFIX" | jq -r '.run.kind')" = ci_fix ] || fail "run kind is not ci_fix"
GHDUP="$(apipost_code "/api/repos/$REPO_ID/ci-fix-runs" '{"ref":"main"}')"
[ "$GHDUP" = 409 ] || fail "a duplicate Fix CI on main should be 409, got $GHDUP"
pass "Fix CI triggered on a GitHub 'failure' run (D8 gate accepts 'failure') — ci_fix run $GHFIX ✓"

# 7) D5 job-log redirect SHAPE, proven at the fake: the logs endpoint returns a 302
#    whose Location points at a text/plain blob the fake serves itself. (The api-side
#    fetch through the SSRF guard is unit-tested; here we assert the wire shape the
#    driver's GetWorkflowJobLogs + fetchJobLog consume.)
say "github job-log redirect (fake): /logs → 302 → text/plain blob (D5 shape)"
GHRUN_ID="$(curl -sSk "$FAKE_BASE/api/v3/repos/group/repo/actions/runs?branch=main&per_page=1" | jq -r '.workflow_runs[0].id')"
GHJOB_ID="$(curl -sSk "$FAKE_BASE/api/v3/repos/group/repo/actions/runs/$GHRUN_ID/jobs" | jq -r '.jobs[0].id')"
GHLOG_CODE="$(curl -sSk -o /dev/null -w '%{http_code}' "$FAKE_BASE/api/v3/repos/group/repo/actions/jobs/$GHJOB_ID/logs")"
[ "$GHLOG_CODE" = 302 ] || fail "job-logs endpoint should 302 (redirect to the blob), got $GHLOG_CODE"
GHLOG_CT="$(curl -sSk -o /dev/null -w '%{content_type}' "$FAKE_BASE/api/v3/repos/group/repo/actions/jobs/$GHJOB_ID/logs/blob")"
case "$GHLOG_CT" in text/plain*) : ;; *) fail "job-log blob content-type must be text/plain, got '$GHLOG_CT'";; esac
pass "job logs return a 302 → text/plain blob (D5 shape) ✓"

pass "PRD #238 M8 GitHub lane complete"
