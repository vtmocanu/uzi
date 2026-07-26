# Agent team workflow for uzi

Generated 2026-07-03 by the `agent-team` skill (roster adapted from the example-app team).

## Team roster

| Role | Subagent type | Model | Tools |
|------|---------------|-------|-------|
| architect | architect | opus | Bash, Read, Grep, Glob, WebFetch, WebSearch, Edit, Write + team tools |
| coder | coder | opus | (inherit) |
| reviewer | reviewer | opus | Bash, Read, Grep, Glob, WebFetch + team tools |
| auditor | auditor | opus | Bash, Read, Grep, Glob, WebFetch + team tools |
| tester | tester | opus | Bash, Read, Grep, Glob, WebFetch + team tools |
| spec-keeper | spec-keeper | opus | Bash, Read, Grep, Glob, Edit, Write + team tools |
| fact-checker | fact-checker | opus | Bash, Read, Grep, Glob, WebFetch, WebSearch + team tools |
| documenter | documenter | sonnet | Bash, Read, Grep, Glob, Edit, Write, WebFetch + team tools |
| web-ux | web-ux | opus | Bash, Read, Grep, Glob, WebFetch + team tools |
| researcher | researcher | opus | Bash, Read, Grep, Glob, WebFetch, WebSearch + team tools |
| release | release | sonnet | Bash, Read, Grep, Glob + team tools |

## Orchestrator workflow

You (the team lead) NEVER do implementation, review, or audit work yourself.
You coordinate the team via Agent (name + subagent_type) + SendMessage +
the Task* tools.

Default flow for a typical task:
0. For a non-trivial task (new component, cross-cutting change, new or changed
   contract/interface), dispatch architect BEFORE the coder and fold its design
   summary into the coder's spawn prompt; skip it for small fixes. Also dispatch
   it whenever a PRD is being written or reviewed (including `/prd-create`
   flows): it contributes the architecture sections and the milestone
   dependency graph when writing, and judges feasibility, hidden milestone
   coupling, and independent shippability when reviewing. Open design questions
   it flags go to the user, not to the coder as guesses.
1. Spawn coder with the full task context. The coder runs the project's
   test/lint gates before reporting done: `cd api && go test -count=1 ./...`;
   `cd web && npm test && npm run typecheck`;
   `cd agent && npm test && npm run typecheck` (plus `./e2e/run-e2e.sh` +
   `./scripts/smoke.sh` for stack-level changes).
2. After coder reports done, spawn reviewer + auditor IN PARALLEL with
   coder's diff + report (pin to commit SHAs). Dispatch fact-checker in the
   same wave when the change touches claim-bearing artifacts (README,
   plan.md, specs, "we beat inspiration X" claims); REFUTED claims are
   blocking. Add architect to this wave for an architectural-fit pass when the
   change moved boundaries.
3. Dispatch tester on the scenario surface when behavior changed.
4. Resolve any blocking findings (route them back to coder via SendMessage).
5. Once blocking findings are resolved, dispatch spec-keeper with the change
   summary plus a user-vs-AI provenance breakdown (lead work — only the lead
   has seen the conversation). specs/ai.md applies directly; specs/human.md
   edits go to the user for confirmation first.

## Context handoff (CRITICAL)

Every teammate cold-starts with no memory of prior conversation or other
teammates' outputs. Whatever you write in the spawn `prompt:` is the entire
context they have, plus the body of `.claude/agents/<role>.md`.

Therefore every spawn prompt MUST include:
- File paths the teammate should read (the spec, the files being modified)
- A summary of any prior teammate's findings when chaining workers
- The exact error message when retrying after a failure
- If context is long, write it to `.claude/agent-team-tasks/<slug>.md` and
  reference that path in the prompt instead of pasting inline

## Re-derive the claim at the moment you assert it (CRITICAL)

**Rule: re-derive a claim from the code at the moment you assert it, however
sure you are.** Having verified something once is not knowing it — you verified
a *past* state, and you assert in the present. This applies to every role.

**A comment is an assertion, so it deserves the same mutation as a test.**
Freeze the field, drop the line, move the path, and watch the assertion fail.
If nothing fails, the comment is describing a mechanism that is not there.

Earned the hard way on PRD #58 (2026-07-16), where **nine claims fell over**, all
believed by someone competent, each disproved in seconds once someone ran it:

- The PRD said quota enforcement was atomic. Measured: with the lock removed,
  **8 of 8 concurrent provisions passed a quota of 2**. Its stated mechanism was
  a guarded insert; the real one is the advisory lock.
- The design said one test caught a misplaced lock. Mutation: it stays **green**
  — a misplaced lock still blocks, so a blocking-assertion cannot see placement.
- The design said only a browser could prove a gate escapable. A page-level test
  does it; the blindness was the *component's*, not the boundary's.
- Three code comments named mechanisms the code did not have (a `seq` nothing
  read; a live region "covering" users it cannot; a `?raw` benefit whose
  corollary was omitted). **The logic was right every time — the story was wrong.**
- A test-count baseline was carried from memory (641 vs the real 612).
- A handoff note outlived the fix that killed it, and was reported as open twice.
- A browser pass "verified" a `title` attribute that reaches **no** screen-reader
  user — it checked *presence*, not *efficacy*.

The root, from the coder that made four of them: *"I trusted any claim I had
personally verified once, and stopped re-checking it, because having checked it
felt like knowing it."*

**Where it hides: the artifacts with no gate on them.** Comments get read in
review, tests get run, commit messages get diffed. A "still open" list, a
checkpoint, a handoff note — that is prose nobody executes, and it is what
decides where the next person spends their time. Re-derive those too.

**Corollaries worth knowing:**
- **Presence ≠ efficacy.** "The attribute is there" and "it reaches anyone" are
  different claims. Two validators can both be right and disagree, because they
  asked different questions. When two reports conflict, find the two questions
  before picking a winner.
- **Correctness ≠ reachability — a DIFFERENT axis from presence, and the reason "write a
  better test" does not close this class.** Asserting an attribute's *exact value* is
  strictly stronger than asserting its presence, and **still** satisfiable while the
  information reaches nobody: strength of assertion is orthogonal to reachability. Measured
  2026-07-26 (PRD #113 M5): `upgrade_detail` reached the user only through a `title`
  attribute, the detail strip rendered only for `upgrade_failed`, and `outdated` was *also*
  an alert state — so one of the two states the nav badge counts had its entire explanation
  in a hover: no keyboard, no touch, inconsistent for screen readers. The test asserted the
  `title`'s exact string and passed, as it should have. **jsdom can verify an attribute is
  correct; it cannot verify anyone can reach it.** Named by the coder who wrote the passing
  test, about its own test.
- **The experiment that justifies a choice usually also bounds it.** Record both
  halves, not the flattering one.

### Traps in this repo that cost real time

- **`expect(document.activeElement?.textContent).toMatch(...)` is vacuous** — on
  `<body>`, `textContent` is the whole page, so it matches anything. Assert
  **identity** (`toBe(el)`), and cross-check text: identity alone gives false
  *negatives* when a selector drifts, text alone gives false *positives*. **The
  disagreement between them is the signal.**
- **`web/` has two `role="status"` regions** — `RateLimitAnnouncer` (app-wide,
  always-present, empty) comes first in the DOM, so any `querySelector("[role=status]")`
  silently grabs the wrong one. Selector-by-role here is ambiguous by construction.
- **`${PIPESTATUS[0]}` IS A BASH-ISM AND THIS SHELL IS zsh — IT EXPANDS TO NOTHING,
  SILENTLY.** zsh's array is `$pipestatus` and it is **1-indexed** (`$pipestatus[1]` is the
  first command; bash's `PIPESTATUS` is 0-indexed). In zsh, `${PIPESTATUS[0]}` is simply an
  unset variable: no error, no warning, an empty string. Verified in zsh 5.9 — `false | true`
  gives zsh `pipestatus=(1 0)` and bash `PIPESTATUS=(1 0)` at `[0]`.
  **Why this earns a traps entry rather than a footnote: it fails in a VERIFICATION step.** A
  bash-ism in a *command* fails loudly and you fix it. A bash-ism in the *check that proves
  the command passed* fails quiet, and the surrounding output still looks like success.
  Measured on PRD #98's merged tip: a `npm run build` run ended with
  `echo "=== BUILD EXIT: ${PIPESTATUS[0]} ==="`, which printed `=== BUILD EXIT:  ===` directly
  beneath `✓ built in 3.19s`. The gate had genuinely passed — but nothing in that output
  proved it, and the line written to prove it contributed nothing.
  **`$pipestatus` IS CLOBBERED BY THE VERY NEXT COMMAND, INCLUDING A PLAIN `echo` — capture it
  on the same line or it lies.** This is the half that makes the trap dangerous rather than
  merely useless, and it was found while verifying the entry above: a first attempt read
  `$pipestatus[1]` after an intervening `echo` and got **`0`** — a plausible, passing-looking
  number describing the *echo*, not the pipeline. So the empty-string form is the LUCKY
  failure (an empty string where a `0` belongs is visible); the correct-array-wrong-moment
  form is the silent one, and it is exactly the "stale `0` would have shipped" case. Capture
  with `a=("${pipestatus[@]}")` immediately, or do not pipe at all.
  **WHY that capture line is legal when the sentence above says "the very next command resets
  it" — and the obvious explanation is wrong.** Measured, zsh 5.9, four cases: a SCALAR
  assignment (`x=1`, `s="${pipestatus[1]}"`) does **not** reset `$pipestatus`, but an ARRAY
  assignment (`a=(1 2)`, and `a=("${pipestatus[@]}")` itself) **does** — it leaves
  `pipestatus=(0)`. So "commands reset it, assignments do not" is FALSE for exactly the form
  the fix uses. The capture works for a different reason: **the right-hand side is expanded
  before the assignment takes effect**, so `a` gets `(1 0)` and *then* the array is reset.
  **Consequence with teeth: you get exactly ONE capture.** A second
  `b=("${pipestatus[@]}")` reads `(0)` — plausible, passing-looking, wrong. After the capture,
  read `a`, never `$pipestatus` again.
  **The robust form: do not pipe when you need the exit status.** Redirect to a file, capture
  `$?` on the very next line, then grep the file:
  ```sh
  npm run build > /tmp/build.log 2>&1
  echo "BUILD EXIT CODE: $?"          # $? is unambiguous in both shells
  grep -E "check-docs|✓ built" /tmp/build.log
  ```
  This also gives you the second half for free: grepping the log for the stage you care about
  proves that stage RAN, not merely that the wrapper exited 0.
  **The honest part, and the reason this is not a story about being careful:** the lesson is
  not "watch for bash-isms", it is that **a verification step needs its own positive control**
  — the same demand made of a live-DB run (`PASS>0`, `SKIP=0`) and of a mutation (assert it
  applied). **Third variant of one shape on this branch**, alongside the fold that produced no
  log file and the grep that ran against the wrong working tree: *the verification mechanism
  failed silently while the thing it was verifying looked fine.* In all three, the output of a
  broken check is indistinguishable from the output of a passing one.
- **A green Go suite can mean nothing ran.** Every `*LiveDB` test skips without
  `UZI_TEST_DATABASE_URL` and the package still prints `ok` — **51 of them were
  skipping in CI, silently, since they were written.** Check tests *ran*, not
  just passed. `test:api-store-it` now fails on zero-passed or any-skipped for
  exactly this reason. **Re-measured on PRD #98 (2026-07-21) because three agents
  had each leaned on weaker evidence anyway:** with the var unset the sweep exits
  `0`, both packages print `ok`, and the tally is `RUN=n PASS=0 SKIP=n` — every
  test in the suite ran nothing. (It was `108` that day and `128` within hours.
  **The run count is whatever the suite holds when you read this; `PASS=0` is the
  finding.**)
  Exit code and "no failures printed" are *both* satisfiable by a run in which
  not one assertion executed. Require a **positive control** — the named test
  appears as `--- PASS`/`--- FAIL`, zero `--- SKIP`, `RUN > 0` — and treat any
  run failing that as INVALID rather than green. See `CLAUDE.md`'s api section
  for the operational form.
  **The positive control catches TWO of the three false-green mechanisms, not
  three, and the distinction is load-bearing.** It catches the skipped suite and
  the run that never happened. It **cannot** catch a mutation that silently
  failed to apply, and no property of the *run* can: the suite genuinely runs,
  every assertion genuinely executes, the control passes cleanly, and the result
  is green because the code under test was never mutated. Only comparing the
  TREE sees that one — which is why "assert the mutation actually applied" is a
  **separate** standing rule below and must stay one. A reader who believes the
  control covers all three will drop the tree comparison as duplicated effort,
  and that is the one of the three that has already produced a false green here.
- **A fixture whose users or runs DELIBERATELY SHARE coordinate strings pins the
  `review_id` join halves FOR FREE — and "tidying" it silently deletes that
  coverage.** The pinning comes from the row-count guards (`rows != 2`,
  `got N rows want 2`), not from any assertion written for it: with shared
  coordinates, dropping a `review_id` half lets rows from *other* reviews attach,
  and the count guard fires. Giving each user or run its own distinct coordinate
  strings looks like tidying and removes the cross-match entirely. Three fixtures
  on PRD #98 depend on this accident (the badge test, the fourth site's
  coordinate test, and the cross-review coordinate in the big backlog test); two
  carry a local do-not-tidy note, but **the property is the durable statement and
  a local note on two of three is how the third gets tidied.**
  **Cite the MECHANISM, never the tally.** At the fourth site, dropping either
  `review_id` half reddens with *"userFT: got 5 rows, want exactly 2"* — and the
  finding is that side rows from OTHER USERS' reviews attached, i.e. a
  cross-TENANT match caught by a guard written for something else. The `5`
  depends on what else the shared database holds; the cross-tenant attach does
  not. Corroborated independently by `TestBulkDispositionFansOutAcrossRunsLiveDB`
  failing with `triage = {Total:12}` against `want 6` — the #94 stats endpoint,
  the `triage.todo` consumer, reading visibly inflated counts under the same
  fold. Same reason assertion *counts* are not citable: a tally drifts exactly
  like a line number.

## Citing and dispatching across a moving tree (CRITICAL)

Several agents commit against one worktree, so **the tree a claim was read from
may already be gone by the time the claim is acted on.** Two rules, both earned
on PRD #98 (2026-07-21), both by incidents rather than by argument.

**0. THE DISPATCHER'S HALF, and on PRD #108 it was the branch's DOMINANT failure
mode — bigger than any code defect.** Rules 1-4 below are all written for the
RECIPIENT ("read the file before acting"). That leaves the cheaper fix unstated,
and the gap cost three round-trips in one wave: a reviewer reported that three of
its last four reports crossed with a fix or a briefing describing a tree that had
already moved, and **every substantive lead-vs-validator disagreement in the whole
run resolved to "both correct for the SHA each of us read."**

**Name the tip in the dispatch, and add: "take the live tip if it has moved past
this, and say so rather than guessing."** One clause. It would have prevented all
three. Corollary the lead kept relearning: **verify the artifact immediately
before SENDING, not before COMPOSING** — three of five crossings on PRD #108 were
a lead verifying at SHA `N`, writing a long dispatch, and sending it after the
worker reached `N+1`.

Two lead-side failures worth naming because neither is a stale-read:
- **Asserting a completed action from having INSTRUCTED it.** The lead told a
  validator "the coder restaged the outage test" — it had only told the coder to.
  Measured: that file changed 202 lines and the test was byte-identical.
- **Reading absence as "elsewhere" instead of "absent".** A grep for a symbol
  returned nothing and the lead concluded the code must live somewhere it had not
  looked, then briefed two agents on it. The symbol did not exist, and its absence
  WAS the blocking defect — the inverse of "a failed grep is not evidence."

**0b. WHEN A LIVE-TREE MEASUREMENT DISAGREES WITH A PINNED ONE, THE FIRST HYPOTHESIS IS
MID-EDIT — NOT DEFECT.** The disposition half of EARLY-vs-STALE, and it is where a correct
attribution still produces a wrong action. Measured 2026-07-26 (PRD #113 M5), by a validator
who had done the pinning correctly: it proved two web failures were *not* the pinned SHA's
fault, and then treated "not this SHA's fault" as "therefore someone must act" — escalating
URGENT. The third possibility, that the tree was mid-edit between two commits of one logical
change, was both likelier and cheaper to test than either defect hypothesis. A rerun sixty
seconds later showed 1033/1033. **Eliminating one cause does not leave only one; enumerate
three (this SHA, elsewhere, mid-edit) and test the cheapest first.** For EARLY the disposition
is *re-probe*, never escalate — and confirming it costs one rerun. Three round-trips on this
branch went to this shape, two of them the lead's, once mis-attributed to a truncated grep,
which is the familiar trap hiding the unfamiliar one.

**1. An instruction to change a file is a CLAIM about that file's current
contents, and it EXPIRES.** Read the file before acting on any dispatch that
quotes it, names a line number, or says a fix "did not land". Evidence:

- The **team lead** re-dispatched a correction for a superseded comment at
  `:643`, stating it "is not in your report" — it had landed one commit earlier;
  the lead had read a stale working tree. The dispatch's own subject was an item
  that had crossed in flight, and it had itself crossed.
- The **"fixed container name"** for `e2e/run-store-it.sh`: asserted by an
  implementer, relayed by the lead as an instruction, and accepted by an auditor
  **who had the two-line file open earlier in the same session**. Three agents,
  none of whom opened it. False — the name is `uzi-store-it-$$`.

The mechanism that caught both: **the recipient opened the file before acting**,
instead of assuming the instructor's read was current. Note the asymmetry that
makes this a standing rule rather than general care — the instructor is reading a
tree the recipient is actively changing, so the instructor's read is the one that
goes stale, and the recipient is the only one positioned to notice. Agreement is
when a claim gets checked least, and an instruction is agreement's most
authoritative form.

**1b. A MEASUREMENT'S SHELF LIFE IS SET BY WHAT IT MEASURED — and getting this
wrong is what makes rule 1 too expensive to keep.**

- A claim about a **COMMENT** expires on the next commit that touches that
  comment, which on a branch doing comment corrections is roughly every commit.
  One expired *within a single commit* on PRD #98. Re-read HEAD immediately
  before sending.
- A claim about a QUERY'S BEHAVIOUR expires when anything it executed against changes — the
  query, the fixture, the assertions, and the ENVIRONMENT. `git diff <measured-sha>..HEAD --
  <those paths>` can only FALSIFY a measurement, never confirm one: a changed path proves it
  stale, an unchanged path proves nothing about the environment it ran against. Use it to stop
  early, never to conclude. The environment is the part that moves without appearing in any
  diff you would think to run: the migration set that applies before yours, the Postgres
  version, the sqlc version. Measured on PRD #98's landing merge (2026-07-21): the five files
  carrying the tenant-boundary pins were BYTE-IDENTICAL across the merge while FIVE new
  migrations landed ahead of ours, so every fold on the branch had been measured against DDL
  the suite no longer ran on. A green suite does not close this — passing proves the tests
  pass, not that they would still FAIL if the code were wrong, and four green gates were in
  hand at the time. The pins were re-folded and all three still reddened. THAT IS THE REASON TO
  RE-FOLD, NOT A REASON TO SKIP IT: "they survived last time" is the inference this entry
  exists to prevent.

**A measurement is bound to a WORKING TREE as well as to a SHA, and a persisted `cd` silently
rebinds it.** Evidence: an auditor verified a "zero hits" grep from a shell whose working
directory still pointed at its own detached worktree, so it measured a clean pre-merge tree
while believing it was measuring the live one — the conclusion happened to be right, the
method was luck. It earns its line because **the failure is invisible in the output**: a grep
against the wrong tree looks exactly like a grep against the right one. Applies to everyone
whose shell has a persisted working directory, which is everyone running verification greps.
**Corollary — a tree with `MERGE_HEAD` set is NEITHER state and cannot support a tally.** File
lists and mechanisms survive from a mid-merge tree; counts do not. Cite the former, re-derive
the latter on the committed merge.

**A FAILED GREP IS NOT EVIDENCE THE TEXT IS ABSENT.** A quoted anchor can be present and
unmatchable — wrapped across a line break, reflowed, or differing in whitespace. Read the
section before reporting "not found". Same family as *a measurement is bound to a working
tree*: in both, the tool's SILENCE reads as a finding when it is only a limit of the query.
Evidence: the lead quoted an anchor for this very amendment that did not match the coder's
grep because the sentence wrapped; the coder read the section and landed it correctly instead
of replying "anchor not found".

**A TRUNCATED VIEW IS NOT THE OUTPUT.** `| head -N` / `| tail -N` produce something that
looks complete — output that stops at your limit is indistinguishable from output that
stopped because it ended. Two instances on PRD #98's landing, on the same `00075` query,
one published (three migrations when the same `--stat` line said six) and one caught by
re-running unbounded (12 shown, 17 real). Count with `rg -c`, `wc -l` or `--stat`'s own
summary and reconcile it against the rows you can see; never let a bounded view be the
basis of a count.
**What makes this fixable rather than a hazard to be careful about: in both instances the
disproof was ON SCREEN.** `--stat`'s summary line said six while three rows were visible;
`rg`'s own counts were available. Unlike the working-tree trap the evidence is right there,
and `head`/`tail` discard it. That is the third member of this family — the failed grep is
the EMPTY case, this is the NON-EMPTY-BUT-PARTIAL case, and partial is worse because empty
at least looks like nothing.
**The honest asymmetry, recorded because it is what makes this a class rather than one
person's slip:** one validator PUBLISHED its wrong count; the other caught its own by
re-running unbounded before quoting numbers, then withheld them anyway because the tree was
mid-merge. One error and one near-miss, same query, same afternoon — **and a third instance
in the coder's own PRD text**, which cited *"six … plus four … all ten"* while mixing line
counts with file counts, and which the merge silently understated by adding two more archived
files. Three people, one query, one day.

Worked example, both halves from one day: the auditor reported a comment gap at
`c1fcdfce` that `a2b554a6` had already closed — genuinely stale. Its **fold**
results from the same run still stood, and note carefully WHAT that took, because the
path diff was only half of it:
`git diff c1fcdfce..HEAD -- api/internal/store/recommendation_dispositions_integration_test.go`
was comment-only — no SQL, no fixture row, no assertion changed — **and** no migration moved
in that window, so the environment was constant too (verified after the fact:
`git diff c1fcdfce..965d7b3e -- internal/store/migrations/` is empty). The comment-only diff
alone would NOT have licensed the conclusion; it merely failed to falsify it. Had a migration
landed in between — as five did at the landing merge — the same empty diff would have
accompanied a dead measurement.

**The point of the entry is the cost.** Without the split, the honest response to
"your tree was stale" is to re-run eleven folds that nothing invalidated — and a
rule expensive enough to get abandoned protects nothing.

**1b-i. DATING A NUMBER IS NOT THE SAME AS BOUNDING IT.** *"Measured 2026-07-21:
`RUN=108`"* is honest and still misleads, because the reader's question is not
"was this true then" but *"is what I am seeing now consistent with it"*. State
which part is durable and which is incidental — `PASS=0` is the finding, `108`
is that day's inventory; **"the ENTIRE suite went green"** is the finding, `126`
is the receipt. Bind the incidental half to a **SHA**, not a date: a date names
when someone typed, a SHA names the tree the number describes.

Evidence, and it is the strongest form the rule has: **this exact figure drifted
`108 → 128` inside one day, across three sites, written by two authors who had
both just adopted the rule about drifting counts** — and a third author then
corrected two of the three and left the fourth, in a file it had open. Four
rule-holders, one number. Corollary worth keeping: when you fix an instance of
this, **grep for the DEFECT (`RUN=`, `PASS=`, a bare tally), not for the string
you just changed** — grepping the string is what leaves the fourth site.

**1b-ii. DUPLICATE THE CLAIM, NEVER THE COUNT — and this one CUTS AGAINST the
consolidation instinct, which is why it needs stating.** PRD #108 produced the
counter-example to its own tally rule. A guard that was designed, ruled on, and
never implemented went missing with **nothing flagging it**; the only trace was
that three artifacts carried three different guard COUNTS (six, five, seven), and
all three were consistent with the guard being absent. It was caught because they
**disagreed with each other**. Meanwhile four comments were each falsified the same
way — true when written, then invalidated by a guard added UPSTREAM of what they
described — and each had exactly **one** copy, so each survived until somebody
folded the code.

So the two halves pull opposite ways and the resolution is the wording: a
duplicated CLAIM is a cross-check that fires when reality moves; a duplicated
COUNT is four things to drift. Resolve a count disagreement by **deleting the
tally and citing the mechanism**, never by picking the winner — picking one
removes the only instrument that detected the defect.

**The operational check that would have caught all four comments** (sharper than
"re-read nearby comments", and it is a grep of the CALLERS not the neighbours):
when you add a guard, ask **"what did I just make unreachable, and what claims
that reachability?"** Each of those comments stayed true about its own function
and became false about the PATH — which is exactly what re-reading it in
isolation cannot show.

**1c. WHEN AN INSTRUCTION SAYS "VERBATIM", IT BINDS THE CLAIM AND NOT THE
EXAMPLES.** A claim generalises; an example is a measurement with a shelf life,
and copying one forward unexamined is how a warning ends up illustrated by
something that stopped being true. Evidence, and it is the sharpest instance on
this branch because the sentence's own subject was claims outrunning execution:
the lead relayed "ship the auditor's honest limit verbatim", and by the time it
reached the coder **both of that limit's examples were false** — the two queries
it named as declared-but-unfolded had since been folded. The author caught its
own decay and supplied a past-tense rewrite. Ship the claim verbatim; restate the
examples as **dated history** ("as of <date>, before X landed, …"), which cannot
expire and reads as evidence rather than assertion.

**A SUMMARY IS CHECKABLE FOR SHAPE; ONLY THE ARTIFACT IS CHECKABLE FOR FACTS.** Both are real
capabilities and they are not substitutes. Evidence: a reviewer described a draft and asked
the auditor to critique it; the auditor correctly refused to approve prose it could not read,
was right about the draft's STRUCTURAL defect anyway — predicting it from the shape alone,
sight unseen — and then found a wrong NUMBER in it within ninety seconds of finally being sent
the actual words. Record with it what that cost: **the lead's screening step and the author's
own self-review had both passed the text with the wrong number in it.** The second validator
reading the artifact is the only reason it is not in a tracked file.

**2. A LINE NUMBER IS MEANINGLESS WITHOUT A SHA.** `grep -n` answers a question
about a tree that may not survive the hour. `git show <sha>:<path>` is the only
citation that crosses a commit boundary, and reviewer, auditor and lead all need
it when quoting a location. Evidence: **both** of the lead's misfires above, plus
a reviewer/lead disagreement over `:620` vs `:632` where *both were right for
their own SHA* and only the unpinned correction was wrong. The structural fix is
better than the discipline: **cite the assertion by name or message, not by
line** — comment edits shift line numbers, which bit three times in one session
even within a single agent's own work.

**3. Before dispatching a recommendation, name the other recommendations
touching the same file or fixture, and say whether they compose.** Three
collisions on PRD #98, none of them a wrong claim — each was two *locally
correct* instructions written against different files at different times and
never read against each other, and in every case the conflict only appeared when
someone implemented both. The agent merging several sources into one work list is
the likeliest producer of this and the least likely to notice it. If you receive
two instructions that land on one fixture, treat "do these compose?" as part of
the work.

**4. Compile a fold before you prescribe it.** On PRD #98 the lead prescribed an
uncompilable mutation **twice — the second time inside the correction of the
first**, by which point the failure mode was known. The point is not that the
lead should have been more careful: it is that `sqlc generate` + `go vet` settles
it in under a minute with no container and no database, so the check costs less
than the correction. See `CLAUDE.md` for the mechanism (sqlc types by
expression).

## Inspiration-first rule

Before implementing something, check the submodules under `inspiration/`
(bottega, multica, dot-agent-deck) for prior art. Match the better
implementation, and beat it where we can. Reviewer and fact-checker
cross-check our work against these; verify "we do it better than X" claims
against the actual submodule code, not from memory.

## Quality gates

Paste this block into every tester, reviewer and auditor dispatch — teammates
cold-start and never read this file, so a slot you do not paste is a slot they
cannot run. A `none (gap)` slot that has been raised once gets a `noted` marker
appended here; roles report a gap only when its line carries no marker.

```
format         none (gap)          # gofmt -l ./api reports pre-existing drift; run it
                                   # for the current list. Do NOT record a count here:
                                   # it read 26, then 25, and a stale tally invites the
                                   # truncated-view error it already caused (a filtered
                                   # 4-file view reported as the whole list, 2026-07-25).
                                   # The check that matters is `comm -12` between
                                   # gofmt -l and your commit's file list being EMPTY.
lint           none (gap)          # no golangci-lint, no eslint; go vet in CI only
typecheck      cd web && npm run typecheck
               cd agent && npm run typecheck
test           cd api && go test -count=1 ./...
               cd controller && go test -count=1 ./...
               cd web && npm test
               cd agent && npm test
               cd web && npm run check-docs
dead code      none (gap)
coverage       none (gap)
security scan  none (gap, noted 2026-07-21)
pre-commit     none (gap)
long-running   ./e2e/run-e2e.sh    # ~30 min; overrides the tester's 5-min bound
```

Every gap above is what PRD #103 exists to close; re-derive this block when its
milestones land rather than trusting these lines.

**`-count=1` on the two Go lines is part of the gate, not decoration — without it a
green can mean the suite never ran.** Go's test cache hashes only files inside the
module root, and this repo reads test inputs ACROSS module boundaries in both
directions: `api/internal/workersvc/judge_backlog_fidelity_test.go` reads
`fixtures/judge-fidelity/` at the repo ROOT, and the controller's contract tests read
`api/`'s goldens. Measured 2026-07-25: deleting a whole case from `cases.json` left
`cd api && go test ./internal/workersvc/` printing `ok (cached)`; `-count=1` reddened
the same tree naming the broken fixture. Two aggravations, same day — the build cache
is content-addressed and **shared across worktrees**, so even a fresh throwaway
worktree can serve `(cached)`; and CI's `test:api` was armed the same way by
`.go_job`'s persisted `.gocache/`. It costs the test-result cache only; compilation is
still reused. See `CLAUDE.md`'s api section for the full measurement.

## Project signals

- Test commands (see CLAUDE.md for detail): `cd api && go test -count=1 ./...`;
  `cd web && npm test && npm run typecheck`; `cd agent && npm test && npm run typecheck`;
  integration: `./e2e/run-e2e.sh` (isolated stack, dummy creds) and
  `./scripts/smoke.sh` (needs a fresh stack). Never bare `docker compose up`
  for testing — `--env-file` with dummy secrets + unique `-p` project.
- Release flow: tag-driven (PRD #52). `v*` tags publish the api/web images +
  the OCI Helm chart to Harbor (Model B: chart `version`/`appVersion` == the
  tag); k8s deploy is GitOps via ArgoCD to dev-cluster (see `deploy/` +
  `deploy/README.md`)
- Spec dir: `specs/` (`human.md` = user contract, edits need user approval;
  `ai.md` = AI design decisions)
- Authoring rules: `CLAUDE.md` at the repo root (commands, architecture map,
  conventions); plan.md is the working plan
- CI: real (`.gitlab-ci.yml`, PRD #52) — validate/test across api/web/agent +
  `helm lint`/`template` + kaniko image validation builds on every MR and
  `main`; `v*` tags additionally publish the images + OCI chart to Harbor. e2e
  is deliberately NOT in CI (it needs docker compose on the runner) — it stays
  the local pre-merge gate. Remote is GitLab (`gitlab.example.com:vtmocanu/uzi`,
  use `glab`, never `gh`/`tea`)
- MVP shape: local laptop demo via docker-compose, PostgreSQL DB, persistent
  storage (per plan.md)
- Inspiration submodules: `inspiration/{bottega,multica,dot-agent-deck}`
- Slash commands the orchestrator may invoke between delegations: none
  project-specific

## The pattern worth inheriting, above any individual finding

*Migrated from `.claude/agent-team-tasks/prd-98-m3-checkpoint.md` (PRD #98, 2026-07-21)
before that file dies — `.gitignore:27` ignores `agent-team-tasks/`, so nothing written there
survives the worktree. None of this is PRD-98-specific.*

**Every substantive correction on that branch was someone applying a rule its own author had
stated and not applied to themselves.** Not carelessness — the rule-holder is simply not the
person best placed to notice their own instance of it.

- The lead recorded "do not claim more than was executed", then wrote "16 of 16" from an
  **assertion count**. Caught by the reviewer.
- The auditor coined "the mutation that looks like data is the one worth choosing", then
  folded a column to a **blank** — the mutation that looks *wrong*. Caught by the reviewer,
  an hour after applying the rule correctly to someone else's commit.
- The reviewer established "verify from the code, not the surrounding comments", then
  endorsed a comment's obtainability claim without running the command. Caught by the auditor.
- The coder wrote the correct standard — *"a version spelling `"because"` would pass under the
  fold exactly as the fake one does"* — against a fixture that made that standard
  unsatisfiable. **Then, one commit later, restated that same superseded criterion in the very
  comment written to correct it**, three paragraphs from the corrected lesson. It caught its
  own instance only because a mechanical re-screen was already running for another reason.
- The lead prescribed an **uncompilable mutation twice — the second time inside the correction
  of the first**, by which point the failure mode was known.

**Four for four, and the tally includes whoever is reading this.** The fix is never "be more
careful": it is a mechanism that does not depend on the author noticing.

**A second, distinct shape — the true local fact that ends the search.** Three times one
validator stated something correct about the thing in front of it and stopped, because a true
finding feels like a finished one. The sharpest instance: it wrote *"there is no custom timeout
configured anywhere in the file **or a vitest config**"* as context for one flaky test. That
sentence **was** the root cause of five intermittent failures across four PRDs, and it did not
ask what it implied beyond the file.

**A third shape — the rule applied to the INSTANCE and not to the CLASS.** Distinct from the
first two, and the cheapest to fix once named. The counterpart to *compile before you believe*
is **before you ship a fix, name the other instances of the same claim** — usually with one
`grep` for the string you just corrected.

**But NAMING the instances is only half of it — CLASSIFY each hit before you touch it.** A
`grep` cannot tell a CURRENT CLAIM (fix it) from a HISTORICAL RECORD (leave it — rewriting it
manufactures a false history), and the more mechanical the sweep, the more reliably it
corrupts the second kind. **A stale reference is the lesser failure; the mechanical sweep is
the greater one.**

Measured twice on PRD #98's landing, and the second measurement is why the rule needs the
second half. The auditor told the coder *"grep `00075` afterwards, the number is referenced
elsewhere"*. The coder ran the grep instead of following the instruction: the premise was
false — no code or spec referenced it — and of the tree hits that existed, one needed the
renumber while the rest belonged to **other PRDs**, two of them reading *"draft `00075` …
landed as `00055`"*. Those are accurate records of a previous renumber done right, and a blind
`sed` would have erased the evidence of the last person who got it exactly right.

Then the landing merge changed the answer. On the MERGED tree the same grep hits live code
(`runtime.sql.go`, `queries/runtime.sql`, an integration test), `specs/ai.md` twice, a mock
fixture and a neighbouring migration's comment — **all of them correct**, because `00075` on
`main` belongs to PRD #99's `run_message_instance`. The sweep that was merely useless before
the merge would have rewritten seven true statements after it.

**The general move: sweep on the UNIQUE token, not the ambiguous one.** `judge_issue_close_sync`
identifies exactly one migration; `00075` identifies whatever each writer meant on the day. When
the string you just corrected is a number, an id, or a version, assume it is ambiguous and find
the token that is not.

Evidence, all from PRD #98 and each one caught by somebody else: a coder identified that a
bare suite tally in a comment reads as a current fact, bound it correctly at the site it was
writing, and left **two identical unbound copies of the same figure in a file it had open** —
minutes after articulating the rule. Earlier the same day the auditor made the same move with
its fold prescriptions, and the lead made it four times with the cast rule. **The instance in
front of you is the one the rule is hardest to see as a class**, because fixing it feels like
completing the thought.

**The unifying diagnosis: all of these substitute a PROXY for the PROPERTY.** An assertion
count proxies for isolation. A blanking fold proxies for a discriminating one. A comment
proxies for the behaviour. Agreement proxies for verification. A passing commit proxies for a
measured property. The property is always the same question — *would this fail if the code
were wrong in the way it is actually likely to be wrong* — and no proxy answers it.

**Why two validators:** neither miss above was catchable by the person holding the rule. That
is the argument, and it is *not* redundancy — the second validator is not re-doing the first
one's job, it is doing the part the first one structurally cannot.

**Two agents reaching the same conclusion independently is itself the finding.** Both hit the
same flaky test in separate sweeps and both declined to attribute it to their own change;
recorded as load-driven on the strength of the independence, not of either judgement.

**Practical consequence: do not rely on care.** The mechanisms are what worked — mutation with
an assert-it-applied check, a positive control on every run, folds chosen to look like data,
fixtures with distinct values per row, and a deferred-instruction backstop that fails the build
for the instruction nobody has written yet.

## Standing rules — each exists because something went wrong once

*Also migrated from the PRD #98 checkpoint. Each keeps its incident: a rule without its
evidence is one the next reader cannot calibrate. Live-DB mechanics (positive control, `-p 1`,
compile-the-mutation) live in `CLAUDE.md`'s api section; these are the general ones.*

- **A DELIVERED TASK DESCRIPTION CARRIES NEITHER ITS CURRENCY NOR ITS COMPLETION — check the
  task's STATUS before acting on its text.** A `TaskUpdate` wakes the named idle agent, which
  reads the description as a live assignment; the delivery says nothing about whether the
  instruction is still true or whether it has already been carried out. Those are **two
  different questions with two different fixes**, and this run produced both forms:
  - **Stale** — task #14's description was written before the work it described happened, kept
    `:198`/"both counts"/"M8 owns it" after all three were corrected, and would have had an
    agent re-correct an already-correct doc row, the specific failure the PRD names. *Fix: put
    a SHA or milestone in the description so its currency is checkable.*
  - **Already-executed** — task #23 was created to dodge the staleness problem, its text was
    accurate, and it was then re-delivered as if pending **after** being completed. Same root
    cause, opposite symptom. *Fix: only checking status before acting catches this; no wording
    can.*
  Named by the agent that received both: *"'Is this instruction current?' and 'has this
  instruction already been executed?' are two different questions, and the delivery carries
  neither answer."* Corollary for the lead: do task-list bookkeeping **before** the dispatch,
  or accept skipping it — an update sent to an idle agent is a dispatch whether you meant it
  as one or not.

- **AN EXECUTION ORACLE IS NOT A COVERAGE ORACLE: the log tells you a query RAN, never that anyone
  was WATCHING.** Postgres `log_statement='all'` on a throwaway container is the strongest
  instrument this repo has for "is this query exercised" — it observes execution instead of
  inferring it from call sites, and on 2026-07-21 it settled in one run what call-site reading had
  got wrong in both directions (an inventory row asserting "no live test executes it" against **4**
  measured executions; `ListCLITokens` at **0** versus `TouchCLIToken` at **54**).
  **Its limit, worth stating in the same breath as its win:** it distinguishes `UNPINNED` from
  not-unpinned, and it is **blind between "pinned" and "executed but unasserted"** — both execute.
  So evidence from the statement log alone supports only the weaker claim. Answering "did anything
  notice?" needs a different instrument: reading what the test asserts, or a fold, which is the only
  thing that shows an assertion would have caught a change. Worst case for the log alone: a caller
  that **swallows the error** (`if x, err := q.Foo(ctx); err == nil`) makes even an outright query
  failure invisible, so the log can show a query running while nothing downstream could observe any
  result it returned.
  **Naming a tool's limits right after it wins an argument is the harder discipline**, and it was a
  validator who did it here, unprompted, about its own technique.
  **HOW THE METHOD ITSELF FAILS, measured the same day: `docker logs X > f 2>&1` and
  `docker logs X 2>&1 > f` are DIFFERENT COMMANDS, and Postgres logs to STDERR.** The second form
  redirects stderr to the terminal's stdout and then points stdout at the file, so the file gets a
  well-formed, non-empty log **with every statement missing** — and the run reported **0 executions
  of a query that runs once**, which for a minute read as evidence against a correct finding.
  **A zero from a broken capture is indistinguishable from a zero from a query nobody calls.** So
  the instrument needs its own positive control like everything else: before believing any zero,
  confirm the capture contains a statement you KNOW ran. Redirect order is the failure, not the
  tool.
  **ANCHOR THE TALLY ON `-- name: <QueryName>`, NEVER ON THE QUERY'S WHERE CLAUSE — and this one is
  STRUCTURAL, not a grep you can be careful around.** Two runs disagreed (1 execution versus 2 in the
  same isolated test) and the cause was neither sloppiness nor parameter lines: **a partial index's
  predicate is textually identical to the query it exists to serve** — necessarily so, since that is
  what makes the index usable. `store.Migrate` runs at test setup, so every fresh database emits
  `CREATE UNIQUE INDEX … WHERE kind = 'self_improve' AND status NOT IN (…)` once, and a tally grepped
  on that WHERE clause counts the DDL as an execution. **It is guaranteed for exactly the queries
  important enough to have a supporting index — i.e. the ones most worth counting.** Header-anchored,
  the same log gives the true count; only `execute` lines carry the `-- name:` header.
  **THIRD FAILURE MODE: Postgres echoes the offending statement on a `STATEMENT:` line beside every
  `ERROR`, so any query a test deliberately makes fail is logged TWICE and counted twice.** Measured:
  17 such lines in one sweep, inflating 10 queries (`CreateUserOIDC` 32→27, `InsertUserSecret`
  129→126, `UpsertRecommendationDisposition` 7→5). Exclude `STATEMENT:` lines. Found by the agent
  that had itself written the recipe everyone else was told to use — no published figure moved, but
  only because the affected queries sat inside stated ranges rather than being quoted individually,
  which is **luck, not method**.
  **SO THE INSTRUMENT HAS THREE KNOWN WAYS OF LYING AND THEY DO NOT POINT THE SAME DIRECTION** — a
  broken capture **under**-counts to zero; a body-text anchor **over**-counts by catching DDL; a
  `STATEMENT:` echo **over**-counts by double-counting errors. Each needs its own guard, and a tally
  that matches your expectation is not thereby correct: it may be two errors, or the wrong one
  cancelling.
  **"We got lucky" was half wrong, and the other half is the operationally useful part.** When the
  `STATEMENT:` defect was found, both the finder and the lead called the fact that no published
  figure moved *"luck, not method"*. Measured afterwards: of 52 inventory rows, only **seven** quote
  a count at all — four quote `0` (**structurally immune**, since an over-count cannot fabricate a
  zero), two sit on non-error paths the defect cannot reach, and one is explicitly labelled
  *reasoned* rather than measured. **That is method, not luck.** What *was* luck is only the
  non-zero side: nothing stopped a row from quoting an inflated count; the ten inflated queries
  simply happened not to be quoted.
  **Why the distinction earns its place: it gives you a re-check list.** If a fourth failure mode
  turns up, **only rows quoting a NON-ZERO count need revisiting** — two, derivable without a sweep
  — instead of all fifty-two. A "we got lucky" framing yields no such list and implies the whole
  artifact is suspect.
  Corollary for authoring: **never let a reasoned number sit in the same column as measured ones.**
  A single "reasoned: 1 execution" row in a file whose entire thesis is that reasoning was replaced
  by observation is the one row a reader will misread as observed.
  **The one reading no over-count can fake is a ZERO** — which is fortunate, because "this query has
  never executed" is the claim these tallies are usually load-bearing for. The corollary is the
  practical rule: **verifying the CAPTURE matters more than verifying any tally.** Confirm the log
  contains a statement you know ran before believing any zero in it.
  Anchor precisely, too: `ListAppSettings` matches `ListAppSettingsForUpdate` unless the anchor
  carries the trailing `" :"`.
  **The resolution is also the model for settling a disagreement between two measurements: ask the
  PROPERTY, not the total.** The dispute here looked like arithmetic and was actually "does the
  masking layer reject before the handler runs?" — settled by a probe that bracketed each principal's
  request with SQL marker statements so the log segmented attributably (`uza_` → 1 execution, `uzc_`
  → **0**). A total could never have answered it; segmenting by principal did, in one run.
- **`go list ./...` CAN RETURN SILENCE INSTEAD OF AN ERROR, INCONSISTENTLY, IN THE SAME SESSION.**
  Measured 2026-07-21: **42** in a detached audit worktree and **0** in `/uzi/main/api` minutes
  apart, same repo, no error either way. So this is not a predictable worktree-shaped failure anyone
  can learn to expect — it is a command that sometimes returns a plausible number and sometimes
  returns nothing, silently. Strictly more dangerous than one that always fails, and the same family
  as the `head` trap below: **a tool whose silence is indistinguishable from a real answer.** For
  package counts, use `go test ./...`'s own output, which additionally separates the two questions
  people conflate — `ok` lines are packages that RAN tests, `no test files` lines are packages that
  exist. A zero-failures claim is entitled to the first denominator only.
- **A LOST WORKING DIRECTORY PRODUCES A CLEAN GREEN FOR CODE IT NEVER RAN — and the positive
  control is the only thing that catches it.** Measured 2026-07-21, the worst instrument failure of
  the wave. A coder's bash cwd silently became **another agent's worktree, on another branch**,
  while it believed it was in its own. Its relative-path commands resolved there, and its first live
  sweep returned `EXIT 0, RUN=95 PASS=95 SKIP=0 FAIL=0` — a clean green describing **a different
  branch's suite**. It caught this only by grepping the output for its own named test and finding it
  absent. Without that grep it would have reported a 95-pass sweep as evidence for code the run
  never executed. Its follow-up `go test -list` and `ls`, run to "confirm" the test did not exist,
  ALSO ran in the wrong tree — so a wrong-tree diagnosis nearly became a wrong-code diagnosis.
  Compounding it, the throwaway Postgres had the *other* branch's migrations applied and had to be
  destroyed and recreated.
  **Defences, in order of strength:** an absolute `cd` on **every** command (not the first one);
  verifying the tree **in the same command** that produces the measurement; and requiring every
  suite result to name the test you were looking for. A run that cannot name your test is not your
  run, however green. This is the "a measurement is bound to a WORKING TREE as well as to a SHA"
  rule, and it had already bitten the same agent once that day as a harmless near-miss — filed as
  minor, which was the mistake.
- **THE VERIFICATION TOOLING IS WHERE THIS TEAM ACTUALLY FAILS — NOT THE READING.** Measured
  2026-07-21 across one wave: **four agents, four broken instruments, zero wrong conclusions from
  careless reading.** Listed because the pattern is the finding, and because every one of them
  produced output indistinguishable from success:
  - `| head -12` cut the twelfth call site, publishing "ten" — the truncated-view trap, committed
    inside a report by the agent that had been citing that trap at others, then relayed onward
    twice by the lead.
  - A mutation targeted **by line number** landed on a comment line, so nothing was mutated and the
    probe returned **PASS**. Caught only because that agent prints the mutated line before every run.
  - A `gsed -E` alternation whose `|` collided with the chosen `s#…#` delimiter errored, the `&&`
    chain skipped to a `go vet` in the wrong directory, and **nothing was mutated or measured**.
    Had the chain not broken, the result would have been a green `go test` from an unmutated tree.
  - A presence check (`getMockImplementation() === undefined`) nearly filed a **false finding
    against a working fix**: vitest's `mockReset()` installs `() => void 0`, not `undefined`, so the
    probe could not distinguish "reset to a stub" from "still holding the real client".
  **The shared part is the DIAGNOSIS, not the remedy — and conflating them is its own failure.**
  What all four have in common is only this: *the verification step had no positive control of its
  own, so its failure was indistinguishable from success.* The mechanisms are different and each
  needs its own countermeasure. Recording a single fix would send the next reader to repair a
  `head` with a content-addressed `sed` — the same shape as applying one correct answer to a sweep
  of hits that needed different ones.
  | mechanism | remedy |
  |---|---|
  | mutation addressed by LINE NUMBER, landed on a comment | address **by content**; assert the changed-line count |
  | census truncated by `head` | count with `rg -c` / `wc -l` / `--stat` and **reconcile the total against the rows shown** |
  | pipeline broke; nothing mutated or measured | prove application with `git diff --numstat` **plus** a re-grep showing zero remaining — but see the NEW-FILE caveat below, where `--numstat` is itself a blind instrument |
  | check asked the wrong QUESTION (presence, not behaviour) | assert **identity/behaviour** (`String(impl)`, `toBe(el)`, `git grep <sha>`), never presence |
  **NEW-FILE CAVEAT, and it makes the prescribed remedy itself a blind instrument.** `git diff
  --numstat` compares against the INDEX, so for a file created in the working tree and not yet
  staged it reports **nothing** — the same empty output it gives when a mutation failed to apply.
  A mutation-applied check that cannot distinguish "landed" from "never ran" is the exact defect
  this table exists to catch, sitting inside the table's own remedy column. Measured 2026-07-26
  (PRD #113 M2): a mutation on a newly-added `upgrade.go` "proved" itself with empty `--numstat`
  output. Use a content hash before/after (`md5`/`shasum`) plus the one-line diff, or stage the
  file first so `--numstat` has a baseline. Found by the coder that hit it, on a rule the lead had
  put in its own brief.
  Before believing any verification step, ask *what would this print if it were broken?* If the
  answer matches what it prints when it passes, it is not evidence. **Naming the class buys no
  immunity:** three of the four were committed by the agents most fluent in these rules, on the
  branch that exists to enforce them — and the fifth instance was the lead asserting that one of
  these remedies covered all three of the others, corrected before it landed here.
- **A MEASUREMENT EXPIRES RELATIVE TO THE SHA IT WAS TAKEN AT — NOT TO WHATEVER LATER LANDMARK IS
  CONVENIENT.** Measured 2026-07-21, and the lead got it backwards first. Fold receipts were taken
  at `31080a40`; six migration files changed between there and the landing merge; zero changed
  between the landing merge and today. The lead read the empty *merge-to-today* diff as showing the
  receipts "were never actually stale" — **which inverts the conclusion.** They were stale, and
  re-folding was genuinely owed; the clean window since the merge says nothing about a measurement
  predating it. Both underlying numbers were correct and neither agent was wrong; the error was
  entirely in choosing a baseline that made the answer comfortable. **Before citing a diff as
  evidence that a measurement still holds, check that the diff STARTS at the SHA the measurement
  was taken at.** A later landmark — a merge, a release, "current main" — is the wrong baseline
  however tidy its diff looks.
- **A MUTATION CAN GO RED FOR THE WRONG REASON, AND NO GATE CAN SEE IT.** The completion of the
  two rules below, and the one they do not cover. Measured 2026-07-21, reproduced independently
  by a reviewer and an auditor on separate trees: dropping `AND f.target = rr.target` from the M1
  filed-issues join reddened at a duplicate-coordinate `t.Fatalf` — *"two backlog rows share the
  coordinate … the fixture is ambiguous"* — **blaming the FIXTURE for a broken production join**,
  while the sibling assertion the comment credited never executed. The tell was in the failure
  text: the two `rec_id`s it printed were **identical**, so it was one recommendation appearing
  twice, a join fan-out, not two rows.
  **Why the existing rules miss it:** the positive control passes (the suite ran), assert-it-
  applied passes (the mutation applied), and the tree comparison passes (the tree changed). Every
  instrument this file already mandates reports green-for-the-right-reasons. **The only thing that
  catches it is reading WHICH assertion fired.** It is the mutation-testing analogue of a live
  suite that exits 0 having run nothing.
  **The rule: a fold result must record the assertion by MESSAGE, not merely RED/GREEN — and a
  fold that lands on a `Fatalf` about the fixture has certified nothing.** A table of folds with
  a RED column and no message column is not evidence.
  **The same class, one layer up, appeared three times in the single commit that fixed it:** a
  message naming a `review_id` half that was never mutated; another naming a CATEGORY half never
  touched; and a third printing `url=""` when `FiledIssueUrl.Valid` was the condition that fired.
  So ask of every diagnostic *could this string be printed by a condition other than the one it
  names?* — and treat a confidently-wrong diagnosis as worse than a vague one, because it routes
  the next reader into the wrong subsystem with evidence attached.
  **Rarest sibling, same session: a REPRO that cannot reproduce.** The obvious way to trip an
  unscoped sweep — run its test twice on one database — fails, because the sweep deletes its own
  row and self-cleans. A fix attempt starting from that repro would have concluded the finding was
  false. "A check that cannot fail is not evidence" applies to the reproduction step too.
- **Bound every enumeration where you write it**, not after a validator finds it. Four needed
  bounding on one branch; the fourth got one at authoring time and that was the first time the
  conversation did not happen afterwards.
- **Assert the mutation actually applied — not just that the test ran.** A mutation that
  silently fails to apply produces a *false green* indistinguishable from a passing gate. A
  coder's edit targeted `BucketAll       = "all"` while gofmt had written `BucketAll = "all"`,
  so it matched nothing and the "verified" result came from **unmutated code**. This is the
  "did I run something that would fail if this were false?" bar failing on its own enforcement
  mechanism, which is why it needs stating separately.
- **A MUTATION CAN APPLY TEXTUALLY AND BE SEMANTICALLY INERT.** "Assert the mutation applied"
  is necessary and not sufficient — the edit can land, the file can differ, the build can
  pass, and the code can behave identically. Measured 2026-07-21: a reviewer mutated
  `s.triage.todo` where `getJudgeStats` returns `TriageCounts` with `.todo` at top level
  (`api.ts:1745`), so the read silently fell back to the context and changed nothing. It
  nearly filed a live assertion as decoration on the strength of it. The check is not "did the
  file change" but "did the BEHAVIOUR change" — **a mutation that reddens nothing has two
  explanations, a weak test and an inert edit, and they are distinguished only by reading what
  the mutated expression now evaluates to.** (Third refinement of the mutation rule in one day
  and the first about semantics rather than mechanics; it is also how that reviewer discovered
  the assertion WAS load-bearing — the inert mutation was the only thing it caught.)
- **A projection pin is isolated ONLY if it reddens under a fold to a value the fixture ALREADY
  CONTAINS.** Blanking, or folding to a novel constant, proves nothing — any assertion catches
  those. **The discriminating fold looks like DATA.**
  **Corollary — THE FIXTURE IS THE PRECONDITION, and it comes first.** Read-back assertions and
  pairwise-different assertions are **both inert while every fixture row carries the same
  value**: with a fixture writing `'because'` everywhere, "read it back from the table" and
  "spell it in the test" are *literally the same expression*, so no experiment could distinguish
  them. Make fixture values distinct per row first; only then does assertion style matter.
- **Restore by COPY-ASIDE, never `git checkout`** — a git restore silently does nothing for an
  untracked or newly-created file. That left a neutered `WHERE (rv.user_id = @user_id OR true)`
  alive past a cleanup step once: a total authz bypass in a file whose own tests were green.
- **Scope live-DB assertions to the fixture, never the whole table.** The LiveDB packages share
  ONE database and fixtures accumulate, so a table-wide assertion passes or fails on what other
  tests left behind. Measured twice: a global `improve_uzi` backlog assertion filtered only by
  target, so an unrelated fixture in another package failed it.
- **Read a file back after writing it through a shell heredoc** — that path corrupts silently.
- **No amends after a SHA is dispatched for review.** Fixes land as follow-up commits.
- **State an invariant where it is ENFORCED; do not derive it from a decision made elsewhere.**
  The tell: *if removing an unrelated predicate elsewhere would make this code unsafe, the
  predicate belongs here too.* Six instances on one branch, the sharpest being a comment
  claiming a join "cannot fan out" — true only because the join used all three columns of a
  unique key, and silently false the moment one was dropped.
- **A printed instruction is an untested claim.** Any string telling a user what to do next is
  a testable assertion and nothing typechecks it. Of three in one CLI, **exactly one had ever
  been executed, and when it was, it was false** — it told the user to re-run a WRITE to
  recover data a re-run cannot return. The only mechanism that catches this class: run the
  command, then execute exactly what its output told the user to do, and assert the outcome.
- **A COMMENT THAT JUSTIFIES A CHOICE ON ONE AXIS CROWDS OUT THE QUESTION OF WHAT ELSE THAT
  CHOICE DECIDES.** The author-facing sibling of the per-claim rule below, and it is nastier
  because the comment is not wrong — it is **complete-looking**. Measured 2026-07-26 (PRD #113
  M4): a SQL clause used `IS DISTINCT FROM` and its comment carefully explained why, on the
  NULL-handling axis, which was true. Nobody then asked what *else* raw-string comparison
  treats as different — and the answer was SemVer build metadata, so the anchor read
  `0.11.7+g1a2b3c4` and `0.11.7+gdeadbeef` as a version change while the classifier read them
  as one release, re-arming a suppression window on every re-cut tag. Diagnosed by the author
  of the comment: *"the stamp made it reachable; the unasked question is what let it ship."*
  The screen: when a comment defends an operator or a type choice, ask **what other property
  does this operator decide**, not merely whether the stated reason is sound. A justification
  is an invitation to stop reading.
- **Apply the screen PER CLAIM, not per comment block.** A verified-true sentence adjacent to
  an unverified one reads as *one continuous argument*, and the reader's guard drops after the
  part that checks out. Live example: a rigorous, correctly-cited sentence sitting four lines
  above its own contradiction, read past by both a reviewer and the implementer because the
  true half had just earned their trust. This is nastier than a wholly false comment — the
  credibility is borrowed from the neighbour.
- **Screen every test you write with TWO questions.** (1) *"What would I have to change in
  PRODUCTION code to make this fail?"* — if the honest answer is "nothing, only the test file
  or stdlib behaviour", it is decoration. (2) *"Would this line ever EXECUTE in the failing
  case?"* — **an assertion sitting behind an earlier `waitFor`/`Fatalf` in the same test is
  documentation, not a gate.** Five instances on one branch of a commit crediting an assertion
  that never runs when the property breaks; in each the property *was* pinned by something
  else, so only the credit was wrong — which is exactly why it kept recurring unnoticed.
  **Apply it hardest to tests whose NAMES make strong claims**, because the name is what stops
  anyone looking again: an assertion on the test file's own mock factory, a test that
  marshalled a hand-built struct and so asserted a property of `encoding/json`, and a "renders
  the two DIFFERENTLY" test that compared a parent element to its own child.
- **Do not run a live-DB suite while another agent is running one — but do NOT record a
  reason.** Two confident explanations have already been wrong (a "fixed container name" that a
  two-line file disproves; a load-contention refutation measured on a far quieter machine).
  Keep the sequencing: it costs nothing, and "we do not know" is the honest inheritance.
- **"Did I run something that would fail if this were false?"** — ask it *before* writing a
  claim down, not after. Every claim mutation-tested has held; every claim reasoned to has
  needed correcting.
- **STAGE BY PATH. Never `git add -A`, `git add .`, or `commit -a` in a worktree that has a
  writer-token holder.** The `fd951e07` merge on PRD #112 used a blanket add and swept up the
  tester's uncommitted edits. The content was correct, so nothing broke — but minutes earlier the
  tester had been running positive-control mutations in that same worktree: `getCLITokenByHash`
  with its revoked/expiry predicate deleted, and `cli_auth.go` with `!user.IsActive` neutered. A
  blanket add landing while either was applied would have committed **a live authentication
  regression under a merge commit's message**, where nobody reads for one, with the tester's own
  suite as the only thing standing between it and `main`.
  **We were saved by timing, not by process.** And on this branch mutation testing was the
  *primary* evidence mechanism rather than an occasional one — over forty mutations across five
  milestones — so a shared worktree is in a mutated state a meaningful fraction of the time. That
  makes a blanket add a standing hazard here, not a one-off.
  It recurred while this very rule was being written, and the second half is the sharper lesson.
  The batch found `e2e/run-e2e.sh` dirty with a change the lead had **explicitly deferred** —
  sitting exactly where `git add -A` would have taken it. Staging by path would have left it
  alone. But before it could be staged at all, a CONCURRENT WRITER committed it (`b71c7d1a`),
  in a worktree that had a named token holder.
  So stage-by-path is necessary and not sufficient: it protects the diff **you** author, and
  protects nothing against a second writer in the same tree. The two rules are one rule —
  **one writer at a time, and stage by path** — and the failure mode of dropping either is the
  same: a commit whose contents nobody chose. Here it also silently invalidated a measurement,
  because the deferred edit landed on the green path *after* the e2e number was taken.
