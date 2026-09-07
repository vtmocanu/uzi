---
title: Codex credentials
order: 41
audience: user
---

# Codex credentials

**Settings → OpenAI / Codex credentials** lets you import an existing
**Codex CLI / ChatGPT-subscription login** into uzi. It is not an OpenAI
API key — that has its own field on the same card.

**Codex credentials do not yet start runs.** Saving one is dark: uzi stores
and seals it, but no run, worker, or judge can spend it. You still need an
[Anthropic token](./anthropic-token.md) for anything to actually run.
This page exists so that when Codex execution lands, your login is already
on file.

## What the field accepts

The **Codex login** field wants a flat JSON object, not a raw token:

```json
{ "access_token": "...", "refresh_token": "..." }
```

- **`access_token`** is the only thing required to save the credential.
- **`refresh_token`** isn't checked at save time, but uzi needs it later to
  renew the login once it expires — paste both together now rather than
  going back for the refresh token afterwards.

Do not paste a bare token string, and do not paste an OpenAI API key here —
use the **OpenAI API key** field beside it for that.

## 1. Sign in with the Codex CLI

```sh
codex login
```

Confirm it took:

```sh
codex login status
```

## 2. Get the value safely

The Codex CLI's own credential file, `~/.codex/auth.json`, is **nested** —
your tokens live under a `tokens` object alongside other fields:

```json
{
  "OPENAI_API_KEY": null,
  "auth_mode": "chatgpt",
  "tokens": {
    "access_token": "...",
    "refresh_token": "...",
    "account_id": "...",
    "id_token": "..."
  }
}
```

uzi wants the **flat** shape above, not the whole file — pasting the whole
file fails with "codex login must contain a non-empty access_token",
because the top level has no `access_token` of its own.

Use `jq` to project just the two fields you need straight to your
clipboard, so the secret never appears in your visible terminal output:

**macOS**

```sh
jq -c '{access_token: .tokens.access_token, refresh_token: .tokens.refresh_token}' ~/.codex/auth.json | pbcopy
```

**Linux (X11)**

```sh
jq -c '{access_token: .tokens.access_token, refresh_token: .tokens.refresh_token}' ~/.codex/auth.json | xclip -selection clipboard
```

**Linux (Wayland)**

```sh
jq -c '{access_token: .tokens.access_token, refresh_token: .tokens.refresh_token}' ~/.codex/auth.json | wl-copy
```

Paste the clipboard contents straight into the **Codex login** field.

### If `~/.codex/auth.json` is missing

If `~/.codex/auth.json` does not exist after `codex login`, your Codex CLI
is configured to store credentials elsewhere; check its docs for
file-backed storage.

## Keep it secret

The value you paste is a renewable credential, not a one-time code — treat
it like a password. Don't commit it, log it, paste it into an issue, or
share it in chat.

uzi seals it at rest the moment you save and never shows it again in the
UI, an API response, or a log — not even to you. If you need to change it,
use **Replace value** to paste a new one; there is nothing to reveal or
edit in place.

## Status legend

Each stored Codex credential shows one of four statuses:

| Status | Meaning |
|---|---|
| `staging` | Saved and encrypted; the provider identity has not been verified yet. This build has no automatic verification — a `staging` credential can stay `staging` indefinitely. |
| `linked` | The provider identity has been verified. |
| `failed` | Verification failed. Replace the value, or re-add the login. |
| `static` | An OpenAI API key, not a Codex login — provider-account linking doesn't apply to a key. |
