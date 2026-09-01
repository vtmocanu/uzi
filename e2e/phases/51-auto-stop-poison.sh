# shellcheck shell=bash
# phase:    auto-stop-poison
# title:    PRD #108 M5: auto-stop kills a run whose messages can't be saved (direct-to-API poison)
# critical: no
# lane:     gitlab
# executor: any
# race-sensitive: yes
# requires: -
# provides: -
# handoff:  -
# mutates:  -
# restores: -
# --- PRD #108 M5: auto-stop kills a run whose messages can't be saved ------------
# The real stub agent cannot prove this end-to-end: it never emits a permanently-
# unstorable payload and its in-flight hold is not abort-aware, so it cannot poison
# ITSELF. So this phase mints a SYNTHETIC worker and drives the whole /api/worker/*
# protocol by hand with curl (the PRD #104 binding phase above is the proven pattern
# for a Bearer-token worker the api cannot tell from a container). The agent
# container was `compose stop`ped by the binding phase, so this worker is the ONLY
# claimant — and the binding phase's own three claimed runs were just terminated (see
# the cleanup above), so the two runs we create are the only queued rows for the admin.
#
# Mechanism the asserts below depend on (workersvc/autostop.go + persistfail.go +
# health.go), so the failure messages name a cause that was actually measured:
#   * Poison {"n":1e1000000} overflows numeric on insert = SQLSTATE 22003 =
#     ErrUnstorableMessage (killable class); the messages route answers 400. Each 400
#     advances run A's persist-fail streak, which never decays here (same seq, same
#     class every time = no reset).
#   * health='looping' needs streak>=5 sustained >=10s; its reason is a fixed string.
#   * The KILL needs streak>=20 AND window>=60s AND a killable class AND
#     peersSucceeding>=1 (another RUNNING run that persisted a message in the trailing
#     60s — run B) AND a live poller (the run's worker heartbeat fresh within
#     E2E_WORKER_HEARTBEAT_STALE=15s, set earlier in the suite). A live poller gets a
#     cancel verdict enqueued; unconsumed (we never poll inputs) it ESCALATES at +60s
#     to a server-side FailRunAutoStop -> status='failed', stop_kind='auto_stopped'.
#     The sweep ticks every 2s now (SWEEP_INTERVAL=2s from boot, PRD #97 M6 / #100), not
#     the shipped 15s. That cadence does NOT set the journey length, though: it is floored
#     by two FIXED 60s server-side timers — autoStopWindow (streak sustained >=60s) then
#     autoStopEscalateAfter (+60s after the verdict is enqueued), workersvc/autostop.go —
#     plus the harness poison-loop-driven streak build, so the whole journey is ~120-140s.
#     The 2s cadence only removes the up-to-two-15s-tick detection slack that made the old
#     figure 135-165s; it does not shorten the 120s of fixed timers.
#   * peersSucceeding is strict recency AND re-checked EVERY sweep before the
#     escalation branch, and a run whose worker heartbeat goes stale is REQUEUED out
#     of running (evicting its streak). So BOTH run B's valid appends AND the worker
#     heartbeat must be sustained on a ~3-5s cadence the WHOLE time — which is why the
#     single loop below drives poison + heartbeat + peer together and never blocks.
say "PRD #108 M5: auto-stop kills a run whose messages can't be saved (direct-to-API poison)"
login  # fresh admin session: unlocks the vault so a claim can assemble its payload

# Mint the synthetic worker's join token, register it, and land one heartbeat.
ASW="$(apipost /api/workers '{"name":"e2e-autostop"}')"
WTOK="$(printf '%s' "$ASW" | jq -r '.token')"
{ [ -n "$WTOK" ] && [ "$WTOK" != null ]; } || fail "auto-stop phase: could not mint the synthetic worker token"
curl -fsS -X POST "$BASE/api/worker/register" -H "Authorization: Bearer $WTOK" \
  -H 'Content-Type: application/json' -d '{"version":"0.9.0-e2e","max_concurrent_runs":2}' >/dev/null \
  || fail "auto-stop phase: synthetic worker register failed"
curl -fsS -X POST "$BASE/api/worker/heartbeat" -H "Authorization: Bearer $WTOK" \
  -H 'Content-Type: application/json' -d '{}' >/dev/null \
  || fail "auto-stop phase: synthetic worker first heartbeat failed"
apiget /api/workers | jq -e '[.workers[] | select(.status == "online")] | length >= 1' >/dev/null \
  || fail "auto-stop phase: the synthetic worker did not show online after register+heartbeat"

# Worker-protocol curl helpers, all Bearer-authenticated as the synthetic worker.
# hb/peer_ok/poison_a set a global-free flow (they fail loudly), so they are safe to
# call from the main loop; asw_claim is the one that runs inside $(...) so it reports
# to stderr and returns non-zero rather than calling `fail` (which would exit only the
# subshell and be captured instead of printed — see claim_token above).
hb() { curl -fsS -X POST "$BASE/api/worker/heartbeat" -H "Authorization: Bearer $WTOK" \
  -H 'Content-Type: application/json' -d '{}' >/dev/null || fail "auto-stop phase: heartbeat POST failed"; }
# asw_claim — bounded-retry the claim, echo the claimed run id. TWO independent causes
# make a claim transiently 204 here, so the window must outlast BOTH:
#   1. the create->claimable FOR UPDATE SKIP LOCKED window (a real race), and
#   2. PRD #216 fleet-aware spread. The agent container was `compose stop`ped by the
#      PRD #104 phase, but its worker ROW stays a LIVE spread peer until its last
#      heartbeat ages past WORKER_HEARTBEAT_STALE (E2E_WORKER_HEARTBEAT_STALE=15s here).
#      The spread defers a run to a strictly-less-loaded live peer, so our FIRST claim
#      (at 0 active runs, never deferred) succeeds immediately, but our SECOND claim is
#      DEFERRED to that stopped-but-still-"live" agent worker — which never claims it —
#      and 204s until the peer finally goes stale. So we poll past the 15s stale window
#      (a stopped worker is GUARANTEED to age out; this is waiting on a deterministic
#      expiry, not a flaky race) AND heartbeat our OWN worker each idle iteration so it
#      does not itself go stale while we wait. 30s gives comfortable margin over 15s.
asw_claim() {
  local raw code body tries=0
  while [ "$tries" -lt 30 ]; do
    tries=$((tries + 1))
    raw="$(curl -sS -w $'\n%{http_code}' -X POST "$BASE/api/worker/runs/claim" \
      -H "Authorization: Bearer $WTOK")"
    code="${raw##*$'\n'}"; body="${raw%$'\n'*}"
    case "$code" in
      200) printf '%s' "$body" | jq -r '.run_id'; return 0 ;;
      204) curl -fsS -X POST "$BASE/api/worker/heartbeat" -H "Authorization: Bearer $WTOK" \
             -H 'Content-Type: application/json' -d '{}' >/dev/null 2>&1 || true
           sleep 1; continue ;;
      *) echo "auto-stop claim returned HTTP $code, not 200" >&2; return 1 ;;
    esac
  done
  echo "auto-stop claim still 204 after 30 tries — no run became claimable (did the stopped agent worker never age past the 15s spread-liveness window?)" >&2
  return 1
}
# asw_running — drive a claimed run to running (allowed with no plan gate). The field
# is `status`, NOT `state`. Assert 200 so a rejected transition surfaces here.
asw_running() {
  local code
  code="$(curl -sS -o /dev/null -w '%{http_code}' -X POST "$BASE/api/worker/runs/$1/state" \
    -H "Authorization: Bearer $WTOK" -H 'Content-Type: application/json' -d '{"status":"running"}')"
  [ "$code" = 200 ] || fail "auto-stop phase: driving run $1 claimed->running returned HTTP $code, not 200"
}

# Two runs: A is the poison target, B is the peer whose successful appends keep
# peersSucceeding>=1. create_run returns the exact ids, so the claim assertion below
# can prove the queue is clean rather than guessing.
IID_ASA="$(apipost "/api/repos/$REPO_ID/issues" \
  '{"title":"E2E autostop A (poison)","description":"implements prds/108-worker-retry-loop-autostop.md"}' | jq -r '.card.iid')"
IID_ASB="$(apipost "/api/repos/$REPO_ID/issues" \
  '{"title":"E2E autostop B (peer)","description":"implements prds/108-worker-retry-loop-autostop.md"}' | jq -r '.card.iid')"
RUN_A="$(create_run "$REPO_ID" "$IID_ASA")" || fail "auto-stop phase: could not create run A (non-transient; see stderr)"
RUN_B="$(create_run "$REPO_ID" "$IID_ASB")" || fail "auto-stop phase: could not create run B (non-transient; see stderr)"
{ [ -n "$RUN_A" ] && [ "$RUN_A" != null ] && [ -n "$RUN_B" ] && [ "$RUN_B" != null ]; } \
  || fail "auto-stop phase: the two runs were not created"

# Claim TWICE. The claim is user-scoped, so the two claims return our two queued runs
# in some order; assert each id is one of {A,B} and that they differ. Any other id is
# a dirty queue (a leftover queued run from an earlier phase) — fail loudly, it should
# not happen at end-of-file. We already know A vs B from create_run, so no remapping
# is needed; this only proves both A and B were the runs claimed (and thus owned).
C1="$(asw_claim)" || fail "auto-stop phase: first claim failed (see stderr)"
C2="$(asw_claim)" || fail "auto-stop phase: second claim failed (see stderr)"
for c in "$C1" "$C2"; do
  case "$c" in
    "$RUN_A"|"$RUN_B") ;;
    *) fail "auto-stop phase: a claim returned run '$c', neither A ($RUN_A) nor B ($RUN_B) — the queue is dirty at end-of-file" ;;
  esac
done
[ "$C1" != "$C2" ] || fail "auto-stop phase: both claims returned the same run id '$C1'"
asw_running "$RUN_A"
asw_running "$RUN_B"
pass "synthetic worker claimed both runs and drove them to running"

# peer_ok — POST the next MONOTONIC valid message to run B (a successful insert marks
# B in the peersSucceeding comparison set). A high, ever-increasing seq never collides.
PEER_SEQ=900100
peer_ok() {
  local code
  code="$(curl -sS -o /dev/null -w '%{http_code}' -X POST "$BASE/api/worker/runs/$RUN_B/messages" \
    -H "Authorization: Bearer $WTOK" -H 'Content-Type: application/json' \
    -d "{\"messages\":[{\"seq\":$PEER_SEQ,\"kind\":\"tool_result\",\"payload\":{\"text\":\"peer ok\"}}]}")"
  case "$code" in 200|204) ;; *) fail "auto-stop phase: peer append to run B returned HTTP $code, not 2xx" ;; esac
  PEER_SEQ=$((PEER_SEQ + 1))
}
# poison_a — POST one permanently-unstorable payload to run A; assert 400. The WHOLE
# body is a literal (never jq — jq would reformat 1e1000000 and defeat the overflow).
# Reusing seq 900001 every time is deliberate: a failed insert commits nothing, and a
# non-advancing seq is exactly what keeps the streak from resetting.
poison_a() {
  local code
  code="$(curl -sS -o /dev/null -w '%{http_code}' -X POST "$BASE/api/worker/runs/$RUN_A/messages" \
    -H "Authorization: Bearer $WTOK" -H 'Content-Type: application/json' \
    -d '{"messages":[{"seq":900001,"kind":"tool_result","payload":{"n":1e1000000}}]}')"
  [ "$code" = 400 ] || fail "auto-stop phase: poison POST to run A returned HTTP $code, not 400 (expected ErrUnstorableMessage)"
}
hb; peer_ok  # seed the heartbeat and peer clocks before the loop's first sleep

# THE MAIN LOOP — poison A, heartbeat, and append to B together on a ~3s cadence
# (safely under the 15s stale window and the 60s peer window). Stop poisoning once the
# streak is past the kill threshold (it does not decay). The health-flag assertion is
# FOLDED IN rather than delegated to wait_health, because wait_health blocks WITHOUT
# heartbeating and A would be requeued out from under us before the flag ever landed.
AS_DEADLINE=$((SECONDS + 240))
POISON_N=0
FLAG_SEEN=0
A_STATUS=""
while [ "$SECONDS" -lt "$AS_DEADLINE" ]; do
  if [ "$POISON_N" -lt 22 ]; then poison_a; POISON_N=$((POISON_N + 1)); fi
  hb; peer_ok
  if [ "$FLAG_SEEN" = 0 ] && [ "$POISON_N" -ge 5 ]; then
    if [ "$(run_health "$RUN_A")" = looping ]; then
      HR="$(apiget "/api/runs/$RUN_A" | jq -r '.run.health_reason')"
      [ "$HR" = "the agent's updates can't be saved, so it keeps resending them" ] \
        || fail "auto-stop phase: run A is looping but health_reason is unexpected: '$HR'"
      pass "run A flipped health='looping' with the persist-failing reason"
      FLAG_SEEN=1
    fi
  fi
  A_STATUS="$(apiget "/api/runs/$RUN_A" | jq -r '.run.status')"
  [ "$A_STATUS" = failed ] && break
  sleep 3
done
[ "$A_STATUS" = failed ] \
  || fail "auto-stop phase: run A was never auto-stopped within ~240s (status=$A_STATUS, health=$(run_health "$RUN_A"))"
[ "$FLAG_SEEN" = 1 ] \
  || fail "auto-stop phase: run A reached failed but never surfaced health='looping' first"
pass "run A completed the flag->kill journey and reached status='failed'"

# The kill's signature: failed AND stop_kind='auto_stopped' (an auto-stop, not a plain
# failure). failure_reason is deliberately NOT asserted.
AS_A="$(apiget "/api/runs/$RUN_A")"
[ "$(printf '%s' "$AS_A" | jq -r '.run.stop_kind')" = auto_stopped ] \
  || fail "auto-stop phase: run A failed but stop_kind is not 'auto_stopped' (stop_kind=$(printf '%s' "$AS_A" | jq -r '.run.stop_kind'))"
pass "run A: status='failed' AND stop_kind='auto_stopped' — the auto-stop KILL landed"

# Run B must not be collateral: the kill is scoped to the wedged run, not the fleet.
AS_B="$(apiget "/api/runs/$RUN_B")"
[ "$(printf '%s' "$AS_B" | jq -r '.run.status')" != failed ] \
  || fail "auto-stop phase: peer run B was collaterally failed"
[ "$(printf '%s' "$AS_B" | jq -r '.run.stop_kind')" != auto_stopped ] \
  || fail "auto-stop phase: peer run B was collaterally auto-stopped"
pass "peer run B is not collateral: neither failed nor auto_stopped"

# The two remedies docs/run-auto-stopped.md prints for an auto-stopped run must actually
# be followable from the CLI, so assert both directly (the issue's "Also worth folding
# in"). Reuses the PRD #97 M2 CLI leg's globals (uzi_cli/$UZI_BIN/$UZI_TOKEN_VAL); that
# leg fails the suite if the CLI cannot be built, so reaching here guarantees $UZI_BIN.
#   * Remedy "`uzi run get <id>` shows it": the run DTO printed by `run get --json` carries
#     stop_kind=auto_stopped at top level (see api/cmd/uzi/run.go — JSON(run)).
#   * Remedy "check the worker's version": `uzi worker list` prints a VERSION column
#     (added in api/cmd/uzi/worker.go for exactly this remedy) and the row for our
#     synthetic worker carries the 0.9.0-e2e it registered with. grep -qF is literal:
#     ugrep mishandles some regex on this host and the version's `.` is a regex any-char.
[ -x "$UZI_BIN" ] || fail "auto-stop phase: \$UZI_BIN ($UZI_BIN) is not executable — the PRD #97 M2 CLI leg should have built it"
[ "$(uzi_cli run get "$RUN_A" --json | jq -r '.stop_kind')" = auto_stopped ] \
  || fail "auto-stop phase: doc remedy 'uzi run get <id>' does not surface stop_kind=auto_stopped for run A"
pass "doc remedy proven: 'uzi run get $RUN_A --json' shows stop_kind=auto_stopped"
WL="$(uzi_cli worker list)" || fail "auto-stop phase: doc remedy 'uzi worker list' failed to run (exit $?)"
printf '%s' "$WL" | grep -qF VERSION \
  || fail "auto-stop phase: 'uzi worker list' output lacks a VERSION header (doc remedy 'check the worker's version' is unfollowable): $WL"
printf '%s' "$WL" | grep -qF '0.9.0-e2e' \
  || fail "auto-stop phase: 'uzi worker list' does not show the synthetic worker's registered version 0.9.0-e2e: $WL"
pass "doc remedy proven: 'uzi worker list' shows the VERSION column and the worker's 0.9.0-e2e"

# Best-effort cleanup so run B does not linger past the phase. A cleanup failure must
# never redden a phase whose assertions already passed.
apipost "/api/runs/$RUN_B/inputs" '{"kind":"cancel","body":""}' >/dev/null 2>&1 || true

# This line enumerates every phase the run covered, and it is the only place a reader
# who did not watch the output learns what was in it — so a phase that lands without
# being named here is invisible in exactly the summary people quote. PRD #98 was missing:
# its M8c printed-instruction phase landed at 4b94f714 without touching this line.
