---
title: Worker model
order: 45
audience: user
---

# Worker model

Pick which model your own runs use, per harness — one setting for Claude runs
and a separate one for Codex runs, overriding the `lead` [agent
template](./agent-templates.md)'s model just for you. Other users' runs are
unaffected.

## Harness and worker models

Both settings live together in one **Settings → Run defaults** card:

- A **default harness** picker, shown only when you have a usable credential
  for both Claude and Codex. It decides which harness a run uses when
  nothing else names one.
- One **model lane per usable harness**: Anthropic and Codex side by side
  when both are usable, or a single lane when only one is. With no usable
  credential yet, the card still shows the Claude lane so you can
  preconfigure it before connecting one.

Switching the default harness never clears either model — each lane is its
own retained preference, so alternating between Claude and Codex keeps both
choices intact. The lane matching your effective default harness carries a
**Default harness** badge. Click **Save defaults** to save the harness and
both lanes together, in one request.

![Settings, Run defaults, showing the grouped Harness and worker models card with side-by-side Claude and Codex model lanes](img/worker-model-settings.png)

## Model precedence

For a given run, uzi resolves the harness first (an explicit run/schedule
choice, then your default harness, then whichever harness you have a
credential for), and only then reads that harness's model lane:

1. The **schedule's model**, if the run was started by a
   [schedule](./scheduling.md) that pins one compatible with the run's
   harness.
2. Your **worker model** for that harness, if set.
3. The harness fallback: the `lead` template's `model` (`opus` by default)
   for Claude, or `gpt-6-astra` for Codex.
4. Whatever the underlying SDK/your account defaults to.

An incompatible frozen schedule or legacy model (e.g. a Claude alias pinned
on a run that resolves to Codex) falls back to your lane for that harness,
with a visible fallback note on the run. A subagent with its own `model`
override always uses that, regardless of the schedule model or your
setting. **Chat always uses your Claude lane**, whichever harness is your
default.

## Custom Codex model

Codex's dropdown offers the curated `gpt-6-astra`, `gpt-5.6-sol` and
`gpt-6-sol`, plus **Other (custom model ID)** — the same escape hatch
Claude has always had. uzi validates the string for length and unsafe
characters but makes no provider call at save time; an unavailable or
unsupported ID only fails visibly on your first run, exactly like an
unrecognized custom Claude ID.

This custom lane applies only to your Codex **worker root** default — the
model your run itself executes on. It never reaches a schedule's pinned
model, a per-role agent-template pin, or the judge/summary models, which
stay on their own curated vocabularies.

A custom Codex default also needs a worker that advertises the
`codex_custom_model_v1` capability (curated Codex models need only the
ordinary Codex capability). Until one is online, a run with a custom Codex
default stays queued rather than silently falling back to `gpt-6-astra` —
see [Hosted workers](hosted-workers.md#my-codex-run-says-no-worker-supporting-custom-codex-models-is-online).

## Task review uses its own built-in model

Codex task review always runs on the built-in `gpt-6-sol`, independent of
your saved Codex worker model — it is not affected by your custom ID or
curated choice. Claude task review keeps its own ambient default the same
way. There is no setting to choose the task-review model in this release;
it's tracked as possible future work in [issue
#1570](https://github.com/vtmocanu/uzi/issues/1570).

## Per-schedule model

A [schedule](./scheduling.md) can run on its own model without changing
your worker-model defaults — handy for a cheap recurring bot (e.g. a
nightly "propose a feature" run on `fable`) while your interactive runs
stay on your normal model.

Set it in the schedule's create/edit form, in the **Model (optional)**
control, or with `uzi schedule create --model <alias|id>`. Leaving it on
Inherit uses your worker model for the schedule's harness. The model a
scheduled run actually used is shown on that run's detail page and by `uzi
run get`. Same validation as the worker-model lanes above (single token, at
most 100 characters; blank means inherit).

Tick **"Apply model also to agents"** (or pass `uzi schedule create
--apply-model-to-agents`) to make the schedule's model apply to every
subagent too, not just the lead — useful for a cheap recurring bot whose
subagents would otherwise run on their own pinned (and pricier) models.
Whether a given run applied the model fleet-wide is shown on that run's
detail page and by `uzi run get`.

## Good to know

- **Not verified at save time.** uzi doesn't check a custom model ID
  against the provider; an unrecognized one only fails on your first run,
  surfaced in that run's messages like any other agent error.
- **Validation.** A model must be a single token: trimmed, no interior
  spaces or control characters, at most 100 characters. Blank means
  inherit.
- **Yours alone.** These settings only change runs you own; they never
  affect other users or the shared `lead` template.
- **Cost status stays honest.** For an API key, usage on a model with no
  price row (any unrecognized custom Codex model) reports `unreported` —
  tokens are still counted, no dollar amount is invented. Subscription
  usage always reports `subscription`, with or without a price row.
- **Separate from the judge model.** The [run judge](./judge.md) runs on
  its own model, set instance-wide by an admin (`opus` by default) — your
  worker-model settings here have no effect on it.
- **Separate from reasoning effort.** Your [reasoning
  effort](./worker-effort.md) — how hard the model thinks — is shared
  across both harnesses; see that page.
