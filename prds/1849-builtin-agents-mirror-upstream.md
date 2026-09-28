# PRD #1849: Builtin agent bodies mirror the upstream role library

**Issue**: [#1849](https://github.com/vtmocanu/uzi/issues/1849)
**Status**: In progress (implemented locally by the maintainer session; M6 after release)
**Priority**: Medium
**Owner**: maintainer (M1, M6); uzi run (M2 to M5)

## Problem

uzi ships twelve builtin agent roles (`api/internal/agenttmpl/builtins/*.md`). Eleven of them (all but `lead`) started as copies of the generic role library in `vtmocanu/skills` (`skills/agent-kit/agent-team/roles.yaml`, published tail-free as `product-agents/`). The two have drifted apart, and the drift has three bad effects:

1. **Equal version, different body.** At skills `v0.39.0`, all eleven shared roles (every builtin except `lead`) carry the same `version:` as upstream, and ten of them differ in body (measured 2026-09-28; only `researcher` matches). The version number no longer tells anyone which body they have.
2. **Fixes flow one way, and not even that way reliably.** Generic improvements made here (the "via SendMessage to `main`" recipient fixes in architect, documenter, spec-keeper, web-ux) never reached the library, and library fixes reach uzi only when someone hand-ports them.
3. **The admin agent-source sync serves stale bodies.** An instance that syncs roles from `product-agents/` (ADR-0602) replaces the shipped builtin bodies wholesale. It then loses uzi's runtime-specific rules (#1765 run-scratch wording) and gets whatever upstream tag it is pinned to. Observed on a live instance: ten builtins synced from `v0.37.0`, missing the #1658 safety rules, and one builtin (`coder`) marked "differs from shipped" after an admin model edit.

Root cause: the builtin bodies mix two kinds of content. **Generic role guidance** (belongs upstream, identical for every harness). **uzi runtime facts** (the `.uzi/scratch/` directory, the `git archive` review-snapshot recipe, the forge MCP tools), which only hold on a uzi worker.

## Current state (verified 2026-09-28)

- `agent/src/agents.ts` `toDefinition` already appends worker-owned text to every subagent prompt, on both rosters (builtin templates and repo-authored agents): `FINDINGS_NUDGE_APPEND`, `WORKER_RUNTIME_APPEND`, `SECRET_FIXTURE_HYGIENE_APPEND`, `SUBAGENT_SAFETY_APPEND`. The Codex path does the same in `agent/src/codex/render.ts` (`renderSubagentPrompt`).
- `SUBAGENT_SAFETY_APPEND` already starts with `RUN_SCRATCH_GUIDANCE` (`agent/src/prompt.ts`): the `.uzi/scratch/` directory, `mktemp .uzi/scratch/gate-log.XXXXXX`, and the fresh-export snapshot recipe. PRD #1719 D9 put it there on purpose. So most of what #1765 wrote into the builtin bodies is already delivered to every subagent by the worker.
- `api/internal/agenttmpl/scratch_guidance_test.go` pins those strings **inside the builtin bodies** and bans upstream's wording (`git worktree add --detach`, `./gate-log.XXXXXX`, "outside the tracked tree", "it can never be staged", "add the pattern if it is not"). The ban is right for a uzi worker: nested worktrees are refused there (PRD #1719 D7), and file tools deny paths outside the worktree.
- PRD #1719 "Out of scope" already names this follow-up: generic roles should say "use the worker-provided scratch directory", with a host fallback, never uzi's literal path.
- `fact-checker.md` declares `mcp__forge__*` read tools in its `tools:` line. Upstream cannot, since those tools exist only on a uzi worker.
- `api/cmd/roleparity` (`task nudge:roles`) compares role NAMES between `.claude/agents/` and `builtins/`. Nothing compares builtin BODIES with upstream.

## Decisions

- **D1. Split by audience.** A builtin body is generic role guidance. uzi runtime facts live in the worker's append layer (`agent/src/prompt.ts`, both harnesses), which reaches builtin and repo-authored agents alike.
- **D2. Builtin bodies mirror upstream.** For every role present in both, the body, `description` and `model` of `builtins/<role>.md` equal `product-agents/<role>.md` at a pinned skills tag, and `version:` equals upstream's. `lead` is product-only and exempt.
- **D3. Upstream becomes runtime-neutral first.** Before uzi resyncs, `roles.yaml` changes so no generic body contradicts a uzi worker: gate logs and review snapshots go in "the scratch directory your runtime provides". The host fallback is conditional: a gitignored path otherwise, and a throwaway detached checkout **only when your runtime permits one** (a uzi worker does not). Ignored artifacts are not staging-proof (`git add -f` overrides an ignore rule), so the bodies require explicit stage-by-path discipline instead of claiming an ignored file "can never be staged". None of the banned literals above appear in a generic body. Dev-team repos keep today's behaviour.
- **D4. Generic fixes go upstream.** The "via SendMessage to `main`" recipient fixes move into `roles.yaml`, so every consumer gets them.
- **D5. Product-only tool grants are a declared delta, not a body fork.** `tools:` may differ from upstream only by entries listed in a committed, embedded accepted-delta file in `api/internal/agenttmpl/` (today: fact-checker's `mcp__forge__*` read tools). The delta applies wherever a builtin row's tools are written from upstream content: the shipped file, AND an agent-source sync overriding a builtin (`agentsource.executeApply`, `ActionOverrideBuiltin`, which today writes the source role's `Tools` verbatim, so an approved sync would strip fact-checker's forge access). A worker-side grant keyed on role name was rejected: it would also hand those tools to a repo-authored agent that happens to be named `fact-checker`.
- **D6. Drift is a nudge, never a gate** (CLAUDE.md, "Builtin agent templates"). A new check reports builtin-vs-upstream differences outside D5's delta. It never fails a build.
- **D7. Record.** The split (D1, D2, D5) is a seam future edits must respect, so it gets an ADR numbered by this issue, written in M5.

## Out of scope

- Any `.github/workflows/**` change (worker PAT lacks `workflow` scope).
- `.claude/agents/*.md` (the dev-team roster; already synced with `roles.yaml` via `sync.py`).
- Changing ADR-0602's agent-source design. Whether a given instance keeps agent-source enabled is an operator decision (M6).
- Builtin skills.

## Milestones

| Phase | Milestone | Depends on | Where |
|---|---|---|---|
| 1 | M1 | none | `vtmocanu/skills` (maintainer, not a uzi run) |
| 2 | M2 | M1 released | `agent/src/prompt.ts`, `agent/src/codex/render.ts`, agent tests |
| 3 | M3, M4 | M2 | `api/internal/agenttmpl/**`, `api/internal/agentsource/**`, `api/cmd/**`, `Taskfile.yml` |
| 4 | M5 | M3, M4 | docs, ADR, CLAUDE.md, CHANGELOG |
| 5 | M6 | uzi release containing M3 | live instance (maintainer) |

One gated uzi run for M2 to M5, sequential. **Dispatch it only after the M1 skills release is published**: M2's body diff, M3's copy and M4's pin all need the final upstream bodies and tag, and the PRD must name that tag before dispatch.

- [x] **M1. Upstream: runtime-neutral, generic fixes in (maintainer, skills repo).** In `roles.yaml`: apply D3 to coder, tester, reviewer, auditor, fact-checker, ux-designer, web-ux and `tui-ux`, and D4 to architect, documenter, spec-keeper, web-ux. `tui-ux` is not a builtin, but an agent-source sync adds every published role as a global template, so its body reaches uzi runs too. Bump each touched role's `version:`, regenerate `product-agents/` (`publish_roles.py`, CI drift check green), cut a skills release. Acceptance: no banned literal from `scratch_guidance_test.go` appears in any `product-agents/*.md` body, checked with whitespace flattened (upstream `web-ux` wraps "outside the tracked tree" across two lines, which a line-oriented grep misses); `sync.py check` in this repo reports the dev-team roster clean after `apply`.
- [x] **M2. The append layer carries every uzi runtime fact.** Diff each builtin body (except `lead`, which stays product-only and outside M3) against its M1 upstream body. Every line that is a uzi runtime fact and is not already in `RUN_SCRATCH_GUIDANCE` / `SUBAGENT_SAFETY_APPEND` moves there, on both harnesses. Known gap today: `RUN_SCRATCH_GUIDANCE` (`agent/src/prompt.ts`) has the scratch path and archive recipe but not "Git commands run inside an export can find the parent checkout; never run Git there". Tests: the rendered subagent prompt on both harnesses (Claude `toDefinition`, Codex `renderSubagentPrompt`) contains the gate-log path, the snapshot recipe with `set -o pipefail`, and the never-run-Git-in-an-export rule, for a builtin rendered from its **M1 upstream body** AND for a repo-authored agent. The red/green check must use those bodies: today's builtin bodies still carry the rule themselves, so removing the append line would leave a test over them green.
- [x] **M3. Resync builtins to upstream.** Replace the body, `description`, `model` and `version:` of the eleven shared builtins with the M1 release's `product-agents/` content; keep D5's tools delta, and make an agent-source builtin override apply the same delta (`agentsource.executeApply`) with a regression test: an approved sync of an upstream `fact-checker` without forge tools leaves the row with them, shown red before the fix. Rework `scratch_guidance_test.go`: drop the "body must contain `.uzi/scratch`" assertions (M2 now pins those on the rendered prompt) and keep the banned-literal assertions as a guard that no generic body contradicts the worker. Pristine rows pick the new bodies up on boot (`RefreshPristineBuiltin`); note that in the CHANGELOG.
- [x] **M4. Parity nudge.** A `task nudge:builtins` target (a new `api/cmd` tool, or an extension of `roleparity`) compares `builtins/*.md` with `product-agents/*.md` at the tag recorded in a committed pin file, and reports body, `description`, `model` or `version:` drift, plus `tools:` drift outside the D5 accepted-delta file. Exit 0 whether or not it finds drift (a nudge), with a distinct non-zero code only when the instrument itself fails (upstream unreadable, pin file missing), following `scripts/deadcode-gate.sh`'s convention. Tested offline against fixture directories: in sync, one body drift, a tools entry inside the delta, a tools entry outside it, and an unreadable source. The live fetch is maintainer-run and needs network; the worker only needs the fixtures.
- [x] **M5. Docs and ADR.** ADR for D1, D2 and D5 (filename by issue number). Update CLAUDE.md "Builtin agent templates" (builtins mirror upstream plus the append layer; sync procedure; `task nudge:builtins`), `docs/agent-templates.md` (what "differs from shipped" means now), and `docs/agent-source.md` (a synced body and the shipped body now come from the same library). Run `task docs:sync` and `task check-docs:web`. CHANGELOG `[Unreleased]` line.
- [ ] **M6. Live rollout (maintainer, after release).** On each instance: decide whether agent-source stays enabled (if kept, bump its ref to the M1 tag); Reset every synced or customized builtin that should track shipped; confirm no "differs from shipped" badge remains unintended; spot-check one rendered subagent prompt for the scratch rules.

## Success criteria

- `task nudge:builtins` reports no drift at the pinned tag.
- Every subagent on both harnesses receives the uzi scratch rules through the append layer, builtin or repo-authored.
- A skills fix reaches uzi through one resync plus a release, with no hand-porting of body text.

## Risks

- **M1 changes every dev-team repo using the skill.** Mitigation: the host fallback keeps current host behaviour; M1 reviewed as a library change.
- **Rendered-prompt length.** Moving lines into the append layer must not duplicate them: after M3 a line lives in the body or the append, never both. M2's diff is the check.
- **Customized rows keep old bodies.** Expected (edit-preserving by design); M6 resets what should track.

## Decision Log

- 2026-09-28: D1 to D7 agreed in the maintainer session (maintainer's model: dev-team agents carry repo specifics in their `## For this repo` tail; shipped builtins mirror upstream; port what makes sense upstream). Interim: the live agent-source sync stays as is until this PRD lands.
- 2026-09-28: Codex peer review (REVISE, 4 items, all accepted): D5 delta must survive an agent-source builtin override; M2's red/green check runs over the M1 upstream bodies, not today's; D3's host fallback is conditional and staging-safe; the uzi run is dispatched only after the M1 release.
- 2026-09-28: Codex peer re-review (REVISE, 1 item, accepted): M1 also covers `tui-ux`, a synced global role whose body carries a banned literal; M1's acceptance check flattens whitespace.
- 2026-09-28: Implementation choices (maintainer session, Codex peer reviewed M1 over 4 rounds). **Existing machinery, not new:** `library/manifest.json`, `TestBuiltinLibraryDrift` and the weekly `roles-manifest-refresh` workflow (PRD #85) already gate the version stamps; this PRD keeps them and changes only what the body copy means ("adapted" became "verbatim"; `library/README.md` rewritten). **D5 as code, not a file edit:** the delta is `productToolDelta` in `api/internal/agenttmpl/tool_delta.go`, applied by `WithProductTools` at builtin init and in agentsource's diff and override, so every builtin file stays byte-identical to upstream and the M4 nudge is a plain byte compare. The round-trip render test now parses the file itself. **M4** is `scripts/builtin-parity.sh` (fetches `product-agents/` at the manifest SHA) plus `api/cmd/builtinparity`. **M2** also adds `scratch=.uzi/scratch` and "no detached checkout here" to the append, which the runtime-neutral upstream bodies defer to.
- 2026-09-28: M1 landed as vtmocanu/skills PR #78 (squash `f3246f35`); the manifest pins that commit rather than a release tag, since `library/manifest.json` has always pinned a SHA.
