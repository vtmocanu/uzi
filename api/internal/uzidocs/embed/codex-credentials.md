---
title: Codex credentials
order: 41
audience: user
---

# Codex credentials

**Settings → OpenAI / Codex credentials** lets you import an existing
**Codex CLI / ChatGPT-subscription login** into uzi. It is not an OpenAI
API key; that has its own field on the same card.

A saved Codex login is what a Codex-harness run spends — see
[Worker model](./worker-model.md) for choosing Codex as your default harness
and its worker model. You still need an [Anthropic
token](./anthropic-token.md) for a Claude run; the two harnesses' credentials
are independent, so having one doesn't unlock the other.

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
avoids this shared-copy failure mode — uzi is a second, independent runner,
so it needs its own login, not a copy of yours.

Run this in a fresh shell. It signs in to a throwaway `CODEX_HOME`, verifies
the login actually produced a usable file-based credential, and copies only
the two fields uzi needs — never the whole file, never your real
`~/.codex/auth.json` — to your clipboard:

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
  (see the [Codex auth docs](https://developers.openai.com/codex/auth)),
  which would leave nothing at `$CODEX_HOME/auth.json` for the next step to
  read.
- The **`jq -e` check** verifies the isolated login actually produced a
  usable `chatgpt`-mode, file-backed credential before anything is copied,
  and fails loudly instead of silently copying `null`s. It never prints the
  tokens themselves.
- Only after that check passes does the second `jq` project the **flat**
  shape uzi wants (`{"access_token", "refresh_token"}`) out of the **nested**
  file the Codex CLI actually writes (tokens live under a `tokens` object
  alongside `auth_mode`, `account_id`, `id_token`, and more) — straight to
  your clipboard, so the secret never appears in your visible terminal
  output. Pasting the whole nested file into uzi is rejected, because the
  top level has no `access_token` of its own: the web field says it looks
  like the whole Codex `auth.json` file and asks for the flat object
  instead, and the API replies "codex login must contain a non-empty
  access_token".

**Do not delete `$CODEX_HOME` yet.** It's the only copy of this login until
you've confirmed the paste worked (the credential's status moves to
`linked` in uzi — see [Status legend](#status-legend) below). Once it has,
remove the temporary directory; don't reuse it, and don't reuse the login
it holds anywhere else.

If a managed credential-store policy on your machine overrides the `-c`
flag — the login doesn't produce a file-based `auth.json` and the `jq -e`
check fails — this recipe isn't available to you. Don't fall back to your
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
| `linked` | The provider identity has been verified. |
| `failed` | Verification failed. Use **Replace value** with a *new* [login dedicated to uzi](#1-sign-in-with-a-login-dedicated-to-uzi), never a re-paste of the old one. |
| `static` | An OpenAI API key, not a Codex login; provider-account linking doesn't apply to a key. |

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

A linked account's meter can land in a state that needs your attention:

- **No reading yet** — just linked, or waiting on the first poll; a reading
  appears within one poll interval.
- **Credential action required** — the login has expired with nothing left
  to renew it automatically, or the provider outright rejected uzi's refresh
  attempt (the hint on this state names which). Either way, use **Replace
  value** and paste a *new* [login dedicated to uzi](#1-sign-in-with-a-login-dedicated-to-uzi)
  from the recipe above — never a re-paste of the old one, and never a copy
  of your everyday Codex CLI's `auth.json`. `uzi rate-limits --provider
  codex` shows the same state as `credential_action_required (login
  rejected by provider)` when the provider rejection is the cause. If
  Codex usage polling is turned off, the state reads **Polling off**
  instead (`polling_disabled (login rejected by provider)` in the CLI), and
  the rejection hint still shows.
- **Vault locked** — uzi can't open the login to poll it until you unlock
  your vault again. The last known reading stays on screen, greyed and
  marked stale, rather than disappearing.
