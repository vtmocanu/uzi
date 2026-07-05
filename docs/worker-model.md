---
title: Worker model
order: 45
audience: user
---

# Worker model

Pick which Claude model your own runs use, overriding the `lead` [agent
template](./agent-templates.md)'s model just for you. Other users' runs are
unaffected.

## Model precedence

For your runs (the lead orchestrator, and any subagent whose own template
leaves `model` unset), uzi resolves the model in this order:

1. Your **Worker model** setting below, if set.
2. The `lead` template's `model` (`opus` by default).
3. Whatever the Claude Agent SDK/your Anthropic account defaults to.

A subagent with its own `model` override always uses that, regardless of
your setting.

## Set your worker model

1. Open **Settings → Worker model**.
2. Pick a curated alias (`opus`, `sonnet`, `haiku`, `fable`), or choose
   **Other** and paste a full model ID.
3. Click **Save model**. It applies starting with your next run.

Leave it on **Inherit** (the default) to fall back to the `lead` template's
model.

## Good to know

- **Not verified at save time.** uzi doesn't check a custom model ID against
  Anthropic; an unrecognized one only fails on your first run, surfaced in
  that run's messages like any other agent error.
- **Validation.** A model must be a single token: trimmed, no interior
  spaces or control characters, at most 100 characters. Blank means inherit.
- **Yours alone.** This setting only changes runs you own; it never affects
  other users or the shared `lead` template.
