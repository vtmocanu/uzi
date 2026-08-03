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

uzi seeds eleven builtin templates:

| Name | What it does |
|---|---|
| `lead` | Plans the task, delegates, and drives the run through the approval gate. |
| `coder` | Implements features, fixes bugs, refactors code. |
| `reviewer` | Reviews code changes for correctness, style, and edge cases. |
| `auditor` | Audits code for security vulnerabilities and unsafe patterns. |
| `tester` | Validates changes against representative real-world inputs. |
| `architect` | Designs the approach before coding and reviews changes for architectural fit; writes design docs only. |
| `documenter` | Updates documentation only; never touches source code. |
| `fact-checker` | Adversarially verifies factual claims against authoritative sources. |
| `spec-keeper` | Keeps `specs/` in sync with implementation work. |
| `researcher` | Investigates the codebase or external sources to gather context; reports findings only. |
| `web-ux` | Validates web interfaces in a real browser (agent-browser), reviewing UX, accessibility, and visual consistency; reports findings only. |

The `lead` is the orchestrator: the main agent thread. It runs on `opus`
by default unless you set a personal override in
[Worker model](./worker-model.md). Editing a template's prompt only tunes
its persona and workflow; the primary-directive guardrails (never touch
`main`, no `git push`, the plan gate) are enforced by the worker regardless.

## Parallel dispatch

The lead can dispatch more than one subagent in the same turn when their
work doesn't overlap, instead of always waiting for one to finish before
starting the next:

- **Read-only validators fan out together, twice.** The lead sends every
  allocated read-only subagent (`reviewer`, `auditor`, `tester`,
  `fact-checker` — whichever the run allocated) in one wave rather than one at
  a time: first over the **plan**, before it reaches you at the approval gate,
  and again over the **diff** once an implementation unit lands. The plan-time
  wave is what backs up the plan's claims — for every mechanism the plan
  asserts, it names the file and quotes the line — so what you approve has
  already been read against the code. The lead tells that wave to report only
  and to change nothing in the worktree; like everything else in a prompt, that
  is an instruction rather than one of the guardrails the worker enforces.
- **Coders fan out only for genuinely independent units.** The lead
  parallelizes implementation work only when the plan splits it into pieces
  that share no Go package, no TypeScript project, and no file (including
  `go.mod`, lockfiles, generated code, or wiring/registration files) between
  them. Each parallel coder gets an explicit file scope in its delegation
  prompt, doesn't commit, and doesn't run repo-wide build or test commands;
  the lead diffs the working tree against the last commit to confirm only the
  declared scopes changed, commits once, then runs the quality gate once.
- **Anything uncertain stays serial** — overlapping scope, a dependency
  between units, or a fix that depends on a reviewer's finding.

You'll see this on a run's activity feed as multiple subagents active within
the same turn. Two parallel invocations of the same coder template currently
render as one merged section with interleaved messages — per-invocation
attribution is a possible future improvement, not built yet.

There's no separate concurrency cap for subagents within a run; width is
bounded by how the plan splits, and by the lead defaulting to serial whenever
it's unsure. It also compounds with
[worker run concurrency](./worker-setup.md#concurrent-runs): if your worker
runs more than one run at once, every parallel subagent in every concurrent
run shares the same per-user Anthropic token.

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

### Resetting a builtin template

Open a builtin template's detail page (click its name from **Agents**) and
click **Reset to default**. It re-applies the shipped builtin body
**verbatim** — it does not merge. If you've customized a builtin's prompt
(say, `lead` or `coder`) and reset it to pick up a change shipped in a newer
uzi version, your customization is gone, not folded into the new body.

That's also why a shipped change to a builtin's prompt doesn't reach you
automatically: it seeds into a fresh database on first boot, but an
already-seeded row is never silently overwritten (that's what keeps your
customizations durable across every other upgrade). Reset is the only
automatic path that pulls in a newer builtin body for an existing deployment,
and it's all-or-nothing — the alternative is pasting the new body in by hand,
below.

To pick up a new builtin body without losing your own edits:

1. Compare your current template body against the new one shipped in this
   version (its git history is `api/internal/agenttmpl/builtins/` in the uzi
   repo, or ask whoever deployed the upgrade).
2. Reset the template.
3. Re-apply your customization on top of the new body.

Or skip reset entirely and hand-merge the new paragraphs into your
customized body yourself.

A repo can ship its own agent roster in `.claude/agents/`; you can run those
instead of your templates, chosen per run at the plan gate — see
[Repo agents](./repo-agents.md).

See [ARCHITECTURE.md](../ARCHITECTURE.md#agent-templates) for the renderer,
scope/allocation model, claim filtering, and API surface.
