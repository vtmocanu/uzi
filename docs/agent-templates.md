---
title: Agent templates
order: 50
audience: user
---

# Agent templates

An agent template defines one role an agent can play: a name, a
one-sentence description, an optional model override, an optional tools
allowlist, and a system prompt body. Manage them from **Agents**.

## Scopes

Every template has a scope that decides who sees it and who can edit it:

| Scope | Who sees it | Who edits it |
|---|---|---|
| **Builtin** | Everyone | Admins (edit or reset; never deletable) |
| **Global** | Everyone | Admins |
| **Mine** (user) | Only you | Only you |

Anyone can create their own **Mine** template (persona and workflow only —
the worker's guardrails apply no matter what the prompt says). An admin can
additionally publish a **Global** one for everyone. `lead` and
`orchestrator` are reserved: a template you create may not take those names.

## Builtin roles

uzi seeds eight builtin templates:

| Name | What it does |
|---|---|
| `lead` | Plans the task, delegates, and drives the run through the approval gate. |
| `coder` | Implements features, fixes bugs, refactors code. |
| `reviewer` | Reviews code changes for correctness, style, and edge cases. |
| `auditor` | Audits code for security vulnerabilities and unsafe patterns. |
| `tester` | Validates changes against representative real-world inputs. |
| `documenter` | Updates documentation only; never touches source code. |
| `fact-checker` | Adversarially verifies factual claims against authoritative sources. |
| `spec-keeper` | Keeps `specs/` in sync with implementation work. |

The `lead` is the orchestrator: the main agent thread. It runs on `opus`
by default unless you set a personal override in
[Worker model](./worker-model.md). Editing a template's prompt only tunes
its persona and workflow; the primary-directive guardrails (never touch
`main`, no `git push`, the plan gate) are enforced by the worker regardless.

## Allocation: which agents ride your runs

The **In my runs** toggle on each row decides whether that template is
delivered to your runs. Builtin and global templates are **on by default**
(admins set that default set via the **Global default** toggle); your own
templates are off until you enable them. Absent your own choice, a template
follows the global default. Once you flip a toggle it stays an explicit
choice for that template (there is no per-row "follow the default again"
affordance yet); a global template you turned off stays off for you until you
turn it back on.

If you name a **Mine** template the same as a builtin or global one, it is
**shadowed**: the shared one wins and yours is dropped from your runs (shown
with a `shadowed` badge). Delete and recreate it under a different name to use it
(a template's name is fixed once created).

## Create or edit a template

1. Open **Agents**, click **New agent** (or a template's name to edit).
2. Pick the scope (admins only — yours is always **Mine**), then set the
   name (kebab-case, immutable after creation), description, and prompt
   body; optionally override the model or restrict its tools.
3. The detail page shows a live preview of the rendered Markdown.

![Editing an agent template, with the rendered Markdown preview alongside](img/agent-templates-editor.png)

Never paste a credential into a description or prompt: uzi rejects anything
that looks like a real Anthropic token. Credentials belong in
[Anthropic token](./anthropic-token.md).

A repo can ship its own agent roster in `.claude/agents/`; you can run those
instead of your templates, chosen per run at the plan gate — see
[Repo agents](./repo-agents.md).

See [ARCHITECTURE.md](../ARCHITECTURE.md#agent-templates) for the renderer,
scope/allocation model, claim filtering, and API surface.
