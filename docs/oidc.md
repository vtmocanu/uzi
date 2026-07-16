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
  (subject to `UZI_REGISTRATION_ENABLED`, `UZI_ALLOWED_EMAIL_DOMAINS`, and, if
  configured, `UZI_OIDC_ALLOWED_GROUPS` — see
  [Group-based roles and access](#group-based-roles-and-access-prd-55)
  below); an existing password-registered account is instead **linked** by
  verified email. Either way the match requires the IdP's `email_verified`
  claim to be `true` — see
  [Enabling email verification](#enabling-email-verification-required)
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

**Do this before you enable OIDC on a brand-new instance — unless you're
setting `UZI_OIDC_ADMIN_GROUPS` (below), in which case it's optional.**
Without admin groups configured, the first-ever login — password or OIDC —
becomes the admin, same rule either way. If you turn on OIDC on a fresh
instance with no `UZI_SEED_EMAIL` and no `UZI_ALLOWED_EMAIL_DOMAINS` set, the
first person anywhere in your organization to click "Sign in with SSO"
becomes the uzi admin, not you. Set `UZI_SEED_EMAIL` (and
`UZI_SEED_PASSWORD`) to pre-seed your own admin account, and/or set
`UZI_ALLOWED_EMAIL_DOMAINS` to your org's domain(s) so JIT provisioning can't
reach outside it — see [configuration.md](configuration.md#access-control-prd-5).

**With `UZI_OIDC_ADMIN_GROUPS` set, this warning no longer applies.** The
group decides who's admin, not order of arrival: first-SSO-user-becomes-admin
is disabled outright, and any member of a configured admin group gets
`is_admin` on their first login regardless of whether they're first, tenth,
or the hundredth. Pre-seeding is then optional and only serves as
break-glass — see [Group-based roles and access](#group-based-roles-and-access-prd-55)
below.

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

uzi also requires the claim to be a real JSON boolean: an IdP that emits
`email_verified` as the *string* `"true"` is treated as unverified. Keycloak
and Pocket ID both emit a proper boolean; only other IdPs need checking.

- **Keycloak**: Users → select the user → Details tab → toggle **Email
  verified** ON, Save.

  **LDAP/AD-federated realms** (the user's Details tab shows a **Federation
  link**): federated accounts typically import with the flag OFF, so every
  SSO login bounces with `oidc_forbidden` even though the email came from
  the corporate directory. Three fixes, least- to most-invasive (verified
  against Keycloak 26 / a Meta AD-federated realm, 2026-07-16):

  1. **Per-user flip** (as above) — fine for a handful of users, but every
     newly federated user needs the same click.
  2. **Hardcoded claim mapper, scoped to the uzi client** (recommended when
     the directory owns the mailboxes): Clients → your uzi client → Client
     scopes tab → the `<client-id>-dedicated` scope → Add mapper → By
     configuration → **Hardcoded claim**; Token Claim Name
     `email_verified`, Claim value `true`, **Claim JSON Type: boolean**
     (uzi rejects the string `"true"`), Add to ID token ON. Zero per-user
     maintenance, and no other client in the realm is affected.
  3. **Trust Email** on the federation provider (User Federation → the
     LDAP provider) — systematic but realm-wide; clear it with the realm's
     owner first.
- **Pocket ID**: either flip the **Emails Verified** setting under
  Application Configuration so new accounts start verified, or have the user
  complete Pocket ID's own email-verification flow. Either way flips the
  per-user flag that uzi checks.

## Group-based roles and access (PRD #55)

By default OIDC login has exactly one role decision: whoever logs in first
becomes admin (see [Fresh-instance admin pre-seeding](#fresh-instance-admin-pre-seeding)
above). If your IdP already models teams as groups, you can hand that
decision to the IdP instead: membership in a configured group grants admin,
and membership in a configured group can be required just to log in at all.
Both are opt-in and off by default — see
[configuration.md](configuration.md#oidc-single-sign-on-prd-45) for the full
env var reference (`UZI_OIDC_GROUPS_CLAIM`, `UZI_OIDC_ADMIN_GROUPS`,
`UZI_OIDC_ALLOWED_GROUPS`).

- **`UZI_OIDC_ADMIN_GROUPS`** (comma-separated, empty = off): membership in
  any listed group makes a user admin. Setting this disables
  first-SSO-user-becomes-admin outright — see the pre-seeding note above.
- **`UZI_OIDC_ALLOWED_GROUPS`** (comma-separated, empty = no gate): membership
  in any listed group is required to SSO-login or JIT-provision at all.
  Admins get no implicit pass: if your admin group is separate (e.g.
  `uzi-admins` alongside `uzi-users`), list it here too
  (`UZI_OIDC_ALLOWED_GROUPS=uzi-users,uzi-admins`) or your admins can't log
  in. A
  user outside every listed group gets `/login?error=oidc_forbidden`, same as
  any other rejected login (no detail beyond the server log — see
  [Troubleshooting](#troubleshooting)).
- Matching is **exact and case-sensitive** after trimming whitespace from the
  config value — no glob, no regex, no path normalization. This is why the
  Keycloak walkthrough below has you turn **Full group path** off: with it
  left on, Keycloak emits `/uzi-admins` instead of `uzi-admins`, and that
  won't match `UZI_OIDC_ADMIN_GROUPS=uzi-admins`. (You can instead set
  `UZI_OIDC_ADMIN_GROUPS=/uzi-admins` verbatim if you'd rather keep Full
  group path on — either works, as long as the two sides match exactly.)

### Authoritative sync: groups grant AND demote

Group membership is re-checked on **every** OIDC login, not just the first.
There are no sticky roles:

- A user added to `UZI_OIDC_ADMIN_GROUPS` becomes admin on their next SSO
  login.
- A user **removed** from that group is demoted on their next SSO login —
  no admin action needed, and none available (there is no manual
  promote/demote in this PRD; the IdP is the source of truth).
- A user removed from `UZI_OIDC_ALLOWED_GROUPS` is blocked on their next SSO
  login attempt.

This is a **login-time** sync, not a live one. Two staleness windows follow
from that:

- A demoted-in-the-IdP user who simply never logs in again keeps their old
  `is_admin` until they do. For an OIDC-only user, `AUTH_TOKEN_TTL` (default
  `168h`) is a **max-idle** bound here, not an absolute cap: uzi's rolling
  refresh (see [How it works](#how-it-works)) slides an active session
  forward past its half-life on every request, so a continuously-active
  user's stale `is_admin` can persist well past 168h for as long as they
  keep using uzi without a gap that long. A user who also has a uzi password
  can keep re-authenticating that way indefinitely and never trigger the
  sync at all (groups apply to OIDC logins only — a password login never
  touches `is_admin`). Use the admin deactivate-user action for an immediate
  cutoff regardless of session activity.
- Likewise, removing someone from `UZI_OIDC_ALLOWED_GROUPS` blocks their
  *next* SSO login but does not revoke a session they're already holding. To
  cut off access immediately, use the existing admin deactivate-user action,
  which does kill live sessions.

The other side of that same mechanism works in the sync's favor: once a
grant or demotion IS written, at the user's next OIDC login, it takes effect
for every one of that user's **other** live sessions immediately, not just
the one they just logged into. `is_admin` isn't carried in the JWT — every
request reloads the user row — so there's no separate per-session
propagation delay once the write lands.

### The groups claim: present-but-absent isn't the same as present-but-empty

uzi reads groups from the **verified ID token only** — there is no userinfo
fallback — and treats two situations very differently:

- **The claim is present, as a JSON array of strings, and the user isn't in
  it** (including an empty array `[]`): that's real, authoritative removal.
  Demotion and gating apply normally.
- **The claim is missing entirely, `null`, or not shaped like an array of
  strings** (a renamed claim, a mapper switched off, a stray string instead
  of an array): uzi treats this as an **IdP misconfiguration, not a mass
  removal**. An existing user keeps whatever role they already had and still
  passes the allowlist gate; the login proceeds, but the server logs a
  loud warning on every such login so the misconfiguration doesn't go
  unnoticed. A **brand-new** user hitting JIT provisioning in this state has
  no established role to fall back on, so they're admitted as non-admin and,
  if `UZI_OIDC_ALLOWED_GROUPS` is set, refused outright.

  This fail-safe exists so that one broken mapper or one renamed claim can't
  silently demote every admin and lock every existing user out at the same
  time.

### Seed admin: exempt from demotion, not from the gate

`UZI_SEED_EMAIL` is your break-glass account, and it keeps that role here:
it is **never demoted** by the group sync, even if it's missing from
`UZI_OIDC_ADMIN_GROUPS` or removed from it later. It can, however, still be
**promoted** by group membership like anyone else.

The exemption does not extend to the allowlist gate. If the seed admin's
email is used to SSO-login and `UZI_OIDC_ALLOWED_GROUPS` is set without them
in it, the gate rejects the SSO attempt before user resolution ever runs —
group config can't grant an SSO exception to itself. The seed admin's actual
break-glass path is **password login**, which is entirely outside OIDC and
bypasses both the gate and the sync by design (groups apply to OIDC logins
only).

### Keycloak: emitting the groups claim

Builds on the client you created in the [Keycloak walkthrough](#keycloak-walkthrough)
above.

1. **Groups → Create group** for each role you want to map (e.g. `uzi-admins`,
   `uzi-users`).
2. Add the relevant users to those groups (**Groups → &lt;group&gt; → Members
   → Add member**, or per-user under **Users → &lt;user&gt; → Groups → Join
   Group**).
3. The simplest home for the mapper is the client's own dedicated scope
   (no separate scope to create or attach): **Clients → your uzi client →
   Client scopes tab → `<client-id>-dedicated` → Add mapper → By
   configuration → Group Membership**.
4. In the mapper: set **Token Claim Name** to `groups` (or whatever you set
   `UZI_OIDC_GROUPS_CLAIM` to), turn **Full group path** **OFF** (see the
   exact-match note above), and turn **Add to ID token** **ON** — this is the
   step PRD #45's own setup can't cover, since a plain login mapper doesn't
   include it by default.
5. Set `UZI_OIDC_ADMIN_GROUPS` and/or `UZI_OIDC_ALLOWED_GROUPS` to the group
   name(s) from step 1 (e.g. `UZI_OIDC_ADMIN_GROUPS=uzi-admins`), restart uzi,
   and log in as a member to confirm.

No scope needs requesting for this on Keycloak — group emission there is a
client-scope/mapper concern, not something `UZI_OIDC_SCOPES` controls.

### Pocket ID: emitting the groups claim

Builds on the client you created in the [Pocket ID walkthrough](#pocket-id-walkthrough)
above.

1. In the Pocket ID admin panel, create the **user groups** you want to map
   (e.g. `uzi-admins`, `uzi-users`) and add the relevant users to them.
2. Add `groups` to `UZI_OIDC_SCOPES` (e.g.
   `UZI_OIDC_SCOPES=openid profile email groups`) — unlike Keycloak, Pocket ID
   emits the groups claim via a requested scope, not a separate mapper step.
3. Set `UZI_OIDC_ADMIN_GROUPS` and/or `UZI_OIDC_ALLOWED_GROUPS` to the group
   name(s) from step 1, restart uzi, and log in as a member to confirm.

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
| `oidc_forbidden` | The IdP denied the request, the email claim was missing/unverified, the email's domain isn't in `UZI_ALLOWED_EMAIL_DOMAINS`, registration is disabled and no existing account matched, the email already belongs to a *different* linked account, or (if configured) the user isn't a member of any `UZI_OIDC_ALLOWED_GROUPS` group — see [Group-based roles and access](#group-based-roles-and-access-prd-55). See [Enabling email verification](#enabling-email-verification-required) — this is the most common cause. |
| `oidc_deactivated` | The matched uzi account has been deactivated by an admin. |
| `oidc_error` | An internal error (DB, cookie sealing) — check the API logs. |
