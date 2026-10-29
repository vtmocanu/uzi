# shellcheck shell=bash
# phase:    forge-unreachable-park
# title:    PRD #1392: pre-clone forge park — recovery_wait(forge_unreachable), promote+complete, owner hold/Resume/rehold no judge, cancel
# critical: no
# lane:     gitlab
# executor: stub
# requires: REPO_ID UZI_WORKER_TOKEN
# provides: -
# handoff:  -
# mutates:  rewrites the agent gitconfig so the AGENT's clone/fetch URL redirects to an unresolvable .invalid host (a pre-phase copy is saved); forge-fake stays UP the WHOLE phase so the api's claim-time default-branch guardrail + the poller keep reaching it over HTTP; lowers the api forge-park cap to 2 via E2E_FORGE_UNREACHABLE_MAX_PARKS in the env-file (api force-recreated while forge-fake is healthy)
# restores: gitconfig restored byte-for-byte from the pre-phase copy; the forge-park cap default restored (env line removed, api recreated) — done in-phase on the happy path AND by an EXIT-trap fail-safe (phase 20 / 35 precedent) so the fail-soft driver never continues on a broken stack
# =============================================================================
# M5 contract update — the pre-clone forge-unreachable park, end to end through the LIVE
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
#      forge_park_count=1, retry stamped, exact-generation custody released after the
#      guarded final receipt (legacy release is atomic), one feed event, NOT failed); the git path is restored, the sweeper
#      promotes subject to actual-origin cooldown after the retry stamp, the worker re-claims (guard passes) and re-clones against
#      the working local bare, and it COMPLETES on approve.
#   B  two timed parks consume the allowance; the next exhausted retry group holds
#      recovery_wait(forge_unreachable), count=2, retry=NULL, preserving the exact
#      worker/session/claim and custody (guarded empty settlement after ACK allowed).
#      Owner Resume queues one ordinary attempt; if admission headroom exists it
#      exhausts and reholds without resetting the allowance. Cancel cleans up.
#   C  an owner cancel during the clone retries ends the run cancelled (the park
#      transaction reads the stamped cancel under its lock and cancels instead of parking).
#
# The incident's DNS shape stays a unit case in agent/test/forge-retry.test.ts; this phase
# asserts the PARK, not the error phrase.
# =============================================================================
say "PRD #1392 M4: pre-clone forge-unreachable park — park+promote, owner hold/Resume/rehold (no judge), cancel"

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
  wait_custody_headroom 1
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

# Guarded workers provide empty-inventory evidence after the park acknowledgement;
# legacy holds release inside the park transaction. Require the exact generation receipt.
# ParkRunForgeUnreachable (runtime.sql) stamps status_since; claim_released_at can
# remain NULL for this park, so its receipt is compared with status_since.
PARK_GENERATION="$(db_psql "SELECT claim_generation FROM runs WHERE id='$RUN_A'")"
wait_forge_park_release "$RUN_A" "$PARK_GENERATION"
pass "parked generation custody released with the required evidence"

# Exactly one feed event on the run feed (D4/D10).
assert_one_park_event "$RUN_A"

# Restore the working clone path: the sweeper may promote after the run retry stamp;
# actual-origin cooldown remains a hard claim filter until due, with one half-open
# reservation. Local-bare insteadOf restores git only, not cooldown health. The worker
# re-claims (guard passes) and re-clones against the working local bare, reaches the plan
# gate, and completes on approve.
restore_git_path
pass "git clone path restored (working local bare); awaiting promotion + resume"
wait_status "$RUN_A" awaiting_approval 600
apipost "/api/runs/$RUN_A/inputs" '{"kind":"approve_plan","body":""}' >/dev/null
wait_status "$RUN_A" completed 150
pass "Case A: run $RUN_A promoted, re-cloned against the working path, and completed"

# --- Case B: finite timed allowance → owner hold → Resume → rehold ------------
say "Case B: exhausted forge allowance holds count=2, no terminal failure or judge"
break_git_path
dispatch_run_forge_broken "E2E forge owner hold"
RUN_B="$DISPATCHED_RUN"
# Capture the third claim WHILE its bounded clone retries are running, so the
# subsequent hold must preserve its worker/session/current generation and hold ID.
# The actual-origin backoff can exceed the run retry; do not force it due in SQL.
wait_eq true 900 "forge third attempt" forge_attempt_ready "$RUN_B"
BEFORE_B="$(forge_park_posture "$RUN_B")"
wait_eq true 150 "forge owner hold" forge_owner_hold_ready "$RUN_B"
FB="$(forge_park_posture "$RUN_B")"
assert_forge_owner_hold "$BEFORE_B" "$FB"
PARK_GENERATION_B="$(printf '%s' "$FB" | jq -r '.generation')"
# Guarded empty inventory may settle AFTER ACK, but legacy custody is preserved.
# Judge is disabled by earlier phases: this proves absence, not its exclusion gate.
if [ "$(printf '%s' "$FB" | jq -r '.holds[0].guarded')" = true ]; then
  wait_forge_park_release "$RUN_B" "$PARK_GENERATION_B"
fi
[ -z "$(db_psql "SELECT id FROM runs WHERE kind='judge' AND target_run_id='$RUN_B'")" ] \
  || fail "a judge run was enqueued for the forge owner hold"
pass "Case B: same claim held, count=2 retry=NULL, no terminal failure or judge"

# Resume queues an ordinary attempt, not a cooldown/custody bypass. The API
# rereads after the write: if cooldown expired, that response may already show
# the ordinary claim. No fresh park allowance is granted.
RESUMED_B="$(apipost "/api/runs/$RUN_B/resume" '{}')"
printf '%s' "$RESUMED_B" | jq -e '(.run.status=="queued" or .run.status=="claimed" or .run.status=="running")
  and .run.forge_park_count==2
  and .run.recovery_wait_cause==null and .run.recovery_retry_not_before==null
  and .run.fail_origin==null and .run.finished_at==null' >/dev/null \
  || fail "Resume did not queue one ordinary attempt: $RESUMED_B"
# Check counted admission, not total open custody. At limit=1 or saturation the
# retained hold may block the ordinary claim: keep that queued contract valid and
# leave rehold execution to the hermetic/live-DB tests rather than promising it.
ADMISSION_B="$(apiget /api/recovery/holds)"
printf '%s' "$ADMISSION_B" | jq -e '.aggregate | [.admission_counted_holds,.custody_hold_limit]
  | all(.[]; type=="number" and .>=0 and .==floor)' >/dev/null \
  || fail "invalid custody admission aggregate: $ADMISSION_B"
if printf '%s' "$ADMISSION_B" | jq -e '.aggregate | .custody_hold_limit==0
    or (.custody_hold_limit>1 and .admission_counted_holds+1<=.custody_hold_limit)' >/dev/null; then
  wait_eq true 900 "forge resumed ordinary attempt" forge_attempt_ready "$RUN_B" "$PARK_GENERATION_B"
  BEFORE_REHOLD_B="$(forge_park_posture "$RUN_B")"
  wait_eq true 150 "forge owner rehold" forge_owner_hold_ready "$RUN_B"
  assert_forge_owner_hold "$BEFORE_REHOLD_B" "$(forge_park_posture "$RUN_B")"
  pass "Case B: one resumed retry group exhausted and reheld at count=2"
else
  pass "Case B: Resume queued; custody limit/saturation prevents an execution guarantee (rehold not exercised)"
fi
[ -z "$(db_psql "SELECT id FROM runs WHERE kind='judge' AND target_run_id='$RUN_B'")" ] \
  || fail "a judge run was enqueued after Resume/rehold"
apipost "/api/runs/$RUN_B/inputs" '{"kind":"cancel","body":""}' >/dev/null
wait_status "$RUN_B" cancelled 150
pass "Case B: owner cancel converged from held/queued posture"
restore_git_path

# --- Case C: an owner cancel during the retries → cancelled -------------------
say "Case C: an owner cancel during the clone retries ends the run cancelled"
break_git_path
dispatch_run_forge_broken "E2E forge park cancel"
RUN_C="$DISPATCHED_RUN"
# An earlier origin observation may still cool this fresh run. Wait for an
# actual ordinary claim before canceling, rather than canceling a queued run.
wait_status "$RUN_C" running 600
PARK_GENERATION_C="$(db_psql "SELECT claim_generation FROM runs WHERE id='$RUN_C'")"
# Cancel while the worker is still in its clone-retry window (pre-park). The park
# transaction reads the stamped cancel under its lock and transitions to cancelled instead
# of parking (D4). Legacy custody releases in that transaction; guarded custody
# requires worker evidence after the terminal ACK. A latched cancel converges the
# same way. Landing slightly after a park still cancels on the next cycle — allow room.
apipost "/api/runs/$RUN_C/inputs" '{"kind":"cancel","body":""}' >/dev/null
pass "dispatched run $RUN_C and queued an owner cancel during the retries"
wait_status "$RUN_C" cancelled 150
wait_forge_park_release "$RUN_C" "$PARK_GENERATION_C" 10 cancelled
[ -z "$(db_psql "SELECT id FROM runs WHERE kind='judge' AND target_run_id='$RUN_C'")" ] \
  || fail "a judge run was enqueued for the cancelled clone"
pass "Case C: run $RUN_C cancelled with its exact final custody receipt, no judge"

# --- restore (explicit happy path) + drop the fail-safe ----------------------
say "restore: default clone path, default forge-park cap"
restore_stack
wait_http
login
wait_worker_online
trap - EXIT
pass "restored the gitconfig and the api default cap for any later phase"
