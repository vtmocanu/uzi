---
title: Registering an OAuth client
order: 67
audience: operator
---

# Registering an OAuth client

An admin makes an [external product](./product-tokens.md) an **OAuth client** so that, in place of a pasted `uzp_` token, it can send a user's browser to uzi to approve access (PRD #1910). The product's id is its `client_id`. Pasted product tokens keep working for every product, client or not.

> **Status.** This page covers registration (the redirect URIs, the allowed scopes and the client secret), the consent step, the token endpoint (code exchange and refresh) and the revoke endpoint. A registered client can send a user to `/api/oauth/authorize`, the user approves or denies on uzi's `/connect` page (see [Connecting a product](./connect-a-product.md)), and the client receives an authorization code at its redirect URI together with its own `state` and uzi's `iss` (the instance's public origin). It then exchanges the code at `POST /api/oauth/token` (below) for an access token and a refresh token, renews the access token with the refresh token, and can disconnect itself at `POST /api/oauth/revoke`.

## The token endpoint

`POST /api/oauth/token`, on the same origin as the rest of uzi, form-encoded, parameters in the body only (a query string is refused), at most 16 KiB. The client authenticates with **HTTP Basic** (`client_id` is the product's id, the password is its `uzs_` secret, both form-urlencoded as RFC 6749 section 2.3.1 says); sending the secret in the body as well is `invalid_request`. For `grant_type=authorization_code` send `code`, `redirect_uri` (the exact URI the authorization request used) and `code_verifier` (the PKCE verifier, 43 to 128 unreserved characters; only `S256` is supported). A parameter sent twice is `invalid_request`.

The 200 response is `access_token` (a `uzp_` token valid for one hour, used as a Bearer token on `/api/v1` exactly like a pasted product token), `token_type` `Bearer`, `expires_in` `3600`, `refresh_token` (a `uzr_` token) and `scope` (the approved scopes, space-separated). Every response carries `Cache-Control: no-store`.

- A code lives 60 seconds and works once. Presenting it again with the right client, redirect URI and verifier revokes the whole connection (RFC 6749 section 4.1.2); a replay with a wrong binding revokes nothing. A refused exchange never uses the code up.
- Failed or missing client authentication is **401 `invalid_client`** with `WWW-Authenticate: Basic`. Any other credential problem (unknown, used, expired or wrongly bound code) is 400 `invalid_grant`.
- A database error is **503 `temporarily_unavailable`** with `Retry-After`, never `invalid_grant`, so a client must treat a 503 as "try again", not "the connection is gone". If a 503 followed a commit that did land, the retry presents a used code and, being fully bound, counts as a replay and revokes the connection: start a new connection then.
- A connection already holding 10 live access tokens, or one for which 30 access tokens were minted in the last hour (revoked ones count, so refreshing and then revoking each new token does not get around it), answers **429 `temporarily_unavailable`** with `Retry-After` (seconds until a slot frees or the oldest mint leaves the hour): reuse your current token. Rate limits answer 429 with the same body.
- Limits are per client IP (every request) and per product (after authentication): `OAUTH_RATE_LIMIT_MAX` requests per `OAUTH_RATE_LIMIT_WINDOW`, 60 per minute by default. The per-product budget is one bucket shared by all of that product's users, so raise `OAUTH_RATE_LIMIT_MAX` for a busy multi-user product.

## Refresh an access token

When the one-hour access token expires (or sooner), `POST /api/oauth/token` with `grant_type=refresh_token`, the same Basic client authentication and request rules as the code exchange, and:

```
grant_type=refresh_token&refresh_token=<the uzr_ token>&scope=jobs:read
```

`scope` is optional: send a subset of the connection's current scopes to get a narrower token, never a wider one (`invalid_scope`); the connection itself is unchanged. The 200 response is `access_token`, `token_type`, `expires_in` and `scope`, and **no `refresh_token`**: uzi does not rotate refresh tokens, so keep using the one you hold. Because nothing rotates, a retried refresh, or several replicas of your product refreshing at once, are safe.

```
HTTP/1.1 200 OK
Cache-Control: no-store

{"access_token":"<a uzp_ token>","token_type":"Bearer","expires_in":3600,"scope":"jobs:run jobs:read"}
```

A refresh token is refused with 400 `invalid_grant` when it should no longer be used:

- it has not been used for 30 days;
- 90 days have passed since the user last approved the connection (approving again, through the normal consent page, starts a fresh 90 days and issues a new refresh token);
- the user or an admin revoked the connection, or the user approved again, which replaces the previous refresh token;
- it belongs to another client;
- the user's account is deactivated. Nothing is revoked, and reactivating the account makes the same refresh token work again, but a product may discard the connection meanwhile and ask the user to connect again.

On `invalid_grant`, stop using this refresh token and send the user through consent again.

A request is refused with **401 `invalid_client`** (with `WWW-Authenticate: Basic`) when your own client registration is the problem: the product is disabled or deleted, it is no longer registered as an OAuth client, or its allowed scopes no longer cover the connection's scopes. The connection is untouched, so **keep the refresh token** and retry after an admin fixes the registration. While the allowed scopes are narrowed, a refresh whose `scope` names only scopes you are still allowed (and the connection holds) succeeds with those; a refresh without `scope`, or naming a scope you are no longer allowed, is `invalid_client`.

A 503 or a 429 (a connection already holding 10 live access tokens or at its 30 mints an hour, with `Retry-After`) means try again later, not that the connection is gone.

## Revoke from the product

`POST /api/oauth/revoke` (RFC 7009), form-encoded, the same Basic client authentication, request rules, limits and `Cache-Control: no-store` as the token endpoint:

```
token=<the uzr_ or uzp_ token>&token_type_hint=refresh_token
```

`token` is required. `token_type_hint` (`refresh_token` or `access_token`) is advisory: a wrong or unknown hint still finds the token.

- Revoking the **refresh token** disconnects: the connection, every access token under it and the refresh token stop working at once, and jobs those tokens started are cancelled.
- Revoking an **access token** that the connection issued revokes only that token; the connection and its refresh token keep working. Revoking an access token, expired or not, also cancels the still-running jobs that token created.
- An unknown token answers **200** and revokes nothing. So does an access token that is already revoked, or expired and not yours, whichever product it belongs to; and so does a token a user pasted by hand into your own product, which was not issued through OAuth (the user revokes it). Your own expired access token answers 200 and is marked revoked.
- A **live** (not revoked, not expired) token of another client's product is **400 `invalid_grant`** and revokes nothing.
- 200 has an empty body. A database error is **503 `temporarily_unavailable`** with `Retry-After`: retry.

These endpoints are part of the `/api/v1` compatibility promise in [Product tokens](./product-tokens.md#compatibility-promise); `api/openapi/v1.yaml` declares an `oauth2` security scheme beside `bearerAuth`.

When too many authorization requests are waiting from one network, a new request is not stored and the user returns to your redirect URI with `error=temporarily_unavailable`; starting the connection again after a few minutes works, since a pending request lives 5 minutes. The limits are per application: 20 waiting requests per IPv4 address or IPv6 /64, 40 per IPv6 /56, and 100 per IPv4 /24 or IPv6 /48 (a 6to4 `2002::/16` address counts as the IPv4 address it embeds). Beyond those, an application can have at most 5000 requests waiting, and the instance 50000, to bound storage. A request that arrives while the application is busy with other requests can also be answered `temporarily_unavailable`; retrying works.

The network is the client address uzi sees. Behind a reverse proxy, set `TRUSTED_PROXIES` to that proxy's address so uzi reads the real client address from `X-Forwarded-For` (not on docker compose, where leaving it empty is the only safe setting; see [Auth design](auth-design.md)). With it unset (the docker compose default), every browser behind the proxy shares one network and so one 20-request limit for each application, as with the other per-address limits described in [Auth design](auth-design.md).

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

To stop a product being a client, clear every URI and every scope and save. The secret is kept, so registering it again later does not need a new one; rotate it if you want it gone. Narrowing or clearing the registration does not cut access at once: existing access tokens keep their scopes until they expire (at most one hour). Clearing it refuses every refresh; narrowing it refuses a refresh for any scope the product no longer has (a refresh whose `scope` names only scopes it still has succeeds). Disable the product to cut access immediately.

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
