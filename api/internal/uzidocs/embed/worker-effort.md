---
title: Reasoning effort
order: 46
audience: user
---

# Reasoning effort

Choose reasoning effort independently for your Claude and Codex runs in
**Settings → Run defaults → Reasoning effort**. Both product defaults are
**medium**. Leave a control on **Inherit** to use that product default, or
choose `low`, `medium`, `high`, `xhigh` or `max` for that harness.

Your explicit preference overrides the product default for the lead and its
subagents. The setting is resolved when a worker claims the run, including a
scheduled or already queued run and a resumed run; there is no per-schedule
effort freeze. Other users and the shared lead template are unaffected.

When upgrading from the shared setting, every existing explicit value is
copied into the Codex setting and retained for Claude. An unset or blank
preference inherits medium. Changing or clearing one harness's setting does
not change the other. Schema rollback retains the original Claude/shared
value but loses independently edited Codex preferences.

## Set an override

1. Open **Settings → Run defaults → Reasoning effort**.
2. Pick the **Claude effort** or **Codex effort** level, or **Inherit**.
3. Click **Save Claude effort** or **Save Codex effort**.

The controls currently offer five levels. Codex's additional `ultra` catalog
level is deliberately not offered. Lower levels favor speed and lower cost;
higher levels spend more reasoning time and can produce deeper answers.

Claude models that do not support a selected level can be silently
downgraded by their SDK. Codex passes the selected level directly to its
`turn/start` reasoning-effort parameter; an unsupported model/level pair is
reported as a provider error rather than silently downgraded by uzi.

The [judge](./judge.md) uses the preference for its own harness. Task-review
and internal PR-description passes retain their existing SDK/catalog effort
behavior; their built-in Codex model remains `gpt-6-sol`.

There is no CLI settings command or effort setter today. Use the web controls;
the CLI's settings decode mirrors still carry both fields. A separate runtime
default choice and effective effort reporting are planned follow-up work.

## Worker rollout

Codex claims require a worker advertising `codex_runtime_v2`: the current
runtime/catalog baseline and correct claimed-effort handling. Until the
worker image is updated, Codex runs wait with the reason **no worker with the
current Codex runtime is online; waiting for the worker roll**. This applies
to hosted workers and local compose workers, including Codex judges and
reviews. Claude runs are unaffected by that gate.
