---
title: Configuration
audience: operator
---

# Configuration

All configuration is via environment variables, set in `.env` (copied from `.env.example`) and passed into the `api` container by `docker-compose.yml`. `POSTGRES_*` also configures the `db` service directly.

| Var | Default | Notes |
|---|---|---|
| `JWT_SECRET` | — (required) | HS256 signing key for session JWTs. The API refuses to start if it is missing, empty, shorter than 16 characters, or a known placeholder (`change-me`, `secret`, `password`, etc. — see `api/internal/config/config.go`). Generate with `openssl rand -hex 64`. |
| `UZI_SECRET_KEY` | — (required) | Base64-encoded 32-byte master key for AES-256-GCM. The API refuses to start if it is missing, not valid base64, not exactly 32 bytes decoded, or a low-entropy placeholder such as an all-zero key (same boot-guard stance as `JWT_SECRET`). Generate with `openssl rand -base64 32`. It is the platform's one shared encryption-at-rest key: it seals every stored secret kind (today the per-user Anthropic token and forge bot PATs; any future kind) before it reaches Postgres, so a DB dump alone never recovers a plaintext secret. Rotating this key invalidates **all** stored secrets across every kind at once, not just one feature's: every affected user has to reconnect or re-paste theirs. There is no re-encrypt path. |
| `POSTGRES_PASSWORD` | — (required) | Password for the bundled Postgres role. Generate with `openssl rand -hex 24`. Compose refuses to start without it. |
| `POSTGRES_USER` | `uzi` | Postgres role name. |
| `POSTGRES_DB` | `uzi` | Postgres database name. |
| `AUTH_TOKEN_TTL` | `168h` (7 days) | Session JWT lifetime and cookie `MaxAge`, as a Go duration. Rolling refresh (see [auth-design.md](auth-design.md)) slides this on every authenticated request past the halfway point, so active users are never logged out mid-session. |
| `FRONTEND_ORIGIN` | `http://127.0.0.1:8080` | User-facing origin. Its scheme decides the cookie `Secure` flag: `https://` makes cookies `Secure`, anything else does not. Use an `https://` origin behind TLS in production. |
| `RATE_LIMIT_MAX` | `10` | Max requests per window, per (route, client IP), for `/api/auth/register` and `/api/auth/login`. |
| `RATE_LIMIT_WINDOW` | `1m` | Fixed window for the rate limiter, as a Go duration. |
| `TRUSTED_PROXIES` | *(empty)* | CIDRs whose direct connections are trusted to speak for a real client via `X-Forwarded-For`. Only requests whose `RemoteAddr` falls in one of these ranges get their `X-Forwarded-For` honored; everyone else's is ignored. **Empty (the default) never trusts `X-Forwarded-For`, and that is correct for compose** — see [auth-design.md](auth-design.md). Set it **only** if you front uzi with your own reverse proxy, and set it to *that proxy's address*, narrowly: a hand-typed CIDR also covers the `agent` container, which shares the network and runs untrusted code by design. |
| `DATABASE_URL` | set by compose | pgx connection string, built from `POSTGRES_*` and the `db` service name. Not meant to be set directly when using compose. |
| `API_ADDR` | `:8080` | Address the `api` binary listens on inside its container. Set by compose; no need to change it. |
| `API_TLS_CERT` | unset (no TLS listener) | Path to a PEM certificate. Set together with `API_TLS_KEY` to make the `api` serve a **second, TLS** listener alongside the plain one. Unset on compose, where nothing needs it. See [the TLS listener](#the-optional-tls-listener) below. |
| `API_TLS_KEY` | unset (no TLS listener) | Path to the matching PEM private key. Setting exactly one of the pair refuses to boot: a cert without a key cannot serve TLS, and treating that as "TLS off" would silently hand you a plaintext hop you believed was encrypted. |
| `API_TLS_ADDR` | `:8443` | Address of the TLS listener. Ignored unless the cert/key pair is set. Must differ from `API_ADDR`. |

Invalid values for `AUTH_TOKEN_TTL`, `RATE_LIMIT_MAX`, `RATE_LIMIT_WINDOW` or an unparseable `TRUSTED_PROXIES` entry fall back to their defaults rather than failing boot (the last one is logged as a warning); only a bad `JWT_SECRET`, a bad `UZI_SECRET_KEY`, a missing `DATABASE_URL`, or a broken `API_TLS_CERT`/`API_TLS_KEY` pair refuses to start.

### The optional TLS listener

The `api` normally serves one plain HTTP port, reached only by the `web` reverse proxy on the same private network. That is all a compose stack needs, and leaving `API_TLS_CERT`/`API_TLS_KEY` unset keeps it exactly that.

On Kubernetes there is a second kind of client: **hosted workers and the worker controller dial the `api` directly**, with no nginx in the path. A claim response on that hop carries the run owner's *decrypted* forge PAT and Anthropic token, and on a shared cluster the pod network is not private. Setting the pair adds a TLS listener for those clients:

- It is **additive**. The plain listener stays, because that is what `web`'s nginx and the kubelet probes speak to.
- The two are **separate ports on purpose**: it is what lets a NetworkPolicy admit the worker namespace to the TLS port and to nothing else, while the plain port stays reachable only from the `web` pods.
- **The TLS port serves a smaller surface**, not the same one. Only `/api/worker/*` and `/api/controller/*` are mounted there — the exact set the agent and the controller dial. `/api/auth/*`, `/api/admin/*` and the rest are not reachable from a hosted worker at all. This is deliberate: a hosted worker runs an agent against a user's cloned repo, so it is a semi-hostile position by design, and "unreachable" is a better resting place than "reachable but defended".
- **No `X-Forwarded-For` is trusted on the TLS port, because the header is removed before routing.** This one needs care, and the enforcement is the point rather than the intent: `TRUSTED_PROXIES` is matched on the *peer IP alone* — it knows nothing about listeners or routes. On a cluster it is typically the whole pod CIDR (pod IPs are dynamic, so no narrower value is maintainable), which means **hosted worker pods fall inside it and are trusted proxies by construction**. Stripping the header at this listener is what makes "the `api` sees a worker's real peer address" true; narrowing the CIDR cannot, and a future pod-CIDR change would silently invalidate it. The listener exists for clients that dial the `api` *directly*, so they are never proxied and an `X-Forwarded-For` on this hop could only be forgery. **If you ever front this port with a real reverse proxy, that strip is the thing to revisit.**
- The values are **paths, not material**. The `api` re-reads them when they change, so a cert-manager renewal is picked up without a restart. A malformed pair fails at boot rather than at the first handshake.

Clients verify the certificate against the issuing CA (`UZI_API_CA_FILE` on the controller). There is no "skip verification" switch anywhere in this path, by design: an unverified peer is the exact attack the encryption exists to prevent, so TLS-without-verification would be worse than the plain HTTP it replaced — it would look solved.

On Kubernetes the Helm chart wires all of this from `api.tls.*` (certificate included); see [deploy/README.md](../deploy/README.md).

There is no CORS configuration to make, by design: nginx serves the SPA and proxies `/api/*` to the API on the same origin (see [ARCHITECTURE.md](../ARCHITECTURE.md)), so the browser never makes a cross-origin request.

## Hosted k8s workers (PRD #58)

k8s-only: nothing here applies to compose, and every variable below is set by the Helm chart, not hand-edited — this table exists for completeness and for anyone reading the chart's rendered env. See [Hosted workers](hosted-workers.md) for the user-facing feature, [Admin settings](admin-settings.md#hosted-worker-quota) for the per-user quota, and [deploy/README.md](../deploy/README.md#hosted-workers-prd-58) for the operator rollout runbook (turning it on, rotating the controller token, and the residuals that are only proven on a real cluster).

### `api`

| Var | Default | Notes |
|---|---|---|
| `WORKER_HOSTING_ENABLED` | `false` | The feature gate. `true` requires `WORKER_HOSTING_CONTROLLER_TOKEN_SHA256` to also be set, or the api refuses to boot — enabling hosting with no way to authenticate a controller would be a silent no-op, and this is a security control rather than a tuning knob, so a malformed value also refuses to start. |
| `WORKER_HOSTING_CONTROLLER_TOKEN_SHA256` | — | The sha256 (hex) of the controller's bearer credential, generated once with `openssl rand -base64 32` and hashed. Setting this while `WORKER_HOSTING_ENABLED` is `false` also refuses to boot — a hash with nothing to gate it is a stray credential the api would silently ignore. |
| `WORKER_HOSTING_PENDING_TOKEN_TTL` | `1h` | How long a hosted worker's join token may sit sealed in Postgres, undelivered, before the expiry sweep destroys it. Read regardless of `WORKER_HOSTING_ENABLED`, so a stack that provisioned workers and then turned hosting off doesn't strand ciphertext. `0` disables the sweep — an undelivered token then stays sealed under `UZI_SECRET_KEY` indefinitely; see [vault-threat-model.md](vault-threat-model.md#hosted-worker-join-tokens-prd-58). |

### `controller`

A new component, one per deployment, the only thing in uzi holding a kube-API credential:

| Var | Default | Notes |
|---|---|---|
| `UZI_API_URL` | — (required) | The api's base URL, as the controller itself dials it (the release namespace's short Service name is fine here). |
| `UZI_CONTROLLER_TOKEN_FILE` | — (required) | Path to the controller's own bearer credential, file-mounted like the worker's join token and for the same reason: an env-borne secret is readable via `/proc/<pid>/environ`. |
| `UZI_API_CA_FILE` | — (optional) | PEM bundle to verify the api's TLS certificate against, pinned exclusively (not additive to the system roots). Required in practice whenever `UZI_API_URL` is `https://`. |
| `UZI_WORKER_NAMESPACE` | — (required) | The dedicated namespace hosted workers render into — empty of everything else by design (see [ARCHITECTURE.md](../ARCHITECTURE.md#worker-controller-k8s-only)). |
| `UZI_WORKER_SERVICE_ACCOUNT` | — (required) | The workers' own zero-permission ServiceAccount (no token automount). |
| `UZI_WORKER_IMAGE_REPO` | — (required) | Registry prefix the per-template agent images live under; the controller appends `/agent-<template>:<tag>`. |
| `UZI_WORKER_IMAGE_TAG` | — (required) | The release tag to run. Lives here, not on the api — the api never learns an image tag (Decision 1/7 in the PRD). Changing it rolls every hosted worker onto the new release once each holds no non-terminal run. |
| `UZI_WORKER_API_URL` | — (required) | The FQDN a **worker** dials — necessarily the full cluster-DNS name, since a short Service name doesn't resolve cross-namespace. Deliberately separate from `UZI_API_URL` above. |
| `UZI_WORKER_STORAGE_CLASS` | *(cluster default)* | StorageClass for a hosted worker's PVCs. |
| `CONTROLLER_POLL_INTERVAL` | `10s` | How often the controller fetches desired state and reconciles the fleet. The controller is stateless, so a restart loses nothing between polls. |
| `CONTROLLER_HTTP_TIMEOUT` | `15s` | Per-call timeout on every request to the api. |

## Forge integration

See [gitlab-bot-setup.md](gitlab-bot-setup.md) for the bot-account procedure these variables support.

These extend the core table above; `UZI_SECRET_KEY` (which also encrypts forge bot PATs) is documented there.

| Var | Default | Notes |
|---|---|---|
| `FORGE_ALLOWED_BASE_URLS` | `https://gitlab.example.com` | Comma-separated SSRF allowlist: the only forge base URLs a connection may target. Every entry must be an absolute `https://` URL; boot fails if the list is empty or any entry is malformed or non-`https`. The Forge tab (under Settings) offers exactly this set in its base-URL dropdown. |
| `FORGE_POLL_INTERVAL` | `60s` (`1m`) | Per-enabled-repo incremental sync cadence (Go duration). An invalid or non-positive value falls back to the default. See "Freshness contract" below. |
| `FORGE_RECONCILE_EVERY` | `10` | Every Nth incremental poll is a full reconcile instead (fetches the complete `PRD`-labeled issue set with no `updated_after` bound, diffs, and evicts cache rows the forge no longer returns). A non-positive or unparseable value falls back to the default, same as the other numeric/duration vars here (the poller itself additionally clamps any value `< 1` to `1` — every poll becomes a full reconcile — as defense in depth, but that path isn't reachable through this env var). The very first poll after a repo is enabled is always a full reconcile regardless of this setting, since it has to seed the cache. |
| `FORGE_HTTP_TIMEOUT` | `15s` | Hard per-call timeout on every outbound HTTP request to the forge (connect, projects, labels, issues, label updates). Closes the untimeouted-`http.DefaultClient` wart the `multica` inspiration shipped with. |
| `FORGE_RATE_LIMIT_MAX` / `FORGE_RATE_LIMIT_WINDOW` | `30` / `1m` | Per-user request budget (not per-IP) on the forge-*proxying* endpoints only — connection verify, project listing, issue move, manual sync — so one user's connection can't hammer the upstream forge. Separate limiter and separate budget from `RATE_LIMIT_MAX`/`RATE_LIMIT_WINDOW`, which only cover `/api/auth/register` and `/api/auth/login`. |
| `UZI_SEED_EMAIL` | — (optional) | Setting this provisions an admin user automatically at startup, after migrations, if no user with that email already exists yet (an existing user's password is never touched — the seed is idempotent and safe to leave set across restarts). Must be a syntactically valid address or boot fails. Leave unset to disable seeding entirely (the normal first-registration-becomes-admin flow still applies). |
| `UZI_SEED_PASSWORD` | — (required if `UZI_SEED_EMAIL` is set) | Password for the seeded admin, hashed with the same argon2id parameters as normal registration. Must satisfy the same length policy as registration (12–1024 characters) or boot refuses to start — a set-but-invalid seed is a loud misconfiguration, not a silent skip. |
| `UZI_SEED_NAME` | — (optional) | Display name for the seeded admin. |
| `UZI_SEED_FORGE_PAT` | — (optional) | Setting this seeds a forge connection (owned by the seed admin) at startup. Requires `UZI_SEED_EMAIL` to be set, or boot fails. At boot, if the seed admin has no connection for the target base URL yet, the API verifies this bot PAT against the forge, stores it encrypted (same path as the Forge tab's connect flow), lists the bot's projects, and enables the repos named in `UZI_SEED_FORGE_REPOS`. An **already-present** connection is left entirely untouched (never re-verified, never overwritten) — safe to leave set across restarts. A **forge outage** at seed time (network error, 401) is non-fatal: it is logged and skipped, and seeding retries on the next boot; only a bad *static* config (PAT without email, non-allowlisted base URL) refuses to start. The PAT is never logged. |
| `UZI_SEED_FORGE_BASE_URL` | first entry of `FORGE_ALLOWED_BASE_URLS` | Forge base URL the seeded connection targets. Must be one of `FORGE_ALLOWED_BASE_URLS` or boot fails (same SSRF allowlist as user-created connections). |
| `UZI_SEED_FORGE_REPOS` | — (optional) | Comma-separated `path_with_namespace` list (e.g. `vtmocanu/uzi,vtmocanu/foo`) to enable on the seeded connection. Entries the bot cannot see are logged as a warning and skipped, not fatal. Empty means the connection is created but no repo is enabled. |
| `UZI_SEED_ANTHROPIC_TOKEN` | — (optional, dev convenience) | Setting this seeds the seed admin's [Anthropic token](anthropic-token.md) at startup, create-only: the seed runs only when that user holds **no** Anthropic token, so an existing one (UI-pasted, or already seeded) is left untouched and a user with several named tokens is never touched either. It lands labelled `default` and flagged as the account default — the same shape migration `00077` backfills onto a pre-existing row, so a seeded stack and an upgraded one are indistinguishable afterwards; there is no way to seed a second, named token. Requires `UZI_SEED_EMAIL` to be set (the token belongs to that user) or boot fails. Only format-checked at seed time, never verified against Anthropic and never logged. Exists so a local `docker compose down -v` doesn't force re-pasting your own token; leave unset in any shared deployment. |
| `UZI_SEED_SLACK_BOT_TOKEN` / `UZI_SEED_SLACK_APP_TOKEN` | — (optional, must be set together) | Seeds the [Slack](slack.md) tokens into instance settings at startup, create-only per key: sealed at rest with `UZI_SECRET_KEY`, and `slack_enabled` is flipped to `true` in the same pass. Rows that already exist (UI-set, or previously seeded — including an admin's later "off" flip of the enable toggle) are left untouched, so the admin **Settings → Slack** page stays the editable source of truth and only a fresh `docker compose down -v` re-seeds. Unlike the `SLACK_BOT_TOKEN`/`SLACK_APP_TOKEN` overlay (which wins over the DB on every read and greys the UI fields), and mutually exclusive with it — setting a seed var and its overlay counterpart refuses to boot. Prefix-checked (`xoxb-`/`xapp-`) at boot, never validated against Slack at seed time (a bad token shows as a failed connect on the status chip) and never logged. |
| `UZI_SEED_PUBLIC_BASE_URL` | — (optional) | Seeds the `public_base_url` setting (the deep-link base for Slack messages) the same create-only way. Must be `http(s)` or boot fails. Mutually exclusive with the `UZI_PUBLIC_BASE_URL` overlay. |
| `SLACK_SOCKET_DEBUG` | — (diagnostic only) | `1` turns on slack-go's raw Socket Mode wire logging (every WebSocket frame). The connect line includes the wss `?ticket=` credential, so use it only while diagnosing a socket problem on a private box and turn it off after. Normal inbound visibility (receipt logs with envelope/action types, never payloads) is always on and does not need this. |

### Default column labels are created on the forge

The first time a repo's board is opened, uzi ensures three labels exist on that GitLab project — `In Progress`, `Upcoming`, `Later` (`forgesvc.DefaultColumns`) — creating any that are missing via the forge's label-create API. This is a deliberate, visible side effect: those labels then show up in GitLab's own label list and board for that project, not just inside uzi. Board columns are reconfigurable afterward to any of the project's labels; the default set is only what's seeded on first open.

### Freshness contract

- Content edits and label adds/removes made at normal human cadence (spaced further apart than GitLab's `updated_at` bump throttle — see below) are picked up within one `FORGE_POLL_INTERVAL`.
- GitLab throttles how often it bumps an issue's `updated_at` to roughly once per ~60-second window, regardless of whether the triggering change is a label add or a label remove (verified against gitlab.example.com — see the PRD's Sync engine section for the full finding). Multiple edits landing inside the same throttle window collapse to a single bump, so only the latest of them is guaranteed to be caught incrementally; earlier ones in that window are caught by the next reconcile pass instead. `FORGE_POLL_INTERVAL`'s default (`60s`) is the same order of magnitude as the throttle window, so normal editing cadence is still caught incrementally almost all the time.
- De-labeling, issue deletion, and any edit whose `updated_at` bump the incremental filter missed are only guaranteed to be visible within one `FORGE_RECONCILE_EVERY`-th poll (the full reconcile), because eviction — noticing a previously-cached issue is now absent from the forge's current set — is structurally impossible for an `updated_after`-filtered incremental query to do.

## CI status integration (PRD #6)

uzi caches the latest pipeline per **watched ref** (a repo's default branch, plus the branches of that repo's recent agent runs) on the same poll tick as the issue sync — no second loop or interval — and renders it as a status badge on the repos list, the board header, and each card. A failed pipeline offers a **Fix CI** button that queues a plan-gated `ci_fix` agent run; when that run's fix branch pipeline concludes, the sync stamps the run `verified` or `fix_failed` ("uzi verifies its work"). See [ARCHITECTURE.md](../ARCHITECTURE.md) for the full pipeline-sync + verification design.

| Var | Default | Notes |
|---|---|---|
| `CI_WATCH_MAX_REFS` | `20` | Max agent run branches watched per repo per tick (newest first). **Set to `0` to disable the pipeline sync entirely** — no CI badges, no Fix CI — reproducing pre-PRD-6 behaviour bit-for-bit for operators who want CI awareness off. Hitting the cap is logged, never silent. |
| `CI_WATCH_RUN_WINDOW` | `336h` (14 days) | How long a **finished** run's branch keeps being watched for CI after it completes (a non-terminal run is watched regardless). Go duration syntax only (`h`/`m`/`s`, no `d`), so 14 days is written `336h`; a literal `14d` is unparseable and silently falls back to the default. Long enough to cover review cycles, bounded so dead branches age out of the cache. |
| `CI_FIX_MAX_JOBS` | `10` | Max failed jobs a Fix CI snapshot captures from the failed pipeline. Bounds the snapshot (jobs × tail) frozen onto the `ci_fix` run at queue time. |
| `CI_FIX_LOG_TAIL_BYTES` | `32768` (32 KiB) | Bytes captured from the **end** of each failed job's trace (a failure concludes its log). Tails are treated as untrusted evidence and pass a PAT + known-token redaction pass before they are stored. |

**Verification caveat**: `verified` means "the fix MR's latest pipeline passed". A merge-result failure that only surfaces on `main` (a semantic conflict) is caught only if the project runs [merged-results pipelines](https://docs.gitlab.com/ee/ci/pipelines/merged_results_pipelines.html) — a GitLab-config concern, not a uzi setting.

**Secrets-in-logs residual risk**: the snapshot scrubber strips uzi's bot PAT by value (the forge driver's connection-PAT redactor) plus known token *shapes* (GitLab `glpat-`/`gloas-`/`glrt-`/`glcbt-`/`glptt-`/`glsoat-`/`glimt-`/`glagent-`/`gldt-`, Anthropic `sk-ant-`) and any `Authorization`/`PRIVATE-TOKEN`/`Bearer` header line. It does NOT scrub the worker join token or a per-user Anthropic token by value — those are caught only if a pipeline echoes them inside one of those header lines — and it cannot know every third-party secret shape a teammate's pipeline might print. A snapshot is stored server-side like any run message, visible only to the run's owner and admins.

## Access control (PRD #5)

Registration controls and PAT least-privilege verification. See [auth-design.md](auth-design.md#registration-controls) for the registration policy and [gitlab-bot-setup.md](gitlab-bot-setup.md#least-privilege-what-uzi-verifies) for what the privilege checker enforces.

| Var | Default | Notes |
|---|---|---|
| `UZI_REGISTRATION_ENABLED` | `true` | Registration kill-switch. When `false`, `POST /api/auth/register` returns `403` and the SPA replaces the register form with a "registration is disabled" notice; login is unaffected. Unlike the tuning knobs above, a **malformed** value (not a boolean) **refuses to start** — this is a security control, so a typo fails loud rather than silently defaulting to open. The seed admin (`UZI_SEED_EMAIL`) is created out-of-band and is never gated by this. |
| `UZI_ALLOWED_EMAIL_DOMAINS` | — (empty = all) | Comma-separated registration email-domain allowlist, matched case-insensitively, **exact match only** (no subdomain wildcards: `a.example.com` does not match `example.com`). Empty/unset allows every domain (today's open behavior; the compose demo stays zero-config). A rejected domain returns `403` with the allowed list named. The domain is taken from the parsed address, so display-name forms (`Alice <alice@example.com>`) are handled correctly. The seed admin is **exempt** — an allowlist that excludes your own seed email never causes a bootstrap lockout. |
| `UZI_PRIVILEGE_CHECK_INTERVAL` | `24h` | Cadence of the background PAT least-privilege re-check sweep (Go duration). When enabled (`> 0`), the sweep runs a boot pass shortly after start — back-filling never-checked (grandfathered) connections so an over-privileged token surfaces without waiting a full interval — then re-checks on the interval. `0` **disables the sweep entirely, including that boot pass**: it is the one duration knob where `0` is honored, not treated as "unset". With `0`, a never-checked connection therefore stays `unchecked` until its owner runs the on-demand "Check privileges" button — only the save-time check (which blocks an over-privileged token at connect) still runs regardless. A negative or otherwise malformed value falls back to the default. |

## OIDC single sign-on (PRD #45)

See [oidc.md](oidc.md) for the operator setup guide (Keycloak and Pocket ID walkthroughs, break-glass, troubleshooting) and [auth-design.md](auth-design.md#oidc-single-sign-on) for the design.

| Var | Default | Notes |
|---|---|---|
| `UZI_OIDC_ISSUER_URL` | — (unset = OIDC off) | The IdP's issuer URL; `/.well-known/openid-configuration` is fetched relative to it. Must be `https://` — plain `http://` is accepted only for a loopback host (`localhost`, `127.0.0.0/8`, `::1`), for local IdP development. Setting this enables the "Sign in with SSO" button; it stays visible even if discovery fails at boot (see below), since a login attempt retries discovery on its own. |
| `UZI_OIDC_CLIENT_ID` | — | The confidential client's ID at the IdP. |
| `UZI_OIDC_CLIENT_SECRET` | — | The confidential client's secret. Env-only: never stored in the DB, same trust level as `JWT_SECRET`. `UZI_OIDC_ISSUER_URL`, `UZI_OIDC_CLIENT_ID`, and `UZI_OIDC_CLIENT_SECRET` are all-or-nothing — setting some but not all of them refuses to start. |
| `UZI_OIDC_SCOPES` | `openid profile email` | Space-separated requested scopes. `openid` is always force-included even if omitted (an ID token can't be issued without it); duplicates are dropped. |
| `UZI_OIDC_PROVIDER_NAME` | `SSO` | Label shown on the login button ("Sign in with {name}"). |
| `UZI_OIDC_HTTP_TIMEOUT` | `15s` | Hard timeout on every outbound call to the IdP (discovery, JWKS, token exchange), mirroring `FORGE_HTTP_TIMEOUT`'s posture. |
| `UZI_PASSWORD_LOGIN_ENABLED` | `true` | Kill-switch for email+password: `false` hides the password form in the SPA and makes `POST /api/auth/register` return `403` (no point minting a password account that can never log in). Setting this `false` while OIDC is unconfigured **refuses to start** (a total lockout — nobody could ever authenticate); the break-glass is flipping it back to `true` and restarting, since the seed admin (`UZI_SEED_EMAIL`) always keeps a `password_hash`. A malformed (non-boolean) value also refuses to start. |

The redirect URI is not itself a setting: it's derived as `FRONTEND_ORIGIN + /api/auth/oidc/callback`, and must match exactly what's registered at the IdP.

### Group-based roles and access (PRD #55)

See [oidc.md](oidc.md#group-based-roles-and-access-prd-55) for the full semantics (authoritative sync, fail-safe on an absent claim, seed-admin exemption, staleness windows) and the Keycloak/Pocket ID walkthroughs. Setting `UZI_OIDC_ADMIN_GROUPS` or `UZI_OIDC_ALLOWED_GROUPS` while OIDC (above) is unconfigured refuses to start, same posture as the core OIDC triple. `UZI_OIDC_GROUPS_CLAIM` is a format knob, not a gate: it's inert on its own (ships as a compose/`.env.example` default of `groups`) and never blocks boot by itself.

| Var | Default | Notes |
|---|---|---|
| `UZI_OIDC_GROUPS_CLAIM` | `groups` | The ID-token claim name carrying the user's group membership. Override only if your IdP names it differently; Keycloak and Pocket ID can both be configured to emit `groups`. Dormant on its own — has no effect unless `UZI_OIDC_ADMIN_GROUPS` or `UZI_OIDC_ALLOWED_GROUPS` is also set, and setting it alone never trips the refuse-to-start guard above. |
| `UZI_OIDC_ADMIN_GROUPS` | — (empty = off) | Comma-separated group names; membership in any one grants `is_admin` on SSO login. Setting this disables the first-OIDC-user-becomes-admin rule outright — the group decides, not order of arrival. Authoritative on every login: leaving the group demotes on the next SSO login. The seed admin (`UZI_SEED_EMAIL`) is exempt from demotion only (break-glass), never from the allowlist gate below. |
| `UZI_OIDC_ALLOWED_GROUPS` | — (empty = no gate) | Comma-separated group names; membership in any one is required to SSO-login or JIT-provision at all. Checked before any DB read or write, so a rejected login never links or creates a row. A claim that's present but shows no match rejects both existing and brand-new users; a claim that's entirely absent or unparseable (IdP misconfig, not removal) still lets an *existing* user through, but a brand-new JIT user is refused (no established role to fail safe into). |

Matching is exact and case-sensitive after trimming whitespace from the config value — no glob/regex/path normalization. There is no `UZI_OIDC_*` variable for multiple providers or SAML — out of scope for this iteration (see the PRD).

## Agent runtime (PRD #4)

See [ARCHITECTURE.md](../ARCHITECTURE.md#agent-runtime-workers-runs-live-view) for how these are used (run lifecycle, sweeper, claim affinity) and [worker-setup.md](worker-setup.md) for the operator procedure. `UZI_SEED_ANTHROPIC_TOKEN` (dev convenience: boot-seeds an existing Anthropic token for the seed admin) is documented above, in the Forge integration seed table, alongside the other startup seeds.

The run view in the web UI shows a terse, one-line-per-event feed (tool calls with argument summaries, results folded under their call, durations, an in-flight spinner), never raw JSON. When you need the complete raw frames for debugging, set `UZI_LOG_LEVEL=debug` and read them from the worker's `docker logs` (see the `UZI_LOG_LEVEL` row below).

### Server (`api`)

| Var | Default | Notes |
|---|---|---|
| `RUN_TIMEOUT` | `2h` | Wall-clock cap on a `running` run; the sweeper fails it past this. Also sent to the worker in the claim payload (`RunTimeoutSeconds`) for its own reference; the server's own sweeper is the actual enforcement. The admin-tunable [Run health](admin-settings.md#run-health) "slow" threshold is clamped to stay below this at read time, so it always warns before this ends the run. |
| `RUN_IDLE_TIMEOUT` | `10m` | No-SDK-message idle cap, enforced **worker-side**; read here and shipped in the claim payload (`IdleTimeoutSeconds`). |
| `RUN_MAX_ITERATIONS` | `5` | Cap on the implement ⇄ review loop count, enforced worker-side; read here and shipped in the claim payload. |
| `PLAN_MAX_REVISIONS` | `3` | Cap on the number of `revise_plan` rounds per run before the plan-approval gate refuses more (PRD #41). Enforced **server-side** (the authoritative bound, counting all persisted revise rows) and again worker-side; read here and shipped in the claim payload. |
| `RUN_MAX_REQUEUES` | `1` | How many times the sweeper may re-queue a run whose worker went stale before failing it instead. `0` means fail immediately on worker death (no re-queue). |
| `UZI_AUTOSTOP_ENABLED` | `true` | Operator kill switch for the automatic stop of a run whose updates cannot be saved (PRD #108 M5). See [Why was my run stopped automatically?](run-auto-stopped.md). Setting it to `false` disables **only the stop** — the run is still flagged `looping` with a truthful reason, so you keep the visibility and give up the intervention. It is deliberately **not** the admin [Run health](admin-settings.md#run-health) toggle: an admin turning health off must not silently disable loop protection, and an automatic destructive behaviour should not depend for its off switch on the database it might be misbehaving against. Unlike the tuning knobs above, a set-but-unparseable value **aborts boot** rather than defaulting — a typo like `flase` would otherwise leave you believing you had disarmed something that kills runs. |
| `WORKER_HEARTBEAT_STALE` | `45s` | No heartbeat past this and the sweeper marks a worker offline and re-queues its non-terminal runs. |
| `WORKER_AFFINITY_GRACE` | `2m` | How long a re-queued run is claimable only by the worker that was already running it, before any of the user's other workers may claim it (gives a resume a chance to land back on the disk that still holds the session + git worktree). |

Invalid values for any of the above fall back to their defaults (the same lenient-parse behavior as the core table); none of them is a boot guard. All seven are wired into the `api` service's `environment:` block in `docker-compose.yml` and documented in `.env.example`, so setting them in `.env` takes effect on the bundled stack.

`config.Load` also parses `WORKER_HEARTBEAT_INTERVAL` and `WORKER_POLL_INTERVAL` into the server's `Config` struct, but nothing on the API side reads those two fields back out (the sweeper acts on `WORKER_HEARTBEAT_STALE`, not on them) — they are not server knobs despite being parsed here. The values that actually matter are the **worker's own copy** of the same-named variables, consumed by the `agent` binary and wired to the `agent` compose service; see the Worker container table below.

### Worker container (`agent`)

Set on the `agent` compose service (profile `agent`) or on a standalone `docker run` — see [worker-setup.md](worker-setup.md).

| Var | Default | Notes |
|---|---|---|
| `UZI_API_URL` | — (required) | Base URL the worker uses to reach `api`. The bundled compose profile sets `http://api:8080` (the compose network); a remote/standalone worker points this at wherever `api` is actually reachable. |
| `UZI_WORKER_TOKEN` | — (required, unless `_FILE` is set) | The join token from Settings → Workers, sent as a Bearer credential on every worker call. |
| `UZI_WORKER_TOKEN_FILE` | — (optional, preferred) | Path to a file containing the join token; the worker reads it once at startup. The bundled compose profile mounts it as a read-only compose file secret at `/run/secrets/worker_token`, which the root-started entrypoint forces to `0400 worker`-owned so the cap-less `runner` uid cannot read it (PRD #51; a restricted-namespace non-root single-uid start runs without the split — the PRD #58 posture); the post-read unlink no-ops on the read-only mount, so the file persists and the uid boundary — not removal — is the close. Takes precedence over `UZI_WORKER_TOKEN` when set. Keeps the token out of every process's `/proc/<pid>/environ`; see [proc-hardening.md](proc-hardening.md). |
| `UZI_DATA_DIR` | `/data` | Persistent storage: the bare-clone cache, per-run git worktrees, and the pinned Claude Agent SDK session directory. Must be a durable volume for resume-after-restart to work. In the bundled compose profile this is **hardcoded** to `/data` (matching the `agentdata` volume mount), not read from `.env`; it's only a real knob for a standalone `docker run` or a compose override. |
| `UZI_WORKER_NAME` | container hostname | Display name shown in Settings → Workers / Admin. |
| `UZI_AGENT_VERSION` | `0.1.0-m4` | Version string the worker reports on `register`/`heartbeat`; informational only. |
| `UZI_EXECUTOR` | `sdk` | `sdk` runs real Claude Agent SDK turns (the product path); `stub` is a no-AI executor the project's own tests use — leave at `sdk` for real use. |
| `UZI_STUB_PLAN_GATE` | `false` | Only meaningful with `UZI_EXECUTOR=stub`: makes the stub drive the M4 plan-approval gate before committing, instead of committing immediately. Test/harness use only. |
| `WORKER_HEARTBEAT_INTERVAL` | `15s` | How often the worker posts a heartbeat. |
| `WORKER_POLL_INTERVAL` | `3s` | How often an idle worker asks the server for a run to claim. |
| `WORKER_MESSAGE_BATCH_INTERVAL` | `500ms` | How long the worker accumulates SDK output before a batched `POST .../messages` call. |
| `WORKER_HTTP_TIMEOUT` | `30s` | Per-request timeout on the worker's own control-plane HTTP calls to `api`. |
| `WORKER_PLAN_APPROVAL_TIMEOUT` | `24h` | How long a run may sit at `awaiting_approval` before the worker fails it: generous so a human has time, finite so an abandoned plan never pins its slot indefinitely — at the default cap of 1 (`WORKER_MAX_CONCURRENT_RUNS`) that means the whole worker. |
| `WORKER_MAX_CONCURRENT_RUNS` | `1` | How many runs this worker executes concurrently, each in its own slot; advertised at registration for the `active/cap` badge in Settings → Workers but never enforced server-side. Honored above a soft ceiling of 8 but warned at boot. See [worker-setup.md](worker-setup.md#concurrent-runs) for sizing guidance and the residuals of raising it. |
| `UZI_HOME_RECLAIM` | `true` | One-off startup sweep (PRD #108 M6) that reclaims `agent-home/<runId>` directories stranded by an older worker image's cleanup — a `0555`-mode Go module cache subtree that plain `fs.rm` could not remove (167.3 MB measured for one run). Deletes a directory only when the api positively reports its run **terminal** (`completed`/`failed`/`cancelled`); every other outcome — non-terminal, a could-not-ask (api down/5xx/timeout), or a 404 (the run row is already gone) — skips without deleting. The sweep bails after 3 consecutive could-not-ask failures (a 404 does not count and resets that streak) and is bounded by a 500-directory / 60s wall-clock budget per boot, so a hanging api cannot turn cleanup into hours of blocked startup; a later boot resumes where the last one stopped. `false`/`0` disables the sweep entirely — a destructive startup pass an operator cannot turn off is a bad thing to ship even with its guards right. |
| `UZI_LOG_LEVEL` | `info` | Worker log verbosity: `debug`/`info`/`warn`/`error`. At `debug` the worker also writes every raw run event (each `tool_use`, `tool_result`, status, etc.) to its stdout as it is emitted, so `docker logs uzi-agent-1` becomes the full-frame debug surface. Secrets are redacted before logging. `info` stays terse (no per-event lines). |

Duration values accept the same Go-style strings used server-side (`15s`, `3s`, `500ms`, `2h`) or a bare integer read as milliseconds.

**Proxied deployments and worker egress.** The worker's git operations (`gitEnv`) and the self-improvement check runner (`buildCheckEnv`, PRD #46 M9/M10) run under a scrubbed *replacement* environment — a deliberate security measure that keeps the join token and API URL out of worker-spawned git/test subprocesses. That replacement env does **not** carry `HTTP_PROXY`/`HTTPS_PROXY`/`NO_PROXY`. On the default internal-forge / direct-egress deployment this is fine (the e2e is green), but a deployment that reaches the forge (git push over HTTPS) or the npm registry (`npm ci` for the self-improvement job's test evidence) **through a proxy** must allowlist those vars in the two env builders, or the push / `npm ci` will fail (the checks then degrade to an honest "skipped", never a false failure).

## Chat (PRD #39)

The in-app chat agent ([chat.md](chat.md)) rides the run machinery as a `chat` run kind, so it reuses the run knobs above and adds its own. See [ARCHITECTURE.md](../ARCHITECTURE.md#chat-with-uzi-the-fifth-surface) for how they fit together.

**The worker's chat lifecycle windows are delivered by the `api` in the claim, not read from the worker's own env.** Set `WORKER_CHAT_IDLE_TIMEOUT`, `WORKER_CHAT_TURN_TIMEOUT`, and `CHAT_MAX_TURNS` on the **`api`** service — the worker prefers the per-claim value and treats its own same-named env var only as a fallback default when a claim omits one. This mirrors how `RUN_IDLE_TIMEOUT` etc. are shipped in the claim.

### Server (`api`)

| Var | Default | Notes |
|---|---|---|
| `CHAT_IDLE_TIMEOUT` | `70m` | Server-side idle sweep: completes a claimed/running chat whose newest message is older than this. A backstop **above** `WORKER_CHAT_IDLE_TIMEOUT` so the worker normally completes an idle chat first; this catches a dead/silent worker. |
| `WORKER_CHAT_IDLE_TIMEOUT` | `60m` | Worker-side idle window, **shipped in the claim**: the worker completes a parked chat after this with no new user message. |
| `WORKER_CHAT_TURN_TIMEOUT` | `10m` | Per-turn wall-clock cap, **shipped in the claim** (the idle timer re-arms on every SDK message, so a busy single turn needs its own bound). |
| `CHAT_MAX_TURNS` | `50` | Per-conversation turn cap. Enforced **server-side** (counted from persisted inputs, so a compromised worker can't exceed it) and also shipped in the claim; the seeded first message counts as turn 1. |
| `CHAT_RATE_LIMIT_MAX` / `CHAT_RATE_LIMIT_WINDOW` | `60` / `1m` | Per-user fixed-window limiter on chat create + message endpoints. |
| `PROPOSAL_RATE_LIMIT_MAX` / `PROPOSAL_RATE_LIMIT_WINDOW` | `20` / `1m` | Per-worker limiter on the `propose_issue` worker endpoint (bounds a prompt-injected propose loop; the per-run pending-proposal cap of 10 is a separate, non-env constant). |
| `PROPOSAL_CONFIRM_STUCK_TIMEOUT` | `2m` | The sweep reverts a proposal stranded in the transient `confirming` state (a crash between the claim-first flip and the forge write) back to `pending` after this. **Boot-clamped up to at least 2× `FORGE_HTTP_TIMEOUT`** so a slow-but-alive confirm is never reaped mid-flight and re-confirmed into a duplicate issue; `0` disables the sweep. |

### Worker container (`agent`)

| Var | Default | Notes |
|---|---|---|
| `WORKER_CHAT_POLL_MS` | `1000` | How often the worker polls the chat claim lane (`?lane=chat`) — faster than the run poll so a chat turn starts promptly. |
| `WORKER_CHAT_SESSIONS` | `1` | How many chat sessions the worker runs **concurrently** with its single run slot. `1` means one live conversation per user-worker; a second chat queues until the first ends. |

`CHAT_MAX_TURNS`, `WORKER_CHAT_TURN_TIMEOUT`, and `WORKER_CHAT_IDLE_TIMEOUT` are also parsed on the worker as fallback defaults, but the value shipped in the claim (from the `api` config above) wins — set them on `api`.

## Run judge and self-improvement (PRD #46)

See [judge.md](judge.md) and [self-improvement.md](self-improvement.md) for what these features do; both are also gated by settings in **Admin → Instance settings** (global on/off, judge model, self-improvement repo/interval), not env vars.

| Var | Default | Notes |
|---|---|---|
| `JUDGE_RATE_LIMIT_MAX` / `JUDGE_RATE_LIMIT_WINDOW` | `60` / `1m` | Per-user limiter on the "re-run judge" action. Dedicated budget, separate from `CHAT_RATE_LIMIT_MAX`/`_WINDOW` — re-running the judge and chatting don't share an allowance. |
| `UZI_SELFIMPROVE_CHECK_INTERVAL` | `1h` | How often the self-improvement engine **wakes** to check whether a cycle is due — the wake cadence, not the improvement interval itself (that's the `selfimprove_interval` admin setting, default `48h`). A boot pass runs immediately when enabled, so a cycle that came due while the API was down fires promptly rather than waiting a full interval. `0` disables the engine entirely (no boot pass, no loop). |

## uzi CLI (PRD #64)

See [cli.md](cli.md) for the user-facing install/auth/command guide. The one
server-side knob:

| Var | Default | Notes |
|---|---|---|
| `CLI_POLL_RATE_LIMIT_MAX` / `CLI_POLL_RATE_LIMIT_WINDOW` | `60` / `1m` | Dedicated budget for `POST /api/auth/cli/poll` (the `uzi login` poll loop). Deliberately separate from the shared `authLimiter` (`RATE_LIMIT_MAX`/`_WINDOW`, `10`/`1m`): an RFC 8628 poll at the server-returned 5s interval is 12/min, which would trip the shared limiter at poll #11. This budget and the returned poll interval are one decision — if the interval ever changes, this must still comfortably exceed it. |

## Claude rate limits (PRD #53)

See [rate-limits.md](rate-limits.md) for what the meters show. The background poller reads each user's Anthropic 5-hour/7-day rate-limit windows with their own token; see [ARCHITECTURE.md](../ARCHITECTURE.md) for the poller design.

| Var | Default | Notes |
|---|---|---|
| `UZI_USAGE_POLL_INTERVAL` | `5m` | How often the poller ticks. `0` disables the engine entirely (existing rows are still served, always marked stale). A nonzero value below `1m` is clamped up to `1m` with a boot warning — the header-probe fallback spends a user's own Anthropic tokens, so a too-tight interval is a footgun, not a convenience. |
| `UZI_USAGE_PROBE` | `true` | `false` disables the ~1-token header-probe fallback entirely; affected users (whose credential the free usage endpoint refuses) then show `unavailable` instead of a reading. See [rate-limits.md](rate-limits.md#the-probe-and-turning-it-off) for the token-cost accounting. |
| `UZI_ANTHROPIC_HTTP_TIMEOUT` | `15s` | Hard per-call timeout on every outbound request to Anthropic (usage endpoint and header probe), mirroring `FORGE_HTTP_TIMEOUT`'s and `UZI_OIDC_HTTP_TIMEOUT`'s posture. |
