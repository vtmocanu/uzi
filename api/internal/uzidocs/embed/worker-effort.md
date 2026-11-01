---
title: Reasoning effort
order: 46
audience: user
---

# Reasoning effort

Choose reasoning effort independently for your Claude and Codex runs in
**Settings → Run defaults → Reasoning effort**. Both product defaults are
**medium**. Leave a control on **Inherit** to use that product default,
choose `low`, `medium`, `high`, `xhigh` or `max` for that harness, or choose
**Runtime default** to send no level.

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
2. Pick the **Claude effort** or **Codex effort** level, **Runtime default**, or **Inherit**.
3. Click **Save Claude effort** or **Save Codex effort**.

The controls offer five levels; Codex's `ultra` catalog level is deliberately
not offered. Lower levels favor speed and cost; higher levels reason longer.

Claude models that do not support a selected level can be silently
downgraded by their SDK. Codex passes the selected level directly to its
`turn/start` reasoning-effort parameter and never rewrites, clamps or retries
a level at another value. Before the first turn of an ordinary Codex lead
claim that carries an explicit level (Runtime default sends none), the worker
reads the Codex runtime's own catalog with one read-only `model/list` request
(limit 200, hidden models included, 2000 ms timeout). If the matched model's
entry does not list the chosen level, the run feed shows one warning per claim, before that claim's first turn: "Codex
does not list reasoning effort `<level>` for `<model>`. uzi sends it
unchanged; the provider may reject it. Choose a listed level or Runtime
default." The level is still sent unchanged, and a provider rejection fails the
turn through the normal failure path (the provider's message is not surfaced,
so a rejection of a listed level stays opaque). An unknown, unreadable,
oversized or timed-out catalog, or no matching entry, produces no warning. The
catalog is per-account and can refresh, so the warning is advisory and can be
wrong either way.

The [judge](./judge.md) uses the preference for its own harness. Task-review
and internal PR-description passes retain their existing SDK/catalog effort
behavior; their built-in Codex model remains `gpt-6-sol`.

There is no CLI effort setter: the write endpoint is cookie-only, so use the
web controls. `uzi settings get` reports the result (see
[Effort reporting](#effort-reporting)).

## Runtime default

Each harness also offers **Runtime default** beside **Inherit (uzi default:
medium)** and the five levels. The option is labelled **Runtime default · value
not reported** for Claude and **Runtime default · Codex decides** for Codex.
It is stored as the token `runtime` in the same preference (`default_effort`
or `default_codex_effort` on `PUT /api/me/settings`, cookie-only; `GET`
returns the stored token).

With Runtime default, uzi sends no effort: the Claude CLI gets no `--effort`
and Codex thread and turn requests carry no effort, so the runtime picks its
own level. This applies to ordinary issue and lead runs and their subagents,
the judge, chat, job and isolated runs. It differs from **Inherit**: an unset
preference still means an explicit medium. A stored value uzi does not
recognise falls back to medium and is never forwarded, and workers drop any
claimed default effort outside the five levels when decoding the claim,
without logging the value.

Settings do not report the level the runtime then uses; a run's lead record
shows what the runtime reported for that run (see [Read requested and
effective effort](#read-requested-and-effective-effort)). A resumed session or thread
may keep a level it was previously driven with: omitting effort does not clear
it.

Plan and Code cross-check checkers are independent of this choice. An unpinned
checker stays `high` (source `cross-check default`), and checker pins are
explicit levels that reject `runtime`. See
[Configuration](./configuration.md#plan-cross-check-model-and-effort-pins).

### Effort reporting

`GET` and `PUT /api/me/settings` return a read-only `effort_settings` with two
cells, `claude` and `codex`: `harness`, `effort` (stored), `default_effort`,
`default_source`, `resolved_effort`, `source` (`user_default` or `builtin`) and
`mode`. `resolved_effort` is `null` exactly when `mode` is `runtime_default`.
`uzi settings get` shows them as two `effort` rows; see
[CLI](./cli.md#plan-cross-check-settings).

### Rollback

Before rolling the API back below this feature, clear the token. The chart
runs one API replica with the Recreate strategy:

```sql
UPDATE users SET default_effort = NULL WHERE default_effort = 'runtime';
UPDATE users SET default_codex_effort = NULL WHERE default_codex_effort = 'runtime';
```

Affected users return to the unset (medium) behavior. The behavior of an older
API that reads the token is not specified.

## Read requested and effective effort

A run with a lead agent shows the effort it was **requested** to use and where
that came from, and what the runtime **reported** it applied. Both appear in
the run page's `lead:` chip, in the `LEAD` and `LEAD_SOURCE` rows of `uzi run
get`, and per attempt in `uzi run lead-config`; see [See which model a run
used](./worker-model.md#see-which-model-a-run-used).

- **Requested** is what uzi sent: your Claude or Codex effort override
  (*your Claude effort*, *your Codex effort*), or *built-in* when you left it on
  Inherit. With [Runtime default](#runtime-default) uzi sends no level, so the
  request reads *runtime default*, for example `lead: opus · runtime default`.
  The display shows that mode in place of a source; the stored record keeps the
  source (`user_default`), which `uzi run lead-config --json` returns.
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
