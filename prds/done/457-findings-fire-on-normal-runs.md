# PRD #457 — Make incidental findings actually fire on normal runs

**Issue**: [#457](https://github.com/vtmocanu/uzi/issues/457)
**Priority**: Medium
**Status**: Complete

## Problem

The **Findings** backlog (incidental findings, `docs/findings.md`) stays empty on
ordinary runs. A worker is supposed to call `mcp__findings__report_incidental_issue`
when it reads past an off-task bug, but in practice the tool only fired on a run that
was explicitly told to hunt for bugs. Normal issue / ci_fix / prompt / self_improve
runs produce no findings, so the feature is effectively dark.

Investigation (all facts below verified against the current tree, no internet needed
to re-verify — every claim is a codebase read) found the plumbing is correct and the
cause is two independent design gaps.

## What already works (do not change)

The capture path is sound and is not the problem:

- The in-process MCP server is wired **unconditionally** onto the run lane for
  `issue` / `ci_fix` / `prompt` / `self_improve` runs — `agent/src/sdk-executor.ts`
  (`buildFindingsToolsServer(...)` in the `if (this.client)` block, ~line 717).
- It runs under `permissionMode: "bypassPermissions"` with `disallowedTools`
  containing only the async-deferral tools, so nothing globally blocks the tool.
- The handler POSTs worker→api, emits a `finding` stream card, and returns a soft
  ack; the api re-sanitises every field and enforces a **per-run cap of 10**
  (`MaxFindingsPerRun = 10` in `api/internal/workersvc/findings.go`; the agent side,
  `agent/src/findings-tools.ts`, only turns the api's 429 into a soft ack). A
  forced/"go find bugs" run used it successfully, which is how we know the end-to-end
  path (worker → api → backlog) is healthy.

## Root causes

**A — Nothing ever tells any agent the tool exists.**
The only description of the capability is the MCP tool's own schema string
(`agent/src/findings-tools.ts`, the `report_incidental_issue` tool definition).
No system prompt or turn prompt references it:

- `buildLeadSystemPrompt` (`agent/src/prompt.ts`, ~line 175) appends only
  `LEAD_GUARDRAIL_APPEND`, `PRD_LIFECYCLE_APPEND` (issue kind),
  `REPO_SUBAGENT_UNTRUSTED_APPEND` (repo-sourced), and the untrusted repo
  instructions. None mentions findings.
- `buildImplementPrompt` / `buildPlanPrompt` (`agent/src/prompt.ts`) — the only
  "finding" mention is *review* findings, unrelated.
- No builtin agent template (`api/internal/agenttmpl/builtins/*.md`) mentions the
  tool; every "finding" there means reviewer/audit findings.

A model does not spontaneously invoke a secondary-behaviour tool the task prompt
never references. When forced, the task itself said "find bugs", so the lead/coder
reached for it.

**B — The agents that actually read the code can't call the tool.**
Subagent tool access is an allowlist set **verbatim** from each template's `tools:`
line, in `toDefinition` (`agent/src/agents.ts`: `if (t.tools && t.tools.length > 0)
def.tools = [...t.tools];`). Nothing runtime-appends the findings tool (contrast the
signal/memory servers, which are *denied* to subagents there). So the tool is
available only to:

- `lead` — the main thread (no per-subagent allowlist), but it orchestrates and
  delegates reading to subagents; it rarely reads past code itself.
- `coder` — ships **no** `tools:` line → inherit-all → has the tool; but it is
  heads-down on *the task*, i.e. the role least likely to be off-task.

Every broad-reading role declares a restricted `tools:` list that **omits** the tool
and therefore literally cannot call it — verified in the builtin templates:

| Role | `tools:` includes findings tool? |
|---|---|
| `reviewer` | no (`Bash, Read, Grep, Glob, WebFetch, SendMessage, …`) |
| `researcher` | no |
| `auditor` | no |
| `tester` | no (has `Edit, Write` but not findings) |
| `architect` | no |
| `fact-checker` | no (explicitly lists `mcp__forge__*` but not findings) |
| `coder` | yes (inherit-all, no `tools:` line) |

So the population most likely to notice an unrelated bug is exactly the population
that cannot report one.

**Together**: a normal run gives no nudge (A) and hands the capability to the wrong
roles (B) → zero findings. A forced run overrides A for the two roles that do hold
the tool.

## Key constraint: shared-library repo agents

This deployment (and many users) run with **repo-sourced** agents — the roster in the
clone's `.claude/agents/`, drawn from a shared library — not the builtin templates.
Those agent files are external and must not be edited by this change. Both root
causes must therefore be fixed by **injection at agent-assembly time**, not by editing
template files.

The single chokepoint that already covers both paths is `toDefinition` in
`agent/src/agents.ts`: it is called by `assembleAgents` (own/builtin roster) **and**
by `subagentsFromTemplates` (repo roster). There is precedent for post-build mutation
over both rosters (`applySubagentModelOverride`). This is where the fix belongs.

## Solution

Two changes, both injection at assembly time:

1. **Capability (root cause B).** In `toDefinition`, when a subagent has a non-empty
   `tools` allowlist, append the findings tool name
   (`reportIncidentalIssueToolName()` from `agent/src/findings-tools.ts`), guarding
   against a duplicate. When `tools` is unset (inherit-all, e.g. `coder`), the tool is
   already available — leave it. This covers builtin + repo + custom rosters uniformly.
   The tool is **not** a write tool (`WRITE_PATH_TOOLS`), so it survives the plan-turn
   write-strip in `planTurnSubagents` — findings can fire during the read-heavy
   planning phase too.

2. **Discovery (root cause A).**
   - **Lead**: add a short findings nudge to the lead system-prompt append in
     `buildLeadSystemPrompt` (new append constant alongside `LEAD_GUARDRAIL_APPEND`),
     unconditional across run kinds (the tool is mounted on all run lanes).
   - **Subagents**: inject the same one-line nudge into every subagent's `prompt` in
     `toDefinition`, so shared-library repo agents get it **without editing their
     files**.

The nudge should be short and point at the tool — the tool's own schema description
already carries the "only real off-task bugs, no style nits, don't stop your task"
guardrails, so the nudge must not duplicate them. Suggested wording (final wording is
the implementer's; keep it terse and framed as capability, not obligation):

> If while working your task you notice a real, actionable bug **outside** your
> current task, call `mcp__findings__report_incidental_issue` to record it and keep
> working — don't fix it and don't stop your task. Off-task bugs only; when in doubt,
> keep working.

## Milestones

- [x] **M1 — Capability grant at assembly.** `toDefinition` appends the findings tool
  to every subagent that has a restricted allowlist (dedup-guarded); inherit-all
  subagents untouched. Applies to both the builtin roster (`assembleAgents`) and the
  repo roster (`subagentsFromTemplates`). The tool survives the plan-turn write-strip.
  **Explicit deliverable: preserve the write-only-drop decision (R1).** The grant must
  not rescue a write-only custom agent (`tools: Edit, Write`) from being dropped by
  `planTurnSubagents` on the plan turn — compute that drop against the pre-grant list,
  or exclude the findings tool from the "consisted only of write tools" test. Ship the
  regression test with this milestone (see M4).
- [x] **M2 — Lead discovery nudge.** A short findings nudge is appended in
  `buildLeadSystemPrompt`, on every run kind.
- [x] **M3 — Subagent discovery nudge.** The same one-line nudge is injected into each
  subagent's `prompt` in `toDefinition`, reaching shared-library repo agents without
  editing their files.
- [x] **M4 — Tests.** Unit tests prove: every subagent def carries the findings tool
  in its allowlist and the prompt nudge, across **both** the own/builtin path and the
  repo-sourced path; an inherit-all subagent (`coder`) is unaffected and can still
  call it; the plan-turn roster still carries the tool; and the lead system prompt
  carries the nudge (`agent/test/prompt.test.ts`, which directly tests
  `buildLeadSystemPrompt`). **Ship the R1 regression test explicitly: a write-only
  custom agent (`tools: Edit, Write`) is STILL dropped by `planTurnSubagents` after
  the grant.** Update the pinned tool-content/order assertions
  (`agent/test/agents.test.ts`, `agent/test/sdk-executor.test.ts`,
  `agent/test/repoagents.test.ts`) that will move when the allowlists gain a member,
  and confirm the repo-roster path tests (`agent/test/runner-repo-agents.test.ts`,
  `agent/test/sdk-resume-honors-agents.test.ts`) don't deep-equal tool contents in a
  way the added member breaks.
- [x] **M5 — Docs.** Reconciled `docs/findings.md` with the new behaviour. It already
  describes "the worker" flagging findings with no lead-only wording, and the "Which
  runs can report a finding" note is framed by run kind, not by which internal agent
  reports — so no edit was warranted (the optional subagent note was declined because
  it would describe internal wiring, which this milestone excludes). No file change.

## Success criteria

1. On a normal (non-forced) issue run over a repo that contains at least one genuine
   off-task bug, a read-heavy subagent (reviewer/auditor/researcher) is *able* to and
   does record an incidental finding — validated at least at the unit level (the tool
   is in its resolved allowlist and the nudge is in its prompt) and, ideally, on a
   live/dogfood run.
2. Both the builtin roster and the repo-sourced roster carry the capability and the
   nudge; shared-library agent files are unchanged on disk.
3. `coder`'s tool/capability access is unchanged — it stays inherit-all and can still
   call the tool; it gains the same one-line prompt nudge as every other subagent.
4. The plan-turn roster still carries the findings tool (write-strip does not remove
   it).
5. `task gate:agent` is green (lint, deadcode, typecheck, tests), plus any api-side
   template tests touched.

## Decision log

- **D1 — Grant the tool to *all* subagents, not only the read-only ones.** Every
  subagent reads code and can plausibly read past an off-task bug; the api's per-run
  cap of 10 bounds noise regardless of how many roles can report. Unlike the signal
  and memory servers (kept lead-only for control-flow integrity and provenance),
  findings is fire-and-forget: no turn-ending, no control-flow effect, run-scoped by a
  closure over `runId`, api-sanitised and capped. There is therefore no integrity
  reason to restrict it to the lead, and a uniform grant is the simplest robust rule.
- **D2 — Inject at `toDefinition`, not by editing templates.** The deployment runs
  shared-library repo agents whose files must not be edited; `toDefinition` is the one
  seam both the builtin and repo rosters pass through, so a single injection covers
  every source (builtin, repo, custom) and future rosters. This is the mechanism the
  issue author (the user) proposed.
- **D3 — Nudge points at the tool, does not restate its rules.** The tool's schema
  description already carries the quality bar ("real bugs, outside your task, no style
  nits, don't stop"). The nudge exists only to make the tool *discoverable*; keeping it
  terse avoids two divergent copies of the same guidance.
- **D4 — Lead nudge is unconditional across run kinds.** The findings server is mounted
  on all run lanes (issue/ci_fix/prompt/self_improve), so the nudge is not gated to
  `issue` the way `PRD_LIFECYCLE_APPEND` is.

## Risks and edge cases

- **R1 — A write-only custom agent could be rescued from the plan-turn drop.**
  `planTurnSubagents` drops any subagent whose allowlist consisted **only** of write
  tools (empty on every shipped builtin). If M1 naively appends the findings tool to
  such an agent's list, after the write-strip it becomes a findings-only agent and is
  no longer dropped — it would run on the plan turn. Mitigation: apply the capability
  grant so it does not change the write-only-drop decision (e.g. compute the drop
  against the pre-grant list, or exclude the findings tool from the "consisted only of
  write tools" test). The implementer must handle this so the plan-turn drop behaviour
  for write-only agents is unchanged. Add a test.
- **R2 — Pinned tool-list assertions.** `agent/src/agents.ts` notes the content and
  order of subagent tool lists are pinned by tests. M1 changes those lists, so the
  pinned assertions must be updated deliberately (not loosened) — M4 owns this.
- **R3 — Prompt-injection surface unchanged.** The nudge is uzi-authored trusted text;
  it adds no new untrusted input. The finding fields stay untrusted and are already
  sanitised at the api and rendered inert in the UI. No guardrail layer is touched.
- **R4 — More findings, some low-value.** Making the feature actually fire will
  surface findings that were previously never produced, including some the user may
  dismiss. That is the intended outcome (the backlog is a human-gated triage queue);
  the per-run cap of 10 and the dedupe-by-location keep it from flooding.

## Out of scope

- Any change to the api-side findings store, cap, dedupe, filing, or the web/CLI
  surfaces — the capture path already works.
- Changing which run kinds mount the findings server (chat deliberately excluded — it
  has `propose_issue`).
- Editing the shared-library `.claude/agents/` files.
