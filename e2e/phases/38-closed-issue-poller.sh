# shellcheck shell=bash
# phase:    closed-issue-poller
# title:    PRD #98 M8b/B6': a closed forge issue reaches Done THROUGH THE POLLER (M6's wiring)
# critical: no
# lane:     gitlab
# executor: any
# requires: F_REVIEW F_IID
# provides: -
# handoff:  -
# mutates:  closes forge issue #$F_IID on the fake (:145); leaves a RETAINED auto-Done disposition (done|issue_close) on install_worker_tool/jq — deliberately not cleaned up (:210), shifts global triage todo by 1
# restores: - (issue left closed, disposition retained by design)
# =============================================================================
# PRD #98 M8b / B6' — the close→Done WIRING leg. THE POLLER ACTUALLY RUNS IT.
# =============================================================================
#
# WHAT THIS ROW IS FOR, AND WHAT IT DELIBERATELY DOES NOT RE-ASSERT. The behaviour
# matrix — auto-Done once, Undo sticks, a dismissed verdict is not overwritten, repo
# scoping, unsettled/orphaned rows skipped, a reopen does not reopen — is pinned by the
# six TestFiledIssueClose*LiveDB tests against a real Postgres, and re-asserting any of
# it here would buy nothing but harness minutes. Every one of those six calls
# svc.SyncFiledIssueCloses(ctx, repoID) DIRECTLY. What NOTHING in the repo does is RUN
# THE POLLER: its call site is covered only by forgesvc's TestSyncFiledIssueClosesWiring,
# which runs against a FAKE store. So the chain
#
#   a forge issue is closed → the issue cache reflects it → the poller's tick calls
#   SyncFiledIssueCloses → a disposition with the right provenance appears
#
# is unpinned end to end, and it is the one assertion neither a fake nor a live-DB store
# test can make. This block asserts exactly that.
#
# PLACEMENT. It rides $F_IID — the issue the #68 phase already filed against $F_REC on
# $F_REVIEW — so it costs no second judged run and no second forge issue. It sits BEFORE
# the judge-disable restore below, which is safe because the poller's call takes only the
# repo id and is NOT gated on judge_enabled, unlike the funnel above it.
#
# LANE. No gate is needed and none is added, and the argument is STRUCTURAL rather than
# "it is a long way above". Five facts, each checkable: `$FORGE` is validated at parse time
# to be exactly `gitlab` or `forgejo`, so there is no third value that skips both branches;
# the forgejo lane's `if` opens at column 0; the harness's ONLY bare `exit 0` sits at the
# TOP LEVEL of that block, directly under its closing `pass` — not nested in a deeper
# conditional that might not fire; the matching `fi` is at column 0 with no column-0 `fi`
# between, so the block is continuous; and the #98 phases open ~2000 lines below it.
# Adding `[ "$FORGE" = gitlab ] || fail` here would therefore guard a state the control
# flow cannot produce. The forge-fake mutator is lane-neutral in any case — it mutates the
# shared state.issues, which the Forgejo lane serves through toForgejoIssue.
#
# 🔴 FIDELITY LIMIT, stated here rather than left for a reader to infer, because it bounds
# what a green means. forge-fake contains ZERO occurrences of `updated_after`: GET /issues
# returns every recorded issue wholesale, by deliberate design ("Keeps a reconcile pass
# from evicting the cache"), and the Forgejo lane does the same. The real IncrementalSync
# DOES send UpdatedAfter (forgesvc/service.go, forge/gitlab.go). SO: this block proves the
# poller WIRES THE EDGE UP GIVEN A CACHE THAT REFLECTS THE CLOSE. It does NOT prove a real
# incremental sync would ever OBSERVE the close. That hole is deliberately not closed
# inside #98 — changing GET /issues' semantics would change them for every phase that
# depends on "return all recorded issues" — and it is raised separately instead.
say "PRD #98 M8b/B6': a closed forge issue reaches Done THROUGH THE POLLER (M6's wiring)"

B6_CAT=install_worker_tool
B6_TGT=jq

# 🔴 THIS FILE HAS TWO BOOLEAN IDIOMS AND YOU MUST NOT UNIFY THEM. `psql -tAc` renders a
# bare boolean as `t`/`f`, and `(expr)::text` as `true`/`false` — measured, not assumed:
# `SELECT (1 IS NULL)::text, 1 IS NULL` returns `false|f`. Both spellings are live and both
# are CORRECT where they sit: the PRD #68 and #104 phases compare bare booleans to `t`, and
# the #98 blocks below cast to ::text and compare to `true`. A "consistency" sweep that
# unifies them is a REGRESSION, because it changes the value without changing the comparison.
# The coexistence is also what produced a real defect here: a failure legend written for one
# convention against a value produced by the other, naming `f`/`t` for a message that could
# only ever print `false`/`true`. Read which form the projection uses before writing the
# expected value, every time.

# One-shot getter for wait_eq, in this file's own idiom.
b6_issue_state() { db_psql "SELECT state FROM issues WHERE repo_id='$REPO_ID' AND forge_issue_iid=$F_IID"; }

# wait_disposition REVIEW CAT TGT WANT [TIMEOUT] — poll for the auto-Done and, ON
# TIMEOUT, SAY WHICH OF THE TWO CAUSES IT IS. "No disposition after N seconds" has two
# explanations that need opposite fixes — the poller never ran, or it ran and did not
# consume the edge — and a message that does not separate them routes the next reader
# into the wrong subsystem with evidence attached, which is worse than a vague one.
# B6a's probe fixtures went with the dropped matrix, so there is no separate positive
# control left; the DIAGNOSIS is gathered at failure time from the two rows that
# discriminate. Reads $REPO_ID and $F_IID from the enclosing phase.
#
# TIMEOUT FLOOR: the chain crosses two stages inside ONE poller tick (the issue sync
# writes the cache, then SyncFiledIssueCloses reads it), so one tick suffices only if
# the close lands before that tick's sync; land it mid-tick and the cache write is next
# tick and the disposition the tick after. The reconcile period here is
# E2E_FORGE_POLL_INTERVAL=2s x FORGE_RECONCILE_EVERY=2 = 4s, so the floor is 2 periods
# = 8s. The default below is 20s: the floor plus real slack, and report_margins will
# show the headroom actually used rather than leaving the ceiling to guesswork.
wait_disposition() {
  local review="$1" cat="$2" tgt="$3" want="$4" timeout="${5:-20}"
  local start=$SECONDS deadline=$((SECONDS + timeout)) got="" cache stamp
  while [ $SECONDS -lt $deadline ]; do
    got="$(db_psql "SELECT status FROM recommendation_dispositions WHERE review_id='$review' AND category='$cat' AND target='$tgt'")"
    if [ "$got" = "$want" ]; then
      record_margin "disposition $cat/$tgt -> $want" "$((SECONDS - start))" "$timeout"
      return 0
    fi
    sleep 0.3
  done
  cache="$(db_psql "SELECT state FROM issues WHERE repo_id='$REPO_ID' AND forge_issue_iid=$F_IID")"
  stamp="$(db_psql "SELECT (close_synced_at IS NOT NULL)::text FROM recommendation_filed_issues WHERE review_id='$review' AND category='$cat' AND target='$tgt'")"
  fail "PRD #98 M8b/B6': no '$want' disposition on $cat/$tgt after ${timeout}s (status is '${got:-<none>}').
  DIAGNOSIS — issue cache state for #$F_IID: '${cache:-<no cached row>}'; edge consumed (close_synced_at IS NOT NULL): '${stamp:-<no filed row>}'.
    cache 'closed' + edge 'false' -> the poller's ISSUE SYNC ran but SyncFiledIssueCloses did not consume the edge. Look at the poller call site and ListFiledIssueCloseEdges' filters. NOT a forge-fake problem.
    cache 'opened' or empty       -> the poller's ISSUE SYNC did not run or did not see the close, so SyncFiledIssueCloses was never in a position to act. Look at the poll interval and the forge-fake state route. NOT a judge problem.
    cache 'closed' + edge 'true'  -> the edge WAS consumed and the insert wrote nothing, i.e. a competing disposition already existed on this coordinate. The precondition below exists to have caught that first.
  WHAT THIS CANNOT RULE OUT: an api that is dead or wedged, which presents as the second case. What DOES separate it is the synced_at liveness probe armed after close_issue — that one moves only when a poller tick touches this repo's issue cache, so if it passed and this failed, the poller RAN. (The cache precondition earlier proves the FILING landed; it is written by the file-issue handler in its own transaction and says nothing about the poller.)"
}

# PRECONDITION 1: the issue cache reflects #$F_IID as OPEN.
#
# 🔴 THIS IS A STATE CHECK, NOT A LIVENESS CHECK, and it was labelled as one. The cache row
# is written by the FILE-ISSUE HANDLER, not by the poller: settleFiledIssue
# (handler/review_issue_file.go) calls UpsertIssue in the SAME TRANSACTION as settling the
# filed link, with State: created.State, and its own comment says why — "so the board card
# appears without a poll". forge-fake creates issues state:"opened". So this wait is
# satisfied instantly by a row the #68 phase's own API call wrote, and IT PASSES AGAINST A
# STONE-DEAD POLLER. Two reviewers derived that independently, from different starting
# points, after it had been written here as a liveness control.
#
# It still earns its place — it proves the filing reached the cache, and it pairs with the
# wire check below — but the liveness question is answered by the probe underneath.
wait_eq opened 20 "the issue cache reflects filed issue #$F_IID as open (the FILING landed; not a poller check)" b6_issue_state

# PRECONDITION 2, or the assertion below is vacuous. The coordinate must be SETTLED-filed
# with the edge unconsumed, and must carry NO disposition — a pre-existing verdict would
# make ApplyFiledIssueCloseEdge's ON CONFLICT DO NOTHING write nothing while still
# stamping the edge, and the row would then be asserting a disposition it did not cause.
[ "$(db_psql "SELECT count(*) FROM recommendation_filed_issues WHERE review_id='$F_REVIEW' AND category='$B6_CAT' AND target='$B6_TGT' AND filed_at IS NOT NULL AND close_synced_at IS NULL")" = 1 ] \
  || fail "PRD #98 M8b/B6': the $B6_CAT/$B6_TGT coordinate on review $F_REVIEW is not a settled filed link with an unconsumed edge (want exactly 1 row with filed_at NOT NULL and close_synced_at NULL)"
[ "$(db_psql "SELECT count(*) FROM recommendation_dispositions WHERE review_id='$F_REVIEW' AND category='$B6_CAT' AND target='$B6_TGT'")" = 0 ] \
  || fail "PRD #98 M8b/B6': the $B6_CAT/$B6_TGT coordinate already carries a disposition — the auto-Done assertion below would pass without the poller having done anything"
# And the WIRE agrees it is filed, not merely the tables: this is the rung the shared
# BucketOf ladder puts a settled filed link on, read through the API the panel reads.
apiget "/api/me/judge/recommendations?bucket=filed" \
  | jq -e --arg c "$B6_CAT" --arg t "$B6_TGT" 'any(.groups[]?; .category == $c and .target == $t)' >/dev/null \
  || fail "PRD #98 M8b/B6': $B6_CAT/$B6_TGT does not bucket 'filed' on GET /api/me/judge/recommendations — the precondition the close edge acts on is not what the wire reports"
pass "precondition: #$F_IID cached open, $B6_CAT/$B6_TGT filed on the wire, edge unconsumed, no disposition"

# THE HUMAN ACTION M6 REACTS TO. uzi never closes an issue itself — it only ever reads
# the state — which is why this needs the /_e2e mutator at all.
B6_SYNC0="$(db_psql "SELECT to_char(synced_at,'YYYYMMDDHH24MISSUS') FROM issues WHERE repo_id='$REPO_ID' AND forge_issue_iid=$F_IID")"
close_issue "$F_IID"
pass "closed forge issue #$F_IID on the fake forge (issue-cache synced_at before the close: ${B6_SYNC0:-<none>})"

# THE LIVENESS PROBE, which is the positive control B6a's dropped probe fixtures were going
# to buy — recovered for free, with no probe coordinate, no second issue and no perturbation
# of triage.todo.
#
# MECHANISM: UpsertIssue's conflict path sets `synced_at = now()` UNCONDITIONALLY, not only
# when a column changed (queries/forge.sql), and the poller's sync runs it for every issue
# the forge returns — and forge-fake returns every recorded issue wholesale on every poll.
# So synced_at advances on EVERY tick whether or not anything changed. Re-derived here: after
# the filing, the only callers that re-upsert this row are the poller's three sync paths in
# forgesvc/service.go; the two non-poller callers are the file-issue handler (one-shot, above)
# and the manual /issues endpoint, which nothing in this phase touches. So a moving synced_at
# is the poller and nothing else.
#
# to_char because that is this file's own idiom for making a timestamp shell-comparable.
# Armed AFTER close_issue so it bounds the window from below by OBSERVED poller work rather
# than by elapsed time.
#
# 🔴 THIS PROBE WAS INFERRED FROM THE QUERY AND THE SYNC PATH, NOT EXECUTED — nobody has run
# it against a live stack. Its failure message says so, because a new wait that has never run
# is exactly the thing that turns into a confident wrong diagnosis on someone else's night.
b6_synced_advanced() {
  local now; now="$(db_psql "SELECT to_char(synced_at,'YYYYMMDDHH24MISSUS') FROM issues WHERE repo_id='$REPO_ID' AND forge_issue_iid=$F_IID")"
  [ -n "$now" ] && [ "$now" != "$B6_SYNC0" ] && echo advanced || echo same
}
B6_LIVE_START=$SECONDS
B6_LIVE_DEADLINE=$((SECONDS + 20))
while [ $SECONDS -lt $B6_LIVE_DEADLINE ]; do
  [ "$(b6_synced_advanced)" = advanced ] && break
  sleep 0.3
done
[ "$(b6_synced_advanced)" = advanced ] || fail "PRD #98 M8b/B6': issues.synced_at for #$F_IID has not moved off '$B6_SYNC0' in 20s, so no poller tick has touched this repo's issue cache — the close→Done chain below cannot start.
  BEFORE CONCLUDING THE POLLER IS DEAD: this probe was INFERRED from UpsertIssue's unconditional 'synced_at = now()' on the conflict path and from forge-fake returning every issue wholesale. It has never been executed against a live stack. If the incremental sync short-circuits before the upsert — or forge-fake's list route changed — the probe is wrong and the poller may be perfectly healthy. Check a second issue's synced_at before touching the judge code."
record_margin "issues.synced_at advances (poller liveness)" "$((SECONDS - B6_LIVE_START))" 20
pass "poller liveness: issues.synced_at for #$F_IID advanced off '$B6_SYNC0' — a tick has run since the close"

# `"done"` IS QUOTED TO KEEP IT A WORD, NOT FOR STYLE. Bare `done` here is the
# WANT positional (wait_disposition's `want="$4"`, compared with `[ "$got" =
# "$want" ]`), but shellcheck reads a bare `done` after a simple command as a
# missing `;` before a loop terminator and reports SC1010 at warning severity.
# The quotes are semantically inert -- the shell word is `done` either way -- and
# they are what lets PRD #103 M5 gate `shellcheck --severity=warning` at zero
# without an `-e SC1010` that would blind every other script in the repo to a
# genuinely missing `;`. Do not unquote it.
wait_disposition "$F_REVIEW" "$B6_CAT" "$B6_TGT" "done"

# THE PROVENANCE TRIPLE, and it is the assertion that makes this row about the POLLER
# rather than about a disposition existing. status='done' alone is equally satisfied by a
# human clicking Done; set_via='issue_close' with a NULL actor is reachable only from
# ApplyFiledIssueCloseEdge, whose provenance is fixed in the query text rather than
# parameterised precisely so no call site can forge it.
B6_PROV="$(db_psql "SELECT COALESCE(status,'?') || '|' || COALESCE(set_via,'?') || '|' || (set_by_user_id IS NULL)::text
                      FROM recommendation_dispositions
                     WHERE review_id='$F_REVIEW' AND category='$B6_CAT' AND target='$B6_TGT'")"
[ "$B6_PROV" = "done|issue_close|true" ] \
  || fail "PRD #98 M8b/B6': the auto-Done's provenance is '$B6_PROV', want 'done|issue_close|true' (status|set_via|set_by_user_id IS NULL). A 'done|manual|false' here means a HUMAN path wrote it and the poller proved nothing"
# The other half of the atomic statement: the edge is consumed, so a second tick cannot
# re-apply after an Undo. The six live-DB tests pin what that guarantees; this only
# asserts the poller's own run left the marker.
[ "$(db_psql "SELECT (close_synced_at IS NOT NULL)::text FROM recommendation_filed_issues WHERE review_id='$F_REVIEW' AND category='$B6_CAT' AND target='$B6_TGT'")" = true ] \
  || fail "PRD #98 M8b/B6': the disposition landed but close_synced_at was never stamped — the two halves of ApplyFiledIssueCloseEdge are no longer atomic"
pass "PRD #98 M8b/B6': closing #$F_IID drove the POLLER to auto-Done $B6_CAT/$B6_TGT with provenance done|issue_close|<null actor>, edge consumed"

# NOT CLEANED UP, deliberately: the disposition IS the outcome, and deleting it would
# delete the evidence. It shifts the global triage `todo` by one, which is why every
# later count assertion in this file is a DELTA against its own pre-seed reading rather
# than an absolute. The issue is left CLOSED for the same reason a reopen is not tested:
# close_synced_at stays stamped and the product does not act on a reopen.

