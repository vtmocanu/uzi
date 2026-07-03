# uzi — AI Design & Implementation Decisions

Decisions the AI made within the human constraints in [specs/human.md](./human.md).
Records the *why* and the human item each serves. This file is replaceable: a
rebuild may decide differently, but must still satisfy every item in human.md.

Behavior and constraints are authoritative here; specific file paths/library
versions are given as the current realization, not a requirement.

---

## 1. Stack

Serves human: "Stack: Go API + React/Vite SPA"; "PostgreSQL"; "local docker-compose".

- **Backend**: Go, `chi` router, `pgx/v5` (pgxpool) driver, `sqlc` for typed
  query code, `golang-jwt/v5`, `golang.org/x/crypto/argon2`.
  - Why: multica-proven combination; sqlc gives compile-checked SQL without an ORM.
- **Frontend**: React 18 + Vite + TypeScript SPA, Tailwind for styling.
  - Why: SPA is simpler than multica's Next.js — no SSR needed for this shell.
- **DB**: PostgreSQL 17 (`postgres:17`), named docker volume `pgdata` for persistence.
  - Why: serves the "persistent storage" requirement; survives `compose down/up`.
- **Migrations**: `goose` (standard tool).
  - Why: deliberate improvement over BOTH inspirations' bespoke runners
    (multica's ~130 numbered migrations / 176 up-files; bottega's hand-rolled
    idempotent in-code migrations).

Repo layout: `api/` (Go module, `cmd/server` + `internal/{config,store,auth,middleware,handler,httpx}`),
`web/` (Vite app + `nginx.conf` + `Dockerfile`), `docker-compose.yml`, `scripts/smoke.sh`.

## 2. Password hashing

Serves human: "user/password stored in DB"; best-practice bar.

- **argon2id**, OWASP Password Storage params: m=19456 KiB (19 MiB), t=2, p=1,
  16-byte salt, 32-byte key.
- Stored in **PHC format** `$argon2id$v=19$m=19456,t=2,p=1$<b64salt>$<b64hash>`;
  params are **parsed from the stored hash on verify**, so cost can be raised
  later without breaking existing hashes.
- Constant-time comparison (`crypto/subtle`).
- Why argon2id over bcrypt: bottega uses bcrypt(12); OWASP prefers argon2id.

## 3. Sessions / JWT

Serves human: "auth flow: password + revocation".

- **JWT HS256**, signed with `JWT_SECRET`. Payload: `{user_id, token_version}`
  plus registered claims (`sub`, `iat`, `exp`).
- Delivered in an **HttpOnly, SameSite=Strict** cookie (`uzi_auth`);
  `Secure` flag derived from `FRONTEND_ORIGIN` scheme (https ⇒ Secure).
  - Why cookie (not localStorage): beats bottega's localStorage (XSS-readable).
- **TTL** default 168h (7d) via `AUTH_TOKEN_TTL`.
- Parse pins the algorithm to HS256 (`WithValidMethods`) to block alg-confusion
  / `alg=none` forgery.
- **Boot guard**: server refuses to start if `JWT_SECRET` is missing, empty, a
  known placeholder, or shorter than 16 chars. No dev fallback (multica shipped
  a footgun dev fallback; bottega refused — we adopt bottega's stance).

## 4. Revocation (token_version)

Serves human: "revocation".

- `users.token_version` (int, default 1) is embedded in the JWT and compared
  against the DB **on every authenticated request**.
- Bump triggers (each kills ALL of the user's live sessions): **logout**,
  **password change** (future), **admin deactivation**.
  - Why: real revocation — beats multica (HttpOnly cookie but NO revocation).
- Every authenticated request also **re-reads the user from the DB** and rejects
  inactive accounts. `is_admin` is always read from the DB row, **never trusted
  from a claim**.

## 5. Rolling refresh (improvement over inspirations)

- The auth cookie is re-issued **only once the token is past half its TTL**
  (not on every request as bottega/multica-style rolling would).
  - Why half-TTL: keeps active users logged in while shrinking the window in
    which concurrent requests could observe a mismatched cookie pair.
- Both cookies (`uzi_auth` + `uzi_csrf`) are **always re-issued together** because
  the CSRF token is HMAC-bound to the JWT (see §6).

## 6. CSRF

Serves human: "auth flow" (state-changing safety).

- Readable (non-HttpOnly) cookie `uzi_csrf` = `hex(nonce).hex(HMAC-SHA256(nonce, jwt))`.
- SPA echoes it in the `X-CSRF-Token` header; server recomputes the HMAC over
  the auth cookie's JWT and compares (`hmac.Equal`) — HMAC-bound double-submit,
  not a plain cookie==header compare.
- Enforced only on **state-changing methods when cookie-authenticated**; safe
  methods (GET/HEAD/OPTIONS) always pass.
  - Why HMAC-bound: an attacker who plants a cookie on a sibling subdomain still
    can't forge a valid token without the JWT. Pattern from multica.

## 7. First-user-becomes-admin

Serves human: "admin user list" (needs a first admin) + "open self-registration".

- Open self-registration; the **first** account ever created gets `is_admin=true`.
- Check-and-insert runs in **one transaction holding `pg_advisory_xact_lock`**
  (fixed key) so two concurrent first-registrations cannot both become admin.
  - Why the lock: bottega's in-tx re-check is only race-safe on SQLite's single
    writer; Postgres READ COMMITTED would let both concurrent txns see zero users.
    The advisory xact lock serializes registrations on Postgres.

## 8. Auth endpoints

- `POST /api/auth/register` — email + password. Email lowercased+trimmed and
  validated (`net/mail`). Password min 12 chars enforced **server-side**
  (client adds strength feedback only), max 1024 (argon2 DoS guard). Logs the
  new user in (sets cookies). Returns 201.
- `POST /api/auth/login` — verify hash; generic "invalid credentials" for both
  unknown-email and wrong-password. **Timing equalized**: on unknown email,
  verify against a precomputed dummy hash so response time doesn't leak account
  existence. Deactivated accounts get 403. Updates `last_login`. Sets cookies.
- `POST /api/auth/logout` — bumps `token_version`, clears both cookies.
- `GET /api/auth/me` — current user (safe DTO; never returns hash/token_version).
- `GET /api/admin/users` — admin-only list.
- `PATCH /api/admin/users/{id}` — admin-only activate/deactivate. Deactivation
  bumps the target's `token_version` (kills live sessions). **Self-lockout guard**:
  an admin cannot deactivate their own account.
- `GET /api/health` — pings DB; used by the compose healthcheck chain.

Router: rate limiter applied **per-route** to register + login only;
`RequireAuth` group for logout/me; `RequireAuth`+`RequireAdmin` for `/admin/*`.

## 9. Delivery / container hardening

Serves human: "local docker-compose demo".

- **nginx** (`nginxinc/nginx-unprivileged`, uid 101, listens on 8080) serves the
  built SPA and proxies `/api` to the api service **same-origin** — no CORS by
  design. Restrictive CSP (`default-src 'self'`, no inline script/eval),
  `X-Content-Type-Options`, `X-Frame-Options DENY`, `Referrer-Policy no-referrer`.
  SPA fallback (`try_files … /index.html`).
- **api** image is `gcr.io/distroless/static-debian12:nonroot`; static CGO-off
  build; migrations embedded via `go:embed` so the runtime image needs only the
  binary. Container healthcheck is the binary probing itself (`/server -health`)
  because distroless has no shell/curl.
- **All base images are digest-pinned** (postgres, golang, node, nginx, distroless).
- **Only `web` is published**, at `127.0.0.1:8080`. `db` and `api` are
  unpublished — reachable only on the private compose network.
  - Why loopback binding: copies multica's `127.0.0.1` pattern; nothing exposed
    on the LAN.

## 10. Migrations at startup

Serves human: "docker compose up" one-command demo.

- Migrations run automatically at api startup, **before** serving, via a
  temporary `database/sql` DB opened with the pgx stdlib driver (`goose` needs
  `*sql.DB`); the app then uses a `pgxpool` for queries.
- Startup **waits/retries** for Postgres to be reachable (belt-and-suspenders
  alongside compose `depends_on: service_healthy`).

## 11. Rate limiting

Serves human: best-practice / abuse resistance.

- In-process **per-IP fixed-window** limiter (no Redis dependency for the MVP,
  unlike multica), keyed by `(route, client-IP)`, with a **background sweeper**
  evicting expired buckets. Defaults: 10 requests / 1m (`RATE_LIMIT_MAX`,
  `RATE_LIMIT_WINDOW`).
- Client IP honored from `X-Forwarded-For` **only when the direct peer
  (RemoteAddr) is within `TRUSTED_PROXIES` CIDRs**; otherwise a spoofed XFF is
  ignored. nginx **overwrites** XFF with `$remote_addr` (not
  `$proxy_add_x_forwarded_for`), discarding any client-supplied value. Empty
  `TRUSTED_PROXIES` ⇒ never trust XFF.

## 12. Users schema

```
users(
  id            uuid PK default gen_random_uuid(),
  email         text UNIQUE NOT NULL,   -- stored lowercased + trimmed
  password_hash text NOT NULL,
  display_name  text,
  is_admin      boolean NOT NULL default false,
  is_active     boolean NOT NULL default true,
  token_version int     NOT NULL default 1,
  created_at    timestamptz NOT NULL default now(),
  last_login    timestamptz
)
```

`pgcrypto` extension enabled for `gen_random_uuid()`.

## 13. Configuration (env)

| Var | Default | Notes |
|---|---|---|
| `JWT_SECRET` | — (required, boot guard) | HS256 key; `openssl rand -hex 64` |
| `DATABASE_URL` | set by compose | pgx pool DSN |
| `POSTGRES_USER/PASSWORD/DB` | `uzi`/required/`uzi` | compose-internal |
| `API_ADDR` | `:8080` | api listen addr (unpublished) |
| `AUTH_TOKEN_TTL` | `168h` | session TTL + cookie MaxAge |
| `FRONTEND_ORIGIN` | `http://127.0.0.1:8080` | scheme derives cookie `Secure` |
| `RATE_LIMIT_MAX` / `RATE_LIMIT_WINDOW` | `10` / `1m` | register+login |
| `TRUSTED_PROXIES` | compose: private CIDRs + loopback | XFF trust set |

## 14. Verification

- `scripts/smoke.sh`: scripted e2e scenario
  (register → login → dashboard → admin → deactivate → revocation →
  persistence-across-restart), incl. a concurrent-first-registration race test.

## 15. Known / accepted limitations (documented, not bugs)

- **Email enumeration** via duplicate-email 409 on register — accepted for an
  internal MVP (noted against OWASP ASVS L1). (Login itself is
  timing-equalized and generic, so login does not leak existence.)
- In the local compose NAT topology **all clients share one rate-limit bucket**
  (they arrive as the nginx hop / NAT address). Fail-safe; per-client
  granularity needs a front proxy that preserves real client IPs.
- No password reset / email verification / 2FA (deferred; see human.md).
- Secret scanner and CI are deferred. Minor-tag → digest pinning is done.
- **`UZI_SECRET_KEY` rotation invalidates every stored secret** (§16): no
  re-encrypt path; each user re-pastes. Accepted MVP limitation, documented.

---

# PRD #3 — Agent templates & per-user Anthropic token

## 16. Shared secret-at-rest (secretbox + `UZI_SECRET_KEY`)

Serves human: "Anthropic token encrypted in the DB"; best-practice bar.

- **`api/internal/secretbox/`**: AES-256-GCM (`Seal`/`Open`), per-message 12-byte
  random nonce prepended to ciphertext; GCM gives integrity (tampered row →
  decrypt error, not garbled plaintext). `LoadKey` reads a base64 32-byte key
  from an env var; empty ⇒ treated as unset.
  - Why: multica's secretbox pattern, applied to provider creds (which multica
    itself stored plaintext). NaCl-style name, AES-GCM construction.
- **Boot guard**: `config.Load` calls `secretbox.LoadKey("UZI_SECRET_KEY")`;
  missing / non-base64 / wrong-length aborts start **before** any DB connection.
  Key held for the process lifetime as one shared `*secretbox.Box` (`main.go`),
  not re-derived per request.
- **Provenance**: the secretbox util + config wiring were cherry-picked
  **byte-identical** from PRD #2's branch (958f9b3 → e6f17cb) so the two parallel
  branches merge conflict-free (whichever lands M1 first ships it). Its comments
  are still bot-PAT-oriented (PRD #2's use); genericizing them is a tracked
  post-merge follow-up, not a behavior change.
- **Rotation** invalidates all stored secrets across every feature at once
  (§15). Accepted, matches PRD #2.

## 17. `user_secrets` table + Anthropic token API

Serves human: "each user stores their own Anthropic token via the webui,
encrypted in the DB".

- **Generic kind-keyed table** (migration `00010`), chosen over a column on
  `users` so the next secret kind needs no shape change:
  ```
  user_secrets(
    id uuid PK, user_id uuid → users ON DELETE CASCADE,
    kind text CHECK (kind IN ('anthropic_token')),  -- new kind = one ALTER-CHECK migration
    ciphertext bytea,                                -- secretbox.Seal(secret)
    created_at, updated_at timestamptz,
    UNIQUE(user_id, kind)
  )
  ```
- **API** (current user only; no admin path to another user's value):
  - `PUT /api/me/secrets/anthropic_token` `{token}` → sanity-check → `Seal` →
    upsert. **Response is metadata-only** `{kind, created_at, updated_at}`.
  - `GET /api/me/secrets` → metadata list.
  - `DELETE /api/me/secrets/anthropic_token` → idempotent (absent ⇒ 204).
- **Token sanity check**: trimmed, non-empty, **1–4096 bytes**, no interior
  whitespace/control chars. **No format assumption** (no `sk-ant-` prefix check —
  Anthropic prefixes are not a documented contract); accepts both `setup-token`
  OAuth tokens and console API keys.
- **No reveal endpoint** (re-paste to rotate; multica's reveal+audit is more than
  MVP needs). **No live verification** (validated on first agent run, PRD #4 —
  avoids burning quota / hard-coding endpoint behavior).
- **Redaction**: token never logged, never in error strings; validation errors
  carry no token bytes. Enforced by a handler-level redaction test grepping logs
  for the plaintext fixture.

## 18. `agent_templates` store + builtins reconciler

Serves human: "agent templates stored in DB, editable via the UI (agents
themselves sit with code)".

- **Schema** (migration `00011`): `id, name (UNIQUE), description, model (NULL=inherit),
  tools (jsonb, NULL=inherit all), prompt_body, is_builtin, updated_by (→users ON
  DELETE SET NULL), created_at, updated_at`. No versioning/history in MVP;
  `updated_by`/`updated_at` give minimal attribution.
- **Builtin source of truth is Go, not SQL**: the repo's seven `.claude/agents/*.md`
  (coder, reviewer, auditor, tester, documenter, fact-checker, spec-keeper) are
  embedded **byte-identical** under `api/internal/agenttmpl/builtins/` (embed can't
  reference paths outside the module root, so they are copies; a **drift test**
  enforces they stay identical to the checked-in originals), parsed once at
  package init.
  - Why Go over a SQL seed: an **idempotent startup reconciler**
    (`store.ReconcileBuiltinTemplates`, `ON CONFLICT DO NOTHING`) inserts missing
    builtins and **never overwrites** an existing row, so admin edits survive
    restarts and future releases can add/upgrade builtins without a
    non-re-runnable seed.
- **Builtin lifecycle**: editable; **not deletable** (`DELETE` → 409); **Reset**
  (`POST /:id/reset`, builtins only, 400 on non-builtin) re-applies the embedded
  definition. Guarantees the core roles always exist for PRD #4.

## 19. Renderer (template → Claude Code subagent Markdown)

Serves human: templates are the definition PRD #4 will consume.

- **`agenttmpl.Render`** is a **pure function**, no DB dependency (so golden-file
  tests can pin output). Fixed field order: **name, description, tools, model**;
  `tools`/`model` lines **omitted when empty** (inherit).
- **`tools` is an inline comma-separated string** (`tools: Bash, Read, …`), not a
  YAML sequence — matches this repo's own `.claude/agents/*.md`. Built with
  ordered `strings.Builder`, **not `yaml.Marshal`** (a map marshal would reorder
  keys and break byte-stability).
- **Byte-match guarantee**: a builtin's rendered output byte-matches the
  checked-in source file, pinned by golden-file tests.
- **`GET /api/agent-templates/:id/rendered`** serves the Markdown **raw**
  (`text/markdown`, `X-Content-Type-Options: nosniff`), not wrapped in JSON, for
  any authenticated user.

## 20. Template validation & frontmatter/secret hardening

Serves human: best-practice bar; admin-edited free-form content is untrusted.

- **`name`**: kebab-case `^[a-z0-9]+(-[a-z0-9]+)*$`, unique, and **immutable after
  creation** (structural: `UPDATE` never touches `name`) — it is the subagent
  filename + PRD #4 routing key; rename = create-new + delete-old (non-builtins;
  builtins never renamed).
- **`description`, `prompt_body`**: non-empty. **`model`**: NULL or non-empty token
  (no CHECK — upstream accepts aliases *and* full model IDs, incl. `fable`).
  **`tools`**: NULL or JSON array of non-empty strings; `[]` **normalized to NULL**
  (inherit-all) so empty-list and inherit render identically.
- **Frontmatter-injection hardening**: reject control chars
  (`unicode.IsControl` + U+FFFD) in `description`, `model`, and each tool name,
  and **commas in tool names** (they'd break the inline comma-joined `tools`
  line). `prompt_body` is **exempt** (renders after the frontmatter, its newlines
  are ordinary Markdown).
- **Secret guardrail**: server **hard-rejects** a high-confidence full `sk-ant-…`
  token in `description`/`prompt_body`; the **UI warns** (non-blocking) on looser
  credential-ish patterns and unknown tool names, so prompts that merely *mention*
  token formats stay savable.
- **Concurrency**: last-write-wins (`updated_at`/`updated_by` attribute it); an
  `If-Match`-style precondition is a noted follow-up, out of scope.

## 21. Template API surface & authorization

Serves human: "editable via the UI"; admin-only writes (USER-CONFIRMED 2026-07-03).

- **Any authenticated user**: `GET /api/agent-templates`, `GET /:id`,
  `GET /:id/rendered` (they'll run these roles in PRD #4).
- **Admin only** (`RequireAdmin`): `POST`, `PUT /:id` (name ignored — immutable),
  `DELETE /:id` (409 for builtins), `POST /:id/reset` (400 for non-builtins).
  - Why admin-write / all-read: closes bottega's hole where any user rewrites the
    shared prompts everyone else's agents run with. Per-user forks deferred to
    PRD #4.
- All under PRD #1 session + CSRF.

## 22. Tooling & parallel-safety (PRD #2/#3 coordination)

- **Goose version ranges reserved**: PRD #2 `00002`–`00009`, PRD #3 `00010`+.
  Duplicate goose versions from parallel branches merge cleanly in git but fail
  at `goose up`; reserving ranges prevents the silent-until-runtime break.
- **Shared frontend shell files** (sidebar nav, Settings layout, route table) are
  touched by both PRDs — expected small merge conflicts, kept in dedicated commits.
- **sqlc** regen via pinned `go run github.com/sqlc-dev/sqlc/cmd/sqlc@v1.30.0 generate`.
- **Build gate** uses `-buildvcs=false` (worktree VCS-stamp quirk).
- **E2E**: `scripts/smoke-prd3.sh` — seed → admin edit → non-admin blocked (403) →
  render stable → token set/rotate/delete → restart persistence → DB dump shows
  only ciphertext.
