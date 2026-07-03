# PRD #3: Agent Definitions, Templates & Anthropic Token Storage

**GitLab Issue**: [vtmocanu/uzi#3](https://gitlab.example.com/vtmocanu/uzi/-/issues/3)
**Status**: Draft
**Priority**: High
**Created**: 2026-07-03
**Depends on**: PRD #1 (auth shell, done); PRD #2 M1 only (secretbox utility + `UZI_SECRET_KEY` boot guard — see Coordination)

## Problem

uzi will soon have a forge-synced kanban of PRD issues (PRD #2), but no notion of the agents that will do the work. plan.md requires: agents defined/edited from the UI, agent templates stored in DB and editable via UI (see `/dot-ai-agent-team` for how agents are created/updated), and a per-user Anthropic OAuth token — with a doc on how to obtain one — stored encrypted in the DB via the webui. None of that exists. This PRD delivers the *data and management* layer only; spawning/running agents (client containers, server↔agent connection, status dashboard) is the next PRD.

## Solution Overview

Two independent halves, both pure DB+API+UI on the PRD #1 shell:

1. **Agent templates in DB, admin-edited, user-visible.** Template shape follows the dot-ai-agent-team role library (`name`, `description`, `tools` allowlist, `model`, `prompt_body`), seeded from this repo's own agent set (`.claude/agents/`: coder, reviewer, auditor, tester, documenter, fact-checker, spec-keeper). Templates render deterministically to Claude Code subagent files (`.claude/agents/{name}.md` frontmatter format) — the export format the PRD #4 runtime will consume; this PRD ships the renderer and a read-only preview.
2. **Per-user Anthropic token, encrypted at rest.** Each user pastes their Anthropic OAuth token (obtained via `claude setup-token`, documented) in Settings; uzi stores it AES-256-GCM-encrypted with the same secretbox/`UZI_SECRET_KEY` machinery PRD #2 M1 builds, in a **generic `user_secrets` table** (kind-keyed, so future per-user secrets need no new schema). Never returned to the client after save; re-paste to rotate.

All three inspirations were audited for both concerns (2026-07-03, submodule audit — see decision log):

| Concern | multica does | bottega does | dot-agent-deck does | uzi will do |
|---|---|---|---|---|
| Agent definition storage | Per-workspace `agent` DB rows, fully UI-editable (instructions, model, env, MCP config, custom args) | Prompts as file-based Markdown templates (UI-editable, but **instance-wide overrides: any user rewrites everyone's agents** — weakness); model/provider per-user DB JSON; tools/permissions hardcoded | Per-project TOML (`OrchestrationRoleConfig`: name, command, prompt_template) + embedded `roles.toml` library; file-only, no UI, no DB | **DB `agent_templates` table, admin-write / all-users-read** (closes bottega's any-user-rewrites hole), seeded from a default role library, full shape (prompt + tools + model) editable — not just the prompt |
| Definition ↔ runtime format | Flags assembled ad hoc per runtime (`--append-system-prompt`, temp MCP file); no standard subagent format | Claude Agent SDK `query()` calls; no `.claude/agents/*.md` | Launcher strings + hook installs into `~/.claude/settings.json`; no subagent files | **Canonical render to Claude Code subagent Markdown** (frontmatter `name`/`description`/`tools`/`model` + body), same format as this repo's own `.claude/agents/`; preview in UI, consumed by PRD #4 |
| AI provider credentials | Per-**agent** `custom_env` JSONB, **plaintext in Postgres** (their secretbox exists but only wraps Lark/Slack secrets — weakness); removed from normal API responses entirely, dedicated reveal endpoint audit-logged | Per-user OAuth token **files, plaintext**, chmod 0600 only (weakness); nice in-app device-flow UX; strips global `ANTHROPIC_*` env so the per-user token wins | None — inherits the daemon's env (global key, plaintext, leaks into every spawned agent — weakness) | **Per-user, AES-256-GCM encrypted in DB** (multica's secretbox pattern *applied to provider creds*, which multica itself failed to do), never echoed back, kind-keyed generic table |
| Secret entry UX | Env-var editor per agent | In-app OAuth device flow (best UX; more moving parts) | None | Paste-token form + `claude setup-token` doc (MVP); bottega-style in-app device flow noted as a later upgrade |

Patterns copied: multica's secretbox (AES-256-GCM, via PRD #2's shared util) and its redact-by-default API responses; dot-agent-deck's embedded default role library idea; bottega's per-user credential isolation. Weaknesses avoided: plaintext provider creds (all three), instance-wide user-writable prompt overrides (bottega), per-agent-instead-of-per-user creds (multica).

## Technical Design

### Coordination with PRD #2 (shared secretbox)

PRD #2 M1 builds `api/internal/secretbox/` (AES-256-GCM Seal/Open, `LoadKey` from base64 32-byte `UZI_SECRET_KEY`, refuse-to-start guard) explicitly earmarked for this PRD. Whichever PRD lands M1 first ships the util in a **standalone commit touching only `api/internal/secretbox/` + config wiring**, so the other rebases cleanly.

Two further parallel-safety rules (post-review):

- **Goose migration version ranges are reserved**: PRD #2 uses `00002`–`00009`, PRD #3 starts at `00010`. Duplicate goose versions from parallel branches merge without git conflict but fail at `goose up` — reserving ranges prevents the silent-until-runtime break. (Mirrored in PRD #2.)
- **Shared frontend shell files** (sidebar nav, Settings layout, route table) are touched by both PRDs. These are small, expected merge conflicts — keep those edits in dedicated commits and rebase; everything else (tables, handlers, pages) is disjoint.

### Schema (goose migrations, extends PRD #1 DB)

```sql
agent_templates (
  id uuid PK default gen_random_uuid(),
  name text NOT NULL UNIQUE,              -- kebab-case; becomes the subagent name; immutable after creation
  description text NOT NULL,              -- single sentence, used for routing
  model text,                             -- NULL = inherit; else alias (sonnet|opus|haiku|fable) or full model ID — no CHECK; upstream accepts full IDs
  tools jsonb,                            -- NULL = inherit all; else JSON array allowlist
  prompt_body text NOT NULL,              -- system prompt body (Markdown)
  is_builtin boolean NOT NULL DEFAULT false,  -- seeded rows; editable but not deletable, "Reset to default" available
  updated_by uuid REFERENCES users ON DELETE SET NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now()
)

user_secrets (                             -- generic per-user secret store
  id uuid PK default gen_random_uuid(),
  user_id uuid NOT NULL REFERENCES users ON DELETE CASCADE,
  kind text NOT NULL CHECK (kind IN ('anthropic_token')),  -- new kind = one ALTER-CHECK migration; table shape never changes
  ciphertext bytea NOT NULL,               -- secretbox.Seal(secret)
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (user_id, kind)
)
```

**Builtin source of truth is Go, not SQL** (post-review): the seven builtin definitions (coder, reviewer, auditor, tester, documenter, fact-checker, spec-keeper — content taken from this repo's `.claude/agents/*.md`, so a rendered builtin byte-matches the checked-in file; read-only roles exclude `Edit`/`Write`/`NotebookEdit`, every allowlist includes the team-plumbing tools `SendMessage`, `TaskUpdate`, `TaskList`, `TaskGet`) are embedded in the binary and **reconciled at startup**: missing builtin rows are inserted with `is_builtin=true`, existing rows are never overwritten (admin edits survive restarts). The "Reset to default" endpoint re-applies the same embedded definition. This lets future releases add or improve builtins without a SQL seed that can't be re-run.

No template versioning/history in MVP (deferred; `updated_by`/`updated_at` give minimal attribution). Deleting a non-builtin template is allowed; builtins get "Reset to default" instead of delete, so PRD #4 can always rely on the core roles existing.

### Agent template semantics

- **Authorization**: admin-only writes (`PUT/POST/DELETE`); all authenticated users can list/read/preview (they'll run these agents in PRD #4). This deliberately closes bottega's hole where any user edits the shared prompts that everyone else's agents run with. Per-user template forks are deferred until PRD #4 proves the need.
- **Validation** (server-side): `name` kebab-case `^[a-z0-9]+(-[a-z0-9]+)*$`, unique, and **immutable after creation** (it is the subagent identity — filename and routing key PRD #4 depends on; rename = create new + delete old for non-builtins, builtins are never renamed); `description` non-empty; `model` NULL or a non-empty token (UI offers the aliases, full model IDs accepted — upstream does); `tools` NULL or a JSON array of non-empty strings (no strict allowlist — MCP tool names are legitimate; the UI autocompletes known Claude Code tools and warns on unknown names); `prompt_body` non-empty. Secret guardrail: the server rejects only high-confidence full-token matches (e.g. a complete `sk-ant-…` token); the UI additionally warns on looser credential-ish patterns without blocking (so prompts that *mention* token formats stay legal).
- **Concurrency**: template edits are last-write-wins; two admins editing simultaneously is accepted for MVP (`updated_at`/`updated_by` show what happened). An `If-Match`-style `updated_at` precondition is a noted follow-up, not in scope.
- **Rendering**: `GET /api/agent-templates/:id/rendered` returns the canonical Claude Code subagent Markdown — YAML frontmatter in **fixed field order** (`name` first, then `description`, `tools`, `model`; omit `tools`/`model` when NULL to inherit), with `tools` serialized as the **inline comma-separated string** this repo's `.claude/agents/*.md` files use (`tools: Bash, Read, Grep, …`), not a YAML sequence — a map-based marshal would reorder fields and break byte-stability. `prompt_body` is the body. Golden-file unit tests pin field order and tools serialization. PRD #4 writes this output to the agent workspace's `.claude/agents/`; nothing in this PRD touches the filesystem.

### Anthropic token handling

- **Obtain**: `docs/anthropic-token.md` documents running `claude setup-token` (Claude Code CLI, requires a Claude subscription login) to mint a long-lived OAuth token, plus the console API-key alternative and which to prefer. Doc is written against the current CLI and re-verified at implementation time.
- **Store**: `PUT /api/me/secrets/anthropic_token` body `{token}` → server-side sanity check (non-empty, trimmed, 1–4096 bytes — generous for both `setup-token` OAuth tokens and console API keys; no interior whitespace/control chars; **no format assumption** beyond that — Anthropic token prefixes are not a documented contract) → `secretbox.Seal` → upsert `user_secrets`. Response and all subsequent reads return **metadata only**: `{kind, created_at, updated_at}`. There is no reveal endpoint (multica's reveal+audit is more than MVP needs; the user can re-paste).
- **Rotate/delete**: re-`PUT` overwrites; `DELETE /api/me/secrets/anthropic_token` removes (idempotent — deleting an absent secret returns 204). `UZI_SECRET_KEY` rotation invalidates stored secrets exactly as documented in PRD #2 — same limitation, same doc note.
- **Verification**: no live call against the Anthropic API in this PRD (avoids burning quota and hard-coding endpoint behavior); the token is validated for real the first time an agent runs (PRD #4). UI copy says exactly that.
- **Redaction**: token never logged, never in error strings; establishes (or reuses, if PRD #2 M5 landed first) the redaction-test pattern — a unit test greps handler/store logs for the plaintext fixture.

### API surface (all authenticated, PRD #1 session/CSRF)

- `GET /api/agent-templates` · `GET /api/agent-templates/:id` · `GET /api/agent-templates/:id/rendered` — any user
- `POST /api/agent-templates` · `PUT /api/agent-templates/:id` · `DELETE /api/agent-templates/:id` (409 for builtins) · `POST /api/agent-templates/:id/reset` (builtins only) — admin
- `GET /api/me/secrets` (metadata list) · `PUT /api/me/secrets/anthropic_token` · `DELETE /api/me/secrets/anthropic_token` — current user only; no admin read path to other users' secrets (admin can see *presence* via the PRD #4 agent-status views later, not values)

### Configuration (env)

No new variables. Reuses `UZI_SECRET_KEY` (PRD #2, required, boot guard). The boot guard moves from "PRD #2 feature" to "shared platform requirement" in the config doc.

### UI (extends PRD #1 shell)

1. **Agents** (new sidebar entry) — template list (name, description, model, builtin badge); detail view with rendered-Markdown preview; admin-only edit form (fields + tools tag editor + prompt body textarea with Markdown preview) and Reset-to-default for builtins. Read-only rendering of the same views for non-admins.
2. **Settings → Anthropic token** — status card (not set / set + dates), paste-token form, delete button, link to `docs/anthropic-token.md`, explicit "stored encrypted; validated on first agent run" copy.

## User Journey

1. Admin opens Agents → sees the seven seeded templates; edits `coder`'s prompt body, saves; preview shows the updated subagent Markdown.
2. Regular user opens Agents → sees the same templates read-only.
3. User follows `docs/anthropic-token.md`, runs `claude setup-token`, pastes the token in Settings → Anthropic token → status flips to "set", token is never displayed again.
4. User re-pastes a new token to rotate; deletes it to disconnect.
5. `docker compose down && up` → templates and encrypted secrets survive; DB dump alone cannot recover any token.
6. (PRD #4 consumes both: renders templates into the agent workspace and injects the decrypted token into that user's agent.)

## Milestones

- [ ] **M1 — Secretbox + user_secrets + token API**: shared `api/internal/secretbox/` (skip if PRD #2 M1 already merged it; else ship standalone commit) with `UZI_SECRET_KEY` boot guard; `user_secrets` migration (goose versions `00010`+, range reserved vs PRD #2); `PUT/GET/DELETE /api/me/secrets/*` with metadata-only responses, sanity checks, redaction unit test.
- [ ] **M2 — Token UI + doc**: Settings → Anthropic token page (set/rotate/delete/status); `docs/anthropic-token.md` (`claude setup-token` walkthrough, verified against the current CLI, API-key alternative, rotation and `UZI_SECRET_KEY` caveats).
- [ ] **M3 — Agent template store**: `agent_templates` migration + Go-embedded builtin definitions (seven roles from `.claude/agents/`) with startup reconciler; CRUD API with admin authz, validation (immutable name), builtin delete-protection + reset; renderer to Claude Code subagent Markdown (fixed field order, inline tools string) with golden-file tests proving builtins byte-match the checked-in files.
- [ ] **M4 — Agent template UI**: Agents list/detail/preview for all users; admin edit form + reset; secret guardrail (server reject on full-token match, UI warn on loose patterns) and unknown-tool-name warnings surfaced in the form.
- [ ] **M5 — E2E + hardening**: scripted scenario (seed → admin edit → non-admin blocked (403) → render stable → token set/rotate/delete → restart persistence → DB-dump shows only ciphertext); auditor pass on secret handling and template authz; fix blockers.
- [ ] **M6 — Docs**: README/ARCHITECTURE updates (agent template model, secrets model); configuration doc gains `UZI_SECRET_KEY` as shared platform requirement.

## Success Criteria

- Admin can edit any template field in the UI and the rendered subagent Markdown reflects it immediately; non-admins can view but every write path returns 403.
- The seven builtin roles always exist (startup reconciler, delete blocked, reset works) and their rendered output byte-matches this repo's `.claude/agents/*.md` — PRD #4 can depend on them by name.
- A user can go from no token → token stored encrypted in under 2 minutes following the doc; the plaintext token appears nowhere afterward (no API response, no log line — redaction test enforced); a DB dump alone cannot recover it.
- Rendered output is byte-stable (golden files) and loads as a valid Claude Code subagent definition.
- Demonstrably ≥ inspirations: encrypted per-user provider creds (all three store them plaintext), admin-gated shared templates (bottega lets any user rewrite them), full-shape template editing in a UI (dot-agent-deck is file-only, bottega UI edits prompts only).

## Risks

- **Format drift vs Claude Code subagent spec** (frontmatter fields change upstream) — mitigation: golden-file tests pin *our* output; renderer is one function; PRD #4 integration is the real validation gate.
- **Template content quality** (free-form prompt edits can break agents in ways this PRD can't test) — mitigation: builtin reset path, attribution via `updated_by`, PRD #4 adds run-level feedback; versioning deferred deliberately.
- **Token UX confusion** (`claude setup-token` requires a subscription login; users may paste API keys) — mitigation: doc covers both credential types and the UI copy links it; no format rejection that could false-negative valid credentials.
- **Key-rotation lockout** (`UZI_SECRET_KEY` rotation invalidates templates' users' tokens) — same accepted limitation as PRD #2, documented in both places.
- **Scope creep into agent runtime** — mitigation: hard boundary — this PRD writes no files, spawns nothing, makes no Anthropic API calls; anything execution-shaped is PRD #4.

## Decision Log

- 2026-07-03 (user): create PRD #3 scoped to templates + token storage, parallel-safe against PRD #2; agent runtime explicitly deferred to PRD #4.
- 2026-07-03 (research, submodule audit): multica stores agent defs per-workspace in DB with full UI editing but provider creds plaintext in `custom_env` JSONB (its AES-256-GCM secretbox wraps only Lark/Slack secrets); bottega stores prompts as instance-wide UI-editable files (any user can rewrite them) and per-user OAuth tokens as plaintext chmod-0600 files; dot-agent-deck defines roles in per-project TOML with an embedded default library, no UI, no stored creds (env inheritance). None encrypts provider creds at rest.
- 2026-07-03 (AI): templates admin-write/all-read (closes bottega's shared-prompt hole; stricter than plan.md's literal "we can define/edit agents from UI" — user-confirmed 2026-07-03); template shape = dot-ai-agent-team role fields rendered to Claude Code subagent Markdown (PRD #4's consumption format); generic kind-keyed `user_secrets` over a column on `users` (next secret kind needs no shape change); paste-token MVP over bottega-style device flow (fewer moving parts, doc'd CLI path exists); no live token verification (first agent run validates); no reveal endpoint (re-paste to rotate); builtin templates editable-but-not-deletable with reset.
- 2026-07-03 (AI, post-review — design review + fact-check, all factual claims confirmed): goose migration version ranges reserved across parallel PRDs (#2: 00002–00009, #3: 00010+ — duplicate versions merge cleanly in git but fail at `goose up`); builtin source of truth moved from SQL seed to Go-embedded definitions with idempotent startup reconciler (reset reuses the same source; future releases can add/upgrade builtins); builtins expanded to the repo's actual seven agents, rendered output required to byte-match `.claude/agents/*.md`; renderer pins name-first field order + inline comma-separated `tools` string (repo format; map marshal would break byte-stability); `model` CHECK dropped (upstream accepts `fable` and full model IDs); template `name` immutable (PRD #4 keys on it); edit concurrency = documented last-write-wins; secret guardrail split into server-reject (full-token match only) + UI-warn (avoids false-positives on prompts mentioning token formats); token length bounds 1–4096B, DELETE idempotent; `user_secrets` "no new schema" wording corrected to "no shape change, still an ALTER-CHECK migration"; shared frontend shell files (nav/settings/routes) flagged as expected merge points with PRD #2; multica table cell tightened (custom_env removed from normal responses entirely, not merely redacted).
