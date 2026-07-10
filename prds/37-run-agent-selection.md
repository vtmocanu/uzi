# PRD #37: Per-Run Agent Selection — Repo `.claude/agents` Detection with Plan-Gate Choice

**GitLab Issue**: [vtmocanu/uzi#37](https://gitlab.example.com/vtmocanu/uzi/-/issues/37)
**Status**: Draft — reviewed 2026-07-10 by 3 agents (design, security, fact-check); all blocking/major findings folded in below (marked ↳review where the design changed).
**Priority**: Medium
**Created**: 2026-07-10
**Mockup**: [prds/mockups/37-agent-picker-mock.html](mockups/37-agent-picker-mock.html) — approved by the user; the implemented UI must be visually compared against it (M5).
**Depends on**: PRD #3 (agent templates, done), PRD #4 (worker runtime, done), PRD #16 (skills — precedent for loading a capped, parsed subset of a repo's `.claude/` dir, done), PRD #17/#18 (lead template + worker templates, done).

## Problem

Every run uses the user's uzi agent templates (DB-seeded builtins plus custom ones). Repos that ship their own curated agent roster in `.claude/agents/` — including uzi itself, whose dev team defines eight role agents there — are ignored: the worker sets `settingSources: []` (`agent/src/sdk-executor.ts:214`) so nothing under a cloned repo's `.claude/` is loaded, and the user has no way to say "this repo's team knows this codebase better than my generic templates". The plan-approval gate, the run's one human decision point, offers no agent choice at all.

## Solution Overview

1. After clone, the worker enumerates and parses the repo's `.claude/agents/*.md` (frontmatter + prompt body, size/count caps) and reports the detected roster on its first `running` state report — decoupled from the gate so autopilot runs record it too (↳review B2/m1).
2. The plan-approval panel grows an "Agents for this run" section (per the approved mock): two radio cards — **Repo agents** (default when detected, showing detected names) and **My agent templates** — with per-agent exclusion chips and a live approve-button label. When nothing is detected, the repo card is inert and the user's templates are the default.
3. The `approve_plan` input carries the selection (`source` + exclusions); the worker rebuilds the subagent roster at the gate boundary for the implementation phase. The `lead` is always uzi's builtin and is never selectable.
4. Autopilot/PRDLESS runs (which auto-resolve the gate worker-side) apply the default automatically — repo agents if detected, else the user's templates — persist that resolved selection via a state report, and say so in the activity feed.

## Design Decisions

1. **Repo agent files are parsed by the worker, never loaded by the SDK.** `settingSources` stays `[]`. The worker reads `.claude/agents/*.md` itself (like `agent/src/repo-skills.ts` does for repo skills), converts each to the existing `AgentTemplate` shape (`agent/src/protocol.ts:87`), and feeds them through the same programmatic `query({ options: { agents } })` path (`agent/src/agents.ts`). Repo hooks, settings, and commands never load. This keeps the guardrail *mechanism* intact while adding a deliberate, user-visible opt-in for agent definitions.
2. **Repo-declared `tools` and `model` frontmatter are honored (user decision, 2026-07-10) — with a hard denylist, model validation, and an honestly-stated residual.** ↳review F1/F5/F7/n1/M2:
   - **This deliberately diverges from the repo-skills posture** (`repo-skills.ts:49` drops every frontmatter key except name+description; ARCHITECTURE.md calls that stripping "the security point"). The divergence is conscious: an agent definition without its tools is not that agent. Documented here so it reads as a decision, not an inconsistency.
   - **Denylist repo agents can never get**: `Agent` and the async-deferral tools (already structurally denied for every subagent — `agents.ts:73`, `sdk-executor.ts:235`) **plus `WebFetch`/`WebSearch`** (new: repo-sourced subagents never receive a first-class network tool). `disallowedTools` wins over a repo-declared `tools` allowlist; M3 pins this with a test. Unknown tool names in frontmatter are silently unavailable, not errors.
   - **Mitigations that actually hold**: `PreToolUse` deny-hooks (`agent/src/guardrails.ts` — git push, history rewrites, `env`/`ps`/procfs/secret-path reads), no nested Agent spawning, the **forge PAT** is worker-held (the agent never sees it), GitLab Developer role + protected branches.
   - **Corrected claim + named residual (security F1)**: the agent subprocess env DOES hold the Anthropic OAuth token (`agent/src/sdk-env.ts` sets `CLAUDE_CODE_OAUTH_TOKEN`) — "the agent never sees credentials" is true only for the forge PAT. Since Bash remains available to coder-type roles, shell-level egress (`curl`, `node -e 'fetch(…)'`) is an open exfiltration channel for that token once prompt authorship is attacker-controlled. **Accepted residual for this PRD**, recorded in specs and docs; the structural close (agent container on an `internal` compose network + egress allowlist, already contemplated in docs/proc-hardening.md) is a follow-up PRD candidate, not in scope here.
   - **Model**: repo `model` must pass the existing `ValidateModel` (`api/internal/agenttmpl/model.go:27`); values outside `MODEL_ALIASES` are ignored (inherit the run default) — bounds cost abuse (a repo pinning the most expensive model onto the user's quota) and bogus-id self-DoS. Alias models are honored as declared.
   - A parse-failed or over-cap file is skipped with a run-message note, never a run failure.
3. **`lead` is pinned, and repo subagents are untrusted in its eyes.** The orchestrator always comes from the claim payload (builtins-only, PRD #17). Repo agents and exclusions apply to subagents only; a repo file named `lead` is just another subagent candidate. ↳review F2: because choosing the repo source replaces *all* subagents — including reviewer/auditor — with attacker-authorable ones, (a) the builtin lead prompt gains a conditional passage (only when repo source is active) that repo-sourced subagent results are unverified input, not uzi's own review; (b) the run view and the MR description state that the run used repo agents, so the human MR reviewer knows the internal review loop was repo-authored. Decision 4 (no mixing) stands; "keep verification roles builtin" was considered and rejected as mixing by the back door.
4. **Either/or source with exclusions, no mixing (user decision, 2026-07-10).** One source per run; chips exclude individual agents from the chosen source. At least one subagent must remain selected (UI-enforced, server-validated). No name-collision rules needed.
5. **Selection is applied at the gate boundary — which is real, but the re-assembly is new work (↳review B1, fact-check).** The plan turn runs exactly as today (claim templates): the roster question only matters once the plan is approved. But the executor builds `baseOptions` (agents map, Agent-guard hook, model) ONCE at `sdk-executor.ts:208` and every turn reuses it via `{ ...baseOptions, abortController }` (`:374`); the plan and implement turns share one resumed SDK session. Applying the selection therefore means, between the gate verdict and the implement loop (`:319-321`): rebuild the agents map from the chosen source, rebuild the `PreToolUse` Agent-guard hook (its `allowSet` is frozen at construction — `guardrails.ts:435`), and recompute the subagent names fed to the implement prompt. **`prompt.ts` hardcodes "coder and reviewer" in `buildImplementPrompt` and `LEAD_GUARDRAIL_APPEND` (↳review M1) — these are genericized to the resolved roster names**, otherwise a repo roster without those names gets a prompt referencing agents that don't exist. Caveat stated openly: the human approves a plan whose delegation targets may then be swapped; the panel copy ("Agent choice locks in on approval") and the read-only post-approval view make that visible.
6. **The roster snapshot and the autopilot selection travel on state reports, not the gate (↳review B2/m1).** `repo_agents` (names/descriptions only) rides the first `running` report after detection — every run records what was detected, gate or no gate. Autopilot runs (worker self-approves in `runner.ts:257-263`, never reporting `awaiting_approval` and never receiving a `SubmitInput`) include their resolved default selection (`source`, empty exclusions) on that report and emit a status run-message ("using 8 repo agents from .claude/agents/"). Human-path selections are persisted by the `SubmitInput` approve branch. Both paths land in the same `runs` columns.
7. **Detection is capped worker-side AND validated API-side (↳review F3/F6).** Worker caps mirror the PRD #16 skills pattern: max 16 agent files, 64 KiB per file; over-cap files dropped with a run-message note. The API does not trust the worker payload: the state endpoint enforces roster length ≤ 16, per-item name/description length caps, control-char rejection, and the kebab-case name rule (mirroring agent_templates validation) before persisting. Detection has no side effects — the roster is data until a selection activates it.
8. **The selection is persisted on the run** (source, exclusions, detected-roster snapshot) so the run view shows which agents ran and the board/history can render it. Two caveats stated plainly: (a) reproducibility is partial — prompt bodies are NOT persisted, so a rerun re-reads the repo, possibly at a different commit; (b) requeue/resume re-enters plan → gate (the executor has no already-approved skip; the worker re-clones and re-detects), so the selection is re-collected at the re-entered gate and the persisted columns are overwritten by the latest approval — no claim-payload re-delivery needed (↳review m3). Migration number is a **draft** (`00061`, live head is `00051`); renumber to the next free slot at landing (CLAUDE.md convention).
9. **Visual parity with the approved mock is a deliverable, not a vibe.** `prds/mockups/37-agent-picker-mock.html` is committed with this PRD; M5 compares the implemented plan panel against it side-by-side in a real browser (web-ux agent: same states, spacing, chip behavior, button-label behavior) and findings are fixed before the PRD closes. The mock is the design contract for States A and B.

## Technical Design

### Worker (agent/)

- **Detect + parse** (new `agent/src/repoagents.ts`): after checkout, glob `.claude/agents/*.md`; parse YAML frontmatter (`name`, `description`, `tools`, `model`) + body → `AgentTemplate`. Name defaults to the filename slug; dedupe on name (first wins, note dropped). Caps per Decision 7; denylist + model clamp per Decision 2.
- **Report**: extend `StateRequest` (`protocol.ts:243`) so the first `running` report after detection carries `repo_agents: {name, description}[]` (bodies stay worker-side), and an autopilot-resolved `agent_selection` when the worker self-approves (Decision 6).
- **Apply** (↳review M3 wire detail): the selection reaches the worker inside the `approve_plan` input. The input wire is `{id, kind, body}` (`workersvc/service.go:718-725` → `steering.ts:99-117`) and `approve_plan`'s `body` is unused today — the selection is **JSON-encoded into `body`**; `PlanVerdict` (`steering.ts:24-27`) and `route()` are extended to carry it. At the gate boundary the executor rebuilds the agents map, the Agent-guard hook, and the implement-prompt roster names (Decision 5). Touched files: `sdk-executor.ts`, `agents.ts`, `guardrails.ts`, `prompt.ts`, `steering.ts` — not `executor.ts` (the stub interface).
- **Autopilot**: the existing self-approve path (`runner.ts:257-263`) fills the default selection, reports it (Decision 6), and emits the status message.

### API (api/)

- **Migration (draft `00061`)**: `runs.agent_source text CHECK (agent_source IN ('repo','own'))`, `runs.agent_exclusions jsonb`, `runs.repo_agents jsonb`. All nullable; NULL = pre-feature run.
- **Worker state endpoint** (`workersvc/service.go:673-687` SetState switch): persist `repo_agents` (validated per Decision 7) from the `running` report; persist the autopilot `agent_selection` when present.
- **Input validation** (`SubmitInput` approve branch; handler decode at `handler/workers.go:478-491` extended for the structured selection): selection only valid on `approve_plan`; `source="repo"` requires a non-empty persisted roster; exclusions ⊂ chosen source's names; ≥1 subagent survives. Persist to the run row; deliver via the `approve_plan` input body. **approve_plan is live-poller-only** — only cancel/reject have a server-side no-poller branch (`service.go:1018-1055`); a run can only be awaiting approval if a live worker put it there (↳review m2).
- **Run DTO**: expose `repo_agents`, `agent_source`, `agent_exclusions` on the run payloads the web consumes.

### Web (web/)

- **PlanPanel** (`web/src/pages/RunView.tsx:319`): the "Agents for this run" section per the mock — source radio cards, detected/none-detected pills, exclusion chips, pinned `lead` summary line, live approve-button label. Approve submits the selection with `approve_plan`.
- **Rendering rule (↳review F4)**: repo agent names/descriptions render as plain JSX text — never through `<Markdown>`, which would make attacker-supplied links clickable inside the approval panel. Vitest asserts a link-bearing description renders inert.
- **States**: A (detected → repo default) and B (none detected → own templates default, repo card disabled) exactly as mocked; the user's template list comes from `api.listAgentTemplates()` (`web/src/pages/Agents.tsx:37`).
- **After approval / terminal runs**: read-only locked-in selection (source + agents), plus the "repo agents" marker per Decision 3(b).

### Docs + specs

- `docs/` agents page (user-audience): repo-agents section — what is detected, defaults, the security posture including the Decision 2 accepted residual and the Decision 3 untrusted-review caveat, stated plainly.
- `specs/ai.md`: Decisions 1–9. `specs/human.md`: the user-stated requirement (choose agents at the plan gate, repo agents default, own templates fallback) — needs user approval to edit.

## Milestones

- [ ] **M1 — Worker detection + report**: `repoagents.ts` parser (caps, dedupe, denylist, model clamp, skip-on-parse-error), roster on the first `running` report, autopilot `agent_selection` shape; unit tests for parser/caps/denylist. Validation: a run against the uzi repo reports the 8 dev-team agents.
- [ ] **M2 — API persistence + selection contract**: migration (draft `00061`), state-report persistence **with API-side roster validation (Decision 7)**, `approve_plan` selection validation + body encoding, run DTO exposure; Go tests for the validation matrix (incl. oversized/malformed roster rejected). Validation: bad selections 400; roster round-trips worker → API → web payload.
- [ ] **M3 — Worker applies selection**: gate-boundary rebuild (agents map, Agent-guard hook, prompt roster names — `sdk-executor.ts`/`guardrails.ts`/`prompt.ts`/`steering.ts`), lead untrusted-review framing + MR-description marker, autopilot default + feed message; agent tests: repo agent gets only declared tools; **denylisted tool in repo frontmatter still denied**; excluded agent denied by the Agent-guard; prompt names match the resolved roster. Validation: e2e stub run flips source and the implement turn uses it.
- [ ] **M4 — Plan-gate UI**: PlanPanel per the mock (states A/B, chips, live approve label, read-only view after approval, plain-text rendering + inert-link vitest); typecheck green.
- [ ] **M5 — Visual parity vs mock**: web-ux browser pass comparing the implemented panel against `prds/mockups/37-agent-picker-mock.html` (both states, chip interactions, button labels, spacing/tones); findings fixed and re-verified. The PRD does not close with unresolved visual drift.
- [ ] **M6 — Docs + specs + e2e**: docs page updated (frontmatter rules per `docs/README.md`), specs/ai.md decisions recorded, specs/human.md updated with user approval, e2e scenario covering detect → choose → apply (incl. an autopilot run recording its roster).

## Milestone dependency / parallelization

| Phase | Milestones | Depends on | Files touched |
|---|---|---|---|
| 1 (parallel) | M1 (agent/), M4 UI shell against mocked API types (web/) | — | `agent/src/repoagents.ts`, `agent/src/protocol.ts` · `web/src/pages/RunView.tsx` |
| 2 | M2 (api/) | M1's wire shape (protocol.ts state + input body) | `api/internal/store/migrations/`, `workersvc/service.go`, `handler/workers.go` |
| 3 | M3 (agent/), M4 wiring (web/) | M2 | `agent/src/sdk-executor.ts`, `agents.ts`, `guardrails.ts`, `prompt.ts`, `steering.ts` · web api types |
| 4 | M5, M6 | M3+M4 | docs, specs, e2e |

Note: `sdk-executor.ts`/`steering.ts` churn in Phase 3 — check for conflicts with other in-flight PRDs before starting.

## Out of Scope

- Mixing sources in one run (Decision 4).
- A per-repo default setting in Repos settings (offered, not chosen — revisit if gate-time choice gets repetitive).
- Repo-defined lead/orchestrator, hooks, settings, or commands.
- Editing/curating repo agents from the uzi UI (they are the repo's files; edit them in the repo).
- Agent-container egress restriction (the structural close for the Decision 2 residual) — follow-up PRD candidate per docs/proc-hardening.md.

## Success Criteria

- A run on a repo with `.claude/agents/` shows the detected roster at the gate, defaults to it, and the implementation phase demonstrably uses those agents (feed shows repo-agent invocations; MR description carries the repo-agents marker).
- A run on a repo without them defaults to the user's templates with the repo card inert.
- Autopilot runs persist and display which roster they used, without human interaction.
- The shipped panel is visually confirmed against the approved mock (M5).
- No guardrail regression: `settingSources` still `[]`, deny-hooks still fire for repo agents, nested Agent spawning + WebFetch/WebSearch denied for repo agents, `disallowedTools` beats repo-declared `tools`.
