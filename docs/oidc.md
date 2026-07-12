---
title: OIDC single sign-on
audience: operator
---

# OIDC single sign-on (PRD #45)

uzi can delegate login to a single OIDC identity provider — Keycloak (work) or
Pocket ID (homelab) are the two verified targets — instead of, or alongside,
its own email+password accounts. See [auth-design.md](auth-design.md#oidc-single-sign-on)
for the design rationale; this page is the operator setup guide.

## How it works

- Standard **Authorization Code + PKCE (S256)** flow, confidential client. Two
  endpoints: `GET /api/auth/oidc/login` (redirects to the IdP) and
  `GET /api/auth/oidc/callback` (code exchange, ID-token verification, session
  issuance).
- Setting `UZI_OIDC_ISSUER_URL` (with the client id/secret) turns on a
  "Sign in with {`UZI_OIDC_PROVIDER_NAME`}" button; email+password login stays
  available unless you also set `UZI_PASSWORD_LOGIN_ENABLED=false`.
- Once past the callback, an OIDC login is a uzi session exactly like a
  password login: same cookie, same lifetime (`AUTH_TOKEN_TTL`), same
  revocation. uzi does not store IdP tokens and does not track the IdP
  session, so the two are fully decoupled.
- **Logging out of uzi does not end your IdP session.** If Keycloak or
  Pocket ID still considers you signed in, clicking "Sign in with SSO" again
  logs you straight back in with no prompt — this is expected, not a bug. To
  actually sign out everywhere, log out of the IdP itself too.
- First login for an email uzi has never seen **JIT-provisions** a new user
  (subject to `UZI_REGISTRATION_ENABLED` and `UZI_ALLOWED_EMAIL_DOMAINS`); an
  existing password-registered account is instead **linked** by verified
  email. Either way the match requires the IdP's `email_verified` claim to be
  `true` — see [Enabling email verification](#enabling-email-verification-required)
  below.

## Environment variables

Full defaults and boot-validation behavior are documented in
[configuration.md](configuration.md#oidc-single-sign-on-prd-45); the essentials:

| Var | Default | Purpose |
|---|---|---|
| `UZI_OIDC_ISSUER_URL` | — (unset = OIDC off) | The IdP's issuer URL. Must be `https://` (plain `http://` only for a loopback host, for local dev). Setting this, together with the two below, turns OIDC on. |
| `UZI_OIDC_CLIENT_ID` / `UZI_OIDC_CLIENT_SECRET` | — | The confidential client's credentials from your IdP. All three of issuer/id/secret must be set together, or boot refuses. |
| `UZI_OIDC_SCOPES` | `openid profile email` | Space-separated. `openid` is always force-included. |
| `UZI_OIDC_PROVIDER_NAME` | `SSO` | Label on the login button ("Sign in with {name}"). |
| `UZI_OIDC_HTTP_TIMEOUT` | `15s` | Timeout on every outbound call to the IdP (discovery, JWKS, token exchange). |
| `UZI_PASSWORD_LOGIN_ENABLED` | `true` | Set `false` to go SSO-only (hides the password form, disables `/api/auth/register`). Refuses to boot if OIDC isn't configured — see [break-glass](#idp-outage-break-glass) below. |

The redirect URI is derived, not configured directly: `FRONTEND_ORIGIN +
/api/auth/oidc/callback`. Whatever you register at the IdP must match this
exactly.

## Fresh-instance admin pre-seeding

**Do this before you enable OIDC on a brand-new instance.** The first-ever
login — password or OIDC — becomes the admin, same rule either way. If you
turn on OIDC on a fresh instance with no `UZI_SEED_EMAIL` and no
`UZI_ALLOWED_EMAIL_DOMAINS` set, the first person anywhere in your
organization to click "Sign in with SSO" becomes the uzi admin, not you. Set
`UZI_SEED_EMAIL` (and `UZI_SEED_PASSWORD`) to pre-seed your own admin account,
and/or set `UZI_ALLOWED_EMAIL_DOMAINS` to your org's domain(s) so JIT
provisioning can't reach outside it — see
[configuration.md](configuration.md#access-control-prd-5).

## Keycloak walkthrough

1. In the Keycloak admin console, pick (or create) the realm you want to log
   into uzi from.
2. **Clients → Create client.** Set a Client ID (this becomes
   `UZI_OIDC_CLIENT_ID`). Turn **Client authentication** ON (this is what
   makes it a confidential client — uzi never uses a public client) and leave
   **Standard flow** ON; leave Direct access grants and Implicit flow off.
3. On the same wizard's Login settings step, set **Valid redirect URIs** to
   `<FRONTEND_ORIGIN>/api/auth/oidc/callback` (e.g.
   `http://127.0.0.1:8080/api/auth/oidc/callback` for the bundled compose
   default; use your real origin in production). Save.
4. **Credentials** tab → copy the **Client secret** → `UZI_OIDC_CLIENT_SECRET`.
5. Set `UZI_OIDC_ISSUER_URL` to `https://<keycloak-host>/realms/<realm-name>`
   (Keycloak's issuer is the realm URL; `/.well-known/openid-configuration`
   is fetched relative to it).
6. **Enable email verification (required)** — see below.
7. Restart uzi so `config.Load` picks up the new env vars, then click
   "Sign in with SSO" from the uzi login page.

No separate PKCE setting to flip here: Keycloak accepts the S256 challenge
uzi always sends opportunistically, with no extra client configuration —
confirmed working out of the box.

## Pocket ID walkthrough

1. In the Pocket ID admin panel, go to **OIDC Clients → Add OIDC Client** (opens
   the "Create OIDC Client" form).
2. Name the client and set its **Callback URL** to
   `<FRONTEND_ORIGIN>/api/auth/oidc/callback`.
3. Pocket ID generates a **Client ID** and **Client Secret** for you — copy
   both into `UZI_OIDC_CLIENT_ID` / `UZI_OIDC_CLIENT_SECRET`.
4. Set `UZI_OIDC_ISSUER_URL` to your Pocket ID instance's own base URL (Pocket
   ID has no separate realm path).
5. **Passkey note**: Pocket ID's own login is passkey-first. That's entirely
   between the user and Pocket ID — uzi only ever sees the ID token Pocket ID
   issues once the user has completed whatever login method Pocket ID asked
   for, so nothing here changes on uzi's side.
6. **Enable email verification (required, and NOT the default)** — see below.
7. Restart uzi, then click "Sign in with SSO" from the uzi login page.

## Enabling email verification (required)

Both IdPs expose a per-user `email_verified` flag, and **both default it to
unverified**. uzi refuses to link or JIT-provision an unverified email — a
sloppy IdP letting anyone register `victim@yourcompany.com` unverified would
otherwise be an account-takeover vector — so an unflipped flag sends every
login from that user to `/login?error=oidc_forbidden`. There is no override.

- **Keycloak**: Users → select the user → Details tab → toggle **Email
  verified** ON, Save.
- **Pocket ID**: either flip the **Emails Verified** setting under
  Application Configuration so new accounts start verified, or have the user
  complete Pocket ID's own email-verification flow. Either way flips the
  per-user flag that uzi checks.

## Development setups

Running `api` in a container while the IdP listens on the **host's**
loopback (`127.0.0.1`) does not work: inside the container, `127.0.0.1` is
the container itself, not the host, so the discovery dial refuses (observed:
`oidc discovery failed at boot; SSO is configured but degraded` followed by
`dial tcp 127.0.0.1:8081: connection refused` in the API logs, and every
login attempt bouncing to `oidc_exchange`). Pointing `UZI_OIDC_ISSUER_URL` at
a container-reachable host alias instead (e.g. `http://host.docker.internal:8081`)
doesn't work either — by design the issuer must be `https://` except for a
loopback host, and `host.docker.internal` isn't loopback. Two topologies
actually work for local development: run **both** `api` and the IdP on the
host's own loopback with no container boundary between them, or give the dev
IdP a real `https://` endpoint. Production is unaffected by any of this — the
issuer there is always `https://`.

**TLS**: uzi verifies the IdP's certificate against the container's system
trust store; there is no custom-CA configuration knob. A self-signed dev
certificate will not verify unless that CA is added to the `api`
image/container's own trust store.

## IdP outage: break-glass

If `UZI_PASSWORD_LOGIN_ENABLED=false` (SSO-only) and the IdP is unreachable,
nobody can log in — a known trade-off, not a bug. The seed admin
(`UZI_SEED_EMAIL`) still has a `password_hash` even in SSO-only mode, so
recovery is: set `UZI_PASSWORD_LOGIN_ENABLED=true` and restart. This
re-enables the password form (and `/api/auth/register`) without touching
anything OIDC-related, and the seed admin can log in with their password
while the IdP is down.

## Troubleshooting

**OIDC discovery failing at boot does not crash the API.** A misconfigured or
unreachable IdP at startup leaves OIDC **configured-but-degraded**: the SSO
button stays visible (so it doesn't vanish just because the IdP blipped), and
every login attempt retries discovery on the spot. Check the API logs for a
loud discovery-failure line if the button is there but every click bounces
back with `oidc_exchange`.

**A misconfigured redirect URI usually never reaches uzi at all.** Keycloak
rejects it at its own authorize step with an IdP-branded error page ("We are
sorry ... / Invalid parameter: redirect_uri") before ever redirecting back —
uzi's callback is never hit, so no `?error=` code appears. If you land on a
page like that instead of back on uzi's `/login`, check that the redirect
URI registered at the IdP is exactly `FRONTEND_ORIGIN + /api/auth/oidc/callback`.

Callback failures that uzi itself does see redirect to `/login?error=<code>`
with one of a fixed set of codes (the raw error never reaches the URL or the
browser):

| Code | Meaning |
|---|---|
| `oidc_state` | The state cookie was missing, wouldn't decrypt, or didn't match — usually a stale/expired attempt (the cookie lives 10 minutes) or a cookie blocked by the browser. Retry from the login page. |
| `oidc_exchange` | Discovery, the token exchange, or ID-token verification failed — e.g. the IdP went unreachable between login and callback, the client secret is wrong, or a clock-skew/nonce mismatch at the token endpoint. Check the API logs. |
| `oidc_forbidden` | The IdP denied the request, the email claim was missing/unverified, the email's domain isn't in `UZI_ALLOWED_EMAIL_DOMAINS`, registration is disabled and no existing account matched, or the email already belongs to a *different* linked account. See [Enabling email verification](#enabling-email-verification-required) — this is the most common cause. |
| `oidc_deactivated` | The matched uzi account has been deactivated by an admin. |
| `oidc_error` | An internal error (DB, cookie sealing) — check the API logs. |
