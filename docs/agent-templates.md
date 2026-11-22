---
title: Agent templates
order: 50
audience: user
---

# Agent templates

An agent template defines one role an agent can play: a name, a
one-sentence description, an optional model pin for each harness, an optional
tools allowlist, and a system prompt body. Manage them from **Agents**.

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

uzi seeds twelve builtin templates. `lead` is uzi's own. The other eleven are
unchanged copies of uzi's upstream role library (the `product-agents/` folder of
the skills repository, at the release commit pinned as `upstream_sha` in the uzi
source's `api/internal/agenttmpl/library/manifest.json`), so their generic role
text is the same as in a plain Claude Code agent team. uzi adds its runtime
rules and, for `fact-checker`, its forge tools on top (both explained below):

| Name | Claude pin | What it does |
|---|---|---|
| `lead` | `opus` | Plans the task, delegates, and drives the run through the approval gate. |
| `coder` | `sonnet` | Implements features, fixes bugs, refactors code. |
| `reviewer` | `opus` | Reviews code changes for correctness, style, and edge cases. |
| `auditor` | `opus` | Audits code for security vulnerabilities and unsafe patterns. |
| `tester` | `opus` | Validates changes against representative real-world inputs, authoring and extending tests as part of the implementation phase. |
| `architect` | `opus` | Designs the approach before coding and reviews changes for architectural fit; writes design docs only, and only once the plan is approved. |
| `documenter` | `sonnet` | Updates documentation only; never touches source code. |
| `fact-checker` | `opus` | Adversarially verifies factual claims against authoritative sources. |
| `spec-keeper` | `opus` | Keeps `specs/` in sync with implementation work. |
| `researcher` | `sonnet` | Investigates the codebase or external sources to gather context; reports findings only. |
| `web-ux` | `opus` | Validates web interfaces in a real browser (agent-browser), reviewing UX, accessibility, and visual consistency; reports findings only. |
| `ux-designer` | `opus` | Sets opinionated visual and information-architecture direction, then builds and browser-validates the frontend/UI; owns the design layer and defers backend logic to the coder. |

The table lists the shipped Claude pins; shipped builtin Codex pins are empty.
The lead is the orchestrator: the main agent thread. Both harnesses select
the first delivered own template whose name is exactly `lead` or
`orchestrator`, case-insensitively, preserving delivered order. All own
templates with either name are withheld from the subagent roster, including
later matches. Repo-sourced templates, including a repo `lead`, remain
ordinary subagents; they do not replace the root.

For run model settings, see [Worker model](./worker-model.md). Editing a
template's prompt only tunes its persona and workflow; the primary-directive
guardrails (never touch `main`, no `git push`, the plan gate) are enforced by
the worker regardless.

The list has a **Model & effort** column that names both harnesses with
per-family lines: **Claude: sonnet**, then **effort: high**, and the same for
Codex. The detail page uses the same per-family lines. Empty pins use the same honest
**Default · …** labels in the editor, builtin diff and agent-source review:

- Subagent model: **Default · follows lead**, for either harness.
- Subagent effort: **Default · follows the run's effort**, for either harness.
- Lead (or orchestrator), Claude model: **Default · SDK/account default (built-in)**.
- Lead (or orchestrator), Codex model: **Default · built-in (gpt-6.1-sol)**.
- Lead (or orchestrator), effort, either harness: **Default · built-in (medium)**.

A subagent whose four pins are all empty collapses to the two Default lines
plus **both harnesses**. A lead or orchestrator template never collapses.

The list also shows a personalized lead line, **For your Claude runs: …** (and
the Codex equivalent), built from your effective settings: the worker model and
effort with their source. It appears only for harnesses you can use. It sits on
the lead row when exactly one lead template is enabled for you; otherwise it is
a note above the table (**For your lead runs**). It refreshes after you change
**In my runs** or **Global default**, and is hidden if the read fails. The
stored-pin cells are shared and never change per viewer.

For the lead, the editor help states the precedence. Model: a model chosen when
starting the run or a schedule's model, then your **Run defaults** model, beat
the pin; with none and no pin, Claude uses the SDK/account default and Codex
uses gpt-6.1-sol. Effort: an effort chosen when starting the run, then your
**Run defaults** effort (a level or Runtime default), beat the pin; otherwise
the built-in medium. The subagent help keeps that **Use the run's resolved model
for subagents too** overrides model pins only.

## Model pins

A template stores two independent pins: `model` is the **Claude pin**, and
`codex_model` is the **Codex pin**. A run receives the pin for its own harness.
For a subagent, an empty pin follows the model the run resolved for the lead.
The schedule's model takes precedence over your worker-model lane, then the
selected lead template's pin for the run's harness. An empty lead pin falls
back to the SDK/account default for Claude or `gpt-6.1-sol` for Codex. The
shipped lead has a Claude pin of `opus` and an empty Codex pin.

A subagent's own pin for the run's harness takes precedence over following
the resolved root unless the schedule's **Use the run's resolved model for
subagents too** toggle is set; then subagents follow the resolved root too. The lead's Codex fallback
needs a worker from this release; older Codex workers use the built-in
provider model when no higher-priority setting applies. See
[Model precedence](./worker-model.md#model-precedence) for rollout and the
pending hosted validation.

Save-time rules apply to builtin, global and personal templates:

- The Claude pin accepts aliases such as `opus` and `sonnet` and custom model
  IDs, except the five supported Codex role IDs below. Model IDs must satisfy
  the shared syntax rules (at most 100 bytes, no control or format characters).
- The Codex pin accepts `gpt-5.6-sol`, `gpt-6-astra`, `gpt-6-luna`,
  `gpt-6-sol` and `gpt-6.1-sol`. Other IDs, including custom Codex IDs and
  Claude aliases, are rejected.
- JSON `null` or a blank string clears a pin. Omitting `codex_model` on create
  leaves it empty; omitting it on update preserves the stored Codex pin.
  Invalid pins return HTTP 400 naming `model` or `codex_model`.

For example, a template with `model: sonnet` and `codex_model: gpt-6-sol`
delivers `sonnet` on a Claude run and `gpt-6-sol` on a Codex run. Claim delivery
also filters stored values by harness, so a misplaced Codex role ID in the
Claude column is omitted rather than sent to Claude.

On upgrade, the migration moves recognized Codex role IDs already stored in
`model` into `codex_model` and clears `model`. Surrounding whitespace is trimmed
when recognizing those IDs; other stored model values are left unchanged.

Use **Claude model** and **Codex model** to edit the pins independently.
Claude's **Other (custom model ID)…** option accepts custom IDs; Codex offers
only the five role IDs above. The editor warns and blocks Save for a pin in
the wrong family. See the [approved PRD](../prds/3249-per-harness-template-pins.md).

### Changing your own pins from the CLI

You can read and change the Claude and Codex pins of **your own** templates with
`uzi agent-template list|show|set|reset`, for example `uzi agent-template set
my-reviewer --harness claude sonnet`; the same commands pin the reasoning
effort with `--effort` (see [Reasoning effort pins](#reasoning-effort-pins)).
The save-time rules above apply, since the
server validates the pin like the web editor, and a clear (`reset`) makes that
family inherit again. Only a template you own (scope `user`) is editable: a
builtin or global template is edited in the web UI by an admin, even when the
CLI token is an admin's, and another user's template is not found. The command
changes only the pins; description, tools and prompt are untouched. See
[Agent-template model pins](./cli.md#agent-template-model-and-effort-pins) for the commands,
name and id resolution and exit codes.

The builtin `fact-checker` also gets six `mcp__forge__*` read tools, so it
alone can read the run's own forge (issues, merge requests, pipelines, label
history) to check a claim against live state; see
[Forge read tools](./forge-read-tools.md). uzi adds them to its tools list
when the template is seeded; they are not in the upstream file. A subagent
reaches the forge only through an explicit `mcp__forge__*` entry in its
`tools:` list: a template with no `tools:` list inherits the standard tools but
not the forge ones. The lead is the exception and always has forge read access.

## Reasoning effort pins

A template also stores two independent effort pins: `effort` is the **Claude
effort** and `codex_effort` is the **Codex effort**. A run receives the pin for
its own harness only; the other family's pin is never sent.

- **Levels.** `low`, `medium`, `high`, `xhigh` or `max`. The editor's
  **Default · follows the run's effort** leaves a subagent's pin empty; for the
  lead it reads **Default · built-in (medium)**.
  `runtime` (Runtime default) is not a template effort. The API rejects it, and
  any other value, with HTTP 400 naming `effort` or `codex_effort`; the CLI
  refuses it before sending anything (exit 2), and the editor offers only the
  five levels.
- **Clearing.** JSON `null` or a blank string clears a pin. Omitting an effort
  field on update preserves the stored pin.
- **Subagents.** A subagent with a pin for the run's harness uses it. An empty
  pin follows the run's resolved lead effort for its phase, and when that is
  **Runtime default** nothing is sent. This is the stored contract; see
  [Reasoning effort](./worker-effort.md#subagent-effort-pins) for where it
  applies.
- **The lead template.** Its effort is not a subagent pin. It is a tier of the
  lead's own effort: used only when you have no per-harness default effort, and
  beaten by a run-level effort and by your own preference, including **Runtime
  default**. See [Reasoning effort](./worker-effort.md#the-lead-templates-effort).
  The shipped `lead` has no effort pin.
- **Older workers.** A worker that predates template effort (it does not
  advertise `template_effort_v1`) is sent no effort pins, so those subagents run
  at the run's effort. When at least one delivered subagent template pins an
  effort for the run's harness, the run feed shows one line per claim: "stored
  subagent effort pins omitted: this worker predates template effort; those
  subagents use the run's effort". The lead template's effort still reaches it
  through the lead's default effort.

Use **Claude effort** and **Codex effort** to edit the pins independently.
Editing a builtin's effort customizes it (see [Resetting a builtin
template](#resetting-a-builtin-template)).

## Parallel dispatch

The lead can dispatch more than one subagent in the same turn when their
work doesn't overlap, instead of always waiting for one to finish before
starting the next:

- **Validators fan out together, once per unit.** The lead sends every allocated
  validator (`reviewer`, `auditor`, `tester`, `fact-checker` — whichever the run
  allocated) in one wave rather than one at a time: first over the **plan**,
  before it reaches you at the approval gate, and then again each time an
  implementation unit lands — over that unit's **immutable commit range**, not the
  live working tree, so a later unit's edits can't be mis-attributed to it. There
  is one review procedure, run per unit and early, rather than a single wave held
  to the end of the run. The plan-time wave is what backs up the plan's claims —
  for every mechanism the plan asserts, it names the file and quotes the line — so
  what you approve has already been read against the code. Of those four,
  `reviewer`, `auditor` and `fact-checker` declare no file-writing tools at all,
  so they are read-only everywhere; `tester` does declare them, because authoring
  tests is its job during implementation.
- **Nothing a subagent does before the gate can change the worktree by the
  ordinary route.** On the planning turn the worker takes the file-writing tools
  (`Edit`, `Write`, `MultiEdit`, `NotebookEdit`) off every subagent it dispatches,
  so what you approve is not quietly accompanied by edits you never saw. This is
  half of the property, and the honest half to state: those roles still have
  `Bash`, and the worker does not screen shell redirection, so the lead's
  report-only instruction to the wave still carries the rest. Like everything
  else in a prompt, that part is an instruction rather than one of the guardrails
  the worker enforces.
- **Coders fan out only for genuinely independent units.** The lead
  parallelizes implementation work only when the plan splits it into pieces
  that share no Go package, no TypeScript project, and no file (including
  `go.mod`, lockfiles, generated code, or wiring/registration files) between
  them. Each parallel coder gets an explicit file scope in its delegation
  prompt, doesn't commit, and doesn't run repo-wide build or test commands;
  the lead diffs the working tree against the last commit to confirm only the
  declared scopes changed, commits each landed unit, then runs the quality gate
  over that commit — by default overlapped with the read-only validator wave
  rather than serialized ahead of it, but still blocking: a red gate holds that
  unit, it is not advanced or built on until the gate is green. Once review
  invalidates a candidate whose full gate is costly, the lead finishes the repair
  reviews and focused checks before repeating the full gate. A failed gate is
  triaged (narrowest reproducer, on candidate and base) before it is rerun.
- **A unit others depend on can publish its seam early.** When the plan declares
  that one unit's output (a schema change, a shared type, an interface or route
  shape) another builds on, the lead can land and commit that seam first so the
  dependents start against it — but only once the seam has actually been exercised
  by a test, not merely compiled.
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
   name (kebab-case, immutable after creation), description, and prompt body.
3. Optionally set **Claude model**, **Codex model**, **Claude effort** and
   **Codex effort**, or restrict tools.
   Leave either harness on **Default** (effort: **Default · follows the run's
   effort**) so a subagent follows the lead on that harness. **Use the run's resolved model for subagents too** on a schedule overrides
   these model pins; effort pins are kept.
4. The live Markdown preview exports the Claude definition, including its
   Claude model and effort pins. The Codex pins are saved separately.

![Creating an agent template with both harness pins and the Claude Markdown preview](img/agent-templates-editor.png)

Never paste a credential into a description or prompt: uzi rejects anything
that looks like a real Anthropic token. Credentials belong in
[Anthropic token](./anthropic-token.md).

### Resetting a builtin template

Open a builtin template's detail page (click its name from **Agents**) and
click **Reset to default**. It re-applies the shipped builtin body
**verbatim** — it does not merge. If you've customized a builtin's prompt
(say, `lead` or `coder`) and reset it to pick up a change shipped in a newer
uzi version, your customization is gone, not folded into the new body.

A shipped change to a builtin's prompt reaches a **pristine** template (one
an admin has never edited and no [agent source](./agent-source.md) sync has
replaced) automatically on the next boot after you upgrade uzi: uzi re-applies
the body it ships for that builtin, so recipient fixes, prompt improvements, and
sandbox tweaks land without a manual step. Nothing changes between upgrades; the
role library a uzi version ships is the release commit pinned as `upstream_sha` in its
`api/internal/agenttmpl/library/manifest.json`. A builtin an admin has
**customized** is never overwritten on boot; your edit stays until you
**Reset** it, so customizations remain durable across upgrades.

Editing a builtin marks it customized and opts it out of that automatic
tracking until it is reset. Changing either model pin counts as a customization:
setting a Codex pin also stops shipped prompt refreshes until **Reset**.
Changing either effort pin counts too. **Reset** restores the shipped values,
including the shipped efforts (none for the builtins today).
Saving it with exactly the shipped description, both model pins, both effort
pins, tools and body is not an edit: the template stays pristine and keeps tracking shipped changes.
**Reset to default** returns a builtin to pristine: it re-applies the shipped
content, including the Claude pin and the empty Codex pin, and re-enables
tracking, so the template picks up future shipped changes on boot again.

A **differs from shipped** badge on the Agents list and on a builtin's
detail page tells you when a **customized** builtin's stored description,
Claude pin, Codex pin, tools, or prompt body no longer matches what this uzi
version ships for it. (A pristine builtin refreshes on boot, so it already matches and carries
no badge.) Open the template before resetting: the editor shows exactly what's
different, so you're not resetting blind.

When a run starts, uzi adds its own runtime rules to each subagent (the run's
scratch directory, the review-snapshot recipe, the safety rules) through a
shared block, and gives the lead its own runtime guidance. Neither is part of
the stored template, and neither can be edited from **Agents**; they live in
the uzi source (`agent/src/prompt.ts`) and apply to your own templates and to
repo-sourced agents too.

A builtin can also arrive by a third route: an admin-configured
[agent source](./agent-source.md) repo an admin has approved a sync from. A
**synced** template shows a distinct `synced` badge instead of the drift
badge — its body came from that source, not the shipped default and not a
hand edit — and, if it's overriding a builtin, is still resettable back to
the embedded body the same way a customized one is. A synced builtin does not
take shipped changes on boot until it is reset.

Drift and reset live in the web UI and the REST API — the badge, the
shipped-vs-stored diff, and the reset action. There is no `uzi` command for
template or skill drift or reset, by design (`uzi agent-template` edits only the
model pins of your own templates): the CLI's admin surface is
read-only, and reset produces nothing a script would capture. Builtin **skills** follow the same edit-preserving rule
with one difference worth knowing: a shipped change to a builtin skill is **not**
re-applied on boot and carries no drift badge, so a builtin skill an admin has
customized only picks up a newer shipped body when it is explicitly reset.

To pick up a new builtin body on a **customized** template without losing your
own edits:

1. Open the template and read the shipped-vs-stored diff to see exactly
   what changed (or check `api/internal/agenttmpl/builtins/`'s git history
   in the uzi repo, or ask whoever deployed the upgrade).
2. Reset the template — this returns it to pristine and re-enables tracking.
3. Re-apply your customization on top of the new body.

Or skip reset entirely and hand-merge the new paragraphs into your
customized body yourself.

A repo can ship its own agent roster in `.claude/agents/` or `.codex/agents/`; you can run those
instead of your templates, chosen per run at the plan gate — see
[Repo agents](./repo-agents.md).

See [ARCHITECTURE.md](../ARCHITECTURE.md#agent-templates) for the renderer,
scope/allocation model, claim filtering, and API surface.
