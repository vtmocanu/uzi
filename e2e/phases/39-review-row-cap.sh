# shellcheck shell=bash
# phase:    review-row-cap
# title:    PRD #98 M8b/B4': the server's row cap, and the truncation remedy executed against it
# critical: no
# lane:     gitlab
# executor: any
# requires: F_REVIEW
# provides: -
# handoff:  -
# mutates:  -
# restores: settings:judge_enabled=false
# =============================================================================
# PRD #98 M8b / B4' — the ROW CAP, and Part C's truncation remedy EXECUTED against it.
# =============================================================================
#
# RUNS LAST IN THIS PHASE, so its teardown is the single owner of the cleanup.
#
# WHY IT HAS TO BE HERE AND CANNOT BE CHEAPER. JudgeBacklogMaxRows is a compile-time const
# (2000) with no env override, and the service reads Lim = cap+1 then slices — so the ONLY
# arrangement in the repo that reaches `truncated: true` is a seed above the cap. The
# largest Lim any live-DB test passes is 1000, and the one cap assertion in the tree
# (handler/judge_recommendations_test.go) feeds a FAKE store 2001 rows, which proves the
# service's slice and says nothing about the query's LIMIT. This block is the only place
# the real SQL meets the real cap.
#
# THE SEED'S SHAPE IS THE WHOLE DESIGN — four reviews at three ages, each on a run this
# fixture mints for itself, because `ORDER BY rv.updated_at DESC …` decides what the cut
# keeps and `run_reviews.target_run_id` is UNIQUE (see the seeding note below):
#
#   B4_OLD  (-3d, on B4_RUN_OLD)   1 coordinate, `b4-cut-me`. MUST be cut.
#   B4_BIG  (-2d, on B4_RUN_BIG)   2001 distinct coordinates. The cap.
#   B4_SA   (now, on B4_RUN_A)     $B4_TGT + `b4-dup` twice
#   B4_SB   (now, on B4_RUN_B)     $B4_TGT again, on a SECOND run
#
# so the returned window is B4_SA/B4_SB first, then most of B4_BIG, and `b4-cut-me` falls
# off the end. A `truncated` boolean on its own is satisfiable by a flag flip over complete
# data; the absent coordinate is what makes it a claim about the CUT.
#
# WHY B4_BIG SITS ON A RUN THE REMEDY NEVER ANCHORS TO, and this is the non-obvious part:
# the ?run= anchor is a COORDINATE semi-join, so anchoring to a run returns every coordinate
# appearing in ANY of that run's reviews. Had the 2001 rows shared a run with a settled
# coordinate, the anchored re-read would return all 2001 again — still truncated — and the
# remedy the CLI prints would be FALSE. Two runs carry the settled coordinate and neither
# carries the bulk seed.
#
# TWO SETTLED RUNS, NOT ONE, for the reason the undo row above states: a count of 1 cannot
# distinguish the real behaviour from a `head -1` regression. Dismissing $B4_CAT/$B4_TGT
# fans out to both reviews, so the CLI prints exactly two remedy lines.
#
# THE TARGET IS A VARIABLE, and that is a fix rather than a flourish. This block hardcoded
# the literal at the dismiss call site while using $B4_CAT for the category, and the two
# halves drifted: the seed said `b4-remedy` and the dismiss said `remedy`. Matching is exact
# equality — reviewCoord only TrimSpaces the flag, and the bulk query joins
# `want.target = rr.target` with no LIKE and no prefix match — so nothing would have
# settled, and the dismiss STILL EXITS 0 because the CLI treats a no-match as a legal
# updated=0. It would then have failed twice over, at the arrange count and again with the
# lift seeing 0 matches against want=2, neither message naming the typo. The M8c row three
# phases up already does this correctly through $PI_TGT; this now matches it.
#
# FOLDS IN B2's ONE UNCOVERED SHAPE at no extra cost: `dup` is seeded TWICE on ONE review,
# which is `occurrences > run_count` (occurrences 2, run_count 1). It costs one INSERT row.
# review_recommendations has no UNIQUE on (review_id, category, target), which is what makes
# it expressible at all — the same absence the #68 phase's count-first guard exists for.
say "PRD #98 M8b/B4': the server's row cap, and the truncation remedy executed against it"

# THE CATEGORY IS NOT FREE-FORM, and this cost a rewrite: review_recommendations carries
# CHECK (category IN ('enable_tool','install_worker_tool','adjust_template','improve_agent',
# 'add_agent','improve_uzi','cost_efficiency')), so the invented `e2e_b4` this block first used would have
# aborted the run on its very first INSERT. Measured against a throwaway Postgres before
# committing, not discovered by a 30-minute harness run.
#
# `adjust_template` is chosen because it is the only one of the six that NO other phase in
# this file uses (measured: the harness's sole other category is install_worker_tool), so
# the teardown below can assert on the category and be exact. NOT `improve_uzi`: that feeds
# the self-improvement backlog, whose ListOpenImproveUziRecommendations selects across the
# WHOLE table with no review or user scope, and this repo has already lost time to a fixture
# that seeded an open improve_uzi row. Every target additionally carries a `b4-` prefix.
B4_CAT=adjust_template
B4_TGT=b4-remedy
B4_OWNER="$(db_psql "SELECT user_id FROM run_reviews WHERE id='$F_REVIEW'")"
printf '%s' "$B4_OWNER" | grep -qE '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' \
  || fail "PRD #98 M8b/B4': could not resolve the fixture owner off review $F_REVIEW (got '$B4_OWNER')"

# 🔴 THE FIXTURE SEEDS ITS OWN RUNS, AND IT MUST. `run_reviews.target_run_id` is
# `NOT NULL UNIQUE` (migration 00059, whose own comment says "One review per reviewed run …
# so a re-judge UPSERTs"). ONE REVIEW PER RUN, by design. The first version of this block
# reused the harness's existing runs and aborted at the second INSERT with
#
#   ERROR: duplicate key value violates unique constraint "run_reviews_target_run_id_key"
#
# broken twice over: it put TWO of its reviews on RUN_CLI, and J_RUN already owns F_REVIEW
# from the #68 phase. Found by RUNNING the harness. Five validators read this block
# statically and three attacked the fixture arrangement specifically; none of us saw it,
# because "are the runs distinct enough for the semi-join" and "may these runs carry a
# review at all" are different questions and only the first was asked. That is the argument
# for the e2e leg existing, made by the leg on its first execution.
#
# Four dedicated runs also make the arrangement SELF-CONTAINED rather than dependent on what
# earlier phases left behind: the separation the anchor needs is now a property of this
# fixture, not a coincidence of the harness's history.
#
# Direct-seeded as `completed`, which is safe rather than merely convenient: a judge run is
# enqueued by maybeEnqueueJudge on a just-committed TERMINAL TRANSITION, never by a scan for
# unjudged completed runs, so a row inserted straight into the terminal state is never
# judged and its review slot stays free. `kind` is omitted — it defaults to 'issue'
# (migration 00043). The iid range is far above anything forge-fake mints, and no `issues`
# row is created for them, so they cannot appear on a board or collide with a phase that
# addresses an issue by iid.
#
# b4_seed_run VAR IID — insert a completed run owned by the fixture owner, assign its id.
b4_seed_run() {
  local var="$1" iid="$2" id
  db_psql "INSERT INTO runs (user_id, repo_id, issue_iid, issue_title, issue_description, status)
           VALUES ('$B4_OWNER', '$REPO_ID', $iid, 'PRD98-B4 fixture run $iid', 'B4 fixture; never executed', 'completed')" >/dev/null
  id="$(db_psql "SELECT id FROM runs WHERE repo_id='$REPO_ID' AND issue_iid=$iid")"
  printf '%s' "$id" | grep -qE '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' \
    || fail "PRD #98 M8b/B4': seeded run for iid $iid did not read back as a bare uuid: '$id'"
  printf -v "$var" '%s' "$id"
}
b4_seed_run B4_RUN_OLD 990001
b4_seed_run B4_RUN_BIG 990002
b4_seed_run B4_RUN_A   990003
b4_seed_run B4_RUN_B   990004
# The separation the ?run= anchor depends on, asserted rather than assumed: the bulk seed's
# run must not be one of the two the remedy anchors to. All four are freshly minted here, so
# this cannot fail today — which is exactly why it is cheap to keep. It fires the moment
# someone "simplifies" the fixture back onto shared runs, which is how it broke the first time.
[ "$B4_RUN_BIG" != "$B4_RUN_A" ] && [ "$B4_RUN_BIG" != "$B4_RUN_B" ] && [ "$B4_RUN_A" != "$B4_RUN_B" ] \
  || fail "PRD #98 M8b/B4': the bulk-seed run must differ from BOTH runs the remedy anchors to. The anchor is a COORDINATE semi-join, so an anchored re-read returns every coordinate in any of that run's reviews — sharing a run would return all 2001 rows again, the re-read would still be truncated, and the remedy the CLI prints would be FALSE"
pass "seeded 4 dedicated runs for the B4' fixture (run_reviews.target_run_id is UNIQUE: one review per run)"

# b4_seed_review VAR TARGET_RUN SUMMARY AGE_INTERVAL — insert a review and ASSIGN its id
# to VAR.
#
# It assigns rather than echoing, and that is not style. A `fail` inside `$( )` runs in a
# SUBSHELL: its `exit 1` kills only that subshell, and its message goes to the subshell's
# STDOUT, which is exactly what the caller is capturing — so the diagnostic ends up INSIDE
# the variable instead of on screen. `set -e` would still stop the run, on an assignment,
# with no message. Assigning through `printf -v` keeps the check in the caller's shell.
#
# NOT `RETURNING id` either: db_psql is `psql -tAc … | tr -d '\r\n'`, so psql's command TAG
# is welded onto the returned row and yields `<uuid>INSERT 0 1` — non-empty, passes a bare
# -n guard, and explodes several statements later. Read it back with a SELECT and assert the
# SHAPE. (Measured on this branch by the M8c fixture above; repeated here because the trap
# belongs to the helper, not to one call site.)
b4_seed_review() {
  local var="$1" run="$2" summary="$3" age="$4" id
  db_psql "INSERT INTO run_reviews (target_run_id, user_id, verdict, summary_md, updated_at)
           VALUES ('$run', '$B4_OWNER', 'ok', '$summary', now() - interval '$age')" >/dev/null
  id="$(db_psql "SELECT id FROM run_reviews WHERE summary_md='$summary'")"
  printf '%s' "$id" | grep -qE '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' \
    || fail "PRD #98 M8b/B4': seeded review '$summary' did not read back as a bare uuid: '$id'"
  printf -v "$var" '%s' "$id"
}
b4_seed_review B4_OLD "$B4_RUN_OLD" 'PRD98-B4-oldest'  '3 days'
b4_seed_review B4_BIG "$B4_RUN_BIG" 'PRD98-B4-bulk'    '2 days'
b4_seed_review B4_SA  "$B4_RUN_A"   'PRD98-B4-small-a' '0 days'
b4_seed_review B4_SB  "$B4_RUN_B"   'PRD98-B4-small-b' '0 days'

db_psql "INSERT INTO review_recommendations (review_id, category, target, rationale_md, confidence)
         VALUES ('$B4_OLD','$B4_CAT','b4-cut-me','B4 oldest-review coordinate: it must fall outside the cap','low')" >/dev/null
B4_SEED_START=$SECONDS
db_psql "INSERT INTO review_recommendations (review_id, category, target, rationale_md, confidence)
         SELECT '$B4_BIG', '$B4_CAT', 'b4-bulk-' || g, 'B4 bulk seed row ' || g, 'low'
           FROM generate_series(1, 2001) g" >/dev/null
B4_SEED_SECS=$((SECONDS - B4_SEED_START))
db_psql "INSERT INTO review_recommendations (review_id, category, target, rationale_md, confidence)
         VALUES ('$B4_SA','$B4_CAT','$B4_TGT','B4 remedy coordinate, run A','low'),
                ('$B4_SA','$B4_CAT','b4-dup','B4 duplicate coordinate on ONE review, member 1','low'),
                ('$B4_SA','$B4_CAT','b4-dup','B4 duplicate coordinate on ONE review, member 2','low'),
                ('$B4_SB','$B4_CAT','$B4_TGT','B4 remedy coordinate, run B','low')" >/dev/null
pass "seeded 2001 bulk coordinates (${B4_SEED_SECS}s) plus the oldest-review, duplicate and remedy fixtures"

# The design ASSUMED this seed is sub-second and asked for that to be measured rather than
# inherited. MEASURED before this landed, against a throwaway Postgres 17 on the real table
# shape: ~27 ms (median, Execution Time; the 8.4 ms figure this line used to carry is the
# Insert node's BEST CASE) for the 2001-row insert. Still two orders of magnitude under a
# second, so the assumption holds and the added harness time is the round trip, not the write.
#
# 🔴 CORRECTED, AND BOTH HALVES OF THE CORRECTION ARE REUSABLE. Measured properly against the
# live stack: five samples, each inside BEGIN…ROLLBACK so nothing persists.
#   Insert node     8.497 – 25.655 ms   (median ~11.9)
#   Execution Time 21.454 – 42.940 ms   (median ~26.7)
# (1) The old number was not WRONG, it was measured ONCE — 8.497 ms came back on sample 1,
#     within 0.1 ms of it. Drawing the good end of an 8.5–25.7 ms spread is what a single
#     sample does; "measured once" is the finding, not "measured wrong".
# (2) THE NODE IS NOT THE STATEMENT. `Insert on …` excludes planning-adjacent setup,
#     constraint and index work, and returning; Execution Time is the statement. Quoting the
#     node was quoting the most flattering slice of what was measured — the same discipline
#     the tally corrections in this wave turned on numerals, applied to a profile.
#
# B4_SEED_SECS ABOVE DOES NOT CORROBORATE ANY OF THIS AND MUST NOT BE READ AS DOING SO. It
# comes from $SECONDS — WHOLE-SECOND resolution — so it reads 0 or 1 whether the true cost
# is 8 ms or 900 ms, and it includes docker exec and psql startup besides. What it is good
# for is a floor check across runs: if it ever climbs, the seed stopped being cheap and that
# is worth knowing. Say so rather than lowering the cap — the cap is the thing under test.

# --- the cap itself ----------------------------------------------------------
B4_ALL="$(apiget "/api/me/judge/recommendations?bucket=all")"
echo "$B4_ALL" | jq -e '.truncated == true' >/dev/null \
  || fail "PRD #98 M8b/B4': 2001+ owned recommendation rows did not set truncated on GET /api/me/judge/recommendations?bucket=all — the query's LIMIT or the service's slice is not what the cap claims (truncated=$(echo "$B4_ALL" | jq -c '.truncated'), groups=$(echo "$B4_ALL" | jq '.groups|length'))"
# THE CUT, not just the flag. A `truncated: true` alone is satisfied by a flag flip over
# complete data; this is the assertion that the rows were actually dropped, and dropped
# from the OLDEST review, which is the ordering the cut depends on.
echo "$B4_ALL" | jq -e --arg c "$B4_CAT" 'any(.groups[]?; .category == $c and .target == "b4-cut-me") | not' >/dev/null \
  || fail "PRD #98 M8b/B4': the coordinate seeded on the OLDEST review still came back under a truncated read — the cap is reporting truncation without cutting, or the ORDER BY no longer decides what survives"
echo "$B4_ALL" | jq -e --arg c "$B4_CAT" --arg t "$B4_TGT" 'any(.groups[]?; .category == $c and .target == $t)' >/dev/null \
  || fail "PRD #98 M8b/B4': the remedy coordinate is not in the truncated window, so the dismiss below would settle nothing"
# B2's uncovered shape, live: the same coordinate twice on ONE review is 2 occurrences
# behind 1 run. This is the SQLSTATE 21000 shape the grouper's own comment names, and the
# only place in the tree it is exercised against the real query.
echo "$B4_ALL" | jq -e --arg c "$B4_CAT" 'any(.groups[]?; .category == $c and .target == "b4-dup" and (.occurrences|length) == 2 and .run_count == 1)' >/dev/null \
  || fail "PRD #98 M8b/B4': the duplicate coordinate did not group as 2 occurrences behind 1 run (got $(echo "$B4_ALL" | jq -c --arg c "$B4_CAT" '.groups[]? | select(.category == $c and .target == "b4-dup") | {occ: (.occurrences|length), run_count}'))"
pass "row cap reached: truncated=true, the oldest review's coordinate is CUT, and the duplicate coordinate is 2 occurrences behind 1 run"

# --- printed-instruction row: uzi review backlog --run -----------------------
# THE FOURTH ROW. Part C declared this one evidenceNotExecuted with the reason "needs the
# 2001-row seed, which is M8b's"; that seed now exists, so the entry flips to evidenceE2E in
# the SAME commit as this row. The flip is what couples them: the registry check reads THIS
# FILE for the label below, so flipping without the row goes red and landing the row without
# the flip leaves a stale not-executed claim.
#
# The printed text CHANGED to make this expressible at all (user-approved). It used to be a
# single line carrying the literal `uzi review backlog --run <run-id>` — a placeholder in
# the OUTPUT, not a value substituted at emit time — which cannot be executed verbatim by
# anyone. It is now one runnable line per settled run.
PI_LABEL_TRUNC="printed-instruction row: uzi review backlog --run"
PI_OUT_TRUNC="$(uzi_cli review dismiss --category "$B4_CAT" --target "$B4_TGT" --reason wont-do)" \
  || fail "$PI_LABEL_TRUNC: the group dismiss that EMITS the remedy failed (exit $?)"
# ARRANGE, not the assertion: the write must have settled BOTH members, or there is only one
# run to name and the count below stops discriminating.
[ "$(db_psql "SELECT count(*) FROM recommendation_dispositions WHERE category='$B4_CAT' AND target='$B4_TGT'")" = 2 ] \
  || fail "$PI_LABEL_TRUNC: the group dismiss settled $(db_psql "SELECT count(*) FROM recommendation_dispositions WHERE category='$B4_CAT' AND target='$B4_TGT'") coordinate(s), want 2 (one per review, on two different runs). Output was:
$PI_OUT_TRUNC"
run_printed_instructions "$PI_LABEL_TRUNC" 'uzi review backlog --run [0-9a-f-]{36}' 2 "$PI_OUT_TRUNC"
# THE OUTCOME, and it is the whole point of the remedy: the anchored re-read is NOT
# truncated. Exit 0 alone would be satisfied by an anchored read that truncates identically,
# which is exactly what `--bucket all` does and why naming it was the false instruction.
#
# Spelled as an `if`, not as `grep … && fail`, because an `if` condition is exempt from
# `set -e` in every position. The rewrite stays; the REASON it originally carried was too
# broad and is corrected here, since a wrong explanation of a shell rule is worse than none
# — one reviewer nearly filed three false findings against the other sites on the strength
# of it.
#
# MEASURED under `set -euo pipefail`, three positions:
#   top level                             echo hi | grep -qF nope && fail "x"   SURVIVES
#   inside a for loop                     same                                  SURVIVES
#   LAST COMMAND OF A FUNCTION, called
#   at top level                          same                                  ABORTS
# So the AND-list IS exempt — except as a function's last command, where the FUNCTION
# returns the list's non-zero status and the call becomes an ordinary top-level command that
# `set -e` does not exempt. Much narrower than "a failing final command in an AND-list is
# not exempt", which is what this comment used to say.
#
# The other `grep … && fail` sites in this file were checked against that rule and left
# alone: none is a function's last command. Rewriting them would be a mechanical sweep on a
# mechanism that does not reach them.
# 🔴 THE ASSERTION IS `truncated == false`, PER EXECUTION, AND IT IS NOT A COORDINATE GREP.
# Architect's ruling, and it was already in the design note (§2.4, B4' step 3): "execute it
# verbatim, assert the re-read is NOT truncated".
#
# THAT IS THE REMEDY'S ACTUAL PROMISE. The warning said the read was cut; the remedy claims
# to get you below the cap; not-truncated is exactly that claim discharged. A COORDINATE GREP
# ASSERTS FIXTURE SHAPE; THIS ASSERTS THE PROPERTY — and it is immune to which bucket the
# coordinate lands in, which is precisely how the previous marker broke.
#
# The history, because two corrections in a row missed and the sequence is the lesson: the
# original `grep -q <coordinate>` over the concatenation was correctly found too WEAK
# (satisfiable by the FIRST re-read alone — N lifted, N executed, ONE certified). The fix, a
# count over the same concatenation with `-ge 2`, moved it from satisfiable-by-one to
# satisfiable-by-NONE and reddened on the first run that reached it: `uzi review backlog
# --run <id>` renders the TODO bucket, and the dismissed coordinate has left todo BY
# CONSTRUCTION, so zero was the correct answer. Neither proposed replacement marker could
# have carried `-ge 2` either — renderBacklog never prints the run id, and `b4-dup` survives
# on ONE of the two runs only. The weakness was real, both coordinate-shaped remedies were
# wrong, and the property was the answer all along.
#
# PER EXECUTION is the half that must not be lost while swapping the marker — but NOT for the
# reason it is tempting to write, and getting this exactly right matters because it is a claim
# about shell semantics:
#
#   * the NEGATIVE (`! grep -q "row cap"`) is ALREADY union-equivalent. If the warning appears
#     in any re-read it appears in the concatenation, so per-file buys it nothing. Measured:
#     a truncated re-read #1 with a clean #2 reddens either way.
#   * the POSITIVE companion is where the split does the work. Over the concatenation,
#     "some output rendered a backlog" is satisfied by re-read #1 alone — so #2 erroring, or
#     printing NOTHING, passes. Measured: both of those go GREEN on a union check and RED
#     per-file.
#
# And the two are a PAIR, which is the point: absence of a truncation warning is trivially
# true of output that does not exist, so the negative is vacuous without a per-execution
# positive beside it. That is why run_printed_instructions writes $PRINTED_OUT.$i.
[ "$PRINTED_N" = 2 ] \
  || fail "$PI_LABEL_TRUNC: $PRINTED_N instruction(s) executed, want 2 — the per-execution assertions below cannot certify what did not run"
for i in 1 2; do
  # truncated == false, read off the CLI's own rendering: renderBacklog prints the "row cap"
  # warning if and only if b.Truncated, so its ABSENCE is the flag being false.
  if grep -q "row cap" "$PRINTED_OUT.$i"; then
    fail "$PI_LABEL_TRUNC: anchored re-read #$i came back TRUNCATED — the anchor did not narrow it below the cap, so the remedy the CLI printed is FALSE:
$(cat "$PRINTED_OUT.$i")"
  fi
  # And it rendered a BACKLOG rather than erroring or printing nothing. Both spellings are
  # legitimate — an empty result is an ANSWER here, not a dead end (nothing on that run is
  # still un-triaged) — so accepting either is correct rather than lax. This is what makes
  # the not-truncated check above meaningful: absence of a warning in EMPTY output would
  # otherwise be satisfied by a command that printed nothing at all.
  grep -qE "groups \(|no recommendations in this bucket" "$PRINTED_OUT.$i" \
    || fail "$PI_LABEL_TRUNC: anchored re-read #$i produced neither a groups listing nor the empty-bucket line, so it did not render a backlog at all — and 'no row cap warning' is trivially true of output that does not exist:
$(cat "$PRINTED_OUT.$i")"
done
# NOT ASSERTED HERE, deliberately: that the re-reads name any particular coordinate. That the
# dismissed one has left todo is the BULK DISPOSITION's property, pinned by the `= 2`
# disposition count above and by the four TestBulkDisposition*LiveDB tests; that a given
# coordinate is still present is fixture shape, which is what the ruling above removes.
pass "$PI_LABEL_TRUNC — 2 remedy lines lifted from the dismiss's own stdout, both executed verbatim, and EACH came back NOT truncated with a rendered backlog"

# --- positive control + teardown ---------------------------------------------
# Delete the bulk review and re-assert BOTH directions. Without this, a truncated:true from
# some unrelated cause is indistinguishable from one this fixture produced — and the
# reappearance of `cut-me` is the second half: it proves the coordinate was cut by the CAP
# rather than being absent for any other reason.
db_psql "DELETE FROM run_reviews WHERE id='$B4_BIG'" >/dev/null
B4_AFTER="$(apiget "/api/me/judge/recommendations?bucket=all")"
echo "$B4_AFTER" | jq -e '.truncated == false' >/dev/null \
  || fail "PRD #98 M8b/B4': deleting the 2001-row review left truncated still true — the flag was not this fixture's, so every assertion above proved nothing about it"
echo "$B4_AFTER" | jq -e --arg c "$B4_CAT" 'any(.groups[]?; .category == $c and .target == "b4-cut-me")' >/dev/null \
  || fail "PRD #98 M8b/B4': the oldest review's coordinate is STILL absent after the cap was removed, so its earlier absence was not the cut"
pass "positive control: bulk review deleted -> truncated=false AND the previously-cut coordinate returns"

# TEARDOWN BY RUN, so the fixture's own runs go too and nothing it created survives into the
# later phases. One DELETE, three cascade levels: runs -> run_reviews (target_run_id ON
# DELETE CASCADE, migration 00059) -> review_recommendations and
# recommendation_dispositions. The bulk review was already deleted by the positive control
# above; its run is removed here with the rest.
db_psql "DELETE FROM runs WHERE id IN ('$B4_RUN_OLD','$B4_RUN_BIG','$B4_RUN_A','$B4_RUN_B')" >/dev/null
# Assert all three levels, and assert them on the CATEGORY, which is exact because no other
# phase uses adjust_template. Asserting the cascaded tables is asserting the CASCADE rather
# than a second DELETE — the point being that a fixture leaving dispositions behind moves
# every later triage count, and one leaving runs behind moves every later run count.
[ "$(db_psql "SELECT count(*) FROM review_recommendations WHERE category='$B4_CAT'")" = 0 ] \
  || fail "PRD #98 M8b/B4': the fixture teardown left $(db_psql "SELECT count(*) FROM review_recommendations WHERE category='$B4_CAT'") $B4_CAT recommendation row(s) behind; later sections would read them"
[ "$(db_psql "SELECT count(*) FROM recommendation_dispositions WHERE category='$B4_CAT'")" = 0 ] \
  || fail "PRD #98 M8b/B4': the teardown left $(db_psql "SELECT count(*) FROM recommendation_dispositions WHERE category='$B4_CAT'") $B4_CAT disposition row(s) behind — the ON DELETE CASCADE from run_reviews did not fire"
[ "$(db_psql "SELECT count(*) FROM runs WHERE issue_iid BETWEEN 990001 AND 990004")" = 0 ] \
  || fail "PRD #98 M8b/B4': the teardown left $(db_psql "SELECT count(*) FROM runs WHERE issue_iid BETWEEN 990001 AND 990004") fixture run(s) behind; every later assertion counting runs would read them"
pass "PRD #98 M8b/B4' fixtures removed — 4 runs, and their reviews, recommendations and dispositions by cascade"

# STILL NOT PROVEN HERE, deliberately: `uzi login`, the device-auth polling hint. It is
# permanently unreachable from a harness — the command declares no flags, it is a
# device-authorization flow, and the hint fires inside the polling loop on a terminal or
# timed-out approval, so executing it verbatim means driving a browser approval. That is
# declared, with that reason, in api/cmd/uzi/instructions_test.go, where an honest
# evidenceNotExecuted is a legal, green and permanent state.
#
# ONE WRITER FOR THIS FILE. That was true while #98's phases were being built and it stays
# true: two concurrent agents editing run-e2e.sh is the conflict the note this replaces
# existed to prevent.

# Restore the default (judge OFF) so later sections' runs are not auto-judged and the
# PRD #42 concurrency capacity math (judge runs count toward worker capacity) is clean.
apiput /api/admin/settings '{"settings":{"judge_enabled":"false"}}' >/dev/null
apiput /api/me/judge '{"enabled":false}' >/dev/null
pass "judge disabled again (global + opt-in) — later sections run unjudged"

# --- PRD #94 triage (dismiss / undo) — DROPPED by PRD #97 M4 ------------------
# The dismiss/undo triage phase used to sit here. Every property it asserted is proven
# at a cheaper layer that runs in CI on every MR:
#   - self-improve backlog EXCLUSION on dismiss and RE-INCLUSION on undo, the
#     status/reason CHECK (dismissed REQUIRES a reason, done FORBIDS one), disposition
#     survival across a re-judge, and the triage join — live Postgres,
#     `api/internal/store/recommendation_dispositions_integration_test.go`
#     (TestRecommendationDispositionsLiveDB), run by `test:api-store-it`;
#   - the HTTP surface (PUT/DELETE → 204, owner-only, enum validation, idempotent
#     double-PUT, double-undo, unknown-rec 404, the disposition on the review DTO and
#     its server-computed stale flag, the triage ladder) —
#     `api/internal/handler/review_disposition_test.go`;
#   - "no spend, no forge write" — TestDispositionTouchesStoreOnly, a positive
#     store-call ALLOWLIST proving the path calls only the owner-resolve reads plus the
#     single disposition write, never a run-create/enqueue or any forge method. That is
#     a structural proof, strictly stronger than this harness's before/after run count
#     and forge-state signature, which could only catch a write that happened to land;
#   - the PER-REVIEW `triage.false_positives` counter this phase read off the review DTO
#     was the one leg with NO lower-layer test (reviewToDTO assembles its own triage
#     rows), so PRD #97 M4 added handler-level TestGetRunReviewPerReviewTriage rather
#     than dropping the property uncovered.

