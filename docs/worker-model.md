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
harness label, model pin or model chosen at start, Anthropic credential
override or Plan/Code cross-check; other runs keep the usual resolution: an explicit harness, else
Claude). It picks the harness whose weekly allowance
resets soonest, falling back to Claude when usage is unknown; see [How the
automatic default picks a harness](./rate-limits.md#how-the-automatic-default-picks-a-harness).
The Run options dialog names the source for the harness too: its Default
option reads like `Default · Claude (your default harness)`, built from what
Default would pick right now, or `Default · chosen when started` when uzi picks
the harness at create. The schedule modal names the source as well (see [Which harness a schedule runs
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

1. A **model you chose when starting the run** (`uzi run create --model`, or
   `model` on the create-run API); see [A model chosen when starting a
   run](#a-model-chosen-when-starting-a-run).
2. The **schedule's model**, if the run was started by a
   [schedule](./scheduling.md) that pins one compatible with the run's
   harness.
3. Your **worker model** for that harness, if set.
4. The harness fallback: the `lead` template's Claude pin (`opus` by default)
   for Claude, or its Codex pin, else `gpt-6.1-sol`, for Codex.
5. Whatever the underlying SDK/your account defaults to.

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

## A model chosen when starting a run

You can choose the model (and, see [Reasoning effort](./worker-effort.md#a-run-level-effort),
the effort) for one run when you create it: `uzi run create --model <id>`, or
an optional `"model"` string on `POST /api/repos/{id}/runs`, or **Options**
beside **Start run** on a board card or issue page (see
[Board](./board.md#starting-a-run-with-options); one-click Start run sends
neither). The
choice is stored on the run and shown with the source **this run**
(`model_source: run_override` in `uzi run get --json`).

- **It wins.** It beats the schedule's model and your worker model for that
  harness, and is frozen: requeues, reclaims and resumes keep it, and later
  changes to your settings do not touch it. Subagents without their own
  model pin follow it; an explicit subagent pin still wins. A run started
  without one is unchanged.
- **Shape.** A single token, no whitespace anywhere (it is not trimmed), at
  most 100 bytes, no control or format characters (such as a bidirectional
  override); otherwise 400.
- **Checked against the run's harness at create.** Claude accepts a Claude
  alias or custom model ID but not a Codex model ID; Codex accepts only the
  curated models (`gpt-6-astra`, `gpt-5.6-sol`, `gpt-6-sol`, `gpt-6.1-sol`).
  An incompatible pair returns 422 with `code: model_incompatible_with_harness`
  and the resolved `harness`, and creates no run. With an explicit harness the
  check runs before the forge is read. uzi never substitutes another model.
- **Held, not substituted, if it stops working.** If a worker claims the run
  and the model is no longer honorable on its harness, the run parks
  `paused` with hold `run_override_unavailable` (`model choice unavailable`)
  rather than falling back to your lane the way an incompatible schedule
  model does. Resume retries only if the choice has become available;
  otherwise cancel the run and start a new one with a different choice. See
  [the CLI reference](./cli.md#waiting-for-a-state-uzi-run-wait).
- **Choosing a model opts the run out of the automatic harness choice.** A
  run with a model chosen at start is not eligible for the limit-aware
  **Default · automatic** harness choice or for overflow, just like a
  schedule model pin; it uses the usual resolution (an explicit harness,
  else your default, else Claude). An effort-only choice stays eligible. The
  Start preview (`POST /api/me/effective-settings/start-preview`) takes the same optional
  `model` and `effort` and applies the same rule; it returns `worker_model` /
  `worker_effort` with source this run, or the refusal
  `model_incompatible_with_harness`, and never starts a run.
- Successor runs do not inherit the choice, and chat and Slack start cards
  carry no model or effort.

## Planning model and effort

Claude and Codex runs can plan on one model and effort, then implement on
another: for example, Codex can plan on `gpt-6-astra` at `high` and implement
on `gpt-6.1-sol` at `medium`. Each harness retains its own optional per-user
planning model and effort. Left empty, they read **Same as the run**
and the run uses one model and effort throughout, as before.

- **Set them in the web app.** **Settings → Run defaults → Planning** has a
  model picker and an effort picker for each usable harness; the same
  **Save planning defaults** button saves the changed Claude and Codex fields
  together. The intro on the card explains the two phases. The empty option reads like
  `Same as the run · <model> (<where it comes from>)`.
- **Or through the API.** `PUT /api/me/settings` takes a planning model and a
  planning effort per harness: `planning_claude_model`,
  `planning_claude_effort`, `planning_codex_model` and `planning_codex_effort`
  (absent leaves it unchanged, `null` clears it, a value sets it). That
  endpoint is cookie-only; there is no CLI setter for planning settings yet. `uzi settings get` shows them (see [the CLI
  reference](./cli.md#effective-settings)).
- **Planning effort** takes `low`, `medium`, `high`, `xhigh` or `max`, with no
  runtime option; see [Reasoning effort](./worker-effort.md#planning-effort).

### What counts as planning

The planning pair applies to the lead's turns **before the plan is approved**:
the first plan, clarification rounds, revisions, and a re-plan after a later
claim (for example a resumed run that plans again). After approval the run
switches to its own model and effort, in the same session and conversation,
so the plan and everything said so far carry over. A re-plan switches back to
the planning pair for the new claim; each claim fixes its pair when the worker
claims the run, and a settings change takes effect at the next claim.

It applies to Claude and Codex lead-lane runs: issue, CI-fix, self-improve,
prompt, MR-rework and task runs. Planning stays on the run's harness. Review,
chat, judge, cross-check, job and egress-bound runs keep their existing lanes.

- **Frozen choices win in both phases.** A model chosen when starting the run
  or pinned by a [schedule](./scheduling.md), and an effort chosen at run start,
  apply to planning and implementation alike. Model and effort are decided
  independently: a frozen model still allows a planning effort, and vice versa.
- **Task (handoff) runs plan first.** A [task run](./cli.md) started with
  `uzi handoff` also has a plan turn before it implements, so with a planning
  model set it spends that turn on the planning pair. That turn costs as
  the planning model prices it, so a strong planning model makes each handoff
  more expensive than the same handoff on your worker model alone.
- **An empty Claude worker-model lane uses the template model.** With no worker model
  and the built-in `lead` template (`opus`), implementation uses the
  template's model, so the switch is from your planning model to `opus`.

### Unsupported configuration

A Claude planning model needs an implementation model to switch to. If you set a
planning model but there is no worker model and no template model, the run is
refused before any turn with "unsupported phase configuration": set a worker
model or clear the planning model. The refusal applies to every start or
resume, whether the run is new, already approved or resumed at its plan gate,
even if you would simply approve. It does not apply to provider runs.

### Which workers run it

Deploy the API before the workers. Separate planning settings require
`claude_phase_models_v1` for Claude or `codex_phase_models_v1` for Codex.
An effective custom Codex planning model also requires `codex_custom_model_v1`;
a curated planning model or effort-only setting does not add that requirement.
A curated frozen run model wins in both phases, so a saved custom planning
model does not add a custom-model requirement in that case.
Older workers leave the run queued. For otherwise eligible runs with curated
roots, missing phase support reads **no worker supporting separate planning
settings is online**. A custom effective root can show the custom-model health
reason when no worker supports both requirements;
see [Hosted workers](./hosted-workers.md#my-run-says-no-worker-supporting-separate-planning-settings-is-online).
A run that was queued on an incapable worker may stay with it for about two
minutes before another capable worker can take it.

### The switch is verified

For Claude, before the first turn after approval, the worker withholds the
prompt and asks the runtime which model and effort it has applied, then
compares them with
a table characterized on the pinned CLI. Only a match sends the prompt.

| Alias | Expected model | Effort when none is sent |
| --- | --- | --- |
| `opus` | `claude-opus-5-5` | `medium` |
| `sonnet` | `claude-sonnet-5-5` | `medium` |
| `haiku` | `claude-haiku-5-5` | `medium` |
| `fable` | `claude-fable-5-2` | `medium` |

A full model id, or one mapped by a provider setting, is compared verbatim.
Anything that cannot be verified holds the run (below); it never implements on
the planning pair. The same check applies to a reclaim of an approved run and to
a switch back to planning after a re-plan.

Known limits:

- A provider gateway that rewrites model ids can make a correct switch look
  like a mismatch and hold the run.
- The alias table was characterized with a dummy key; alias stability under
  subscription auth is not separately proven.
- With no implementation effort sent, the expected effort comes from the model
  the runtime reports. A provider run holds unless its provider maps the alias
  (`ANTHROPIC_DEFAULT_<ALIAS>_MODEL`, compared as given) to one of the four
  characterized model ids.
- The refusal of a planning model with no implementation model is skipped on
  provider runs, because the provider map always yields an explicit model or
  fails with `provider_model_unmapped`.

For Codex, approval cold-resumes the same conversation on the implementation
pair. Before an implementation turn, the worker compares the resumed thread's
reported model with the intended canonical model ID. A mismatch, missing model
or unsafe model value holds the run before the turn starts. The phase switch
has no fresh-thread fallback. Runtime default implementation effort sends no
level; on pinned Codex 0.167.0 cold resume, the reported effort is present but
`null`, not an inferred numeric level. This allowance does not cover a warm
switch that could retain a prior effort override.

### When the switch cannot be confirmed

The run parks `paused` with hold `phase_switch_unavailable` (**planning switch
unavailable**) and a feed line such as "Paused: the runtime kept the planning
model X instead of switching to Y. Resume retries on a new claim; Cancel ends
the run." **Resume** retries on a new claim, which verifies again; **Cancel**
ends the run. See [Pausing and resuming a run](./run-pause.md#held-when-the-planning-to-implementation-switch-cannot-be-verified)
and [the CLI reference](./cli.md#waiting-for-a-state-uzi-run-wait).

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

Safe custom IDs are accepted for Codex **worker and planning roots**. The
Codex lane rejects Claude aliases (`opus`, `sonnet`, `haiku`, `fable`);
use a canonical Codex model ID. These root settings do not widen the accepted
models for a run-start choice, schedule pin, per-role agent-template pin,
or judge/summary lane. The dedicated Plan cross-check checker pin retains its
separate custom-ID contract.

A custom Codex root also needs a worker that advertises the
`codex_custom_model_v1` capability. Curated models do not add this custom-model
requirement; separate planning settings still require `codex_phase_models_v1`.
Until a capable worker is online, a run with a custom Codex
default stays queued rather than silently falling back to `gpt-6-astra`;
see [Hosted workers](hosted-workers.md#my-codex-run-says-no-worker-supporting-custom-codex-models-is-online).

## Task review model

The review pass of a [handoff](./handoff.md#the-review--then-fix-loop)
(`uzi handoff --review`) has its own model per harness, set under **Settings →
Run defaults → Task review**. It is independent of your worker model: a custom
or curated worker choice never reaches the review.

| Harness | Accepts | Empty means |
| --- | --- | --- |
| Claude | Any Claude model id; Codex ids are refused. | The review sends no model, so the SDK decides. |
| Codex | One of the supported Codex role models. | The built-in `gpt-6-sol`. |

From the CLI, use `uzi settings set task-review-model <id> --harness
claude|codex` and `uzi settings reset task-review-model --harness
claude|codex` (see [CLI](./cli.md#changing-model-and-effort-settings)). The API
keys are `task_review_model` (Claude) and `codex_task_review_model` (Codex) on
`PUT /api/me/model-settings` and `PUT /api/me/settings`; `null` or a blank
value clears the setting.

- A stored model that later becomes invalid (for example a Codex model that
  leaves the supported list) falls back to the built-in at review time, and
  the effective view shows the built-in as the source.
- A worker older than this release does not read the setting. It runs the
  built-in review model, and the run feed records a
  `task_review_settings_omitted` notice.
- The server does not call the provider when you save. A configured model the
  provider cannot run produces the existing failed review.

The review's reasoning effort is set separately; see [Task review
effort](#task-review-effort).

## Task review effort

Each harness's review also has its own reasoning effort, next to the model under
**Settings → Run defaults → Task review**. It accepts `low`, `medium`, `high`,
`xhigh` or `max`, or **Runtime default** (`runtime`), which sends no level and
lets the runtime decide.

Left unset, the review follows your [worker effort](./worker-effort.md) for the
same harness (the Claude effort or the Codex effort), including a Runtime
default. If the worker effort is unset too, the built-in `medium` is sent.
**Behaviour change:** before this release no review effort was sent, so an owner
who sets nothing now has reviews send their worker effort (the built-in
`medium` when that is unset too).

From the CLI, use `uzi settings set task-review-effort
<low|medium|high|xhigh|max|runtime> --harness claude|codex` and `uzi settings
reset task-review-effort --harness claude|codex`; reset follows your worker
effort again, it does not pick a level. The API keys are `task_review_effort`
(Claude) and `codex_task_review_effort` (Codex) on `PUT /api/me/model-settings`
and `PUT /api/me/settings`.

- A run's one-run effort override does not affect the review.
- A stored value that is corrupt falls back to the built-in `medium`.
- A worker older than this release does not read the setting. The api withholds
  it, the run feed records a `task_review_settings_omitted` notice that names
  "task review effort" (including an inherited `medium`), and the review runs
  with no effort as before.
- The effective view resolves the review effort per harness; its note is
  `not_sent` only for Runtime default.

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
the custom Codex worker/planning-root escape hatch does not extend to schedule pins.
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

- **Requested** model and effort, with their **source**: *this run* (chosen
  when the run was started), *fixed by schedule*,
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

The current Codex worker uses runtime 0.169.0 and defaults to `gpt-6.1-sol`
when no schedule model, worker-model override or lead Codex pin is set.
Explicit model preferences remain unchanged. Both harnesses inherit medium
reasoning effort; Claude and Codex keep separate overrides, and each can
instead be set to **Runtime default**, which sends no effort level. See
[Reasoning effort](worker-effort.md).

Every Codex claim requires `codex_runtime_v2`, so an older worker cannot
silently use the old model/catalog or ignore the requested effort. Codex runs
queue until the worker image rolls, on hosted workers and compose alike.
