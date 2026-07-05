# PRD #17: Builtin Lead Template (opus) + Worker Model Selection from UI

**GitLab Issue**: [vtmocanu/uzi#17](https://gitlab.example.com/vtmocanu/uzi/-/issues/17)
**Status**: In Progress
**Priority**: High
**Created**: 2026-07-05
**Depends on**: PRD #3 (agent templates, done), PRD #4 (agent runtime/workers, done)

## Problem

The lead orchestrator — the main SDK thread that plans, delegates, and gates every run — is the only agent role with no template. Verified against the running stack (2026-07-05):

1. `agent_templates` holds only the seven builtin subagent roles; there is no `lead` row. The worker identifies a lead template by name convention (`agent/src/agents.ts:41` — `/^(lead|orchestrator)$/i`) and, finding none, leaves `baseOptions.model` unset (`agent/src/sdk-executor.ts:184`).
2. With no model set, the Claude Agent SDK falls back to the Claude Code default for the run owner's OAuth token — recent `run_messages` show the lead running on `claude-sonnet-5`. Nobody chose that; it's invisible and unconfigurable.
3. There is no way to pick a default worker model anywhere in the product. The lead's model is arguably the single highest-leverage quality knob in the system, and it's not a knob today.

## Solution Overview

1. **Ship `lead` as the eighth builtin agent template** with `model: opus` and a real orchestrator prompt body, living **only** in `api/internal/agenttmpl/builtins/lead.md`. As part of this PRD the builtin/dev-team coupling is dissolved: `builtins/` becomes the single source of truth for product templates, and `.claude/agents/` remains purely the repo's own dev agent-team. The boot reconciler (idempotent, edit-preserving) seeds the lead into existing databases on upgrade.
2. **Editable from the UI**: the lead becomes visible on the Agents page and editable via the existing admin template editor (model, prompt body; resettable to the builtin like the other seven). The editor's model field becomes a dropdown of curated aliases (`opus`, `sonnet`, `haiku`, inherit) plus a custom free-text option for full model IDs.
3. **Per-user default worker model**: each user can pick the SDK model for their runs (same dropdown + custom control) on the Settings page, next to their Anthropic token. When set, it overrides the lead template's model for that user's runs; null-model subagents inherit it too (SDK subagents without an explicit `model` follow the main thread).

**Model precedence (lead / main thread)**: run owner's default model setting → lead template `model` → SDK/account default. Subagents: their own template `model` → main-thread model.

## Design Decisions

1. **Decouple builtins from `.claude/agents/`; lead ships in builtins only** (user, 2026-07-05 — supersedes the earlier "full builtin, both dirs" choice made the same day). Rationale: the 1:1 convention was already broken (`web-ux` exists only in `.claude/agents/`); `.claude/agents/` is a live Claude Code subagent directory, so a product-template `lead.md` there would masquerade as a spawnable dev-team teammate; and the dual-home bought nothing at runtime — only the embedded copy ships. Consequences: `api/internal/agenttmpl/builtins/` is the single source of truth for product templates (versioned in git, `go:embed`-shipped, boot-seeded); the golden byte-match tests against `.claude/agents/*.md` are removed and replaced with parse/validity tests on the embedded files; `.claude/agents/` is free to drift as the dev team's roster; CLAUDE.md ("Builtin agent templates" bullet) and `docs/agent-templates.md` (which claims seven templates "mirroring this repo's own agent role library") are updated to the new convention.
2. **Lead prompt body augments, never replaces, the guardrail layer.** The worker builds the lead system prompt as claude_code preset + `prompt_body` + `LEAD_GUARDRAIL_APPEND` (`agent/src/prompt.ts:62-66`). The template body is persona/workflow guidance only; the primary-directive guardrails stay hardcoded in the worker and are appended regardless of what the template says. Editing the template cannot weaken guardrails.
3. **Default model is per-user, not global** (user, 2026-07-05). Runs are owned and claimed per user with per-user Anthropic tokens; the model default follows the same ownership. Stored as a nullable `default_model` column on `users` (no new table needed for one scalar).
4. **Dropdown + custom for model inputs** (user, 2026-07-05). Curated aliases (`opus`, `sonnet`, `haiku`, `fable` — preserving the full `MODEL_ALIASES` set the template editor's datalist already ships, `AgentTemplateEditor.tsx:13`) plus an "other…" free-text input accepting any model ID. Validation: non-empty, trimmed, sane length, no whitespace; typos in custom IDs surface as run-time SDK errors (accepted — the API cannot enumerate valid IDs without calling Anthropic). Note the current editor is already a datalist-backed input, not bare free text; the change is promoting it to a shared `ModelSelect` control with an explicit inherit option, reused by Settings.
5. **Plumbing rides the claim payload.** The run owner's `default_model` is added to `ClaimConfig` (`api/internal/workersvc/claim.go`) alongside the existing caps; the worker applies `baseOptions.model = config.default_model ?? leadModel` — no new endpoints on the worker path, no worker-side settings fetch.
6. **User default wins over the lead template model** (review finding + user, 2026-07-05). With template-wins precedence the per-user setting would be inert on a default install (the builtin lead always pins opus), only activating after an admin globally cleared the lead model. Flipping precedence keeps both features live: the lead template's `opus` is the instance-wide default — every user with the setting unset (NULL) gets opus out of the box — and a user who picks a model overrides it for their own runs only. Subagent templates with explicit models are unaffected.

## Technical Design

### 1. Builtin lead template (api + .claude)

- **Decouple first**: remove the **two** golden byte-match tests pinning builtins to `.claude/agents/*.md` (`TestRenderBuiltinsByteMatch`, `TestEmbeddedCopiesMatchRepo` in `api/internal/agenttmpl/render_test.go`); replace with parse/validity tests over the embedded files (frontmatter parses, name/description non-empty, name unique, model alias sane). Update `TestBuiltinsSetIsExactlySeven` and its hardcoded `builtinNames` slice (render_test.go:11-14) for the eighth template; keep `TestCoderInheritsAllTools` as is. `.claude/agents/` is no longer touched by product changes. Update CLAUDE.md ("Builtin agent templates" convention bullet) and `docs/agent-templates.md` (seven-template table + "mirroring this repo's roster" claim).
- `api/internal/agenttmpl/builtins/lead.md` (single home): frontmatter `name: lead`, `description`, `model: opus`, **no `tools:` line** (the lead needs the full toolset — same null-tools-inherit-all contract the `coder` template uses, `agent/src/agents.ts` header). Body: orchestrator persona — plan-first workflow, delegate to the seven roles by name, respect the approval gate and `signal_done`, never touch `main`.
- No seeding code changes expected: the boot reconciler inserts missing builtins and preserves edited rows, so upgraded instances get the lead row automatically; `ResetAgentTemplate` works for it like any builtin. **Upgrade collision**: seeding is `ON CONFLICT (name) DO NOTHING`, so an instance where an admin already created a custom template named `lead`/`orchestrator` keeps its custom row (`is_builtin=false`, not resettable) and never gets the builtin — degrades gracefully (worker routes it the same); log a boot-time warning when a builtin insert is skipped by a non-builtin row of the same name, and document it.
- Also sweep the stale sync-convention comments when decoupling: `builtins.go:12-16,:70`, `render.go:5,:23` reference `.claude/agents/` mirroring.
- Lead prompt body stays **role-agnostic**: do not hard-list the seven role names — the invokable subagent set is dynamic and already injected per turn via `delegatesLine` (`agent/src/prompt.ts:152`).
- Worker: no changes needed for pickup (`assembleAgents` already partitions by `LEAD_NAME_RE` and routes `prompt_body`/`model` to the main thread instead of registering a subagent). Verify with a unit test that the shipped builtin name matches the regex.

### 2. Model select UI (web)

- New shared `ModelSelect` component: radio/select over `inherit` (null), `opus`, `sonnet`, `haiku`, `fable`, `custom` (reveals text input). Used by `AgentTemplateEditor` (replacing the current datalist-backed input) and by the new Settings section.
- `ModelSelect` init: an existing value not in the alias list (e.g. `claude-fable-5` or any custom ID) must initialize into the `custom` state with the text prefilled — never silently reset to `inherit`.
- Submit gating preserved: the custom value keeps flowing through `frontmatterFieldWarning` so an injection-suspect model string still disables submit (`AgentTemplateEditor.tsx:227` behavior).
- Validation unified in one place, server + client: the template model validator (`agent_templates.go:362-370`) currently allows interior spaces and has no length cap — tighten it to the Decision 4 rules and reuse the same rules for the user default-model endpoint, so the two surfaces can't drift.
- Agents page: lead renders like other builtins (edit + reset). Optionally badge it as "lead / orchestrator" so it reads differently from invokable subagents.

### 3. Per-user default model (api + web)

- Migration (drafted as `00022`; **final number assigned at merge time** — next free above the live head, per the CLAUDE.md convention: PRD #24 landed `00029` after this draft, and strict goose refuses to boot on below-head versions), with a `+goose Down`: `ALTER TABLE users ADD COLUMN default_model text` (nullable; NULL = inherit SDK default). sqlc queries: get/update for the current user (note: `SELECT *`/`RETURNING *` queries on `users` regenerate the sqlc `User` struct — harmless, expected).
- API: extend the existing current-user/settings surface with `GET`/`PUT` for the default model (session-authenticated, own-user only; no admin involvement). Validation server-side per Decision 4.
- Web: "Worker model" section on `Settings.tsx` under the Anthropic token block, using `ModelSelect`; explains precedence (lead template overrides this; empty = account default).

### 4. Claim plumbing (api + agent)

- `GetRunClaimContext` (or companion query) additionally selects the run owner's `default_model` (JOIN `users`); `ClaimConfig` gains `default_model?: string` (omitted when NULL).
- Agent: `protocol.ts` ClaimConfig type gains `default_model`; the runner's ClaimConfig→RunContext.config mapping must carry it through (one extra hop the worker side needs); `sdk-executor.ts` resolves `ctx.config?.default_model ?? assembled.leadModel` and applies it **set-only-when-defined** (as line 184 does today — `model: undefined` must remain "omit", never an explicit key).

## Milestones

- [ ] **M1 — Decouple + builtin lead template lands**: both golden byte-match tests removed in favor of parse/validity tests, `TestBuiltinsSetIsExactlySeven`/`builtinNames` updated; CLAUDE.md + `docs/agent-templates.md` updated to the builtins-only convention; `lead.md` added to `api/internal/agenttmpl/builtins/` with `model: opus`; boot reconciler seeds it on a fresh and an upgraded DB (collision warning logged when a custom same-name row blocks it); worker pickup proven at unit level (`assembleAgents` routes the shipped lead's model to `leadModel`; live `run_messages` verification is M7's manual step).
- [ ] **M2 — Lead editable from UI**: Agents page lists lead; admin can edit model/prompt and reset to builtin; `ModelSelect` (dropdown + custom) replaces the free-text model field in the template editor.
- [ ] **M3 — Per-user default worker model**: migration + API get/put + Settings UI section working end to end; validation rejects junk input.
- [ ] **M4 — Claim plumbing + precedence enforced**: `default_model` flows through ClaimConfig; worker applies user-default → lead-template-model → SDK-default precedence; covered by Go (workersvc) and agent (`node --test`) unit tests.
- [ ] **M5 — Tests green across packages**: `go test ./...`, `npm test` (web), `npm test` (agent) all pass with the new coverage.
- [ ] **M6 — Docs + specs updated**: `docs/agent-templates.md` covers the lead template and model precedence; `docs/configuration.md`/Settings docs cover the default model; `specs/ai.md` records the design decisions (specs/human.md untouched without approval).
- [ ] **M7 — Validated**: automated: e2e (`./e2e/run-e2e.sh`) passes and unit tests assert the resolved `baseOptions.model` for each precedence case (the e2e stack runs a stub executor with dummy creds, so `run_messages` carry no real model — model resolution is proven at unit level). Manual (user-assisted, needs a real Anthropic token): one live run confirming the lead reports an `opus`-family model in `run_messages` and honors a changed user default.

## Success Criteria

- Fresh install: first run's lead executes on opus with zero configuration.
- Upgraded install: lead row appears after API restart; existing template edits untouched.
- An admin changes the lead model in the UI → next run of any user without a personal default uses it, no restart.
- A user sets a default model → their runs (lead + null-model subagents) use it, overriding the lead template's model; other users unaffected. Clearing it back to inherit restores the lead template's model (opus by default).
- No guardrail regression: lead prompt edits cannot remove `LEAD_GUARDRAIL_APPEND` or the worker deny-hooks (asserted by existing + new tests).

## Risks

- **Model alias drift**: curated aliases go stale as Anthropic ships models — mitigated by the custom free-text escape hatch and keeping the alias list in one shared constant.
- **Invalid custom model IDs** fail only at run time — mitigated by surfacing the SDK error in run messages (already the failure path for bad tokens).
- **Convention change churn**: dropping the `.claude/agents/` ↔ builtins sync touches CLAUDE.md, dev docs, and tests in the same PRD as the feature — mitigated by doing it first (M1) as a small, self-contained commit. `.claude/agents/` drift after the decouple is intended, not a regression.
