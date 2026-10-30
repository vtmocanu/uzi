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
preference inherits medium. Changing one harness's setting does not change the
other. Schema rollback keeps the Claude/shared value but loses independently
edited Codex preferences.

## Set an override

1. Open **Settings → Run defaults → Reasoning effort**.
2. Pick the **Claude effort** or **Codex effort** level, or **Inherit**.
3. Click **Save Claude effort** or **Save Codex effort**.

The controls offer five levels; Codex's `ultra` catalog level is deliberately
not offered. Lower levels favor speed and cost; higher levels reason longer.

Claude models that do not support a selected level can be silently
downgraded by their SDK. Codex passes the selected level directly to its
`turn/start` reasoning-effort parameter; an unsupported model/level pair is
reported as a provider error rather than silently downgraded by uzi.

The [judge](./judge.md) uses the preference for its own harness. Task-review
and internal PR-description passes retain their existing SDK/catalog effort
behavior; their built-in Codex model remains `gpt-6-sol`.

There is no CLI settings command or effort setter today. Use the web controls;
the CLI's settings decode mirrors still carry both fields. A separate
**Runtime default** effort choice remains planned follow-up work (child B,
[#3247](https://github.com/vtmocanu/uzi/issues/3247), of [PRD
#3244](https://github.com/vtmocanu/uzi/issues/3244)); it is not available yet.

## Read requested and effective effort

A run with a lead agent shows the effort it was **requested** to use and where
that came from, and what the runtime **reported** it applied. Both appear in
the run page's `lead:` chip, in the `LEAD` and `LEAD_SOURCE` rows of `uzi run
get`, and per attempt in `uzi run lead-config`; see [See which model a run
used](./worker-model.md#see-which-model-a-run-used).

- **Requested** is what uzi sent: your Claude or Codex effort override
  (*your Claude effort*, *your Codex effort*), or *built-in* when you left it on
  Inherit.
- **Effective** is what the runtime said it applied. When it differs from the
  request, the chip shows both, for example `high (requested xhigh)`.
- **Claude** reports its effort when the SDK exposes it. On the pinned SDK it
  may read *not reported by the runtime*, which means the runtime did not say,
  not that the request was ignored. *not sent* means Claude reported no effort
  parameter for the turn.
- **Codex** reports the configuration of the thread: *sent to Codex* when uzi
  passed an effort on the turn, otherwise *Codex thread config* (the thread's
  configured effort, including one kept on a resumed thread). With no effort
  sent and none reported, it reads *not reported by the runtime*. This is the
  thread's configuration, not proof of what every provider turn used.
- An effort level uzi does not recognize reads *an unrecognized level*.

The record is attested by the worker and is display only: it never affects
billing, authorization or scheduling.

## Worker rollout

Codex claims require a worker advertising `codex_runtime_v2`: the current
runtime/catalog baseline and correct claimed-effort handling. Until the
worker image is updated, Codex runs wait with the reason **no worker with the
current Codex runtime is online; waiting for the worker roll**. This applies
to hosted workers and local compose workers, including Codex judges and
reviews. Claude runs are unaffected by that gate.
