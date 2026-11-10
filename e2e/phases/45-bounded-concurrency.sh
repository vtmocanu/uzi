# shellcheck shell=bash
# phase:    bounded-concurrency
# title:    PRD #42 bounded-concurrency scenario (stub-only)
# critical: no
# lane:     gitlab
# executor: any
# race-sensitive: yes
# requires: UZI_WORKER_TOKEN
# provides: -
# handoff:  -
# mutates: owner autopilot disabled; group/repo2 enabled; agent cap 2; creates cap-test runs; wires the DinD sidecar for the cap-2 and kill scenarios when the stack is not already wired (starts dind/dind-init, appends UZI_DIND_SOCKET to $ENVFILE, force-recreates agent); a fresh canonical-control run is killed, refused and cancelled after the posture is restored
# restores: owner autopilot stays disabled to isolate later phases; repo2 stays enabled and agent stays at cap 2 for the next phase; the entry Docker posture (docker_restore_posture: stops and removes only the sidecar services this phase started, removes only the env line it appended, then force-recreates agent and asserts it unwired; nothing changes when the stack was already wired); open custody holds left <= the entry census E45, after the declared owner discards (resolve_fixture_source_hold) of the killed predecessors' generation-1 holds of runs KA and KB and of both generations of the canonical control run C; every hold this phase opens is individually released (completion receipt), awaited to its archive release (a hold with an available capture) or discarded, so phases 72 and 74 see no custody residue from this phase under the unchanged custody limit 8
# custody:  holds are counted relative to E45, the open_holds census taken at phase entry (log_hold_census "phase 45 entry"). Each accepted claim generation opens ONE hold. admission_counted_holds (ADR-2445) = open holds minus at most one hold per run that is live and healthy at its current generation; every other open hold counts, including a terminal or killed run's hold, a run in recovery_wait, and a restored run's predecessor-generation hold, which stays open until its declared discard. wait_custody_headroom N asks for counted + N <= limit (8) before N NEW initial claims; a restore claim is a continuation (1-7 own holds) and needs none.
#   A, B (repo, repo2; one claim each): +2 open while live at the gate, +0 counted; the entry headroom wait reserves 2. Completion releases both holds, awaited by wait_completed_custody_receipt before the kill scenario: back to E45. Peak E45+2 open.
#   KA, KB (headroom 2 re-reserved first; generation 1 killed by the agent SIGKILL, restored by one successor claim at generation 2, asserted original+1, no extra park or generation): after the kill each run's generation-1 hold counts, E45+2 open and +2 counted; after the restore each run holds generation 1 (counted, kept) and generation 2 (live, excluded): peak E45+4 open, +2 counted. Completion releases the two generation-2 holds (receipt awaited): E45+2. resolve_fixture_source_hold then discards generation 1 of KA and of KB: E45+0 (a hold with an available capture is instead awaited to its archive release, never discarded).
#   C canonical control (docker_restore_posture first; headroom 1): generation 1 +1 open; after the kill the canonical-shaped predecessor is not restored, the generation-2 claim is refused into recovery_wait and neither hold is live: +2 open, +2 counted. Owner Cancel keeps both; resolve_fixture_source_hold discards generation 1 and generation 2 (GEN_C_ORIGINAL, +1): E45+0.
#   Phase peak E45+4 open (KA and KB restored at the gate), at most +2 counted from this phase's own runs; exit bound: assert_hold_count_le "phase 45 exit" E45, so every hold this phase opened is released by receipt or discarded by the declared discards. The bound is <= rather than = because earlier phases' holds may settle meanwhile.
# =============================================================================
# PRD #42 — bounded worker concurrency (cap 2). ADDITIVE + LAST: the entire suite
# above ran the single worker at the DEFAULT cap (1 — the pre-#42 serial loop),
# unchanged. This final phase reconfigures that ONE worker to
# WORKER_MAX_CONCURRENT_RUNS=2 and proves, on the stub path, that it:
#   (b) executes two runs on two DIFFERENT repos GENUINELY concurrently — both are
#       simultaneously parked at the plan gate (awaiting_approval), which a cap-1
#       worker can NEVER do: a slot is held across the gate (PRD #42 Decision 2),
#       so at cap 1 the second run would stay `queued`. Same single worker, both
#       past `claimed`, both non-terminal at once = real overlap, deterministically
#       (no reliance on racing the fast stub);
#   (c) reports active_runs=2 / max_concurrent_runs=2 on the API worker listing
#       while both are live (the "N/M runs" saturation badge's data, PRD #42 M3a);
#   (d) lands BOTH MRs, each on its own repo's independent git bare-cache, with no
#       message cross-talk between the two concurrent run streams;
#   (e) on a mid-run SIGKILL of the agent (two in-flight runs), re-queues BOTH
#       together via the SWEEPER at N=2 (worker-loss recovery, now exercised with
#       two runs), and a restarted worker re-claims both by affinity and completes
#       them.
#
# STATED LIMIT (PRD #42 M5 / review — do not overclaim): the stub executor is
# already concurrency-safe (zero instance-level run state), so this exercises the
# worker loop + server + API-listing path, NOT the M1 per-run executor kill/reap
# isolation fix (per-instance SdkExecutor / runId-scoped killAgentTree). M1's unit
# test (agent/test/) is the guard for that; the stub cannot exercise it.
assert_cap2_recovered_run() {
  local run="$1" original="$2" branch="$3" generation parks
  generation="$(db_psql "SELECT claim_generation FROM runs WHERE id = '$run'")"
  parks="$(db_psql "SELECT recovery_wait_count FROM runs WHERE id = '$run'")"
  [ "$generation" = "$((original + 1))" ] \
    || fail "run $run recovery generation is $generation, want $((original + 1))"
  [ "$parks" = 0 ] || fail "run $run re-parked retained recovery ($parks parks)"
  # #3644: the re-claim seeds a fresh attempt from the killed predecessor's verified pin (no bundle
  # capture). The stub's pin is already on the default branch here, hence the on_default binding. The
  # run is parked at the gate, so the witness also proves the predecessor is untouched while S is live.
  # The stub executor commits only after the plan gate (StubExecutor.run, agent/src/executor.ts), so S's HEAD
  # at awaiting_approval is exactly the seeded head.
  wait_independent_restore_resume "$run" "$generation" "$branch" on_default 60
  restore_assert_seed_witness "$run" "$branch" "$generation" exact
}

if [ "$EXECUTOR" != stub ]; then
  say "PRD #42 bounded-concurrency scenario: SKIPPED (stub-only; executor=$EXECUTOR)"
else
  say "PRD #42: reconfigure the worker to WORKER_MAX_CONCURRENT_RUNS=2 and enable a second repo"
  login   # fresh admin session (also re-unlocks the admin vault so claims proceed)

  # Phase 29 restores its opt-in, but selective/failed earlier phases must not let
  # enabling repo2 dispatch old autopilot issues alongside this exact-count test.
  set_owner_autopilot false || fail "could not disable autopilot before enabling repo2"

  # Enable group/repo2 (served by forge-fake only when FORGE_FAKE_PROJECT2 is set;
  # the seed enabled just group/repo, so repo2 has been a disabled, invisible row).
  CONN_ID="$(apiget /api/forge/connections | jq -r '.connections[0].id // empty')"
  [ -n "$CONN_ID" ] || fail "no forge connection to enable repo2 against"
  REPO2_ID="$(apiget "/api/forge/connections/$CONN_ID/projects" \
    | jq -r '.repos[] | select(.path_with_namespace=="group/repo2") | .id')"
  [ -n "$REPO2_ID" ] && [ "$REPO2_ID" != null ] \
    || fail "forge-fake did not advertise group/repo2 (is FORGE_FAKE_PROJECT2 set in the overlay?)"
  apiput "/api/repos/$REPO2_ID" '{"enabled":true}' | jq -e '.repo.enabled == true' >/dev/null \
    || fail "could not enable group/repo2"
  pass "second repo group/repo2 enabled (id $REPO2_ID)"

  # Recreate the one worker at cap 2. UZI_WORKER_TOKEN reaches this (post-#966) subshell
  # via phase 13's `provides` round-trip (see `requires: UZI_WORKER_TOKEN` above), so the
  # force-recreated container re-reads the SAME join token and re-registers into its
  # existing row; without the provide it would read the --env-file placeholder and 401.
  #
  # #3644: the kill scenarios below need attempt-shaped predecessors, and the agent resolves its Docker
  # wiring once at startup, so wire the DinD sidecar BEFORE this recreate (docker_wire_prepare starts it
  # and appends UZI_DIND_SOCKET; this recreate is the one that picks it up) and assert it right after.
  # The trap restores the entry posture if the phase ends (fails) before its normal docker_restore_posture;
  # it is a no-op once that has run, and never changes the phase's exit status.
  trap docker_wire_exit_restore EXIT
  docker_wire_prepare
  printf 'UZI_E2E_MAX_CONCURRENT_RUNS=2\n' >> "$ENVFILE"
  capture_agent_logs 45-bounded-concurrency before-recreate-cap2
  "${COMPOSE[@]}" up -d --no-deps --force-recreate agent >/dev/null
  # Wait for the NEW worker's registration to actually LAND its advertised cap —
  # not merely for `online`. The recreated container reuses the join token (same
  # worker id), and the old cap-1 row can still read `online` for a beat before the
  # fresh register overwrites max_concurrent_runs, so gating on status alone races.
  cap_deadline=$((SECONDS + 40)); CAP=""
  while [ $SECONDS -lt $cap_deadline ]; do
    W0="$(apiget /api/workers | jq -c '.workers[0]')"
    CAP="$(echo "$W0" | jq -r '.max_concurrent_runs')"
    { [ "$(echo "$W0" | jq -r '.status')" = online ] && [ "$CAP" = 2 ]; } && break
    sleep 0.3
  done
  [ "$CAP" = 2 ] || fail "worker did not advertise max_concurrent_runs=2 after recreate (got ${CAP:-none})"
  pass "worker back online advertising cap 2"
  # Before any run exists: the new container wired the sidecar and registered the docker capability.
  docker_wire_assert

  # Entry custody census: this phase must leave no more open holds than it found (asserted at the end).
  E45="$(hold_open_count)" || fail "phase 45: cannot read the entry open custody hold count"
  log_hold_census "phase 45 entry (E45=$E45)"
  wait_custody_headroom 2

  # --- (b) two runs on two DIFFERENT repos, genuinely concurrent ---------------
  say "PRD #42: two runs on two different repos → both reach the gate concurrently (cap-1 could not)"
  IID_A="$(apipost "/api/repos/$REPO_ID/issues" \
    '{"title":"E2E cap2 A (repo)","description":"implements prds/4-agent-runtime-workers.md"}' | jq -r '.card.iid')"
  IID_B="$(apipost "/api/repos/$REPO2_ID/issues" \
    '{"title":"E2E cap2 B (repo2)","description":"implements prds/4-agent-runtime-workers.md"}' | jq -r '.card.iid')"
  { [ -n "$IID_A" ] && [ "$IID_A" != null ] && [ -n "$IID_B" ] && [ "$IID_B" != null ]; } \
    || fail "could not create the two concurrency issues"
  RUN_A="$(create_run "$REPO_ID" "$IID_A")" || fail "cap2 run A run-create failed (non-transient; see stderr)"
  RUN_B="$(create_run "$REPO2_ID" "$IID_B")" || fail "cap2 run B run-create failed (non-transient; see stderr)"
  { [ -n "$RUN_A" ] && [ "$RUN_A" != null ] && [ -n "$RUN_B" ] && [ "$RUN_B" != null ]; } \
    || fail "the two runs were not created"
  # Both park at the gate and HOLD their slot there (Decision 2), so once both
  # arrive they stay — a single combined snapshot then shows both non-terminal at
  # once. At cap 1 the second run would still be `queued` here.
  wait_status "$RUN_A" awaiting_approval
  wait_status "$RUN_B" awaiting_approval
  SA="$(apiget "/api/runs/$RUN_A")"; SB="$(apiget "/api/runs/$RUN_B")"
  { [ "$(echo "$SA" | jq -r '.run.status')" = awaiting_approval ] \
    && [ "$(echo "$SB" | jq -r '.run.status')" = awaiting_approval ]; } \
    || fail "the two runs are not both at the gate simultaneously (no genuine overlap)"
  WID_A="$(echo "$SA" | jq -r '.run.worker_id')"; WID_B="$(echo "$SB" | jq -r '.run.worker_id')"
  { [ -n "$WID_A" ] && [ "$WID_A" != null ] && [ "$WID_A" = "$WID_B" ]; } \
    || fail "the two concurrent runs were not both claimed by the SAME single worker ($WID_A vs $WID_B)"
  { [ "$(echo "$SA" | jq -r '.run.claimed_at')" != null ] \
    && [ "$(echo "$SB" | jq -r '.run.claimed_at')" != null ]; } \
    || fail "a concurrent run has no claimed_at (never got past claimed)"
  pass "both runs simultaneously past claimed + at the gate, on the one worker $WID_A — genuine concurrency"

  # No cross-talk between the two concurrent runs: each plan references its OWN
  # issue and never the sibling's (the stub writes `issue #<iid>` into plan_md, set
  # at the gate). The [^0-9]/end-of-line guard keeps #1 from matching #12, etc.
  PLAN_A="$(echo "$SA" | jq -r '.run.plan_md')"; PLAN_B="$(echo "$SB" | jq -r '.run.plan_md')"
  echo "$PLAN_A" | grep -qE "issue #$IID_A([^0-9]|\$)" || fail "run A's plan does not reference its own issue #$IID_A"
  echo "$PLAN_A" | grep -qE "issue #$IID_B([^0-9]|\$)" && fail "run A's plan references run B's issue #$IID_B (cross-talk)"
  echo "$PLAN_B" | grep -qE "issue #$IID_B([^0-9]|\$)" || fail "run B's plan does not reference its own issue #$IID_B"
  echo "$PLAN_B" | grep -qE "issue #$IID_A([^0-9]|\$)" && fail "run B's plan references run A's issue #$IID_A (cross-talk)"
  pass "no cross-talk: each concurrent run's plan references only its own issue"

  # --- (c) API worker listing: active_runs=2 / cap=2 while both are live -------
  WL="$(apiget /api/workers | jq '.workers[0]')"
  [ "$(echo "$WL" | jq -r '.active_runs')" = 2 ] \
    || fail "worker listing active_runs != 2 while two runs are live (got $(echo "$WL" | jq -r '.active_runs'))"
  [ "$(echo "$WL" | jq -r '.max_concurrent_runs')" = 2 ] || fail "worker listing cap != 2"
  [ "$(echo "$WL" | jq -r '.busy')" = true ] || fail "worker not marked busy with two live runs"
  pass "worker listing shows active_runs=2 / cap=2 (busy) while both runs are live"

  # --- (d) approve both → both land MRs, on independent bare-caches, no cross-talk
  apipost "/api/runs/$RUN_A/inputs" '{"kind":"approve_plan","body":""}' >/dev/null
  apipost "/api/runs/$RUN_B/inputs" '{"kind":"approve_plan","body":""}' >/dev/null
  wait_status "$RUN_A" completed "${UZI_E2E_COMPLETE_TIMEOUT:-$COMPLETE_TIMEOUT_DEFAULT}"
  wait_status "$RUN_B" completed "${UZI_E2E_COMPLETE_TIMEOUT:-$COMPLETE_TIMEOUT_DEFAULT}"
  # Completed precedes the worker's final custody ACK: wait for each run's own receipt so the census
  # returns to the entry baseline before the kill scenario needs admission headroom.
  wait_completed_custody_receipt "$RUN_A"
  wait_completed_custody_receipt "$RUN_B"
  log_run_hold_census "after runs A and B" "$RUN_A" "$RUN_B"
  FA="$(apiget "/api/runs/$RUN_A")"; FB="$(apiget "/api/runs/$RUN_B")"
  [ "$(echo "$FA" | jq -r '.run.branch')" = "agent/issue-$IID_A" ] || fail "run A branch mismatch"
  [ "$(echo "$FB" | jq -r '.run.branch')" = "agent/issue-$IID_B" ] || fail "run B branch mismatch"
  MRA="$(echo "$FA" | jq -r '.run.mr_iid')"; MRB="$(echo "$FB" | jq -r '.run.mr_iid')"
  { [ "$MRA" != null ] && [ "$MRA" -gt 0 ] && [ "$MRB" != null ] && [ "$MRB" -gt 0 ]; } \
    || fail "both concurrent runs must open an MR (got A=$MRA B=$MRB)"
  # Each branch landed on its OWN repo's bare — proving the two runs used
  # independent git caches (repo2's branch is NOT in repo1's bare, and vice versa).
  #
  # This REMOTE-bare independence check holds only for the default (insteadOf) transport,
  # where repo.git and repo2.git are two distinct local bares. Under E2E_GIT_SMART_HTTP
  # forge-fake routes EVERY repo path onto the ONE shared bare (forge-fake.mjs
  # PATH_INFO=/repo.git${rest}), so both branches necessarily land on repo.git and this
  # check is unsatisfiable by construction — the pre-#97-M1 opt-in smart-HTTP full run was
  # already red here (PRD #97 M1 confirm-and-fix). The WORKER-side cache independence — the
  # actual #42 property — still holds under smart-HTTP (repo and repo2 have DISTINCT clone
  # URLs ⇒ distinct worker bare dirs, no per-repo GitCache lock shared) and is proven by the
  # concurrency asserts above (both parked at once on the one worker) plus the per-project
  # MR attribution below. So gate the remote-bare check to the default transport; under
  # smart-HTTP assert only that both branches reached the shared bare.
  if [ -z "${E2E_GIT_SMART_HTTP:-}" ]; then
    git --git-dir="$RUNROOT/fakeremote/repo.git"  show-ref --verify --quiet "refs/heads/agent/issue-$IID_A" \
      || fail "run A branch not on the repo1 bare"
    git --git-dir="$RUNROOT/fakeremote/repo2.git" show-ref --verify --quiet "refs/heads/agent/issue-$IID_B" \
      || fail "run B branch not on the repo2 bare (independent bare-cache check)"
    git --git-dir="$RUNROOT/fakeremote/repo.git"  show-ref --verify --quiet "refs/heads/agent/issue-$IID_B" \
      && fail "run B's branch leaked into the repo1 bare (caches not independent)"
  else
    git --git-dir="$RUNROOT/fakeremote/repo.git" show-ref --verify --quiet "refs/heads/agent/issue-$IID_A" \
      || fail "run A branch not on the shared smart-HTTP bare"
    git --git-dir="$RUNROOT/fakeremote/repo.git" show-ref --verify --quiet "refs/heads/agent/issue-$IID_B" \
      || fail "run B branch not on the shared smart-HTTP bare (forge-fake collapses repo/repo2 — see comment)"
  fi
  FS="$(fake_state)"
  [ "$(echo "$FS" | jq --arg b "agent/issue-$IID_A" '[.mrs[]|select(.source_branch==$b)]|length')" -ge 1 ] \
    || fail "fake recorded no MR for run A's branch"
  # The repo2 MR is attributed to project 2 (the multi-project fake resolves :id).
  [ "$(echo "$FS" | jq --arg b "agent/issue-$IID_B" '[.mrs[]|select(.source_branch==$b)][-1].project_id')" = 2 ] \
    || fail "run B's MR not attributed to forge project 2 (group/repo2)"
  if [ -z "${E2E_GIT_SMART_HTTP:-}" ]; then bare_note="each on its own independent bare"; else bare_note="both on the one shared smart-HTTP bare"; fi
  pass "both runs completed: MRs !$MRA (repo) + !$MRB (repo2, project 2), $bare_note"

  # --- (e) mid-run SIGKILL → sweeper re-queues BOTH (N=2) → restart completes ---
  say "PRD #42: mid-run SIGKILL of the agent with two in-flight runs → sweeper re-queues BOTH → restart completes"
  # The heartbeat-stale window is already tightened to 15s from BOOT (E2E_WORKER_HEARTBEAT_STALE
  # in the env-file), so the sweeper's worker-loss recovery is bounded here with no dedicated
  # api recreate — 15s is still 3× the 5s heartbeat, so a LIVE worker is never spuriously swept.

  wait_custody_headroom 2
  IID_KA="$(apipost "/api/repos/$REPO_ID/issues" \
    '{"title":"E2E cap2 kill A","description":"implements prds/4-agent-runtime-workers.md"}' | jq -r '.card.iid')"
  IID_KB="$(apipost "/api/repos/$REPO2_ID/issues" \
    '{"title":"E2E cap2 kill B","description":"implements prds/4-agent-runtime-workers.md"}' | jq -r '.card.iid')"
  RUN_KA="$(create_run "$REPO_ID" "$IID_KA")" || fail "cap2 kill-scenario run A run-create failed (non-transient; see stderr)"
  RUN_KB="$(create_run "$REPO2_ID" "$IID_KB")" || fail "cap2 kill-scenario run B run-create failed (non-transient; see stderr)"
  { [ -n "$RUN_KA" ] && [ "$RUN_KA" != null ] && [ -n "$RUN_KB" ] && [ "$RUN_KB" != null ]; } \
    || fail "the two kill-scenario runs were not created"
  wait_status "$RUN_KA" awaiting_approval
  wait_status "$RUN_KB" awaiting_approval
  pass "two fresh runs in-flight (both parked at the gate, each holding a slot)"
  GEN_KA_ORIGINAL="$(db_psql "SELECT claim_generation FROM runs WHERE id='$RUN_KA'")"
  GEN_KB_ORIGINAL="$(db_psql "SELECT claim_generation FROM runs WHERE id='$RUN_KB'")"
  # The wired worker seeds attempt-shaped predecessors (attemptId in the journal, a `.attempt-` clone path).
  # Prove that before the kill, so the restore assertions below cannot pass on a canonical predecessor.
  restore_check_p_shape "$RUN_KA" "agent/issue-$IID_KA" attempt || fail "phase 45: predecessor of run KA is not attempt-shaped before the kill"
  restore_check_p_shape "$RUN_KB" "agent/issue-$IID_KB" attempt || fail "phase 45: predecessor of run KB is not attempt-shaped before the kill"

  # Hard-kill the agent: no graceful drain, no re-register — only the server-side
  # sweeper can recover the two orphaned runs. Do NOT restart the worker yet, so
  # the SWEEPER (not the restart's register-time requeue) is what re-queues them.
  # The compose slot identity is the container: the kill/start cycle must restart this same container.
  AGENT_IDENTITY_BEFORE="$(restore_agent_identity)" || fail "phase 45: cannot read the agent container identity before the kill"
  capture_agent_logs 45-bounded-concurrency before-sigkill
  "${COMPOSE[@]}" kill -s KILL agent >/dev/null
  pass "SIGKILL delivered to the agent container (two runs left in-flight)"

  # Both orphaned runs go back to `queued` together — the sweeper's N=2 recovery.
  wait_status "$RUN_KA" queued 60
  wait_status "$RUN_KB" queued 60
  pass "sweeper marked the dead worker offline and re-queued BOTH runs (N=2)"
  # Predecessor snapshots: the agent is stopped, so nothing mutates P between here and the restart.
  restore_snapshot_predecessor "$RUN_KA" "agent/issue-$IID_KA" || fail "phase 45: could not snapshot the killed predecessor of run KA"
  restore_snapshot_predecessor "$RUN_KB" "agent/issue-$IID_KB" || fail "phase 45: could not snapshot the killed predecessor of run KB"

  # Restart the worker (same join token ⇒ same worker id): it re-claims both by
  # affinity and drives them to completion. The exported UZI_WORKER_TOKEN re-sources
  # the `worker_token` secret; no token re-delivery is needed.
  #
  # #3644 restores independently: the successor claim seeds a fresh attempt from the
  # killed predecessor's verified pin and captures nothing. The 90s gate ceiling covers recovery
  # work under CI load; no extra park, promotion or generation is expected.
  capture_agent_logs 45-bounded-concurrency before-restart-after-kill
  "${COMPOSE[@]}" up -d --wait agent >/dev/null
  wait_worker_online
  restore_assert_agent_identity "phase 45 kill/start" "$AGENT_IDENTITY_BEFORE"
  wait_status "$RUN_KA" awaiting_approval 90
  wait_status "$RUN_KB" awaiting_approval 90
  assert_cap2_recovered_run "$RUN_KA" "$GEN_KA_ORIGINAL" "agent/issue-$IID_KA"
  assert_cap2_recovered_run "$RUN_KB" "$GEN_KB_ORIGINAL" "agent/issue-$IID_KB"
  pass "restarted worker restored and re-gated both runs inside one successor claim each (affinity preserved)"

  apipost "/api/runs/$RUN_KA/inputs" '{"kind":"approve_plan","body":""}' >/dev/null
  apipost "/api/runs/$RUN_KB/inputs" '{"kind":"approve_plan","body":""}' >/dev/null
  wait_status "$RUN_KA" completed "${UZI_E2E_COMPLETE_TIMEOUT:-$COMPLETE_TIMEOUT_DEFAULT}"
  wait_status "$RUN_KB" completed "${UZI_E2E_COMPLETE_TIMEOUT:-$COMPLETE_TIMEOUT_DEFAULT}"
  FKA="$(apiget "/api/runs/$RUN_KA")"; FKB="$(apiget "/api/runs/$RUN_KB")"
  [ "$(echo "$FKA" | jq -r '.run.requeue_count')" -ge 1 ] || fail "run KA was never re-queued (requeue_count=0)"
  [ "$(echo "$FKB" | jq -r '.run.requeue_count')" -ge 1 ] || fail "run KB was never re-queued (requeue_count=0)"
  MRKA="$(echo "$FKA" | jq -r '.run.mr_iid')"; MRKB="$(echo "$FKB" | jq -r '.run.mr_iid')"
  { [ "$MRKA" != null ] && [ "$MRKA" -gt 0 ] && [ "$MRKB" != null ] && [ "$MRKB" -gt 0 ]; } \
    || fail "re-queued runs must still land their MRs after the restart (got A=$MRKA B=$MRKB)"
  pass "both re-queued runs completed after restart (requeue_count>=1), MRs !$MRKA + !$MRKB"
  # The successor generation's own custody hold is released at completion: observe that, per run.
  wait_completed_custody_receipt "$RUN_KA"
  wait_completed_custody_receipt "$RUN_KB"
  # The phase created and killed these exact originals. Their preservation was
  # asserted above; now record the fixture owner's decision, keeping ready archives.
  resolve_fixture_source_hold "$RUN_KA" "$GEN_KA_ORIGINAL"
  resolve_fixture_source_hold "$RUN_KB" "$GEN_KB_ORIGINAL"
  log_run_hold_census "after the KA/KB discards" "$RUN_KA" "$RUN_KB"

  # --- ISOLATED CANONICAL CONTROL (#3644) ---------------------------------------------------------
  # Runs only after the sidecar and env line this phase added are gone and the FINAL agent container
  # proved unwired (docker_restore_posture step 4), and only after the KA/KB discards above, so its
  # custody admission and attribution are its own. A fresh issue is a fresh, never-wired branch key: the
  # predecessor is canonical-shaped ({runId, clonePath}), which this release does not restore independently.
  docker_restore_posture
  say "#3644 canonical control: a canonical-shaped predecessor killed mid-gate is refused, not restored"
  wait_custody_headroom 1
  IID_C="$(apipost "/api/repos/$REPO_ID/issues" \
    '{"title":"E2E canonical control","description":"implements prds/4-agent-runtime-workers.md"}' | jq -r '.card.iid')"
  [ -n "$IID_C" ] && [ "$IID_C" != null ] || fail "could not create the canonical control issue"
  RUN_C="$(create_run "$REPO_ID" "$IID_C")" || fail "canonical control run-create failed (non-transient; see stderr)"
  [ -n "$RUN_C" ] && [ "$RUN_C" != null ] || fail "the canonical control run was not created"
  wait_status "$RUN_C" awaiting_approval
  GEN_C_ORIGINAL="$(db_psql "SELECT claim_generation FROM runs WHERE id='$RUN_C'")"
  restore_check_p_shape "$RUN_C" "agent/issue-$IID_C" canonical || fail "phase 45: predecessor of the canonical control is not the canonical {runId, clonePath} shape"
  pass "canonical control run $RUN_C parked at the gate with a canonical-shaped predecessor"
  AGENT_IDENTITY_BEFORE_C="$(restore_agent_identity)" || fail "phase 45: cannot read the agent container identity before the canonical kill"
  capture_agent_logs 45-bounded-concurrency before-sigkill-canonical
  "${COMPOSE[@]}" kill -s KILL agent >/dev/null
  wait_status "$RUN_C" queued 60
  capture_agent_logs 45-bounded-concurrency before-restart-canonical
  "${COMPOSE[@]}" up -d --wait agent >/dev/null
  wait_worker_online
  restore_assert_agent_identity "phase 45 canonical kill/start" "$AGENT_IDENTITY_BEFORE_C"
  wait_status "$RUN_C" recovery_wait 90
  restore_assert_canonical_refusal "$RUN_C" "$((GEN_C_ORIGINAL + 1))" 90
  sleep 6 # three sweep ticks: the hold must not promote or re-claim on its own
  restore_assert_canonical_refusal "$RUN_C" "$((GEN_C_ORIGINAL + 1))" 10
  # Owner Cancel keeps the recovery evidence; then record the fixture owner's decision for both generations.
  apipost "/api/runs/$RUN_C/inputs" '{"kind":"cancel","body":""}' >/dev/null
  wait_status "$RUN_C" cancelled 30
  resolve_fixture_source_hold "$RUN_C" "$GEN_C_ORIGINAL"
  resolve_fixture_source_hold "$RUN_C" "$((GEN_C_ORIGINAL + 1))"
  log_hold_census "after phase 45"
  assert_hold_count_le "phase 45 exit" "$E45"
fi
