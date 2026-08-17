# PRD #53: Per-user Claude rate-limit visibility — 5h/7d window meters

**GitLab Issue**: [#53](https://github.com/vtmocanu/uzi/-/issues/53)
**Status**: Done (merged 2026-07-15, MR !51)
**Priority**: Medium
**Depends on**: nothing in-repo. Reuses the vault (PRD #32), the self-improve engine pattern, and the usage endpoint split (PRD #40).
**Mockup**: [prds/mockups/53-rate-limits-mock.html](../mockups/53-rate-limits-mock.html)

## Problem

Anthropic enforces two account-wide rate-limit windows per Anthropic account —
**5-hour** and **7-day** — and uzi shows nothing about either. Consequences:

- A run stalls mid-flight because its owner's window is exhausted, and neither
  the owner nor an admin saw it coming. PRD #35 (retry after limit) handles the
  *recovery*; this PRD handles the *foresight*.
- An admin planning factory work has no view of which users have headroom.
- The data is cheap to get: every Messages API response already carries the
  numbers as `anthropic-ratelimit-unified-*` headers, and there is a free
  usage endpoint for credentials it accepts.

Prior art (proven in vtmocanu/cc-statusline v2.11–v2.12, which fetches these
same numbers per account):

- `GET https://api.anthropic.com/api/oauth/usage` with `Authorization: Bearer
  <token>` + `anthropic-beta: oauth-2025-04-20` returns 5h/7d utilization and
  reset timestamps for claude.ai OAuth logins — but persistently answers **429
  to `claude setup-token` credentials** (exactly what uzi users typically
  store).
- Fallback that works for every credential the Messages API accepts: a minimal
  Messages request (Haiku, `max_tokens: 1`) and read the response headers:
  `anthropic-ratelimit-unified-5h-utilization` / `-7d-utilization` (0–1
  fraction; calibrated `0.55` == 55%) and `-5h-reset` / `-7d-reset` (epoch
  seconds). Cost ≈ 1 token of the user's own quota per probe.

## Solution Overview

A background poller (modeled on the self-improve engine) ticks every
`UZI_USAGE_POLL_INTERVAL` (default 5m). Each tick it lists users holding an
`anthropic_token` secret, skips those whose dek-sealed token cannot be opened
while the vault is locked (master-sealed tokens are polled regardless, D3),
opens the token,
asks Anthropic (usage endpoint first, header-probe fallback), and upserts one
row per user. Two read endpoints mirror the PRD #40 usage split: users get
their own numbers, admins get everyone's. The SPA renders meters in three
places (mockup): a **Settings card**, a **sidebar-footer micro-meter**, and an
**Admin → Rate limits** table.

## Design Decisions

- **D1 — Poll server-side with the user's own token, not client-side.** The
  token never leaves the api container (PRD #50 direction); the SPA only ever
  sees percentages. Workers/agents are not involved.
- **D2 — Usage endpoint first, header probe as fallback, per user per tick.**
  The free endpoint costs nothing where it works; the probe costs ~1 token and
  only runs when the endpoint refuses the credential (observed: 429 for
  setup-tokens). `UZI_USAGE_PROBE=false` disables the probe entirely for
  instances that refuse to spend tokens (those users then show `unavailable`
  unless the free endpoint works for them).
- **D3 — Vault-locked users are skipped, their last reading kept and marked
  stale.** Same behavior as the self-improve engine: a **dek-sealed** token
  cannot be opened while the vault is locked (the DEK isn't in memory).
  Legacy `sealed_with='master'` tokens ARE openable regardless of lock state
  (`vault.Open` dispatches on `sealed_with`) and are polled normally. No inbox
  notification — unlike self-improve there is nothing the user must act on;
  the meters simply age. Staleness is `synced_at` older than 3× the poll
  interval; the UI greys the bars and badges the row (mockup, `mihai` row).
  With the poller disabled (`UZI_USAGE_POLL_INTERVAL=0`) existing rows are
  still served, always marked `stale: true`.
- **D3b — Token deletion deletes the row.** The secret-delete path also runs
  `DeleteRateLimits(user)`, so a token-less user never shows a ghost reading;
  the admin table then shows them as `no_token` (mockup, `irina` row). Pinned
  by an M2 test. Saving/replacing a token pokes the poller for that user, and
  the engine's `Boot` pass polls immediately at startup, so the
  token-saved→meters-visible gap is seconds, not a full interval; until the
  first reading lands the API returns `unavailable`.
- **D4 — One row per user, no history.** This is a live gauge, not analytics.
  A `usage-history` table (charts, trends) is out of scope; the row is
  overwritten each tick.
- **D5 — Failure semantics copied from cc-statusline: fail closed, back off.**
  A malformed response never overwrites the last good row. An HTTP error with
  no usable fallback sets a per-user backoff (fixed 15m, in-process, no knob)
  so a refusing credential is not hammered every 5 minutes. Transport errors
  just wait for the next tick.
- **D6 — Meter thresholds reuse the worker-gauge language.** ok < 80% ≤ warn
  < 95% ≤ danger, same as `WorkerStats.toneFor` — one visual vocabulary for
  "resource nearly exhausted" across the app.
- **D7 — Reset times shown as countdowns, stored as epochs.** The api stores
  what Anthropic said (`resets_at` epoch); the SPA renders "resets in 1h 23m"
  and re-derives it client-side between polls.

## Technical Design

### API (api/)

- **Migration (draft `00080`, renumber at merge per CLAUDE.md; live head is
  `00064`, `00070` held by PRD #41, `00095` by PRD #35)**:
  `anthropic_rate_limits` — `user_id UUID PK REFERENCES users ON DELETE
  CASCADE`, `five_hour_pct SMALLINT CHECK (five_hour_pct BETWEEN 0 AND 100)`,
  `five_hour_resets_at TIMESTAMPTZ`, `seven_day_pct SMALLINT CHECK
  (seven_day_pct BETWEEN 0 AND 100)`, `seven_day_resets_at TIMESTAMPTZ`,
  `source TEXT CHECK (source IN ('usage_endpoint','header_probe'))`,
  `synced_at TIMESTAMPTZ NOT NULL`. sqlc queries: `UpsertRateLimits`,
  `GetRateLimits` (one user), `ListRateLimits` (join users for email/name;
  admin), `DeleteRateLimits` (D3b).
- **`api/internal/usagepoller/`** — engine cloned from
  `selfimprove.Engine` (`Boot` + `Run` + ticker; vault gate on the OPEN OUTCOME —
skip only on `vault.ErrLocked`, never a blanket `Unlocked()` pre-check, which
would wrongly skip master-sealed users (reconciled with D3 at review);
  `settings.Cache` not needed — env-only knobs). Per user: open token via the
  same vault path as `workersvc.openAnthropic` (factor that helper out of
  workersvc rather than duplicating it), call the client, upsert. Bounded
  concurrency (reuse the forge poller's semaphore pattern), per-user backoff
  kept in-process (a map; a restart just retries once).
- **`api/internal/anthropic/`** — net-new small outbound client (the repo has
  none): `Usage(ctx, token)` hitting `/api/oauth/usage`, and
  `ProbeHeaders(ctx, token)` doing the minimal Messages call and parsing the
  unified headers. The probe body is PINNED: `{"model": "<cheapest haiku>",
  "max_tokens": 1, "messages": [{"role": "user", "content": "hi"}]}` — a fixed
  innocuous string, never user or run content. Headers: `Authorization:
  Bearer`, `anthropic-beta: oauth-2025-04-20`, `anthropic-version:
  2023-06-01`, `User-Agent: uzi/<version>`. Timeout via a new
  `UZI_ANTHROPIC_HTTP_TIMEOUT` (default 15s; the newer `UZI_`-prefixed knob
  convention, cf. `UZI_OIDC_HTTP_TIMEOUT`). Utilization fractions floor+clamp
  to 0–100 ints; ISO resets from the usage endpoint parse to epochs; any
  missing UTILIZATION fails the whole reading (fail closed, D5) — a
  missing/unparseable reset stores `null`, as the frozen DTO's
  `resets_at: <epoch|null>` requires (reconciled at review). Errors are
  constructed from status code + a sanitized body excerpt — never from the
  request — so no error path can carry the token (pinned by an M1 test).
  PRD #50's egress proxy, if it lands later, wraps this same client.
- **Handlers** (`api/internal/handler/ratelimits.go`) — the DTO contract
  below is FROZEN so M3's web work can build against mocks in phase 1.
  `window` = `{"pct": <int 0-100>, "resets_at": <epoch seconds|null>}`;
  `synced_at` ISO-8601; the union discriminates on `status`:

  ```
  GET /api/me/rate-limits →
    {"status": "ok", "five_hour": <window>, "seven_day": <window>,
     "source": "usage_endpoint"|"header_probe",
     "synced_at": "2026-07-15T09:20:11Z", "stale": false}
  | {"status": "no_token"}
  | {"status": "unavailable"}        // token saved but no reading yet,
                                     // probe disabled, or refused credential

  GET /api/admin/rate-limits →      (under the existing RequireAdmin group)
    {"users": [{"id": "<uuid>", "email": "...", "name": "...",
                "vault_locked": bool, "limits": <same union as /me>}]}
  ```

  Every user appears in the admin list, including `no_token` ones. DTOs
  hand-written like `usage.go`; `stale` computed server-side (D3).
- **Config** (`api/internal/config/config.go`): `UZI_USAGE_POLL_INTERVAL`
  (default `5m`, `0` disables the engine, nonzero values clamped to ≥ `1m`
  with a boot warning — the probe spends users' own tokens, D2),
  `UZI_USAGE_PROBE` (default `true`), `UZI_ANTHROPIC_HTTP_TIMEOUT` (default
  `15s`). Wire in `api/cmd/server/main.go` under the same `bgWG` as the
  other engines.

### Web (web/)

- **`api.ts`**: `getMyRateLimits()` / `getAdminRateLimits()` + types; mock
  fixtures in `src/mocks/` covering live/warn/danger/stale/no-token/
  unavailable.
- **Shared meter**: lift `WorkerStats`' private `Bar` into
  `src/components/ui.tsx` (or a new `Meter.tsx`) with the toneFor thresholds
  (D6); reuse in all three surfaces.
- **Settings → Account & token**: new "Claude limits" card under the token
  card (mockup frame A). Hidden when no token is set; on `unavailable` the
  card shows the two windows greyed with "no reading yet" and the Live badge
  replaced by a neutral one.
- **Sidebar footer**: two 5px micro-bars under the signed-in user block
  (mockup frame B), hover title = reset countdowns. Hidden for `no_token`
  and `unavailable` (no dead chrome). Polls with `usePollWhileVisible(60s)` —
  the data changes at most every poll interval, so the pages' usual 10s
  cadence would be pure amplification.
- **Admin → Rate limits**: new page + route (`/admin/rate-limits`, behind
  `AdminRoute`), nav item in the Admin group; table per mockup frame C
  (user, 5h bar, 7d bar, status badge, updated). Sort: danger first, then
  warn, then by 5h% desc.

### Docs + specs

- `docs/rate-limits.md` (frontmatter `title` + `order` + `audience: user` —
  `web/scripts/check-docs.mjs` fails a user page without `order`): what the
  meters mean, the two windows, the ~1-token probe cost and
  `UZI_USAGE_PROBE`, vault-locked staleness.
- `docs/configuration.md`: the three new env knobs.
- `ARCHITECTURE.md`: short section linking here as the decision record.

## Milestones

- [x] **M1 — API: client + poller + storage**: migration (draft `00080`),
  `anthropic` client (usage + probe, pinned probe body, fraction→pct,
  fail-closed parsing, sanitized errors), `usagepoller` engine (vault gate
  incl. master-sealed exception, Boot immediate pass, poke-on-token-save,
  fixed 15m backoff, bounded concurrency), config knobs (incl. the ≥1m
  clamp), `api/cmd/server/main.go` wiring; factor `openAnthropic` out of
  workersvc; **the frozen DTO contract above is part of this milestone's
  deliverable** (M3 builds against it). Go tests: header/JSON parsing table
  tests (fractions, clamps, missing fields fail closed), poller skips locked
  dek-sealed users but polls master-sealed ones, backoff after refused
  credential, upsert overwrite, **error paths carry no token material** (a
  failing request's error string must not contain the token). Validation:
  with a real token in a dev stack, the row appears within one Boot pass and
  matches the account's real numbers.
- [x] **M2 — API: read endpoints**: `ratelimits.go` handlers + DTOs + routes
  (self + admin), `stale`/`no_token`/`unavailable` states, row deletion on
  token delete (D3b). Go tests: role gating (403 non-admin), shape per the
  frozen contract, stale computation, deleted-token returns `no_token` with
  no ghost row. Validation: `curl` both endpoints as member and admin.
- [x] **M3 — Web: meters everywhere**: shared `Meter`, Settings card, sidebar
  micro-meter, Admin page + route + nav, api client + mocks; vitest for
  toneFor thresholds and the five row states (live/warn+danger/stale/no
  token/unavailable); typecheck green. Validation: mock mode reproduces all
  mockup frames; live mode shows real numbers.
- [x] **M4 — Docs + polish**: `docs/rate-limits.md`, configuration doc, the
  ARCHITECTURE.md pointer; `check-docs.mjs` green. Validation: page renders
  in-app under /docs.
- [x] **M5 — E2E + landing**: e2e happy path (seeded token fixture → meters
  render; admin sees the table, member gets 403 on the admin endpoint);
  migration renumbered to the live head (`00080` → `00065`, next free above
  `00064`). The move of this file to `prds/done/` happens post-merge per repo
  convention (cf. PRD #49).

## Milestone dependency / parallelization

| Phase | Milestones | Depends on | Files touched |
|---|---|---|---|
| 1 (parallel) | M1 (api/), M3 UI against mocked api (web/) | — (M3 builds against the frozen DTO shapes above) | `migrations/`, `internal/anthropic/`, `internal/usagepoller/`, `config.go`, `main.go` · `web/src/**` |
| 2 | M2 (api/handler) | M1 | `handler/ratelimits.go`, `handler.go` |
| 3 | M4, M5 | M1–M3 | `docs/`, e2e |

## Out of Scope

- **History / trend charts** (D4) — separate PRD if wanted.
- **Acting on the numbers** (pausing queued runs when the owner is near a
  limit, picking a different user's worker) — PRD #35 territory and beyond.
- **Org/workspace-level Anthropic quotas** — only the two unified per-account
  windows are shown.
- **Console API-key accounts without unified limits** — if the probe response
  carries no unified headers, the user shows `unavailable`; no attempt to
  parse the legacy per-model `x-ratelimit-*` headers.

## Accepted residuals

- **The probe spends the user's own tokens** — ~1 token per interval worst
  case (≈ 300/day at the 5m default if the free endpoint never works for
  them; the ≥1m clamp bounds the worst configuration at ≈ 1 440/day). Named
  in the user doc; `UZI_USAGE_PROBE=false` opts an instance out (D2).
- **Two uzi users sharing one Anthropic account show duplicate meters and
  double the probe spend.** uzi cannot detect account identity from a token
  and does not try; the admin capacity view will double-count that one pool
  of headroom.
- **Vault-locked (dek-sealed) users go stale silently** (D3) — by design; the
  lock screen already tells the user their vault is locked.
- **A reading can be up to one interval old** — acceptable for a gauge whose
  underlying windows are 5h/7d.
- **429-from-the-usage-endpoint is assumed persistent per credential type**,
  based on cc-statusline observations; the backoff (D5) covers the case where
  it is actually transient.

## Success Criteria

- A member with a saved token sees their two meters on Settings and in the
  sidebar within seconds of saving (poke-on-save, D3b), within a few
  percentage points of what their own Claude session reports (readings
  minutes apart legitimately differ).
- An admin sees every user's meters and staleness states on one page; a
  non-admin gets 403 from the admin endpoint.
- A locked vault degrades to a greyed stale reading — never a wrong or
  missing-row error.
- Instance operators can turn the probe (`UZI_USAGE_PROBE=false`) or the whole
  poller (`UZI_USAGE_POLL_INTERVAL=0`) off.
- No Anthropic token ever appears in a log line, an API response, or the SPA.
