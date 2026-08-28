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

While you hold any token at all, you have exactly one default — uzi will not
let you end up with none. To move the default, click **Make default** on
another token; the badge moves in one step, with no window in which you have
none.

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

It is opt-in **per token**, and the pool starts empty. On **Settings →
Anthropic tokens**, tick **Auto-select from this token** on each one you are
happy for uzi to spend; `uzi token pool <name> --on|--off` does the same. The
pool is empty by default on purpose — one that helped itself to every
credential would spend the one you reserved for something else.

**Opting a token in does not guarantee it gets picked.** Beside the toggle,
each pooled token shows whether auto-selection could pick it *right now*:

| chip | what it means |
|---|---|
| **in pool** | it can be picked |
| **never polled** | uzi has never read a usage figure for it, so it cannot be ranked — it will never be picked |
| **no usage data** | it was polled, but the reading carried no percentage |
| **stale reading** | the last reading is too old to steer a choice |
| **low headroom** | it is nearly exhausted, so it is picked only if every pooled token is |

Those chips are the point rather than decoration: a token uzi cannot read a
usage figure for can never be chosen, and without the chip it would sit there
looking active. Check them after opting a token in.

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
can't pick but the pool isn't empty — nothing pooled has a usable reading —
it spends the **best pooled token anyway**, stale reading and all, rather
than reach outside the pool. Only when the pool is genuinely **empty** does
it stop spending anything: the run **holds**, waiting, until you opt a token
in (or you resume it yourself).

So a token you deliberately kept **out** of the pool is safe from ordinary
auto runs, full stop — that's the whole point of the toggle. The one thing
auto-selection does not do is fail a run outright: a thin pool still runs (on
whatever is pooled, however stale), and an empty pool waits rather than
errors.

The run view says which of these happened — see below.

### Waiting for a token

When an `auto` worker's pool has nothing at all to spend, the run doesn't
fail and doesn't reach for the default — it **holds**, showing "Waiting for
a pooled token" on the run view and `pool_wait` as its status everywhere
else. It resumes on its own the moment you opt a token into the pool, or you
can skip the wait with `uzi run resume-now <run-id>` or the run view's
**Resume now** button. This is a different wait than
[a usage-limit pause](run-limit-wait.md): a `pool_wait` hold means there was
nothing pooled to spend at all, not that a pooled token hit its rate limit.

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
you hold more than one token). It covers the [run judge](./judge.md) and
uzi's own self-improvement runs — retrospective work, which you may well want
billed separately from the runs being reviewed. Leave it on **your default
token** to keep everything on one account.

## Rotating a value

Rotation is **not** destructive and no longer replaces "the" token: it
replaces *one* token's value, in place, leaving its name, its default flag,
and every worker bound to it exactly as they were.

Under **Replace a token's value**, pick the token, paste the new value, and
click **Replace value**. The new value is used from the next run; nothing
else changes. Renaming is likewise safe — bindings follow the token, not its
name, so a rename never silently re-points a worker.

## Deleting a token

Click **Delete** on the token's row. Two rules:

- **Deleting a token unbinds; it never deletes a worker.** Any worker (or the
  judge setting) bound to it falls back to your default token from its next
  claim. The confirmation dialog says so, because a silent fallback is
  acceptable behavior but not acceptable surprise.
- **You cannot delete your default while other tokens exist.** Make another
  token the default first — the **Delete** button is disabled with a
  hint explaining why. Deleting your *last* token is allowed, and returns you
  to the disconnected state: no token, no runs.

## Good to know

- **Encrypted, never returned.** uzi seals each token at rest and never
  echoes it back in any response or log; see
  [ARCHITECTURE.md](../ARCHITECTURE.md#secrets-per-user-credentials-at-rest)
  for the mechanism and [the vault threat model](./vault-threat-model.md) for
  what that does and does not protect against.
- **Not verified at save time.** uzi doesn't call Anthropic when you paste
  a token, so a bad or expired one only surfaces the first time an agent
  actually runs against it. Replace its value if that happens.
- **Meters are per token.** Each stored token gets its own 5-hour and 7-day
  reading — see [Claude rate limits](./rate-limits.md).
- **The CLI can list tokens, and can move them in and out of the pool.**
  `uzi token list` prints names, default flags, pool opt-in and live
  eligibility; `uzi token pool <name> --on|--off` is the one write it has.
  Adding, renaming, re-defaulting and deleting are web-only. That is a deliberate boundary, not a gap: a CLI
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
  down this same Settings page; see [Theming](./theming.md).
