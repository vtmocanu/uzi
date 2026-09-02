# shellcheck shell=bash
# phase:    mr-close-watcher
# title:    PRD #24: MR-close watcher (Human Review <-> In Progress on MR close/reopen)
# critical: no
# lane:     gitlab
# executor: any
# requires: IID MR_IID RUN
# provides: env:E2E_FORGE_POLL_INTERVAL=2s
# handoff:  -
# mutates:  api:E2E_FORGE_POLL_INTERVAL=2s,FORGE_RECONCILE_EVERY=2
# restores: -
# =============================================================================
# PRD #24 — MR-close watcher: a reviewer closing an agent's MR without merging
# moves the card from Human Review back to In Progress; reopening restores it; a
# manual drag is never fought. The happy-path run above left card #$IID in Human
# Review with an open MR ($MR_IID) — exactly the watcher's precondition.
say "PRD #24: MR-close watcher (Human Review ⇄ In Progress on MR close/reopen)"

# The watcher only ticks inside the poller; the overlay default is 24h. Switch to
# ~2s and recreate the api so the MR-state watcher actually runs. 2s is the
# practical floor for cadence sanity (with FORGE_RECONCILE_EVERY=2 a ~4s reconcile
# period), but the mid-pass-cancellation hazard that used to force it is gone: since
# issue #139 the whole-tick deadline is floored at 2x the forge HTTP timeout
# (poller.go tickBudget), so a short interval no longer cancels a slow tick or loses
# the autopilot record-then-comment (the comment is never retried by design). The reconcile
# cadence (FORGE_RECONCILE_EVERY, the PRD #19 FullSync-eviction dedup's bounded
# wait) is set in the SAME recreate: a full reconcile only mirrors the forge the
# watcher already wrote forge-first (FullSync writes the issue cache, never moves
# cards or touches runs.mr_state), so tightening it here changes nothing this or
# the intervening PRD #16/#18 phases assert — and saves a second api recreate.
printf 'E2E_FORGE_POLL_INTERVAL=2s\nFORGE_RECONCILE_EVERY=2\n' >> "$ENVFILE"
"${COMPOSE[@]}" up -d --no-deps --force-recreate api >/dev/null
wait_http
login
# The completed run's run-lifecycle move to Human Review is async; tolerate it
# settling (and confirm the fake retained the issue across the restart).
wait_card_column "$IID" "Human Review" 20
pass "poller sped to ~2s; card #$IID in Human Review with open MR !$MR_IID"

# NULL-bootstrap (Decision 9): the first tick records the MR's CURRENT state
# ('opened') WITHOUT moving, so a pre-existing state never triggers a spurious
# move. Give it 2 poll ticks (2s each) to act + confirm-stable; the card must stay
# put (Decision 5 floors a 2s-poll negative window at 2 ticks = 4s, PRD #97 M5).
sleep 4
[ "$(card_column "$IID")" = "Human Review" ] \
  || fail "NULL-bootstrap must record MR state without moving the card (Decision 9)"
pass "NULL-bootstrap recorded MR state without moving the card"

# Close edge: reviewer closes the MR without merging → rework → In Progress. The
# watcher also records the run's mr_state='closed' (PRD #33: what the "MR !N closed"
# chip renders), so assert both the move and the surfaced state.
flip_mr "$MR_IID" closed
wait_card_column "$IID" "In Progress" 40
wait_run_mr_state "$RUN" closed 20
pass "MR closed unmerged → card #$IID moved Human Review → In Progress; run mr_state=closed"

# Reopen edge: reopening the MR restores the card to Human Review, symmetrically, and
# the run's mr_state returns to 'opened' (the plain chip again).
flip_mr "$MR_IID" opened
wait_card_column "$IID" "Human Review" 40
wait_run_mr_state "$RUN" opened 20
pass "MR reopened → card #$IID returned In Progress → Human Review; run mr_state=opened"

# Manual-drag pre-emption, exercising the Go source-column guard (not just the SQL
# prefilter): re-close so the card is In Progress AND still a watch candidate
# (mr_state='closed'), drag it to Later, then reopen. The reopen edge's guard sees
# the card is no longer in its expected source column (In Progress) and backs off —
# the human's placement wins.
flip_mr "$MR_IID" closed
wait_card_column "$IID" "In Progress" 40
apipost "/api/repos/$REPO_ID/issues/$IID/move" '{"to_column":"Later"}' >/dev/null
# The move is forge-first; let any in-flight reconcile settle.
# ⚠️ DELIBERATELY LEFT AT 10s (PRD #97 M9). This is the tightest wait_* ceiling in the
# suite — it waits on a reconcile (4s period), so 10s is only ~2.5 periods, against
# siblings at 20-40s. It is still ABOVE the 2-period floor, and raising it on that hunch
# alone is exactly the move that produced M9's own worst error (a timeout "fixed" on a
# guess, masking rather than diagnosing). The margin instrumentation now records what
# this wait ACTUALLY takes; if the data shows it running near the wire, raise it then,
# with evidence. Do not raise it without that measurement.
wait_card_column "$IID" "Later" 10
flip_mr "$MR_IID" opened
# Two ticks (2s each) must pass with the card LEFT in Later (a fight would yank it
# to Human Review within one tick; Decision 5 floors this at 2 ticks = 4s, PRD #97 M5).
sleep 4
[ "$(card_column "$IID")" = "Later" ] \
  || fail "watcher fought a manual drag: card #$IID left Later after the MR reopened"
pass "manual drag wins: card #$IID stayed in Later despite the MR reopening"

