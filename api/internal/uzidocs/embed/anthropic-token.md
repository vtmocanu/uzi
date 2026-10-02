---
title: Anthropic tokens
order: 40
audience: user
---

# Anthropic tokens

uzi runs your agents with **your own** Anthropic credentials. You can store
several, each under a name you choose, and point individual workers (and the
run judge) at a particular one. Everything that doesn't name a token spends
your **default**.

The token value itself is sealed at rest and is never shown again after you
save it — not in the UI, not in an API response, not in a log.

## Which credential to use

Prefer the first.

| Credential | How you get it | Best for |
|---|---|---|
| **OAuth token** (recommended) | `claude setup-token` (needs the Claude Code CLI and a Claude Pro/Max subscription login) | Anyone already on Claude Code; billed against your subscription. |
| **Console API key** | [console.anthropic.com](https://console.anthropic.com) → **API keys** → **Create key** | Anyone on usage-based API billing without a subscription. |

Both paste into the same field; uzi doesn't check for a particular prefix,
so either kind is accepted. Storing one of each is the common reason to hold
more than one token: subscription for the work, console key for the
retrospectives.

## 1. Mint a credential

- **OAuth token**: install the [Claude Code CLI](https://docs.claude.com/en/docs/claude-code/overview)
  if needed, then run `claude setup-token`. It opens your browser to sign
  in and prints a long-lived token to your terminal: copy it, then clear
  your terminal scrollback/history if it persists.
- **Console API key**: sign in at [console.anthropic.com](https://console.anthropic.com),
  open **API keys**, **Create key**, and copy it; the console shows it
  only once.

## 2. Store it in uzi

Open **Settings → Anthropic tokens**, paste it, and click **Save token**.
Your first token needs no name — it is stored as `default` and becomes your
default automatically.

![Settings, Anthropic tokens, showing the paste field and the stored token](img/anthropic-token-settings.png)

To store another, paste it in **Add another token**, give it a **Name**, and
click **Add token**. Names are yours to pick (`subscription`, `console-key`,
`team-billing`), up to 64 characters, and must be unique within your account
— they are compared case-insensitively, so `Console` and `console` are the
same name. A name is not a secret: it appears in the UI, in the CLI, and in
admin views. The value never does.

## The default token

Exactly one of your tokens is the default, marked with a **default** badge.
It is what runs whenever nothing more specific applies:

- every worker you have not bound to a particular token;
- every **chat** run, on any worker, bound or not (see below);
- the run judge, unless you point it somewhere else.

While you hold any enabled token, you have exactly one default — uzi will
not let you end up with none. To move the default, click **Make default** on
another token; the badge moves in one step, with no window in which you have
none. The default is always an enabled token; the one way to be left without
a default is to [disable](#disabling-a-token) every token you hold.

## Pointing a worker at a token

**Settings → Workers** lists each worker with the token it spends. With more
than one token stored, each row gets a picker: choose a name to bind it, or
**default token** to clear the binding.

The change takes effect on that worker's **next claim** — no restart, no
re-issued join token. The credential has never lived on the worker; it rides
each individual claim response, so re-pointing a worker is a server-side
change the worker learns about the next time it picks up work.

From the CLI:

```sh
uzi token list                                 # the names you can pass below
uzi worker set-token <worker-id> console-key   # bind
uzi worker set-token <worker-id> --default     # clear the binding
```

> **A bound worker's *chat* runs still spend your default token.** The
> binding covers the run lane — issue runs, autopilot runs, and CI-fix runs.
> In-app [chat](./chat.md) resolves your default on every claim, whichever
> worker serves it. So "worker `alpha` spends `console-key`" is a true
> statement about `alpha`'s *runs*, and chatting with an agent on `alpha`
> will still move the meter on your default token. If you are reconciling a
> meter against what you thought you spent, this is usually the reason.

## Letting uzi pick the token (auto-selection)

A worker can choose its token per claim instead of being pinned to one. Set
its picker to **Auto-select from the pool** (or `uzi worker set-token
<worker-id> --auto`) and each claim spends whichever of your **pooled**
tokens has the most rate-limit headroom.

Your **first (or only) token starts in the pool automatically** — a
single-token account gets auto-selection for free, nothing to opt in. That's
safe by construction: with one token, the pool and your default are the same
credential, so pooling it opens no new spend surface.

**Every token after that starts opt-in**, and stays out until you say
otherwise. On **Settings → Anthropic tokens**, tick **Auto-select from this
token** on each one you are happy for uzi to spend; `uzi token pool <name>
--on|--off` does the same. That's deliberate — a second token that helped
itself into the pool would spend a credential you reserved for something
else, which is the whole reason it starts out.

> **New workers default to auto-select when a pool exists.** Pin a worker to a token
> (above) and it stays **pinned**. Otherwise, a newly created worker — a
> [worker uzi
> auto-provisions for an unmet
> capability](./scheduling.md#auto-provisioning-a-worker-for-an-unmet-capability),
> a hosted worker you provision yourself, or one you join with a plain join
> token — comes up set to **Auto-select from the pool** as long as you have
> at least one pooled token, so a burst of new workers spreads across your
> pool instead of leaning on one credential. If your pool is empty at worker
> creation, the worker starts in **Default** mode instead. This is a
> creation-time choice, not a fallback for an **Auto-select** worker: an
> existing auto-select worker whose pool becomes empty holds its run and
> does not spend an unpooled default token.
> This is the default for **newly created** workers only: it does not
> retroactively re-point a worker you already set up, so an existing worker
> keeps whatever mode it already has until you change it yourself.

**Opting a token in does not guarantee it gets picked.** Beside the toggle,
each pooled token shows whether auto-selection could pick it *right now*:

| chip | what it means |
|---|---|
| **in pool** | it can be picked |
| **rejected** | the current value was rejected; auto-selection excludes it, including from the last-resort pooled-token floor |
| **never polled** | uzi has never read a usage figure for it, so normal ranking cannot rank it and passes it over — though, if it is pooled and nothing pooled has a usable reading, the last-resort floor can still spend it |
| **no usage data** | it was polled, but the reading carried no percentage |
| **stale reading** | the last reading is too old to steer a choice |
| **low headroom** | it is nearly exhausted, so it is picked only if every pooled token is |

These chips explain normal ranking. A pooled token without a usable usage
reading can still be selected by the last-resort pooled-token fallback
described below; a rejected token cannot. Check them after opting a token in.

Creation-time mode and claim-time fallback verified against worker creation
and `autoChoice` on 2026-09-08.

### How it chooses

Headroom is whichever window is fuller: `min(100 − 5-hour %, 100 − 7-day %)`.
The emptiest token wins. When two are within a few points of each other, the
one that **replenishes soonest** wins — and it is the reset of the window
that is actually holding it back, because a 5-hour reset does not relieve a
7-day cap. Ties beyond that are broken deterministically, so the same inputs
always give the same answer.

A small bias spreads work: each run already in flight on a token counts
slightly against it, so several claims arriving inside one polling interval
do not all pile onto the same credential. It is a nudge, not a cap — an empty
token still wins with a couple of runs on it.

A credential that just paused a run on a usage limit is excluded from that
run's next claim, so an `auto` worker doesn't immediately pick the token that
just refused it back up. Its meter also reads 100% for the window it hit as
soon as the pause happens, so every other run's claim ranks it as exhausted
too — see [Paused on a usage limit](run-limit-wait.md).

### "Auto" means "only my pool"

**An `auto` worker never spends a token you have not opted into the pool** —
not even your default, if you kept it out. If it can pick, it does. If it
can't pick but the pool has a non-rejected value with no usable reading,
it spends the **best non-rejected pooled token anyway**, stale reading and all,
rather than reach outside the pool. A rejected value is excluded even at this
last-resort floor. If the pool is empty or every pooled value is rejected,
the run **holds**, waiting for a spendable pooled token (or for you to resume
it yourself).

So a token you deliberately kept **out** of the pool is safe from ordinary
auto runs, full stop — that's the whole point of the toggle. The one thing
auto-selection does not do is fail a run outright: a thin pool can still run
on a stale, non-rejected value, while a pool with nothing spendable waits.

The run view says which of these happened — see below.

### Waiting for a token

When an `auto` worker's pool has nothing spendable (empty or only rejected
values), the run doesn't fail and doesn't reach for the default — it **holds**, showing "Waiting for
a pooled token" on the run view and `pool_wait` as its status everywhere
else. It resumes on its own once a spendable token is pooled (for example, after
you opt one in or replace a rejected value), or you can skip the wait with `uzi run resume-now <run-id>` or the run view's
**Resume now** button. This is a different wait than
[a usage-limit pause](run-limit-wait.md): a `pool_wait` hold means there was
no spendable pooled value, not that a pooled token hit its rate limit.
It is also different from [a transient-recovery
park](run-recovery-wait.md): `pool_wait` means there was nothing spendable in the pool, not that a resumed turn hit a transient interruption.

### Reading it back

Every run names the credential it spent **and why**, as a chip in the run
view and an `ANTHROPIC_TOKEN` row in `uzi run get`:

| what you see | what happened |
|---|---|
| `console-key — auto, 62% headroom` | auto-selection picked it; it had 62 points of headroom |
| `console-key — auto (best of pool), 8% headroom` | every pooled token was nearly exhausted, so it spent the least-consumed one |
| `console-key — pinned` | the worker is bound to this token |
| `console-key — default` | nothing named a token, so your default paid |
| `review-key — judge binding` | your judge setting chose it, not a worker's |
| `console-key — auto (pooled token, no fresh readings)` | no pooled token had a usable reading to rank, so auto floored onto this pooled token as a last resort — still one of yours, never your default |
| `console-key — auto (fell to another pooled token; the chosen one would not open)` | auto's first pick would not decrypt, so it floored onto this other pooled token instead — never your default (the run-view chip shortens this to `auto (fell to another pooled token)`) |
| `meta — default (auto: pool was empty — legacy)` | historical only, from a run claimed before this floor/hold behavior shipped, when an empty pool really did spend the default (`meta` here is that default). A new run never shows this: an empty pool [holds instead](#waiting-for-a-token) |

The two floor reasons are shown in amber and linked to this page, because
the worker is set to auto and its pool needed to fall back to a weaker
signal to keep the run going — worth knowing about, even though nothing
outside your pool was spent. Pool in a fresher token, or look at why the
readings are missing or unusable, to get back to an ordinary pick.

A run whose token you later delete still names it, with **(deleted)** — the
name is a snapshot taken when the run was claimed, so history stays readable.

Once you hold **more than one token**, the **Runs** list also shows a compact
version of this beside each run's status — the token's label, a small dot when
the state is worth a look (info or amber), and the mode and reason on hover — so
you can see which account paid across a batch of runs without opening each one.
A single-token user's runs all billed the one token, so the list stays as it was.

## Pointing the judge at a token

**Settings → Run judge** has a **Token the judge spends** picker (shown once
you hold at least one token), offering your default token, a token you name
for the judge lane specifically, or **Auto-select from the pool** — the
default. It covers the [run judge](./judge.md) and uzi's own self-improvement
runs — retrospective work, which you may well want billed separately from the
runs being reviewed. Leave it on **your default token** to keep everything on
one account.

## Choosing the token for one run

A worker's or the judge's binding is the default a run picks up, but you can
also choose the token for **one run**, or one scheduled job, without touching
either binding. The choice lives on the run itself, so a worker bound to
`console-key` can still spend `subscription` on one run you ask for.

Four modes:

| Mode | Meaning |
|---|---|
| **Inherit** (default) | No override — the run follows whatever the worker is bound to, exactly as today. |
| **Pin** to a named token | Spend that token, whatever the worker is bound to. |
| **Auto** | Auto-select from your pool for this run, even on a worker that is itself pinned or set to default. |
| **Default** | Spend your default token, even on a worker that is pinned or set to auto. |

`default` is not the same as `inherit` on a worker that isn't itself on
default — `inherit` follows whatever the worker is currently bound to (auto,
pinned, or default), while `default` always means your default token
regardless of the worker.

### Where you can pick it

- **Starting a run**: the token picker in the start dialog, or
  `uzi run create --token <label>|auto|default|inherit`.
- **A schedule**: the same picker in the schedule modal, or
  `uzi schedule create/edit --token <label>|auto|default|inherit`; every run
  the schedule fires inherits its choice.
- **At the plan gate**: a token picker next to the agent-source picker,
  defaulting to inherit and shown explicitly rather than left blank; the
  implementation phase runs on whatever you leave it at when you approve.
  `uzi run approve --token <label>|auto|default|inherit` switches and
  approves in one step.
- **On a run already underway**: the run page's **Switch token** action, or
  `uzi run set-token <run-id> <label>|--auto|--default|--inherit`. A queued
  run picks the choice up at its next claim; a run parked on
  [a usage limit, a pooled-token wait, or a transient recovery
  park](run-limit-wait.md) promotes straight back to `queued`; a running run
  or one held at the plan gate switches when the worker next releases its
  claim, losing at most the step in flight (the run page says so before you
  confirm). Switching to a token that looks tight can warn instead of refuse
  — see [Claude rate limits](rate-limits.md).

### Which choice wins

Only one thing ever outranks a per-run choice: the [run judge](./judge.md)
and uzi's own self-improvement runs always follow the **judge's** binding (see
[above](#pointing-the-judge-at-a-token)) — a run override never reaches them.
For every other run, the ladder is:

1. A per-run override, if one is set — pinned, auto, or default.
2. Otherwise, the worker's own binding.

So setting a per-run token never changes what the worker itself is bound to,
and clearing it (`--inherit`) hands the run straight back to that binding.

`uzi run get` shows the current choice in a **TOKEN** row (next to the
**ANTHROPIC_TOKEN** row for what the run actually spent), any switch that's
in flight, and — once a run has spent more than one credential — a
**TOKEN_HISTORY** of the switches that were actually applied; `--json`
carries the same as `credential_override`, `credential_switch` and
`credential_epochs`.

## Rotating a value

Rotation is **not** destructive and no longer replaces "the" token: it
replaces *one* token's value, in place, leaving its name, its default flag,
and every worker bound to it exactly as they were.

Under **Replace a token's value**, pick the token, paste the new value, and
click **Replace value**. The new value is used from the next run; nothing
else changes. Renaming is likewise safe — bindings follow the token, not its
name, so a rename never silently re-points a worker.

## Disabling a token

A token you have stopped using does not have to be deleted. Click
**Disable** on its card (beside **Rename**, **Make default** and **Delete**)
to put it aside, and **Enable** to bring it back. Disabling is a pause, not
a deletion: the stored value, the name, the pool opt-in and your sidebar
choice are all kept, and come back unchanged when you enable it again. It
does not revoke the token at Anthropic.

While a token is disabled:

- nothing new spends it: no new run, no chat, no judge or self-improvement
  run, and no scheduled fire;
- uzi stops checking its usage in the background, and it disappears from
  the sidebar meters, from `uzi rate-limits`, and from the admin **Rate
  limits** page;
- it is left out of the token pickers (starting a run, switching a run's
  token, the plan gate, the schedule dialog) and of the auto-selection
  pool, and the pickers say how many disabled tokens they are not listing;
- you cannot point anything new at it: binding a worker, pinning a run or a
  schedule, choosing it for the judge, adding it to the pool or making it
  the default are refused with a message that sends you to **Settings**.

What it has already done stays on record: past runs keep its name and their
usage and cost, as before.

**A run already using it finishes first.** Disabling stops future use; a
run that is already working with the token keeps it until that run is done
with it.

### The disable dialog

**Disable** opens a dialog that explains the above and lists what relies on
the token right now: workers bound to it (they will wait), schedules pinned
to it (their fires will be skipped), runs in flight (they finish first),
and whether the judge uses it.

If the token is your **default**, the dialog asks you to pick which of your
other enabled tokens becomes the default, and applies both changes
together. uzi never picks a replacement for you. If it is your **last**
enabled token, there is nothing to pick: the dialog tells you that you will
be left with no default token. Runs that rely on the default then fail, as
they would with no token at all, while chat, the judge and self-improvement
wait until you enable a token (a new run that doesn't name a harness may
start on Codex instead if you have a usable Codex credential).

### Where it goes

A disabled token moves into a **Disabled** section at the bottom of the
Anthropic tokens card, collapsed by default (your browser remembers whether
you expanded it). Each row shows the name, **Disabled since** and the date,
and **Enable** and **Delete** buttons. The pool and sidebar checkboxes are
hidden while it is disabled, not cleared.

When every token is disabled, a notice at the top of the card says you
have no default token, even with the Disabled section collapsed, and each
disabled row's button reads **Enable and make default**.

### Work that needs a disabled token waits

uzi never quietly spends a different token in its place, because that
changes which account pays. Work that needs this token specifically waits
instead:

- a run on a worker bound to it, or a run pinned to it;
- the judge or a self-improvement run when the judge is set to it, or when
  it uses your default and every token is disabled;
- a chat, when every token is disabled and you have no default.

A waiting run shows **waiting: credential disabled** on the run page, with
an **Enable** button for the token, plus **Run with another token** when
the page offers a per-run token switch (it does not for chats, judge and
self-improvement runs, task reviews or Codex runs; for those, enable the
token or change the default or the binding in Settings). The run resumes on its own
as soon as you enable the token, and the time it spends waiting does not
count against its time limit. In the CLI the run's status is `paused`, and
`uzi run get` prints a `HOLD` row reading `credential disabled` with the
same next step.

An auto worker is different: disabled tokens simply leave its pool. If the
pool ends up empty, its runs [wait for a pooled token](#waiting-for-a-token)
as usual.

### Schedules

A schedule pinned to a disabled token starts nothing when it comes due. Its
last fire records the skip **pinned credential is disabled**
(`credential_disabled`). A recurring schedule keeps its cadence and fires
normally again once you enable the token or change the schedule's token. A
one-time schedule is not used up: it stays due and fires once the token is
enabled or the schedule points at another one. A schedule that just uses
your default follows whichever token is the default at the time.

### Enabling it again

**Enable** brings the token back exactly as it was, except that it returns
as an ordinary token rather than the default (unless you had no default
left, in which case the button is **Enable and make default**). Its usage
meter reads "checking usage…" until a fresh reading arrives; a reading from
before you disabled it is never shown as current.

Disabling and enabling are web-only. The CLI shows the state: `uzi token
list` has a `STATE` column (`enabled`, or `disabled since` and the date),
and its `--json` output carries `enabled` and `disabled_at`.

## Deleting a token

Click **Delete** on the token's row. Two rules:

- **Deleting a token unbinds; it never deletes a worker.** Any worker (or the
  judge setting) bound to it falls back to your default token from its next
  claim. The confirmation dialog says so, because a silent fallback is
  acceptable behavior but not acceptable surprise.
- **You cannot delete your default while another enabled token exists.**
  Make another token the default first — the **Delete** button is disabled
  with a hint explaining why. Deleting your *last* token is allowed, and returns you
  to the disconnected state: no token, no runs.

## Good to know

- **Encrypted, never returned.** uzi seals each token at rest and never
  echoes it back in any response or log; see
  [ARCHITECTURE.md](../ARCHITECTURE.md#secrets-per-user-credentials-at-rest)
  for the mechanism and [the vault threat model](./vault-threat-model.md) for
  what that does and does not protect against.
- **Save does not wait for a verdict.** Saving or replacing a token requests
  a best-effort background usage poll, so a meter or rejection may appear soon
  afterwards; the save succeeds without waiting for that poll. Use **Test** on
  its card for an immediate check. Test tries Anthropic Usage first. If Usage
  returns an HTTP refusal and usage probing is enabled, it may send a small
  Messages request (about one output token); a Messages 401/403 marks the
  value rejected. With usage probing disabled, Test sends no Messages request
  and a Usage refusal is inconclusive. A rejected value is excluded from
  auto-selection, including its last-resort floor. Replace the value and test
  again; see [CLI testing](./cli.md#anthropic-tokens).
- **Meters are per token.** Each stored token gets its own 5-hour and 7-day
  reading — see [Claude rate limits](./rate-limits.md).
- **The CLI can list tokens, and can move them in and out of the pool.**
  `uzi token list` prints names, default flags, pool opt-in, live
  eligibility and whether each token is enabled; `uzi token pool <name>
  --on|--off` is the one write it has. Adding, renaming, re-defaulting,
  disabling, enabling and deleting are web-only. That is a deliberate boundary, not a gap: a CLI
  token is a bearer credential, and if it could mint or replace Anthropic
  credentials, a stolen one could swap out your account's credentials rather
  than merely read their names. See [the CLI guide](./cli.md#anthropic-tokens).
- **Key rotation resets everything.** If an operator rotates the server's
  master key, every stored token (yours included) must be re-pasted; see
  [configuration.md](./configuration.md).
- **Which model runs against it?** See [Worker model](./worker-model.md),
  further down the Settings page, to pick or override the Claude model your
  runs use.
- **Looking for the theme picker?** It's the **Appearance** section further
  down this same Settings page; see [Appearance](./appearance.md).
