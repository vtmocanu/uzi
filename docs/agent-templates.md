---
title: Agent templates
order: 50
audience: user
---

# Agent templates

An agent template defines one role an agent can play: a name, a
one-sentence description, an optional model override, an optional tools
allowlist, and a system prompt body. Every authenticated user can list,
view, and preview templates from **Agents**; only an admin can create,
edit, delete, or reset one, since a template's prompt is shared by everyone
who runs that role.

## Builtin roles

uzi seeds seven builtin templates, mirroring this repo's own agent role
library:

| Name | What it does |
|---|---|
| `coder` | Implements features, fixes bugs, refactors code. |
| `reviewer` | Reviews code changes for correctness, style, and edge cases. |
| `auditor` | Audits code for security vulnerabilities and unsafe patterns. |
| `tester` | Validates changes against representative real-world inputs. |
| `documenter` | Updates documentation only; never touches source code. |
| `fact-checker` | Adversarially verifies factual claims against authoritative sources. |
| `spec-keeper` | Keeps `specs/` in sync with implementation work. |

Builtins can be edited freely but not deleted; use **Reset to default** to
re-apply the shipped definition instead.

## Edit a template

1. Open **Agents** and pick a template (or **New** for a custom one).
2. Set its name (kebab-case, immutable after creation: renaming means
   creating a new template and deleting the old one), description, and
   prompt body; optionally override the model or restrict its tools.
3. The detail page shows a live preview of the rendered Markdown as you
   edit.

![Editing an agent template, with the rendered Markdown preview alongside](img/agent-templates-editor.png)

Never paste a credential into a description or prompt: uzi rejects
anything that looks like a real Anthropic token. Credentials belong in
[Anthropic token](./anthropic-token.md).

## Permissions

| Action | Who |
|---|---|
| List, view, preview | Any authenticated user |
| Create, edit, delete, reset | Admin only |

See [ARCHITECTURE.md](../ARCHITECTURE.md#agent-templates) for the
renderer, full validation rules, and API surface.
