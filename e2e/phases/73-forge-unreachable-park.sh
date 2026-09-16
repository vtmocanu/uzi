# shellcheck shell=bash
# phase:    forge-unreachable-park
# title:    PRD #1392: pre-clone forge park — recovery_wait(forge_unreachable), promote+complete, cap-fail no judge, cancel
# critical: no
# lane:     gitlab
# executor: stub
# requires: REPO_ID UZI_WORKER_TOKEN
# provides: -
# handoff:  -
# mutates:  drops the agent gitconfig insteadOf rewrite so the clone path hits forge-fake over HTTPS (a pre-phase copy is saved); lowers the api forge-park cap to 2 via E2E_FORGE_UNREACHABLE_MAX_PARKS in the env-file (api force-recreated while forge-fake is healthy); stops/starts forge-fake and the agent around each case
# restores: gitconfig restored byte-for-byte from the pre-phase copy; the forge-park cap default restored (env line removed, api recreated); forge-fake + agent brought back up — done in-phase on the happy path AND by an EXIT-trap fail-safe (phase 20 / 35 precedent) so the fail-soft driver never continues on a broken stack
# =============================================================================
# PRD #1392 M4 — the pre-clone forge-unreachable park, end to end through the LIVE
# claim/clone/park/promote loop (the store-level and worker-level halves are proven at
# cheaper layers: api/internal/workersvc/forgepark_livedb_test.go +
# forgepark_resume_livedb_test.go, and agent/test/runner-forge-park.test.ts).
#
# The default harness rewrites the clone URL to a bind-mounted LOCAL bare (insteadOf), so
# stopping forge-fake does NOT affect clone/fetch (fact 16). This phase borrows phase 20's
# technique to put forge-fake back on the clone path WITHOUT booting the whole suite under
# E2E_GIT_SMART_HTTP: it DROPS the insteadOf lines from the worker's bind-mounted global
# gitconfig (git reads that file fresh per op — no container recreate needed), so the warm
# bare's stored https remote.origin.url now fetches over HTTPS against forge-fake. Stopping
# forge-fake then makes the worker's clone/fetch get connection-refused, which
# withForgeRetry classifies transient — the trigger the park needs. The original gitconfig
# is saved and restored so every later phase keeps the local-bare transport it relies on.
#
# The park happens in phaseClone, UPSTREAM of the stub executor (fact 4). So each case is
# dispatched with the WORKER DOWN (issue-create and the run-create GetIssue snapshot both
# hit the forge — StartRunForUser — so the forge must be UP for the create), then the forge
# is stopped and the worker brought back so its FIRST clone attempt hits an unreachable
# forge. Three cases:
#
#   A  transient outage → the run PARKS (recovery_wait, cause forge_unreachable,
#      forge_park_count=1, retry stamped, zero open custody holds — release+park are one
#      transaction — one feed event, NOT failed); the forge returns, the sweeper promotes
#      it, the worker re-clones against the healthy forge and it COMPLETES on approve.
#   B  the forge stays down through the cap (lowered to 2): two parks, then the third
#      forge-unreachable attempt cap-fails the run with fail_origin=forge_unreachable and
#      NO judge run (forge_unreachable is on the judge pre-start exclusion list).
#   C  an owner cancel during the clone retries ends the run cancelled (the park
#      transaction reads the stamped cancel under its lock and cancels instead of parking).
#
# The incident's DNS shape stays a unit case in agent/test/forge-retry.test.ts; this phase
# asserts the PARK, not the error phrase (a stopped container answers connection-refused or
# a connect-timeout, both transient — fact 16).
# =============================================================================
say "PRD #1392 M4: pre-clone forge-unreachable park — park+promote, cap-fail (no judge), cancel"

GITCONFIG="$RUNROOT/agent-gitconfig/gitconfig"
GITCONFIG_SAVE="$RUNROOT/agent-gitconfig/gitconfig.pre-73"
DISPATCHED_RUN=""

# --- fail-safe restore (EXIT trap + explicit happy-path teardown) -------------
# Restore the clone-path gitconfig to its EXACT pre-phase bytes (insteadOf in the default
# run, or the smart-HTTP form under E2E_GIT_SMART_HTTP), bring the forge + worker back, and
# reset the api forge-park cap to its default. Idempotent and best-effort throughout so a
# broken step can never mask the FAIL that fired the trap. The api recreate is LAST after
# forge-fake is healthy again (api depends_on forge-fake: service_healthy).
restore_stack() {
  if [ -f "$GITCONFIG_SAVE" ]; then
    cp "$GITCONFIG_SAVE" "$GITCONFIG" 2>/dev/null || true
  fi
  "${COMPOSE[@]}" up -d --no-deps --wait forge-fake >/dev/null 2>&1 || true
  if grep -q '^E2E_FORGE_UNREACHABLE_MAX_PARKS=' "$ENVFILE" 2>/dev/null; then
    if grep -v '^E2E_FORGE_UNREACHABLE_MAX_PARKS=' "$ENVFILE" > "$ENVFILE.tmp73" 2>/dev/null; then
      mv "$ENVFILE.tmp73" "$ENVFILE" 2>/dev/null || true
    fi
  fi
  "${COMPOSE[@]}" up -d --no-deps --force-recreate api >/dev/null 2>&1 || true
  "${COMPOSE[@]}" up -d --no-deps agent >/dev/null 2>&1 || true
}

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

# dispatch_forge_down TITLE — sets DISPATCHED_RUN to a fresh run that is claimed and
# retrying its clone against a STOPPED forge. On return: forge-fake DOWN, agent UP+online.
# The worker is stopped for the create so the queued run is NOT claimed+cloned while the
# forge is still reachable; the forge is dropped only after the run exists, then the worker
# is brought back so its first clone attempt hits connection-refused.
dispatch_forge_down() {
  local title="$1" iid
  DISPATCHED_RUN=""
  "${COMPOSE[@]}" up -d --no-deps --wait forge-fake >/dev/null
  "${COMPOSE[@]}" stop agent >/dev/null
  iid="$(apipost "/api/repos/$REPO_ID/issues" \
    "$(jq -nc --arg t "$title" '{title:$t,description:"implements prds/4-agent-runtime-workers.md"}')" \
    | jq -r '.card.iid')"
  { [ -n "$iid" ] && [ "$iid" != null ]; } || fail "could not create the issue for: $title"
  DISPATCHED_RUN="$(create_run "$REPO_ID" "$iid")" || fail "run-create failed for: $title (non-transient; see stderr)"
  { [ -n "$DISPATCHED_RUN" ] && [ "$DISPATCHED_RUN" != null ]; } || fail "run was not created for: $title"
  "${COMPOSE[@]}" stop forge-fake >/dev/null
  "${COMPOSE[@]}" up -d --no-deps agent >/dev/null
  wait_worker_online
}

# --- 0) put forge-fake on the clone path + arm the fail-safe -------------------
cp "$GITCONFIG" "$GITCONFIG_SAVE"
# Arm the trap NOW, right before the first stack mutation, so any mid-phase `fail` still
# restarts forge-fake+agent, restores the gitconfig and resets the api cap for a later phase.
trap 'restore_stack || true' EXIT
cat > "$GITCONFIG" <<'GITEOF'
[safe]
	directory = *
GITEOF
pass "clone path now points at forge-fake over HTTPS (insteadOf dropped; original saved to reinstate)"

# --- 1) lower the forge-park cap to 2 (Case B exhausts it) --------------------
# Interpolated into the api's RUN_FORGE_UNREACHABLE_MAX_PARKS via the env-file. The api
# recreate MUST run while forge-fake is HEALTHY (api depends_on forge-fake: service_healthy),
# so it is done here at phase start with the forge still up.
printf 'E2E_FORGE_UNREACHABLE_MAX_PARKS=2\n' >> "$ENVFILE"
"${COMPOSE[@]}" up -d --no-deps --wait forge-fake >/dev/null
"${COMPOSE[@]}" up -d --no-deps --force-recreate api >/dev/null
wait_http
login
wait_worker_online
pass "api recreated with the forge-park cap lowered to 2; worker back online"

# --- Case A: transient outage → park → promote → complete ---------------------
say "Case A: a transient forge outage at clone PARKS the run, then it promotes and completes"
dispatch_forge_down "E2E forge park then recover"
RUN_A="$DISPATCHED_RUN"
pass "dispatched run $RUN_A; forge-fake stopped, worker back — its clone now hits an unreachable forge"

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

# Bring the forge back: the sweeper promotes on the retry stamp, the worker re-claims and
# re-clones (now reachable, Basic-auth over HTTPS), reaches the plan gate, completes on approve.
"${COMPOSE[@]}" up -d --no-deps --wait forge-fake >/dev/null
pass "forge-fake restarted; awaiting promotion + resume"
wait_status "$RUN_A" awaiting_approval 150
apipost "/api/runs/$RUN_A/inputs" '{"kind":"approve_plan","body":""}' >/dev/null
wait_status "$RUN_A" completed 150
pass "Case A: run $RUN_A promoted, re-cloned against the healthy forge, and completed"

# --- Case B: the forge stays down through the cap → forge_unreachable, no judge -
say "Case B: the forge stays down through the cap (2) — the run fails forge_unreachable, no judge"
dispatch_forge_down "E2E forge park cap-fail"
RUN_B="$DISPATCHED_RUN"
pass "dispatched run $RUN_B; forge-fake stays down so every re-clone re-parks toward the cap"

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
# forge_unreachable is on the judge pre-start exclusion list, so no judge run is enqueued.
# (Target column per phase 35; the exclusion itself is unit-proven in judge_m7a_test.go.)
[ -z "$(db_psql "SELECT id FROM runs WHERE kind='judge' AND target_run_id='$RUN_B'")" ] \
  || fail "a judge run was enqueued for the forge_unreachable failure"
pass "Case B: run $RUN_B failed forge_unreachable (count=2), no open hold, no judge run"

# --- Case C: an owner cancel during the retries → cancelled -------------------
say "Case C: an owner cancel during the clone retries ends the run cancelled"
dispatch_forge_down "E2E forge park cancel"
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
say "restore: insteadOf clone path, default forge-park cap, forge-fake + agent up"
restore_stack
wait_http
login
wait_worker_online
trap - EXIT
pass "restored the gitconfig, the api default cap, and the forge + worker for any later phase"
