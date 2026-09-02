# shellcheck shell=bash
# phase:    bounded-concurrency
# title:    PRD #42 bounded-concurrency scenario (stub-only)
# critical: no
# lane:     gitlab
# executor: any
# race-sensitive: yes
# requires: -
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

  # Recreate the one worker at cap 2. The exported UZI_WORKER_TOKEN still sources the
  # `worker_token` secret, so the recreated container re-reads the same join token.
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

  # Hard-kill the agent: no graceful drain, no re-register — only the server-side
  # sweeper can recover the two orphaned runs. Do NOT restart the worker yet, so
  # the SWEEPER (not the restart's register-time requeue) is what re-queues them.
  "${COMPOSE[@]}" kill -s KILL agent >/dev/null
  pass "SIGKILL delivered to the agent container (two runs left in-flight)"

  # Both orphaned runs go back to `queued` together — the sweeper's N=2 recovery.
  wait_status "$RUN_KA" queued 60
  wait_status "$RUN_KB" queued 60
  pass "sweeper marked the dead worker offline and re-queued BOTH runs (N=2)"

  # Restart the worker (same join token ⇒ same worker id): it re-claims both by
  # affinity and drives them to completion. The exported UZI_WORKER_TOKEN re-sources
  # the `worker_token` secret; no token re-delivery is needed.
  "${COMPOSE[@]}" up -d --wait agent >/dev/null
  wait_worker_online
  wait_status "$RUN_KA" awaiting_approval 60
  wait_status "$RUN_KB" awaiting_approval 60
  pass "restarted worker re-claimed both re-queued runs (affinity) — both back at the gate"

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
fi

