# shellcheck shell=bash
# phase:    bounded-concurrency
# title:    PRD #42 bounded-concurrency scenario (stub-only)
# critical: no
# lane:     gitlab
# executor: any
# race-sensitive: yes
# requires: REPO_ID UZI_WORKER_TOKEN UZI_BIN UZI_TOKEN_VAL
# provides: -
# handoff:  -
# mutates:  -
# restores: -
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
#   (e) resumes two cooperatively paused, positively ended sources after restart;
#   (f) refuses two active predecessors after SIGKILL, preserving source/custody,
#       then records explicit fixture-owner disposition.
#
# STATED LIMIT (PRD #42 M5 / review — do not overclaim): the stub executor is
# already concurrency-safe (zero instance-level run state), so this exercises the
# worker loop + server + API-listing path, NOT the M1 per-run executor kill/reap
# isolation fix (per-instance SdkExecutor / runId-scoped killAgentTree). M1's unit
# test (agent/test/) is the guard for that; the stub cannot exercise it.
# Owner Resume mints one ordinary claim without charging worker-loss budget.
assert_cap2_owner_resume() {
  local run="$1" original="$2" requeues="$3" iid="$4" project="$5" view
  view="$(apiget "/api/runs/$run")"
  [ "$(db_psql "SELECT claim_generation FROM runs WHERE id='$run'")" = "$((original + 1))" ] \
    || fail "owner resume generation mismatch"
  printf '%s' "$view" | jq -e --argjson requeues "$requeues" --arg branch "agent/issue-$iid" \
    '.run|.status=="completed" and .requeue_count==$requeues and .branch==$branch and (.mr_iid>0)' >/dev/null \
    || fail "owner resume outcome/branch/requeue mismatch"
  fake_state | jq -e --arg branch "agent/issue-$iid" --argjson project "$project" \
    'any(.mrs[]; .source_branch==$branch and .project_id==$project)' >/dev/null \
    || fail "owner resume MR project mismatch"
}

# One capped log: exact foreign-active refusal on the second and last claim.
assert_cap2_crash_refusal() {
  local run="$1" original="$2" branch="$3" log="$4" view
  view="$(apiget "/api/runs/$run")"
  [ "$(db_psql "SELECT claim_generation FROM runs WHERE id='$run'")" = "$((original + 1))" ] \
    || fail "crash refusal generation mismatch"
  printf '%s' "$view" | jq -e '.run|.status=="failed" and .requeue_count==1 and
    (.failure_reason|contains("Retained recovery blocked: preservation failed") and contains("local work and custody retained"))' >/dev/null \
    || fail "crash refusal failure_reason/budget mismatch"
  jq -Rse --arg run "$run" --arg branch "$branch" --argjson gen "$((original + 1))" '
    [split("\n")[]|sub("^[^{]*";"")|fromjson?] as $events |
    [$events|to_entries[]|select(.value.run_id==$run and .value.msg=="run claimed")] as $claims |
    [$events|to_entries[]|select(.value.run_id==$run and .value.msg=="retained predecessor refused by source-use admission; capture not started"
      and (.value.error|contains("source is active under another worker incarnation")))] as $refusals |
    [$events|to_entries[]|select(.value.run_id==$run and .value.msg=="outbox: terminal journaled write-ahead"
      and .value.claim_generation==$gen and .value.status=="failed")] as $terminals |
    ($claims|length)==2 and ($refusals|length)==1 and ($terminals|length)==1 and
    $claims[1].key<$refusals[0].key and $refusals[0].key<$terminals[0].key and
    all($events[($claims[1].key+1):][];
      (if .run_id==$run then
        (.msg!="recovery: bundle uploaded (durable capture)" and .msg!="recovery: prior open custody holds inventoried after clone"
         and .msg!="stub executor committed marker") else true end) and
      (if .branch==$branch then .msg!="runner clone: seeding" else true end))' "$log" >/dev/null \
    || fail "crash lacks exact foreign-active refusal or captured/reseeded/executed"
}

assert_cap2_crash_bytes() {
  local before="$1" after="$2"
  jq -ne --slurpfile b "$before" --slurpfile a "$after" '
    $b[0] as $b|$a[0] as $a|
    ($b|del(.pins))==($a|del(.pins)) and all($b.pins[]; . as $p|$a.pins|index($p)!=null)' >/dev/null \
    || fail "crash original source bytes/head/journal/pins changed"
}

cap2_early_barrier() {
  local file="$CAP2_SCRATCH/early-barrier"
  fixture_crash_read "$file" apiget "/api/runs/$1/messages" || return 1
  jq -r 'any(.messages[]; .payload.text=="stub: outbox waiting for API cut")' "$file"
}

# Three fresh inspections allow normal local-record cleanup. There is no resolve
# RPC: unresolved records/refusals retain bytes. Only a fresh explicit local
# discard disposition authorizes the guarded client; failures stop this run.
cap2_crash_local_teardown() {
  local run="$1" gen="$2" original="$3" dir="$4" n source disposition attempt generation descriptor rc
  attempt="$(jq -er '.sources[0].attempt' "$original")" || fail "missing original attempt identity"
  for n in 1 2 3; do
    rc=0
    fixture_crash_inspect "$run" "$gen" "$dir/local" "$attempt" || rc=$?
    if [ "$rc" = 2 ]; then
      fail "crashed-active source disappeared before guarded local discard: $run"
    fi
    [ "$rc" = 0 ] || fail "crash original inspection unavailable during local teardown ($rc)"
    source="$(jq -ce --slurpfile old "$original" '.sources[0]|select(.runId==$old[0].sources[0].runId and
      .barePath==$old[0].sources[0].barePath and .branch==$old[0].sources[0].branch and
      .attempt==$old[0].sources[0].attempt and .sources[0].clonePath==$old[0].sources[0].sources[0].clonePath)' "$dir/local")" \
      || fail "local teardown source changed identity"
    disposition="$(printf '%s' "$source" | jq -r '.disposition.kind')"
    case "$disposition" in
      local_discard|local_protected_discard)
        attempt="$(printf '%s' "$source" | jq -r '.attempt')"
        generation="$(printf '%s' "$source" | jq -r '.generation')"
        descriptor="$(printf '%s' "$source" | jq -r '.descriptor')"
        local protected=()
        [ "$disposition" != local_protected_discard ] || protected=(--protected)
        fixture_crash_read "$dir/discard" "${COMPOSE[@]}" exec -T -u worker agent /usr/bin/env -i \
          PATH=/usr/local/bin:/usr/bin:/bin /usr/local/bin/node /app/src/ops/retained-source.mjs discard \
          --run "$run" --generation "$generation" --attempt "$attempt" --confirm "$descriptor" "${protected[@]}" \
          || fail "guarded local discard failed"
        jq -e --argjson source "$source" '.ok==true and .op=="discard" and .result.kind=="done" and
          .result.metadata=="retired" and (.result.sources|length==1) and
          (.result.sources[0]|.clonePath==$source.sources[0].clonePath and (.outcome|IN("removed","missing")))' "$dir/discard" >/dev/null \
          || fail "local discard not acknowledged for exact original path"
        rc=0; fixture_crash_inspect "$run" "$gen" "$dir/local-after" "$attempt" || rc=$?
        [ "$rc" = 2 ] || fail "local source remains after discard"
        fixture_crash_snapshot "$original" "$dir/absent" absent || fail "guarded discard left original bytes"
        jq -e '.absent==true' "$dir/absent" >/dev/null || fail "missing positive original absence"
        pass "crash source guarded local discard observed: $run"
        return ;;
      resolve_local_record|owner_hold_discard|refused) ;;
      *) fail "unknown local crash disposition" ;;
    esac
    [ "$n" = 3 ] || sleep 1
  done
  fixture_crash_snapshot "$original" "$dir/retained" || fail "unresolved crash source bytes unavailable"
  assert_cap2_crash_bytes "$dir/before" "$dir/retained"
  jq -c '{sources:[.sources[]|{runId,generation,attempt,disposition,localRecords,consumers}]}' "$dir/local"
  pass "crash source positively retained after bounded normal cleanup attempts: $run"
}

if [ "$EXECUTOR" != stub ]; then
  say "PRD #42 bounded-concurrency scenario: SKIPPED (stub-only; executor=$EXECUTOR)"
else
  say "PRD #42: reconfigure the worker to WORKER_MAX_CONCURRENT_RUNS=2 and enable a second repo"
  login   # fresh admin session (also re-unlocks the admin vault so claims proceed)

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
  export TMPDIR="$ROOT/.uzi/scratch"
  CAP2_SCRATCH="$(mktemp -d)"
  CAP2_TICKS_SET="${UZI_STUB_OUTBOX_TICKS+x}" CAP2_TICKS="${UZI_STUB_OUTBOX_TICKS-}"
  cap2_restore_ticks() {
    if [ "$CAP2_TICKS_SET" = x ]; then export UZI_STUB_OUTBOX_TICKS="$CAP2_TICKS";
    else unset UZI_STUB_OUTBOX_TICKS; fi
  }
  trap cap2_restore_ticks EXIT
  export UZI_STUB_OUTBOX_TICKS=3
  printf 'UZI_E2E_MAX_CONCURRENT_RUNS=2\n' >> "$ENVFILE"
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
  FA="$(apiget "/api/runs/$RUN_A")"; FB="$(apiget "/api/runs/$RUN_B")"
  [ "$(echo "$FA" | jq -r '.run.branch')" = "agent/issue-$IID_A" ] || fail "run A branch mismatch"
  [ "$(echo "$FB" | jq -r '.run.branch')" = "agent/issue-$IID_B" ] || fail "run B branch mismatch"
  MRA="$(echo "$FA" | jq -r '.run.mr_iid')"; MRB="$(echo "$FB" | jq -r '.run.mr_iid')"
  { [ "$MRA" != null ] && [ "$MRA" -gt 0 ] && [ "$MRB" != null ] && [ "$MRB" -gt 0 ]; } \
    || fail "both concurrent runs must open an MR (got A=$MRA B=$MRB)"
  # Each branch landed on its OWN repo's bare — proving the two runs used
  # independent git caches (repo2's branch is NOT in repo1's bare, and vice versa).
  # Both local-path and smart-HTTP transports select the same independent bares.
  git --git-dir="$RUNROOT/fakeremote/repo.git" show-ref --verify --quiet "refs/heads/agent/issue-$IID_A" \
    || fail "run A branch not on the repo1 bare"
  git --git-dir="$RUNROOT/fakeremote/repo2.git" show-ref --verify --quiet "refs/heads/agent/issue-$IID_B" \
    || fail "run B branch not on the repo2 bare (independent bare-cache check)"
  git --git-dir="$RUNROOT/fakeremote/repo.git" show-ref --verify --quiet "refs/heads/agent/issue-$IID_B" \
    && fail "run B's branch leaked into the repo1 bare (caches not independent)"
  git --git-dir="$RUNROOT/fakeremote/repo2.git" show-ref --verify --quiet "refs/heads/agent/issue-$IID_A" \
    && fail "run A's branch leaked into the repo2 bare (caches not independent)"
  FS="$(fake_state)"
  [ "$(echo "$FS" | jq --arg b "agent/issue-$IID_A" '[.mrs[]|select(.source_branch==$b)]|length')" -ge 1 ] \
    || fail "fake recorded no MR for run A's branch"
  # The repo2 MR is attributed to project 2 (the multi-project fake resolves :id).
  [ "$(echo "$FS" | jq --arg b "agent/issue-$IID_B" '[.mrs[]|select(.source_branch==$b)][-1].project_id')" = 2 ] \
    || fail "run B's MR not attributed to forge project 2 (group/repo2)"
  pass "both runs completed: MRs !$MRA (repo) + !$MRB (repo2, project 2), each on its own independent bare"

  # --- A: cooperative pause, positive source end, restart, owner resume --------
  say "PRD #42: two running owners pause now before worker restart"
  PAUSE_RUNS=() PAUSE_IIDS=() PAUSE_GENS=() PAUSE_REQUEUES=()
  for repo in "$REPO_ID" "$REPO2_ID"; do
    wait_custody_headroom 1
    iid="$(apipost "/api/repos/$repo/issues" '{"title":"E2E cap2 cooperative pause","description":"implements prds/4-agent-runtime-workers.md UZI_STUB_OUTBOX UZI_STUB_OUTBOX_FIRST_CUT_BARRIER UZI_STUB_FIRST_BARRIER_CHECKPOINTABLE_WORK UZI_STUB_SOURCE_END_READY"}' | jq -er '.card.iid')"
    run="$(create_run "$repo" "$iid")" || fail "cap2 pause run-create failed"
    PAUSE_RUNS+=("$run"); PAUSE_IIDS+=("$iid")
    wait_status "$run" awaiting_approval
    PAUSE_GENS+=("$(db_psql "SELECT claim_generation FROM runs WHERE id='$run'")")
    PAUSE_REQUEUES+=("$(apiget "/api/runs/$run" | jq -er '.run.requeue_count')")
    apipost "/api/runs/$run/inputs" '{"kind":"approve_plan","body":""}' >/dev/null
  done
  for i in 0 1; do
    run="${PAUSE_RUNS[$i]}"
    wait_status "$run" running
    wait_eq true 40 "cap2 first-claim early barrier $run" cap2_early_barrier "$run"
  done
  for run in "${PAUSE_RUNS[@]}"; do
    apipost "/api/runs/$run/inputs" '{"kind":"pause","body":"now"}' >/dev/null
  done
  for i in 0 1; do
    run="${PAUSE_RUNS[$i]}"
    wait_status "$run" paused
    wait_fixture_worker_event "$run" "${PAUSE_GENS[$i]}" "stub: source-end ready" \
      || fail "cooperative pause lacks positive exact source end"
  done
  "${COMPOSE[@]}" stop agent >/dev/null
  "${COMPOSE[@]}" up -d --wait agent >/dev/null
  wait_worker_online
  for run in "${PAUSE_RUNS[@]}"; do apipost "/api/runs/$run/resume-now" '{}' >/dev/null; done
  for i in 0 1; do
    run="${PAUSE_RUNS[$i]}"
    wait_status "$run" completed "${UZI_E2E_COMPLETE_TIMEOUT:-$COMPLETE_TIMEOUT_DEFAULT}"
    assert_cap2_owner_resume "$run" "${PAUSE_GENS[$i]}" "${PAUSE_REQUEUES[$i]}" "${PAUSE_IIDS[$i]}" "$((i+1))"
    resolve_fixture_run_source_holds "$run"
  done
  pass "both cooperatively paused runs completed on G+1 without worker-loss requeue charges"

  # --- B: active sources cannot be recovered from a foreign incarnation -------
  say "PRD #42: hard crash preserves two active predecessor sources"
  CRASH_RUNS=() CRASH_IIDS=() CRASH_GENS=()
  for repo in "$REPO_ID" "$REPO2_ID"; do
    wait_custody_headroom 1
    iid="$(apipost "/api/repos/$repo/issues" '{"title":"E2E cap2 active crash","description":"implements prds/4-agent-runtime-workers.md"}' | jq -er '.card.iid')"
    run="$(create_run "$repo" "$iid")" || fail "cap2 crash run-create failed"
    CRASH_RUNS+=("$run"); CRASH_IIDS+=("$iid")
    wait_status "$run" awaiting_approval
    gen="$(db_psql "SELECT claim_generation FROM runs WHERE id='$run'")"
    CRASH_GENS+=("$gen")
    dir="$CAP2_SCRATCH/$run"; mkdir "$dir"
    fixture_worker_log "$dir/seed-log" || fail "crash seed identity log unavailable"
    # Branch is bound by this just-created issue/run; the guarded inspection must
    # independently confirm the resulting selector's run/generation/path.
    attempt="$(jq -Rse --arg branch "agent/issue-$iid" --argjson gen "$gen" '
      [split("\n")[]|sub("^[^{]*";"")|fromjson?|
        select(.msg=="runner clone: seeding" and .branch==$branch)|.path]|unique|
      if length!=1 then error("ambiguous original seed") else .[0] end |
      if contains(".attempt-") then
        capture("\\.attempt-(?<id>[0-9]{8}T[0-9]{6}Z-g"+($gen|tostring)+"-[0-9a-f]{16})$").id
      else "canonical" end' "$dir/seed-log")" || fail "crash original attempt identity unavailable"
    fixture_crash_inspect "$run" "$gen" "$dir/original" "$attempt" || fail "crash source original identity unavailable"
    jq -e 'any(.sources[0].sources[]; .live==true)' "$dir/original" >/dev/null || fail "crash predecessor not positively live"
    fixture_crash_snapshot "$dir/original" "$dir/before" || fail "crash original snapshot unavailable"
    db_psql "SELECT count(*) FROM run_messages WHERE run_id='$run' AND payload->>'text'='stub executor starting'" > "$dir/executions"
    [ "$(cat "$dir/executions")" = 1 ] || fail "crash original executor start missing/ambiguous"
    fixture_crash_read "$dir/holds" uzi_cli run recovery "$run" --json || fail "crash custody snapshot unavailable"
    jq -e --arg run "$run" --argjson gen "$gen" '[.[]|select(.run_id==$run and .generation==$gen and .state=="open")]|length==1' "$dir/holds" >/dev/null \
      || fail "crash original custody missing"
    fixture_worker_event_ready "$run" "$gen" "stub: source-end ready" && fail "active crash already has source end"
  done
  # Keep the worker stopped until the sweeper has requeued BOTH original claims.
  "${COMPOSE[@]}" kill -s KILL agent >/dev/null
  for run in "${CRASH_RUNS[@]}"; do wait_status "$run" queued 60; done
  for run in "${CRASH_RUNS[@]}"; do [ "$(run_status_quick "$run")" = queued ] || fail "sweeper pair not simultaneously queued"; done
  "${COMPOSE[@]}" up -d --wait agent >/dev/null
  wait_worker_online
  for run in "${CRASH_RUNS[@]}"; do wait_status "$run" failed 90; done
  fixture_worker_log "$CAP2_SCRATCH/crash.log" || fail "bounded crash log unavailable"
  for i in 0 1; do
    run="${CRASH_RUNS[$i]}"; gen="${CRASH_GENS[$i]}"; dir="$CAP2_SCRATCH/$run"
    assert_cap2_crash_refusal "$run" "$gen" "agent/issue-${CRASH_IIDS[$i]}" "$CAP2_SCRATCH/crash.log"
    fixture_crash_snapshot "$dir/original" "$dir/after" || fail "crash retained original snapshot unavailable"
    assert_cap2_crash_bytes "$dir/before" "$dir/after"
    [ "$(db_psql "SELECT count(*) FROM run_messages WHERE run_id='$run' AND payload->>'text'='stub executor starting'")" = "$(cat "$dir/executions")" ] \
      || fail "refused crash successor executed the stub"
    fixture_crash_read "$dir/holds-after" uzi_cli run recovery "$run" --json || fail "crash custody recheck unavailable"
    jq -ne --slurpfile b "$dir/holds" --slurpfile a "$dir/holds-after" '
      all($b[0][]; . as $h|any($a[0][]; .id==$h.id and .run_id==$h.run_id and .generation==$h.generation and .state=="open"))' >/dev/null \
      || fail "crash original custody not retained"
    jq -ne --slurpfile b "$dir/holds" --slurpfile a "$dir/holds-after" '
      ([$b[0][]|.captures[]?.id]|sort) == ([$a[0][]|.captures[]?.id]|sort)' >/dev/null \
      || fail "refused crash successor captured work"
    discard_fixture_crash_owner_holds "$run" "$((gen + 1))"
    cap2_crash_local_teardown "$run" "$gen" "$dir/original" "$dir"
  done
  # Prove admission AND fresh source use on each of the same repositories.
  for repo in "$REPO_ID" "$REPO2_ID"; do
    wait_custody_headroom 1
    iid="$(apipost "/api/repos/$repo/issues" '{"title":"E2E cap2 fresh after refused crash","description":"implements prds/4-agent-runtime-workers.md"}' | jq -er '.card.iid')"
    run="$(create_run "$repo" "$iid")" || fail "fresh post-crash run-create failed"
    wait_status "$run" awaiting_approval
    apipost "/api/runs/$run/inputs" '{"kind":"approve_plan","body":""}' >/dev/null
    wait_status "$run" completed "${UZI_E2E_COMPLETE_TIMEOUT:-$COMPLETE_TIMEOUT_DEFAULT}"
    resolve_fixture_run_source_holds "$run"
  done
  cap2_restore_ticks
fi
