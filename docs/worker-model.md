---
title: Worker model
order: 45
audience: user
---

# Worker model

Pick which model your own runs use, per harness: one setting for Claude runs
and a separate one for Codex runs, overriding the `lead` [agent
template](./agent-templates.md)'s model just for you. Other users' runs are
unaffected.

## Harness and worker models

Both settings live together in one **Settings → Run defaults** card:

- A **default harness** picker, shown only when you have a usable credential
  for both Claude and Codex. It decides which harness a run uses when
  nothing else names one. With an explicit default it is subject to the
  optional creation-time overflow below.
- One **model lane per usable harness**: Anthropic and Codex side by side
  when both are usable, or a single lane when only one is. With no usable
  credential yet, the card still shows the Claude lane so you can
  preconfigure it before connecting one.

Each picker's empty option is a **Default · …** label naming where the value
comes from: the Claude model picker reads `Default · lead template` (the
`lead` template's model) and the Codex model picker reads
`Default · gpt-6.1-sol (built-in)`. The harness picker's empty option reads
`Default · automatic`: uzi picks your only usable harness, or, when both are
usable, chooses by weekly limits for eligible new issue runs (started
manually, by autopilot or by a schedule, with no explicit harness or issue
harness label, model pin, Anthropic credential override or Plan/Code
cross-check; other runs keep the usual resolution: an explicit harness, else
Claude). It picks the harness whose weekly allowance
resets soonest, falling back to Claude when usage is unknown; see [How the
automatic default picks a harness](./rate-limits.md#how-the-automatic-default-picks-a-harness).
The start-run form still says **Use my default** for the harness; the
schedule modal names the source instead (see [Which harness a schedule runs
on](./scheduling.md#which-harness-a-schedule-runs-on)).

Switching the default harness never clears either model. Each lane is its
own retained preference, so alternating between Claude and Codex keeps both
choices intact. The lane matching your effective default harness carries a
**Default harness** badge. Click **Save defaults** to save the harness and
both lanes together, in one request.

![Settings, Run defaults, showing the grouped Harness and worker models card with side-by-side Claude and Codex model lanes](img/worker-model-settings.png)

## Overflow to the other harness

In **Settings → Run defaults**, turn on **Overflow to the other harness when my default is near its limits**.
This per-user boolean is off by default and saves automatically, separately
from **Save defaults**. The CLI equivalent is
`uzi settings harness-overflow on|off`; `uzi settings get` reads it back.
Overflow applies only when your default harness is an explicit Claude or
Codex. With **Default · automatic** the toggle has no effect: the card
disables it with an explanation, and the CLI prints `(inactive: default
harness is automatic)`.

Overflow applies at creation to **new issue runs** started manually, by
autopilot or by a schedule, when both harnesses are usable and the harness
choice is implicit. An explicit harness (including an issue harness label),
a run/schedule model pin, an Anthropic credential override, or either saved
Plan or Code cross-check enable flag excludes it, even if that checker
would be inactive for the preferred harness. Chat, jobs, tasks, prompts,
reviews, rework, CI fixes and other origins do not use overflow.

uzi compares stored usage and capacity holds. The preferred comparison
score must be **below 20%**, the other harness **at least 30%**, and the
advantage **at least 15 percentage points**. An unknown comparison keeps
the preferred harness. See [How overflow compares headroom](./rate-limits.md#how-overflow-compares-headroom)
for account selection, conservative Claude aggregation and hold handling.

The harness and creation explanation are frozen atomically. Existing runs,
including resumes and usage-limit parks, never switch harness through
overflow. The owner's run page, `uzi run get <id>` (human and `--json`) and
TUI detail rail show the stored preferred/chosen harness, reason, comparison
scores, measured headroom and holds (and, for an automatic choice, the weekly
resets compared). This is creation-time evidence, not
live usage; legacy or unavailable evidence reads **not recorded**.

## Model precedence

For a given run, uzi resolves the harness first (an explicit run/schedule
choice, then your default harness, then whichever harness you have a
credential for; with an explicit default, eligible overflow is applied at
creation, and with **Default · automatic** and both harnesses usable, the
harness is chosen by weekly limits), and only then
reads that harness's model lane:

1. The **schedule's model**, if the run was started by a
   [schedule](./scheduling.md) that pins one compatible with the run's
   harness.
2. Your **worker model** for that harness, if set.
3. The harness fallback: the `lead` template's Claude pin (`opus` by default)
   for Claude, or its Codex pin, else `gpt-6.1-sol`, for Codex.
4. Whatever the underlying SDK/your account defaults to.

The lead's Codex fallback requires a worker from this release. Older Codex
workers fall through to the built-in provider model (`gpt-6.1-sol`) when no
schedule or worker-model setting takes precedence. Roll out the API first,
then the hosted worker; hosted validation of this behavior remains pending.
The pickers' existing **Default · …** labels above are unchanged pending
separate UI work.

An incompatible frozen schedule or legacy model (e.g. a Claude alias pinned
on a run that resolves to Codex) falls back to your lane for that harness,
with a visible fallback note on the run. A subagent with its own Claude
(`model`) or Codex (`codex_model`) pin uses the pin for the run's harness
unless the schedule applies its model to all agents. Your worker-model
setting does not override a subagent pin. **Chat always uses your Claude
lane**, whichever harness is your default.

To see what these rules give you right now, without starting a run, read the
read-only [effective settings](./configuration.md#effective-settings) view
(`uzi settings get`).

## Claude model guard and recovery

Saving the Claude worker lane (`default_claude_model`, or legacy
`default_model` when resolved to Claude) rejects these known Codex role IDs:
`gpt-5.6-sol`, `gpt-6-astra`, `gpt-6-luna`, `gpt-6-sol` and `gpt-6.1-sol`.
This is a closed list, not model-family guessing: valid custom Claude IDs
outside it remain accepted. The Codex lane's accepted models are unchanged.

Previously stored preferences are not migrated or cleared. Ordinary Claude
worker claims skip a stored lane value from this list and try to add a
fallback note to the run feed, using the existing fallback: the `lead`
template's model, then the SDK default. A compatible schedule or frozen per-run model
still takes precedence. Chat and jobs log a server warning and skip the
lane value, using their own runner defaults when no applicable override
wins. Frozen Claude schedule or legacy run models reject the same Codex role
IDs and fall back to the validated Claude lane or its default. Codex frozen
model acceptance and cross-check pin rules are unchanged.

In **Settings → Run defaults**, **Not a Claude model** identifies the
problem. Click **Reset to Default** to clear the draft, then **Save
defaults** to persist it. The warning and reset action also appear when
only Codex is usable and the Claude picker is hidden, because the grouped
save resends both lanes. `uzi settings get --json` still shows the stored
preference until you save a change; there is no new CLI command.

## Custom Codex model

Codex's dropdown offers the curated `gpt-6-astra`, `gpt-5.6-sol`,
`gpt-6-sol` and `gpt-6.1-sol`, plus **Other (custom model ID)**, the same escape hatch
Claude has always had. uzi validates the string for length and unsafe
characters but makes no provider call at save time; an unavailable or
unsupported ID only fails visibly on your first run, exactly like an
unrecognized custom Claude ID.

This custom lane applies only to your Codex **worker root** default, the
model your run itself executes on. It never reaches a schedule's pinned
model, a per-role agent-template pin, or the judge/summary models, which
stay on their own curated vocabularies.

A custom Codex default also needs a worker that advertises the
`codex_custom_model_v1` capability (curated Codex models need only the
ordinary Codex capability). Until one is online, a run with a custom Codex
default stays queued rather than silently falling back to `gpt-6-astra`;
see [Hosted workers](hosted-workers.md#my-codex-run-says-no-worker-supporting-custom-codex-models-is-online).

## Task review uses its own built-in model

Codex task review always runs on the built-in `gpt-6-sol`, independent of
your saved Codex worker model. It is not affected by your custom ID or
curated choice. Claude task review keeps its own ambient default the same
way. There is no setting to choose the task-review model in this release;
it's tracked as possible future work in [issue
#1570](https://github.com/vtmocanu/uzi/issues/1570).

## Per-schedule model

A [schedule](./scheduling.md) can run on its own model without changing
your worker-model defaults. This is handy for a cheap recurring bot (e.g. a
nightly "propose a feature" run on `fable`) while your interactive runs
stay on your normal model.

Set it in the schedule's create/edit form, in the **Model (optional)**
control, or with `uzi schedule create --model <alias|id>`. The modal groups
Harness, Model, Anthropic token and the subagent toggle in a **Where it runs:
harness, model & token** fieldset. A model pin is fixed when the schedule
fires; **Default** is read later, when a worker picks the run up. Leaving it
on **Default** uses your worker model for the schedule's harness, and the
label names the source rather than guessing. When your saved per-harness
worker model is known it is shown, for example `Default · opus (your Claude
default)`; otherwise it reads `Default · your Claude worker model` or
`Default · your Codex worker model`. It reads `Default · your worker model`
when the harness is not determined: automatic selection, or a schedule that
may overflow to the other harness when it fires (see [Which harness a
schedule runs on](./scheduling.md#which-harness-a-schedule-runs-on)). The
schedule field accepts a single token of at most 100
characters; blank means Default. The Codex schedule picker remains curated;
the custom Codex worker-root escape hatch does not extend to schedule pins.
The frozen model is readable with `uzi run get <id> --field model` (or in
`uzi run get <id> --json`); to see the model a run actually used, see [See
which model a run used](#see-which-model-a-run-used).

Tick **"Use the run's resolved model for subagents too"** (or pass `uzi
schedule create --apply-model-to-agents`; the flag name is unchanged) to
make the run's resolved model apply to every subagent too, not just the
lead, overriding each subagent's own model pin. With it off, a subagent
that has its own pin keeps it and the rest follow the lead. Reasoning effort
is not changed either way. This is useful for a cheap recurring bot
whose subagents would otherwise run on their own pinned (and pricier) models.
Whether a given run applied the model fleet-wide is readable with `uzi run
get <id> --field override_subagent_model` (or `uzi run get <id> --json`).
The run page shows a "model on all agents" badge when it is set; the human
`uzi run get` view has no row for it.

## See which model a run used

A run with a lead agent records the model and reasoning effort the lead was
asked to use, where each came from, and what the runtime reported back. This
covers issue, CI-fix, prompt, self-improve and MR-rework runs and task runs
that are not reviews. Review, cross-check, chat and job runs, and runs bound
to an egress profile (the isolated lane), have no lead agent here and show
nothing.

Each record is kept per claim attempt (every time a worker claims the run) and
per phase: `plan` while the lead plans, `implement` after approval. A run that
resumes after its plan was approved records only `implement` for the new
attempt; the earlier attempt's `plan` row stays in the history.

For each phase the record holds:

- **Requested** model and effort, with their **source**: *fixed by schedule*,
  *your Claude default* or *your Codex default*, *lead template*, or
  *built-in*. A Claude row that sent no model reads *Claude SDK default*. When
  you chose [Runtime default](./worker-effort.md#runtime-default) effort, uzi
  sends no level and the requested effort reads *runtime default*.
- **Runtime-reported** model and effort, with the evidence behind them
  (*Claude SDK init*, *Claude SDK settings*, *Codex thread config* or *sent to
  Codex*).

Where the runtime reports a different effort than requested, the display shows
both, for example `high (requested xhigh)`. A value not yet observed
reads *not reported yet*, whatever the attempt or run status, including on a
finished run. *Not reported by the runtime* means the runtime was asked and did
not say. An effort Claude reports as not sent reads *not sent*. See [Reasoning effort](./worker-effort.md#read-requested-and-effective-effort)
for how to read the effort side.

These rows are attested by the worker and are for display only. They never
feed billing, authorization or scheduling, and they describe the lead agent:
a subagent with its own Claude or Codex pin may differ.

Before a worker claims the run, nothing is resolved yet, so the chip reads
`lead: model and effort resolve at claim`, or, for a schedule-pinned model,
`lead: <model> · effort resolves at claim` with the model marked *fixed by
schedule*. A claimed run whose worker recorded nothing (an older worker, or a
run that ended before its first lead turn) reads `lead: not recorded`.

Where to see it:

- **Run page.** A `lead: <model> · <effort>` chip beside the harness badge.
  Hover it for the sources and runtime lines (a native tooltip); screen
  readers read them on focus. If the plan and
  implement phases differ, it reads `lead: plan <model> · <effort> →
  implement <model> · <effort>`.
- **`uzi run get <id>`.** `LEAD` and `LEAD_SOURCE` rows, shown only for runs
  with a lead agent. `uzi run get <id> --json` carries the full `lead_config`
  object.
- **`uzi run lead-config <id>`.** The history across every attempt; see
  [the CLI reference](./cli.md#lead-model-and-effort-history-uzi-run-lead-config).

## Good to know

- **Not verified at save time.** uzi doesn't check a custom model ID
  against the provider; an unrecognized one only fails on your first run,
  surfaced in that run's messages like any other agent error.
- **Validation.** A model must be a single token: trimmed, no interior
  spaces or control characters, at most 100 characters. Blank means
  Default (the picker's empty option).
- **Yours alone.** These settings only change runs you own; they never
  affect other users or the shared `lead` template.
- **Cost status stays honest.** Codex uses uzi's pinned Standard price table
  for both API-key and subscription runs. Unknown models, expired price rows
  or incomplete pricing evidence report `unreported`: tokens remain, with no
  per-run dollar figure. Legacy `subscription` rows mean no estimate was
  recorded. API-equivalent cost compares work, not a bill; subscription
  users may consume purchased credits. See [run cost](./run-cost.md#metered-subscription-and-unreported-cost).
- **Separate from the judge model.** The [run judge](./judge.md) runs on
  its own per-harness judge models (Claude `opus` and Codex `gpt-6.1-sol` by
  default), set per user or instance-wide by an admin. Your worker-model
  settings here have no effect on them.
- **Separate from reasoning effort.** Your [reasoning
  effort](./worker-effort.md), which controls how hard the model thinks,
  is set separately for Claude and Codex; see that page.

## Current Codex worker baseline

The current Codex worker uses runtime 0.166.0 and defaults to `gpt-6.1-sol`
when no schedule model, worker-model override or lead Codex pin is set.
Explicit model preferences remain unchanged. Both harnesses inherit medium
reasoning effort; Claude and Codex keep separate overrides, and each can
instead be set to **Runtime default**, which sends no effort level. See
[Reasoning effort](worker-effort.md).

Every Codex claim requires `codex_runtime_v2`, so an older worker cannot
silently use the old model/catalog or ignore the requested effort. Codex runs
queue until the worker image rolls, on hosted workers and compose alike.
