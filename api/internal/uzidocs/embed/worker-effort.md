---
title: Reasoning effort
order: 46
audience: user
---

# Reasoning effort

Choose reasoning effort independently for your Claude and Codex runs in
**Settings → Run defaults → Reasoning effort**. Both product defaults are
**medium**. Leave a control on **Default · lead template's effort, else medium (built-in)** to use that default,
choose `low`, `medium`, `high`, `xhigh` or `max` for that harness, or choose
**Runtime default** to send no level.

Your explicit preference overrides the product default for the lead and its
subagents. An effort you choose when starting a run (see [A run-level
effort](#a-run-level-effort)) is frozen on the run and beats the setting.
For a run without one, the setting is resolved when a worker claims the run,
including a scheduled or already queued run and a resumed run; there is no
per-schedule effort freeze. Subagents without their own effort pin inherit
the run's effort (see [Subagent effort pins](#subagent-effort-pins)). Other
users and the shared lead template are unaffected.

## A run-level effort

Choose the effort for a single run when you create it: `uzi run create
--effort <level>` (`low`, `medium`, `high`, `xhigh` or `max`), or an optional
`"effort"` string on `POST /api/repos/{id}/runs`, or **Options** beside
**Start run** on the board or issue page (see
[Board](./board.md#starting-a-run-with-options); one-click Start run sends
none). It is checked
for shape at create (otherwise 400, no whitespace allowed), stored on the run
with the source **this run** (`effort_source: run_override`), and frozen:
requeues, reclaims and resumes keep it, and later changes to your Settings do
not touch it. It applies on both harnesses.

uzi does not check the level against the model. Claude may silently
downgrade an unsupported level and Codex may reject it; both show up in the
requested-versus-effective effort below. If a claim can no longer honor the
choice, the run parks `paused` on hold `run_override_unavailable` instead of
running at another effort; see [Worker
model](./worker-model.md#a-model-chosen-when-starting-a-run). Choosing only an
effort does not opt the run out of the automatic harness choice.

When upgrading from the shared setting, every existing explicit value is
copied into the Codex setting and retained for Claude. An unset or blank
preference uses the lead template's effort for that harness when it has one
(see [The lead template's effort](#the-lead-templates-effort)), otherwise the
built-in medium. Changing one harness's setting does not change the
other. Schema rollback keeps the Claude/shared value but loses independently
edited Codex preferences.

## Subagent effort pins

A [template](./agent-templates.md#reasoning-effort-pins) can pin an effort per
harness (`effort` for Claude, `codex_effort` for Codex). The rule for a
subagent is:

1. A pin for the run's harness is used.
2. An empty pin follows the run's resolved lead effort for the phase the
   subagent runs in.
3. When that is **Runtime default**, nothing is sent.

Only the pin for the run's harness is considered. A worker that predates
template effort never receives the pins and shows a run-feed notice; see
[Older workers](./agent-templates.md#reasoning-effort-pins).

Two limits apply on Claude. The SDK may silently downgrade a level the model
does not support, so a pin is a request, not a guarantee. And the lead model
can pass its own `effort` when it calls the Agent tool, which the SDK may
honour over the template's pin or the inherited level.

## The lead template's effort

The lead's effort for a harness resolves in this order:

1. The effort chosen for the run ([A run-level effort](#a-run-level-effort)).
2. Your per-harness preference: a level, or **Runtime default**.
3. The lead template's effort for the run's harness, shown with the source
   *lead template*.
4. The built-in **medium**.

On either harness, a configured [planning effort](#planning-effort) overrides steps 2 through 4 for the plan phase.

The template tier is lead-only: with an unset preference, everything outside the lead lane (judge, chat, jobs, isolated and review runs) uses medium. It applies only when your preference is unset or blank, so
choosing **Runtime default** keeps sending no level even when the lead template
pins one. It reaches the worker as the claimed default effort. `uzi settings get` and
`GET /api/me/effective-settings` show it with the source `template`. The
Codex catalog advisory below still applies to the lead only.

## Planning effort

Claude and Codex runs can plan at a different effort than they implement.
In **Settings → Run defaults → Planning**, each harness's planning effort
takes `low`, `medium`, `high`, `xhigh` or `max`; its empty option,
**Same as the run**, plans at the run's own effort. The shared
**Save planning defaults** button saves changed fields for both harnesses.
The empty option names the lead template tier when that supplies the run's
effort, for example **Same as the run · high (lead template)**, matching what
a run actually plans at.
There is no Runtime default choice for planning effort, and
the setting is written through `PUT /api/me/settings` (cookie-only) or that
card; no CLI command sets it. Phases, scope and the planning model are in
[Worker model](./worker-model.md#planning-model-and-effort).

- **Any combination is allowed.** A planning effort can be set while your
  implementation effort is **Runtime default**. When the run switches to
  implementation, uzi sends no effort and the runtime reverts to its own
  default for the model. This combination is accepted in either save order.
  Claude verifies the applied effort before the first implementation prompt,
  holding if it differs. Codex uses a cold resume of the same conversation;
  pinned 0.167.0 reports effort present as `null`. uzi sends no level and
  invents no numeric effective effort. A warm switch retaining the planning
  override is outside this allowance.
- **Claude defaults.** After approval the runtime default is the one
  characterized for the implementation model: `medium` for `opus`, `sonnet`
  and `haiku`, and `high` for `fable`. An unlisted model has no independently
  established default, so that switch holds. The model is the one the runtime
  reports, so a provider run with no implementation effort holds unless its
  provider maps the alias to one of those four characterized models.
- **A run-level effort wins.** An effort chosen when starting the run applies to
  both phases, and the planning effort never overrides it.

## Set an override

1. Open **Settings → Run defaults → Reasoning effort**.
2. Pick the **Claude effort** or **Codex effort** level, **Runtime default**, or **Default · lead template's effort, else medium (built-in)**.
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

With a separate planning pair, this one probe checks the first actual phase
(the planning pair on a gated run). Approval does not trigger another catalog
probe or warning for the implementation pair. Both phases still send their
requested effort unchanged.

The [judge](./judge.md) uses the preference for its own harness. The
handoff task review has its own per-harness effort, which follows your worker
effort until you set one (see [Task review
effort](./worker-model.md#task-review-effort)). Internal PR-description passes
retain their existing SDK/catalog effort behavior; the built-in Codex review
model remains `gpt-6-sol`.

From the CLI, `uzi settings set effort --harness claude|codex
<low|medium|high|xhigh|max|runtime>` writes the same preference and `uzi
settings reset effort --harness claude|codex` returns it to **Default · lead template's
effort, else medium (built-in)**; `runtime` is **Runtime default**, which is not a reset (see
[Changing model and effort settings](./cli.md#changing-model-and-effort-settings)).
`uzi settings get` reports the result (see
[Effort reporting](#effort-reporting)).

## Runtime default

Each harness also offers **Runtime default** beside **Default · lead template's
effort, else medium (built-in)** and the five levels. The option is labelled **Runtime default · value
not reported** for Claude. For Codex the label carries the catalog default last
reported for your credential, or says it is unknown (see [What the Codex catalog
default means](#what-the-codex-catalog-default-means)).
It is stored as the token `runtime` in the same preference (`default_effort`
or `default_codex_effort` on `PUT /api/me/settings`, cookie-only, or on
`PUT /api/me/model-settings`, which also accepts a CLI token; `GET`
returns the stored token).

With Runtime default, uzi sends no effort: the Claude CLI gets no `--effort`
and Codex thread and turn requests carry no effort, so the runtime picks its
own level. This applies to ordinary issue and lead runs and their subagents,
the judge, chat, job and isolated runs. It differs from **Default · lead template's effort, else medium (built-in)**: for
runs outside the lead lane (judge, chat, jobs, isolated and review runs) an unset preference still means an explicit medium, and
for the lead it means the lead template's effort when it has one, else medium. A stored value uzi does not
recognise falls back to medium and is never forwarded, and workers drop any
claimed default effort outside the five levels when decoding the claim,
without logging the value.

For Claude, settings report no level. For Codex they show the catalog default as
last reported, which a given run may still not use. A run's lead record
shows what the runtime reported for that run (see [Read requested and
effective effort](#read-requested-and-effective-effort)). A resumed session or thread
may keep a level it was previously driven with: omitting effort does not clear
it.

Plan and Code cross-check checkers are independent of this choice. An unpinned
checker stays `high` (source `cross-check default`), and checker pins are
explicit levels that reject `runtime`. See
[Configuration](./configuration.md#plan-cross-check-model-and-effort-pins).

### What the Codex catalog default means

With Runtime default uzi sends Codex no level, so Codex (pinned 0.164.0) uses
its model catalog's default level for the model. This was verified in the Codex
source at commit `6aaac694803004a63c6f393d153c96d22521a6b9` (paths under
`codex-rs/`):

- Request fallback, `core/src/client.rs:957-966` (`build_reasoning`):
  `.or_else(|| model_info.default_reasoning_level.clone())` then
  `.map(|effort| model_info.resolve_reasoning_effort(effort))`.
- Absent default reads `none`, `protocol/src/openai_models.rs:967-969`:
  `default_reasoning_effort: info.default_reasoning_level.unwrap_or(ReasoningEffort::None)`.
- `model/list` copies that preset value, `app-server/src/models.rs:39`:
  `default_reasoning_effort: preset.default_reasoning_effort,` (sent as
  `defaultReasoningEffort`).
- Only `Ultra` and `Persistent` are remapped before sending,
  `protocol/src/openai_models/reasoning_effort.rs:10-39`
  (`resolve_reasoning_effort`).

- `none` means no reasoning level is requested: either the catalog has no
  default for the model, or its default is `none`.
- A catalog default of `ultra` is not sent as-is; Codex substitutes another
  level.
- Repository `.codex/config.toml` files are ignored (Codex disables project
  config for untrusted checkouts, and uzi marks the checkout untrusted), so
  they cannot change the default.
- A resume that reattaches to a still-running Codex thread keeps that thread's
  level.

Workers report the catalog default when an issue or other lead Codex run starts
(from its `model/list` probe; cross-check, review, judge, chat, job and
isolated-lane runs do not report), once per claim and without retry. The
server stores it per user and per Codex credential (and, for a subscription,
account). Settings show the report for the credential a new run of yours would
use, if it is at most 30 days old. The Codex **Runtime default** option reads:

| Label after **Runtime default ·** | Meaning |
| --- | --- |
| `catalog default: {effort} ({model}, Codex {version})` | Known: the level the catalog default is, for your Codex model, as last reported. |
| `catalog default: none (no reasoning level requested) (...)` | Known, and the level is `none` (see above). |
| `catalog default: ultra (Codex substitutes another level) (...)` | Known, and the level is `ultra` (see above). |
| `... · may differ on workers still on another version` | Varies: workers on different Codex runtime versions reported different levels for the current credential. The label shows the highest runtime version's level (ties: the latest report). |
| `catalog default unknown until a Codex run reports it` | Unknown: no report within 30 days for the current credential, no usable Codex credential, an unlinked subscription, or Codex account mode `auto` with at least one pooled login (the login is chosen only when a run is created, so none can be predicted). With `auto` and an empty pool uzi uses the default credential, so the label can be known. |

A planning-model catalog report is stored under that planning model. Settings
filter reports by the implementation model as well as the credential, so a
different planning model cannot supply its label. A gated run that probes only
the planning model can leave the implementation model’s label unknown or older.

Saving a different Codex model refreshes the label. Because the value is a
report from an earlier run, it can lag a Codex runtime upgrade, or a catalog
refresh on your account (the default can change with no version or credential
change), until a new run reports. After a worker image rollback the newer
version's value can remain the label for up to 30 days. A replaced or
re-linked credential reads unknown until a run reports with it; a token refresh
alone keeps the label.

### Effort reporting

`GET` and `PUT /api/me/settings` return a read-only `effort_settings` with two
cells, `claude` and `codex`: `harness`, `effort` (stored), `default_effort`,
`default_source`, `resolved_effort`, `source` (`user_default` or `builtin`) and
`mode`. The `codex` cell also carries `catalog_default` (`state`, `effort`,
`model`, `runtime_version`, `observed_at`; the value fields are `null` when the
state is unknown). `resolved_effort` is `null` exactly when `mode` is `runtime_default`.
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
  **Default · lead template's effort, else medium (built-in)** and the lead template pins none. With [Runtime default](#runtime-default) uzi sends no level, so the
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

To see the effort your next run would use before it starts, read the read-only
[effective settings](./configuration.md#effective-settings) view
(`uzi settings get`).

## Worker rollout

Codex claims require a worker advertising `codex_runtime_v2`: the current
runtime/catalog baseline and correct claimed-effort handling. Until the
worker image is updated, Codex runs wait with the reason **no worker with the
current Codex runtime is online; waiting for the worker roll**. This applies
to hosted workers and local compose workers, including Codex judges and
reviews. Claude runs are unaffected by that gate.
