# PRD #55: OIDC group → role/access mapping (Keycloak / Pocket ID)

**GitLab Issue**: [#55](https://github.com/vtmocanu/uzi/-/issues/55)
**Status**: Implemented (M1–M5) — awaiting PR review. Real-IdP e2e (live Keycloak / Pocket ID) is a manual verify item (see M5).
**Priority**: Medium
**Created**: 2026-07-16
**Depends on**: PRD #45 (OIDC SSO login) — done

## Problem

uzi's OIDC SSO (PRD #45) auto-provisions users just-in-time but cannot use IdP
group membership to decide roles or access. Admin is only ever the first user to
log in or the `UZI_SEED_EMAIL` env seed, and there is no runtime promotion — on a
shared/team deployment the IdP should own who is an admin (and optionally who may
log in at all), instead of a first-login race or an env seed. PRD #45 explicitly
deferred this ("No groups-claim mapping in this iteration"); issue #55 is the
explainer.

## Solution Overview

1. **Parse a `groups` claim** (array of strings) from the verified ID token into
   `oidc.Identity` (`api/internal/oidc/provider.go`). Claim name configurable
   (`UZI_OIDC_GROUPS_CLAIM`, default `groups`) — providers differ.
2. **Configurable group names** (user requirement):
   - `UZI_OIDC_ADMIN_GROUPS` (comma-separated; empty = feature off) — membership
     in any ⇒ `is_admin = true`.
   - `UZI_OIDC_ALLOWED_GROUPS` (comma-separated; empty = no gate) — membership in
     any is required to SSO-login or JIT-provision at all.
3. **Authoritative sync on every OIDC login** (user decision, 2026-07-16): groups
   both grant AND demote. Leaving the admin group demotes on next SSO login;
   leaving the allowed group blocks the next SSO login. No sticky roles. An
   entirely absent/unparseable claim is treated as IdP misconfig, not removal
   (fail-safe — decision 1; user decision, 2026-07-16).
4. **Bootstrap interplay** (user decision, 2026-07-16): when `UZI_OIDC_ADMIN_GROUPS`
   is set, first-OIDC-user-becomes-admin is disabled (the group decides).
   `UZI_SEED_EMAIL` stays as break-glass and is exempt from group demotion. With
   groups configured, seeding becomes optional: it is no longer needed to
   designate the admin, only for break-glass password login and credential
   seeding (`UZI_SEED_FORGE_PAT`, `UZI_SEED_ANTHROPIC_TOKEN`).
5. **OIDC-only scope** (user decision, 2026-07-16): groups apply only at OIDC
   login. Password-login users (incl. the seed admin) keep their stored
   `is_admin`; password-registration first-user-admin is untouched.
6. **Works with both Keycloak and Pocket ID** (user requirement): `docs/oidc.md`
   grows per-provider walkthroughs for emitting the groups claim.

## Design Decisions

1. **Groups come from the verified ID token only, with fail-safe semantics for
   an absent claim** (user decision, 2026-07-16, review must-confirm). No
   userinfo-endpoint fallback: both Keycloak (group-membership mapper with "Add
   to ID token" on) and Pocket ID (`groups` scope) can put groups in the ID
   token, and the ID token is already signature/nonce-verified in the PRD #45
   flow. Two distinct cases:
   - **Claim present as a JSON array of strings, user not in the group** → real
     removal: demote / gate applies (authoritative sync).
   - **Claim entirely absent or unparseable** (mapper toggled off, claim
     renamed, wrong shape — string, mixed array) → likely IdP misconfig, NOT
     removal: existing users keep their stored role and pass the allowlist
     gate; the login proceeds and the server logs loudly (warn, per login).
     JIT-created users in this state get `is_admin = false` and, when
     `UZI_OIDC_ALLOWED_GROUPS` is set, are still refused (a brand-new user has
     no established role to fail safe into). This prevents one IdP misconfig
     from demoting every admin and locking every existing user out at once.

2. **Group matching is exact, case-sensitive string comparison** after trimming
   config values. No glob/regex, no path normalization: Keycloak's "Full group
   path" mapper option (leading `/uzi-admins`) must be disabled, or the operator
   configures `UZI_OIDC_ADMIN_GROUPS=/uzi-admins` verbatim — documented in the
   walkthrough. Keeps the matcher trivially auditable.

3. **Scopes are not auto-appended, and no boot warning on a missing `groups`
   scope.** Requesting an undefined scope is an `invalid_scope` error on strict
   IdPs (Keycloak), and Keycloak group emission is a client-scope/mapper
   concern, not a scope-request concern — so a "groups configured but no
   `groups` scope" boot warning would false-positive on every Keycloak
   deployment and train operators to ignore it (review finding). Instead the
   docs say: if your provider emits groups via a requested scope (Pocket ID),
   add `groups` to `UZI_OIDC_SCOPES`; Keycloak needs a mapper, no scope. The
   runtime absent-claim warn log (decision 1) is the misconfig signal.

4. **Enforcement order in the callback resolve path**
   (`api/internal/handler/oidc.go`), after ID-token verification and before any
   DB write:
   1. `UZI_OIDC_ALLOWED_GROUPS` set, claim present, and no intersection →
      reject with the existing generic `oidc_forbidden` code (detail in server
      log only — matches the PRD #45 no-enumeration posture). JIT never runs,
      links never happen, deactivated rows are never touched. Claim
      absent/unparseable → fail-safe per decision 1 (existing users pass, JIT
      refused).
   2. Resolve user (subject match / email link / JIT) exactly as today.
   3. **Sync `is_admin`** when `UZI_OIDC_ADMIN_GROUPS` is set and the claim is
      present: desired = membership; if stored differs, `SetUserAdmin` (new
      sqlc query), and the in-handler `user` value is refreshed from the
      returned row so the issued session payload reflects the flip (review
      finding). Exemption: the seed admin (email == `UZI_SEED_EMAIL`) is never
      demoted (break-glass; promotion-by-group still allowed) — guard the
      comparison against an empty `SeedEmail` explicitly, so a future refactor
      can't turn disabled seeding into a blanket exemption (review finding).
      Grant and demote are logged (user id + direction, no group PII beyond
      configured names).
   JIT creation passes `IsAdmin: membership` directly when admin groups are
   configured; `count == 0` first-admin applies only when they are not. The
   advisory-locked transaction in `createOIDCUserFirstAdmin` is kept in both
   branches (harmless, and preserves the concurrent-JIT race handling); its
   signature grows the desired-admin input instead of hardcoding
   `IsAdmin: count == 0` (`api/internal/handler/oidc.go:311`).
   Note the exemption covers **demotion only**: a seed admin outside
   `UZI_OIDC_ALLOWED_GROUPS` cannot SSO-login (the gate rejects before
   resolve); their break-glass is password login, which bypasses both gate and
   sync by design (decision — OIDC-only scope).

5. **Role/gate changes land at OIDC-login time; once written they propagate
   instantly.** The `is_admin` flip happens *at* the user's next OIDC login;
   because `RequireAuth` loads the user row per request
   (`api/internal/middleware/auth.go:68`) and `is_admin` is not in the JWT, the
   write is then visible to every live session on its next request — no
   token_version bump. Staleness windows, documented in `docs/oidc.md`:
   - A demoted-in-IdP user who never re-logs-in keeps admin until their next
     OIDC login. For OIDC-only users that is bounded by session TTL
     (`AUTH_TOKEN_TTL`, default 168h); a *linked* user with a password can keep
     re-authing via password (no sync — OIDC-only scope) and retain the stale
     role longer (review finding).
   - Likewise the allowlist is a login gate only (user decision, 2026-07-16):
     removal from `UZI_OIDC_ALLOWED_GROUPS` blocks the next SSO login but does
     not revoke live sessions. Offboarding within the TTL window is the
     existing admin deactivate-user action (which does kill sessions).

6. **No admin-UI role management in this PRD.** The IdP owns roles; adding a
   manual promote/demote endpoint would fight the sync (and is exactly what the
   issue says doesn't exist today). Revisit only if a non-OIDC deployment asks
   for it.

7. **Feature is fully dormant when unset.** Neither env var set → no groups
   parsing consequences, no behavior change, all existing tests pass unchanged.
   Config validation: group vars set while OIDC itself is unconfigured → refuse
   to start (same all-or-nothing posture as PRD #45 Decision 8).

   **Refinement (M5 live validation, 2026-07-16):** the refuse-to-start guard
   keys on the GATING vars only (`UZI_OIDC_ADMIN_GROUPS` / `UZI_OIDC_ALLOWED_GROUPS`).
   `UZI_OIDC_GROUPS_CLAIM` is an inert format knob that ships as a compose/`.env`
   default (`groups`), so arming the guard on it made the zero-config
   password-login stack (OIDC off) refuse to boot — a regression against
   "fully dormant when unset". General principle recorded in `specs/ai.md` §254:
   any env var shipped as a non-empty compose/`.env` default must not arm a
   refuse-to-start guard.

## Technical Design

### API (api/)

- `api/internal/config/config.go`: `OIDCGroupsClaim` (default `groups`),
  `OIDCAdminGroups []string`, `OIDCAllowedGroups []string` + validation
  (decision 7) and the scope-hint warning (decision 3).
- `api/internal/oidc/provider.go`: `oidc.Config` gains `GroupsClaim` (threaded
  from app config into the provider); `Identity` gains `Groups []string` plus a
  `GroupsClaimPresent bool` (fail-safe needs absent vs empty). The claim name
  is dynamic at runtime, so the static `rawClaims` struct cannot carry it — a
  second `idToken.Claims(&map[string]json.RawMessage{})` decode looks up
  `cfg.GroupsClaim` and tolerant-parses per decision 1 (review finding).
- `api/internal/handler/oidc.go`: allowlist gate + admin sync per decision 4;
  `createOIDCUserFirstAdmin` gains the groups-configured branch.
- `api/internal/store/queries/users.sql`: `SetUserAdmin :one` (id, is_admin);
  sqlc regen. No migration needed (no schema change).
- `.env.example`, `docker-compose.yml` api env block, `deploy/` chart values.

### Web (web/)

- No required changes. Optional: admin-settings OIDC status line mentions group
  mapping active (follows the PRD #45 degraded-state line pattern).

### Docs + specs

- `docs/oidc.md`: new "Group-based roles and access" section — env var
  reference, authoritative-sync semantics (demote-on-login, absent-claim
  fail-safe, seed exemption incl. "seed can't SSO past the gate, password is
  the break-glass", staleness windows per decision 5), **Keycloak walkthrough**
  (create groups, client scope + group-membership mapper, Full group path OFF,
  add to ID token) and **Pocket ID walkthrough** (user groups, `groups`
  scope/claim in `UZI_OIDC_SCOPES`), updated fresh-instance guidance: with
  `UZI_OIDC_ADMIN_GROUPS` set, pre-seeding an admin is optional (break-glass
  only — still recommended) and the first-SSO-click-becomes-admin warning
  (PRD #45 audit M2) no longer applies.
- `docs/configuration.md`: new `UZI_OIDC_*` rows.
- `specs/ai.md`: decision entries. `specs/human.md`: group-mapping requirement
  (needs user approval per specs contract).

## Milestones

- [x] **M1 — Config + claim parsing** (`d51c61d`): the three env vars, boot
      validation + scope-hint doc-log, `Identity.Groups`/`GroupsClaimPresent`
      with tolerant parse, env plumbing (`.env.example`, compose, chart). Config
      + provider unit tests. Reviewed + audited clean.
- [x] **M2 — Enforcement** (`d4d6292`): allowlist gate (before any DB write),
      admin sync (grant/demote/log), seed-admin demotion-only exemption, JIT
      first-admin gating, `SetUserAdmin` + sqlc regen. Reviewed + audited clean;
      fail-closed on `SetUserAdmin` DB error (endorsed in review).
- [x] **M3 — Tests green** (`6102a5e`): live-DB callback matrix — allowed-group
      member/non-member (existing user AND JIT), admin grant, admin demote,
      seed-admin not demoted, empty-SeedEmail exemption guard, fail-safe cases
      (claim absent/malformed: existing admin keeps role AND passes the gate,
      JIT still refused when gate set, warn logged; claim present-but-empty
      array: demotes/gates), dormant-when-unset regression, first-user NOT
      admin when admin groups configured. `go test ./...` green; matrix ran live
      by coder, reviewer, auditor, tester against a throwaway Postgres.
- [x] **M4 — Docs + specs** (`36b12a6`, `a43f615`, `9865634`): `docs/oidc.md`
      group section + Keycloak & Pocket ID walkthroughs, `configuration.md`,
      `specs/ai.md` §253–257, `specs/human.md` Feature #55; `npm run build`
      (check-docs) green. Fact-checked 0-refuted; reviewer + auditor doc passes
      clean.
- [x] **M5 — Live validation** (`405571f` + doc/spec corrections `76fd9c4`,
      `37ff691`, `03d47ff`): matrix reproduced green + stack-level boot-config
      validation (dormant-when-unset, refuse-to-start on the gating vars). Found
      + fixed a BLOCKING bug — the compose/`.env` `UZI_OIDC_GROUPS_CLAIM=groups`
      default tripped refuse-to-start, breaking the zero-config no-OIDC stack
      (Decision 7 refinement above). Re-validated: default stack boots dormant.
      **Remaining manual verify (PR checklist):** end-to-end against a real
      Keycloak + Pocket ID (grant / demote-on-relogin / allowlist block /
      JIT-gate / fail-safe on mapper-off / seed break-glass); Pocket ID
      groups-scope emission is unverified against a live instance. No live IdP
      was reachable in the dev env; runbook in the PR description.

## Milestone dependency / parallelization

| Phase | Milestones | Depends on | Files touched |
|---|---|---|---|
| 1 | M1 | — | config.go, oidc pkg, .env.example, compose, chart |
| 2 | M2 | M1 | handler/oidc.go, users.sql + sqlc |
| 3 (parallel) | M3, M4 | M2 | tests · docs/specs |
| 4 | M5 | M2–M4 | live stack |

## Out of Scope

- Password-login enforcement of groups (user decision: OIDC-only).
- Admin-UI manual role promote/demote (decision 6).
- Keycloak realm/client **roles** claim mapping (groups only; roles can be
  mapped into a groups-shaped claim by the operator if wanted).
- Mid-session forced logout on demotion (per-request `is_admin` already applies;
  role display staleness bounded by session TTL).
- Multiple providers, per-group fine-grained permissions beyond the binary
  `is_admin`.

## Success Criteria

- With Keycloak or Pocket ID emitting groups per the docs: a member of a
  configured admin group is admin after SSO login; removing them from the group
  demotes them on next login; a user outside `UZI_OIDC_ALLOWED_GROUPS` cannot
  log in or JIT-provision via SSO.
- With `UZI_OIDC_ADMIN_GROUPS` set on a fresh instance, no seed is required to
  get a correctly-scoped admin, and the first SSO user does NOT become admin by
  order of arrival.
- Seed admin (`UZI_SEED_EMAIL`) can always password-login with admin intact,
  regardless of IdP group state (break-glass preserved).
- Dropping the groups claim at the IdP (mapper off / renamed) does NOT demote
  existing admins or lock existing users out — logins proceed with loud warn
  logs until the claim is fixed (fail-safe verified in M3/M5).
- Everything dormant and all existing tests unaffected when the new vars are
  unset.
