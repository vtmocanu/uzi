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
- **The CLI can list tokens but not change them.** `uzi token list` prints
  names, default flags and timestamps; adding, renaming, re-defaulting and
  deleting are web-only. That is a deliberate boundary, not a gap: a CLI
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
