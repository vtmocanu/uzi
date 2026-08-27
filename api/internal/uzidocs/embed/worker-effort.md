---
title: Reasoning effort
order: 46
audience: user
---

# Reasoning effort

Pick how hard the Claude Agent SDK reasons on your own runs — the lead
orchestrator and the subagents that inherit it. This overrides the SDK
default just for you. Other users' runs are unaffected.

## Effort levels

- `low`
- `medium`
- `high`
- `xhigh`
- `max`
- **Inherit** (the default)

**Unset means the SDK default (`high`).** When you leave it on Inherit, uzi
sends no effort setting at all, so the Claude Agent SDK's own default
(`high`) applies — exactly as before this setting existed.

## Good to know

- **Per-model silent downgrade.** `xhigh` and `max` are only honored on
  models that support them. If the model your run uses doesn't support the
  level you picked, the SDK **silently downgrades** it to that model's own
  highest supported level. uzi stores your choice verbatim and does not
  second-guess it — you may pick `max` and a given model quietly runs at
  its own highest supported effort instead.
- **Cost and latency tradeoff.** Lower levels (`low`/`medium`) are cheaper
  and faster; higher levels (`xhigh`/`max`) reason more deeply at higher
  cost.
- **Yours alone.** This setting only changes runs you own; it never affects
  other users or the shared `lead` template.
- **Separate from the worker model.** It is independent of your [Worker
  model](./worker-model.md) — see that page. There is no per-schedule
  effort override and no CLI setter; the Settings control below is the
  only place to change it.

## Set your reasoning effort

1. Open **Settings → Reasoning effort**.
2. Pick a level (`low`, `medium`, `high`, `xhigh`, `max`), or leave it on
   **Inherit**.
3. Click **Save effort**. It applies starting with your next run.

Leave it on **Inherit** to use the SDK default (`high`).
