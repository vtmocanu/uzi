# PRD #3258: Per-harness reasoning effort on agent templates

**Issue:** #3258  
**Sibling issues (children of #1703):** A #3255, B #3247, B2 #3251, C #3245, D #3248, E #3257, F #3254, G #3249, H #3244, I #3263, J1 #3256, J2 #3260, K #3261, L #3262, M #3259, N #3258, O #3264, P #3250, Q-spike #3246, Q-claude #3252, Q-codex #3253.  
**Status:** Implemented 2026-11-21 (issue #3258, M1-M4 delivered on the run branch). Drafted as r2 2026-11-21, buddy-approved (child N of #1703), against main `f99e8a84d9111dea0f0431485295d2411ec5e971`.
**Priority:** Normal.  
**Parent:** #1703.  
**Blocked by:** none for implementation. G (#3249) is implemented on main by PR #3884 (`codex_model` column, per-family pins, harness-filtered claim projection); only its hosted acceptance is open, which is not a code dependency. O's agent-template CLI (#3264, PR in `prds/done/3264-cli-model-settings.md`) and P's run-start model and effort (#3250, `prds/done/3250-run-start-model-effort.md`) are on main.  
**Related:** child L (#3262, Agents UI presentation of pins and effort, blocked by N), Q-claude (#3252, shipped in #4571) and Q-codex (#3253) (planning/implementation phase pair), child F (#3254, effective-settings resolver), #3265 (repo `.codex/agents/*.toml` model as a Codex pin, done). The main integration preserves Claude phase-specific effort and template pins.
**Delivery:** one PRD, one uzi run, one PR, four serial milestones (M1 to M4). Use the agent in-run/CI boundary in AGENTS.md *Run economy*. The run needs Docker for the named LiveDB tests; unavailable or skipped execution blocks milestone completion. Neither implementation nor validation touches `.github/workflows/**`. Choose a new migration number above the implementation base's head (`00497` at drafting); run `task migration:renumber` on the landing rebase.  
**Mock:** `prds/mockups/1703-model-config/`, section 3 (agent editor) is the presentation reference for the effort select beside each model select. Child L owns the full redesign; this PRD ships a usable interim control.

**Scope gate:** one independently valuable outcome: a subagent template can carry a reasoning effort per harness that actually reaches that subagent's model calls, and an empty effort follows the run. Storage, API, CLI, claim, both runtimes and repo-agent parsing are one PR because a pin that is accepted on one side and dropped on the other is exactly the failure the issue forbids. No milestone waits for unfinished sibling code.

## Open questions for the user

None. The user approved dispatch on 2026-11-21. Decisions N1 to N9 were drafted by the lead and revised after the buddy's review (see Decision Log).

## Problem

Templates have a model pin per harness (G) but no effort. Every subagent runs at the lead's effort:

- Claude: `toDefinition` in `agent/src/agents.ts` (~162-223) maps `model` onto the SDK `AgentDefinition` and never sets `effort`, although the pinned SDK (`@anthropic-ai/claude-agent-sdk` 0.3.319, `agent/package.json`) declares `AgentDefinition.effort?: 'low'|'medium'|'high'|'xhigh'|'max' | number`.
- Codex: `HarnessAgent` (`agent/src/harness.ts` ~354-363) has no effort; `toHarnessAgent` (`agent/src/codex/codex-executor.ts` ~629-641) maps none; `buildRender` (`agent/src/codex/render.ts` ~496-502) hard-codes `modelReasoningEffort: requestEffort` for every role. The downstream delegation seam already carries a per-role effort to the child turn (`codex/run-builder.ts` ~85-92 → `DelegationRole.effort`, `codex/delegation.ts` ~78-98 and ~320-327, `codex-executor.ts` ~7062 `turnParams.effort = spec.effort`).

## Verified baseline (main `f99e8a84d`)

- Storage: `agent_templates.model` (Claude pin) and `codex_model` (migration `00401`). Queries in `api/internal/store/queries/agent_templates.sql` (create, update, builtin insert, refresh-pristine with the `IS DISTINCT FROM (description, model, codex_model, tools, prompt_body)` guard, reset, synced insert, synced update).
- Effort vocabulary: `agenttmpl.EffortLevels` (`api/internal/agenttmpl/effort.go:16`), `ValidateEffort`, `ValidateFamilyEffort` (`cross_check_pins.go:66`); agent `HarnessEffort` (`agent/src/harness.ts:55`), `CONTRACT_EFFORTS` / `resolveEffort` (`codex/render.ts` ~141, ~348-358, bounded `unknown_effort` diagnostic). `runtime` is a preference value only, never a stored pin.
- Run effort: `leadEffortFor` / `resolveLeadEffort` in `api/internal/workersvc/lead_settings.go`: P run override, then the user's per-harness default, then built-in; `runtime` resolves to no effort sent. The worker reads `config.default_effort` in `resolveLeadRequest` (`agent/src/lead-config-report.ts`).
- Claim: `ClaimAgent` (`api/internal/workersvc/claim.go` ~650) carries one `model` per agent, harness-filtered by `agentsFromTemplates` via `agenttmpl.PinForHarness` (~852-885).
- API: `/api/me/agent-templates` and `PUT /api/me/agent-templates/{id}/model-pins` are in the `RequireUser` group (`routes_me.go` ~311-312); the template CRUD under `/agent-templates` is cookie-only `requireAuth` (`routes_agents.go`). Tri-state optional pins: `optionalTemplatePin`, `mergeOmittedTemplatePins` (`agent_templates_pins.go`).
- CLI: `api/cmd/uzi/agent_template.go` `set <name|id> --harness claude|codex <model>` / `reset`, `agentTemplateHarnessFields`.
- Builtins: `api/internal/agenttmpl/builtins.go` `parse` (~102-126) rejects unknown frontmatter keys; agent-source `api/internal/agentsource/parser.go` ignores unknown keys. Drift contract: Go `SameContent` (`agenttmpl/compare.go`), web mock `sameContent` (`web/src/mocks/mockApi/agents.ts`), `driftedColumns` (`web/src/lib/agentTemplates.ts`), fixture `fixtures/agent-template-drift/cases.json`.
- Repo agents: `agent/src/repoagents.ts` `parseAgentFile` reads name/description/model/tools and ignores `effort:`; `agent/src/repoagents-codex.ts` explicitly ignores `.toml` `model_reasoning_effort` (`docs/repo-agents.md` ~213-215).
- Subagent model override (schedule "Apply model also to agents"): `applySubagentModelOverride` (`agents.ts` ~344) and `override_subagent_model` (Codex, `codex-executor.ts` ~7202) touch models only.
- P D13 (`prds/done/3250-run-start-model-effort.md` ~17, ~424): explicit subagent pins win over P's run choice; unpinned subagents inherit.

## Outcome

1. Each template has a Claude effort and a Codex effort, each `NULL` (Default · follows the run's effort) or one of the five closed levels.
2. On a run, a subagent with a pin for the run's harness uses that effort for its own model calls; a subagent without one uses the run's resolved lead effort for the current phase; when that is "Runtime default", nothing is sent for it either.
3. No accepted pin is silently discarded: a worker that cannot apply template effort makes that visible in the run feed (N6).
4. Own templates, builtins (admin edits, pristine refresh, reset), agent-source sync, repo `.claude/agents/*.md` and `.codex/agents/*.toml`, the CLI and an interim editor all carry it.

## Out of scope

Cross-harness execution; changing P/Q lead precedence; schedule effort pins; new effort levels (e.g. Codex `ultra`) or SDK numeric efforts; L's presentation redesign; changes to native Codex subagent execution (`spawn_agent`).

## Decisions

- **N1 Storage.** Two nullable columns on `agent_templates`: `effort` (Claude) and `codex_effort` (Codex), with a `CHECK` that each is NULL or one of the five levels. Every query above carries both, including the pristine-refresh guard tuple; `sqlc generate`. Down drops both columns (the only data lost is the new pins).
- **N2 Precedence for subagents.** For the run's harness: the template's family effort if set, else the resolved lead effort of the phase the subagent runs in (N3's lead chain, or Q's phase pair once it lands), else nothing. The pin wins over P and Q (D13). The worker computes the fallback from the claim's already-resolved lead request; it never re-reads user defaults per child.
- **N3 Lead-template effort sits below the user default, mirroring the model chain.** The lead's effort becomes: P run override → user per-harness default → the lead template's family effort (G's one lead-template rule) → built-in. It applies only when the user has no default for that harness, exactly as the lead template's model pin sits below the user lane (`specs/human.md` ~204). Q's resolved phase pair, once landed, stays above it. A user preference of `runtime` is an explicit choice and wins over the template (nothing is sent); the template applies only when the stored preference is absent, and a corrupt stored preference keeps today's fallback. Do not treat "no effort on the wire" as "no preference". The source reuses the existing token `template` (`api/internal/leadcfg/leadcfg.go` `EffortSourceTemplate`, `EffortSources`; agent `protocol.ts` ~1226; migration `00388`), so no source vocabulary or schema change. The tier applies to the lead only: non-lead consumers (judge, jobs, checkers, advice, and the non-lead override path in `claim_assembly.go` ~539) keep their existing effort policies; adding the template argument to `leadEffortFor` must not reach them. **Effective-settings parity:** F's effective-settings view, the run-start preview, the CLI and the web mock read the same resolved lead effort (not a second resolver): today `api/internal/handler/effective_settings.go` ~275-282 builds the worker effort cell from the user preference only via `effortSettingCell` (`model_setting_cells.go` ~93-94); extend that seam so a NULL user preference with a template effort shows the template value and source. The editor's help on lead-shaped templates says "Used when you have no default effort for this harness".
- **N4 Claim.** `ClaimAgent` gains `effort *string`, projected for the run's harness exactly like `model` (a new `agenttmpl.EffortForHarness(harness, claudeEffort, codexEffort)` beside `PinForHarness`). The other family's value never reaches the worker.
- **N5 Runtimes.** Claude: `toDefinition` sets `def.effort` to the pin, or to the phase's resolved lead effort when unpinned, omitted when that is Runtime default; plan/implement roster rebuilds (`planTurnSubagents` clone, implement agents) preserve it. Codex: `HarnessAgent.effort`, mapped in `toHarnessAgent`; `buildRender` uses the role's pin, else `requestEffort`; a pin outside the contract goes through the existing `resolveEffort` bounded diagnostic and is dropped with a run-feed notice, never silently. The Codex catalog advisory (`effort_not_in_catalog`, `catalogProbeFor` ~5638-5653, wired only for the root at ~6007) stays lead-only: a role pin is sent unchanged with no per-child catalog probe, no network wait and no downgrade or retry; the provider's own rejection path applies. Reason: a per-child probe adds latency and notice volume for an advisory, and the lead-only scope is documented in `docs/worker-effort.md`.
- **N6 Rollout.** New protocol capability `template_effort_v1` (`api/internal/capability`), self-reported by workers that implement N5. The api sends `ClaimAgent.effort` only to workers advertising it. When it withholds at least one non-null pin for the claiming run, it records one run-feed status line per claimed generation ("stored subagent effort pins omitted: this worker predates template effort; those subagents use the run's effort") so the omission is visible; it never claims a pin was applied. The N3 lead tier needs no capability: it travels in the existing `config.default_effort`, which older workers already honor. No placement gate: an older worker still runs the work at today's behavior, and the fleet catches up on its next roll.
- **N7 API and CLI.** The template write request and DTO gain tri-state `effort` and `codex_effort` (omitted keeps, `null` clears), validated with `agenttmpl.ValidateEffort`. The existing `PUT /api/me/agent-templates/{id}/model-pins` (RequireUser, owner-only `scope='user'`) accepts the two keys in its allowlist; the route name stays for compatibility. CLI: `uzi agent-template set <name|id> --harness claude|codex [<model>] [--effort <level>]` needs a model, `--effort`, or both; `reset <name|id> --harness … [--effort]` keeps clearing the model when `--effort` is absent (unchanged behavior) and clears only the effort with it. `list` gains `CLAUDE EFFORT` and `CODEX EFFORT` columns; `show` prints both; `--json` passes the envelope. Update `api/internal/uzicli/skill/SKILL.md`.
- **N8 Builtins and agent-source.** Builtin `.md` files are upstream copies, so builtin effort comes only from admin edits (`customized = true`); the `builtins.go` parser accepts an optional `effort:` key (Claude family, validated, unknown value is a parse error) so a later upstream release can ship one. `SameContent` and both web drift readers add the two columns; the drift fixture gains optional `effort` / `codex_effort` case keys (absent = null). Reset returns both to the shipped values. The agent-source parser accepts the same optional `effort:` key (Claude family), skipping an invalid value with the existing bounded safe-name diagnostic; preview, apply and the admin card show it. Extend the existing shared Go/TS parser differential golden (`api/internal/agentsource/parser.go` header twin contract, `agent/test/agentsource-parity.test.ts`) with valid and invalid `effort:` cases, and carry `effort` through the canonical render/source preview so a definition does not lose it on a render/parse round trip.
- **N9 Repo agents.** `.claude/agents/*.md` `effort:` becomes the Claude-family effort of that agent; `.codex/agents/*.toml` `model_reasoning_effort` becomes its Codex-family effort. Each is validated against the five levels; an invalid value is dropped with a bounded run-feed notice naming the file, the agent stays. Projection follows the harness like `projectModelForHarness`. `docs/repo-agents.md` replaces the "ignored" sentence.

## Testing decisions

- **Regressions, each watched red on current code and recorded in the PR:**
  - (a) Claude, request capture through the prepare-options path (`agent/test/agents.test.ts` or the SDK-options capture used by `sdk-executor` tests): run effort `medium`, subagent A pinned `high`, subagent B unpinned → A's `AgentDefinition.effort` is `high`, B's is `medium`. Red today: no `effort` on any definition.
  - (b) Codex, `agent/test/codex-render.test.ts`: roles pinned `high` and `low` against root `medium` render distinct `modelReasoningEffort`; an unpinned role inherits root. Red today: the "effort is request-scoped" assertion (~436) hard-codes root for every role (update that assertion).
  - (c) Codex production child RPC capture (`agent/test/codex-executor.test.ts`): the child `turn/start` effort differs from the root for a pinned role and is omitted for an unpinned role when the run is Runtime default. Kills a mutation at `codex-executor.ts` ~7062.
  - (e) Lead effort (`lead_settings_test.go` plus the `request_cases` fixture H uses): user default unset, lead template Codex effort `high` → a Codex run's `default_effort` is `high` with source `template`; with a user default `low` set, `low` wins; a user preference `runtime` sends nothing and beats the template; a P run override beats all; a judge/checker/advice claim for the same user is unchanged (lane control). The effective-settings handler and run-start preview report the same value and source for each case. Red today: no template tier.
  - (d) Claim (`claim_test.go` or the LiveDB claim test): a Codex run never receives a Claude-family effort and vice versa; a worker without `template_effort_v1` receives no `effort` and the run feed gets exactly one notice.
- Also cover: Runtime default plus one explicit pin; plan/implement phases with different lead efforts (unpinned child follows each phase; Q fixtures when Q has landed, otherwise P's single effort); schedule "Apply model also to agents" leaves effort intact; hostile/invalid values at API (400), claim projection and repo files (bounded diagnostic, no accepted pin dropped silently); `codex-run-builder.test.ts` (~148) and `codex-delegation.test.ts` (~113) with distinct role values.
- **Migration LiveDB:** Up adds both nullable columns with the CHECK (an out-of-set write fails); existing rows unchanged incl. `customized`, `origin`, `updated_at`; Down drops them.
- **Boot reconcile:** extend `agent_templates_origin_livedb_test.go`: setting either effort on a builtin customizes it and blocks refresh; Reset returns it to pristine.
- **Drift contract:** new cases for effort differs, NULL equals NULL, a value moved between the two columns differs; green in Go and vitest.
- **API contract:** re-record `fixtures/api-contract/agent_template.full.json` / `.zero.json`; `apiContract.test.ts` covers the web type.
- **CLI:** `agent_template_test.go`: `set --effort` without a model, model plus effort, `reset --effort`, `reset` without `--effort` still clears only the model, invalid level is exit 2 before any request.
- **Web:** `AgentDetail.test.tsx` / `AgentNew.test.tsx` drive both effort selects (Default label, five levels, lead-shaped help, submit payload); mock data carries one pinned template. Follow `.claude/rules/web.md`.
- **Agent-source:** `computeDiff` and `agent_source_apply_livedb_test.go`: a staged `effort: high` previews, applies to `effort`, and re-previews unchanged; an invalid value warns and stages NULL.
- **E2E contract sweep:** `git grep -F` template payload literals across `e2e/phases/`, `e2e/lib.sh`, `e2e/lib.test.sh`, `e2e/forge-fake/`; update expectations; run `task test:e2e-lib` and `task test:forge-fake`; name compose phases left unrun.

### Landing obligations (not worker milestones)

- **full agent suite: exact-head PR CI**.
- The worker executes the named LiveDB migration, template, reconcile and agent-source tests with positive PASS evidence and no relevant skips. On the landing rebase, renumber the migration, regenerate sqlc, and repeat the affected exact-head database checks.
- Full `test:api`, `test:web`, `test:web-browser`, `gate:repo`, `scan:secrets` and `sast:semgrep` stay pending exact-head PR CI, per *Run economy*.

## Milestones

### M1. Storage, API, CLI and claim projection

- N1 migration and queries; N3 lead-template tier in `lead_settings.go` plus effective-settings/start-preview/CLI parity; N4 projection helper and `ClaimAgent.effort`; N6 capability constant and the api-side withholding plus run-feed notice; N7 API and CLI; drift contract (Go side) and API contract fixture; web `AgentTemplate` type.
- Docs: `docs/agent-templates.md` (the two effort fields, Default meaning, the lead-template tier (N3), builtin customization), `docs/cli.md`, `docs/worker-effort.md` (subagent effort precedence, N2), `ARCHITECTURE.md` "Agent templates"; `task docs:sync`.
- Acceptance: regressions (d) and (e); migration and reconcile LiveDB green; CLI tests green.
- Delivered: migration 00503 (renumbered at landing), the nine template statements plus the lead lateral, `EffortForHarness`, `template_effort_v1` with the withheld-pin `status` notice (`template_effort_omitted`), the N3 tier with effective-settings parity (start preview through the effective cell), cookie and CLI pin routes, `uzi agent-template set --effort` / `reset --effort`, drift and label contracts, docs.

### M2. Both runtimes honor template effort

- N5 Claude and Codex; worker self-reports `template_effort_v1`; repo-agent parsing (N9).
- Docs: `docs/repo-agents.md`.
- Acceptance: regressions (a), (b), (c) watched red/green; `UZI_DEADCODE_AGENT_REQUIRED=1 task check:agent`, touched test files with `--test-timeout=120000`, and `task test:codex-m4`; full agent suite pending CI.
- Delivered: Claude `toDefinition` pin plus `fillSubagentEffort` on the plan and implement rosters; Codex per-role effort through `renderCodexRun` with a once-per-role `unknown_effort` notice; `.codex/agents` TOML `model_reasoning_effort` with `effort_ignored` notes; family-by-folder projection; the capability advertised unconditionally. The Markdown half of N9 moved to M3 (it shares the agent-source parser golden).

### M3. Builtins and agent-source

- N8 parser key, pristine refresh, reset, agent-source preview/apply and the admin card.
- Docs: `docs/agent-source.md`.
- Acceptance: agent-source and boot-reconcile tests green.
- Delivered: builtin `effort:` (parse error when invalid) and `Render` emitting it; agent-source and `.claude/agents` Markdown `effort:` (Claude family) with `effort_ignored`; staged snapshots, preview, diff, apply and the admin card carry it; the shared parity golden gains valid, invalid and block-scalar cases; `docs/agent-source.md`, `docs/repo-agents.md`.

### M4. Interim editor, list and detail

- An effort select beside each model select in `web/src/components/AgentTemplateEditor.tsx`, the lead-shaped help, list/detail cells, mock data (including the mock effective-settings response for N3), web drift reader `driftedColumns` and mock `sameContent`.
- `specs/human.md` under the template-pin and precedence entries (~81-82, ~204-205, ~2000-2001): terse entries tagged `(AI-synced YYYY-MM-DD)`.
- Changelog: one new `changelog.d/<uuid v4>.md` fragment (category Added) citing #3258.
- Acceptance: web tests green; `task check:web`; mock mode shows a template with a Codex effort pin.
- Delivered: both effort selects with the lead help, list/detail labels, the shipped-vs-stored diff, mock data (`migration-writer` pinned Codex `high`) and a mock effective-settings transform that serves a delivered lead template's effort as source `template`. The focused browser case is committed but pending exact-head CI (owner decision: no browser Vitest in-run). `specs/human.md` and the changelog fragment were landed as integration edits.

## Risks

- **Older workers** silently ran pins before N6; N6's notice is the guard. A hosted fleet pinned to an older worker image shows the notice on every run until the next roll.
- **Q interaction.** If Q-claude lands first, N2's fallback must read the phase pair from Q's resolved lead request rather than a single effort; the regression for phases covers it. If N lands first, Q must keep N's per-agent override (note this on #3252/#3253).
- **SDK inheritance** for an unpinned Claude subagent is not runtime-proven, so N5 sets it explicitly instead of relying on the SDK.
- **Real provider acceptance** of per-subagent effort is not exercised by offline request capture; record it as unverified in the PR.

## Decision Log

- 2026-11-22 (integration): `/me/settings` Planning "Same as the run" still derives its displayed default from the user's preference, like the ordinary preference cell; claims and effective previews include the template tier. The preference-cell UI correction is deferred to [#3262](https://github.com/vtmocanu/uzi/issues/3262#issuecomment-7070339319).

- 2026-11-21: Drafted by the lead (Claude session) with the Codex buddy. Runtime research (buddy, origin/main `f99e8a84`): the SDK 0.3.319 type contract was verified from the exact npm tarball; the Codex delegation seam already carries per-role effort. Storage/API/CLI research (subagent) found the #3249 PRD's "no agent-template CLI" baseline stale (#3264 shipped it).
- N3 mirrors the model chain: the lead template applies only when the user has no per-harness default.
- r2 (buddy REVISE 3 on r1, digest `9f4350ef`): N3 reuses the existing `template` source token instead of a new one, preserves an explicit `runtime` preference, keeps non-lead lanes unchanged, and adds effective-settings/preview parity; N5 keeps the Codex catalog advisory lead-only (no per-child probe); N8/N9 extend the shared parser differential golden and the render round trip.
- N6 chosen over a placement gate: the notice makes the omission visible with no new scheduler predicate; the work still runs at today's behavior.
- 2026-11-21 (implementation, issue #3258 run): migration `00503_agent_template_effort.sql` (renumbered above main at landing). The Markdown half of N9 moved from M2 to M3 so the `.claude/agents` parser and the agent-source parser change together under the shared parity golden. Invalid repo or source effort uses one new note token, `effort_ignored`, shared by Go and TypeScript; on repo agents it carries the file basename only when that basename passes the agent-name regex and length cap, and the value is never echoed. The N6 withheld-pin notice and the Codex out-of-contract notice are `status` run messages. Start-preview parity needs no API change: a run-start effort is `run_override`, and otherwise the web preview composes the effective-settings cell, which now carries the template tier. Deferred: the Claude lead model can still pass its own `effort` on an Agent call, and an out-of-contract Claude pin is dropped without a notice (the api CHECK makes it unreachable).
- 2026-11-22 (#3253 / PR #4599 integration after #3262): the deferred Planning default correction above is complete for both harnesses. `/me/settings` now composes the same template-aware lead effort and planning override as claims and effective previews, including the Codex planning fields.
