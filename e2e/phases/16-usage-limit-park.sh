# shellcheck shell=bash
# phase:    usage-limit-park
# title:    PRD #35: opt-in park -> sweeper promotes -> resume SKIPS the gate -> completes
# critical: no
# lane:     gitlab
# executor: any
# requires: -
# provides: -
# handoff:  -
# mutates:  -
# restores: -
# =============================================================================
# PRD #35 M6: the usage-limit park, end to end
# =============================================================================
# The park path is unreachable here without a real exhausted subscription, so the
# stub carries UZI_STUB_LIMIT (agent/src/executor.ts) and dies on a simulated limit.
# It fires ONLY on the first attempt, keyed on plan_approved, which is what makes the
# recovery half expressible rather than just the park.
#
# THE CLOCK IS THE SERVER'S, NOT THE TEST'S. retry_not_before is floored at
# `now + jitter` with jitter 60-180s (limitwait.go), so a park here really does wait
# out the server's own computed minimum. Nothing is stubbed away and no test knob
# shortens it — the stub only reports a NEAR-FUTURE reset, which selects the
# reported-reset branch instead of the 15m exponential fallback.
#
# 🔴 PLACEMENT IS LOAD-BEARING: this block needs the LIVE agent container, so it must
# stay AHEAD of the PRD #104 binding phase, which `compose stop`s the agent and never
# restarts it (see that phase's own note). Written at the END of the file first, and
# every scenario died on its very first assertion — `wait_status "$RUN_LW"
# awaiting_approval` timed out with the run still `queued`, because no worker was left
# to claim it (measured 2026-07-28; the harness burned 579s reaching that point). The
# symptom, a run sitting in `queued` forever, reads exactly like a credential fault
# resetting it out of `claimed` — so it points away from its cause. It had never been
# claimed at all.
#
# It also must not be the phase IMMEDIATELY BEFORE PRD #95's steer-queue leg (see the
# PRD #97 M2 placement note below, which owns that constraint) — this block ends with
# a freshly-freed warm worker, exactly the condition that phase is sensitive to. Here,
# M2 sits between them and absorbs it.

# secs_until RUN_JSON FIELD — whole seconds from now until .run.<FIELD>, floored.
#
# The `sub` is not defensive tidying, it is required. jq's `fromdate` is exactly
# strptime("%Y-%m-%dT%H:%M:%SZ") and REJECTS fractional seconds — it does not truncate
# them, it errors the whole filter out: `jq: error (at <stdin>:1): date
# "2026-07-27T22:17:57.70053Z" does not match format "%Y-%m-%dT%H:%M:%SZ"`, observed
# 2026-07-28 on this very field. And it fails INTERMITTENTLY by nature: Go marshals
# RFC3339Nano with trailing zeros stripped, so a stamp landing on a whole second
# carries no fraction and parses fine. A version of this that passed once proves
# nothing. Same trap the PRD #95 steer-margin print documents ~500 lines below, which
# needs sub-second resolution and so reassembles the milliseconds; here whole seconds
# are the unit of every assertion, so dropping the fraction is exact enough.
secs_until() {
  printf '%s' "$1" \
    | jq -r --arg f "$2" '((.run[$f] | sub("\\.[0-9]+";"") | fromdate) - now) | floor'
}

say "PRD #35: opt-in park -> sweeper promotes -> resume SKIPS the gate -> completes"
apiput /api/me/wait-on-limit '{"enabled":true}' >/dev/null || fail "could not set the user's wait-on-limit default"
IID_LW="$(apipost "/api/repos/$REPO_ID/issues" \
  '{"title":"E2E limit park","description":"implements prds/4-agent-runtime-workers.md then hits UZI_STUB_LIMIT"}' | jq -r '.card.iid')"
RUN_LW="$(create_run "$REPO_ID" "$IID_LW")" || fail "limit-park run-create failed"
apiget "/api/runs/$RUN_LW" | jq -e '.run.wait_on_limit == true' >/dev/null \
  || fail "the run did not inherit the user's wait_on_limit default"
wait_status "$RUN_LW" awaiting_approval
apipost "/api/runs/$RUN_LW/inputs" '{"kind":"approve_plan","body":""}' >/dev/null
# The stub throws LimitReachedError after the gate, so the run parks rather than fails.
wait_status "$RUN_LW" limit_wait 120
PARKED="$(apiget "/api/runs/$RUN_LW")"
echo "$PARKED" | jq -e '.run.rate_limit_type == "five_hour"' >/dev/null \
  || fail "parked run did not carry the allowlisted rate_limit_type (got $(echo "$PARKED" | jq -c .run.rate_limit_type))"
echo "$PARKED" | jq -e '.run.limit_wait_count == 1 and (.run.retry_not_before != null)' >/dev/null \
  || fail "parked run did not stamp limit_wait_count / retry_not_before"
# The server's own floor, observed rather than assumed: never sooner than 60s out.
PARK_S="$(secs_until "$PARKED" retry_not_before)"
[ "$PARK_S" -ge 55 ] || fail "retry_not_before is only ${PARK_S}s out; the 60s jitter floor did not apply"
pass "run parked at limit_wait (five_hour, attempt 1), promotion gated ${PARK_S}s out by the server's own jitter floor"

# The feed carries the STRUCTURED kind, not a status line with the facts in prose —
# M4 maps rate_limit_type through a known-value lookup, which prose cannot support.
apiget "/api/runs/$RUN_LW/messages" \
  | jq -e '[.messages[] | select(.kind == "limit_wait")] | length >= 1' >/dev/null \
  || fail "no structured limit_wait feed message"
pass "feed carries the structured limit_wait message"

# Now the recovery. The sweeper promotes once the clock passes; the resume must NOT
# stop at the gate again, which is the PRD's headline criterion.
wait_status "$RUN_LW" completed "$((PARK_S + 180))"
FINAL_LW="$(apiget "/api/runs/$RUN_LW")"
# 🔴 THE GATE-SKIP EVIDENCE. "It completed" is not evidence — a run that re-planned
# and re-gated would also complete, since the harness has no second approver but the
# stub self-approves nothing. What proves the skip is the worker's own feed line, and
# that the plan entered the gate exactly ONCE for this run.
apiget "/api/runs/$RUN_LW/messages" \
  | jq -e '[.messages[] | select(.payload.text // "" | test("skipping the planning turn"))] | length == 1' >/dev/null \
  || fail "the resume did not report skipping the planning turn and the gate"
# COUNTS GATE ENTRIES VIA kind='plan'. `runner.ts` emits exactly one such message as the
# FIRST line of gatePlan(), unconditionally and before any branch, so this count IS the
# number of gatePlan invocations: 1 for the original attempt, 2 if the resume re-gated.
#
# 🔴 It used to read `payload::text LIKE '%awaiting_approval%'`, which is not a weaker
# version of this — it is unmeasurable. `awaiting_approval` is a run STATUS, reported to
# the runs table via /state; it never appears in a run_messages payload. That query
# returned 0 on every run ever recorded here (7 of 7 checked), whether the run gated
# once, twice or never, so it could not discriminate the thing the comment above claims.
# It was also captured and never asserted, which is what hid it: the value only reached a
# `pass` string, so a phase printing "gate markers: 0" read as evidence for two sessions.
# Asserting the OLD query `= 1` would have reddened every green run; asserting it `= 0`
# would have pinned a constant. Both halves had to be fixed together — wiring up an
# assertion on the wrong column is how a vacuous check becomes a confidently wrong one.
# WHAT THIS CATCHES, stated narrowly because the obvious claim is wrong: it is NOT what
# catches a StubExecutor missing the Decision 6b branch. That defect strands the resume at
# awaiting_approval with nobody to approve it, so `wait_status … completed` above times out
# and this line is never reached (measured 2026-07-28). This closes the OTHER hole, the one
# the comment at the top of this block describes: a re-gate that DOES complete — autopilot,
# a future auto-approve, a verdict already queued — satisfies every other assertion here,
# including the completion and `limit_wait_count == 1`. Only the count sees it.
#
# Non-vacuous, and measured rather than assumed: across one full run of this suite the
# per-run `kind='plan'` counts were 0, 1 and 2 (the PRD #41 revise loop produces 2), so `= 1`
# discriminates. The query it replaced returned 0 for EVERY run_messages row in the whole
# database, not merely for this run.
GATES="$(db_psql "SELECT count(*) FROM run_messages WHERE run_id = '$RUN_LW' AND kind = 'plan'")"
[ "$GATES" = 1 ] \
  || fail "the plan entered the gate $GATES time(s), want exactly 1 — the resume re-planned and re-gated instead of skipping past an approval that already happened"
echo "$FINAL_LW" | jq -e '.run.limit_wait_count == 1' >/dev/null \
  || fail "the resumed run parked again (limit_wait_count moved), so the sentinel is not first-attempt-only"
pass "resume skipped the planning turn AND the gate, then completed with zero human intervention (gate entries: $GATES, asserted)"

# --- the exponential fallback, which a poller-disabled deployment uses by default --
say "PRD #35: a park with no reported reset uses the exponential fallback schedule"
# UZI_USAGE_POLL_INTERVAL is 0 in this overlay, so every gauge row classifies stale and
# neither the cross-check nor NextAvailable can contribute — the fallback is this
# deployment's ORDINARY path. Observed by reading the stamp rather than by waiting it
# out: the first-park fallback is 15m, which no one would run.
IID_NR="$(apipost "/api/repos/$REPO_ID/issues" \
  '{"title":"E2E limit noreset","description":"implements prds/4-agent-runtime-workers.md then hits UZI_STUB_LIMIT:noreset"}' | jq -r '.card.iid')"
RUN_NR="$(create_run "$REPO_ID" "$IID_NR")" || fail "noreset run-create failed"
wait_status "$RUN_NR" awaiting_approval
apipost "/api/runs/$RUN_NR/inputs" '{"kind":"approve_plan","body":""}' >/dev/null
wait_status "$RUN_NR" limit_wait 120
NR="$(apiget "/api/runs/$RUN_NR")"
echo "$NR" | jq -e '.run.limit_resets_at == null' >/dev/null \
  || fail "the noreset park reported a reset it should not have"
NR_S="$(secs_until "$NR" retry_not_before)"
# 15m base + 60-180s jitter. Bounded on both sides so a silent switch to the
# reported-reset branch (which would be ~60-180s) fails here.
[ "$NR_S" -ge 840 ] && [ "$NR_S" -le 1140 ] \
  || fail "fallback stamp is ${NR_S}s out; expected the 15m exponential base plus jitter"
pass "unknown reset took the exponential fallback: promotion stamped ${NR_S}s out (15m base + jitter)"

# --- cancel while parked ------------------------------------------------------
say "PRD #35: cancelling a PARKED run takes effect immediately, server-side"
# A parked run keeps its worker_id and that worker keeps heartbeating for other runs,
# so hasLivePoller would have called it live and enqueued a cancel nothing consumes
# until the promotion. server_side=true is the assertion that it did not.
SS_LW="$(apipost "/api/runs/$RUN_NR/inputs" '{"kind":"cancel","body":""}' | jq -r '.server_side')"
[ "$SS_LW" = true ] || fail "cancel of a PARKED run was not applied server-side (got server_side=$SS_LW)"
[ "$(apiget "/api/runs/$RUN_NR" | jq -r '.run.status')" = cancelled ] \
  || fail "parked run did not transition to cancelled immediately"
pass "parked run cancelled immediately, server-side (not deferred to the promotion)"

# --- opt-out gets a better death ----------------------------------------------
say "PRD #35: an opted-out run FAILS with the server-composed explanatory reason"
apiput /api/me/wait-on-limit '{"enabled":false}' >/dev/null || fail "could not clear the wait-on-limit default"
IID_OO="$(apipost "/api/repos/$REPO_ID/issues" \
  '{"title":"E2E limit optout","description":"implements prds/4-agent-runtime-workers.md then hits UZI_STUB_LIMIT"}' | jq -r '.card.iid')"
RUN_OO="$(create_run "$REPO_ID" "$IID_OO")" || fail "opt-out run-create failed"
apiget "/api/runs/$RUN_OO" | jq -e '.run.wait_on_limit == false' >/dev/null \
  || fail "the opted-out run should not carry wait_on_limit"
wait_status "$RUN_OO" awaiting_approval
apipost "/api/runs/$RUN_OO/inputs" '{"kind":"approve_plan","body":""}' >/dev/null
wait_status "$RUN_OO" failed 120
OO="$(apiget "/api/runs/$RUN_OO")"
# The reason is COMPOSED BY THE SERVER from its allowlisted enum (Decision 8). The
# worker sends only the structured type + reset, so this text is the proof the server
# composed it rather than echoing worker prose.
echo "$OO" | jq -r '.run.failure_reason' | grep -qi 'usage limit' \
  || fail "opt-out failure_reason does not name the usage limit (got: $(echo "$OO" | jq -r .run.failure_reason))"
echo "$OO" | jq -r '.run.failure_reason' | grep -qF 'five_hour' \
  || fail "opt-out failure_reason does not name the limit type"
echo "$OO" | jq -e '.run.status == "failed" and .run.limit_wait_count == 0' >/dev/null \
  || fail "an opted-out run must fail without parking"
pass "opted-out run failed with the server-composed reason naming the limit type, and never parked"

