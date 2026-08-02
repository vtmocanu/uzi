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
  design" is a mood; the lookup is what has actually paid.
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
  TaskGet`, **none of which exist in a uzi run**. Pre-existing; D5 contains it for now.
- Whether #85 grows a second track for the orchestration half.

#### Standing, unchanged

No amends after a SHA is dispatched for review. Report the tip SHA at the top of every
report. Commit locally on `fix/197-lead-design-critique-wave`; never push, never touch
`main`.
