---
title: Registering an OAuth client
order: 67
audience: operator
---

# Registering an OAuth client

An admin makes an [external product](./product-tokens.md) an **OAuth client** so that, in place of a pasted `uzp_` token, it can send a user's browser to uzi to approve access (PRD #1910). The product's id is its `client_id`. Pasted product tokens keep working for every product, client or not.

> **Status.** This page covers registration (the redirect URIs, the allowed scopes and the client secret) and the consent step: a registered client can send a user to `/api/oauth/authorize`, the user approves or denies on uzi's `/connect` page, and the client receives an authorization code at its redirect URI together with its own `state` and uzi's `iss` (the instance's public origin). The token endpoint that exchanges the code ships in a later milestone of PRD #1910, so a client cannot finish a connection yet.

A product is an OAuth client only when it has all three of:

- at least one **redirect URI**,
- a non-empty **scope list** (`jobs:run`, `jobs:read`),
- a **client secret** (`uzs_…`).

## Register a client

All steps are browser actions under **Admin → Products**, on the product's card, in the **OAuth client** section. They are cookie-only admin writes: a CLI or `uza_` token cannot make them.

1. Open the section and enter the **redirect URIs**, one per line.
2. Tick the **scopes** the product may request, then **Save OAuth settings**. The two lists are saved together.
3. Click **Create client secret**. Copy it into the product now: it is shown once and only its SHA-256 hash is stored.

The section then shows **Client** once all three parts exist, and says what is still missing until then.

To stop a product being a client, clear every URI and every scope and save. The secret is kept, so registering it again later does not need a new one; rotate it if you want it gone.

## Redirect URI rules

A redirect URI is matched **exactly**, so register each one the product uses.

- `https` is required. `http` is accepted only for the loopback IP literals `http://127.0.0.1:<port>/…` and `http://[::1]:<port>/…`, with the port. `http://localhost` is refused.
- No fragment (`#…`) and no user info (`user:pass@`).
- The query may not use a name uzi adds to the redirect itself: `code`, `state`, `iss`, `error`, `error_description` and `error_uri`. Any other query parameter is kept as registered.
- At most 5 URIs, each at most 2048 bytes, printable ASCII only, no duplicates.
- Scopes must be a non-empty list drawn from `jobs:run` and `jobs:read` whenever there is a redirect URI. With no redirect URI the scope list must be empty too.

A request breaking a rule is a 400 and changes nothing.

## Rotate the client secret

**Rotate secret** replaces the secret at once; the old one stops working with no overlap, so update the product straight away. The list shows only the secret's first characters (for example `uzs_Qm4x…`) and when it was rotated, never the value.

A secret is `uzs_` plus 256 bits of randomness. Like uzi's other bearer credentials it is redacted from logs, issue drafts and CI-failure snapshots.

## From the CLI

`uzi admin products` shows a `CLIENT` column (`yes` when the product is a full client) and a `SCOPES` column. `--json` carries the `oauth_client` object (redirect URIs, scopes, `has_secret`, `secret_prefix`, `rotated_at`, `is_client`) and never the secret or its hash. Registration itself is browser-only. See [CLI](./cli.md#managing-tokens).

## API

For automation by an admin session (cookie and CSRF token, as the web app):

| Request | Effect |
|---|---|
| `PUT /api/admin/products/{id}/oauth` | Body `{"redirect_uris": [...], "scopes": [...]}`; both keys required. Returns `{"product": …}`. |
| `POST /api/admin/products/{id}/oauth/secret` | Rotates the secret. Returns `{"client_secret": "uzs_…", "product": …}`, the only time the value is shown. |

An unknown product is a 404 and a deleted product a 409, as for any product edit.
