---
title: Anthropic token
order: 40
audience: user
---

# Anthropic token

uzi runs its agents with **your** Anthropic credentials, one token per user. You
paste the token once in **Settings, Anthropic token**; uzi stores it encrypted
and injects it into your agents when they run (that part lands in a later
release). This page explains how to obtain a token, which kind to use, and how
uzi keeps it safe.

## Which credential to use

You can give uzi either of two credentials. Prefer the first.

| Credential | How you get it | Best for |
|---|---|---|
| **OAuth token** (recommended) | `claude setup-token` (needs the Claude Code CLI and a Claude Pro or Max **subscription** login) | Anyone who already uses Claude Code; billed against your subscription. |
| **Console API key** | [console.anthropic.com](https://console.anthropic.com) then **API keys**, **Create key** | Anyone on usage-based API billing without a subscription. |

Both are pasted into the same field. uzi makes no assumption about the format
(it does not check for an `sk-ant-` prefix), so a valid credential of either
kind is accepted.

## Option A: mint an OAuth token with `claude setup-token`

1. Install the Claude Code CLI if you do not have it (see the
   [Claude Code docs](https://docs.claude.com/en/docs/claude-code/overview)).
2. Run:

   ```bash
   claude setup-token
   ```

3. The command opens your browser to sign in to your Claude subscription and
   authorize a long-lived token, then prints the token to the terminal.
4. Copy the printed token. Because the token is long-lived and printed to the
   terminal, treat your terminal scrollback and shell history as sensitive
   afterward (clear them if they persist the token).

`claude setup-token` requires a Claude subscription login. If you do not have a
subscription, use a Console API key instead (Option B).

## Option B: create a Console API key

1. Sign in at [console.anthropic.com](https://console.anthropic.com).
2. Open **API keys** and click **Create key**.
3. Copy the key when it is shown. The console displays it only once.

## Store it in uzi

1. Open **Settings, Anthropic token**.
2. Paste the token into the field and click **Save**.
3. The status flips to **Set** with the save date. The token itself is never
   shown again.

That is the whole flow, and it takes under two minutes.

### Rotate or remove

- **Rotate:** paste a new token and click **Save**. It overwrites the old one.
- **Remove:** click **Delete**. This disconnects uzi from your Anthropic account
  (removing an absent token is a no-op, so it is always safe to click).

## How uzi protects it

- **Encrypted at rest.** The token is sealed with AES-256-GCM before it is
  written to the database, keyed by the server's `UZI_SECRET_KEY` (see
  [configuration.md](configuration.md)). A database dump alone cannot recover
  it.
- **Never returned.** After you save it, no API response and no log line ever
  contains the token. uzi only ever reports metadata about it: its kind and the
  created and updated timestamps. There is no "reveal" button; to change it, you
  re-paste.
- **Yours only.** A token belongs to the user who saved it. No other user, admin
  included, can read its value.

## Two things to know

- **No verification at save time.** uzi does not call Anthropic when you paste
  the token, so it cannot tell you at that moment whether the token is valid.
  The token is exercised for real the first time one of your agents runs; a bad
  or expired token surfaces then. Re-paste a fresh one if that happens.
- **Key rotation invalidates stored tokens.** If an operator rotates
  `UZI_SECRET_KEY`, every stored token can no longer be decrypted and each user
  must paste theirs again. There is no re-encrypt path. This is the same
  accepted limitation documented for all secrets in
  [configuration.md](configuration.md).
