# PRD #5: Access Control & PAT Least-Privilege Hardening

**GitLab Issue**: [vtmocanu/uzi#5](https://gitlab.example.com/vtmocanu/uzi/-/issues/5)
**Status**: Draft
**Priority**: High
**Created**: 2026-07-04
**Depends on**: PRD #1 (auth, done); PRD #2 (forge connections + repos, done). **No dependency on PRD #4** — chosen precisely so it can run in parallel with PRD #4's remaining milestones (M3–M7), which live in `agent/`, the worker-protocol handlers, and the run-view UI. See "Parallel-safety" below.

## Problem

Two open plan.md items, both security-shaped, neither touched by PRDs #1–#4:

1. **Registration is wide open** (plan.md line 54: "we should allow registration only from @example.com email addresses - configurable"; line 65: "enable/disable registration for users"). Today `POST /auth/register` accepts any syntactically valid email. The compose MVP runs on a laptop, but the moment uzi is reachable by anyone else (k8s deploy is on the backlog), open registration is an unauthenticated door into a system that stores encrypted PATs and Anthropic tokens and will soon command agents.
2. **uzi never verifies the bot PAT is least-privilege** (plan.md line 48: "can uzi verify the glpat does not have more permissions than needed for a MR? for each repo? when we save it? and afterwards?"). PRD #4's own risk section calls this out: under prompt injection, the blast radius **is** the PAT's privilege. plan.md line 43 makes it a primary directive: agents "should not be able to modify resources, code on main directly etc". Today a user can paste a `sudo`-scoped instance-admin PAT and uzi will happily encrypt and use it; a bot added as Maintainer/Owner can push to `main` and rewrite repo settings, and nothing warns anyone. GitLab-side role enforcement is one of PRD #4's guardrail layers — this PRD makes that layer *verified* instead of *hoped*.

## Solution Overview

Two independent workstreams, one PRD (both are thin, same theme: tighten who gets in and what the credentials can do):

1. **Registration controls (server + web)** — an operator-configured email-domain allowlist (`UZI_ALLOWED_EMAIL_DOMAINS`, empty = allow all, preserving current behavior) and a registration kill-switch (`UZI_REGISTRATION_ENABLED`, default on). Enforced server-side in the register handler; surfaced to the SPA via a small unauthenticated auth-config endpoint so the register form can hide itself / hint the allowed domains instead of failing after submit.
2. **PAT least-privilege verification (server + web)** — a privilege checker that runs (a) at connection save, (b) on demand from Settings, and (c) periodically in the background, and answers plan.md's three questions:
   - **when we save it**: token-level checks — scopes must be exactly `api` (the documented minimum for uzi, see `docs/gitlab-bot-setup.md`; anything more — `sudo`, `admin_mode`, additional scopes — is over-privilege), token active/not expired, bot user is not an instance admin. Violations **block the save** with a precise error.
   - **for each repo**: the bot's effective role on every *enabled* repo must be exactly Developer (access_level 30). Maintainer/Owner (≥40) is a violation (can push protected branches, edit repo settings — breaks the primary directive). Additionally the repo's default branch must be protected and not pushable by Developers, otherwise "bot = Developer" protects nothing.
   - **and afterwards**: a periodic re-check (default daily) re-runs everything and stamps the results; the UI shows per-connection and per-repo badges, so privilege drift (someone bumps the bot to Maintainer, unprotects main, or rotates in a fatter token) becomes visible without anyone asking.

Per-repo findings **warn, not block** — membership changes happen on the forge after save, repos are enabled/disabled over time, and blocking issue-sync over a role finding would punish the wrong action. Token-level findings block at save because that is the one moment uzi holds the plaintext and the user is present to fix it.

### Inspiration check (per plan.md)

None of the three inspirations does any of this: bottega has no registration control and shares one host `gh` identity (its audited weakness); multica stores plaintext creds and never inspects them; dot-agent-deck delegates auth entirely to the agent's ambient credentials. This PRD is net-new ground — the comparison table stays in PRD #4; the relevant prior art is GitLab's own token/membership introspection APIs.

## Technical Design

### Registration controls

Config (`api/internal/config`, env-var pattern as everything else — DB-backed admin settings are deliberately deferred, see Out of scope):

- `UZI_REGISTRATION_ENABLED` — bool, default `true`. When `false`, `POST /api/auth/register` returns 403 with a static message; login is unaffected. (Both policy rejections — disabled and domain-not-allowed — use **403** with distinct messages: the request is well-formed, the policy forbids it; 400 stays for malformed input as today.)
- `UZI_ALLOWED_EMAIL_DOMAINS` — comma-separated list (e.g. `example.com`), matched case-insensitively, exact match only (no subdomain wildcards — `a.example.com` ≠ `example.com`). Empty/unset = all domains allowed (today's behavior; compose demo stays zero-config). The domain is extracted by splitting the **parsed** addr-spec (`mail.ParseAddress(...).Address`) on its final `@` — never the raw input, which `mail.ParseAddress` also accepts in display-name/comment forms (`Alice <alice@example.com>`) whose raw final-`@` suffix is junk; quoted local parts containing `@` still yield the true domain under a final-`@` split of the addr-spec (fact-checked empirically on Go 1.26).

Enforcement lives in `Register` (`api/internal/handler/auth.go`) after the existing `mail.ParseAddress` check, before any DB work — and *only* there, never in a shared helper the seed path could inherit. The **seed path is exempt**: the operator's admin is created by `seedAdmin()` in `api/cmd/server/main.go` (direct `CreateUser`, never through the handler; `api/internal/seed` seeds only the forge *connection*). The operator sets both the seed email and the allowlist, so gating one on the other would only create bootstrap deadlocks. Non-ASCII/IDN domains are matched byte-wise after lowercasing (no IDNA folding — irrelevant for `example.com`, noted for completeness). Rejection messages are specific ("registration is restricted to: example.com" / "registration is disabled") — domain-list disclosure is acceptable for an internal tool and the register page will display the same hint anyway.

New unauthenticated endpoint `GET /api/auth/config` → `{"registration_enabled": bool, "allowed_email_domains": [...]}`. Note this is uzi's **first unauthenticated JSON surface besides `/health`** — the existing `ForgeConfig` endpoint is *not* a precedent (it sits inside the `RequireAuth` group, `handler.go`). It must be registered outside `RequireAuth`, return only operator-set, user-visible policy (nothing else, ever — this endpoint's shape is a security boundary), and sit behind the auth rate limiter like register/login. The register page consumes it: registration disabled → the register form/route is replaced by a "registration is disabled" notice (login untouched); domains restricted → hint under the email field and client-side pre-validation (server remains authoritative).

### PAT least-privilege verification

**Forge interface additions** (`api/internal/forge/forge.go`, GitLab driver in `gitlab.go` — same neutral-domain-type discipline as the existing six methods):

```go
// TokenInfo returns introspection data for the PAT the client authenticates
// with: scopes, active, expiry. GitLab: GET /personal_access_tokens/self.
TokenInfo(ctx context.Context) (TokenInfo, error)
// ProjectRole returns the bot's effective (direct or inherited) access level
// on a project, and whether the bot is a member at all.
// GitLab: GET /projects/:id/members/all/:user_id (404 = not a member).
ProjectRole(ctx context.Context, projectID, forgeUserID int64) (role int, member bool, err error)
// DefaultBranchProtection reports whether the given branch is protected and
// whether Developer-level (30) push is allowed on it.
// GitLab: GET /projects/:id/protected_branches/:name (404 = unprotected).
DefaultBranchProtection(ctx context.Context, projectID int64, branch string) (BranchProtection, error)
```

The admin flag needs **no new method**: `VerifyToken` already calls `GET /user`; `BotIdentity` grows an `IsAdmin bool` field populated from that same response (GitLab includes `is_admin` only when the caller is an admin — absent decodes to `false`, which is exactly the non-admin pass case; fact-checked 2026-07-04). Note the interface growth (3 methods here + `CreateIssue` from PRD #4 M5) breaks every hand-written fake at compile time — see parallel-safety.

**Checker service** (`api/internal/privcheck`, new package — keeps `forgesvc` focused on sync): given a connection, produce a `PrivilegeReport`:

```
report = {
  checked_at,
  token:  { scopes, active, expires_at, violations: []string },   // e.g. "scope sudo beyond required api", "token expires within 14 days" (warning), "bot user is an instance admin"
  repos:  [ { repo_id, path, role, violations: []string } ],       // e.g. "bot role is Maintainer (40), expected Developer (30)", "default branch main is not protected", "Developers may push to protected main"
  status: ok | warnings | violations
}
```

Rules, restated precisely:
- scopes must equal `{api}` — uzi's documented minimum (`docs/gitlab-bot-setup.md` explains why `read_api` is insufficient). Fewer ⇒ the connection wouldn't work at all (VerifyToken/ListProjects already fail today); more ⇒ over-privilege violation.
- `active == true`; `expires_at` absent or > now. Expiry within 14 days ⇒ *warning* (advance notice, not a violation).
- `is_admin == false` ⇒ else violation (an instance-admin PAT with `api` scope is effectively god-mode).
- per enabled repo: effective access level `== 30`. `> 30` ⇒ violation. `< 30` or **no membership at all** (the `/members/all/:user_id` lookup 404s — bot removed or demoted after the repo was enabled) ⇒ explicit finding ("bot is no longer a Developer member of this repo; sync is broken"), since repos are not auto-disabled on downgrade. (`< 30` can't appear via `ListProjects` — it filters `min_access_level=Developer` — but the per-repo check is independent of that listing.)
- per enabled repo: default branch protected, and its push access levels do not include 30 (or 0/no-one only) ⇒ else violation. Uses the repo's stored `default_branch`; repos with no default branch (empty projects) are skipped with a note.

**When it runs**:
1. **Save time** (`CreateConnection`): after the existing `VerifyToken`, run token-level checks (scopes/active/expiry/admin) against the plaintext token. Any token-level *violation* ⇒ 422 with the violations listed, nothing stored. (Per-repo checks can't run here — repos aren't enabled yet.)
2. **On demand**: `POST /api/forge/connections/{id}/privilege-check` runs the full report (token + all enabled repos), persists it, returns it. Owner-only authz; **behind the per-user forge rate limiter** like every forge-proxying route (it is the heaviest of them: 1 + 2×repos upstream calls). The Settings page gets a "Check privileges" button next to the existing "Verify".
3. **Periodic**: a background loop (`UZI_PRIVILEGE_CHECK_INTERVAL`, default `24h`, `0` disables), modeled on the **worker sweeper, not the poller**: it runs an immediate pass at boot (`Boot()`-style, like `main.go`'s worker sweeper) so pre-existing/never-checked connections get a report right after deploy, then ticks on the interval. Per-repo fan-out uses bounded concurrency (same `maxConcurrency=4` discipline as the poller) to stay polite to gitlab.example.com. Failures (forge unreachable, token revoked) are recorded *in the report* rather than crashing the loop — a revoked token is exactly what the report must surface; a connection or repo deleted mid-sweep is a 0-rows-affected write-back, tolerated silently. Single-instance assumption (compose has one API): like the existing poller and sweeper, this loop has no leader election — a multi-replica k8s deploy needs that for all three loops (noted, deferred with the k8s work).

**Persistence** (goose migration `00030`+ — range reserved above PRD #4's `00020+`, same gap convention):

```sql
ALTER TABLE forge_connections ADD COLUMN privilege_report jsonb,
                              ADD COLUMN privilege_checked_at timestamptz,
                              ADD COLUMN privilege_status text;  -- ok|warnings|violations|error, denormalized for cheap list queries & badges
```

One jsonb report per connection (repo findings embedded) — a normalized findings table is over-modeling for "show the current state"; history/audit-log is out of scope. All columns nullable: `NULL` status = **never checked** (every pre-existing connection at migration time), rendered as an explicit "unchecked" badge, never as ✓. The boot sweep (above) back-fills these within seconds of the first deploy, so grandfathered over-privileged tokens surface immediately, not one interval later.

**Surfacing (web)**:
- Settings → Forge: each connection card shows a privilege badge (`least-privilege ✓` / `N warnings` / `N violations` / `unchecked`, with `checked_at`), expandable to the finding list; "Check privileges" button.
- Repos page: per-repo badge on repos with findings (role / branch-protection issues), tooltip with the specific violation.
- Connect form: a save rejected with 422 renders the violations as the error (e.g. "PAT has scopes [api sudo]; only api is allowed — mint a new token, see the bot setup doc"), linking `docs/gitlab-bot-setup.md`.

Admin cross-user visibility is *not* in scope (admins can already see workers/runs in PRD #4; a fleet-wide privilege dashboard is a later item).

### Parallel-safety vs PRD #4 (M3–M7 in flight)

Reviewed 2026-07-04 against PRD #4 M3–M7 (blocker in first review draft: the original table undersold the overlap). The honest list:

| Shared edit point | This PRD | PRD #4 (M3–M7) | Merge risk |
|---|---|---|---|
| `handler/auth.go`, `config` parsing, web register page | registration controls | not touched | none |
| `forge/forge.go` + `gitlab.go` | +3 interface methods, `BotIdentity.IsAdmin` | M5 adds `CreateIssue` | additions on both sides, **but every interface growth compile-breaks all hand-written fakes** (`forgesvc/sync_test.go`, `seed/seed_test.go`, board/handler fakes) — the second branch to land updates all fakes, not just resolves a two-line conflict |
| `handler/handler.go` `Routes()` (+ possibly `Handler`/`New()`) | `/auth/config`, `/privilege-check` routes | M5 adds run-view/WS/issue routes | textual conflicts in one function — mechanical but certain; second-lander rebases |
| `api/cmd/server/main.go` background wiring (`bgWG` block) | third loop: privilege sweep | M4/M5 adjust run/worker wiring | same block; coordinate |
| `docs/gitlab-bot-setup.md` (protected-branch section!), `docs/configuration.md`, README | M6 writes all three | M7 writes all three | **both PRDs add a protected-branch section to the same doc** — whoever lands second merges the two into one section, not two |
| web Settings page | Forge section badges | M5 adds Workers section | same page, different sections — coordinate |
| `handler/forge.go`, new `privcheck` pkg, migration `00030+` | yes | no (worker-protocol files, `00020+`) | none (reserved ranges) |

**Merge-owner rule**: whichever branch merges to the integration branch second owns fake updates, `Routes()`/`main.go` conflict resolution, and the doc-section merge — and runs the full test tree before declaring done (per the PRD #4 lesson: lenient fakes hide wire truth).

## User Journey

1. Operator sets `UZI_ALLOWED_EMAIL_DOMAINS=example.com` in `.env`. A contractor tries to register with `someone@gmail.com` → the form already hints "@example.com only" and the server rejects anyway. A colleague with `@example.com` registers normally.
2. Operator later sets `UZI_REGISTRATION_ENABLED=false` → the register route shows "registration is disabled"; existing users log in as always.
3. A user connects a bot PAT they minted with scopes `api, read_user, sudo` → save is rejected: "PAT scopes [api read_user sudo] exceed the required [api]" with a link to the setup doc. They mint a clean `api`-only token; save succeeds; the connection card shows `least-privilege ✓`.
4. Weeks later a well-meaning teammate promotes the bot to Maintainer on one project "so it stops asking". Next daily sweep: the connection badge flips to `1 violation`, the repo shows "bot role is Maintainer (40), expected Developer (30)". The user demotes the bot, hits "Check privileges", badge returns to ✓.
5. A repo's `main` was never protected. The report says so — this would otherwise silently void PRD #4's GitLab-side guardrail layer. The user protects the branch per the setup doc's new section.

## Milestones

- [ ] **M1 — Server: registration controls**: `UZI_REGISTRATION_ENABLED` + `UZI_ALLOWED_EMAIL_DOMAINS` config parsing/validation; enforcement in `Register` only (403 for both disabled and domain-rejected, distinct messages), domain taken from the parsed addr-spec (see above) and the stored email canonicalized to `addr.Address` (today's handler stores the raw string — `handler/auth.go:63`); unauthenticated `GET /auth/config` outside `RequireAuth`, behind the auth limiter; seed path (`seedAdmin()` in `api/cmd/server/main.go`) verified exempt; handler tests (allowed/rejected/case/multi-domain/empty-list/disabled, display-name/quoted-local-part forms, config endpoint shape).
- [ ] **M2 — Web: registration UX**: register page consumes `/auth/config` — disabled notice, domain hint + client-side pre-validation, server error rendering; login flow untouched; component tests.
- [ ] **M3 — Server: privilege checker core**: forge interface + GitLab driver methods (`TokenInfo`, `ProjectRole`, `DefaultBranchProtection`; `BotIdentity.IsAdmin` on the existing `VerifyToken`) with redaction discipline, driver tests against recorded fixtures, and **all existing hand-written fakes updated**; `privcheck` package computing `PrivilegeReport` per the rules above, unit-tested against fixture matrices (scope sets, roles incl. not-a-member 404, protection shapes, expiry windows); live check of gitlab.example.com's version (≥15.5) and the 404-on-unprotected-branch convention.
- [ ] **M4 — Server: enforcement + persistence + periodic sweep**: save-time token-check blocking in `CreateConnection` (422 + violations, nothing stored); migration `00030` (nullable report/checked_at/status columns, NULL = unchecked); on-demand `POST .../privilege-check` endpoint (owner-only authz, per-user forge rate limiter); background sweep with boot pass + interval + bounded per-repo concurrency, error-as-report handling, 0-rows-tolerant write-back; tests incl. a drift scenario (report flips ok→violations when the fake forge changes the bot's role) and the grandfathered-connection path (NULL → checked at boot sweep).
- [ ] **M5 — Web: privilege surfacing**: connection-card badge + expandable findings + "Check privileges" action; per-repo violation badges on the Repos page; 422 save errors rendered with doc link; responsive like the rest of Settings.
- [ ] **M6 — Docs + E2E**: `docs/configuration.md` (three new env vars), `docs/gitlab-bot-setup.md` (least-privilege section: exact scopes, Developer-only role, protected-branch requirement + how uzi now verifies each), `docs/auth-design.md` (registration policy), README `.env.example` updates; E2E-style handler test walking journey steps 3–4 against the fake forge; live compose smoke against gitlab.example.com with the existing bot (per the PRD #4 lesson: fakes lie, smoke before merge — uses the user's existing PAT, never a fresh one).

## Success Criteria

- With `UZI_ALLOWED_EMAIL_DOMAINS=example.com`: registering `x@gmail.com` fails server-side with the domain message; `x@example.com` succeeds; with `UZI_REGISTRATION_ENABLED=false` no registration path exists (API or UI); empty config reproduces today's behavior bit-for-bit.
- Saving an over-privileged PAT (extra scopes or instance-admin bot) is impossible — rejected before anything is stored, with the exact violations named.
- Privilege drift on the forge (role bump, branch unprotection, token swap) is visible in uzi within one sweep interval without any user action, as a badge + specific finding.
- A fully compliant setup (api-scope PAT, Developer role, protected main) shows `least-privilege ✓` everywhere — the PRD #4 guardrail assumption "GitLab-side bot = Developer + protected main" is now continuously *checked*, not documented-and-hoped.
- No new secret-handling surface: the checker uses the already-decrypted token only in-process at save/check time (same lifecycle as `VerifyToken` today), reports contain scopes/roles/branch names but never token material; redaction tests cover the new driver methods.

## Risks

- **GitLab API variance**: `GET /personal_access_tokens/self` requires GitLab 15.5+ (fact-checked 2026-07-04 against the GitLab API docs); older instances 404 it. Mitigation: treat introspection-unsupported as a *warning* in the report ("cannot verify scopes on this GitLab version"), never a hard save-block; verify against gitlab.example.com's actual version in M3 (it's the only allowlisted forge today).
- **`is_admin` visibility**: GitLab returns `is_admin` on `GET /user` **only when the caller is an admin** (`true`); a regular user's own response omits the field entirely (fact-checked 2026-07-04). Absent therefore means non-admin ⇒ **pass** — every compliant bot omits it. The warning tier is reserved for genuine can't-introspect failures (endpoint error), not for the healthy-absent case.
- **False-positive fatigue**: over-strict rules (e.g. flagging a token expiring in 13 days as red) train users to ignore badges. Mitigation: strict two-tier model — *violations* are only things that break the primary directive; everything advisory is a *warning*, visually distinct.
- **Effective-role subtlety**: inherited group membership can exceed direct project membership; using `/members/all/` (effective) not `/members/` (direct) is load-bearing. Fixture tests encode this; the live smoke double-checks one inherited case if available.
- **Config lockout**: operator sets a domain allowlist that excludes their own seed email — seed is exempt by design, so the admin always exists; documented in configuration.md.

## Out of scope (deferred)

DB-backed/admin-UI-editable settings (env-only for the compose MVP; the settings-table conversation belongs with SSO/KC later); admin fleet-wide privilege dashboard; automatic remediation (uzi demoting the bot or protecting branches itself — uzi observes, humans act); Anthropic-token introspection (no equivalent API); forgejo driver parity (single-driver reality today, interface stays neutral); registration approval queues / email verification (PRD #1 deferred list); PAT rotation reminders beyond the expiry warning.

## Decision Log

- 2026-07-04 (user): create PRD #5 = option A (registration domain allowlist + registration toggle + PAT least-privilege verification), chosen for zero file overlap with in-flight PRD #4 M3–M7; review by agents after drafting.
- 2026-07-04 (AI, fact-check wave — GitLab docs + Go net/mail verified empirically): `GET /personal_access_tokens/self` introduced in GitLab **15.5** (draft said 16.x — corrected); `is_admin` on `GET /user` is present only for admins, absent for regular users ⇒ absent = pass, not warning (draft's risk line contradicted the rule — reconciled); email-domain extraction pinned to the parsed addr-spec (`mail.ParseAddress(...).Address`, final-`@` split) because raw-input splitting mis-handles display-name/comment forms; stored email to be canonicalized to the addr-spec in M1. All other external claims (members/all effective membership, push_access_levels shape, access-level codes 30/40/50, `api` scope sufficiency) and cross-doc quotes confirmed; 404-on-unprotected-branch is undocumented convention — live check stays in M3.
- 2026-07-04 (AI, design-review wave — reviewer verified go-gitlab client coverage for all needed endpoints): fixed the blocker — parallel-safety table rewritten to name the real shared edit points (`Routes()`, `main.go` bgWG block, three shared docs incl. both PRDs writing a protected-branch section into `gitlab-bot-setup.md`, and the fake-compile-break ripple of any `Forge` interface growth) plus a merge-owner rule. Should-fixes applied: API paths corrected to `/api/...` (no `/v1` exists); `/auth/config` re-specced as uzi's first unauthenticated JSON surface besides `/health` (ForgeConfig is authed — not a precedent), outside `RequireAuth`, behind the auth limiter; seed exemption re-pointed at `seedAdmin()` in `api/cmd/server/main.go` (`internal/seed` only seeds the forge connection) with enforcement confined to `Register`; unchecked/NULL badge state added + sweep modeled on the worker sweeper's boot pass (grandfathered tokens surface at deploy, not one interval later); privilege-check behind the per-user forge limiter + sweep fan-out bounded at 4. Nits adopted: 403 (not 422) for both registration policy rejections; `BotUser` method dropped — `IsAdmin` rides on `VerifyToken`'s `BotIdentity` (no second `GET /user` round-trip); not-a-member 404 is an explicit finding; deleted-mid-sweep = tolerated 0-row write-back; multi-replica leader-election gap noted as shared with poller/sweeper, deferred to k8s work; `UZI_` prefix kept deliberately for operator-policy knobs (consistent with `UZI_SECRET_KEY`/`UZI_SEED_*`, unlike mechanical tuning knobs); IDN matching is byte-wise, noted in-text.
- 2026-07-04 (AI, defaults chosen — revisit on review): env-var config over DB settings table (matches every existing knob; compose MVP has one operator); token-level violations **block** the save, per-repo findings **warn** (user present + plaintext in hand at save vs drift after the fact); scopes rule = exactly `{api}` per the existing bot-setup doc; role rule = exactly Developer(30) via effective membership (`/members/all/`); protected-default-branch check included because Developer-role enforcement is vacuous without it; jsonb report on `forge_connections` over a normalized findings table (current-state display, no audit history); `privcheck` as a new package (keeps `forgesvc` sync-only); migration range `00030+` reserved; introspection-unsupported ⇒ warning not block (older GitLab tolerance); seed path exempt from the domain allowlist (bootstrap deadlock otherwise).
