# shellcheck shell=bash
# phase:    run-health
# title:    PRD #47: run-health detection (stall / loop / in-flight suppression)
# critical: no
# lane:     gitlab
# executor: any
# requires: -
# provides: -
# handoff:  -
# mutates:  -
# restores: -
# =============================================================================
# PRD #47 — run-health detection: the sweeper's server-side detector flags a run
# that looks stalled or looping and clears it on resume/exit, and does NOT flag a
# long single in-flight tool call. The stub reproduces the three telemetry shapes
# via UZI_STUB_* sentinels (stub-only). Slack DELIVERY is NOT asserted: the isolated
# stack configures no Slack and there is no Slack fake, so the owner nudge is proven
# at its server seam — the sweeper's health_notified_at stamp (single-writer,
# nudge-worthiness) — read via db_psql. The health flag itself rides the run DTO.
say "PRD #47: run-health detection (stall / loop / in-flight suppression)"
if [ "$EXECUTOR" != stub ]; then
  say "PRD #47 health scenario: SKIPPED (stub-only — UZI_STUB_STALL/LOOP/INFLIGHT are stub sentinels; executor=$EXECUTOR)"
else
login   # fresh admin session re-unlocks the vault for the run claim

# Tighten the stall threshold to its 60s floor so the test doesn't crawl (the stub
# pauses ~95s, above it). The cache invalidates on write, so the detector sees it on
# its next tick. Other signals stay at defaults — a <2min run never trips slow (45m)
# or stuck-queued (10m).
apiput /api/admin/settings '{"settings":{"health_stall_seconds":"60"}}' >/dev/null
pass "health_stall_seconds tightened to 60s for the scenario"

# PRD #97 M6 / #100: run the three health legs CONCURRENTLY instead of serially, so the
# phase's wall clock is ~max(leg) not ~sum(leg). health / health_notified_at are per-run
# (the sweeper's detectRunHealth iterates each run independently and SetRunHealth writes
# one run), so the three legs — on distinct runs from distinct UZI_STUB_* sentinels —
# never interfere. Three concurrent legs need three claim slots at once, so bump the
# worker from the cap 2 it entered on (PRD #42, set earlier and never reverted) to cap 3.
# EXPORT the cap rather than appending to the env-file: a shell env var out-ranks the
# env-file's UZI_E2E_MAX_CONCURRENT_RUNS=2 (compose ranks shell env above --env-file), so
# there is no ambiguity about which of two duplicate keys wins. Reverted (`unset`) after
# the #47 legs finish (below) so the cap-3 window is scoped to this phase; no phase after
# #47 asserts cap==2, but the unset keeps a future one inserted here from inheriting cap 3.
export UZI_E2E_MAX_CONCURRENT_RUNS=3
"${COMPOSE[@]}" up -d --no-deps --force-recreate agent >/dev/null
# Wait for the recreated worker to actually ADVERTISE cap 3, not merely `online`: the
# reused join token means the old cap-2 row can still read online for a beat before the
# fresh register overwrites max_concurrent_runs — the same race the #42 cap-2 wait guards.
cap_deadline=$((SECONDS + 40)); H47_CAP=""
while [ $SECONDS -lt $cap_deadline ]; do
  h47_w0="$(apiget /api/workers | jq -c '.workers[0]')"
  H47_CAP="$(echo "$h47_w0" | jq -r '.max_concurrent_runs')"
  { [ "$(echo "$h47_w0" | jq -r '.status')" = online ] && [ "$H47_CAP" = 3 ]; } && break
  sleep 0.3
done
[ "$H47_CAP" = 3 ] || fail "worker did not advertise max_concurrent_runs=3 after recreate (got ${H47_CAP:-none})"
pass "worker back online advertising cap 3 (room for three concurrent health legs)"

# hrun SENTINEL — create a PRD issue carrying the sentinel, start a run, approve the
# plan gate, and echo the run id (stdout is only the id: the helpers it calls are
# silent on success).
hrun() {
  local iid run
  iid="$(apipost "/api/repos/$REPO_ID/issues" \
    "$(jq -cn --arg s "$1" '{title:"E2E health",description:("implements prds/47-loop-hang-detection.md " + $s)}')" | jq -r '.card.iid')"
  run="$(create_run "$REPO_ID" "$iid")" || fail "hrun: run-create failed for sentinel '$1' (non-transient; see stderr)" >&2
  wait_status "$run" awaiting_approval
  apipost "/api/runs/$run/inputs" '{"kind":"approve_plan","body":""}' >/dev/null
  echo "$run"
}
# notified_at RUN — the sweeper's nudge-worthiness stamp (health_notified_at), or ''.
notified_at() { db_psql "SELECT COALESCE(to_char(health_notified_at, 'YYYYMMDDHH24MISSUS'), '') FROM runs WHERE id = '$1'"; }

# Create + approve all three runs UP FRONT, serially (fast: create issue, claim, gate,
# approve). This setup step alone needs the cap-3 worker — each approved run holds its
# slot past the gate (PRD #42 Decision 2) and stays running while the stub holds ~95s, so
# the third hrun can only reach the gate if a third slot exists.
#
# ORDER MATTERS: the legs only start observing AFTER all three runs exist, so a run
# created earlier ages by the create+approve time of every run made after it. STALL and
# LOOP carry a time-boxed transient state (leg_loop's `wait_health looping 60`, leg_stall's
# stalled window) that CPU contention can push past its window before the leg polls, so
# create them LAST to minimise their aging — LOOP (tightest 60s ceiling) truly last. The
# in-flight run has no transient window (its leg only asserts stalled NEVER appears), so it
# absorbs the up-front aging and is created first.
RUN_IF="$(hrun UZI_STUB_INFLIGHT)"
RUN_ST="$(hrun UZI_STUB_STALL)"
RUN_LP="$(hrun UZI_STUB_LOOP)"
pass "three health runs created + approved on the cap-3 worker (stall=$RUN_ST loop=$RUN_LP inflight=$RUN_IF)"

# --- (a) STALL → flagged stalled, nudged once, self-clears on resume -----------
leg_stall() {
  # Backgrounded with & → runs in its own subshell. Clear the EXIT trap so a `fail` in
  # here can NEVER run the parent's teardown (docker compose down -v); the parent
  # aggregates each leg's outcome via `wait`. Bash already resets traps in a subshell,
  # so this is explicit belt-and-braces given how destructive that teardown is.
  trap - EXIT
  local run="$1" na1 na2
  say "PRD #47 (a): a run that goes quiet is flagged stalled, nudged once, and self-clears on resume"
  wait_status "$run" running 60
  wait_health "$run" stalled 120
  pass "run $run flagged stalled"
  # The owner nudge fired at its seam: the sweeper stamped health_notified_at.
  na1="$(notified_at "$run")"
  [ -n "$na1" ] || fail "health_notified_at not stamped — the nudge-worthiness seam did not fire"
  # And exactly once per window: after more sweep ticks (still stalled) it is unchanged.
  # 6s spans ~3 ticks at SWEEP_INTERVAL=2s (was sleep 18 at the 15s default). The stamp
  # is written only on an ok→flagged transition (workersvc/health.go), never while a run
  # stays continuously flagged, so any wait covering >=1 further tick proves once-per-window.
  sleep 6
  na2="$(notified_at "$run")"
  [ "$na1" = "$na2" ] || fail "health_notified_at re-stamped while still stalled (want one nudge/window): '$na1' -> '$na2'"
  pass "nudge stamped exactly once while stalled (Slack DELIVERY not asserted — no Slack fake in the isolated stack)"
  # The stub resumes → activity bump → self-clear back to ok while STILL running.
  wait_health "$run" ok 60
  pass "flag self-cleared on resume"
  wait_status "$run" completed "${UZI_E2E_COMPLETE_TIMEOUT:-$COMPLETE_TIMEOUT_DEFAULT}"
  [ "$(apiget "/api/runs/$run" | jq -r '.run.health')" = ok ] || fail "completed run still carries a health flag"
  pass "run completed with health=ok (exit contract)"
}

# --- (b) LOOP → flagged looping, clears on exit --------------------------------
leg_loop() {
  trap - EXIT   # see leg_stall: no parent teardown from a backgrounded leg
  local run="$1"
  say "PRD #47 (b): a run repeating the same tool call is flagged looping"
  wait_health "$run" looping 60
  pass "run $run flagged looping"
  wait_status "$run" completed "${UZI_E2E_COMPLETE_TIMEOUT:-$COMPLETE_TIMEOUT_DEFAULT}"
  [ "$(apiget "/api/runs/$run" | jq -r '.run.health')" = ok ] || fail "completed looped run still flagged"
  pass "looped run completed with health=ok"
}

# --- (c) IN-FLIGHT NEGATIVE: a long single tool call is NOT flagged stalled -----
leg_inflight() {
  trap - EXIT   # see leg_stall: no parent teardown from a backgrounded leg
  local run="$1" hif if_end
  say "PRD #47 (c): a long single in-flight tool call is NOT flagged stalled (suppression)"
  wait_status "$run" running 60
  # Poll across the 60s stall threshold with margin (threshold 60 + a sweep tick +
  # slack), still under the ~95s stub hold: health must never read stalled while the
  # one tool call is still open (no matching tool_result yet), so a broken suppression
  # cannot slip through a too-short poll window.
  if_end=$((SECONDS + 90))
  while [ $SECONDS -lt $if_end ]; do
    hif="$(apiget "/api/runs/$run" | jq -r '.run.health')"
    [ "$hif" = stalled ] && fail "a long in-flight tool call was wrongly flagged stalled"
    sleep 5
  done
  pass "in-flight tool call was never flagged stalled across the threshold"
  wait_status "$run" completed "${UZI_E2E_COMPLETE_TIMEOUT:-$COMPLETE_TIMEOUT_DEFAULT}"
  pass "in-flight run completed cleanly"
}

# Run the three legs concurrently. Each leg's stdout+stderr goes to its own file so the
# streams don't interleave; then surface them in a stable order and fail the phase if any
# leg failed (a `fail` inside a leg exits that subshell non-zero, which `wait` reports).
say "PRD #47: the three health legs run CONCURRENTLY (per-run health, cap-3 worker)"
leg_stall    "$RUN_ST" > "$RUNROOT/h47-stall.log"    2>&1 &  H47_ST=$!
leg_loop     "$RUN_LP" > "$RUNROOT/h47-loop.log"     2>&1 &  H47_LP=$!
leg_inflight "$RUN_IF" > "$RUNROOT/h47-inflight.log" 2>&1 &  H47_IF=$!
h47_fail=0
wait "$H47_ST" || h47_fail=1
wait "$H47_LP" || h47_fail=1
wait "$H47_IF" || h47_fail=1
cat "$RUNROOT/h47-stall.log" "$RUNROOT/h47-loop.log" "$RUNROOT/h47-inflight.log"
[ "$h47_fail" -eq 0 ] || fail "PRD #47: one or more health legs failed (see the per-leg output above)"
pass "PRD #47: all three health legs passed concurrently (stall / loop / in-flight suppression)"

# Restore the default threshold so nothing downstream inherits the tightened value.
apiput /api/admin/settings '{"settings":{"health_stall_seconds":"300"}}' >/dev/null
# Revert the cap-3 export so the cap-3 window is scoped to THIS health phase only. The
# export out-ranks the env-file's UZI_E2E_MAX_CONCURRENT_RUNS=2, and unsetting it (rather
# than exporting =2) restores that env-file value for the NEXT agent --force-recreate
# (PRD #83 M2, further down). No recreate needed here — nothing between now and there
# asserts on the cap; this just stops a future phase inserted here from silently inheriting
# cap 3 when it expects cap 2.
unset UZI_E2E_MAX_CONCURRENT_RUNS
fi

