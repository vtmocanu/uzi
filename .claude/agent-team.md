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
   test/lint gates before reporting done: `cd api && go test ./...`;
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
- A claim about a **QUERY'S BEHAVIOUR** expires only when the QUERY, the FIXTURE
  or the ASSERTIONS change. `git diff <measured-sha>..HEAD -- <those paths>`
  settles it in seconds, and a **comment-only diff means the result STANDS**.

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

Worked example, both halves from one day: the auditor reported a comment gap at
`c1fcdfce` that `a2b554a6` had already closed — genuinely stale. Its **fold**
results from the same run still stood, because
`git diff c1fcdfce..HEAD -- api/internal/store/recommendation_dispositions_integration_test.go`
was comment-only: no SQL, no fixture row, no assertion changed.

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
format         none (gap)          # gofmt -l ./api reports 26 drifted files
lint           none (gap)          # no golangci-lint, no eslint; go vet in CI only
typecheck      cd web && npm run typecheck
               cd agent && npm run typecheck
test           cd api && go test ./...
               cd controller && go test ./...
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

## Project signals

- Test commands (see CLAUDE.md for detail): `cd api && go test ./...`;
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
