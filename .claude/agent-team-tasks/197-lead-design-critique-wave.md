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

- *(none yet)*
