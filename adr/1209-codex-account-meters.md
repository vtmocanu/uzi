# ADR-1209: Codex account rate-limit visibility

**Status**: Accepted (implementation); maintainer runtime acceptance PENDING
**Date**: 2026-09-20
**Deciders**: Vlad (maintainer) + agent team
**Issue**: [vtmocanu/uzi#1209](https://github.com/vtmocanu/uzi/issues/1209)
**Numbering**: `1209` is the **issue** number, not an ADR sequence number (the
convention ADR-0214/0238 record). A reader who assumes "ADR number == ADR
count" will miscount.
**Extends**: [PRD #1209](../prds/1209-codex-account-rate-limits.md) (Codex
account rate-limit visibility — read it for the full milestone breakdown,
the plan-gate addendum, and the Decision and progress log; this ADR is the
durable subset a future change must not silently break). Reuses the
credential/refresh coordination core landed by [PRD #1171](../prds/done/1171-codex-production-adapter.md)
(closed) without weakening it.

## Context

uzi already shows a Claude token's 5-hour/7-day rate-limit windows in
Settings, the sidebar, admin, the CLI and the TUI (PRD #53). A user with a
linked Codex **subscription** login had no equivalent: uzi could store the
login, but nothing polled its usage, and no surface displayed it. Codex's own
shape differs from Claude's — a set of independently-durationed **buckets**,
each with up to two windows, rather than Claude's fixed pair — so the feature
needed its own DTO, its own poller, and its own display preference, while
reusing the credential-coordination core PRD #1171 had just landed.

The central risk was **not** the display work; it was connecting an idle,
run-less poller to the same access-token lifecycle a running worker
authorizes against, without opening a second, weaker path to that token. The
decisions below are the ones that keep that boundary intact.

## The decisions

### API-side idle polling, in the app server, with no worker/run/model involvement

A dedicated background engine, `api/internal/codexusagepoller`, runs inside
`api` itself — the same `Boot()`-immediate-tick-plus-interval-ticker shape as
the Claude `usagepoller` and the scheduler. It reads a linked account's usage
whether or not that user has ever started a Codex run, ever will, or has any
worker online. This is a deliberate departure from how uzi obtains most
provider state (through a worker's claim): reading `/wham/usage` needs no
model turn, no app-server subprocess, and no synthetic claim, so making it a
worker responsibility would only add moving parts. The poller makes zero
model calls, spends no token, and has no run/worker dependency of any kind.

### The raw access token never leaves package `workersvc`

`CollectCodexAccountUsage` (`api/internal/workersvc/codexusage.go`) is the
**one** operation the poller calls. It opens the account's committed login,
calls `codexauth.Client.ReadUsage`, and returns either a
`CodexUsageReading` (a normalized bucket set plus the `(generation,
credential_revision)` it was captured under — no token, no login blob, no raw
provider id) or a typed `*CodexUsageFailure` drawn from a closed enum
(`reauth_required`, `vault_locked`, `rate_limited`, `provider_mismatch`,
`transient`, `disabled`, `not_linked`). Every step that touches the token —
opening the committed login, the identity-mismatch check against the
account's frozen tuple, the bounded 401 retry/rotation — happens inside this
one function; nothing it returns can carry the secret forward. The poller
engine, the store writes, and every DTO downstream see only the sanitized
result.

### Reusing #1171's coordination core WITHOUT weakening worker authorization

The poller needs the same durable, revision-fenced refresh #1171 built for
the worker lane (`coordinatedRefresh`, the unexported core), but it has no
run and no worker claim to authorize against. Two things make that safe:

- **The worker-facing wrappers are untouched.** `AuthorizeCodexCredentialOp`,
  `CoordinatedCodexRefresh` and `ReleaseCodexAccessToken` keep their existing
  run/current-worker/capability checks, pre/post-operation checks, and
  refusal of stale/revoked authority. `CollectCodexAccountUsage` never calls
  them — it calls the unexported `coordinatedRefresh` core directly, the same
  core the worker wrapper itself calls after its own checks pass.
- **The poller's authority is a different, owner-scoped principal, not a
  weaker version of the worker's.** `codexPollPrincipal`
  (`api/internal/workersvc/codexusage.go`) has no exported fields and is
  minted only by `newCodexPollPrincipal`, from a freshly re-read,
  owner-scoped `codex_provider_account` row (`GetCodexProviderAccountByID`
  filtered on `(user_id, id)`). Neither HTTP input nor a caller-supplied
  account id can mint one directly. It carries the account/user ids and the
  observed `(generation, credential_revision)` fences, nothing else — no
  bearer capability, no run id, no display selection. There is no new
  public token-release or refresh route, and the run-id parameter on the
  worker route is never made optional.

The two paths converge on the **same** durable operation: a poller-driven
rotation and a worker-driven refresh for the same account race through the
same `coordinatedRefresh` core under the same operation-id retry,
durable-commit-before-release, and recovery-fencing rules, so exactly one
provider exchange and one committed generation result regardless of which
side wins.

### The account-level `reauth_required` discriminator, fenced to generation and credential revision

Before this feature, an account whose access token was proven expired with
no renewal material (`coordinatedRefresh` returning `ErrCodexRefreshNoToken`)
was left usable-looking with a dead login — nothing marked it, and the
existing reconciler only repaired a **quarantined** account, not this case.
A fresh same-account re-login could then silently link back to the same dead
canonical blob.

Migration `00238` adds three columns to `codex_provider_account`:
`reauth_required BOOLEAN NOT NULL DEFAULT false`, `reauth_generation
BIGINT`, and `reauth_credential_revision BIGINT`, under a `NOT VALID` CHECK
that the two counters are populated whenever the flag is set. The poll path
sets it (`MarkCodexReauthRequired`) fenced on the `(generation,
credential_revision)` it observed the no-renewal expiry under, so a
concurrently-refreshed or replaced account cannot have the flag pinned onto
its new material by an old, in-flight poll. It is cleared only by installing
a verified replacement.

**One canonical restore query, extended, not duplicated.**
`RefreshCodexAccountLogin` (`api/internal/store/queries/codex_binding.sql`)
gained a second CAS arm rather than a parallel query: the existing
`quarantined` arm is unchanged; a new `REAUTH` arm fires only when
`reauth_required = true AND coord_state IN ('idle','committed') AND
recovery_sealed IS NULL AND reauth_generation = generation AND
reauth_credential_revision = credential_revision`. Both arms additionally
require `generation = @from_generation`, so a stale writer loses either way.
A verified re-login through `CodexReconciler.reconcileTuple` restores the
canonical login and links the alias inside **one transaction** — a lost link
CAS rolls the restore back, so the account never ends up with new login
material and no alias pointing at it. This is how re-authentication restores
collection **even for an account no worker has ever touched**: the flag,
the fence, and the restore all live in the idle-account path this feature
owns, independent of whether #1171's worker-side repair ever runs.

### Revision-fenced snapshot and health writes: discard, never overwrite, when authority moved

Both store writes the poller performs are fenced on the same two counters:

- `UpsertCodexAccountRateLimits` installs a successful reading only when the
  caller's observed `(generation, credential_revision)` still matches the
  live account; 0 rows means a refresh or relink committed between the
  provider call and the write, and the poller discards the reading rather
  than risk publishing a stale or wrong-account snapshot.
- `RecordCodexAccountPollFailure` is the same fence applied to a **failed**
  attempt: it updates `last_attempt_at`/`attempt_status`/`attempt_error`
  without ever touching `buckets` or `last_success_at`, so a failure after a
  success reads as "stale reading, last attempt failed" rather than
  reverting to "no reading at all."

`CollectCodexAccountUsage` performs the identical re-read-and-compare itself
right after the provider call returns (`finalizeCodexUsage`), so a reading
whose authority moved mid-call is refused before it is even handed back to
the poller, not merely rejected at the write.

### One JSONB snapshot per canonical account, keyed by an owner-scoped composite FK

`codex_account_rate_limits` (migration `00238`) is keyed `PRIMARY KEY
(user_id, provider_account_id)`, with an owner-scoped composite FK to
`codex_provider_account (user_id, id)` — the same `UNIQUE (user_id, id)`
target `00199` cut for exactly this purpose, so a snapshot can never
reference another user's account even in the schema. `buckets` is nullable
JSONB (NULL until the first success, preserved across a later failure);
`observed_generation`/`observed_credential_revision` are the fence values
above. Because the key is the **canonical account**, not the alias, several
`codex_auth` aliases that resolve to the same subscription share exactly one
row, one snapshot, and therefore one budget on every surface — duplicating a
login is never mistaken for extra capacity.

### A shared sidebar/TUI selection preference: `sidebar_codex_account_ids`

Mirroring `sidebar_token_ids` (00123) on the Claude side, `users` gained a
`sidebar_codex_account_ids uuid[]` column (migration `00238`). The account
holding the user's current default `codex_auth` alias is always shown and
needs no entry in the set; the column holds only the **additional** accounts
the user opted into. Both the web sidebar and the TUI read the same
persisted preference through their existing settings-fetch paths, so a
Settings checkbox change reaches both surfaces without a TUI restart. The
handler validates every submitted id against the caller's own linked
subscription accounts and silently prunes an id that has since been deleted
or unlinked, rather than selecting a fallback account in its place. This
preference is purely a **display** choice — it never touches polling
(every linked account is polled regardless of what is checked), credential
defaults, run routing, or quota eligibility.

### SSRF bounding: a fixed host, refused redirects, a bounded body

`codexauth.Client.ReadUsage` (`api/internal/codexauth/usage.go`) is a plain
GET against `c.usageBase + "/wham/usage"`, where `usageBase` defaults to the
fixed `DefaultUsageBaseURL` (`https://chatgpt.com/backend-api`) and is not
influenced by any per-account or per-request input. The client's
`CheckRedirect` refuses **every** redirect
(`http.ErrUseLastResponse`) — load-bearing here because the request carries
a custom `ChatGPT-Account-Id` header Go would otherwise forward across a
redirected, possibly cross-host hop; a redirect response is surfaced as a
non-2xx and treated as a `transient` failure rather than followed. Every
response body — success or failure — is read through
`io.LimitReader(resp.Body, maxBodyBytes)` (1 MiB), so a hostile or broken
upstream cannot make the poller buffer an unbounded body. A decoded bucket
set is further capped (`maxAdditionalRateLimits`, 16 entries) and every
label is sanitized and length-bounded (`sanitizeBucketLabel`) before it can
reach a stored or cross-user-displayed DTO.

### The `UZI_CODEX_USAGE_POLL_INTERVAL` gate

A dedicated interval, separate from the Claude engine's
`UZI_USAGE_POLL_INTERVAL`, defaults to 5 minutes. `0` disables the whole
engine — no boot pass, no ticker, no poke-on-credential-save, no staged-alias
reconcile — exactly mirroring what "0" means for the Claude poller. A
nonzero value under 1 minute is clamped up with a boot warning, not because
the read spends a user token (the Codex usage GET is free) but to bound
provider egress: an unbounded interval fires one request per linked account
per tick for no added freshness. The isolated e2e compose stack sets it to
`"0"` alongside the Claude setting, because the usage base URL is a fixed
external constant and the stack must never call the real provider host.

## Consequences

- A Codex subscription account's usage is visible with zero active runs or
  workers, on every surface Claude's meters already reach, without adding a
  second, weaker path to the access token #1171 protects.
- The idle-account re-authentication gap #1171 did not have to close (no
  worker had ever touched such an account) is now closed by this feature's
  own fenced discriminator and restore arm, not deferred to a future run.
- Duplicate Codex logins of one account, and an API-key-only default,
  produce no double-counted or fabricated budget: the schema and the DTO
  both key on the canonical account, and an API key is never sent to the
  usage endpoint at all.
- The display preference, the poller, and the credential coordination core
  are three independently-fenced layers; a bug in one (e.g., a stale
  selection id) cannot silently corrupt another (a pruned id never causes a
  fallback poll target or a wrong meter).

## Maintainer acceptance procedure (PENDING — not performed by the uzi worker)

Everything above was implemented and tested (unit, live-DB, and a fake-provider
end-to-end proof) by the uzi worker run that produced this ADR. **None of that
substitutes for live acceptance against a real Codex subscription on the
primary Kubernetes runtime**, and the PRD is explicit that this stays pending
until a maintainer runs it. Do not read the implementation's tests, this
ADR, or any docs update as evidence the following has happened — it has not.

1. **Deploy** the release carrying this change through the normal release
   workflow (API + web + CLI).
2. **Link a real, explicitly authorized test subscription** as a Codex login
   (see [docs/codex-credentials.md](../docs/codex-credentials.md)), with the
   secret injected safely (never typed into a shell history or committed
   anywhere).
3. **Confirm a reading appears with zero active Codex runs or workers.**
   Nothing should need to run for the meter to populate within one poll
   interval.
4. **Confirm the values agree across every surface for one captured
   snapshot**: Settings' Codex limits card, the sidebar, Admin → Rate
   limits, `uzi rate-limits --provider codex` / `uzi admin rate-limits
   --provider codex`, and the TUI must all show the same account/window
   percentages and reset timestamps at the same moment.
5. **Confirm an API-key-only credential produces no subscription polling** —
   no meter, no attempted read, just the "subscription windows do not
   apply" explanation in the credential surface.
6. **Confirm usage incurred outside uzi appears on a subsequent poll** (use
   the subscription from another Codex client and watch the next tick pick
   it up), and that **duplicate imports of the same account still show one
   budget**.
7. **Vault lock / unlock**: lock the owner's vault and confirm the last
   reading is retained and marked stale/vault-locked (never a false zero);
   unlock and confirm collection resumes.
8. **A controlled expired generation renews cleanly**: force (or wait for) a
   token expiry on the test account and confirm the shared coordinator
   renews it without losing the login and without starting any model turn.
9. **Toggle an additional account's "Show in sidebar and TUI" checkmark**
   on and off; confirm the account appears/disappears on both the web
   sidebar and a running TUI after their next settings refresh, that the
   subscription-default meter and existing Claude selections are
   unaffected, and that the choice survives a browser reload / TUI restart.
10. **Record sanitized outcomes only** — versions, timing bounds, and
    pass/fail per step above. Never record a token, a raw provider id, a
    session id, or a private deployment coordinate.

Only a maintainer completing all ten steps against the primary runtime may
mark this feature's live acceptance as done; a green CI run, a passing
fake-provider e2e proof, or a UI screenshot is explicitly insufficient on
its own (PRD #1209, "Maintainer acceptance and completion criteria").
