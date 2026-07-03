# Agent templates

An agent template is the definition of one role an agent can play: a name,
a one-sentence description, an optional model override, an optional tools
allowlist, and a system prompt body. uzi stores templates in the database and
admins edit them from **Agents** in the UI; every authenticated user can list,
view, and preview them. This page covers the template model this PRD ships.
Spawning agents from a template, connecting them to the server, and running
them against real work is a later release (see [plan.md](../plan.md)).

## Builtin roles

uzi seeds seven builtin templates from this repo's own `.claude/agents/*.md`
role library:

| Name | What it does |
|---|---|
| `coder` | Implements features, fixes bugs, refactors code. |
| `reviewer` | Reviews code changes for correctness, style, and edge cases. |
| `auditor` | Audits code for security vulnerabilities and unsafe patterns. |
| `tester` | Validates changes against representative real-world inputs. |
| `documenter` | Updates documentation only; never touches source code. |
| `fact-checker` | Adversarially verifies factual claims against authoritative sources. |
| `spec-keeper` | Keeps `specs/` in sync with implementation work. |

Builtins are inserted at API startup by an idempotent reconciler: a missing
builtin is created, an existing row (including one an admin has edited) is
never overwritten. This means builtins survive both restarts and admin edits
without needing a re-runnable SQL seed, and future releases can add or update
builtins the same way. A builtin's rendered output byte-matches the
corresponding checked-in `.claude/agents/*.md` file, pinned by golden-file
tests.

Builtins can be edited freely but not deleted (`DELETE` returns 409); use
**Reset to default** instead, which re-applies the shipped definition. This
guarantees the core roles always exist for whatever consumes them later.

## Rendered format

Every template renders to Claude Code's subagent Markdown format, the same
shape as `.claude/agents/*.md`: YAML frontmatter (`name`, `description`,
`tools`, `model`, in that fixed order) followed by a blank line and the
prompt body. `tools` is written as an inline comma-separated string, not a
YAML list, to match this repo's own files. `tools` and `model` are omitted
entirely when the template inherits (a `NULL` tools allowlist means "all
tools"; a `NULL` model means "inherit the default"):

```markdown
---
name: coder
description: Implements features, fixes bugs, refactors code. Runs the project's test/lint commands before reporting done.
model: opus
---

<prompt body...>
```

`GET /api/agent-templates/:id/rendered` returns this Markdown directly (not
wrapped in JSON) for any authenticated user. The **Agents** detail page shows
the same output as a live preview, both for a stored template and while an
admin is editing one.

## Permissions

Reading is open to every authenticated user (they will run these roles once
agent execution lands); writing is admin-only:

| Action | Who |
|---|---|
| List, view, preview | Any authenticated user |
| Create, edit, delete, reset | Admin only |

This is deliberate: a template's prompt and tools allowlist are shared by
every user who runs that role, so only an admin can change what everyone
else's agents do.

## Validation

- `name`: kebab-case (`^[a-z0-9]+(-[a-z0-9]+)*$`), unique, and **immutable
  after creation** (it is the subagent's filename and identity, so renaming
  means creating a new template and deleting the old one; builtins are never
  renamed).
- `description` and `prompt_body`: required, non-empty.
- `model`: omit to inherit, or any non-empty token (a model alias or a full
  model ID; nothing is validated against a fixed list).
- `tools`: omit to inherit all tools, or a JSON array of non-empty tool
  names.
- `description`, `model`, and each tool name must not contain a newline,
  carriage return, or other control character (they each render on a single
  frontmatter line; a stray newline could forge or duplicate a YAML key).
  The prompt body's own newlines are ordinary Markdown and are unaffected.
- A template is rejected if its description or prompt body contains what
  looks like a complete Anthropic token (a high-confidence `sk-ant-...`
  match). This only blocks a real credential; the UI separately warns,
  without blocking, on looser patterns (like a prompt that merely mentions
  the token format) so legitimate text stays savable. Credentials belong in
  **Settings, Anthropic token** (see [anthropic-token.md](anthropic-token.md)),
  never in a template.

## API surface

All endpoints require authentication (session + CSRF); the writes below also
require admin:

- `GET /api/agent-templates`: list
- `GET /api/agent-templates/:id`: one template
- `GET /api/agent-templates/:id/rendered`: rendered subagent Markdown
- `POST /api/agent-templates`: create
- `PUT /api/agent-templates/:id`: update (name is ignored; immutable)
- `DELETE /api/agent-templates/:id`: delete a non-builtin (409 on a builtin)
- `POST /api/agent-templates/:id/reset`: reset a builtin to its shipped
  definition (400 on a non-builtin)

Edits are last-write-wins; there is no optimistic-concurrency check in this
release. Every template row records `updated_by` and `updated_at` so
concurrent edits are at least attributable after the fact.
