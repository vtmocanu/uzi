---
title: Product tokens
order: 96
audience: user
---

# Product tokens

A **product token** (`uzp_…`) lets an external product call uzi **as you**. You mint it in Settings for a product an admin has registered, and paste it into that product. Work the product starts runs on your own worker and model credential.

## What it can and cannot reach

- **Only `/api/v1`**, the stable external API. Everywhere else, including every `/api/*` route the web app and the `uzi` CLI use, `/api/ws` and every admin route, a product token is treated exactly like an unknown token. This is structural: product tokens live in their own table that the internal routes never read.
- **Today, one endpoint:** `GET /api/v1/whoami`, which returns your user (id, display name), the product, and the token's scopes. Job endpoints are not built yet, so the scopes below grant nothing beyond identifying the caller until they ship.
- **Never admin.** `/api/v1` drops admin authority for every caller, so a token an admin minted still acts as a plain user.
- **`/api/v1` also accepts your own `uzc_` CLI token** (user scope), for scripts. `uza_` tokens and browser cookies are refused there.

The `uzi` CLI cannot use a product token: it needs a CLI token (`uzc_` or `uza_`).

## Mint a token

1. Open **Settings → Access → Product tokens**.
2. Pick a product, name the token, choose scopes and an expiry, then **Create**.
3. Copy the token now. It is shown once and only its hash is stored.

Minting is a browser action only (no CLI or Bearer mint), so a stolen token cannot mint replacements.

**Scopes:** `jobs:run` (start and cancel jobs) and `jobs:read` (read status and results). Pick at least one; a token can never do more than its scopes.

**Expiry:** 30 days, 90 days (default), 1 year, or never. The server sets the timestamp. Choose "never" only for unattended products you trust, and watch its last-used entry.

**Cap:** at most **10 active** tokens (not revoked, not expired) per product for you. An eleventh mint is refused with a 409; revoke one first.

## Revoke a token

A token stops working on its **next request** when any of these happens:

| Action | Who |
|---|---|
| Revoke one token in **Settings → Access** | you |
| **Revoke all** in **Settings → Access** (revokes your CLI tokens **and** product tokens in one step) | you |
| Revoke one product token on **Admin → Products** | an admin |
| The product is disabled or deleted | an admin |
| Your account is deactivated | an admin |
| The token expires | automatic |

**A password change and logging out do NOT revoke product tokens**, exactly as for CLI tokens. If a token may have leaked, revoke it or use Revoke all.

Each row shows the token prefix, product, scopes, expiry, **last used** and **last IP** (written at most once a minute). There is no per-request audit log, so treat an unfamiliar last IP as a reason to revoke. Lists show active tokens first and say when they were cut off (200 rows for you, 1000 for admins).

## Register products (admins)

Product registration is an admin-only browser action under **Admin → Products**.

- **Register:** a name (unique among live products, case-insensitive, 200 bytes) and an optional one-line description (1000 bytes). Both are shown to every user, so control characters and invisible formatting are rejected.
- **Disable / enable:** disabling refuses every token of the product on its next request; enabling restores them.
- **Delete:** soft. All its tokens stop working, but their rows, last-used data and revoked state stay listed for the audit trail. A deleted product cannot be edited or re-enabled, and its name can be registered again. The confirm says how many tokens the delete stops (0 if it was already disabled).
- **Revoke one token:** each product card lists its tokens with owner, prefix, last used and IP; an admin can revoke a single compromised one. This does not change the rule that admins cannot revoke a user's personal CLI tokens.
- **Read-only from the CLI:** `uzi admin products` (needs a `uza_` token). See [CLI](./cli.md#managing-tokens).

## Compatibility promise

`/api/v1` is described by the checked-in OpenAPI 3.1 document `api/openapi/v1.yaml`, and a test keeps it identical to the router. Changes are **additive only**: new paths, new optional request fields, new response fields. Removing or renaming a path, field or enum value, or making an optional request field required, is breaking. A breaking change keeps the old shape working for **at least two minor releases and at least 90 days, whichever is later**, announced in the [changelog](./changelog.md), or ships under a new `/api/v2`. The internal `/api/*` routes are not covered: products must not call them.

`/api/v1` shares one per-user request budget (the general authenticated limit, 10 per minute by default); over it you get a 429 with `Retry-After`.
