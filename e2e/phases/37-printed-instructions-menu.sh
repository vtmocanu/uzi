# shellcheck shell=bash
# phase:    printed-instructions-menu
# title:    PRD #98 M8c: printed instructions EXECUTED verbatim from the emitting command's own output
# critical: no
# lane:     gitlab
# executor: any
# requires: F_REVIEW
# provides: -
# handoff:  -
# mutates:  -
# restores: -
# =============================================================================
# PRD #98 M8c — the printed-instruction backstop, EXECUTING half.
#
# WHY THIS EXISTS. Three strings in api/cmd/uzi told a user what command to run next and
# none had ever been run by a test. TWO WERE FALSE, and BOTH PARSED PERFECTLY — a
# hand-written copy of either would have gone green. `instructions_test.go` closes the
# static half (nothing new can be printed without an entry) but it can never verify
# execution: it reads source literals, and a literal cannot say whether the command works.
# This phase is the only place in the repo where the printed text is EXECUTED.
#
# WHY HERE and not a build-tagged Go test: the instructions are emitted against a LIVE api
# (the undo addresses come from the server's `settled` array), so a Go test would need this
# same booted stack — at which point it is this harness with more machinery. The harness
# already has `uzi_cli` (:1407, hermetic under env -i), `db_psql`, and — by this line — a
# judged run with a review. NO NEW ENV VAR: the :103-107 allowlist is untouched, because
# widening it is the change that made two e2e runs meaningless on 2026-07-17.
#
# THE ONE RULE: extract from the EMITTING COMMAND'S OWN OUTPUT, never a hand-written argv.
say "PRD #98 M8c: printed instructions EXECUTED verbatim from the emitting command's own output"

# run_printed_instructions LABEL SHAPE WANT OUT — the shared runner every row goes through.
#
#   OUT   the emitting command's own captured output (stdout AND stderr — two of the three
#         instructions below arrive via Exitf, i.e. on stderr with a non-zero exit).
#   SHAPE an ERE describing the WHOLE instruction, UUID-shaped where applicable.
#   WANT  the exact number of instructions the row expects.
#
# Four mechanisms, in descending strength:
#  1. ONE helper. A row that hand-writes argv has to visibly bypass this function, which is
#     reviewable in a way that a subtly-wrong string is not.
#  2. A SHAPE-GUARDED eval. `eval` never sees text that did not come out of the command in
#     the expected form — which is what makes it safe here rather than reckless. It is also
#     what keeps the execution VERBATIM: any hand-splitting reintroduces the copy the
#     mechanism exists to forbid.
#  3. The COUNT is asserted before any match is used. Never `head -1`: output that stops at
#     your limit is indistinguishable from output that ended.
#  4. A CHARACTER ALLOWLIST INSIDE THE HELPER, checked immediately before the eval.
#
# WHY 4 EXISTS WHEN 2 ALREADY GUARDS THE SHAPE. It is not that the helper ignores content —
# every match already satisfied the caller's ERE, and the `case` below already requires the
# span to start `uzi `. The exposure is that the ENTIRE safety burden sat on each caller's
# `$shape`, with NO FLOOR in the helper: a future row passing a loose ERE (`.*`, an
# unanchored class, a `[^ ]+` that happens to admit a metacharacter) hands unreviewed text
# to an `eval` that runs in the HARNESS's own shell on the developer's host — before
# uzi_cli's `env -i`, so an injected `;` runs as the user rather than inside the sandbox.
# All FOUR shapes today are closed EREs, which is why this is a floor and not a fix. (A count
# in a comment is a thing this wave keeps deciding not to write; it is here because the
# argument depends on EVERY caller being closed, so the number is the claim, not decoration —
# and it was already wrong once, saying three after the fourth row landed.)
#
# IT IS AN ALLOWLIST, NOT A BLACKLIST, and that is the whole point. Blacklisting shell
# metacharacters is famously incomplete — you find out which one you forgot by being bitten
# by it. A positive class excludes `< > | ; $ backtick ' " \ newline` and every glob
# character BY CONSTRUCTION, without anyone having to enumerate them.
#
# THIS CLASS IS NARROWER THAN THE STATIC EXTRACTOR'S, DELIBERATELY, AND THE GAP HAS A
# CONSEQUENCE. api/cmd/uzi/instructions_test.go's instructionRE reads SOURCE literals, so it
# must admit format verbs and placeholders (`%<>-`); this one reads EMITTED text, which has
# to be runnable verbatim in a shell. So an instruction whose emitted form carries any
# character outside this class can be REGISTERED there and never EXECUTED here —
# evidenceNotExecuted by construction rather than by choice. `%`, `+`, `@`, `,` and `~` are
# all outside it, so a judge target containing one would fail here with a message about the
# allowlist rather than about the target. Loud rather than silent, and low probability. The
# extractor's comment points back at this one.
#
# SCOPE, so a later comment does not blur it: this is a SHELL-INJECTION floor, not an
# AUTHORIZATION one. It cannot stop an admitted span from naming a destructive `uzi`
# subcommand — nothing here inspects the verb. That is bounded today by the three explicit
# shapes each caller passes plus each row's own outcome assertion, not by this check.
#
# The property worth naming: it makes the wrong option STRUCTURALLY IMPOSSIBLE rather than
# discouraged. A row that tried to substitute a placeholder into the printed text cannot
# pass this floor, because `<` cannot pass it. That is why the sibling change in review.go
# had to be a real format verb rather than a helper special case.
#
# HONEST RESIDUAL, stated rather than papered over, AND NOT CLOSED BY 4: shell cannot make
# the shortcut STRUCTURALLY unavailable. A determined author can still assign a literal to
# the variable passed as OUT, and the allowlist would happily accept it. What these four buy
# is that the shortcut becomes visible in review rather than invisible in a passing test.
# That is a real improvement and it is not the same as impossible.
#
# The `|| fail` on the exec below is a FLOOR (an instruction that errors is definitionally
# false), not the row's assertion — every caller asserts an OUTCOME afterwards.
PRINTED_OUT="$RUNROOT/.printed-instruction.out"
# --- arrange: one coordinate on TWO reviews ----------------------------------
# The undo row needs the group dismiss to settle TWO members, because the count assertion
# is the mechanism and a count of 1 makes it indistinguishable from `head -1`. Two members
# means two REVIEWS: BulkSetDispositions resolves with SELECT DISTINCT ON (rv.id,
# rr.category, rr.target), so the same coordinate twice on ONE review collapses to one
# member. Rather than pay ~2 min for a second judged run, direct-seed a review on a run
# that completed earlier (RUN_CLI, :1426) — the harness's own direct-seed fixture pattern
# (:2732). user_id is copied off F_REVIEW so ownership cannot drift from the token driving
# uzi_cli.
PI_CAT=improve_agent
PI_TGT=e2e-printed-instruction
PI_TODO_PRESEED="$(uzi_cli review stats --json | jq -r '.todo')"
[ -n "$PI_TODO_PRESEED" ] && [ "$PI_TODO_PRESEED" != null ] \
  || fail "PRD #98 M8c: could not read the pre-seed triage todo count via uzi review stats --json"
db_psql "INSERT INTO run_reviews (target_run_id, user_id, verdict, summary_md)
         SELECT '$RUN_CLI', user_id, 'ok', 'PRD #98 M8c printed-instruction fixture'
         FROM run_reviews WHERE id='$F_REVIEW'" >/dev/null
# NOT `RETURNING id`, and this is a measured trap rather than a style choice: db_psql is
# `psql -tAc … | tr -d '\r\n'`, and psql writes the command TAG to stdout alongside the
# returned row, so `tr` welds them into `<uuid>INSERT 0 1` — a string that is non-empty,
# passes a bare -n guard, and only explodes three statements later inside an unrelated
# INSERT. Read the id back with a SELECT, and assert its SHAPE, not merely that it is set.
PI_REVIEW2="$(db_psql "SELECT id FROM run_reviews WHERE target_run_id='$RUN_CLI'")"
printf '%s' "$PI_REVIEW2" | grep -qE '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' \
  || fail "PRD #98 M8c: the seeded review id is not a bare uuid: '$PI_REVIEW2'"
db_psql "INSERT INTO review_recommendations (review_id, category, target, rationale_md, confidence)
         VALUES ('$F_REVIEW','$PI_CAT','$PI_TGT','printed-instruction fixture: member A','low'),
                ('$PI_REVIEW2','$PI_CAT','$PI_TGT','printed-instruction fixture: member B','low')" >/dev/null

# PRECONDITIONS, or the row is vacuous. Two rows on two DISTINCT reviews is what makes
# "exactly 2 printed addresses" a property of the code rather than of the fixture; and a
# pre-existing disposition would make the post-undo "0 rows" assertion pass without the
# undo having done anything.
[ "$(db_psql "SELECT count(DISTINCT review_id) FROM review_recommendations WHERE category='$PI_CAT' AND target='$PI_TGT'")" = 2 ] \
  || fail "PRD #98 M8c: the fixture coordinate must span exactly 2 reviews, or the group dismiss settles fewer members than the row asserts"
[ "$(db_psql "SELECT count(*) FROM recommendation_dispositions WHERE category='$PI_CAT' AND target='$PI_TGT'")" = 0 ] \
  || fail "PRD #98 M8c: the fixture coordinate already carries a disposition — the undo outcome assertion would be vacuous"
PI_TODO_BEFORE="$(uzi_cli review stats --json | jq -r '.todo')"
[ "$PI_TODO_BEFORE" = "$((PI_TODO_PRESEED + 2))" ] \
  || fail "PRD #98 M8c: seeding the coordinate did not raise the wire triage todo by 2 ($PI_TODO_PRESEED -> $PI_TODO_BEFORE) — the fixture never reached the API"
pass "seeded one coordinate ($PI_CAT/$PI_TGT) across 2 reviews; wire todo $PI_TODO_PRESEED -> $PI_TODO_BEFORE"

# --- printed-instruction row: uzi review undo --------------------------------
# The flagship. runGroupDisposition prints one undo address per settled member; both are
# lifted from THAT command's stdout and run verbatim.
PI_LABEL_UNDO="printed-instruction row: uzi review undo"
PI_OUT_UNDO="$(uzi_cli review dismiss --category "$PI_CAT" --target "$PI_TGT" --reason wont-do)" \
  || fail "$PI_LABEL_UNDO: the group dismiss that EMITS the instruction failed (exit $?)"
[ "$(db_psql "SELECT count(*) FROM recommendation_dispositions WHERE category='$PI_CAT' AND target='$PI_TGT'")" = 2 ] \
  || fail "$PI_LABEL_UNDO: the group dismiss did not write 2 dispositions, so there are not 2 addresses to undo. Output was:
$PI_OUT_UNDO"
run_printed_instructions "$PI_LABEL_UNDO" 'uzi review undo [0-9a-f-]{36} [0-9a-f-]{36}' 2 "$PI_OUT_UNDO"
# THE OUTCOME, not the exit code: both dispositions gone, and the wire's own triage count
# back where it started. A `uzi review undo` that exited 0 and deleted nothing fails here.
[ "$(db_psql "SELECT count(*) FROM recommendation_dispositions WHERE category='$PI_CAT' AND target='$PI_TGT'")" = 0 ] \
  || fail "$PI_LABEL_UNDO: the printed undo addresses ran clean but left $(db_psql "SELECT count(*) FROM recommendation_dispositions WHERE category='$PI_CAT' AND target='$PI_TGT'") disposition row(s) behind"
PI_TODO_AFTER="$(uzi_cli review stats --json | jq -r '.todo')"
[ "$PI_TODO_AFTER" = "$PI_TODO_BEFORE" ] \
  || fail "$PI_LABEL_UNDO: triage todo did not return to $PI_TODO_BEFORE after executing both printed undo addresses (got $PI_TODO_AFTER)"
pass "$PI_LABEL_UNDO — 2 addresses lifted from the dismiss's own stdout, both executed verbatim, both dispositions gone and todo back to $PI_TODO_BEFORE"

# --- printed-instruction row: uzi review show --------------------------------
# resolveRecID's refresh hint, emitted through Exitf — STDERR, exit 4 (ExitNotFound). The
# naive form of this row would abort the whole harness under `set -euo pipefail`, and a row
# that "passes" because it never ran is the exact false green this mechanism exists to
# prevent: capture stderr and tolerate the non-zero exit explicitly.
PI_LABEL_SHOW="printed-instruction row: uzi review show"
PI_RC=0
PI_OUT_SHOW="$(uzi_cli review resolve "$J_RUN" 00000000-0000-0000-0000-000000000000 2>&1)" || PI_RC=$?
# The exit code here is ARRANGE, not the assertion: it is how we know the no-match branch
# that emits the hint is the branch that ran. 0 would mean the bogus id matched something.
[ "$PI_RC" = 4 ] \
  || fail "$PI_LABEL_SHOW: expected exit 4 (ExitNotFound) from resolving a bogus rec id, got $PI_RC. Output was:
$PI_OUT_SHOW"
run_printed_instructions "$PI_LABEL_SHOW" 'uzi review show [0-9a-f-]{36}' 1 "$PI_OUT_SHOW"
# THE OUTCOME: the refreshed read names the coordinate seeded on THAT run's review. Exit 0
# alone would also be satisfied by `review show` printing "not judged".
grep -q "recommendations (" "$PRINTED_OUT" \
  || fail "$PI_LABEL_SHOW: the printed refresh command ran but rendered no recommendations block:
$(cat "$PRINTED_OUT")"
grep -q "$PI_TGT" "$PRINTED_OUT" \
  || fail "$PI_LABEL_SHOW: the printed refresh command did not name the $PI_TGT coordinate that lives on run $J_RUN's review — it read the wrong run:
$(cat "$PRINTED_OUT")"
pass "$PI_LABEL_SHOW — hint lifted from Exitf's STDERR (exit 4 tolerated), executed verbatim, and its output names the coordinate on run $J_RUN"

# --- printed-instruction row: uzi repo list ----------------------------------
# `uzi run create` with --repo omitted: Exitf(ExitUsage) — STDERR, exit 2, and no byte
# crosses the wire (the check runs before a client is built).
PI_LABEL_REPO="printed-instruction row: uzi repo list"
PI_RC=0
PI_OUT_REPO="$(uzi_cli run create --issue 1 2>&1)" || PI_RC=$?
[ "$PI_RC" = 2 ] \
  || fail "$PI_LABEL_REPO: expected exit 2 (ExitUsage) from \`uzi run create\` without --repo, got $PI_RC. Output was:
$PI_OUT_REPO"
run_printed_instructions "$PI_LABEL_REPO" 'uzi repo list' 1 "$PI_OUT_REPO"
# THE OUTCOME: the instruction hands back an id that answers the question that produced it.
grep -q "$REPO_ID" "$PRINTED_OUT" \
  || fail "$PI_LABEL_REPO: the printed instruction ran but its output does not name the enabled repo id $REPO_ID it exists to supply:
$(cat "$PRINTED_OUT")"
pass "$PI_LABEL_REPO — instruction lifted from Exitf's STDERR (exit 2 tolerated), executed verbatim, and it names repo $REPO_ID"

# --- containment + positive control ------------------------------------------
# Remove the fixture so later sections see the triage counts they would have seen without
# it, and assert the count returns to the PRE-SEED value. That delete-and-recheck is the
# positive control for the +2/-2 arithmetic above: without it, a todo count that moved for
# some unrelated reason is indistinguishable from one this fixture moved.
db_psql "DELETE FROM run_reviews WHERE id='$PI_REVIEW2'" >/dev/null
db_psql "DELETE FROM review_recommendations WHERE review_id='$F_REVIEW' AND category='$PI_CAT' AND target='$PI_TGT'" >/dev/null
PI_TODO_CLEAN="$(uzi_cli review stats --json | jq -r '.todo')"
[ "$PI_TODO_CLEAN" = "$PI_TODO_PRESEED" ] \
  || fail "PRD #98 M8c: after deleting the fixture the wire triage todo is $PI_TODO_CLEAN, not the pre-seed $PI_TODO_PRESEED — the fixture was not what moved the count, so the +2/-2 assertions above proved nothing about it"
pass "printed-instruction fixture removed; wire todo back to the pre-seed $PI_TODO_PRESEED (positive control for the +2/-2 arithmetic)"

