---
title: Codex credentials
order: 41
audience: user
---

# Codex credentials

**Settings → OpenAI / Codex credentials** lets you import an existing
**Codex CLI / ChatGPT-subscription login** into uzi. It is not an OpenAI
API key; that has its own field on the same card.

A saved Codex login is what a Codex-harness run spends; see
[Worker model](./worker-model.md) for choosing Codex as your default harness
and its worker model. You still need an [Anthropic
token](./anthropic-token.md) for a Claude run; the two harnesses' credentials
are independent, so having one doesn't unlock the other.

## Credential protection

OpenAI API keys and Codex logins share your Anthropic credentials' protection
choice in **Settings → Protect my credentials with my password (vault)**.
See [Vault choice](./anthropic-token.md#vault-choice) for On/Off, SSO passphrases,
restart unlocks and conversion limits. Off accounts do not need vault unlocks.

## What the field accepts

The **Codex login** field wants a flat JSON object, not a raw token:

```json
{ "access_token": "...", "refresh_token": "..." }
```

- **`access_token`** is the only thing required to save the credential.
- **`refresh_token`** isn't checked at save time, but uzi needs it later to
  renew the login once it expires, so paste both together now rather than
  going back for the refresh token afterwards.

Do not paste a bare token string, and do not paste an OpenAI API key here;
use the **OpenAI API key** field beside it for that.

## 1. Sign in with a login dedicated to uzi

**Don't paste your everyday Codex CLI login.** Codex *can* rotate the
refresh token on a refresh and rewrite `auth.json` in place when it does,
and OpenAI's own [Codex CI/CD auth guidance](https://learn.chatgpt.com/docs/auth/ci-cd-auth)
recommends one `auth.json` per runner: if your local Codex CLI and uzi share
one login, a refresh on either side can leave the other holding a stale
copy of a rotated token, and a re-paste from the file your local CLI keeps
using can break one side or the other again. A dedicated, isolated login
avoids this shared-copy failure mode. uzi is a second, independent runner,
so it needs its own login, not a copy of yours.

Run this in a fresh shell. It signs in to a throwaway `CODEX_HOME`, verifies
the login actually produced a usable file-based credential, and copies only
the two fields uzi needs (never the whole file, never your real
`~/.codex/auth.json`) to your clipboard:

**macOS**

```sh
( export CODEX_HOME="$(mktemp -d)"
  codex login -c cli_auth_credentials_store=file
  jq -e '.auth_mode == "chatgpt" and (.tokens.access_token | type == "string" and length > 0) and (.tokens.refresh_token | type == "string" and length > 0)' "$CODEX_HOME/auth.json" >/dev/null \
    || { echo "isolated login did not produce a usable file-based auth.json" >&2; exit 1; }
  jq -c '{access_token: .tokens.access_token, refresh_token: .tokens.refresh_token}' "$CODEX_HOME/auth.json" | pbcopy
  echo "Copied. Paste it into uzi, then remove $CODEX_HOME" )
```

**Linux (X11)**

```sh
( export CODEX_HOME="$(mktemp -d)"
  codex login -c cli_auth_credentials_store=file
  jq -e '.auth_mode == "chatgpt" and (.tokens.access_token | type == "string" and length > 0) and (.tokens.refresh_token | type == "string" and length > 0)' "$CODEX_HOME/auth.json" >/dev/null \
    || { echo "isolated login did not produce a usable file-based auth.json" >&2; exit 1; }
  jq -c '{access_token: .tokens.access_token, refresh_token: .tokens.refresh_token}' "$CODEX_HOME/auth.json" | xclip -selection clipboard
  echo "Copied. Paste it into uzi, then remove $CODEX_HOME" )
```

**Linux (Wayland)**

```sh
( export CODEX_HOME="$(mktemp -d)"
  codex login -c cli_auth_credentials_store=file
  jq -e '.auth_mode == "chatgpt" and (.tokens.access_token | type == "string" and length > 0) and (.tokens.refresh_token | type == "string" and length > 0)' "$CODEX_HOME/auth.json" >/dev/null \
    || { echo "isolated login did not produce a usable file-based auth.json" >&2; exit 1; }
  jq -c '{access_token: .tokens.access_token, refresh_token: .tokens.refresh_token}' "$CODEX_HOME/auth.json" | wl-copy
  echo "Copied. Paste it into uzi, then remove $CODEX_HOME" )
```

What each part is doing:

- The **subshell** (`( … )`) keeps `CODEX_HOME` scoped to this command only,
  so an existing `CODEX_HOME` in your real shell is untouched and back in
  effect the moment the command returns.
- **`-c cli_auth_credentials_store=file`** forces the login to write a
  file-based `auth.json`. Without it, macOS may use the OS keyring instead
  (see the [Codex auth docs](https://learn.chatgpt.com/docs/auth)),
  which would leave nothing at `$CODEX_HOME/auth.json` for the next step to
  read.
- The **`jq -e` check** verifies the isolated login actually produced a
  usable `chatgpt`-mode, file-backed credential before anything is copied,
  and fails loudly instead of silently copying `null`s. It never prints the
  tokens themselves.
- Only after that check passes does the second `jq` project the **flat**
  shape uzi wants (`{"access_token", "refresh_token"}`) out of the **nested**
  file the Codex CLI actually writes (tokens live under a `tokens` object
  alongside `auth_mode`, `account_id`, `id_token`, and more), straight to
  your clipboard, so the secret never appears in your visible terminal
  output. Pasting the whole nested file into uzi is rejected, because the
  top level has no `access_token` of its own: the web field says it looks
  like the whole Codex `auth.json` file and asks for the flat object
  instead, and the API replies "codex login must contain a non-empty
  access_token".

**Do not delete `$CODEX_HOME` yet.** It's the only copy of this login until
you've confirmed the paste worked (the credential's status moves to
`linked` in uzi; see [Status legend](#status-legend) below). Once it has,
remove the temporary directory; don't reuse it, and don't reuse the login
it holds anywhere else.

If a managed credential-store policy on your machine overrides the `-c`
flag (the login doesn't produce a file-based `auth.json` and the `jq -e`
check fails), this recipe isn't available to you. Don't fall back to your
everyday keyring login or your everyday `~/.codex/auth.json`; ask whoever
administers your Codex CLI installation for a way to get a file-based,
uzi-only login instead.

**A re-paste after a failed or expired login must come from a brand-new
isolated login, never the old `auth.json`.** Re-run the recipe above end to
end; don't go back to a `$CODEX_HOME` you already removed or already pasted
from.

### If the isolated login doesn't produce a usable `auth.json`

The `jq -e` check above prints "isolated login did not produce a usable
file-based auth.json" and exits non-zero. This almost always means a
credential-store policy is forcing keyring storage despite the `-c` flag
(see above); check your Codex CLI's docs for how storage is configured on
your machine.

## Keep it secret

The value you paste is a renewable credential, not a one-time code, so treat
it like a password. Don't commit it, log it, paste it into an issue, or
share it in chat.

uzi seals it at rest the moment you save and never shows it again in the
UI, an API response, or a log, not even to you. If you need to change it,
use **Replace value** to paste a new one; there is nothing to reveal or
edit in place.

## Status legend

Each stored Codex credential shows one of four statuses:

| Status | Meaning |
|---|---|
| `staging` | Saved and encrypted; the provider identity has not been verified yet. While Codex usage polling is on (the default), uzi verifies it automatically in the background on a read-only, non-rotating identity check, moving it to `linked` once that succeeds, or `failed` once it proves bad; a transient failure (a timeout, a 5xx) leaves it `staging` to retry. With polling turned off (`UZI_CODEX_USAGE_POLL_INTERVAL=0`, see [Codex account limits](rate-limits.md#codex-account-limits)) nothing verifies it, and it stays `staging`. |
| `linked` | The provider identity has been verified. Saved aliases of the same account share its temporary [account admission wait](rate-limits.md#temporary-account-admission-waits), including after refresh or re-import; linking does not promise immediate admission. |
| `failed` | Verification failed. Use **Replace value** with a *new* [login dedicated to uzi](#1-sign-in-with-a-login-dedicated-to-uzi), never a re-paste of the old one. |
| `static` | An OpenAI API key, not a Codex login; provider-account linking doesn't apply to a key. |

## Testing a credential

Click **Test** on a credential card for an immediate check, or use
[`uzi token test <label>`](./cli.md#anthropic-tokens). A result reports
`ok`, `rejected`, `permission_denied`, or `inconclusive`; it does not reveal
provider response bodies or the stored value. An inconclusive result can
carry `generic`, `vault_locked`, or `superseded` as its reason. A test after
replacement or disablement may be superseded by the newer credential state.

For a Codex login in `staging` or `failed`, Test runs identity reconciliation;
that check can move the link state to `linked` or `failed`. If the verified
account is quarantined or needs re-login, reconciliation can install a newly
verified replacement on it, as the background check does for a new login.
You can replace the value while automatic recovery is still in progress.
To resume held runs, supply the same ChatGPT account and workspace: uzi verifies
the replacement's identity, but a replacement for a different account cannot
resume those runs. For a `linked` login, Test reads the account's usage once and does not refresh or rotate the
login. It is a current check, not a promise that a later run can start.

For an OpenAI API key, Test requests `GET /v1/models`. An `ok` result means
exactly that the models endpoint was accessible with that key. It does not
prove inference access or billing readiness. A 401 is `rejected`; a 403 is
`permission_denied`. Other failures can be inconclusive. The key still has
no subscription usage meter.

## Usage meters

Once a login moves to `linked`, uzi starts polling its subscription usage in
the background and surfaces it everywhere Claude's token meters already
show up: the Settings → **Codex limits** card next to this one, the sidebar,
**Admin → Rate limits**, and `uzi rate-limits --provider codex` (plus its
admin CLI and TUI twins). See [Codex account
limits](rate-limits.md#codex-account-limits) for the full picture, including
choosing which additional accounts show in the sidebar and TUI.

**An OpenAI API key never gets a meter.** It has no subscription lifecycle
behind it, so Codex limits shows a short "subscription windows do not
apply" note next to it instead.

uzi tries automatic recovery after an uncertain renewal failure. A waiting run
may show **Codex account is reconciling** while that happens; let it recover,
and unlock your vault or enable the login if needed. If recovery cannot finish,
follow the re-login request on the run or credential card. The run's action is
separate from its usage meter: an uncertain exchange can leave the meter stale.
See [Codex account unavailable](run-recovery-wait.md#codex-account-unavailable).

A linked account's meter can land in a state that needs your attention:

- **No reading yet** — just linked, or waiting on the first poll; a reading
  appears within one poll interval.
- **Credential action required** — the login has expired with nothing left
  to renew it automatically, or the provider outright rejected uzi's refresh
  attempt (the hint on this state names which). Either way, use **Replace
  value** and paste a *new* [login dedicated to uzi](#1-sign-in-with-a-login-dedicated-to-uzi)
  from the recipe above, never a re-paste of the old one, and never a copy
  of your everyday Codex CLI's `auth.json`. `uzi rate-limits --provider
  codex` shows the same state as `credential_action_required (login
  rejected by provider)` when the provider rejection is the cause. If
  Codex usage polling is turned off, the state reads **Polling off**
  instead (`polling_disabled (login rejected by provider)` in the CLI), and
  the rejection hint still shows.
- **Vault locked** — your protected vault is locked, so uzi can't open the login to poll it until you unlock
  your vault again. The last known reading stays on screen, greyed and
  marked stale, rather than disappearing.

## Disabling a credential

A Codex login or OpenAI API key you have stopped using can be put aside
without deleting it: click **Disable** on its card, and **Enable** to bring
it back. It works the way [disabling an Anthropic
token](anthropic-token.md#disabling-a-token) does: the stored value, the
name and your sidebar choice are kept, nothing new spends it, uzi stops
checking and renewing it in the background, it disappears from the sidebar,
the pickers and the admin **Rate limits** page, and a run already using it
finishes first. It moves into the card's **Disabled** section, and the CLI
shows it in `uzi token list` with a `STATE` of `disabled since` and the
date.

A few things are specific to Codex:

- **Several logins can share one account.** If you saved more than one
  login for the same ChatGPT account, disabling one of them keeps the
  account live as long as another enabled login points at it: its meter
  keeps updating and the disabled row says so. The account goes quiet only
  when every login for it is disabled.
- **Disabling does not select a replacement.** A Codex run bound to a login
  you later disable waits, showing **waiting: credential disabled**, and
  resumes when you enable that login again. Eligible live runs also have an
  explicit owner switch; see [Switching a live run](#switching-a-live-run).
- **An account reset fails queued Codex runs created before it.** An
  admin-issued [account reset](auth-design.md) deletes your Codex logins. A
  Codex run created before the reset keeps its committed binding, whose login is now
  gone, so its next claim fails with "the bound Codex login is unavailable
  after your account reset; start a new run". A Codex run already executing
  when the reset lands is not stopped: it may finish on the credential its
  worker was already issued, but fails the same way if it needs another
  claim. Add a Codex login again and start a new run.
- **The default is shared.** Your Codex logins and OpenAI API key share one
  default. Disabling the default asks you to pick an enabled replacement;
  disabling the last enabled one leaves you with no Codex default, and a
  new run that doesn't name a harness may then start on Claude instead.
- **Enabling a login tries to renew it first.** When you enable a login
  again, uzi first tries to recover and renew it on its own, and only asks
  for a new paste (the **Credential action required** state above) if that
  fails. How long an unused login stays valid at OpenAI is outside uzi's
  control.

## Switching a live run

On an eligible live Codex run, the owner can choose an enabled, owner-owned
**Codex subscription** or **OpenAI API key** in the web credential picker,
or use its label from `uzi token list`:

```sh
uzi run set-token <run-id> <label>
```

This is an explicit choice: Codex does not accept `--auto`, `--default` or
`--inherit` here, and does not use Anthropic pool or worker-binding rules.
The server accepts `running` and worker-held `awaiting_approval`,
`awaiting_input` or `awaiting_followup`, then applies at the next safe
boundary. A claimed run returns 409. Queued runs and every park return 422,
including worker-held `limit_wait` and `recovery_wait`. Lane eligibility
is decided by the server. An unresolved policy episode rejects both request
and apply; changing credentials cannot consent to a policy continuation.
A busy refresh lease or refresh intent refuses the switch; retry as directed.

A successful switch keeps the worker, claim generation and worktree, without
an attempt, custody or reclaim transition. The new credential gets fresh
worker-owned authority and a fresh `CODEX_HOME`; only session data is copied,
not `auth.json`. The worker attempts the saved session ID. Only a confirmed
provider refusal permits a fresh thread in the same directory, with
`resume_lineage_break` / `credential_switch_resume_refused`; preservation
requirements still hold and can refuse the fresh thread.

If persistence fails before teardown, the old credential may continue only
after an exact-operation acknowledgement confirms non-application and the worker
proves it still owns the run. Unproven teardown keeps
the work and custody under `credential_switch_settling`, then
`credential_switch_unsettled`. An uncertain apply uses a plain owner pause
with a `credential_switch_uncertain` feed message. Neither old nor new
credential executes until the uncertainty is resolved. No release/reclaim
fallback is added.

### When a Codex run hits its weekly limit

Codex runs are **account-bound** for automatic selection: a run keeps the account it was created with, and a parked run does not move to another pooled account. A run parked on the 7-day window (or paused by the weekly reserve) therefore shows the reason `account_bound`, plus a fix list. Pooling another account (`uzi token pool --on --kind codex -- <label>`) or `uzi settings codex-mode auto` affects **future** runs only. For a live run, `uzi run set-token <run> -- <label>` applies only where the switch above does; queued and parked runs refuse it, so those fixes are not offered for a parked run. See [Why the run did not move to another token](run-limit-wait.md#why-the-run-did-not-move-to-another-token).

### Recovery and acceptance limits

Normal recovery after actual worker loss can create a new claim and checkout.
It uses the database-committed Codex binding, a new capability and epoch ordinal
0, rather than resolving the Anthropic selection ladder. The candidate saved
session ID comes from `claim.session_id`; path, rollout and provider checks
still decide whether it can resume. A fresh working directory has an explicit
lineage break. Missing session IDs now produce an explicit notice on retained
recovery. Preservation refuses a fresh directory or missing conversation
instead of silently starting over.

Crash recovery gives two separate guarantees. The server fences stale uzi
operations: a fresh claim increments the generation, remints the capability and
records the baseline, and stale applies and capabilities are refused. That does
not prove the old processes stopped. On the worker, same-worker retained
recovery runs the predecessor quiescence proof before the run starts. The
design property is that until it passes, the delivered Codex material (old or
new) is not used for login, provider start, refresh or release. The evidence is
narrower: same-worker retained recovery for an OpenAI API key switched to a
subscription, with release and refresh controlled before the proof; request
bodies, launches and requests are asserted only, and other credential
directions are uncredited. If the proof finds survivors or cannot be
verified, the run ends `failed` with the `quiescence_failed` blocker
(`worker_residue_blocked`) and keeps its clone and custody. That is a terminal
failure, not an owner pause.

If the predecessor's host is unreachable, uzi cannot prove it stopped, and
neither a different credential nor revoking the old uzi capability stops a
provider credential that predecessor already holds. This case is an
owner-recorded limitation, unvalidated and uncredited. The owner's stated
requirement is that the new credential is never used while the predecessor is
unproven. The [claim-provenance restriction](#a-codex-run-held-for-its-last-worker)
now refuses a fresh Codex claim on another worker, even after affinity expiry
or deletion of the previous worker. Missing or stale provenance also refuses a
fresh claim. This is not proof that the old process stopped: a replacement under
the same worker id can still claim while its predecessor incarnation runs, and
the local quiescence proof cannot see the other pod. That same-worker
new-incarnation gap remains unvalidated and uncredited. Claude cross-worker
reclaim and the predecessor's forge PAT remain outside this restriction
([ADR-4476](../adr/4476-codex-claim-provenance.md#residuals-claude-runs-and-the-forge-pat)).
The unreachable host may still hold the old credential, and the new one too if
the switch had already applied there.

If a worker host is lost, revoke or rotate every credential delivered to that host at the provider (OpenAI): the old one, and the new one if the switch had already applied there. For an API key, rotate the key; for a ChatGPT subscription login, sign out of or revoke its sessions. Removing a credential in uzi, or switching away from it, does not stop a copy an unreachable host already holds.

This is not a universal crash-resume or storage-durability guarantee.
Cross-account provider acceptance is **unproven**: evidence uses fakes, and
owner acceptance with two real accounts remains pending and non-closing.
See [ADR-4201](../adr/4201-in-place-credential-switch.md#codex-live-owner-switch-2026-11-15).

## Letting uzi pick the account

By default every new Codex run spends your default Codex login. If you hold
several ChatGPT subscriptions, you can let uzi choose among them when a run is
created, the way [Anthropic auto-selection](anthropic-token.md#letting-uzi-pick-the-token-auto-selection)
chooses among tokens. It is opt-in twice: you switch your account to **auto**,
and you opt each login into the pool.

### Turn it on

1. **Choose the mode.** Your **Codex account mode** is `default` (everyone
   starts here) or `auto`. Set it with the mode selector on the Codex card in
   **Settings**, or run `uzi settings codex-mode auto` (`uzi settings codex-mode default` to
   go back; `uzi settings get` shows the current value). It applies to runs
   created after the change.
2. **Pool the logins you are happy to spend.** On each Codex login, tick its
   pool checkbox in Settings, or run `uzi token pool <label> --kind codex --on`
   (`--off` takes it out). A login you save is **not pooled** until you do
   this. Only Codex subscription logins can be pooled; an OpenAI API key
   cannot, and a disabled login cannot be pooled (un-pooling one is allowed).

`uzi token pool <label>` without `--kind` works only while one pooled-capable
credential carries that label. If an Anthropic token and a Codex login share a
label it is a usage error naming `--kind anthropic|codex`; before Codex
pooling existed, the same command silently chose the Anthropic token.

### How it chooses

The choice is made once, **when the run is created**, and applies to every way
a run can start: a manual run, a [schedule](scheduling.md), an autopilot run, an
MR rework and a cross-check child. With mode `auto` and at least one pooled
login, uzi never spends your default unless it is pooled too.

- **Accounts, not aliases.** Logins that point at the same ChatGPT account
  count as one candidate; uzi binds the pooled, enabled, linked login of that
  account with the lowest internal id.
- **Left out entirely.** A login is never picked while it is not pooled, is
  [disabled](#disabling-a-credential), or is not `linked`, and neither is an
  account that is quarantined or needs re-login.
- **Held back.** An account is used only as a last resort, when nothing else is
  available, while its main `codex` limit bucket reports the limit reached or
  not allowed, while it is in a
  [temporary admission wait](rate-limits.md#temporary-account-admission-waits)
  (including its recovery spread), or while the [weekly usage
  reserve](rate-limits.md#weekly-usage-reserve) holds it.
- **Ranking.** Each remaining account scores `100 − (its fullest window)` from
  the main `codex` bucket of its [usage meter](#usage-meters); extra feature
  buckets are ignored. The score is unknown when the reading is stale, from
  before a reset, when the meter is not fresh, or when your vault is locked.
  Ranking then works as for Claude tokens. By default
  (`UZI_AUTOSELECT_ORDER=reset_first`) the account whose **weekly window resets
  soonest** wins among those with at least the minimum headroom, so allowance
  that would expire first is spent first; headroom (less the in-flight nudge)
  and then the account key break ties. The weekly window is the main `codex`
  bucket window that reports a duration of exactly 7 days, in either position
  (the sooner of the two if both do); an account with no known upcoming weekly
  reset sorts after every account that has one. The 5-hour window, when
  reported, still counts toward headroom and the floor and still decides when a
  limit-paused run resumes. If every measurable account is under the floor
  (`best_of_pool`), or the operator set `UZI_AUTOSELECT_ORDER=headroom_first`,
  the account with the most headroom wins, accounts within the tie band are
  decided by whichever resets soonest, and the account key breaks any
  remaining tie. The headroom floor, tie band, in-flight penalty and order
  (`UZI_AUTOSELECT_MIN_HEADROOM`, `UZI_AUTOSELECT_HEADROOM_TIE_PCT`,
  `UZI_AUTOSELECT_INFLIGHT_PENALTY`, `UZI_AUTOSELECT_ORDER`, see
  [Configuration](configuration.md)) are shared with Claude; how old a Codex
  reading may be follows the Codex poll interval instead. The tie band is
  ignored for `auto` picks under `reset_first`. See
  [ADR-3844](../adr/3844-reset-first-pool-order.md).
- **Spreading runs.** Every unfinished Codex run on an account, a queued one
  included and whichever saved login it uses, counts slightly against that
  account. Under `reset_first`, for picks above the headroom floor, that nudge
  only breaks exact weekly-reset ties,
  so reset-first deliberately concentrates runs on the soonest-expiring
  account until it falls under the floor, and a burst of runs created inside
  one poll interval (such as a sweep firing several issues) all lands on the
  soonest-expiring account, even when it sits just above the floor, which can
  mean more mid-run limit pauses and resumes. Set
  `UZI_AUTOSELECT_ORDER=headroom_first` to spread runs across accounts instead.

### What it records

The run keeps the reason it got its login. `uzi run get` shows it on the
`CODEX_ALIAS` row, the web run view and the TUI detail rail show it too, and a
run created before auto-selection reads `not recorded`.

| Reason | Meaning |
|---|---|
| `default` | Mode is `default`: your default login. |
| `pool_empty` | Mode is `auto` but no login is pooled: your default login. |
| `auto` | Picked with at least the minimum headroom to spare: the soonest weekly reset by default, the most headroom under `headroom_first`. |
| `best_of_pool` | Every measurable account was under the headroom floor; the emptiest was picked. |
| `pool_stale` | No pooled account had a usable reading; the first by account key was picked. |
| `pool_exhausted` | Every usable account was held back (limit reached, wait or reserve); the one releasing soonest was picked. |

### When no pooled account can be used

If you pooled logins but none can ever be bound (each is unlinked, disabled,
quarantined or waiting for re-login), Codex is treated as unusable for you,
and uzi never falls back to your default login:

- A run that did not name a harness starts on **Claude** when Claude is
  usable, as when you have no usable Codex credential.
- A run that explicitly asks for Codex is refused with a `422`, code
  `no_credential_for_harness` and reason `codex_pool_unavailable`
  ("link, re-authenticate or pool a Codex account in Settings"). The CLI
  prints the reason beside the message. If every Codex login you hold is
  disabled, the existing disabled-credential refusal applies instead (a `422`
  with no reason field).
- A [schedule](scheduling.md#fire-outcomes) that needs Codex starts nothing and
  records the skip `codex_pool_unavailable` (or `credential_disabled` when
  every Codex login you hold is disabled). A recurring one tries again next
  cadence; a one-time one is held, not used up, until you fix the pool.
- An automatic MR rework of a Codex run is skipped until a pooled account is
  usable again.

Accounts that are merely at capacity are not "unusable": a run is still created
on the one releasing soonest (`pool_exhausted`) and waits as any run does.

### A Codex run held for its last worker

Once a Codex run has been claimed, only the worker that last ran it may claim it
again. Another worker, including a new ephemeral one, is never offered the run,
even after the stale-requeue grace or the affinity ceiling would have released a
Claude run. A run that was never claimed is unaffected.

When a run is held, its health reason says so:

- "held for worker NAME: wait for it or cancel the run; only the worker that last
  ran this Codex run may resume it". Wait for that worker to come back (for
  example after a restart or a drain), or cancel the run with `uzi run cancel <id>`.
- "held for worker <name>: restart it or cancel the run; its previous process was
  released; only the worker that last ran this Codex run may resume it". Restart
  the recorded worker process so it registers with a new nonce, or cancel the
  run. Waiting for the same released process does not make it claimable.
- "held: predecessor provenance unavailable; cancel the run; no worker may resume
  this Codex run". No worker can resume it. This is the state of any Codex run at
  generation 1 or later when this rule was deployed (including a paused or parked
  one), or that was claimed while an older api was serving claims. Cancel it with `uzi run cancel <id>`.

A run that its own worker is still running in its current flight keeps going; the
hold applies at the next fresh claim. See [admin health](admin-health.md) for the
reasons and their alarm effect and [run recovery](run-recovery.md) for the upgrade
and rollback behaviour.

### What does not change

- **Auto-selection does not switch an existing run.** A resume, a usage-limit park
  or a restart keeps its committed account binding, and the
  [admission wait](rate-limits.md#temporary-account-admission-waits) and
  [reserve](rate-limits.md#weekly-usage-reserve) apply to that account. A
  [disabled](#disabling-a-credential) login still makes its runs wait.
- **Live switches require an explicit owner choice.** They do not use the pool;
  see [Switching a live run](#switching-a-live-run).
- **Out of scope:** pooling OpenAI API keys, and Codex chat. Neither is chosen
  by this mode.
