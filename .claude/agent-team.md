# Agent team workflow for uzi

Generated 2026-07-03 by the `agent-team` skill (roster adapted from the example-app team).

> **🔴 HOW TO ADD TO THIS FILE, because its growth rate is now the thing most likely
> to make it useless.** Measured by SHA, so it stays true: **782 lines at
> `5712a0d4` (2026-07-21) → 1948 at `31a36412` (2026-08-02)**, about 2.5x in twelve
> days. *(A reflect pass reported "163 → 1948, 12x in thirteen days"; the lead was
> about to write that in, agreed with the conclusion, and checked the number only
> because this file's own rule says to. `163` does not reproduce at any commit —
> the real figure is 782. **That is this section's own subject happening inside the
> edit that added it**, and it is the fifth instance in one session: a sound
> conclusion, a decorative number, nobody checking because everybody agreed. The
> conclusion survives; the multiplier was never load-bearing.)* Meanwhile
> `grep -rn 'agent-team.md'
> .claude/agents/*.md` returns **one** hit, a passing citation, and the skill's own
> dispatch step states that teammates cold-start and never read this file. **Every
> rule that lives only here is unenforced on teammates by construction.** That is
> not hypothetical: on PRD #103 M2 two rules already in this file — *STAGE BY PATH,
> never `git add -A`* and *the credibility is borrowed from the neighbour* — were
> violated and independently re-derived, in the same session, by agents that had
> never read them.
>
> Three rules follow, and they are about where text goes, not about writing less:
>
> 1. **Lead each section with its OPERATIVE sentence; evidence below it.** A reader
>    who stops after one line should still have the rule.
> 2. **A rule that has failed TWICE migrates; it does not grow.** Move its operative
>    sentence into the role body of whoever must keep it (the skills-repo
>    `roles.yaml`, which reaches every repo), and leave the evidence here with a
>    pointer. A third paragraph in a file nobody must read cannot fix a rule that is
>    already in it.
> 3. **This file is the ARCHIVE, the role tails are the OPERATIVE copy.** Where both
>    carry a gate slot, the tail holds the command plus the live limits; the full
>    correction history stays here. Compressing both is how the history gets lost —
>    check the pointer's target actually holds the thing before you move anything.

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
1. Spawn coder with the full task context. The coder runs the project's gate
   before reporting done: `task gate:api` / `gate:controller` / `gate:web` /
   `gate:agent` for the components it touched, or `task gate` for all four (plus
   `./e2e/run-e2e.sh` + `./scripts/smoke.sh` for stack-level changes, neither of
   which is a target). Recipes live in root `Taskfile.yml`; never restate one
   here, because a stale pasted command still runs and reports green while a
   stale target name dies loudly.
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

**WHERE TO LOOK, WHICH THE RULE ABOVE DOES NOT TELL YOU: AUDIT THE SUPPORT UNDER
CONCLUSIONS YOU AGREE WITH.** Re-deriving everything is not affordable, so the
useful question is where unchecked claims accumulate — and it is not under
contested conclusions. Those get argued and therefore checked. It is under
**sound conclusions, in the corroborating detail nobody reads adversarially
because the point it supports is already accepted.**

> **This file already carried the mechanism, under *Apply the screen PER CLAIM,
> not per comment block* — "the credibility is borrowed from the neighbour",
> written 2026-07-21 for PRD #98.** A reviewer on PRD #103 M2 re-derived it from
> scratch off its own two misses and reported it as a new finding, having never
> read it. That is not a duplicate to merge: **it is the measurement that the
> channel is broken.** No role file points a teammate at this manifest (one
> passing citation across eleven), and the skill's own dispatch step says
> teammates cold-start and never read it — so a rule living only here is
> unenforced by construction, however well written, and this file more than
> doubled over the same fortnight (see the growth note in this file's header, and
> read that note's own parenthetical). The operative sentence belongs
> in the role body of whoever must keep it. Read the two together; do not
> reconcile them into one.

Measured on PRD #103 M2 (2026-08-02), where every false statement that reached
the tree or a commit message had that exact shape:

- The `gofmt -w` retirement was sound on **vacuity alone** — after the reformat,
  an intersection against an empty set can never fail. A second reason was added,
  labelled *measured*, saying the two sides of the retired `comm -12` idiom never
  shared a path shape. **False**: re-derived, the idiom returns **16**, every file
  the reformat touched. Nobody challenged it, because the conclusion it decorated
  was right.
- *"All 7 `gitleaks:allow` directives are unmoved."* The substantive half is true
  and was the point. The count is not. Measured at `b0d8bf72`: **9 occurrences
  across 7 files** repo-wide, **6 across 5 Go files** under `api/`. So `7` is a
  real number under some scope, mislabelled as **repo-wide** — **and which scope
  is NOT established, because two of them yield 7**: repo-wide *files*, and
  *occurrences* restricted to `.go`/`.yml`. *(This clause said "mislabelled as an
  occurrence count", which holds only under the first candidate — the one the
  same sentence declines to choose. "Repo-wide" is true under both.)* The message does not say, and it
  cannot be recovered. **Each successive correction was also wrong**: *"no scope
  yields 7"* (false, twice, by two people), then *"so `7` is the repo-wide file
  count"* (one of two candidates, asserted as fact — **written into this very
  bullet, by the author of this entry, on a claim handed over by the person who
  had just described the pattern**). **The corroborating clause was wrong at
  every layer and the conclusion it decorated — nothing moved — was right at
  every layer.** Each layer was checked against the layer below it and never
  against the repo, because everyone already agreed on the point. The strongest
  form of the point is that it caught the people writing it down.
  *(Anchored to a SHA on purpose. Writing this bullet ADDED occurrences to the
  repo: **10 across 8 files immediately before it existed (`21979bf9`), 11 across
  9 once committed (`9476973c`)**, and it moves again with the next quotation.
  Both readings of "when it was recorded" are needed, because a sentence about
  counts going stale before commit must not itself be ambiguous about which side
  of the commit it means. A count of a population that includes the document
  counting it is stale before it is committed; `CLAUDE.md` has the same trap on
  its `grep -c '^--- PASS'` tally.)*
- *"Three non-whitespace changes."* The conclusion — commit 1 is semantically
  inert — is true and independently confirmed by a token-stream pass. The
  enumeration bolted onto it is imprecise: one of the three changes **zero**
  non-whitespace bytes.
- **A fourth, and it was sitting in the same delta while this entry was being
  written.** Two sites said a malformed `Taskfile.yml` exits `1` and that "every
  target vanishes at once, so the symptom does not resemble the edit that caused
  it". The conclusions those support are sound with **no** evidence at all —
  *"parse this file after touching it"*, and *"`!= 0` covers two distinct
  meanings"*, which is true because there genuinely are two. Everything else was
  support, and **all of it was wrong**: the code is `109`, and the second half
  inverts the truth, since `task` names the file and the line you just edited.
  Corrected in `5680ab6d`. **The entry predicted an instance that already existed
  three files away** — which is the difference between a lesson and a
  demonstration, and the reason this bullet is here rather than a caveat on the
  sentence above.

**And the sharpest instance is this entry's own first draft.** Handing the lesson
over, the coder wrote that both of its errors were *"decorative evidence attached
to a conclusion already sound without it — the retirement held on vacuity alone
and the `|| exit` pin held on the intrinsic-property argument alone."* The lead
quoted it back approvingly. **The second half does not survive checking**: the
`|| exit` pin's supporting measurement was *true*; what was wrong there was a
mechanism claim, a different defect. A neat generalisation had acquired a second
example it did not have — **decorative evidence, inside the sentence defining
decorative evidence, endorsed on sight by two people.** It was caught by applying
the rule to the sentence itself before committing it, which is the only thing
that would have caught it.

So the practice: when a claim is going into a document, ask which clause is doing
the work and which is *support*. Re-derive the support, or delete it. **A
conclusion that stands without evidence does not need evidence attached to it,
and evidence attached anyway is evidence nobody will check.**

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
- **A CHECK PASSES BECAUSE THE CASE YOU CHOSE IS ONE WHERE BROKEN AND CORRECT AGREE — the umbrella
  class, and it is distinct from everything else here because every instrument already mandated
  reports green for the right reasons.** The positive control passes, the mutation applied, the
  tree changed, the assertion executed — and the result is still worthless, because the *input*
  was one both implementations handle identically. Measured five times on PRD #113, none of them
  PRD-specific:
  - `Compare("0.11.7","0.11.7") = 0` is the right answer from a broken version comparison, so an
    all-current fixture set certifies a classifier that never normalizes. The discriminating
    fixture is one that is genuinely *behind*.
  - a single-tick roll-health test passes against a drift-driven implementation, because the
    broken version is **correct on tick one**. Two ticks discriminate.
  - a negative RBAC assertion ("no verb beyond `list` was issued") passes trivially on a build
    that never calls pods at all. The positive half is what has content.
  - a semver probe fed `v`-prefixed inputs — the case the *design* assumed — cannot discover that
    the assumption is the bug. Diagnosed by its author: *"a probe that feeds the code the inputs
    your design assumes cannot discover that the assumption is the bug."*
  - a shell test written with double quotes expanded at assignment time, measuring nothing about
    the literal-string case it was written to settle.
  **The screen is one question, asked of the INPUT rather than the instrument:** *is there a
  version of this code that is wrong and would still pass on the case I picked?* If yes, pick a
  different case. Note this is not "add a control" — a control aimed at the wrong failure is
  itself decoration; the control has to be aimed at the specific way *this* check could pass
  vacuously, which is usually different per test.
- **PLAIN `$?` AFTER A PIPE READS THE LAST COMMAND, NOT YOURS — and this caught FOUR agents on
  one branch, every one of them citing this file at each other while doing it.** Simpler than the
  `PIPESTATUS` entry below and far more common, because `cmd | head` is how everyone bounds
  output. Measured instances, PRD #113: the lead reported `BUILD EXIT: 0` from `head` and nearly
  told a worker its build was fine without knowing; the lead again on a `go build`; a reviewer
  read `head`'s exit while verifying a `docker tag` claim, in a report that cites this trap; and
  the fact-checker published **busybox tar exit 0** for both failure paths — the real answer is
  **1** — because it piped tar's stderr through `tail`. That last one briefly contradicted a
  correct finding.
  **The remedy is not vigilance, it is not piping when you need the status.** Redirect to a file,
  read `$?` on the very next line, then grep the file — which also proves the stage you care
  about actually ran rather than that the wrapper exited 0. Where you need both the message and
  the code, run it **twice**: once piped for the message, once clean for `$?`. The coder that did
  exactly that said it was for readability rather than because it had the trap in mind, which is
  the honest version — the habit protected it, not the knowledge.
  **Why this belongs above the `PIPESTATUS` entry:** that one fails LOUDLY-ish (an empty string
  where a number belongs). This one always yields a plausible integer, so a broken check and a
  passing one are typographically identical.
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
- **THE zsh WORD-SPLITTING TRAP HAS A VERIFICATION-LOOP FORM, AND IT IS WORSE THAN THE
  COMMAND FORM `CLAUDE.md` ALREADY DOCUMENTS.** Measured on PRD #72: an auditor building a
  mutation table ran `for g in $combo` over a space-joined string. zsh does **not** word-split
  unquoted variables, so the loop iterated **once, on the whole string**, and every pair- and
  triple-mutant silently never applied — while those rows still printed a confident
  **"reject"**. The output is indistinguishable from a guard that works. It was caught only by
  an applied-versus-expected substitution counter.
  Same shape as the `${PIPESTATUS[0]}` entry above and it belongs beside it: **a shell-ism in a
  command fails loudly and you fix it; a shell-ism in the check that proves the command passed
  fails silent, and the surrounding output still looks like success.** Use a real array
  (`combo=(g1 g2); for g in "${combo[@]}"`), and give any mutation loop a counter asserting the
  expected number of substitutions actually applied.
- **DIRECTORY-WIDE `gofmt -w` IS A TRAP ON ANY TREE WHERE `gofmt -l ./api` IS NOT EMPTY —
  which after PRD #103 M2 means an un-rebased branch, and nothing else.** `gofmt -w
  internal/<pkg>/` reformats whatever pre-existing drift that tree carries and sweeps files you
  never touched into your commit, under your commit message. Scope it to your own files, by name.
  **Run `gofmt -l ./api` on YOUR tree before reaching for a directory-wide `-w`** — that is the
  whole check, and it is one command.
  - **Retired on any tree containing M2's reformat** (`b0d8bf72`, and `main` once merged): the
    drift is gone, so there is nothing left to sweep and the rule has no subject.
  - **Still fully live on a branch that has not rebased past it.** Measured 2026-08-02 with
    `git archive <branch> api | tar -x` into a temp dir (no sibling worktree touched), `gofmt -l`
    was non-empty on **nearly every local branch**, most of them carrying MORE drift than `main`.
    No figure is recorded, deliberately: it moves with every commit and the shape is the point.
    **Re-measure your own branch; do not infer it from `main`** — which, note, does not yet
    contain the reformat either. *(Two earlier versions of this clause carried figures and both
    were wrong in the same direction. The first said "five live branches", from a sample
    narrower than the population — **how much narrower is not recoverable, and the obvious
    explanation is false**: there are 18 worktrees here, not five. The second said "every branch
    sampled except two", which is accurate and still invites the reader to treat a snapshot as a
    fact. A number here is a
    liability regardless of whether it is correct — the clause immediately after it says numbers
    move, and the fix is to have none rather than a better one.)*
  - **The new gate does NOT cover this case, and the obvious reading of it is wrong.** `fmt-check`
    detects DRIFT; the hazard here is a SWEEP, and a swept tree is gofmt-clean, so the gate is
    green by construction. It was never the drift's existence that hurt — it was foreign files
    riding into your commit. Nothing automated catches that.

  **The `comm -12` idiom the rule prescribed is retired for VACUITY, on the trees where the rule
  itself is retired**: with `gofmt -l ./api` empty, the intersection is empty BY CONSTRUCTION and
  the check can never fail. **It is not retired for having been broken — it worked.** Measured at
  `755861e8`: `gofmt -l ./api` from the repo root against `git diff --name-only` for commit 1
  gives `comm -12` = **16**, which is *every file commit 1 touched*. A perfect hit, not a failure.
  **Do not rebuild it against the new target**, and this is a live trap because the new target is
  what you now reach for: `task fmt-check:api` carries `dir:` and prints **module-relative**
  paths (`internal/config/config_oidc_test.go`) while `git diff --name-only` prints
  **repo-root-relative** ones (`api/internal/config/config_oidc_test.go`), so `comm -12` over that
  pair measures **0** on a tree where the old form measures 16. Both numbers are real; they are
  answers to different commands. The count ban the rule cited was sound; its evidence is preserved
  as a dated note in the `format` slot below.
  *(Corrected twice on 2026-08-02, one commit apart, and the second correction is the instructive
  one. The bullet was first rewritten as an unconditional retirement claiming the idiom died
  "twice over" because the path shapes never matched, "so it returned empty regardless of what it
  was fed" — labelled **measured** and false. The defect was a **swapped antecedent, not a wrong
  number**: the rule supplies its own antecedent one clause earlier, so its bare `gofmt -l` meant
  `gofmt -l ./api` from the repo root; the 0 was measured with `cd api && gofmt -l .`, a shape
  created by the `dir:`-carrying target **that same commit introduced**. A property of the new
  thing was attributed retroactively to the old one. The unconditional SCOPE was the second and
  larger error, caught by the fact-checker: the retirement travels into dispatches via the
  paste-block, which exists precisely because teammates cold-start, so it would have told a
  teammate on a branch with live drift that there was nothing to sweep. Note what both errors
  share — the retirement's real reason needs no evidence at all, and **a sound argument that needs
  no evidence is the one most likely to acquire decorative evidence.** `cd37e182`'s commit message
  carries the first false sentence and `65e2b053`'s carries the unconditional scope; neither can
  be fixed there, and this bullet is the record.)*
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
  **THE SAME FALSE `PASS=0` HAS A SECOND CAUSE — YOUR GREP RATHER THAN THE SUITE — AND
  BOTH COUNTS READING ZERO IS THE TELL, WHATEVER THE RUNNER.** A pattern shaped for one
  runner's output produces `PASS=0 FAIL=0` over a perfectly healthy run of another, and
  that is indistinguishable from a suite that executed nothing. **A zero from a broken
  pattern and a zero from a dead suite look identical, so treat a double zero as an
  instrument fault until you have proved otherwise, not as a result.** Note it has at
  least three innocent-looking causes, which is why "prove otherwise" means checking the
  pattern rather than reasoning: a broken pattern, the skipped suite the entry above
  documents, and a suite that never compiled.
  **The shapes below are illustration, not the lesson — there will be a fourth, and a
  reader who has memorised three is caught by it.** Before believing either number,
  confirm your pattern matches *this* runner. Three measured here:
  - **Go prints `--- PASS` only under `-v`.** Without it a healthy run emits nothing for
    a `grep -c '^--- PASS'` to count, so the tally reads `RUN=0 PASS=0` at `EXIT=0`.
  - **`node --test` and vitest emit neither TAP nor Go's `--- PASS`.** node's spec
    reporter prints `✔ name (1.2ms)` and an `ℹ tests` / `ℹ pass` / `ℹ fail` / `ℹ skipped`
    summary; vitest prints `✓ name` and `Tests  N passed (N)`. **The two ticks are not
    even the same character** — U+2714 `✔` for node, U+2713 `✓` for vitest — so a glyph
    copied from one report matches nothing in the other. Measured 2026-07-26 at
    `cfa1c0a3`: over a fully healthy `agent/` run (`ℹ pass 851`, `ℹ fail 0`, exit 0) both
    `grep -c '^ok '` and `grep -c '^# tests'` return **0**, and the same two return **0**
    over a green `web/` run.
  - **`./e2e/run-e2e.sh` colour-codes, so ANSI bytes sit between the whitespace and the
    token**: `pass() { printf '  \033[32mPASS\033[0m %s\n' "$1"; }`. A `(^|\s)PASS\b`
    anchor cannot match, and note that **both** halves fail, not just the obvious one —
    the byte before `PASS` is the CSI sequence's terminating `m`, a WORD character, so
    `\s` fails and the `\b` does not exist either. Measured 2026-07-26: a count over a
    525-line log of a **passing** run returned `PASS-ish: 0 FAIL-ish: 0`.
  The durable finding in each is that a tally shaped for the wrong runner reads zero from
  a healthy one; the pass counts are that tree's inventory. Same rule as the Go side, run
  in the other direction: before concluding a suite ran nothing, confirm your pattern
  matches the runner it was pointed at.
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

## A claim about what would happen if you removed it is not readable from the code

*The generalization of "a failed grep is not evidence the text is absent" past greps.
Recorded on PRD #72 (2026-07-26).*

Repeatedly on PRD #72, across every agent on the branch including the lead, someone asserted a
**counterfactual** — *if X were absent, Y would happen* — on the strength of having read the
code. Every one was wrong, and reading could not have caught any of them.

*(No tally here, deliberately. Two authors already disagreed on the count while adding rows to
this very table — which is this file's own "duplicate the claim, never the count" rule catching
an instance of itself. Add rows; never write a number.)*

**READ THE TWO COLUMNS DIFFERENTLY, because they have different warrants.** A fact-check on
2026-07-26 confirmed the technical content of every row that is checkable against the tree, and
correctly returned **UNVERIFIABLE** for the *provenance* — who asserted what, when, and who caught
it. That history lived in dispatches under `agent-team-tasks/`, which is gitignored and gone. So
the claims and shapes are measurement; **the attributions are testimony**, and a later reader
cannot re-derive them. Kept anyway, because "the author who had catalogued this class two
revisions earlier" is the finding in several rows and a claim stripped of who made it loses it —
but labelled, since this file's own rule is never to let a reasoned entry sit in the same column
as a measured one.

| claim | shape |
|---|---|
| "nothing stores the old PRD path" | a grep returned nothing |
| "the only honest control is re-adding the join; that is expensive" | no cheaper precedent was looked for — the repo already had one |
| "this Set is the load-bearing one" | the two were redundant with each other |
| "the `.`/`..` check is the traversal fix; `path.Clean` is belt-and-braces" | three guards, mutually redundant, and the dotfile rule was the sole guard for two other cases |
| "`prds/x.md.bak` → not matched" as evidence the boundary check works | green against a matcher that never matches anything |
| "`prds/../../../etc/passwd` proves the traversal guards fire" | it fails the `.md` suffix rule, so it stays red with all three traversal guards deleted |
| "an `init()` panic fires at boot, so it is later than the Go gate" | an `init()` is a **package-load** check — it fires during `go test` too, in every importing package |
| "a test-only guard lets the bad map entry silently never fire" | it reddens CI on the MR that introduces it; measured by removing the panic and running the suite |
| "the fixture rows exist, so the guard is pinned" | an inference about a mutation that was never run, drawn from a fixture that *was* read |
| "never an unrelated entry in a Related PRDs list" | such an entry IS a link the issue carried, so a basename match repoints it — see the retraction note below, which this row is also an instance of |

The last row has a sharper self-diagnosis than any of the others, from the reviewer that
produced it: **"I checked the conjuncts of one statement and the row-inventory of another."** It
ran three of four guard×site mutation combinations and skipped the one that was defective — and
it had both fixtures open, having explicitly noted that the *query* test's fixture carried the
hostile row it then never looked for in the *backfill* test's. **Testing around the defect is
the normal shape of this error, not an unlucky one:** the combinations you skip are the ones
that felt already-answered.

Note also how one of these travelled. *"Silently never fires"* was written by its author,
retracted by that same author after measurement, and then **re-emitted by the lead from a copy
taken before the retraction arrived**. A retraction propagates only as far as the people holding
copies. If you correct a claim you have already sent, chase the copies.

**THE INTERVAL BETWEEN WRITING A RULE AND BREAKING IT CAN BE ZERO, and the sharpest instance on
this branch has both in the SAME COMMIT.** `53d0f222` added this entire section, including the
sentence directly above about chasing copies — and in that same commit copied a claim out of
`prd_link_patch.go`'s binding comment into four user-facing files: *"never an unrelated entry in a
Related PRDs list"*. `b3c1e188` retracted it hours later as false, and `45fb222f` chased the four
copies. So the rule and four live violations of it shipped together, authored by the same agent in
one commit. Nothing about having just written the rule prevented it — which is the argument for
mechanisms over care, made against the author who had just made that argument.

**What actually surfaced it is the reusable part, and it was not vigilance.** The copies were found
during a routine *"what landed since my commit"* read of `b3c1e188`'s message, done before starting
unrelated work in the same tree. That read is cheap, it is owed anyway on a moving tree, and it is
the only reason a stale claim in four shipped files did not survive to the MR. **Read the commits
that landed under you, not just the diff of the file you are about to touch.**

**And "chase the copies" does NOT mean "paste the same correction".** The four sites needed four
different answers, because a correction inherits the audience of the page it lands on, not the
wording of the retraction: an orientation page took the simple true statement with no residual; the
page whose whole subject is unattended behaviour took the residual in full; a changelog entry needed
the bound restated from the reader's side (*"a run cannot introduce a link, not that it cannot pick
among the ones already there"*); a fourth site was already the weaker correct form and needed only
the residual appended. A fifth carried a *different*, narrower claim that the retraction did not
touch, and correctly changed nothing — **a mechanical sweep would have "fixed" it into being wrong**,
which is the `grep`-then-classify rule from the citing section arriving here from the other end.

**A THIRD instance, in ONE FILE rather than across four.** At `cfa1c0a3`, `prdpath.go` carried the
retraction at `:92-93` — *"the design note calling the explicit `.`/`..` check 'the traversal fix'
and `path.Clean` 'belt-and-braces' has it the wrong way round"* — while the retracted sentence was
still live 160 lines below at `:253`: *"The explicit `.`/`..` rejection is the traversal fix."* One
file, one commit, one author; found independently by the reviewer and the architect, fixed in
`b3c1e188`. Its underlying claim is the traversal row in the table above; this is that row's other
half. **Distance inside one file is not distance from your own attention** — the author had both
sentences in the same buffer.

**⚑ THE PATTERN YOU CHOOSE ENCODES WHAT YOU EXPECT, SO A NULL RESULT CONFIRMS YOUR EXPECTATION
RATHER THAN TESTING IT.** This is the sharper form of the flagged rule above, and it was earned by
two searches that missed two of these instances for *opposite* reasons, neither careless. One agent
searched **at HEAD** for a claim that had been fixed two commits earlier, so no pattern could have
found it, and read the silence as "the instance is not real". Another ran `grep -c 'both counts'`
**case-sensitively** for a lead written in this file's uppercase house form, read the `0` as "the
amendment did not land", and dispatched that as an instruction to go write it — which would have
committed a second copy of an entry that was already there. Both zeros read as findings.
**The countermeasure is not a better pattern.** It is the positive control this file already demands
everywhere else: before believing a zero, run your pattern against a string you KNOW is present. A
search you have not calibrated is a mirror.

**⚑ The unifying error is treating a null result as an observation of the world when it is an
observation of your instrument.** A grep that finds nothing tells you about your pattern. A test
that stays green tells you about your fixture. A search that finds no precedent tells you about
your search. In each case what was measured was the instrument, and what was concluded was about
the code.

**⚑ The evidence for a counterfactual is the counterfactual.** Remove the guard and watch the
test redden; delete the line and watch the build fail; gut the fixture and confirm the gate goes
red. If actually removing it is too expensive, then you do not know — say "I did not check"
rather than asserting.

The same rule covers **when** a mechanism fires, not just **whether** it does. "This is an
`init()`, so it runs at boot" is a counterfactual about a state you have not instantiated — Go
loads the package during `go test` too, so it also runs there, in every importing package. And
the inverse trap is real: a check placed in its own `init()` in a file that sorts **earlier**
reads empty data and panics on a healthy tree. Neither is visible at the call site; each costs
one build to settle.

Two corollaries this branch paid for: a **negative** test case must sit in a fixture containing a
**positive** match, or its green is satisfiable by a mechanism that never fires at all; and an
**absence** assertion needs a guard that the thing it asserts about still exists under that name
(`TestMRStateIsWatcherOwned`'s `sawWriter` flag is the in-repo pattern).

The last row is the one to keep if you keep only one: it was **written by the author who had
catalogued this exact class two revisions earlier**, in the same document, and caught only on a
re-derivation they volunteered. Fluency in the rule buys no immunity from it.

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

**And when the send is a RE-SEND, name the tip you verified against and what you saw**
— *"verified before sending: tip `X`, clean tree, no `Y` started."* That is the
recipient-side half, and it is a different mechanism from the rule above, aimed at a
different moment: verify-before-sending tries to PREVENT the crossing, this makes a
crossing **cheap when it happens anyway**. Both are needed, because the sender-side rule
demonstrably does not eliminate the problem. Measured on PRD #102: **five of the coder's
last six messages crossed one of the lead's**, with the lead following the
verify-before-sending rule throughout — the gap between verifying and sending is not zero
when the dispatch is long, and a long dispatch is exactly when it matters.

Every one of those five resolved the same way: the recipient re-derived against HEAD and
answered "already done, tip `Z`". That works, and it costs a full round-trip each time. A
named tip converts an ambiguous re-dispatch into a **checkable claim** — one `git rev-parse`
tells the recipient whether the message is stale or new, with no round-trip at all.

**The asymmetry is what makes it worth stating rather than leaving to judgement: a recycled
or cold-started agent CANNOT tell a re-dispatch from new scope.** It has no memory of the
first send. To it, an instruction to do work it already did is indistinguishable from an
instruction to do work it has not started, and the only thing that separates them is the
tip the sender claims to have seen. (The same run had the mirror case: a `TaskUpdate`
setting `owner` on a completed task woke the idle agent and read as new scope. Same fix.)

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
- **A dispatch saying an amendment "is not in", from a case-sensitive grep** —
  `grep -c 'both counts'` against a lead written in this file's uppercase house
  form. It returned `0`; the amendment had landed one commit earlier, under the
  shouted lead `BOTH COUNTS READING ZERO IS THE TELL` — quoted here rather than
  cited by line, per rule 2 below, and because seeing the casing is the whole
  point. The recipient opened the file at HEAD and reported the refutation
  instead of complying. **Complying would have committed a SECOND copy of the
  entry**, into the file whose neighbouring rule is *duplicate the claim, never
  the count*. Recorded because of where it happened: the dispatch was itself
  about this file's section on false claims of absence, and quoted it. **Fluency
  in a rule is not exemption from it, and the instructor is the one who cannot
  see their own instance.**

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
of replying "anchor not found". **The general form of this — every null result being an
observation of your instrument, not of the world — has its own section above** (*A claim about
what would happen if you removed it is not readable from the code*), because on PRD #72 the same
error arrived through a green test and an empty precedent search as often as through a grep.

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

**PRD #72 then produced the cleanest instance of this rule being stated and
broken by the same agent, in one afternoon, and it goes one step past the usual
shape.** Two validators reported the gap between a retraction and its surviving
copy in one file as **19 lines** (reviewer) and **160 lines** (architect). The
lead had *both* numbers, *noticed* they disagreed, and wrote — correctly, citing
this very rule — that it would therefore **cite the mechanism and drop the
tally**. It then quoted "nineteen lines apart" in a later dispatch. The recipient
ran `git show cfa1c0a3:api/internal/prdpath/prdpath.go` and measured the
retraction at `:92-93` against the surviving copy at `:253`. (These two figures
are quoted rather than dropped because **their disagreement is the finding** —
which is the same reason the guard-count example above quotes six, five and
seven. Bound to a SHA, per rule 2 below; a count that is the subject of the story
is not a tally sitting in the text as a current fact.)
So this is not "stated a rule and forgot it": the author **diagnosed its own
instance, prescribed the correct remedy in writing, and then did the thing
anyway** — with the disagreement it had already flagged sitting in its own notes.
**The rule-holder is not the person best placed to notice their own instance**,
and the gap between knowing a rule and applying it to yourself is not closed by
knowing it harder. What closed it here was a recipient re-deriving a cited number
before using it, which is a mechanism and costs one `git show`.

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

Before implementing something, check the three prior-art projects — bottega
(<https://github.com/vdaubry/bottega>), multica
(<https://github.com/multica-ai/multica>), dot-agent-deck
(<https://github.com/vfarcic/dot-agent-deck>) — for the same or a similar
feature. Match the better implementation, and beat it where we can.

**They were git submodules under `inspiration/` until 2026-08-03 and are not
vendored any more.** Run `./scripts/link-inspiration.sh` once per worktree: it
clones them to `~/repos/external/` (shared, cloned at most once)
and symlinks them into a **gitignored** `inspiration/`. Do not re-clone into
the repo and never `git add -f` those links.

**🔴 A REPO-WIDE `rg` OR `grep -r` CANNOT SEE THROUGH THOSE SYMLINKS, AND SAYS
SO BY PRINTING NOTHING.** Measured 2026-08-03: neither follows a symlinked
directory during recursive traversal, so the sweep exits 0 with no output —
`rg <pattern> inspiration/` (explicit path) or `rg -L` is what actually
searches it. This is exactly the *instrument that cannot produce the
disconfirming answer* case below, and it is worse than most because the
question ("has anyone already built this?") is the one everybody asks by
sweeping. **An empty repo-wide sweep is not evidence of no prior art.**

Reviewer and fact-checker still cross-check "we do it better than X" claims
against X's actual code. What changed is where the code lives, not the
standard: "not checked" is a legitimate finding; "from memory" is still not.
uzi's own worker containers have no host filesystem and so no symlinks — the
product's path is to clone from the URLs above on demand.

## Two negative results from instruments that share an assumption are ONE negative result

**A search that comes back empty is evidence only if it could have come back full.** Running a
second search and getting empty again feels like corroboration and usually is not: if both
searches are shaped by the same guess about naming, they fail together, and their agreement
measures the guess rather than the code.

Measured 2026-07-27, and it nearly shipped a false security invariant. An auditor ruled a field
safe on the premise that one query was the only one listing a table, having run:

- `grep -rn "ListWorkers" queries/*.sql` — but the counterexample is `ListAllWorkers`, which
  **does not contain the substring `ListWorkers`**. The search was structurally incapable of
  returning it, and its two-line output was read as an enumeration.
- `grep -rln "WorkerDTO\|Worker\b" pages/Admin*.tsx` — wrong twice over: the type is
  `AdminWorkerDTO`, and the admin list is not on an `Admin*.tsx` page at all.

Two empty results, each shaped by a different guess about naming, treated as agreement. The
premise was false: an admin route renders every user's row, so the "safe" field was the only
cross-principal sink in the batch. It was caught because the agent asked to *write the claim
into a comment* checked it first — the last cheap moment before a false invariant becomes
code, carrying two names.

**The fix is to enumerate from the schema object, not from a name you already know** — every
query touching `workers`, via `grep "^-- name:"`, not every query whose name matches a string
you have already seen. The same reduction covers the sibling failures: a grep over
already-inventoried field names, a fixture built to demonstrate one state and read as a search
for its opposite, and a `textContent` assertion read as whole-component coverage. **All four
are searching a space defined by what you already found.**

**Corollary for the lead:** relaying a teammate's verified-sounding premise is not verification.
When a claim is about to be *written down* as an invariant — in a comment, a spec, or a doc —
re-derive it yourself, however well-sourced it is. That is the moment it stops being a finding
and starts being something the next reader will trust without checking.

## An instrument that cannot produce the disconfirming answer is not evidence

**This is the fold of the section above and nine separate incidents from PRD #102.** Each
was a different tool failing a different way; the shape underneath is one. Before believing a
result, ask what answer the instrument *could not have returned*. If that answer is the one
that would have proved you wrong, the run measured your setup, not the code.

The nine, each measured, so nobody has to take the abstraction on faith:

| the instrument | what it could not return |
|---|---|
| `grep -F` with a `\|` alternation | anything, ever: `-F` makes the pipe a literal character, so the pattern matched a string nobody had written |
| `git show $c:path` under zsh | the file: zsh's `:a` history modifier ate the path, and the error read as a missing file |
| `comm` over two path lists | any overlap: one side carried a `./` prefix and the other did not, so two identical sets compared as disjoint |
| `cut -d=` under zsh | a whole field: word-splitting differences silently truncated the value being compared |
| file mtimes, to prove a run happened | the distinction between reading and idling, which was the entire question |
| a mutation that failed to compile | a red for the stated reason; vitest printed `Tests  no tests` and it read as "unguarded" |
| a mutation reddening on `SQLSTATE 42P18` | a red for the stated reason: Postgres rejected the *parameter typing*, not the predicate, so the run proved nothing about the predicate |
| a direction inversion that made the action a no-op | a red at all: the assertion never ran, so green meant "nothing happened", not "the behaviour holds" |
| a test that aborts before its own assertions | the disconfirming answer: a `t.Fatalf` on a status **identical in both configurations** fired before the row checks, so the checks that could have discriminated never executed |

**The operational form, for mutation testing specifically: a mutation is verified by the tests
going red FOR THE REASON THE MUTATION NAMES.** Read the failure text, never the count. Two
agents on this branch shipped mutations that reddened for unrelated reasons and read the red as
confirmation.

CLAUDE.md's **"compile the mutation before you believe it"** is necessary and **not
sufficient** — it eliminates only row 6. Rows 7 and 8 compile perfectly and still say nothing,
and row 9 is not about mutation at all: **the test aborted before reaching the assertions that
would have discriminated**, so the guard has to be ordering, not compilation. Put the checks
that can only fail in one configuration BEFORE any check that fails identically in both.

**Two more from M6, added because they are the shapes a green looks most trustworthy in:**

- **An interrupted long run.** `./e2e/run-e2e.sh` printed `[cleanup] EXIT trap entered (code 0)`
  with **zero FAIL lines**, having reached **4 of the harness's 54 phases**. A zero exit code and
  an absent-failure grep are *both* satisfiable by a run that stopped early. The tell is the
  phase count, or better, that the run reached its terminal teardown phase.
- **A silently-dropped Tailwind arbitrary variant.** A mistyped variant is discarded at build
  with no error, leaving a green class-token test asserting a class that has **no CSS behind
  it**. The test cannot distinguish "shipped" from "typo'd"; only reading the built stylesheet
  can (`npm run build && grep -c 'hover:none' dist/assets/*.css`). Note the same trap one level
  down: the first grep for it used a pattern that could not match minified output and returned
  nothing, which read as "the rule is missing".

**A null result is an observation of your instrument, not of the world.** Before believing a
zero, run the pattern against a string you know is present.

## A right measurement with the wrong gesture attached to it

**A number can be correct and still be about something else, and that is harder to catch than a
wrong number** — because the number reproduces, so checking it confirms the wrong claim.

Measured 2026-07-28. A finding reached me as: *"a drop on the dragged card itself is not a
no-op; end-of-bucket sends it to the bottom; measured `[30,10] -> [10,30]` for the FIRST card
of a lane, unchanged for the LAST, which is why the natural fixture hides it."* I was asked to
correct `prd-102-m5-design.md` accordingly. **The design doc was already right and I did not
change it.** All four combinations, through `dropIntent`:

```
lane displayed [30, 10]
  ANCHOR NULL  drag 30 (FIRST) -> [10,30]      <- the reported change
  ANCHOR NULL  drag 10 (LAST)  -> [30,10]      <- the reported no-change
  SELF-ANCHOR  drag 30 (FIRST) -> [30,10]
  SELF-ANCHOR  drag 10 (LAST)  -> [30,10]      <- a self-drop is a no-op in BOTH positions
```

The `[30,10] -> [10,30]` measurement is **exactly right**. It belongs to the `anchor: null`
gesture — a drop on **lane whitespace** — which the design doc documents correctly as
*"reposition to end of column"*. A drop on the dragged card itself self-anchors and **is** a
no-op, which the design doc also documents correctly. Two adjacent rows of the same table, two
different gestures, and the finding swapped them. Editing the doc as instructed would have
replaced two true rows with one false one.

**What made the mix-up survive is the fixture, and it is the same defect the finding itself
names:** drop the LAST card of a lane and both gestures return `[30,10]`. The only fixture that
separates them drags the FIRST card. A fixture where the two candidate explanations agree
cannot choose between them — which is why "the natural fixture hides it" was the right
instinct pointed at the wrong pair.

**Transferable:** when a finding arrives as *measurement + explanation*, the measurement
reproducing does not confirm the explanation. Vary the thing the explanation names — here, the
gesture — and check that the number moves with it. And when told to correct a doc, read the doc
first: this one was right, and the instruction to change it was the error.

## A rule nobody is keeping is a different failure from a rule that is wrong

**And it needs the opposite fix.** Most of the entries in this file are *stated mechanisms that
were not the operative ones*: the doc said X protects you, X did not, and the remedy is to
re-derive and rewrite it. This one is the mirror: a **correct** rule that nobody was running.

Measured on PRD #102: the lead ran **three concurrent writers** in one shared worktree (coder in
`web/` + `docs/`, spec-keeper in `specs/ai.md`, documenter in `CHANGELOG.md`) while
`prd-102-context.md` said *"exactly one teammate writes at a time"*. Nothing was lost, and that
is the point: it held **by luck as much as by design**, because a single `git add -A` or
`git commit -a` from any of the three would have swept the other two's in-flight work into one
commit — and the natural instinct after a doc sweep is exactly `git add docs/ specs/`. What
protected the tree was three agents independently choosing explicit paths, which is discipline,
not a gate.

**Writing the rule more clearly would not have helped; the rule was already clear.** What was
missing is the relaxation being *visible at the moment it was taken*. So if you deliberately
relax a rule in this file:

1. the relaxed constraint must be **provably** satisfied, not merely expected to be (here: file
   sets provably disjoint);
2. every participant must be **told explicitly** what replaces it (here: stage by path, never
   `-A`, never `-a`, never a directory);
3. **say so in the doc, at the time.** Not afterwards, and not only in the dispatch messages —
   the next reader gets the doc, not your outbox.

The lead met (1) and (2) and skipped (3), which is why this entry exists rather than a cleaner
one. **When you find a rule being violated, establish which kind it is before fixing it** —
weakening a correct rule to match what people were actually doing is the failure that looks
most like tidying up.

## TYPECHECK the mutated tree before reading the test result

**A mutation that does not compile produces a run that says nothing, and "nothing" is one
glance from the finding you were hoping for.** Measured 2026-07-27, on the last round of a
long batch: a replacement string carried literal backslashes into a Python `str.replace`, the
mutant was a TypeScript syntax error, vitest failed to **collect** the file, and the run
printed `Tests  no tests`.

The dangerous reading was right there — *"the mutation applied and no test went red"* reads as
*"this site is unguarded"*, which would have been a false **Blocking** finding against a
correct fix. The reviewer who hit it caught it and said so.

**It is the inverse of the silent no-op mutation** (a fold that matches nothing, runs green,
and understates). Both look like "the tests did not say what I expected", and the only
instrument that separates them is the compiler:

- run `npx tsc --noEmit` (or the language's equivalent) **between mutate and run**;
- keep the pre-assert that the pattern was present and the post-assert that it is gone;
- add a post-assert that no stray escape reached the source, since the escaping bug is what
  produced the syntax error in the first place.

**Read the collection line, not just the tally.** `Tests no tests` and `1 failed` are both
"not the green I expected", and only one of them is a result.

## Mutate at the CALL SITE, not in the shared helper

**Folding a mutation into a shared function proves the function is live. It does not prove
every caller routes through it** — and those are the two different claims a control is
usually being asked to settle.

Measured 2026-07-27. A strip helper had two call sites; the control replaced the helper's
body with the identity and two cases reddened, which read as proof the strip was load-bearing.
Both reddened cases exercised **the same call site**. The other arm had no assertion at all,
so a fix applied to only one of the two would have passed that control unchanged — which is
what happened, and it was caught by an audit reading the comment rather than by the test.

**The discriminating instrument is per-call-site**: fold each site separately (pass the raw
value at site A, restore, pass it raw at site B) and require **each** to red on its own. That
is the same shape as the sweep-per-site discipline usually applied to render sites, and the
reason it gets skipped in a helper is that mutating one function feels more central — it is
the composition-point mistake one level down.

The tester that found this named why it was invisible: *"mutating the shared helper is the
composition-point mistake one level down — the same shape as the finding itself, which is
presumably why neither of us saw it from inside."* A control written from inside the
abstraction inherits the abstraction's blind spot.

## An assertion defines its CHANNEL, and "assert over the whole subtree" covers exactly one

**"Nothing in this component carries X" and "nothing in this component's TEXT carries X" are
different claims, and the test that means the second reads like the first.** A React
`container.textContent` assertion cannot see `title=`, `aria-label=`, `alt=`, `placeholder=`,
a form control's `value`, or `document.title`. The component is in scope; the channel is not,
and the assertion's own wording papers over the gap.

Measured 2026-07-27: nine tests asserting `container.textContent` all passed while **four**
untrusted values reached `title`/`aria-label` attributes unstripped, across three rounds of
review. Every one was found by a sweep or a mutation control; **none by a test.** The
demonstration is one line: revert the attribute fix and only the new case reds — the existing
text-channel case, for the *same field*, stays green.

Three habits follow, all earned in that batch:

- **Fix at the composition point, not the render sites.** One descriptor feeding five
  renderers gets one strip where it is composed; five strips drift out of step, and the sixth
  renderer added later gets none.
- **A test that renders the wrong component passes forever.** A `run.branch` test rendered a
  heading component that does not render the branch. It was green and worthless, and the
  control caught it. **Three files in that batch needed a component extracted to make the
  claim assertable at all** — if the value is not reachable by a test, that is a finding
  about the code's shape, not a reason to assert something adjacent.
- **The suite is not the only gate: `vitest` does not typecheck.** A green run hid a type
  error introduced while restoring a mutation. Run the typecheck alongside, and narrow a type
  rather than casting — a cast hides the regression the type would have caught.

Pairs with the per-fact sweep below: **sweep per fact, and check that your assertion can
observe the channel the value actually travels.**

## A value reconstructed at a layer that does not own it — the derived path always works on the happy path

**That is why it survives review: it is not a bug on the day it is written, it is a SHAPE.**
The tell is that a value is being reconstructed somewhere that does not own it, while the
authoritative source sits one layer away. Nothing fails, no test reddens, and the reviewer
reads working code. Stated as "prefer authoritative sources" the rule is unusable; stated as
"which component OWNS this value, and am I reading it from that one?" it is checkable in
seconds.

Five instances, four of them inside one PRD (#35, 2026-07-27) and the fifth its own precedent:

- **Parse a status out of a stringified error.** `agent/src/client.ts` `reportState` needed the
  run's real status from a 409. Routing it through `postJSON` → `RequestError` and reading the
  text would have worked — `toError` **truncates bodies at 4096 chars**, and a `RunDTO` carrying
  `plan_md` exceeds that comfortably. So it passes against small test DTOs and fails on real
  runs. The response body owned the value; the error string was a copy of it with a length
  limit. Fixed by reading `res` directly for 200 and 409 instead of going through the error path.
- **Freeze a server-owned counter into a client-authored row.** PRD #35's Decision 10 specified
  an `attempt` key on the `limit_wait` feed payload. `runs.limit_wait_count` is incremented by
  the SERVER inside `SetRunLimitWait`, strictly after the worker emits that message — so any
  value the worker can emit is a stale N−1 that disagrees with the row it describes. The claim
  payload does not carry a prior value either (`requeue_count` is a different counter). Dropped;
  the renderer reads `limit_wait_count` off the DTO, where it is always current.
- **Ask a past-tense column a future-tense question.** `adr/0035-run-limit-retry.md` D4: a
  review proposed widening `ListAutoSelectCandidates`' in-flight counter to include parked runs,
  to spread a promotion wave across credentials. `runs.anthropic_secret_id` records the
  credential a run **spent**, not the one it is about to spend, so counting parked runs piles
  load onto the already-excluded exhausted token and spreads nothing. Here the authoritative
  source does not exist *yet* — the run has not chosen — which is the sharpest form of the
  shape: the derived value is not stale, it is unobtainable.
- **The one somebody got right first, which is why it is the reference.** PRD #111 D8
  (`api/internal/workersvc/service.go`, `openAnthropic`): resolution used to happen inside the
  ciphertext query, which returned only plaintext, so a run could not name the account that paid
  for it. It now resolves the default to an id and opens **by** id, making the recorded id
  provably the opened one. Same argument, one level down, and it cost a deliberate race window
  to buy.

**And the fifth is a DIFFERENT shape, which is the part worth knowing** — flattening it into
the other four loses the mechanism:

- **A fake that is lenient exactly where the code is fragile.** `agent/test/fake-api.ts`
  `handleState` answered `{}` and `{"error": …}` where both real handlers answer
  `{"run": {…}}` (`handler/worker_protocol.go`, the 409 and the 200). That was **harmless for
  as long as the client discarded the body** and became a lie the moment PRD #35 made the run's
  real status load-bearing.

**The link is causal, and it is why the first four survive testing: while a field is unread,
both the derived path and the fake are free to be wrong, and they are wrong in the same place.**
So the test agrees with the bug. "It works on the happy path" understates it — the happy path is
the only one the fake can express.

**Two checks, both cheap:**

1. **Which component owns this value?** If it is not the one you are reading it from, you have a
   derived path. Ask it about the *write*, not the read: the owner is whoever last set it, and if
   that write happens after your read, the value cannot exist yet.
2. **If the authoritative field were wrong, would any test notice?** 🔴 **Read the whole
   mechanism before deleting this one — it is not a paradox and it is not rhetoric.** A test can
   only fail on a field something reads. Nothing read `/state`'s response body, so nothing
   constrained the fake's answer, so the fake was free to answer `{}` — and the suite was green
   *because* the field was unread, not despite it. **The green was CAUSED by the gap it was
   being read as ruling out.** So on a field nobody consumes yet, "no test fails" is the
   precondition for the bug rather than evidence against it, and the only way to learn anything
   is to make something read the field.

Sits with its neighbours on purpose: **TYPECHECK the mutated tree**, **Mutate at the CALL
SITE**, and **An assertion defines its CHANNEL** are all the same family — a green or a red that
measures the instrument instead of the code. This one is the case where the instrument was never
pointed at the value at all.

**Corollary, and it has a specific trigger: the moment a previously-ignored field becomes
load-bearing, audit every fake of that endpoint IN THE SAME COMMIT.** Leniency that was free
until that commit becomes a lie at exactly that commit, and it will agree with the new code
rather than contradict it. This is the "two lenient fakes drift" problem the claim wire contract
already guards one endpoint over; the guard did not extend to `/state` because nothing had ever
needed `/state`'s body.

## Sweep per FACT after the last behavioural commit

**A batch's own findings falsify claims in code it never touched. Nothing in a per-commit
or per-file review catches that, because the stale claim is not in the diff.** After the
last behavioural commit — before the final review wave, not after it — grep for every
*fact* the batch established and find every place that asserts otherwise.

Earned on the 2026-07-27 quick-wins batch, where **four separate defects were false claims
about work that had already been done correctly**: four sibling comments left asserting a
mechanism the fix had disproved (one of them directly contradicting another comment three
files away), a placeholder assertion that could not fail, a doc headline falsified by the
function one line beneath it, and a freshness claim that was true of three of a fallback
chain's six rungs. The code was right every time; the prose went stale as neighbouring
facts settled. They cost four review rounds and were found by four different agents.

Three things make it work, all learned the hard way in that batch:

- **Sweep for the CLAIM, not the wording.** A grep for the phrase misses a sentence that
  asserts the same thing in different words — `git.ts` carried the superseded measurement
  table without ever using the phrase the sweep was looking for.
- **The correction has to state the mechanism, not just delete the false clause.**
  "Dropped the wrong claim" and "states the right one" are different edits and only the
  second prevents the next round: a comment that is no longer false but no longer explains
  anything leaves the next reader to re-derive it and get it wrong, which is what happened
  three times to one paragraph in that batch.
- **A comment has no executable control**, so the only check is reading each replacement
  against the code it sits beside, verifying every citation resolves at HEAD, and
  re-measuring any number it quotes. Say which comments were read and which were not —
  silence reads as coverage.
- **A CITATION is an assertion too, and `git log -S` is its control.** "Commit X fixed this"
  is checkable and almost never checked. In one batch the same two commits were transposed
  **twice** — they were one apart, touched the same field, and differed only in *channel*,
  which was the very distinction the sentence existed to teach. A reader following the
  citation landed on a commit whose subject line contradicted the claim in its first six
  words, which discredits the true half along with the false one. Before writing "commit X
  did Y", run `git log -S '<the exact string>' -- <path>` and let it name X.

  **But that plain form is FAIL-OPEN on a merge-introduced line, which makes it the
  wrong instrument inside the rule that teaches distrust of fail-open instruments.**
  Git omits merge diffs by default, so a line produced by a conflict resolution
  returns **nothing** — and nothing reads as "refuted". Three commands, not one:

  ```
  git log --oneline -s -S '<string>' -- <path>                             # non-merge introductions
  git log --oneline -s --diff-merges=first-parent -S '<string>' -- <path>  # + merge resolutions
  git log --oneline -s --first-parent --diff-merges=first-parent -S '<string>' -- <path>   # when did MAIN get it
  ```

  **An empty plain `-S` is not a refutation until the merge-aware forms have run,
  and disagreement between the three IS the finding.** Worked example, measured on
  this repo at `1778f359` with `'race -count=1 ./...'` over `.gitlab-ci.yml`: **0
  hits, 2 hits, 1 hit** for the same string. The 2 are `224b5349` (a branch merge)
  and `77cb96e4` (when `main` got it, a day later); the plain form names neither.

  Three further teeth, each of which cost someone a wrong answer here:

  - **`--oneline` and `-s` are both required.** `--diff-merges` turns patch output
    ON for merges, and without `--oneline` every hit still carries a full commit
    header. The shape, measured rather than tallied (Decision 10 applies — the
    numbers grow with history): `--oneline -s` gives **one line per commit**, `-s`
    alone gives tens, and neither flag gives hundreds. A citation nobody can read
    at a glance is one nobody runs twice.
  - **Keep the path filter.** Unfiltered, that same query also matches PRD documents
    that merely *quote* the string, and a noisy citation is one nobody follows.
  - **`-S` counts occurrences, so a MOVE that leaves the string behind is invisible.**
    `1778f359` moved that command out of `.gitlab-ci.yml` into `Taskfile.yml` and is
    **not** a hit, because the explanatory comment left behind still quotes it: the
    count went 1 → 1. Predicting "the newest hit will be the move" is the natural
    inference and it was wrong. Read the chain, not the head.

Corollary for the lead: when you dispatch a fix that names N sites, **verify all N landed**.
The one that got missed in that batch was missed because the lead checked the site it had
argued about and not the four it had listed in the same message.

## Quality gates

Paste this block into every tester, reviewer and auditor dispatch — teammates
cold-start and never read this file, so a slot you do not paste is a slot they
cannot run. A `none (gap)` slot that has been raised once gets a `noted` marker
appended here; roles report a gap only when its line carries no marker.

**The slots name TARGETS, not recipes** (PRD #103 M1). Every recipe lives in
`Taskfile.yml` at the repo root; `task --list` enumerates it, and `task gate:api` /
`gate:controller` / `gate:web` / `gate:agent` run a whole component when the scope
warrants it. This is not tidiness: a stale pasted *command* still runs, still prints
`ok`, and gets reported green, while a stale *target* dies with
`task: Task "gate:api" does not exist` and a nonzero exit. **`task` exits 201 on any
failure, never the underlying command's code** — test for non-zero, never for a
number. Output is `prefixed` per task and the components run serially, so a failing
component's lines are labelled `[test:api]` and the named failing test is readable
**while the run is still going** — `output: group` was measured and rejected because
it buffers, which under CI's `interruptible: true` means a cancelled job loses every
line it had produced.

```
format         task fmt-check      # gofmt -l over both Go modules; fails on drift and
                                   # NAMES the files, module-relative (internal/...).
                                   # It is FAIL-FAST: with drift in both modules it stops
                                   # at the api half and never reaches the controller one.
                                   # What runs first inside gate:api / gate:controller and
                                   # first in CI's validate:api / validate:controller is
                                   # the PER-MODULE fmt-check:api / fmt-check:controller,
                                   # not this composite -- so a component gate already
                                   # covers this slot for the component it gates.
                                   # 🔴 ON AN UN-REBASED BRANCH, gofmt -l ./api IS STILL
                                   # NON-EMPTY -- M2 cleared it on main, not on your tree.
                                   # RUN IT ON YOUR OWN TREE before a directory-wide
                                   # `gofmt -w`, which will still sweep foreign files into
                                   # your commit there. This slot CANNOT catch that: a
                                   # swept tree is gofmt-clean, so fmt-check passes. It
                                   # detects drift; it cannot detect a sweep.
                                   # (Corrected twice on 2026-08-02, by PRD #103 M2 and
                                   # then by its own follow-up. The slot first read
                                   # `none (gap)` and told you gofmt -l ./api reports
                                   # pre-existing drift; M2 cleared and gated that, so on
                                   # a tree carrying the reformat the old instruction sent
                                   # you looking for drift that is not there. The FIRST
                                   # correction then overshot, stating that flatly -- and
                                   # this block is PASTED INTO EVERY DISPATCH precisely
                                   # because teammates cold-start, so it reached teammates
                                   # on branches where the drift is exactly as live as
                                   # ever. Both halves are tree-conditional; neither is a
                                   # fact about the repo. Its count ban was real and is
                                   # kept as history: the tally read 26, then 25, and a
                                   # filtered 4-file view was once reported as the whole
                                   # list, 2026-07-25. The `comm -12` idiom it prescribed
                                   # is retired for VACUITY on a reformatted tree -- an
                                   # intersection against an empty set can never fail --
                                   # and NOT for having been broken: it worked, returning
                                   # every one of the files commit 1 touched. See the
                                   # standing rule for the full retirement and for why a
                                   # naive rebuild against fmt-check:api returns empty.)
lint           task lint           # composite, all four components (M5 will append
               task lint:api       # shell + YAML to it). Each gate:<c> already runs
               task lint:controller
               task lint:web       # its own lint:<c>, so a COMPONENT GATE ALREADY
               task lint:agent     # COVERS THIS SLOT for that component -- same shape
                                   # as the format slot above. Go is golangci-lint
                                   # (errcheck, staticcheck, ineffassign, unused,
                                   # unparam, nolintlint) via a pinned
                                   # `go run ...@v2.12.2`; npm
                                   # is oxlint 1.76.0 via each package's `npm run
                                   # lint`. Ordering differs by component ON PURPOSE:
                                   # lint runs AFTER build in the Go gates (it
                                   # type-checks, so on a non-compiling tree it says
                                   # "typechecking error" instead of the build error)
                                   # and FIRST in the npm gates (~0.06s, not
                                   # type-aware).
                                   # 🔴 THE GO HALF IS RATCHETED AND TASK'S ECHO
                                   # CANNOT SHOW IT. `issues: {new-from-merge-base:
                                   # origin/main, whole-files: true}` lives in
                                   # `.golangci.yml`, NOT on the command line, so the
                                   # read-the-echo habit this block relies on for
                                   # -race/-count=1 does not protect it -- read that
                                   # file. Consequences you WILL hit: only findings
                                   # your branch introduces block, `whole-files` makes
                                   # PRE-EXISTING findings in a file you touched block
                                   # too, and `task lint:api:all` / `lint:controller:all`
                                   # are the unfiltered companions (reported, never
                                   # gating, not in `task gate`).
                                   # 🔴 AND IF `origin/main` DOES NOT RESOLVE, the run
                                   # does NOT skip the ratchet: it reports the WHOLE
                                   # backlog behind one buried warning line, which
                                   # reads as a huge new regression. The targets carry
                                   # a pre-flight that exits 2 saying so; if you see
                                   # it, `git fetch origin main` -- do not start a
                                   # burn-down.
                                   # 🔴 AND golangci-lint TAKES A HOST-GLOBAL LOCK,
                                   # not just a host-global cache. If you see
                                   # `Error: parallel golangci-lint is running` with
                                   # `exit status 3`, ANOTHER WORKTREE HOLDS IT --
                                   # RE-RUN, DO NOT REPORT A RED GATE. This repo is a
                                   # bare clone with many sibling worktrees and this
                                   # team runs agents concurrently by design, so the
                                   # collision is normal rather than exceptional. It
                                   # fails SAFE (false red, never false green), but
                                   # 🔴 THE STATUS CANNOT DISTINGUISH IT FROM A REAL
                                   # FINDING. golangci-lint exits 3; `go run` prints
                                   # that as the TEXT `exit status 3` and then exits
                                   # **1** itself (measured on a 3-exiting program),
                                   # and 1 is this file's "there are findings" code.
                                   # So the 3 never reaches the exit code at all,
                                   # `task` reports its usual 201, and an automated
                                   # reader testing `!= 0` -- or even reading the
                                   # status carefully -- records a red gate over
                                   # code that is fine. THE ONLY DISCRIMINATOR IS
                                   # THE MESSAGE TEXT. Read it.
                                   # 🔴 THE SAME HOST-GLOBAL DIRECTORY HOLDS A
                                   # RESULT CACHE THAT REPLAYS OTHER WORKTREES'
                                   # FINDINGS, AND IT LIES IN BOTH DIRECTIONS.
                                   # Warm entries from a sibling worktree carry ITS
                                   # absolute paths: the RATCHETED targets then go
                                   # falsely GREEN (the diff processor cannot match
                                   # a foreign path and drops everything) while the
                                   # `:all` targets go falsely LOUD. Measured
                                   # 2026-08-02: `task lint:api:all` printed 120
                                   # findings, every one pathed into another
                                   # worktree, against 107 after a cache clean.
                                   # THE TELL IS A `../` IN A FINDING'S PATH --
                                   # that is an invalid run, not a finding. The
                                   # `:all` targets now `cache clean` themselves;
                                   # the gate targets deliberately do NOT (it would
                                   # clear the cache for every concurrent agent), so
                                   # if you are calibrating, or chasing a surprising
                                   # green, clean first and re-run.
                                   # 🔴 AND CLEAN **AFTER** DELETING A THROWAWAY
                                   # WORKTREE, NOT ONLY BEFORE AN ARM. The cached
                                   # paths OUTLIVE THE TREE. Cleaning before your
                                   # own run protects YOU; it does nothing for
                                   # whoever runs next, and a finding pathed into a
                                   # directory that NO LONGER EXISTS is worse than
                                   # one pointing at a live sibling, because nobody
                                   # can go look at it. That is how the 120 above
                                   # happened: a validator built a throwaway
                                   # worktree for a cache probe, removed it, and
                                   # left the entries behind. CAUSE, NOT ARITHMETIC
                                   # -- the owner claimed only the mechanism, and
                                   # "exactly how" would claim a completeness
                                   # nobody established.
                                   # WHAT THE SURVIVING LOG DOES SHOW, MEASURED:
                                   # all 120 findings carried `../` paths, ZERO were
                                   # repo-relative, and every one pointed into a
                                   # SINGLE foreign tree. So the 120 and the 107 are
                                   # DISJOINT POPULATIONS FROM TWO DIFFERENT RUNS --
                                   # there is no `107 + 13` decomposition, and any
                                   # reasoning about a 13-finding gap is about a
                                   # model this run does not fit.
                                   # WHY THAT TREE'S OWN COUNT WAS 120 RATHER THAN
                                   # ~107 CANNOT BE ESTABLISHED: it is deleted and
                                   # no log of it survives. THE EVIDENCE WAS
                                   # DESTROYED BY THE EXACT FAILURE THIS RULE
                                   # DESCRIBES, which is the rule's justification
                                   # rather than a hole in it.
                                   # (Was `none (gap)`; PRD #103 M3 closed it. `go vet`
                                   # still runs inside gate:api / gate:controller as
                                   # its OWN unratcheted step and is deliberately NOT
                                   # folded in here -- folding it in would weaken it,
                                   # since today every vet finding blocks.)
typecheck      task typecheck:web
               task typecheck:agent
test           task test:api
               task test:controller
               task test:web
               task test:agent
               task check-docs:web
dead code      task deadcode       # all four; or deadcode:{api,controller,web,agent}
                                   # (Was `none (gap, noted 2026-07-26)`; PRD #103 M4
                                   # closed it.) Go = `deadcode -test ./...` per module
                                   # against a COMMITTED, EMPTY baseline, so both
                                   # modules gate at ZERO. npm = knip.
                                   # 🔴 THE npm HALF IS STAGED AND A GREEN DOES NOT MEAN
                                   # "no unused exports". knip's exports/types family is
                                   # at `warn`: printed in full on every run, setting NO
                                   # exit code. 22 findings on web, 53 on agent as of
                                   # 2026-08-02. Unused FILES and DEPENDENCIES gate at
                                   # zero. `--max-issues` is NOT a fallback for the warn
                                   # tier -- measured, it counts error-severity only.
                                   # 🔴 AND NEITHER TOOL SEES A DEAD *BRANCH*. deadcode
                                   # finds unreachable FUNCTIONS, knip finds unused
                                   # EXPORTS/FILES/DEPS; a `case` arm nothing reaches
                                   # inside a live function is invisible to both. The
                                   # live example is PRD #99's `case "Task":` arms in
                                   # web/src/components/RunEvent.tsx, which is also why
                                   # they are NOT a valid probe for this slot -- they
                                   # produce a clean "no findings" from a working tool.
                                   # Dead branches stay the reviewer's job.
                                   # CALIBRATING THIS SLOT? USE AN EXPORTED SYMBOL. An
                                   # unexported one is caught by golangci-lint `unused`,
                                   # which runs EARLIER in gate:api, so the gate
                                   # fail-fasts at lint and deadcode never executes --
                                   # measured, and it demonstrates the lint slot while
                                   # claiming to demonstrate this one.
coverage       none (gap, noted 2026-08-02)
security scan  none (gap, noted 2026-07-21)
pre-commit     none (gap, noted 2026-08-02)
long-running   task gate           # ~8m30s from a fresh checkout; EXCEEDS the 5-min bound.
               ./e2e/run-e2e.sh    # ~30 min; overrides the tester's 5-min bound
```

**`task gate` is a LONG-RUNNING slot, and the mitigation is scope, not patience.**
Measured 2026-08-02: `GATE_EXIT=0, elapsed 511s` (8m31s), serial, in a fresh worktree
with a warm module cache and a cold build cache — a fresh checkout pays the whole
`-race` compile. A second sample the same day, in a long-lived worktree whose build
cache was already warm, ran **193s** with the identical target set and EXIT=0 — so the
spread is the BUILD cache, not the machine and not the test count, and 8m31s is what a
CI-like cold checkout costs. Read it as the budget rather than the expectation, exactly
as the `~30 min` below is read. **This over-runs the generic 5-minute
live-wait bound, and an over-run against a stated bound is what makes an agent
abandon a gate and report an inconclusive run as a failure** — the same failure the
e2e exception below exists to prevent.

**BOTH FIGURES ABOVE PREDATE THE LINT STEP** (PRD #103 M3 wired `lint:<component>`
into every `gate:<component>`), so they are left standing as the samples they were
rather than silently re-attributed. Re-measured on the post-M3 tree, 2026-08-02, in a
long-lived worktree with a warm build cache: **`task gate` EXIT=0 in 126-213s across
three samples** (126 / 191 / 213). That range **straddles** the 193s pre-M3 warm
reading, which is the actual evidence here and is stronger than any single sample:
the warm-cache run-to-run spread is wider than lint's contribution in **both**
directions, so the honest statement is that **lint did not move this slot out of its
envelope** — never that it made the gate faster. Quoting only the 126s would invite
exactly that inference. The 8m31s cold budget was NOT re-measured; lint adds roughly 12s of Go work
and 0.1s of npm work to it, which does not change how it should be read.

**So scope it.** A change touching one component runs `task gate:<component>` and
gets a complete answer in well under a minute. Re-measured post-M3, each with its
lint step included: **api 51.8s, controller 6.5s, web 18.3s, agent 25.9s** — but
🔴 **THESE ARE SINGLE SAMPLES FROM THE 126s RUN, WHICH IS THE FASTEST OF THE THREE,
AND THEY SCALE WITH IT.** The total above ships as a range and these do not, so read
them as the bottom of one: scaled to the 213s run, `gate:api` lands near 87s. If you
need a per-component budget rather than an indication, take your own sample on your
own machine — that is cheaper than any figure recorded here, which is the general
case for every timing in this block. (The pre-M3 samples these replace read api
43-66s, controller ~10s, web 23s, agent 34s.) That is what per-component gates are FOR (PRD #103
Decision 2), and nothing else here tells a cold-starting teammate to prefer them.
Reach for the full `task gate` before a release or when a change crosses components,
and coordinate with the lead the way you would for e2e.

**Read a gate's result by its DISCRIMINATING form, never a bare substring.** A bare
`grep -c -F 'FAIL'` over a fully green `task gate` log is non-zero, because some
*passing* test's NAME contains the word (`✓ a FAILED /api/version reaches the fleet
panel …`, `✔ … the liveness probe FAILS …`) — cite `grep -c -F 'FAIL' <log>` vs
`grep -c -- '--- FAIL' <log>` on your own run rather than trusting a number recorded
here, since the count of name-matches moves with whatever the tree happens to be
called (measured 2026-08-02: 9 on one `task gate` log, 8 on another after a merge
that only *added* tests — nothing broke, the tree's vocabulary changed). The forms
that discriminate **within their own component**: `--- FAIL` for Go, the summary
line for vitest, `ℹ fail` plus the exit code for `node --test`.

**None of those three forms tells you the COMPOSITE gate's result, and trusting one
to do so is the same defect in the other direction.** Measured 2026-08-02 across
three real `task gate` logs, one of them genuinely red: the red run's `--- FAIL`
count was **also 0**, because its actual failure (`batcher-poison`) was a
`node --test` failure, and Go's format never appears in a `node --test` failure. The
only thing distinguishing that red log from the two green ones was `Failed to run
task` (1 vs 0) and the exit code. So each per-component form only says something
about its OWN component — a component whose runner never ran contributes nothing
to another component's pattern, silently — and reading any one of them as a verdict
on the whole gate is exactly the kind of bare-tally trust this repo's own rules
warn against. **The composite verdict is the exit code** (`task` exits 201 on any
failure, per Decision 2 of PRD #103); use the per-component forms only to find
*which* component failed and *which* test, never to decide pass/fail on their own.
This is the same lesson as the `--- PASS` population trap in `CLAUDE.md`, arriving
through test NAMES and cross-component blindness instead of through subtest
indentation.

**The `~30 min` above is left as written, deliberately, and here are the measurements
against it.** *(Two samples, both on one machine, both reaching the final banner and the
`down -v` teardown, so neither is a truncated run: **7m55s** at `53d0f222` 2026-07-26, and
**8m40s** at `30ab9e32` 2026-07-27 with 204 PASS / 0 FAIL. Roughly 4× faster than the
figure recorded here, most likely because the image build was largely cached.*

***Both ran `executor=stub` with no `--profile agent-docker`**, which is the caveat that
stops these being a replacement figure: they do not measure the configuration that spends
real agent time, and the `~30 min` may well be right for one that does. **Two samples are
still not a correction** — replacing a figure because the runs you happened to take were
faster is how the stale-tally problem starts over, in the file that documents it.*

***The direction of the error is why this matters more than a tidy number.*** *`~30 min` is
what makes an agent abandon the run against the tester's 5-minute wait bound, which is the
exact failure the long-gate exception exists to prevent — so an over-estimate here is not
conservative, it is the thing that stops the gate being run at all. If a deliberate
re-derivation confirms ~8 min for the stub configuration, say so per configuration rather
than overwriting one figure with another.)*

Every gap above is what PRD #103 exists to close; re-derive this block when its
milestones land rather than trusting these lines.

**The four load-bearing flags are inside the targets now. You no longer type them, and
you must still recognise one going missing** — Task echoes each command, so they are in
the output of any run you make:

- **`-count=1`** on `task test:api` and `task test:controller` — cross-module fixture
  reads are invisible to Go's test cache; measurement below.
- **`-race`** on `task test:api` — PRD #108 M4. Measured by deleting the lock it
  guards: 3/3 red with it, only 2/3 without.
- **`-p 1`** on `./e2e/run-store-it.sh` (which is a script, not a target) — the two
  live-DB packages share one database and, run concurrently, race goose into
  "relation already exists" and TRUNCATE each other's fixtures. Both observed.
- **`--test-timeout=120000`** inside `agent/package.json`'s `test` script, which
  `task test:agent` invokes — node's own default is NO timeout. It was `30000`
  until 2026-08-03, where it was **binding in CI and nowhere else**: what the cap
  governs depends on the node major, so it bounds each top-level SUITE locally
  (node v26.4.0, files share a process) and each FILE in CI (node:22-alpine,
  child process per file). `runner.test.ts` sums to ~96s locally and passed a 30s
  cap; in CI it lands ~25-30s and flaked. Read `CLAUDE.md`'s `--test-timeout`
  block before touching the number. The cap is live and worth
  carrying as insurance against a future slow test, not as a fix for a current
  hang.

**`-count=1` on the two Go test targets is part of the gate, not decoration — without it a
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

- Gate targets (recipes live in root `Taskfile.yml`; see CLAUDE.md for detail):
  `task gate` or per component `task gate:api` / `gate:controller` / `gate:web` /
  `gate:agent`; individual slots `task fmt-check` (PRD #103 M2 — `gofmt -l` over
  both Go modules; the per-module `fmt-check:api` / `fmt-check:controller` run
  first inside `gate:api` / `gate:controller`), `task test:api`,
  `task typecheck:web`, `task check-docs:web`, … (`task --list`). Integration is NOT a target:
  `./e2e/run-e2e.sh` (isolated stack, dummy creds) and `./scripts/smoke.sh`
  (needs a fresh stack). Never bare `docker compose up` for testing — the
  developer's shell profile exports the real vars and Compose ranks shell
  environment above `--env-file`, so use `env -i HOME=$HOME PATH=$PATH docker
  compose --env-file <dummy.env> -p <unique> …`.
- Release flow: tag-driven (PRD #52). `v*` tags publish the api/web images +
  the OCI Helm chart to Harbor (Model B: chart `version`/`appVersion` == the
  tag); k8s deploy is GitOps via ArgoCD to dev-cluster (see `deploy/` +
  `deploy/README.md`)
- Spec dir: `specs/` (`human.md` = user contract, edits need user approval;
  `ai.md` = AI design decisions)
- Authoring rules: `CLAUDE.md` at the repo root (commands, architecture map,
  conventions); plan.md is the working plan
- CI: real (`.gitlab-ci.yml`, PRD #52) — validate/test across **api, controller,
  web and agent** (four toolchains, not three: `controller/` is its own Go module
  with its own jobs) + `helm lint`/`template` + kaniko image validation builds on
  every MR and `main`; `v*` tags additionally publish the images + OCI chart to
  Harbor. Since PRD #103 M1 each **per-toolchain** gate job invokes the same `task`
  target you run locally (`test:api-store-it` invokes none, by design). **The compose e2e harness (`./e2e/run-e2e.sh`) is deliberately NOT in
  CI** (it needs docker compose on the runner), so it stays a purely local
  pre-merge gate. **`./scripts/smoke.sh` is a different case and the old wording
  here collapsed them:** `e2e:kind-smoke` stands up a KinD cluster, installs the
  chart and runs it — but only on PROTECTED refs (`main` and tags), never on an MR
  pipeline. So smoke is a POST-merge gate in CI and a pre-merge gate only locally.
  Run both locally before merging; a green MR pipeline is not smoke having passed.
  Remote is GitLab (`gitlab.example.com:vtmocanu/uzi`,
  use `glab`, never `gh`/`tea`)
- MVP shape: local laptop demo via docker-compose, PostgreSQL DB, persistent
  storage (per plan.md)
- Prior art (external, not vendored since 2026-08-03): bottega, multica,
  dot-agent-deck — `./scripts/link-inspiration.sh` gives you a gitignored
  `inspiration/` of symlinks; a recursive grep does not follow them
- Slash commands the orchestrator may invoke between delegations: none
  project-specific

## The pattern worth inheriting, above any individual finding

*Migrated from `.claude/agent-team-tasks/prd-98-m3-checkpoint.md` (PRD #98, 2026-07-21)
before that file dies — `.gitignore`'s `.claude/agent-team-tasks/` entry ignores it, so
nothing written there survives the worktree. None of this is PRD-98-specific.*

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
- *(PRD #35, 2026-07-27 — a later branch, same shape, added because leaving it out would be
  selecting evidence in the section about not doing that.)* The lead ruled an `attempt` key out
  of a feed payload and justified it with *"the rows are already distinguished by `resets_at`,
  which differs per park **by construction**"* — about a field that is **conditionally absent**
  (omitted when the reset is unknown, which is exactly the case the exponential fallback exists
  for), **having been told in the same report that unknown keys are omitted rather than
  nulled**. The property was read off the case where the field is present rather than off the
  thing that owns it. Caught by the architect. **The ruling was right and only its justification
  was false, which is the more dangerous half**: the lead was simultaneously asking for that
  justification to be recorded *as reasoning others should rely on*, and had spent the same
  session correcting this exact move in three other agents.

**Four for four ROLES — and the lead accounts for three of the six entries.** That is the
opposite of what seniority predicts, and it is the finding: **holding a rule and applying it to
yourself are separate skills**, so position in the loop is not protection. The tally includes
whoever is reading this. The fix is never "be more careful": it is a mechanism that does not
depend on the author noticing.

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

### A MUTATION RESULT IS MEANINGLESS FOUR DIFFERENT WAYS, and only one of them is the edit

*Measured on PRD #111 (2026-07-27), all four on one branch, by three different agents including
the ones whose job is to catch this.*

`CLAUDE.md` already says to assert a mutation actually applied. That rule covers one of four
failure modes, and the other three are invisible to it:

1. **The edit did not apply.** A replacement string that does not match the file is a no-op. Hit
   twice by the reviewer — once because `00089` puts five enum values on one line while the
   pattern assumed one per line. Both times the run came back green and was scored SURVIVED.
2. **The system did not observe it.** The coder's harness *did* diff the file, and one mutation
   was still a no-op: the harness reused one Postgres container, `store.Migrate` is goose, and
   the mutated migration was never re-applied — *"no migrations to run, current version 89"*.
   **The file changed and the system under test did not.** For anything behind a version-keyed
   cache (a migration runner, a package installer), "the file differs" is not "the mutation is
   live".
3. **The suite did not run.** The same harness never exported `UZI_TEST_DATABASE_URL`, so every
   `*LiveDB` test skipped, `go test` exited 0, and that scored as survival — the positive-control
   failure `CLAUDE.md` documents, occurring **inside the tool built to run the controls**. It
   produced two false survivors, one of which was a perfectly good assertion nearly "fixed".
4. **The build failed.** After (3) was fixed, mutations that change sqlc's inferred nullability
   were found to have scored **RED** purely because a build failure also exits non-zero — a build
   error wearing a failing mutation's clothes.

**The asymmetry is the operational point.** A *reddening* mutation proves itself: the tree must
have changed for the test to fail. A *surviving* one proves nothing until you have ruled out all
four. So the verification effort is needed exactly where the result feels least surprising.
A harness that reads only an exit code is measuring the wrong thing four different ways; require
a `--- PASS`/`--- FAIL` line for the named test before believing any result.

### A SOURCE-TEXT CONTROL IS SATISFIED BY DISABLED CODE — and the class is disabled code, not comments

*Measured on PRD #111 (2026-07-27). Two instances, plus a third that looks identical and is
correct, which is the half that makes this worth writing down.*

A guard asserting a string is **present** in a file's raw text passes when the code it names is
commented out, moved below a section marker, or otherwise present-but-inert:

- `RunView.test.tsx` asserted `runViewSource` contains `<RunCredential run={run} />`. Deleting
  the JSX reddened; wrapping it in `{/* … */}` left **42 passed, exit 0** while the chip never
  rendered. `toContain` over raw file text does not know what a comment is.
- `TestWorkerBindModeBackfillLiveDB` coupled to its migration by
  `strings.Contains(migration, backfill)`. Deleting the statement reddened; **commenting it out
  did not**, and nor would moving it below `-- +goose Down`. Either ships a migration whose
  backfill never runs. Commenting out is the likelier mutation — it is what someone does when
  "temporarily" disabling something.

**Stripping comments closes one member, not the class.** After a three-regex comment strip landed,
`{false && <RunCredential … />}` still passed — disabled code that is not a comment at all. The
honest ceiling of any source-text presence guard is *"the text is present"*, never *"it runs"*;
closing the rest needs a render or an execution, and where that is unavailable the residual should
be **named as known** rather than left implied.

**The inverse is correct and must not be "fixed".** `rateLimits.test.ts` asserts an **absence**
over comment-stripped source (`not.toMatch(/100\s*-/)`). Commented-out code genuinely is not a
second implementation of the gate, so ignoring it is right. **Presence and absence assertions have
opposite relationships to comments** — a lead grouped all three as one class and would have sent
someone to "fix" the one that was already correct.

## Standing rules — each exists because something went wrong once

*Also migrated from the PRD #98 checkpoint. Each keeps its incident: a rule without its
evidence is one the next reader cannot calibrate. Live-DB mechanics (positive control, `-p 1`,
compile-the-mutation) live in `CLAUDE.md`'s api section; these are the general ones.*

- **PREFIX YOUR LONG-RUNNING PROBE COMMANDS WITH YOUR OWN ROLE NAME, in the echo:
  `echo "=== [reviewer] go.mod hashes BEFORE ==="`, never `echo "=== BEFORE ==="`.**
  Costs nothing, survives `pgrep -fl`, and it is the **positive** half of `CLAUDE.md`'s
  process-ownership rule: that one tells you what to do when attribution FAILS (if you
  cannot attribute a process, leave it), and nothing about making it succeed.
  **`pgrep` output is the ONLY view another agent has of your process**, and every
  other identifying channel on this team is shared — the shell snapshot is
  per-CLI-session (measured 2026-08-02: three agents, one identical snapshot file),
  cwd is a shared worktree *and* a shared scratchpad, and a log path identifies you
  only if you happened to name it well.
  **The incident: attribution succeeded once, by luck, and needed two coincidences** —
  two agents happening to record their instrument choice in prose, *and* one of them
  happening to leave a distinctive literal in argv. Its owner's own diagnosis:
  *"the thing that actually made my argv distinctive was an echo string I wrote for
  myself, which is an accident of style, not an identifier."* Meanwhile a wrong
  attribution nearly landed on two different agents in turn, because the shared
  signals do not merely fail — they **manufacture a confident match** with whoever you
  check first. This rule turns the lucky match into a designed one. Read it as a pair
  with the `CLAUDE.md` rule: **make your own processes attributable, and leave alone
  the ones that are not.**

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
- **`git add <path>` PROTECTS WHAT YOU STAGE. IT DOES NOT PROTECT WHAT YOU COMMIT — a bare
  `git commit` commits the ENTIRE INDEX, including whatever another agent staged in the interval.
  Use `git commit -- <path>`.** The rule above covers `add`; nothing covered `commit`, and the gap
  is not theoretical — it fired **twice on issue #145's branch alone**, in opposite directions:
  - the spec-keeper ran a correct `git add specs/ai.md`, then a bare `git commit`, and swept **135
    files another agent had staged in the interval** into a commit whose own message read "one
    markdown file, zero code, zero queries";
  - the other direction is worse, because it looks like success: the agent whose files were taken
    ran its own `git commit` and got **`nothing to commit, working tree clean`** — which reads as
    *already done*, not as *someone else committed my work*. A green, quiet answer to the wrong
    question.

  Recovery is `git reset --soft HEAD~1`, which leaves the index untouched so the other agent's
  staged work is handed back exactly as found, then `git commit -- <path>`.

  **And record why the safe case was safe, because it is the load-bearing half.** The agent that
  avoided this got the good outcome by **verifying its index immediately before committing** —
  which is *noticing*, and `CLAUDE.md` is explicit that a rule relying on noticing loses to one
  that removes the failure mode (its own example: naming throwaway containers outside the `uzi-`
  namespace is the strong rule, "be careful with globs" the weak one). `git commit -- <path>` is
  the strong form here. Treat "I checked first" as a report of good luck, not of process.
