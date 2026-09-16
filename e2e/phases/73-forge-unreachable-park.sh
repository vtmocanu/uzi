# shellcheck shell=bash
# phase:    forge-unreachable-park
# title:    PRD #1392: pre-clone forge park — recovery_wait(forge_unreachable), promote+complete, cap-fail no judge, cancel
# critical: no
# lane:     gitlab
# executor: stub
# requires: REPO_ID UZI_WORKER_TOKEN
# provides: -
# handoff:  -
# mutates:  rewrites the agent gitconfig so the AGENT's clone/fetch URL redirects to an unresolvable .invalid host (a pre-phase copy is saved); forge-fake stays UP the WHOLE phase so the api's claim-time default-branch guardrail + the poller keep reaching it over HTTP; lowers the api forge-park cap to 2 via E2E_FORGE_UNREACHABLE_MAX_PARKS in the env-file (api force-recreated while forge-fake is healthy)
# restores: gitconfig restored byte-for-byte from the pre-phase copy; the forge-park cap default restored (env line removed, api recreated) — done in-phase on the happy path AND by an EXIT-trap fail-safe (phase 20 / 35 precedent) so the fail-soft driver never continues on a broken stack
# =============================================================================
# PRD #1392 M4 — the pre-clone forge-unreachable park, end to end through the LIVE
# claim/clone/park/promote loop (the store-level and worker-level halves are proven at
# cheaper layers: api/internal/workersvc/forgepark_livedb_test.go +
# forgepark_resume_livedb_test.go, and agent/test/runner-forge-park.test.ts).
#
# WHY forge-fake MUST STAY UP (the bug this rework fixes). The park is a CLONE-time,
# agent-side mechanism (phaseClone -> ensureClone, UPSTREAM of the stub executor, fact 4).
# But BEFORE a run ever reaches phaseClone, the api runs the #66 claim-time default-branch
# guardrail (api/internal/privcheck/service.go): at claim it reads the forge's branch
# protection over HTTP to verify the bot token can push. Stopping forge-fake fails THAT
# guard closed, so the run is refused AT CLAIM ("could not verify the bot token against the
# forge") and lands `failed` before phaseClone runs — the pre-clone park is never reached.
# So this phase keeps forge-fake HEALTHY throughout (the claim guard AND the promotion
# re-claim both need it) and breaks ONLY the AGENT's git transport.
#
# HOW the clone is broken WITHOUT touching forge-fake. The api's privcheck + poller reach
# forge-fake using the api's OWN forge config (FORGE_ALLOWED_BASE_URLS), independent of the
# worker. The worker's git clone/fetch goes through the bind-mounted global gitconfig
# ($RUNROOT/agent-gitconfig/gitconfig), read FRESH per git op — no container recreate. This
# phase borrows phase 20's technique: it rewrites that gitconfig so the run's clone URL
# `https://forge-fake.e2e/...` insteadOf-redirects to `https://forge-unreachable.invalid/...`.
# `.invalid` is RFC-6761 guaranteed-unresolvable, so git fails with "could not resolve host"
# — which withForgeRetry classifies transient (agent/src/forge-retry.ts), the trigger the
# park needs — reproducing the real incident ("git fetch failed: Could not resolve host")
# exactly. Restoring the SAVED gitconfig (the default local-bare insteadOf) puts the working
# clone path back so a promoted run re-clones and completes. forge-fake is never stopped and
# the agent is never restarted; the api sees the healthy forge for every claim + snapshot.
#
# Three cases (forge-fake UP throughout; only the git transport toggles broken<->working):
#
#   A  transient outage → the run PARKS (recovery_wait, cause forge_unreachable,
#      forge_park_count=1, retry stamped, zero open custody holds — release+park are one
#      transaction — one feed event, NOT failed); the git path is restored, the sweeper
#      promotes on the retry stamp, the worker re-claims (guard passes) and re-clones against
#      the working local bare, and it COMPLETES on approve.
#   B  the git path stays broken through the cap (lowered to 2): two parks, then the third
#      forge-unreachable attempt cap-fails the run with fail_origin=forge_unreachable and
#      NO judge run (forge_unreachable is in neverJudgeFailOrigins; see the note at the
#      assertion for why the e2e confirms absence and the exclusion gate is proven elsewhere).
#   C  an owner cancel during the clone retries ends the run cancelled (the park
#      transaction reads the stamped cancel under its lock and cancels instead of parking).
#
# The incident's DNS shape stays a unit case in agent/test/forge-retry.test.ts; this phase
# asserts the PARK, not the error phrase.
# =============================================================================
say "PRD #1392 M4: pre-clone forge-unreachable park — park+promote, cap-fail (no judge), cancel"

GITCONFIG="$RUNROOT/agent-gitconfig/gitconfig"
GITCONFIG_SAVE="$RUNROOT/agent-gitconfig/gitconfig.pre-73"
DISPATCHED_RUN=""

# --- fail-safe restore (EXIT trap + explicit happy-path teardown) -------------
# Restore the clone-path gitconfig to its EXACT pre-phase bytes (the default local-bare
# insteadOf, or the smart-HTTP form under E2E_GIT_SMART_HTTP) and reset the api forge-park
# cap to its default. Idempotent and best-effort throughout so a broken step can never mask
# the FAIL that fired the trap. forge-fake + the agent are NEVER stopped by this phase, so
# there is nothing to bring back up — only the gitconfig and the api cap are restored. The
# api recreate always runs while forge-fake is healthy (it was never taken down).
restore_stack() {
  if [ -f "$GITCONFIG_SAVE" ]; then
    cp "$GITCONFIG_SAVE" "$GITCONFIG" 2>/dev/null || true
  fi
  if grep -q '^E2E_FORGE_UNREACHABLE_MAX_PARKS=' "$ENVFILE" 2>/dev/null; then
    if grep -v '^E2E_FORGE_UNREACHABLE_MAX_PARKS=' "$ENVFILE" > "$ENVFILE.tmp73" 2>/dev/null; then
      mv "$ENVFILE.tmp73" "$ENVFILE" 2>/dev/null || true
    fi
  fi
  "${COMPOSE[@]}" up -d --no-deps --force-recreate api >/dev/null 2>&1 || true
}

# break_git_path — redirect the worker's clone/fetch of forge-fake.e2e to an unresolvable
# `.invalid` host. safe.directory=* is re-carried (this file is the worker's
# GIT_CONFIG_GLOBAL; the trust must live in the file, not gitEnv()'s stripped command-scope
# pin — see the phase 20 / lib.sh notes). Read fresh per git op, so no container recreate.
break_git_path() {
  cat > "$GITCONFIG" <<'GITEOF'
[safe]
	directory = *
[url "https://forge-unreachable.invalid/"]
	insteadOf = https://forge-fake.e2e/
GITEOF
}

# restore_git_path — put the working clone path back byte-for-byte from the pre-phase copy
# (the default local-bare insteadOf), so a promoted run re-clones and completes.
restore_git_path() { cp "$GITCONFIG_SAVE" "$GITCONFIG"; }

# assert_one_park_event RUN — poll the run's messages until the forge-park feed event
# (a `status` message, the kind M2 emits in finishForgePark) is FLUSHED, then require
# EXACTLY one. The report→ack lands the recovery_wait state BEFORE finishForgePark emits
# and flushes the event, so a one-shot read right after wait_status can race the flush.
assert_one_park_event() {
  local run="$1" timeout=30 start=$SECONDS n
  while [ $((SECONDS - start)) -lt "$timeout" ]; do
    n="$(apiget "/api/runs/$run/messages" \
      | jq '[.messages[] | select(.kind=="status" and ((.payload.text // "") | test("forge unreachable at clone; parked")))] | length')"
    if [ "${n:-0}" -ge 1 ]; then
      record_margin "forge-park feed event" "$((SECONDS - start))" "$timeout"
      [ "$n" = 1 ] || fail "expected exactly one forge-park feed event, got $n"
      pass "one forge-park feed event on the run feed (\"forge unreachable at clone; parked …\")"
      return 0
    fi
    sleep 0.3
  done
  fail "no forge-park feed event ever landed for run $run"
}

# dispatch_run_forge_broken TITLE — with the git path ALREADY broken, create an issue + run.
# forge-fake is UP, so CreateIssue and the run-create forge snapshot both succeed, and the
# claim-time default-branch guardrail passes; the worker then claims and its FIRST clone
# attempt resolves the `.invalid` host and fails transiently. Sets DISPATCHED_RUN.
dispatch_run_forge_broken() {
  local title="$1" iid
  DISPATCHED_RUN=""
  iid="$(apipost "/api/repos/$REPO_ID/issues" \
    "$(jq -nc --arg t "$title" '{title:$t,description:"implements prds/4-agent-runtime-workers.md"}')" \
    | jq -r '.card.iid')"
  { [ -n "$iid" ] && [ "$iid" != null ]; } || fail "could not create the issue for: $title"
  DISPATCHED_RUN="$(create_run "$REPO_ID" "$iid")" || fail "run-create failed for: $title (non-transient; see stderr)"
  { [ -n "$DISPATCHED_RUN" ] && [ "$DISPATCHED_RUN" != null ]; } || fail "run was not created for: $title"
}

# --- 0) save the clone-path gitconfig + arm the fail-safe ---------------------
cp "$GITCONFIG" "$GITCONFIG_SAVE"
# Arm the trap NOW, before the first stack mutation, so any mid-phase `fail` still restores
# the gitconfig and resets the api cap for a later phase.
trap 'restore_stack || true' EXIT

# --- 1) lower the forge-park cap to 2 (Case B exhausts it) --------------------
# Interpolated into the api's RUN_FORGE_UNREACHABLE_MAX_PARKS via the env-file. forge-fake is
# healthy (never stopped), so the api recreate — which depends_on forge-fake: service_healthy
# — passes immediately.
printf 'E2E_FORGE_UNREACHABLE_MAX_PARKS=2\n' >> "$ENVFILE"
"${COMPOSE[@]}" up -d --no-deps --force-recreate api >/dev/null
wait_http
login
wait_worker_online
pass "api recreated with the forge-park cap lowered to 2; forge-fake healthy; worker online"

# --- Case A: transient outage → park → promote → complete ---------------------
say "Case A: a transient forge outage at clone PARKS the run, then it promotes and completes"
break_git_path
pass "git clone path broken (forge-fake.e2e -> forge-unreachable.invalid); forge-fake still UP for the api"
dispatch_run_forge_broken "E2E forge park then recover"
RUN_A="$DISPATCHED_RUN"
pass "dispatched run $RUN_A; its clone resolves an unresolvable host and fails transiently"

# The clone retries (FORGE_RETRY_SCHEDULE ~31s of sleeps) exhaust with a transient verdict,
# and phaseClone parks instead of failing.
wait_status "$RUN_A" recovery_wait 150
FA="$(apiget "/api/runs/$RUN_A")"
[ "$(echo "$FA" | jq -r '.run.recovery_wait_cause')" = forge_unreachable ] \
  || fail "recovery_wait_cause != forge_unreachable (got: $(echo "$FA" | jq -c '.run.recovery_wait_cause'))"
[ "$(echo "$FA" | jq -r '.run.forge_park_count')" = 1 ] \
  || fail "forge_park_count != 1 (got: $(echo "$FA" | jq -c '.run.forge_park_count'))"
[ "$(echo "$FA" | jq -r '.run.forge_park_max')" = 2 ] \
  || fail "forge_park_max != 2 — the api recreate did not pick up the lowered cap (got: $(echo "$FA" | jq -c '.run.forge_park_max'))"
[ -n "$(echo "$FA" | jq -r '.run.recovery_retry_not_before // empty')" ] \
  || fail "recovery_retry_not_before was not stamped on the park"
[ -z "$(echo "$FA" | jq -r '.run.fail_origin // empty')" ] \
  || fail "run carries a fail_origin while parked (got: $(echo "$FA" | jq -c '.run.fail_origin'))"
pass "run $RUN_A parked: recovery_wait, cause=forge_unreachable, forge_park_count=1 of 2, retry stamped, not failed"

# Release + park are one locked transaction (D3): the parked generation leaves NO open hold.
[ "$(db_psql "SELECT count(*) FROM recovery_custody_holds WHERE run_id='$RUN_A' AND state='open'")" = 0 ] \
  || fail "the parked run still holds an open custody hold — release+park were not atomic"
pass "zero open custody holds for the parked run (release + park were one transaction)"

# Exactly one feed event on the run feed (D4/D10).
assert_one_park_event "$RUN_A"

# Restore the working clone path: the sweeper promotes on the retry stamp, the worker
# re-claims (guard passes) and re-clones against the working local bare, reaches the plan
# gate, and completes on approve.
restore_git_path
pass "git clone path restored (working local bare); awaiting promotion + resume"
wait_status "$RUN_A" awaiting_approval 150
apipost "/api/runs/$RUN_A/inputs" '{"kind":"approve_plan","body":""}' >/dev/null
wait_status "$RUN_A" completed 150
pass "Case A: run $RUN_A promoted, re-cloned against the working path, and completed"

# --- Case B: the git path stays broken through the cap → forge_unreachable, no judge -
say "Case B: the clone stays broken through the cap (2) — the run fails forge_unreachable, no judge"
break_git_path
dispatch_run_forge_broken "E2E forge park cap-fail"
RUN_B="$DISPATCHED_RUN"
pass "dispatched run $RUN_B; the git path stays broken so every re-clone re-parks toward the cap"

# Two parks (count 1, then 2), then the 3rd forge-unreachable attempt exceeds the cap and
# fails the run. Each cycle is ~31s of clone retries + the promotion backoff, so allow room.
wait_status "$RUN_B" failed 360
FB="$(apiget "/api/runs/$RUN_B")"
[ "$(echo "$FB" | jq -r '.run.fail_origin')" = forge_unreachable ] \
  || fail "fail_origin != forge_unreachable (got: $(echo "$FB" | jq -c '.run.fail_origin'))"
[ "$(echo "$FB" | jq -r '.run.forge_park_count')" = 2 ] \
  || fail "forge_park_count != 2 after the cap-fail (got: $(echo "$FB" | jq -c '.run.forge_park_count'))"
[ "$(db_psql "SELECT count(*) FROM recovery_custody_holds WHERE run_id='$RUN_B' AND state='open'")" = 0 ] \
  || fail "the cap-failed run left an open custody hold — the fail path released the hold before the cap check"
# Confirm no judge run exists for the cap-fail. NOTE: the judge is globally disabled at this
# point in the suite (phases 35/39 leave judge_enabled=false), so this checks ABSENCE rather
# than exercising the exclusion gate itself. The forge_unreachable exclusion (neverJudgeFailOrigins)
# is proven at the mutation level by TestForgeParkCapExceededResumedRunNoJudgeLiveDB (which
# enables the judge) and TestNeverJudgeFailOriginsExact. Target column matches phase 35.
[ -z "$(db_psql "SELECT id FROM runs WHERE kind='judge' AND target_run_id='$RUN_B'")" ] \
  || fail "a judge run was enqueued for the forge_unreachable failure"
pass "Case B: run $RUN_B failed forge_unreachable (count=2), no open hold, no judge run"
# Working path back for later phases; broken again per Case C below.
restore_git_path

# --- Case C: an owner cancel during the retries → cancelled -------------------
say "Case C: an owner cancel during the clone retries ends the run cancelled"
break_git_path
dispatch_run_forge_broken "E2E forge park cancel"
RUN_C="$DISPATCHED_RUN"
# Cancel while the worker is still in its clone-retry window (pre-park). The park
# transaction reads the stamped cancel under its lock and transitions to cancelled instead
# of parking (D4), still releasing the generation's hold; a latched cancel converges the
# same way. Landing slightly after a park still cancels on the next cycle — allow room.
apipost "/api/runs/$RUN_C/inputs" '{"kind":"cancel","body":""}' >/dev/null
pass "dispatched run $RUN_C and queued an owner cancel during the retries"
wait_status "$RUN_C" cancelled 150
pass "Case C: run $RUN_C reached cancelled"

# --- restore (explicit happy path) + drop the fail-safe ----------------------
say "restore: default clone path, default forge-park cap"
restore_stack
wait_http
login
wait_worker_online
trap - EXIT
pass "restored the gitconfig and the api default cap for any later phase"
