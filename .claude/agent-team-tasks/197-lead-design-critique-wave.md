# Issue #197 — lead.md dispatches validators only after implementation lands

Brief for the agent-team run. Branch `fix/197-lead-design-critique-wave`, worktree
`/home/user/repos/myorg/vtmocanu/uzi/fix-197`, cut from `main` at `31a36412`.

**This file is force-added to git** (`.gitignore:44` ignores `.claude/agent-team-tasks/`),
so it is tracked and survives the worktree. Two consequences, both documented in
`CLAUDE.md`: a recursive `grep`/`rg` will **not** find it (ignored-but-tracked — use
`git grep`), and every amendment must be committed to be visible to anyone else.

**Amendments are dated at the bottom.** Re-read this file before each commit; the lead
amends here and sends only a pointer, never the requirement itself.

## The spec

The authoritative statement of the defect is **the issue itself**, not this file:

```sh
env -u GITLAB_TOKEN glab issue view 197 --repo vtmocanu/uzi
```

Read it first. This brief adds only what the issue does not: the deliverable shape in
this repo, the open design question, and the gate.

## One-paragraph summary

`api/internal/agenttmpl/builtins/lead.md` tells the product lead that read-only work
fans out "after an implementation unit lands". So in a uzi run no reviewer, auditor or
tester ever sees the plan, and a wrong plan is discovered only once it is built. The
`architect` builtin exists and its own description says it "Designs implementation
approaches before coding", but nothing in `lead.md` ever sequences it before the coder.
Add a design-critique wave on the plan; keep it proportional.

## Scope

In scope:

1. `api/internal/agenttmpl/builtins/lead.md` — the prose change.
2. `api/internal/agenttmpl/render_test.go` — phrase pins for the new behaviours, in the
   style of `TestLeadParallelDispatchPhrases` (line 138). **Required, not optional**: that
   test exists precisely so a reword cannot silently drop a behaviour, and a new
   load-bearing instruction that is not pinned is one reword from gone.
3. `CHANGELOG.md` — one terse line under `[Unreleased]`.
4. `specs/ai.md` — one appended section recording the decision (append-only at the tail;
   sweep siblings for the highest number first, per the standing numbering rule).

Out of scope, do NOT touch:

- `.claude/agents/*.md` — this repo's own dev-team roster. `CLAUDE.md` is explicit:
  "product changes must never touch it", and the `lead` product template lives only in
  `builtins/`.
- The two fixes from the same skills-repo change that the issue says deliberately do NOT
  transfer: *corrections amend the brief rather than being sent as messages* (uzi's lead
  delegates and waits in the same turn — no mailbox, no mid-turn crossing) and *spawn the
  tester after the first commit* (already correct by construction, since `lead.md` fans
  validators out after implementation).
- The two follow-ups the issue raises and explicitly leaves open: whether **#85** grows a
  second track for the orchestration half, and the missing `version:` field on
  `builtins/*.md` (0 of 11 carry one). File as separate issues if worth it; do not fold in.

## Source wording to adapt, not reinvent

The equivalent change landed in `agent-team/SKILL.md` (`a78fc52`, github.com/vtmocanu/skills)
as a new Step 2 task plus a rewritten Step 3. Its load-bearing sentences:

- *"For anything beyond a small fix, dispatch a DESIGN-CRITIQUE wave BEFORE the
  implementer, and treat the design as FROZEN once the implementer spawns."*
- *"The wave's required deliverable is a CITATION, not a critique."* — for every mechanism
  the plan asserts, name the file that implements it and quote the line. "Attack the
  design" is a mood; the lookup is what has actually paid. **This one is NOT from
  `a78fc52`** — it was added 5½ hours later in `0a9f331` ("close the seam where the
  template holds rules the steps never run"), at `agent-team/SKILL.md:634`. Corrected
  2026-08-02 after `git log -S 'deliverable is a CITATION'` was run against the claim;
  the other three below are verbatim in `a78fc52`.
- *"once the implementer has spawned, a DESIGN change is a new wave, not a message."*
- *"Skip this wave for a small fix — a one-line change with an obvious mechanism does not
  need a design round, and a step that fires on everything gets skipped on everything."*

**Adapt, do not paste.** That text addresses a Claude Code team lead with a mailbox, a
Task* tool set and named teammates it spawns. uzi's product lead has none of those: it is
told its subagents in one line and delegates within a turn. Anything referring to task
records, spawning, standby, or messages must be re-expressed in uzi's terms or dropped.

## The open design question — settle this in the design wave, do not guess

**Where does the wave go: the PLAN turn, or the first IMPLEMENT turn?**

Established from the code (re-derive it, do not trust this):

- The run is two-phase. `agent/src/prompt.ts:43-46` — plan, call `submit_plan`, STOP; a
  human approves; then implement turns arrive by session resume.
- **Both phases already tell the lead its subagents**: `delegatesLine(input.subagentNames)`
  is called at `prompt.ts:546` (plan prompt) and `:621` (implement prompt). So dispatching
  a read-only validator during the plan turn is mechanically available, not blocked.

The trade-off:

- **Plan turn** matches the issue's own argument — the human approval gate is not a peer
  critique, so the human should be approving a plan that has already been read against the
  code. Cost: the plan turn grows N read-only invocations before the human sees anything.
- **First implement turn** is cheaper and cannot delay the gate, but the human then
  approved an uncritiqued plan and the critique may contradict what was approved.

Decide with a reason, and answer the second half too: **does `prompt.ts` need a matching
line, or does `lead.md` alone carry it?** `lead.md` is the system prompt and is present in
both turns, which argues it suffices. If `prompt.ts` must change, say so — that adds the
`agent` toolchain to the gate and is a scope increase the lead must approve.

## Design-wave deliverable (reviewer, auditor, architect)

No code exists yet. For every mechanism this brief or the issue asserts, **name the file
and quote the line**. Specifically, each of these is a claim to check, not a fact to apply:

1. `lead.md`'s current text says read-only work fans out only after an implementation unit
   lands. Quote it.
2. `architect`'s builtin description does say it designs before coding, and nothing
   sequences it. Confirm both halves — the second is a negative claim, so say what you
   searched and why that search could have returned a positive.
3. `TestLeadParallelDispatchPhrases` pins the sentence being revised
   (`"send all allocated read-only validators together in one wave"`). Does the proposed
   change keep that pin true, or does it need editing rather than extending?
4. Both prompt phases carry the subagent list (`prompt.ts:546`, `:621`).
5. `builtins/*.md` carry no `version:` field — the issue says 0 of 11. Verify the count.
6. Nothing else in the product asserts the post-implementation-only ordering. If another
   builtin (`coder`, `reviewer`, `architect`) or `prompt.ts` restates it, the change is
   incomplete without touching that too. **This is the finding most likely to be missed**:
   sweep for the CLAIM, not for the wording.

Then attack the design: what does the proposed instruction make true but still bad? What
would a lead do with it that we would not want?

## Gate

Component gate only — this change touches `api/` (and `agent/` only if the design wave
rules that `prompt.ts` must change):

```sh
task gate:api        # 43-66s. fmt-check:api + vet + build + test:api (-race -count=1)
```

`task gate` (all four, ~8m30s cold) is not warranted unless the scope grows past `api/`.
Recipes live in the root `Taskfile.yml` — never restate one. `task` exits **201** on any
failure, never the underlying code; test for non-zero. The composite verdict is the exit
code; `--- FAIL` is a Go-only form and says nothing about the other components.

Quality-gate slots for this repo are in `.claude/agent-team.md` §Quality gates; the lead
pastes that block into every validator dispatch.

## Standing constraints for this run

- **No amends once a SHA is dispatched for review.** Fixes land as follow-up commits.
- **Report the tip SHA** at the top of every report; the lead verifies that SHA, never
  `HEAD`.
- Commit locally on `fix/197-lead-design-critique-wave`. Never touch `main`, never push.
- Prose edits here are claims about code. Re-derive at the moment you assert.

## Round 4 (2026-08-02, at `dcf9d0f9`) — small, then SHIP

**0 Blocking.** One prompt clause, the rest comment and spec prose. The shipped
template is correct at this SHA; what follows is about not *claiming* more than the
guard delivers.

**The ruling that shapes it.** The tester and auditor folded the same mutation and
reported opposite verdicts. Both results were right; only one **referent** was. For the
six **descriptive** pins the pinned behaviour is *the template states rule R*, so a
relocated sentence still states it — green is correct, and relocation genuinely cannot
produce a disconfirming answer there. For the one **transmission** pin (the relay) the
behaviour is *the lead relays the no-write rule **when it dispatches the plan-critique
wave***, which is constituted by **where the sentence sits**. Relocation does not misplace
that behaviour, it destroys one and creates another. Position is not packaging; for a
positional behaviour, position is the content — and a substring is position-independent by
definition, so no anchor can ever express it.

Underneath: a mutation fold is a test **of the pin set**, not of the template. Read
correctly the result is not *"the pins stayed green"* but *"a template missing the
behaviour passes the suite"*.

**R4-1 — NAME THE RECIPIENTS IN THE PHRASE. This supersedes the `during the plan turn`
rebind, which was the weaker of the two fixes.**

The reason position matters for this pin is a single pronoun: the clause says `tell **each
of them** …`, and `each of them` takes its referent from the preceding sentence. Relocated
into the diff bullet it silently re-resolves to the post-implementation validators, and the
plan-turn dispatch is told nothing.

So replace the pronoun with its antecedent — something of the shape *"tell each validator
you send over the plan that it must not change anything in the worktree"* — and update the
pin phrase and anchor to match. That names the recipient set **inside the phrase**, so the
transmission stops depending on position and the pin becomes genuinely anchorable. This
**dissolves** the residual for this pin rather than mitigating it.

The general rule, worth keeping: **a transmission pin is anchorable exactly when its
recipients are named in the phrase.** And the reviewer's one-glance tell: **a pin is
relocation-proof iff its behaviour is fully determined by its own content; look for a
context-bound referent inside the phrase.** P5 had one; pins 1, 2, 3, 4, 6 and 8 have none,
which predicts exactly the six/one split.

**This wording is UNFOLDED — a proposal, not a verified fix.** Run a relocation fold against
it before believing it: move the rewritten clause into the diff bullet and require the relay
pin to red. If it does not, say so rather than shipping it.

Rebinding to `during the plan turn` is still worth taking **alongside** it (it is the right
axis — it anchors the *act of telling* rather than the *content of the constraint*), but on
its own it only downgrades silently-vacuous to visibly-wrong. Do not describe it as a fix
for relocation.

**R4-2 — unsay two things in the test comment.**
(a) *"an anchored pin staying green under relocation is correct rather than blind"* — true
of the descriptive pins, false of the transmission pin. Qualify it and name the exception.
Stated unqualified it tells the next reader not to run the only fold class that finds this,
which is a self-sealing claim: it guarantees its own green by instruction.
(b) Any implication that property 1 enforces turn-naming. It enforces **substring-presence
of a declared token** — measured: `anchor: ""` passes (`Contains(x, "")` is always true) and
`anchor: "the wave"` passes, and round 3's exact blocking finding was reconstructed with the
audit fully green.

**R4-3 — say the residual as ONE root with two consequences**, beside the insertion limit
and in the same register. **Root:** property 1 is a *syntactic containment* check, and
neither semantic property it would need is expressible as a substring relation.

- **(a) quality gap** — it cannot check the anchor **names a turn**. `anchor: ""` passes
  (`Contains(x, "")` is always true) and `anchor: "the wave"` passes. Applies to all pins; a
  better-chosen anchor fixes any instance.
- **(b) expressiveness gap** — it cannot check the behaviour is **anchorable at all**. For a
  pin whose phrase carries a context-bound referent, *no* anchor fixes it. Unfixable within
  the anchor model; **R4-1 removes the referent instead**, which is why it dissolves the case
  rather than mitigating it.

Use the auditor's F6' as the citation for (b), not the tester's reconstruction: F6' needed
only a relocation against the **shipped pin with a genuine anchor**, where the reconstruction
stacked three things (reverted phrase + degenerate anchor + relocation). Cleaner
demonstration, stronger conclusion.

The qualifier for the comment, in the tester's own words: *relocation is non-discriminating
for pins whose behaviour is fully determined by their own content (1, 2, 3, 4, 6, 8) and
remains discriminating for any pin with a context-bound referent. Reversion is the fold for
the former; relocation is still required for the latter.*

**Do NOT add a turn-token allowlist.** It would reject `"the wave"` and pass
`"before the plan is approved"` while the relay pin stays exactly as blind — making the
audit *look* stronger with the load-bearing hole untouched. Net loss for the reader, and it
goes stale like the negative assertion already refused.

**R4-4 — `specs/ai.md` §467 prose.**
(a) State the **new** ceiling. The bullet retires "one clause bounds it" as an over-claim
and says the clause was widened, but the only figure left (~6) belongs to the *rejected*
phrasing; the shipped wording takes it to ~1.
(b) Scope the `docs/agent-templates.md:58-61` citation to its SHA — correct as history, but
a reader at HEAD lands on the new bullet saying the opposite. Three words.
(c) One line worth keeping beyond this issue: **when two agents fold the same mutation and
disagree, settle what each takes "the behaviour" to be before deciding which result is
right.**

**The one thing that WOULD block, and it is not the hole — it is a claim about the hole.**
If the comment asserts property 1 is machine-checked and relocation non-discriminating,
a known limit becomes a believed-closed property, the residual goes invisible, and #205
loses the evidence that motivates it. Ship with the residual stated; never with it claimed
closed.

**Merge note (not a code change): the repo's highest `specs/ai.md` section is now 471**, not
467. `feature/prd-103-m3-m6` added 468-471 and deliberately skipped 467 — verified, and ours
is the only ref carrying `## 467.`, so there is no collision and the merged sequence is
contiguous 466 → 471. Anyone sweeping from here takes **472**, and must not "renumber 467 to
the tail" on the strength of a fresh sweep.

## Round 3 findings (2026-08-02, at `bade5648`) — fix these, then re-gate

Full detail in the round-3 reports. **1 Blocking, 7 Should-fix.** Every claim below
re-derived by the lead.

**B3 (BLOCKING) — three pins are STILL relocation-blind: P3, P5, P8.** None carries a
turn anchor inside its own phrase. Measured: relocating each into the diff bullet leaves
**22/22 pins green**. R2 is the sharpest — the plan turn reverts to *exactly* the
un-relayed wording P5 exists to reject, and P5 stays green. Control that makes this a
finding: relocating an *anchored* pin (P6) also stays green, but the behaviour travels
with the sentence, so nothing is lost. Anchored-relocated is fine; unanchored-relocated
is blind. **Anchor P3, P5 and P8 the way P1/P2/P4/P6 already are.**

**S7 — shorten P7 to `On a revise turn, re-cite only the mechanisms your revision
changed`.** It already self-anchors, so its borrowed prefix buys nothing and costs two
**measured false positives**: inserting one benign clarifying sentence between P6 and P7
reds P7 with both behaviours fully intact.

**S8 — the overlap never bought what it was bought for.** `strings.Contains` is
per-occurrence, so two overlapping pins can be satisfied by two *different* occurrences —
the overlap does not force contiguity and only ever detected deletion, which the pins
already had. Say this where the comment currently claims otherwise.

**S9 — `D2 ⊂ P1`, undeclared.** Verified exact: `send all allocated read-only validators
together in one wave` is a strict substring of P1's phrase, so D2 can never red alone and
the 14-set's one-assertion-per-behaviour property is broken for it. Accept and note, or
narrow P1.

**S10 — the test comment is wrong in three ways**, in a comment about accuracy:
(a) *"every case below quotes enough context to name its wave"* — false for P3, P5, P8;
(b) *"deleting either anchor sentence reddens two cases rather than one"* — the long
dispatch sentence reds **four** (P4's tail, P5, P6, P7's head all live in it). The
comment describes *span overlap*; a reader meets *sentence co-tenancy*, and sentence
granularity is how anyone folds prose. State a per-sentence expectation instead: 2 for the
`submit_plan` sentence, 4 for the dispatch sentence, 1 for every other, and *any other
multiplicity is a bug, not this trade*; (c) *"Measured on four separate phrases"* vs the
lead's "five pins wide" — different units, neither states which. Record the unit rather
than harmonising.

**S11 — document the INSERTION limit, and do not patch it.** A pure insertion (zero
deletion lines) declaring the paragraph *"an optional pre-flight… skip it entirely and
call `submit_plan` straight away"* neutralises the whole behaviour with **22/22 pins
byte-intact**. This is a property of the instrument, not a bad pin choice:
`strings.Contains` is **monotone under insertion**, so no substring-presence set of any
size or anchoring quality can ever detect an addition. The obvious patch — a negative
assertion — is the vacuous-negative trap and would be worse. Region-scoping does **not**
close it either (measured). Say so plainly, or "anchoring closed relocation" will read as
"the problem is closed".

**S12 — `specs/ai.md` cites `architect.md:35`; the sentence is at `:37`.** Third citation
defect of the run and the **first in a shipped file**. Substance holds.

**S13 — §467's "one clause bounds it" over-claims.** The clause names *revise turns*
only, and there are two distinct re-entry paths: `buildRevisePlanPrompt`
(`sdk-executor.ts:804`) and `buildPlanAfterAnswerPrompt` (`:1215`, inside
`drivePlanningTurn`). So it removes the ×4 revise factor and leaves the ×6 question
factor: the ceiling goes 24 → ~6, not → 1. Either widen the clause to *"on any
re-planning turn"* (also right on the merits) or correct §467.

**S14 — `docs/agent-templates.md:132-134` contradicts itself.** It still tells users Reset
is *"the only path"* while `:144-145` offers the hand-merge, and §467 now **knowingly**
records that the hand-merge exists. Pre-existing, but now knowingly wrong. One word:
*"the only automatic path"*.

**Lead's error, for the record:** I told the fact-checker the hand-merge sentence moved to
`:138-139`. It moved **down**, not up — `480c1b02:138` → `60bf036f:143` → `bade5648:144`.
The coder said it first and I relayed it without running the one command that checks it.

**Region-scoping is a FOLLOW-UP, not part of this change.** Decided on the tester's
measured recommendation, not on scheduling. It closes relocation 3-for-3 with anchors
removed and shrinks every phrase — but its correctness rests entirely on a three-clause
guard that has had exactly one evaluation, its own author's, which is the
control-written-from-inside problem that created this whole task. The naive form
(`strings.Cut`) is **strictly worse than what ships today**: a missing boundary makes the
before-region the *entire body* (seven assertions silently revert to whole-body semantics)
while the after-region is empty (one loud red). One correct-looking red naming the bullet
case, concealing seven disarmed assertions.

## Amendments

### 2026-08-02 — design wave settled. THE DESIGN IS NOW FROZEN.

Reviewer, architect and auditor all reported at `f4736c4b`. The lead re-derived
every load-bearing claim below on its own tree before writing it here. Where a
section above contradicts this one, **this one wins**.

#### Corrections to what this brief said before

- **`delegatesLine` has FOUR call sites, not two.** `prompt.ts:546`
  (`buildPlanPrompt`, issue kind), `:621` (`buildImplementPrompt`), `:778`
  (`buildSelfImprovePlanPrompt`), `:901` (`buildCIFixPlanPrompt`).
  `buildRevisePlanPrompt` (`:664`) does not call it. So there are **three plan
  prompts, one per run kind** — an instruction anchored on "the issue" or "before
  the coder" reads wrong in the `ci_fix` and `self_improve` plan turns.
- **Scope was four files; it is five.** See D9.

#### Frozen decisions

**D1 — placement: the PLAN turn.** Not on the issue's argument, on a mechanical
asymmetry: the two phases do not have the same roster. The plan turn runs with the
user's full own roster (`sdk-executor.ts:495` `agents: assembled.subagents`,
guard `:512` `preToolUse(ownSubagentNames)`). Implement turns are **rebuilt** at the
gate boundary from the human's selection (`:901` `agents: selectedSubagents`, `:906`
`preToolUse(selectedNames)`), and the code's own comment records that **an absent
selection resolves to `repo`** when a roster was detected. So in the first implement
turn the critique agents **may not exist**. Second reason, same direction: a
repo-sourced roster cannot reach the plan turn at all, so a plan-time wave always
runs uzi's own reviewed builtins.

**D2 — rule shape: a PROPERTY OF THE PLAN, not a procedural step.** User decision,
2026-08-02. The plan must name the file and quote the line for every mechanism it
asserts; the read-only wave is the *means* of getting those citations, not a
separate ritual. Rejected: "for anything beyond a small fix, dispatch a wave", whose
skip is graded by the same lead whose plan is being critiqued. The property makes
cost scale with the number of mechanisms asserted (a one-line change asserts one and
costs nothing extra, with **no rule skipped**), and makes a deficient plan legible to
the human at the gate — which is the gap the issue opens with.

**D3 — anchor on `submit_plan`. `lead.md` alone; NO `prompt.ts` change.** The system
prompt is **phase-agnostic** — the lead reads the identical sentence in both turns —
so relative wording ("before the implementer") is unresolvable there. `submit_plan`
is phase-self-locating, is already named in `LEAD_GUARDRAIL_APPEND` (`prompt.ts:44`),
and all three plan kinds end with it (`:559`, `:783`, and `:907`/`:913` for ci_fix).
`lead.md` reaches both
phases by construction (`sdk-executor.ts:492-495`, and `:899-905` rebuilds the system
prompt from the same `assembled.leadSystemPrompt`). A `prompt.ts` line would state it
twice and pull `gate:agent` in for no new capability.

**D4 — the dispatch must say the artifact is the PLAN TEXT, not a diff.** Mandatory,
not flavour. `reviewer.md:43-44`, `auditor.md:36-37` and **`tester.md:89-90`** each
instruct their agent to surface the gap and await re-delegation. Dispatched on
a plan without this, the wave returns bounce-backs instead of citations and is worse
than no wave. Fixing those three bodies is the wrong lever — it triples scope and puts
three more pin sets in play.

**D5 — the wave is REPORT-ONLY in the plan turn; it must not modify the worktree.**
`architect.md:4` and `tester.md:4` both carry `Edit, Write`; `agents.ts:110` honours a
template's `tools` list verbatim, and the path guard (`guardrails.ts:756-759`) only
**jails** writes to the worktree, it does not deny them. A plan-turn write is an
uncommitted change the human never saw at the gate, which the first implement commit
then sweeps in. This weakens no guardrail layer — it weakens the **approval gate**.

**D6 — state the bar over THE PLAN, never over the issue.** `buildPlanPrompt` carries
only `issueTitle` and `issueDescription`, which `UNTRUSTED_FRAME` (`prompt.ts:18-23`)
declares attacker-controlled; there is no label or effort field. A predicate like
"beyond a small fix" is therefore computed from hostile text. Phrase any bar as a
property of the plan the lead itself produced.

**D7 — the pins must discriminate; adding one is NOT sufficient.** Measured by the
auditor and re-derived by the lead: `always fans out`, `after an implementation unit
lands` and `Read-only work` each appear **0 times** in `render_test.go`. The ordering
this issue is about is **entirely unpinned**. The auditor's mutant deleted the
post-implementation ordering, replaced it with a plan-time wave reusing the pinned
clause, and **all 14 pins stayed green** (control: mutating one pinned phrase reds
exactly 1 of 14, so the harness discriminates).

So: keep all 14 existing pins **unedited** — the pinned clause `"send all allocated
read-only validators together in one wave"` excludes the `after an implementation unit
lands,` prefix, so rewording the prefix keeps it true. Then add pins that **one
sentence cannot satisfy for both meanings**: a distinct pin on the retained
post-implementation ordering, and distinct pins on the plan-time citation property.
Write them so deleting either behaviour reds its own case on its own.

**D8 — structure: a short separate paragraph, not a sixth bullet.** The existing
bullet list encodes one contract, *what fans out in parallel*. The citation property is
a different axis (*what the plan must contain before `submit_plan`*), and mixing them
in one list is what let the auditor's mutant reuse a single clause for both meanings.

**D9 — scope is FIVE files, and the gate widens.**

1. `api/internal/agenttmpl/builtins/lead.md`
2. `api/internal/agenttmpl/render_test.go`
3. **`docs/agent-templates.md`** — NEW. `:58-61` restates the post-implementation-only
   ordering, and its frontmatter is `audience: user`, so it **renders in-app** at
   `/docs/agent-templates`. Ship without it and uzi's own shipped docs tell users the
   opposite of what its lead does. Found independently by reviewer, architect and
   auditor; it was the brief's own item 6 and the brief missed it.
4. `CHANGELOG.md` — see D11.
5. `specs/ai.md` — see D10.

Gate: **`task gate:api` AND `task gate:web`** (the doc edit runs under
`check-docs:web`). Not `task gate`.

**D10 — `specs/ai.md` must supersede §190 BY NUMBER.** `specs/ai.md:5145-5149` (§190)
states the old ordering in the present tense. The file is append-only, so a new section
that does not name §190 as superseded leaves two present-tense contradicting contracts
in the ledger.

**D11 — `CHANGELOG.md` must carry an OPERATOR note, because the fix does not reach
existing installs.** `agent_templates.sql:74` is `ON CONFLICT (name) WHERE scope <>
'user' DO NOTHING` and `agent_templates_builtins.go:29-33` states it: *"an existing row
(builtin or admin-edited) is never overwritten."* `docs/agent-templates.md:122` tells
users the same. So the patch is correct, fully gated, merged — and inert on
dev-cluster and on every stack that has booted once. The only recovery is the
admin-only, per-template, verbatim **Reset to default**. The changelog line must say
operators need to reset the `lead` template; the MR description must state the reach
limitation.

#### Explicitly OUT of scope — filed separately, do not fold in

- Builtin prompt updates have no propagation mechanism (the D11 gap). This is the real
  fix and it is a schema + reconcile + UI change with a policy question about
  overriding admin customizations. User decision 2026-08-02: ship #197 small and file
  this.
- `builtins/*.md` carry no `version:` field (0 of 11, verified three times) — the
  missing half of any propagation story.
- `architect.md:4` declares `Edit, Write` and also `SendMessage, TaskUpdate, TaskList,
  TaskGet`, ~~**none of which exist in a uzi run**~~. Pre-existing; D5 contains it for now.
  *(Corrected 2026-08-03, issue #210: `SendMessage` and `TaskList` DO exist — 26 and 3
  `tool_use` entries respectively across runs `71d83432` / `84b6a933` / `c13cff61`.
  `TaskUpdate` and `TaskGet` are UNOBSERVED in those traces, which is not the same as
  observed absent, so this correction deliberately does not claim all four exist.)*
- Whether #85 grows a second track for the orchestration half.

#### Standing, unchanged

No amends after a SHA is dispatched for review. Report the tip SHA at the top of every
report. Commit locally on `fix/197-lead-design-critique-wave`; never push, never touch
`main`.

### 2026-08-02 (later) — findings round on `60bf036f`. Fix these, then re-gate.

Reviewer, auditor, tester and fact-checker all reported at `60bf036f`. The lead
re-derived each load-bearing claim. **2 Blocking, 6 Should-fix, 1 new decision.**

#### Correction to D6, the lead's again

D6 says `buildPlanPrompt` "carries only `issueTitle` and `issueDescription`". It carries
**nine** fields (`prompt.ts:498-518`): also `issueIid`, `branch`, `subagentNames`,
`memory`, `priorWork`, `baseCommit`, `defaultBranchCommit`. **The load-bearing half is
exact and unchanged** — there is no label and no effort field, so a "beyond a small fix"
predicate would still be computed from `UNTRUSTED_FRAME`-declared hostile text. Nothing
shipped repeats the wrong version; the imprecision was only here.

#### B1 (BLOCKING) — the no-write rule is never marked for RELAY

`lead.md:19-22` marks exactly one thing to transmit — *"say in each dispatch that the
artifact under review is the plan text, not a diff"* (D4). The no-write rule arrives as
a separate sentence addressed to the lead and is **not** marked for relay.

Why that is fatal rather than untidy: a subagent's system prompt is `t.prompt_body`
(`agents.ts:96`) and the lead cannot alter it, so **the dispatch prompt is the only
channel**. Meanwhile `architect.md:37` tells it to "right-size" to *a SendMessage design
summary for small changes*, an ADR, or a design doc — and ~~**`SendMessage` does not exist
in a uzi run**: `repoagents.ts:25-29` states an allowlist entry matching no real tool is
silently unavailable, naming `SendMessage` explicitly. So the non-writing branch is
unreachable and both remaining options are writes.~~ `tester.md`'s prohibitions are all
*external* (push, merge, comment) and do not cover a local worktree write.

> **Corrected 2026-08-03, issue #210 — the struck claim is FALSE and its citation now
> points at the correction.** `SendMessage` executed 26 times across runs `71d83432` /
> `84b6a933` / `c13cff61` (18 successful), and `ToolSearch` resolved `select:SendMessage`
> six times, which is direct proof the SDK provides it independently of any send
> succeeding. The citation is stale in the direction that inverts it: at HEAD the
> correction begins at `repoagents.ts:29`, while `:25-28` — the lines this bullet cites —
> still carry the STRUCTURAL claim, which is untouched and still true. Only
> `SendMessage` as an instance of it was wrong. **So the non-writing branch is REACHABLE
> and architect's remaining options are not both writes**, which weakens the argument for
> B1 below without touching its conclusion (the no-write rule still has to be relayed,
> on the plan-turn-write argument). The quoted phrase is also retired: `architect.md:37`
> now reads *a summary via SendMessage to `main`*. Scope: `TaskList` is also observed
> (3 calls); `TaskUpdate` and `TaskGet` are UNOBSERVED, not observed absent.
>
> This file is TRACKED and simultaneously ignored (`.gitignore:44`), so `grep -rl` cannot
> see it and `--hidden` changes nothing — which is exactly how this seventh copy survived
> the six-site sweep in `4fde2088`. `git grep` finds it.

`lead.md:43-45` already shows the idiom the paragraph should have used: *"stated in its
delegation prompt, and tell it not to commit"*.

**Fix:** add the relay verb, e.g. `…and tell each of them the wave must not change
anything in the worktree`. **And pin the RELAY, not the constraint** — the current pin
`must not change anything in the worktree` is satisfied by the un-relayed wording, so it
cannot detect this.

#### B2 (BLOCKING) — two new pins are RELOCATION-BLIND

C3 (`for every mechanism it asserts, name the file that implements it and quote the
line`) and C5 (`must not change anything in the worktree`) carry **no anchor to a wave**.
Measured by the tester and reproduced from a clean detached checkout: relocate either
clause into the post-implementation bullet and delete the plan-turn sentence — the
plan-turn constraint is **gone from the template** and **all 20 pins stay green, exit 0**.

This is the issue's own defect one level in. The old set was blind to *which wave fans
out*; this set is blind to *which wave a constraint binds to*.

**It survived three controls because all three were the same instrument.** The coder's
sentence deletions, the reviewer's word-level weakenings and the auditor's seven mutants
are all **presence** mutations; none can produce the disconfirming answer, because in a
relocation mutant every pinned phrase is still present character-for-character.

**Fix** (tester's, strings verified present exactly once — but it did not fold against a
patched pin set, so re-verify):

- C5 → `an edit made during the plan turn is a change nobody saw when approving it`.
  **Do NOT use `That wave must not change anything in the worktree`** — the fold-9 mutant
  contains that string verbatim in the wrong bullet, so it is already disproven.
- C3 → widen to `make the plan carry its own evidence: for every mechanism it asserts,
  name the file that implements it and quote the line`, so relocation must carry C2's
  anchor along. Note this **overlaps C2's tail**, breaking the pairwise-disjoint-spans
  property the tester measured; that is acceptable here but say so rather than letting a
  later reader think it is an accident.
- **Re-verify with RELOCATION folds, not deletions.** A deletion-only re-run reports 6/6
  and proves nothing about this class. B1's new relay pin needs the same treatment.

#### Should-fix

- **S1 — D2 drifts at the close.** `lead.md:24-27` (*"Judge how far to take this … how
  many you could not cite"*) reinstalls a self-graded dial and concedes uncited mechanisms
  as an end state, contradicting the absolute bar seven lines above. Reviewer's
  replacement keeps the C6 pin byte-identical: `What this costs follows from the plan you
  produced — how many mechanisms it asserts — never as a judgement about the issue text,
  which you do not control.`
- **S2 — `docs/agent-templates.md:65-66` over-claims.** *"nothing in the repository is
  changed before you approve"* states a prompt-level, unenforced property as a flat
  guarantee. Found independently by reviewer and fact-checker. The same page draws the
  distinction correctly at `:48-50` (*"enforced by the worker regardless"*). Soften to
  the instructed register.
- **S3 — the CHANGELOG omits that Reset is ADMIN-ONLY.** `authorizeTemplateWrite`
  (`handler/agent_templates.go:146-152`) returns 403 for `builtin` scope unless
  `actor.IsAdmin`. The note exists to make the reader act, so it must say who can.
- **S4 — no bound on wave REPETITION.** Planning turns re-enter from two loops:
  `QUESTION_MAX` default 5 ⇒ up to 6 per gate entry, and `PLAN_MAX_REVISIONS` default 3
  ⇒ up to 4 entries (`config.go:663-664`). Ceiling 24 planning turns, each now carrying a
  wave, against a **non-resetting** 2h wall (`started_at` is `COALESCE`d once,
  `runtime.sql:647`). One clause fixes it and is right on the merits: on a revise turn,
  re-cite only what changed.
- **S5 — record D6's residual in §467.** A plan asserting **zero** mechanisms is
  compliant and gets no wave. The attacker keeps a cost channel, not a safety channel —
  there is no reachable state where a plan asserts a mechanism and skips its citation.
  Write the residual down; optionally one sentence telling the lead a plan asserting
  nothing is itself suspect.
- **S6 — "the only path" over-claims, in `specs/ai.md` §467 and in `2f0017b5`'s message.**
  The CHANGELOG never says it (verified: 0 matches) and is fine. But an admin
  hand-pasting the body also works, and `docs/agent-templates.md:143-144` already says so.
  Say "the only mechanism" where the text survives; the commit message is immutable.

#### D12 (NEW DECISION) — `architect` is deliberately NOT sequenced, and that is half of what the issue asked for

The issue proposes dispatching *"the allocated read-only validators (**and `architect`,
when allocated**)"*. Shipped `lead.md` says only "the allocated read-only validators", and
`architect` mentions appears **zero** times in it (measured). Since `architect` declares
`Edit, Write` it is not a read-only validator, so it is excluded by the wording.

That follows from D5 rather than contradicting it, and it is the safe outcome — but it
was never written down as a decision, which is how a gap becomes a surprise. **Record it:**
`architect` joins the plan-turn wave when **#203** removes its write tools, not before.
Nothing shipped is untrue today; §467 mentions architect only in the statement of the
defect. State it in the MR description so it is a decision on the record.

Note this does **not** weaken B1: `tester` also declares `Edit, Write` and *is* named a
read-only validator by the product's own docs, so the relay verb is still load-bearing
with architect excluded.

#### Not defects, recorded so nobody re-derives them

- `lead.md`'s *"hands the task straight back instead of reading anything"* is slightly
  stronger than any single source line — `tester.md:89-90`'s trigger is an unclear spec,
  not a missing diff (its diff presupposition is at `:20-21`). §467's shipped wording
  ("surface missing context") is careful and true of all three. Leave it.
- *"all three plan prompts end with it"* — `buildPlanPrompt`'s `submit_plan` at `:559-561`
  has a 3-line PRD conditional after it, so it is the terminal *instruction*, not the last
  line.
- `prds/done/43:29`/`:46` still carry the old ordering and are **correctly** left alone:
  past-tense records of a shipped PRD, per `CLAUDE.md`'s convention.

#### Process, for the reflect pass

- **A shared worktree needs per-agent isolation stated in the dispatch, not assumed.** The
  tester restored after each of ten folds and proved the tree clean at the end — but each
  fold held a mutation for 10-30s, and the auditor's gate run and the fact-checker's prose
  read both landed in one of those windows. "Clean at the end" is not "never dirty". My
  dispatch said "detached worktree" to the reviewer and not to the tester.
- **Namespace scratchpad log paths.** `gate-api.log` was clobbered by three agents; one
  red read from it was briefly taken for a regression.
- **Prefer `git ls-remote` over `git fetch --all` for a freshness check** while other
  agents are running — the fact-checker avoided mutating shared refs and still proved the
  tracking refs current. My spec-sync instruction said `fetch --all`; it was wrong to.
- **The lead's zsh `git show` warning was WRONG IN ITS MECHANISM, and it went to two
  agents.** I told the tester and fact-checker that `git show $sha:api/…` "silently reads
  a nonexistent path, returning the SHA-1 of empty input rather than an error." Measured:
  it fails **loudly** — `rc=128`, `fatal: ambiguous argument '…/60bf036fpi/internal/…'`,
  and **no stdout at all**. The empty-input hash in `CLAUDE.md`'s account is what a
  *downstream hasher* produces when fed that empty stdout, not something git returns. So
  the real hazard is narrower and more actionable: `n=$(git show "$r:file" 2>/dev/null | …)`
  in a sweep loop, where suppressing stderr and ignoring `rc` converts a loud failure into
  an empty string that reads as "this branch has no such file". Two distinct expansions,
  both non-zero: `:s` (as in `:specs/…`) aborts with `bad substitution`; `:a` (as in
  `:api/…`) absolutizes and hands git garbage. Bracing — `git show "${sha}:path"` — fixes
  both and needs no second variable.
- **Third citation defect in this brief, all three the lead's.** `tester.md:8`/`:20-21`,
  `prompt.ts:908`, and now `a78fc52` for a sentence that is `0a9f331`'s. Each reached this
  file from a report or from memory and was written down without being opened. The control
  is one command — the skills repo's own `SKILL.md:383` says *"A CITATION is an assertion
  too, and `git log -S` is its control"*, in the very file being mis-cited.
