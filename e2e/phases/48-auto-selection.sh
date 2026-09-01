# shellcheck shell=bash
# phase:    auto-selection
# title:    PRD #111 M6: an auto worker picks the emptiest pooled token, and degrades safely
# critical: no
# lane:     gitlab
# executor: any
# requires: -
# provides: -
# handoff:  -
# mutates:  -
# restores: -
# ---------------------------------------------------------------------------
# PRD #111 M6: auto-selection, end to end through the REAL claim path.
#
# What no lower layer can show. autoselect's unit tests own the ranking; the store
# integration test owns the query and the differential. What only this can prove is
# that a worker set to `auto` reaches ListAutoSelectCandidates, ranks, opens the token
# the ranker chose rather than the owner's default, and that the choice comes back out
# on the run DTO — the whole chain, with a real Postgres, a real worker and a real
# claim.
#
# 🔴 UZI_AUTOSELECT_MAX_STALENESS IS PINNED IN THE OVERLAY, and that is a deliberate
# departure from "the harness exercises the SHIPPED defaults". The default is
# 3 x UZI_USAGE_POLL_INTERVAL, and this overlay disables the poller, so the default
# computes to ZERO — nothing is ever fresh and auto can only ever fall back. That is
# correct behaviour (R2), but it makes the HAPPY PATH untestable in this stack: the
# two cases are the same knob, so one stack cannot have both "the default is 0" and "a
# fresh reading is fresh".
#
# So the knob is pinned here and BOTH paths are driven by synced_at instead, which
# exercises the actual freshness mechanism rather than a side effect of the interval.
# The default itself — poll interval 0 => max staleness 0 — is a pure function of the
# environment and is pinned exactly in config's own unit test, which is where an
# arithmetic default belongs.
say "PRD #111 M6: an auto worker picks the emptiest pooled token, and degrades safely"

AS_ADMIN_ID="$ADMIN_ID"
AS_DEFAULT_SECRET="$ADMIN_SECRET_ID"

# A SECOND credential, so there is something to choose between. Created through the
# real cookie-only mint route (PRD #104 D8), not seeded into the database.
AS_SPARE_SECRET="$(apipost /api/me/secrets/anthropic_token \
  '{"label":"spare-key","token":"sk-ant-e2e-dummy-spare-000000000000"}' | jq -r '.secret.id')"
[ -n "$AS_SPARE_SECRET" ] && [ "$AS_SPARE_SECRET" != null ] \
  || fail "could not mint the second Anthropic token for the auto-selection phase"

# Opt BOTH into the pool through D13's narrow route. The pool is empty by default, so
# without this the phase would assert pool_empty twice and prove nothing.
for sid in "$AS_DEFAULT_SECRET" "$AS_SPARE_SECRET"; do
  apipatch "/api/me/secrets/anthropic_token/$sid/auto-eligible" '{"auto_eligible":true}' >/dev/null \
    || fail "could not opt token $sid into the auto-selection pool"
done

# The DEFAULT is nearly exhausted; the SPARE is nearly empty. The point of the fixture
# is that the two answers DIFFER: a selector that ignored the gauge, or that just
# returned the owner default, produces the default token here and is caught.
as_gauge() {  # as_gauge SECRET_ID FIVE_PCT SEVEN_PCT SYNCED_SQL
  db_psql "INSERT INTO anthropic_rate_limits
             (user_secret_id, user_id, five_hour_pct, five_hour_resets_at,
              seven_day_pct, seven_day_resets_at, source, synced_at)
           VALUES ('$1', '$AS_ADMIN_ID', $2, now() + interval '1 hour',
                   $3, now() + interval '3 days', 'usage_endpoint', $4)
           ON CONFLICT (user_secret_id) DO UPDATE SET
             five_hour_pct = $2, seven_day_pct = $3, synced_at = $4" >/dev/null
}
as_gauge "$AS_DEFAULT_SECRET" 92 40 "now()"   # headroom 8
as_gauge "$AS_SPARE_SECRET"    5  5 "now()"   # headroom 95

# The eligibility the SERVER computes, which is the same predicate the selector gates
# on (D21). Asserting it here means a later `auto` failure can be told apart from a
# pool that was never eligible in the first place.
apiget /api/me/rate-limits \
  | jq -e --arg s "$AS_SPARE_SECRET" '[.tokens[] | select(.secret_id == $s)][0]
      | .auto_eligible == true and .auto_status == "eligible"' >/dev/null \
  || fail "the spare token is not reported eligible before the auto run (got: $(apiget /api/me/rate-limits | jq -c '.tokens'))"

AS_WORKER="$(apiget /api/workers | jq -r '.workers[0].id')"
[ -n "$AS_WORKER" ] && [ "$AS_WORKER" != null ] || fail "no worker to switch into auto mode"
apipatch "/api/workers/$AS_WORKER" '{"anthropic_bind_mode":"auto"}' \
  | jq -e '.worker.anthropic_bind_mode == "auto"' >/dev/null \
  || fail "the worker did not switch to auto bind mode"

as_run_credential() {  # as_run_credential RUN_ID -> "<label>|<reason>|<headroom>"
  apiget "/api/runs/$1" | jq -r '.run | "\(.anthropic_secret_label)|\(.anthropic_select_reason)|\(.anthropic_headroom_pct)"'
}

# ── (a) the happy path: the EMPTIER token wins, and the run says so ──────────────
AS_IID="$(apipost "/api/repos/$REPO_ID/issues" \
  '{"title":"E2E autoselect","description":"implements prds/111-auto-select-anthropic-token.md"}' | jq -r '.card.iid')"
AS_RUN="$(create_run "$REPO_ID" "$AS_IID")" || fail "auto-selection run-create failed (non-transient; see stderr)"
wait_status "$AS_RUN" awaiting_approval
AS_GOT="$(as_run_credential "$AS_RUN")"
[ "$AS_GOT" = "spare-key|auto|95" ] || fail \
  "auto run recorded '$AS_GOT', want 'spare-key|auto|95'. The default token has 8 points of headroom and the spare 95, so a selector that ignored the gauge, or that fell through to the owner default, lands on 'default' here"
pass "an auto worker spent the emptiest pooled token (spare-key, auto, 95% headroom)"
apipost "/api/runs/$AS_RUN/inputs" '{"kind":"cancel","body":""}' >/dev/null 2>&1 || true

# ── (b) #754: a stale pool FLOORS onto the best pooled token, NEVER the out-of-pool default ─
# Pre-#754 an auto run whose every reading had aged out fell back to the owner DEFAULT and
# recorded pool_stale. #754 reshapes that ladder: the run floors onto the best POOLED token
# (with NO headroom, since nothing measured it) and still records pool_stale — it never bills
# the non-pooled default. To make that invariant a DETERMINISTIC test rather than an accident
# of which token happens to rank first, opt the DEFAULT out of the pool (leaving only the
# spare pooled), THEN age every reading stale. The sole pooled token is now the stale spare,
# so the floor MUST land on it — a fallback that reached for the out-of-pool default would
# surface as 'default' here and fail. (The default was pooled by (a); this opt-out runs after
# (a) so its happy-path fixture is untouched.)
apipatch "/api/me/secrets/anthropic_token/$AS_DEFAULT_SECRET/auto-eligible" '{"auto_eligible":false}' >/dev/null \
  || fail "could not opt the default token out of the pool for the stale-floor case"
db_psql "UPDATE anthropic_rate_limits SET synced_at = now() - interval '30 days'
         WHERE user_id = '$AS_ADMIN_ID'" >/dev/null
AS_IID2="$(apipost "/api/repos/$REPO_ID/issues" \
  '{"title":"E2E autoselect stale","description":"implements prds/111-auto-select-anthropic-token.md"}' | jq -r '.card.iid')"
AS_RUN2="$(create_run "$REPO_ID" "$AS_IID2")" || fail "stale-pool run-create failed (non-transient; see stderr)"
wait_status "$AS_RUN2" awaiting_approval
AS_GOT2="$(as_run_credential "$AS_RUN2")"
[ "$AS_GOT2" = "spare-key|pool_stale|null" ] || fail \
  "stale-pool run recorded '$AS_GOT2', want 'spare-key|pool_stale|null'. #754: a stale pool floors onto the pooled token (spare-key), reason pool_stale, with NO headroom — it must NEVER reach for the out-of-pool default"
pass "an entirely stale pool floors onto the pooled token, never the out-of-pool default (spare-key, pool_stale, no headroom)"
apipost "/api/runs/$AS_RUN2/inputs" '{"kind":"cancel","body":""}' >/dev/null 2>&1 || true

# ── (c) #754: a GENUINELY empty pool HOLDS in pool_wait and spends nothing, then RESUMES ──
# Pre-#754 an empty pool fell through to the owner default at awaiting_approval and recorded
# pool_empty. #754 refuses to bill the non-pooled default at all: the run parks in the new
# non-locking pool_wait status, recording NO credential, until a token re-enters the pool
# (a reactive sweeper pass, ~15s) or the owner calls resume-now. Opt BOTH tokens out (the
# default is already out from (b); this also takes the spare) so the pool is genuinely empty.
for sid in "$AS_DEFAULT_SECRET" "$AS_SPARE_SECRET"; do
  apipatch "/api/me/secrets/anthropic_token/$sid/auto-eligible" '{"auto_eligible":false}' >/dev/null \
    || fail "could not opt token $sid out of the pool for the empty-pool hold"
done
AS_IID3="$(apipost "/api/repos/$REPO_ID/issues" \
  '{"title":"E2E autoselect empty","description":"implements prds/111-auto-select-anthropic-token.md"}' | jq -r '.card.iid')"
AS_RUN3="$(create_run "$REPO_ID" "$AS_IID3")" || fail "empty-pool run-create failed (non-transient; see stderr)"
# It must HOLD in pool_wait, NOT fall through to awaiting_approval — the old empty-pool
# fallback would hang here forever, because nothing ever reaches the gate.
wait_status "$AS_RUN3" pool_wait
AS_GOT3="$(as_run_credential "$AS_RUN3")"
[ "$AS_GOT3" = "null|null|null" ] || fail \
  "empty-pool hold recorded '$AS_GOT3', want 'null|null|null'. #754: an empty pool spends nothing and records NO credential (label/reason/headroom all null) while it waits — it must never bill the non-pooled default"
pass "an empty pool holds the run in pool_wait and spends nothing (never the default)"

# Now RESUME it: opt the spare back INTO the pool and expedite the hold with resume-now (the
# DETERMINISTIC path — no wait on the ~15s reactive sweeper tick). resume-now flips pool_wait
# → queued; the worker then claims the run and resolves the now-pooled spare. Its reading is
# still aged (from (b)), so the auto lane floors onto it and records pool_stale; a fresh
# reading would instead record auto. Either way the credential is the POOLED spare-key, and
# NEVER the out-of-pool default.
apipatch "/api/me/secrets/anthropic_token/$AS_SPARE_SECRET/auto-eligible" '{"auto_eligible":true}' >/dev/null \
  || fail "could not opt the spare token back into the pool to resume the held run"
apipost "/api/runs/$AS_RUN3/resume-now" '' >/dev/null \
  || fail "resume-now did not accept the held pool_wait run"
wait_status "$AS_RUN3" awaiting_approval
AS_GOT3B="$(as_run_credential "$AS_RUN3")"
case "$AS_GOT3B" in
  "spare-key|auto|"*|"spare-key|pool_stale|null")
    pass "a held pool_wait run resumes onto a pooled token once one is opted in, never the default ($AS_GOT3B)" ;;
  *)
    fail "resumed pool_wait run recorded '$AS_GOT3B', want the pooled 'spare-key' with reason auto or pool_stale — it must spend the re-pooled spare, NEVER the out-of-pool default" ;;
esac
apipost "/api/runs/$AS_RUN3/inputs" '{"kind":"cancel","body":""}' >/dev/null 2>&1 || true

# ── leave the stack exactly as this phase found it ──────────────────────────────
# The three runs this phase created are NOT deleted, and that is checked rather than
# assumed: measured at this commit, nothing after this line reads /api/runs at all —
# the only later references are this phase's own cancels. `uzi run list` matches
# GET /api/runs ~3100 lines EARLIER, so it is unaffected. If a later phase ever starts
# counting runs, this is the note that says why it broke.
# 🔴 THIS IS NOT TIDINESS. The harness is one sequential stack, so anything left
# behind is an input to every later phase — and the first version of this block
# omitted the token delete, which broke PRD #104's binding phase 350 lines further
# down: it asserts "exactly two anthropic tokens" after ITS create, and found three.
# Measured, not theorised. The health phase restores health_stall_seconds for exactly
# this reason and says so.
apipatch "/api/workers/$AS_WORKER" '{"anthropic_bind_mode":"default"}' >/dev/null \
  || fail "could not restore the worker to default bind mode"
# The spare credential goes with it. Its gauge row CASCADES (00080's composite FK) and
# the runs that spent it keep their label snapshot with a NULL id (00086's SET NULL) —
# which is the historical shape M1 exists to preserve, not collateral damage.
curl -fsS -b "$JAR" -X DELETE "$BASE/api/me/secrets/anthropic_token/$AS_SPARE_SECRET" \
  -H "X-CSRF-Token: $(csrf)" >/dev/null \
  || fail "could not delete the spare Anthropic token; later phases count the caller's tokens"
apiget /api/me/secrets \
  | jq -e '[.secrets[] | select(.kind == "anthropic_token")] | length == 1' >/dev/null \
  || fail "the auto-selection phase leaked a token into the rest of the suite"
# The POOL FLAGS end correct without an explicit restore here, but only because of how (b)
# and (c) leave them: (b) opted the DEFAULT out and it stays out, and (c) re-pooled the SPARE
# to prove the resume — which the token delete just above removes entirely. So the one
# surviving token (the default) is opted OUT, matching the empty pool this phase inherited.
# This is CHECKED, not assumed: drop the default opt-out in (b), or the spare delete above,
# and a pooled token leaks into every later phase with nothing to catch it, at the same
# attribution distance that made the token leak expensive. Symmetric with the count check
# above, and it is the check that would notice.
apiget /api/me/secrets \
  | jq -e '[.secrets[] | select(.kind == "anthropic_token" and .auto_eligible)] | length == 0' >/dev/null \
  || fail "the auto-selection phase left a token in the pool; a later phase would inherit it"
# And the default's gauge goes back to the 55/12 the rate-limit phase seeded, so a
# later reader of /api/me/rate-limits sees what that phase established rather than what
# this one needed.
as_gauge "$AS_DEFAULT_SECRET" 55 12 "now()"

