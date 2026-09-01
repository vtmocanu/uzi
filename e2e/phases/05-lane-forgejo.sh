# shellcheck shell=bash
# phase:    lane-forgejo
# title:    PRD #65 M9: the Forgejo lane (UZI_E2E_FORGE=forgejo)
# critical: no
# lane:     forgejo
# executor: any
# requires: -
# provides: -
# handoff:  -
# mutates:  -
# restores: -
# PRD #65 M9 — the Forgejo lane (UZI_E2E_FORGE=forgejo). A FOCUSED lifecycle
# against the same fake's /api/v1 table, run INSTEAD of the GitLab suite. Skipped
# entirely when FORGE=gitlab, so the GitLab lane below is byte-identical.
#
# Connection mechanism (lead-approved): there is NO shipped path to a forgejo
# connection before M6b — CreateConnection refuses non-gitlab (handler/forge.go)
# and the boot seed hardcodes gitlab (seed.go), both DELIBERATE (M6a dark-landing).
# So the harness flips the boot-seeded connection's forge_type to 'forgejo'
# directly in the THROWAWAY test DB: migration 00067's CHECK admits it, the sealed
# PAT is forge-agnostic, base_url is unchanged (the driver just hits /api/v1). This
# is test state only — no api/seed change — so production dark-landing is fully
# intact, and it exercises exactly the runtime M2-M8 made forgejo-capable. The
# CreateConnection-POST-forgejo path (save-time version/scope block) stays gated
# until M6b and is validated there; it is unit-tested in forgejo_test.go today.
# =============================================================================
say "PRD #65 M9 (Forgejo lane): flip the seeded connection to forgejo in the test DB"
FJPGPW="$(grep '^POSTGRES_PASSWORD=' "$ENVFILE" | cut -d= -f2-)"
fj_psql() { "${COMPOSE[@]}" exec -T -e PGPASSWORD="$FJPGPW" db psql -U uzi -d uzi -tAc "$1" | tr -d '\r\n'; }
FJFLIP="$(fj_psql "UPDATE forge_connections SET forge_type='forgejo' WHERE forge_type='gitlab' RETURNING id")"
[ -n "$FJFLIP" ] || fail "forgejo flip updated no connection row"
[ "$(fj_psql "SELECT forge_type FROM forge_connections")" = forgejo ] || fail "connection is not forgejo after the flip"
pass "connection flipped to forge_type=forgejo (test DB only; production dark-landing intact)"

CONN_ID="$(apiget /api/forge/connections | jq -r '.connections[0].id // empty')"
[ -n "$CONN_ID" ] || fail "no connection after the flip"

# 1) Privilege sweep against forgejo: the on-demand privilege-check re-runs
#    VerifyToken (the version gate) + privcheck (D6/D6b) — the SAME code the
#    periodic sweep runs — so it validates the [live] "same PRD #5 verdicts"
#    criterion against forgejo without the connect POST (deferred to M6b).
say "forgejo privilege check: the compliant flipped connection reports least-privilege"
FJPRIV="$(apipost "/api/forge/connections/$CONN_ID/privilege-check" '')"
echo "$FJPRIV" | jq -e '.report.status == "ok"' >/dev/null 2>&1 \
  || fail "forgejo privilege-check not ok (VerifyToken + privcheck against /api/v1): $FJPRIV"
pass "forgejo connection reports least-privilege ✓ (VerifyToken + D6/D6b via the sweep)"

# 2) Version gate on the sweep: a < 16.0.0 server is refused with the DISTINCT
#    version-downgrade finding (D4 + forge.ErrForgeVersionUnsupported), not the
#    generic "could not verify token".
say "forgejo version gate: a < 16.0.0 server is refused with the downgrade finding"
fake_post /_e2e/forgejo-version '{"version":"15.0.4+gitea-1.22.0"}' >/dev/null
FJDOWN="$(apipost "/api/forge/connections/$CONN_ID/privilege-check" '')"
echo "$FJDOWN" | jq -e '.report.status == "error"' >/dev/null 2>&1 \
  || fail "a < 16 forge should make the check error, got: $FJDOWN"
echo "$FJDOWN" | jq -e '.report.token.warnings | any(test("older than the minimum version"))' >/dev/null 2>&1 \
  || fail "downgrade should raise the distinct version finding, got: $FJDOWN"
pass "< 16.0.0 forge refused with the version-downgrade finding (ErrForgeVersionUnsupported) ✓"
fake_post /_e2e/forgejo-version '{"version":"16.0.0+gitea-1.22.0"}' >/dev/null  # restore

# 2b) Connect-POST validation (M6b go-live). The db-flip above tests the RUNTIME
#     against a forgejo connection, but not the SAVE-TIME connect flow — the one
#     path structurally unreachable in M9 (the gate rejected forge_type:forgejo
#     until M6b). Now that the gate is open, a fresh user connects forgejo via the
#     REAL POST /api/forge/connections, exercising the save-time gates:
#       good >=16 + correct scopes -> created; <16 -> refused (VerifyToken version
#       gate); over-privileged -> 422 (CheckToken scope block, D6b).
say "forgejo connect POST (M6b go-live): good >=16 connects; <16 refused; over-privileged 422"
FJCJAR="$RUNROOT/fj-connect.jar"
curl -fsS -c "$FJCJAR" -X POST "$BASE/api/auth/register" -H 'Content-Type: application/json' \
  -d '{"email":"fj-connect@uzi.e2e","password":"e2e-fj-connect-pass-0000"}' >/dev/null \
  || fail "fresh forgejo-connect user could not register"
fj_conn_post() {  # BODY OUTFILE -> prints HTTP status
  curl -sS -b "$FJCJAR" -o "$2" -w '%{http_code}' -X POST "$BASE/api/forge/connections" \
    -H 'Content-Type: application/json' -H "X-CSRF-Token: $(awk '$6=="uzi_csrf"{print $7}' "$FJCJAR")" -d "$1"
}
# The advert now lists forgejo (the picker's source of truth).
apiget /api/forge/config | jq -e '.forge_types | index("forgejo")' >/dev/null \
  || fail "ForgeConfig must advertise forgejo after the M6b flip"
# a) good >=16 + correct scopes -> 201 created (the real connect path).
FJGOOD="$RUNROOT/fj-good.json"
GC="$(fj_conn_post '{"forge_type":"forgejo","base_url":"https://forge-fake.e2e","token":"e2e-forgejo-good-pat-000000"}' "$FJGOOD")"
[ "$GC" = 201 ] || fail "forgejo connect (good >=16) expected 201, got $GC ($(cat "$FJGOOD"))"
[ "$(jq -r '.connection.forge_type // empty' "$FJGOOD")" = forgejo ] || fail "created connection is not forge_type=forgejo: $(cat "$FJGOOD")"
pass "forgejo connect POST created a connection against a good >=16 instance (VerifyToken + scope gate pass) ✓"
# b) < 16.0.0 -> refused at VerifyToken's version gate (not created).
fake_post /_e2e/forgejo-version '{"version":"15.0.4+gitea-1.22.0"}' >/dev/null
FJLOW="$RUNROOT/fj-low.json"
LC="$(fj_conn_post '{"forge_type":"forgejo","base_url":"https://forge-fake.e2e","token":"e2e-forgejo-good-pat-000000"}' "$FJLOW")"
case "$LC" in 2*) fail "forgejo connect against a <16 instance must be refused, got $LC ($(cat "$FJLOW"))";; esac
# Assert the ACTUAL version-downgrade finding (D4), not the bare word "version" (PRD #97
# M3): the old `\|version` alternative was a false-green — nearly every forge error body
# contains "version" (the driver's own message says "server version …"), so a generic
# "token verification failed" with the downgrade finding ABSENT would have passed. The
# driver names the required floor as `Forgejo 16.0.0` in both refusal paths ("below the
# required Forgejo 16.0.0" for a recognized <16 server, "Forgejo 16.0.0 or newer" for an
# unparseable one — forgejo.go:179/182, min renders without the leading v).
grep -qE "below the required Forgejo 16\.0\.0|Forgejo 16\.0\.0 or newer" "$FJLOW" \
  || fail "the <16 refusal must state the version-downgrade finding (required Forgejo 16.0.0), got $(cat "$FJLOW")"
pass "forgejo connect POST refused against a < 16.0.0 instance, naming the required version (save-time gate) ✓"
fake_post /_e2e/forgejo-version '{"version":"16.0.0+gitea-1.22.0"}' >/dev/null  # restore
# c) over-privileged token -> 422 + violations (D6b save-time scope block).
FJOVER="$RUNROOT/fj-over.json"
OC="$(fj_conn_post '{"forge_type":"forgejo","base_url":"https://forge-fake.e2e","token":"e2e-forgejo-overpriv-pat-0000"}' "$FJOVER")"
[ "$OC" = 422 ] || fail "forgejo connect (over-privileged) expected 422, got $OC ($(cat "$FJOVER"))"
jq -e '.violations | length > 0' "$FJOVER" >/dev/null 2>&1 || fail "422 body missing violations: $(cat "$FJOVER")"
pass "forgejo connect POST 422s an over-privileged token with violations (D6b scope block) ✓"

# 3) Bring the worker online (same path as the GitLab lane).
say "issue a worker join token and bring the worker online"
WTOKEN="$(apipost /api/workers '{"name":"e2e-worker-fj"}' | jq -r '.token')"
{ [ -n "$WTOKEN" ] && [ "$WTOKEN" != null ]; } || fail "no worker token minted"
export UZI_WORKER_TOKEN="$WTOKEN"
"${COMPOSE[@]}" up -d --wait agent
wait_worker_online
pass "worker registered and is online"

# 4) Headline lifecycle: a PRD issue -> run -> plan gate -> approve -> completed,
#    with the worker cloning, working, pushing agent/issue-N and opening a PULL
#    REQUEST via /api/v1/.../pulls — never touching main. The issue is created
#    through the api's forgejo CreateIssue (POST /api/v1/.../issues).
say "forgejo run lifecycle: issue -> run -> gate -> approve -> completed (branch + PR)"
FJIID="$(apipost "/api/repos/$REPO_ID/issues" \
  '{"title":"E2E forgejo","description":"implements prds/4-agent-runtime-workers.md"}' | jq -r '.card.iid')"
{ [ -n "$FJIID" ] && [ "$FJIID" != null ]; } || fail "forgejo issue not created via /api/v1"
FJRUN="$(create_run "$REPO_ID" "$FJIID")" || fail "forgejo run not created"
wait_status "$FJRUN" awaiting_approval
[ "$(apiget "/api/runs/$FJRUN" | jq -r '.run.plan_md // empty')" != "" ] || fail "forgejo run reached the gate with no plan"
pass "run $FJRUN reached the plan gate (awaiting_approval)"
apipost "/api/runs/$FJRUN/inputs" '{"kind":"approve_plan","body":""}' >/dev/null
wait_status "$FJRUN" completed "${UZI_E2E_COMPLETE_TIMEOUT:-$COMPLETE_TIMEOUT_DEFAULT}"
pass "run completed"

# The worker recorded a PR iid; the fake holds exactly one PR, open, against main.
FJMR="$(apiget "/api/runs/$FJRUN" | jq -r '.run.mr_iid // empty')"
{ [ -n "$FJMR" ] && [ "$FJMR" != null ] && [ "$FJMR" != 0 ]; } || fail "run did not record a PR iid (mr_iid)"
PRS="$(fake_state | jq '[.mrs[]] | length')"
[ "$PRS" = 1 ] || fail "expected exactly one PR opened on the fake, got $PRS"
[ "$(fake_state | jq -r '.mrs[0].target_branch')" = main ] || fail "PR base is not main"
[ "$(fake_state | jq -r '.mrs[0].state')" = opened ] || fail "PR is not open"
# D8: the worker persisted the forge-supplied PR web URL (mr_web_url), not a guess.
MRURL="$(apiget "/api/runs/$FJRUN" | jq -r '.run.mr_web_url // empty')"
case "$MRURL" in https://*) : ;; *) fail "mr_web_url not a persisted https URL (D8): '$MRURL'";; esac
pass "worker pushed a branch and opened PR !$FJMR against main (mr_web_url persisted) — never touching main ✓"

# 5) R4: no pull request reaches the board as a card. The fake serves the PR in
#    the /issues list (pull_request != null, number 10000+iid); the driver must
#    filter it. A regressed filter would surface card #(10000+iid).
say "forgejo R4: a pull request must NOT appear on the board as a card"
BADCARD="$(apiget "/api/repos/$REPO_ID/board" | jq '[.board.cards[] | select(.iid >= 10000)] | length')"
[ "$BADCARD" = 0 ] || fail "a pull request leaked onto the board as a card (R4 regression)"
pass "no PR on the board — R4 holds ✓"

# 6) CI status + id-DESC: speed the poller, then enqueue 2 Actions runs on main —
#    an older SUCCESS and a newer FAILURE. LatestPipeline takes runs[0] of the
#    id-DESC list, so the board must cache the NEWEST (failure), proving the
#    driver picks newest-of-2 AND that a failure caches as "failure" (not dropped/
#    neutral, R5-at-cache). NOTE: the fake returns id-DESC, so this proves the
#    driver takes [0]; the SERVER-side id-DESC ordering is the live-container check
#    (deferred — no runner env, D10a).
say "forgejo CI status + id-DESC: newest of 2 runs on main wins (failure over older success)"
printf 'E2E_FORGE_POLL_INTERVAL=2s\nFORGE_RECONCILE_EVERY=2\n' >> "$ENVFILE"
"${COMPOSE[@]}" up -d --no-deps --force-recreate api >/dev/null
wait_http
login
fake_post /_e2e/actions-runs '{"branch":"main","sha":"sha-main","status":"success","jobs":[{"name":"build","status":"success","log":"ok"}]}' >/dev/null
fake_post /_e2e/actions-runs '{"branch":"main","sha":"sha-main","status":"failure","jobs":[{"name":"build","status":"failure","log":"boom at line 5\nFAIL"}]}' >/dev/null
wait_board_pipeline failure 30
pass "board cached the NEWEST run (failure) over the older success — id-DESC [0] + failure-caches-as-failure ✓"

# 7) Fix-CI loop DRIVE — the headline [live] criterion, unblocked by the Go
#    pipeline-status classifier (internal/pipelinestatus). main is at "failure"
#    (above), so this drives the loop end to end and OBSERVES two of the three Go
#    sites against a real run: the Fix-CI START GATE (ci_fix.go:88) must ACCEPT a
#    Forgejo "failure" (no run id is returned unless IsFailed("failure") passes),
#    and the fix VERDICT (pipeline_sync.go) must stamp fix_failed on a re-"failure".
#    The failed-job SNAPSHOT filter (ci_fix.go:144) is deliberately NOT asserted
#    here — an empty snapshot does not error and the fix run is created regardless,
#    so this lane cannot see its content — its "failure"-job inclusion is
#    unit-covered (handler.TestSnapshotFailedPipelineIncludesForgejoFailureJobs).
say "forgejo Fix-CI loop: a 'failure' pipeline drives Fix CI → fix run → fix_failed verdict"
FJFIX="$(apipost "/api/repos/$REPO_ID/ci-fix-runs" '{"ref":"main"}' | jq -r '.run.id')"
{ [ -n "$FJFIX" ] && [ "$FJFIX" != null ]; } \
  || fail "Fix CI not triggered on a Forgejo 'failure' pipeline (the ci_fix.go:88 gate must accept 'failure')"
[ "$(apiget "/api/runs/$FJFIX" | jq -r '.run.kind')" = ci_fix ] || fail "run kind is not ci_fix"
DUP="$(apipost_code "/api/repos/$REPO_ID/ci-fix-runs" '{"ref":"main"}')"
[ "$DUP" = 409 ] || fail "a duplicate Fix CI on main should be 409, got $DUP"
pass "Fix CI triggered on a Forgejo 'failure' pipeline (the ci_fix.go:88 gate accepts 'failure') — ci_fix run $FJFIX"

wait_status "$FJFIX" awaiting_approval
[ "$(apiget "/api/runs/$FJFIX" | jq -r '.run.plan_md // empty')" != "" ] || fail "ci_fix run carried no plan"
apipost "/api/runs/$FJFIX/inputs" '{"kind":"approve_plan","body":""}' >/dev/null
wait_status "$FJFIX" completed "${UZI_E2E_COMPLETE_TIMEOUT:-$COMPLETE_TIMEOUT_DEFAULT}"
FJFIXBR="$(apiget "/api/runs/$FJFIX" | jq -r '.run.branch')"
case "$FJFIXBR" in ci-fix/pipeline-*) : ;; *) fail "ci_fix fix branch not ci-fix/pipeline-* (got $FJFIXBR)";; esac
[ "$(fake_state | jq -r --arg b "$FJFIXBR" '[.mrs[] | select(.source_branch==$b)] | length')" -ge 1 ] \
  || fail "the fake recorded no PR from the fix branch $FJFIXBR"
pass "fix run $FJFIX completed on $FJFIXBR and opened a PR"

# The fix branch's re-run FAILS again ("failure", a higher id than the failure that
# spawned the run): the verdict must stamp fix_failed — the exact pipeline_sync
# IsFailed("failure") path a bare == "failed" never reached. The fake's PR head.sha
# for the fix branch == the run head_sha, so LatestMRPipeline resolves it.
fake_post /_e2e/actions-runs "$(jq -nc --arg b "$FJFIXBR" --arg s "sha-$FJFIXBR" '{branch:$b,sha:$s,status:"failure",jobs:[{name:"build",status:"failure",log:"still broken\nFAIL"}]}')" >/dev/null
wait_verdict "$FJFIX" fix_failed 30
pass "a re-'failure' fix pipeline stamped fix_failed (pipeline_sync IsFailed path) — the CI-fix loop works for Forgejo ✓"

# ---------------------------------------------------------------------------
# OPT-IN LIVE PASS (D10) — documented TODO, deliberately not wired.
#
# This lane runs entirely against forge-fake's /api/v1 table (the default, fast
# lane). A real released `codeberg.org/forgejo/forgejo:16.0.0` container (boots
# ~4s on sqlite: FORGEJO__database__DB_TYPE=sqlite3, __security__INSTALL_LOCK=true;
# reports 16.0.0+gitea-1.22.0, passes the D4a gate) would be an opt-in pass adding
# the two things a fixture CANNOT prove:
#   1. SERVER-side Actions id-DESC ordering — the fake returns id-DESC by
#      construction, so this lane proves the DRIVER takes runs[0]; only a real
#      server proves it RETURNS newest-first (models/actions/run_list.go ToOrders).
#      Enqueue 2+ runs on one branch/sha (a push to a repo with .forgejo/workflows/
#      enqueues a queued run even with NO runner) and assert [0] is the newest.
#   2. Real job-log RETRIEVAL — a registered forgejo-runner executing a workflow,
#      so JobLogTail pulls REAL text/plain logs (auth/redirect/content-type on a
#      live run), not canned fixtures. Needs a second container + runner setup.
# How-to sketch: a UZI_E2E_FORGE_LIVE=1 flag that (a) `docker run`s the pinned
# image BY DIGEST on a loopback port, (b) seeds a bot + token + repo via its API,
# (c) points the connection's base_url at it instead of forge-fake, (d) runs the
# non-Actions criteria live. Deferred (no runner environment — D10a); the released
# image's wire shapes were already probed while writing M2/M4/M5. Do it when a
# runner env exists; it blocks nothing on the critical path.
# ---------------------------------------------------------------------------

pass "PRD #65 M9 Forgejo lane complete"
