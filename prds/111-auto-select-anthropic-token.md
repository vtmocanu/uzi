# PRD #111: Auto-select the Anthropic token per run by rate-limit headroom, and record which token each run used

**GitLab Issue**: [#111](https://gitlab.example.com/vtmocanu/uzi/-/issues/111)
**Status**: **IN PROGRESS** (created 2026-07-22; implementation started 2026-07-27 on `feature/prd-111-auto-select-token`)
**Priority**: Medium
**Related**: [#104](https://gitlab.example.com/vtmocanu/uzi/-/issues/104) (named tokens — this builds directly on its per-token rate-limit gauge and its single credential-resolution seam), [#53](https://gitlab.example.com/vtmocanu/uzi/-/issues/53) (rate limits — the gauge this PRD reads to choose), [#40](https://gitlab.example.com/vtmocanu/uzi/-/issues/40) (token usage reporting — the per-run token record this PRD adds is the attribution join #40 could not make)

Seven milestones. M1 (record token per run) and M2 (opt-in pool) are
file-disjoint and land in parallel; M3 (worker `auto` mode) needs M2; M4 (the
selector) needs M1, M2, M3; M5 (observability) needs M1+M4; M6 (tests) and M7
(docs + specs) land last. Nothing in `agent/` changes — the worker never holds
the token, so choosing a different one is entirely a question of **which row the
API selects at claim time**.

## Problem

Since PRD #104 a user can hold several named Anthropic credentials, point each
worker at one by name, and rebind without re-provisioning. Two gaps remain.

**1. Binding is static and manual.** Anthropic's 5-hour and 7-day windows are
per-credential, and uzi already **measures** each token's utilization: the
`anthropic_rate_limits` gauge is keyed on `user_secret_id`
(`api/internal/store/migrations/00080_rate_limits_per_token.sql`), refreshed
every poll tick by `usagepoller/engine.go`, and surfaced per token as
`TokenRateLimitDTO` (`api/internal/apitypes/ratelimit.go`). But nothing
*consumes* that signal to place work. A worker pinned to token A keeps hammering
A after it throttles while token B sits at 0% — the user has to watch the meters
and rebind by hand. The data to choose automatically is already collected; only
the chooser is missing.

**2. A run never records which token paid.** Credential resolution happens at
claim time in exactly one place — `claimSecretID`
(`api/internal/workersvc/service.go:768`) → `workerSecretID`
(`:779`), consumed by `assembleClaim` at `service.go:818` — and the result is
used to open the token and then discarded. `run_usage` (PRD #40, migration
`00062`) records what a run spent but not which credential spent it; with one
token per user that was tautological, and PRD #104 explicitly left it as a known
gap (its D3/R6). The moment a user holds two tokens — and certainly once uzi
picks between them automatically — "which account did this run bill?" has no
answer in the data.

The user-facing ask: **let a worker choose its token automatically from a pool
I opt in, preferring the account with the most headroom, and show me which token
each run actually used.**

## Why this is cheap

**Workers never hold the token.** The API decrypts it and ships it inside each
claim response; the agent consumes it as an env var it never persists (PRD #104,
"Why this is cheap on the worker side"). Resolution is already **per-claim**, so
a different choice takes effect on the worker's next claim with no restart and no
re-minted join token. Auto-selection is therefore a change to one function's
return value plus the surfaces that configure and display it — the whole feature
lands on the API and web/CLI, and `agent/` is untouched.

## Decisions settled before drafting

Three forks were resolved with the user up front; they shape every milestone.

- **D1 — Scope: a per-worker third bind mode.** A worker is `default`,
  `pinned:<token>`, or `auto`. This composes with the existing
  `workers.anthropic_secret_id` seam (migration `00078`) rather than replacing
  it: you can auto some workers and pin others, and a pinned worker always wins
  over any global heuristic. Rejected: a per-user global toggle (too coarse — a
  user wants the retrospective worker pinned even while the rest auto-balance).

- **D2 — Candidate set: an opt-in pool, per token.** Auto-selection spends only
  tokens the user has flagged `auto_eligible`. This is not paranoia: the product
  already documents holding a subscription token "for the work" and a console key
  "for the retrospectives" (`docs/anthropic-token.md`). Auto-selecting over
  *all* tokens would spend the reserved key on ordinary runs. Default is
  **false** — opting a token in is a deliberate act.

- **D3 — Ranking: least-consumed first, a within-threshold tie broken by soonest
  reset.** Primary key is headroom; the second criterion (prefer the account
  about to replenish) only fires when two tokens are *essentially as empty as
  each other*. See "The ranking, precisely" — the naive form of this is a real
  bug, so the algorithm is specified, not left to the implementer.

## The ranking, precisely (M4's core)

Let `headroom(t) = min(100 − five_hour_pct, 100 − seven_day_pct)` — the binding
window is whichever is fuller, and 7-day is the hard cap.

**Eligibility gate.** A token is a candidate iff all hold:
1. `auto_eligible = true` (D2);
2. it has a gauge row with **non-NULL** `five_hour_pct` *and* `seven_day_pct` — both are schema-nullable (`00080`), and a reading that measured neither tells us nothing, so treat it as ineligible (D12);
3. that row is fresh — `synced_at` within `UZI_AUTOSELECT_MAX_STALENESS` (default 2× the poll interval; a longer lag means steering on numbers Anthropic has since moved past). A token in the poller's 15-min refusal backoff (`usagepoller/engine.go`) goes stale here and drops out — see D11;
4. `headroom(t) ≥ UZI_AUTOSELECT_MIN_HEADROOM` (default 15) — a single formulation, since `headroom` already binds on the fuller window;
5. ~~the token is openable now (owner's vault not locked — reuse `secretopen`'s dispatch, same as the poller).~~ **Dropped — see D15.** The vault check is per-**user** and already happens upstream at `service.go:666`, so this gate is vacuous where it was specified; the per-token residual is caught at open time by D14 instead. Do not build a per-token openability probe.

**Fallback, in order (D10).** If the eligible set is empty *only because every
opted-in token is fresh and openable but below `MIN_HEADROOM`*, pick the
**best-of-pool** anyway — highest headroom, same tie rule below — because "least
consumed" still has a best answer and it beats ignoring the pool for the owner
default, which may itself be the most-throttled token. Only if the pool is
empty, entirely stale, or entirely unopenable does resolution fall to the
**owner default** (an `auto` worker holds no active pin — D9).

~~**Auto-selection never blocks or fails a run.**~~ **That sentence was false as
written and D14 is what makes it true** — measured 2026-07-27, before
implementation: `recoverClaimAssembly` (`workersvc/service.go:728`) maps
`errCredentialUnavailable` to `MarkRunFailedByID`, a **terminal** run failure. A
selector that picks a token whose ciphertext will not decrypt therefore kills a
run that the owner default would have completed. The guarantee holds only with
D14's retry.

**Selection among the eligible.** This is where D3's "within-threshold" lives,
and it must **not** be written as a pairwise comparator. `if |a−b| < T then
compare-by-expiry else compare-by-headroom` is **intransitive** (A=80, B=75,
C=70 with T=5: A≈B, B≈C, but A≁C), so it is not a strict weak ordering and
feeding it to a sort yields order-dependent, undefined results. Instead, anchor
the tolerance to the best token — no sort of the whole list, just cluster the
top:

```
H*   = max headroom over eligible
tie  = { t in eligible : H* − headroom(t) ≤ UZI_AUTOSELECT_HEADROOM_TIE_PCT }   # default T = 5 points
pick = the t in `tie` with the soonest reset, then the lowest secret id
```

`resets_at` is nullable (the poller writes NULL when Anthropic reports no reset,
`00080`): treat a NULL reset as **+∞** — a token that names no reset is never
"about to replenish," so it loses the tie to any token that does. Exactly-equal
resets (and the all-NULL case) fall through to the **lowest secret id**, a total
order, so the pick is deterministic — which M6 needs to assert anything at all.

`H* − headroom(t) ≤ T` is measured against one fixed anchor, so there is no
chain and no intransitivity; it says exactly "the tokens within `T` points of
the emptiest, and among those the one that refills soonest." Thresholds are in
**percentage points** (the gauge is `SMALLINT` 0..100), and both `T` and
`MIN_HEADROOM` are server env knobs so they tune without a code change.

**In-flight bias (herd control).** The gauge lags the poll interval, so several
claims inside one interval all read the same headroom and would pile onto the
same emptiest token. Because M1 records the token on each run, the count of
*currently running* runs per token is a cheap query; subtract
`UZI_AUTOSELECT_INFLIGHT_PENALTY` (default a few points) per in-flight run from
that token's headroom before ranking. This is a bias, not a hard cap — an empty
token still wins even with a couple of runs on it, but ties break toward
spreading.

## Milestones

| # | Milestone | Primary files | Depends on |
|---|---|---|---|
| **M1** | **Record the token per run.** `runs.anthropic_secret_id` (nullable, `ON DELETE SET NULL`) **+ `anthropic_secret_label`** snapshot, **plus M5's `anthropic_select_reason` + `anthropic_headroom_pct` in the same migration** (D19). Recorded at the **assemble** points where the concrete id is known — the run lane resolves the default id *explicitly* (D8) so the recorded id equals the opened one, and the judge (`assembleJudgeClaim`) and chat lanes record theirs the same way. Expose in the run DTO; render in run view + CLI; join to `run_usage` for per-token cost. | `store/migrations/`, `workersvc/service.go` + `judge.go` + `chat.go`, **`handler/workers.go`** (`runToDTO` lives there, NOT in `handler/runs.go`), `web/src` run view, `cmd/uzi/run.go`, `apitypes/wire_test.go` golden | — |
| **M2** | **Opt-in pool.** `user_secrets.auto_eligible BOOLEAN NOT NULL DEFAULT false`; a per-token toggle on **Settings → Anthropic tokens** that also renders each opted-in token's **live auto-eligibility** (fresh / stale / **never polled** / below-threshold — D11 as amended by D16), so a token that can never be picked is visible, not a silent no-op; CLI support via a **new narrow route** (D13, *not* the existing PATCH); add the flag to `TokenRateLimitDTO`. **Ships `autoselect.Classify`** — the eligibility classifier is shared with M4's ranker and must not be implemented twice (D21), so the package is born here even though the PRD orders it under M4. | `store/migrations/`, new `api/internal/autoselect/`, `handler/secrets.go` + `handler/handler.go` (route), `web/src/pages/Settings.tsx`, `cmd/uzi/token.go`, `apitypes/ratelimit.go` | — |
| **M3** | **Worker `auto` mode.** `workers.anthropic_bind_mode` enum `{default,pinned,auto}`, backfilled `pinned` where `anthropic_secret_id IS NOT NULL` else `default`; the worker picker on **Settings → Workers** gains an **auto** choice; CLI parity. | `store/migrations/`, `workersvc/service.go`, `web/src/pages/WorkersSettings.tsx`, `cmd/uzi/` | M2 |
| **M4** | **The selector.** New `autoselect` package implementing the gate + `H*` anchor + within-`T` tie + soonest-reset + in-flight bias + fallback; wired into `claimSecretID` so `auto` mode calls it and every other mode is unchanged. | new `api/internal/autoselect/`, `workersvc/service.go`, `store/queries/` | M1, M2, M3 |
| **M5** | **Observability.** Run view / DTO / CLI show the chosen token **and the mode that chose it** — `<label> — auto, N% headroom` vs `<label> — default` vs `<label> — pinned` (D20) — and, on fallback, why. Its columns land in M1's migration (D19), so M5 is render-only. The admin per-user, per-token meter (PRD #104 M5) already exists; link the two. | **`handler/workers.go`** (`runToDTO`), `web/src`, `cmd/uzi/`, `workersvc/` | M1, M4 |
| **M6** | **Tests.** Store live-DB coverage for the new columns/queries; `autoselect` unit tests (gate, `H*` clustering, tie→expiry, the A/B/C=80/75/70 intransitivity guard, NULL pct/reset handling, in-flight bias, every fallback branch); an e2e phase asserting `auto` picks the emptiest seeded token **and** falls back to default when the poller is disabled; a **k8s validation** pass on dev-cluster (hosted workers share the `workers` table + claim path, and per CLAUDE.md k8s is the primary runtime). | `*_integration_test.go`, `autoselect/*_test.go`, `e2e/`, k8s | M1–M5 |
| **M7** | **Docs + specs.** `docs/anthropic-token.md` gains the auto mode + the pool flag; `docs/cli.md` the new subcommands; `specs/ai.md` records D1–D3 and the ranking; ARCHITECTURE.md if the run-token record warrants a line. | `docs/`, `specs/ai.md`, `ARCHITECTURE.md` | M1–M6 |

**Parallelization.** Phase 1: **M1 ∥ M2** (disjoint files — `runs` + run view
vs. `user_secrets` + tokens settings). Phase 2: **M3** (needs the pool). Phase 3:
**M4** (needs all three). Phase 4: **M5**. Phase 5: **M6 ∥ M7**.

## Data model

Three additive migrations. **The draft numbers `00082`–`00084` written here at
drafting time are now taken** — `00082_run_stop_kind_auto.sql`,
`00083_worker_roll_health.sql` and `00084_seed_builtin_skill_allocations.sql`
landed since, and the live head is `00085_run_prd_done_path.sql` (measured
2026-07-27). Implementation starts at **`00086`**, and renumbers again above the
live head at landing if `main` moves under us, per the goose convention. The boot
runner is strict goose with no `allow-missing`, so landing a version below an
already-applied head makes every upgraded instance refuse to boot.

- `runs.anthropic_secret_id UUID` + `runs.anthropic_secret_label TEXT`, both
  nullable. `ON DELETE SET NULL` on the id so deleting a token never cascade-
  deletes run history; the **label snapshot is why** the id going NULL is
  survivable — the run still shows which account it was even after the token is
  renamed or deleted. Composite-FK ownership `(user_id, anthropic_secret_id) →
  user_secrets (user_id, id)` as in `00078`/`00080`, with the column-list
  `SET NULL (anthropic_secret_id)` so a token delete does not null `runs.user_id`.
- `user_secrets.auto_eligible BOOLEAN NOT NULL DEFAULT false`. Meaningful only
  for `kind = 'anthropic_token'` rows; the default keeps every existing token out
  of the pool until opted in (D2).
- `workers.anthropic_bind_mode TEXT NOT NULL DEFAULT 'default' CHECK (… IN
  ('default','pinned','auto'))`, backfilled in the same migration:
  `pinned` where `anthropic_secret_id IS NOT NULL`, else `default`. The id is
  read **only** in `pinned` mode. No CHECK couples mode to id — it *cannot*,
  because `00078`'s FK nulls the id on token-delete (`SET NULL`) while leaving
  the mode, so a coupling CHECK would make that legal delete fail. Instead,
  **`pinned` with a NULL id resolves as `default`** (D9) — already exactly what
  `workerSecretID` does today (`service.go:779`: invalid id → nil → default), so
  it is not a new rule, just one kept true under the new column.

## Decision log

- **D4 — Judge and self-improve lanes keep their explicit bindings.**
  `claimSecretID` routes `self_improve` to the judge binding and the judge lane
  forks to `assembleJudgeClaim` earlier (`service.go:768`, `:795`); neither
  participates in auto-selection in this PRD. Auto is a run-lane placement
  decision; billing review separately is the judge binding's whole job, and
  auto-spreading it would defeat that. Extending auto to the judge lane is a
  clean follow-up, explicitly out of scope here.
- **D5 — Chat runs are unaffected.** Chat always uses the owner default
  (`docs/anthropic-token.md`); it does not ride a worker binding and gains no
  auto mode.
- **D6 — Thresholds are server env, not per-user settings.** `MIN_HEADROOM`,
  `HEADROOM_TIE_PCT`, `MAX_STALENESS`, `INFLIGHT_PENALTY` ship as
  `UZI_AUTOSELECT_*` env with defaults. Per-user tuning is a future refinement;
  starting with one operator-set policy keeps M4 testable and the UI simple.
- **D7 — Fallback is silent-but-recorded, never a failure.** An empty eligible
  set resolves the worker's non-auto behavior and M5 records the reason. A run
  never fails because the optimizer had nothing to pick.
- **D8 — Recording forces the default id to be resolved explicitly.** Today a
  `default`-mode run returns `nil` from `claimSecretID` (`service.go:768`) and
  the concrete credential is chosen *inside* `secretopen.Open` (`service.go:736`),
  which hands back only `[]byte` — so there is no id to record. M1 makes the run
  lane resolve the owner default's id up front (reusing `GetDefaultUserSecretID`,
  already called by the poller) and always open **by id**, so the recorded id is
  the opened one and a rotate between resolve and open can no longer bill a
  different token than the run recorded. The judge and chat lanes record at their
  own assemble points the same way.
- **D9 — `pinned` with a NULL id resolves as `default`.** See the data-model
  note above: a coupling CHECK is impossible against `00078`'s `SET NULL`, and
  this rule is what `workerSecretID` already enforces, so no new behavior — the
  UI renders such a worker as using the default, honestly.
- **D10 — Below-threshold pool falls back to best-of-pool, not owner default.**
  When every fresh, openable pool token is under `MIN_HEADROOM`, the least-bad
  answer is the emptiest of them, which is what "least consumed" means; falling
  to the owner default could pick a *more*-throttled token that happens not to be
  in the pool. Owner default is reserved for a pool that is empty/stale/locked.
- **D11 — Refusal backoff and the freshness gate interact by design, but must
  be visible.** The poller arms a fixed 15-min per-token backoff on a definitive
  refusal (`usagepoller/engine.go`); during it the gauge goes stale and the token
  drops from the pool — correct, since an un-pollable credential should not be
  auto-spent. The hazard is that this is *silent*: an opted-in token that never
  polls looks active while never being chosen. M2 therefore renders each token's
  live eligibility (fresh / stale / refused / below-threshold), and M6 checks
  whether OAuth setup-tokens can ever produce a fresh gauge at all (see R7).
- **D23 — `GET /api/me/rate-limits` moves to `RequireUser`, on non-additivity —
  NOT on a sensitivity ranking.** Added 2026-07-27 after M2 shipped, on an
  auditor ruling the lead deliberately did not make alone. **The problem**: M2
  gives the CLI a pool toggle, but the endpoint that reports live eligibility is
  cookie-only, so a scripted `uzi token pool x --on` gets no signal that `x` can
  never be picked — reintroducing exactly the silent no-op R7 and D11 exist to
  kill.
  **The lead's first argument for widening was rejected and must not be
  reinstated**: "rate-limit percentages are less sensitive than the labels and
  ids already exposed beside them" ranks *identifiers* against *behavioral
  telemetry* and concludes from the ranking. That is the "it's only metadata"
  move, and it would equally justify putting per-run cost on a shared board. The
  percentages are more sensitive **in kind** and less sensitive only in
  resolution. Two legs hold instead:
  1. **Non-additivity.** Every inference this endpoint enables is already
     available at *finer* granularity through routes that are already
     `RequireUser`. `GET /api/runs` carries per-run `UsageDTO` — input, cache,
     output tokens, `cost_usd`, plus three timestamps — which is a timestamped
     consumption series, strictly finer than a 0-100 aggregate refreshed at the
     poll interval. And `POST /repos/{id}/runs` is `RequireUser`, so a stolen
     `uzc_` can already **spend** the victim's quota; knowing when it resets is a
     rounding error against being able to burn it.
  2. **It is a GET of the caller's own row.** `SelfRateLimits` is a single
     owner-scoped read with no outbound call and no `usagePoker.Poke` (so it
     opens no amplification vector against Anthropic), it mints nothing, and it
     never reads `IsAdmin` — no admin branch, so no escalation path.
  **The caveat is part of the decision, not a footnote: non-additivity is a
  property of the CURRENT route table, not of this endpoint.** If `/api/runs`'s
  usage fields or `CreateRun` were ever moved back to cookie-only,
  `/me/rate-limits` under `RequireUser` would become the widest remaining
  activity channel and this decision must be revisited. A future reader inherits
  the condition, not only the conclusion.
  Rejected: putting `auto_status` on `SecretDTO` instead — it adds a third query
  feeding `Classify`, widening D21's differential surface in M6, for a worse
  result (the status without the meters beside it is less useful than the
  meters). Also rejected: leaving it, which keeps R7 live for scripted use.
  **Implementation constraint**: split the route group, do **not** change the
  group's middleware — `PUT /me/autopilot` and `PUT /me/judge` share it and must
  stay cookie-only. `route_limiter_mounts_test.go` pins the **limiter**, not the
  auth middleware, so it cannot catch a mistake here; the guard must assert both
  that a Bearer request reaches the GET **and** that the two PUTs still 401.
- **D19 — M5's two columns land in M1's migration.** `anthropic_select_reason`
  and `anthropic_headroom_pct` on `runs`. M5 as drafted had nowhere to persist
  "why it fell back", and M1 is already altering `runs`; folding them in costs
  one migration instead of a fourth and leaves M5 render-only.
- **D20 — The run view names the MODE, not just the token.** `<label> — auto,
  62% headroom` vs `<label> — default` vs `<label> — pinned`. The label alone
  cannot answer the user's actual question, because an auto pick and a default
  fallback can name the same token; PRD #104's compat path also creates a row
  labelled literally `default`, so the label is not even a reliable hint. (The
  related worry that a default token might have *no* label is unfounded:
  `user_secrets.label` has been `NOT NULL CHECK (char_length BETWEEN 1 AND 64)`
  since `00077`.)
- **D21 — One eligibility classifier, not two.** M2 renders each token's live
  eligibility and M4 gates candidates on the same predicate. Two implementations
  of one rule drift, and the drift is invisible: the Settings page would promise a
  token is eligible while the selector skips it. So `autoselect.Classify` is
  written once in M2, consumed by both, and the status string is computed
  **server-side** — web and CLI render what the API says rather than re-deriving
  it. The residual is that two different SQL queries feed it (M2's per-user list,
  M4's ranking query), which is pinned by a live-DB differential test asserting
  the two produce identical `Candidate` values **except** `InFlight`, which only
  M4's query populates.
- **D14 — On `auto`, an `errCredentialUnavailable` open retries once on the
  non-auto binding.** Added 2026-07-27 from the architect's design pass; the lead
  re-derived it. Without this, D7's "never fails a run" is simply untrue: a token
  that passes the gauge gate but whose ciphertext will not decrypt (or which was
  deleted between the ranking query and the open) reaches
  `recoverClaimAssembly`, which fails the run terminally. Auto would then kill
  runs that static binding completes — a regression introduced by the optimizer,
  which is the one outcome D7 exists to forbid. So: on `auto` mode **and**
  `errCredentialUnavailable`, retry once against what the worker would have used
  without auto (the owner default, per D9), and record `reason=open_failed`.
  Explicitly **not** for `errVaultLocked` — that path already requeues the run
  (`service.go:730`), which is transient and correct; retrying it would convert a
  wait into a spend on the wrong account.
- **D15 — Eligibility drops "openable now" as a per-token gate; it is already
  enforced per-user, upstream.** The PRD's gate #5 asks whether the token can be
  opened. Measured: `Claim` returns idle when the owner's vault is locked
  (`service.go:666`, an in-memory per-**user** check) and `ClaimRun` is scoped to
  `wkr.UserID`, so by the time the selector runs, the owner's vault is unlocked
  by construction. There is no per-token openability signal short of attempting
  the decrypt, and attempting it for every candidate would decrypt secrets we are
  not going to spend. The residual risk — one specific token being individually
  undecryptable — is exactly what D14 catches at open time, which is the right
  place for it. **Do not build a per-token openability probe.**
- **D16 — The live per-token eligibility states are `fresh` / `stale` /
  `never polled` / `below threshold`. `refused` is not one of them.** D11 promised
  a `refused` state; nothing can serve it today. The poller's 15-minute refusal
  backoff is an unexported in-process map (`usagepoller/engine.go`) with no
  accessor, and `anthropic_rate_limits` has no column recording a failed poll
  (measured: `user_secret_id, user_id, five_hour_pct, five_hour_resets_at,
  seven_day_pct, seven_day_resets_at, source, synced_at`). Rather than widen the
  poller for state that dies on restart, or add a column, we split what the
  LEFT JOIN already distinguishes: a token that **has never produced a reading**
  renders `never polled`, one whose reading has aged out renders `stale`.
  This keeps D11's actual safety property — a token that can never be picked is
  visible rather than silently idle — and it serves R7's case better than
  `refused` would have, since a credential the usage endpoint permanently refuses
  never produces a row at all and now says so. What is lost is the *diagnosis* of
  a token that polled before and is currently backing off; it reads `stale`.
- **D17 — `MAX_STALENESS` defaults to 3× the poll interval so the selector and
  the meter agree, and the meter is not touched.** The PRD drafted 2×;
  `rateLimitStale` (`handler/ratelimits.go:150`) already ships 3× and its D3
  documents that. Two definitions means the meter can call a token fresh while
  the selector calls it stale, which is precisely the invisible no-op D11 exists
  to prevent. Defaulting the new knob to 3× makes them agree **without changing
  any existing behavior**. Residual, stated honestly: they agree by matching
  defaults, not by sharing one definition, so an operator who overrides
  `UZI_AUTOSELECT_MAX_STALENESS` re-opens the divergence. Unifying them means
  changing what the shipped meter says, which is a user-visible change to
  existing functionality and out of scope for this PRD.
- **D18 — The in-flight bias counts every lane and every reason, and
  `awaiting_approval` counts.** Three sub-decisions, together because they share
  one rationale: the count models **concurrent spend against a credential**, and
  nothing about how a run acquired that credential changes the quota it consumes.
  So (a) it counts chat and judge runs too, which M1 makes possible for the first
  time — this **supersedes R3's** stated limit, and M7 must correct R3 rather
  than copy it; (b) it does not filter on the selection reason, since excluding
  fallback-chosen runs would blind the bias to exactly the pile-up a fallback
  creates; (c) `awaiting_approval` counts, because the worker holds the session
  and resumes on the same token. (c) is the arguable one — an awaiting-approval
  run is idle spend at that instant — and it is a deliberate conservative choice,
  not an oversight.
- **D13 — The `auto_eligible` toggle ships as its own narrow route, not on the
  existing token PATCH.** Added 2026-07-27, during implementation, from an
  auditor pre-flag; the lead re-derived it against the tree before settling it.
  M2 says "CLI `token` support", and the obvious way to deliver that — extend
  `PATCH /me/secrets/anthropic_token/{id}` and move it where the CLI can reach it
  — is wrong. `handler/handler.go` splits the secrets routes deliberately: GET is
  `RequireUser` (metadata only, CLI-reachable) and **every write is `RequireAuth`,
  cookie-only**, because "a Bearer-reachable mint would let a stolen `uzc_`
  replace a user's tokens" (PRD #104 D8). Relocating that PATCH would make
  rename, rotate and set-default Bearer-reachable as collateral damage. Instead:
  `PATCH /me/secrets/anthropic_token/{id}/auto-eligible` under `RequireUser`,
  with the existing writes untouched. The precedent is exact — `PATCH
  /workers/{id}` is `RequireUser` because "it mints nothing and yields no
  credential the caller lacks — it only re-points a worker at a token they
  already hold" — and the toggle is the same class, re-pointing spend among
  tokens the caller already holds. R5's CLI parity is met without widening the
  credential-write surface by one route.
- **D12 — A gauge row with NULL pct is ineligible.** The columns are nullable
  (`00080`); a reading that measured neither window carries no headroom signal,
  so it cannot be ranked and is excluded rather than defaulted to some
  assumed-full value.

## Risks

- **R1 — Intransitive comparator.** Addressed head-on in "The ranking,
  precisely": anchor-to-best clustering, not pairwise tolerance. M6 pins it with
  the A/B/C=80/75/70 case so a future refactor cannot quietly reintroduce a
  `sort.Slice` over an intransitive `less`.
- **R2 — Poller disabled ⇒ no gauge.** `UZI_USAGE_POLL_INTERVAL=0` (the e2e
  overlay sets exactly this) means no fresh rows, so every token fails the
  freshness gate and auto degrades to default. That is the correct behavior and
  M6 asserts it as a first-class case, not an accident.
- **R3 — Staleness / herd.** Mitigated by the freshness gate (R2's mechanism)
  plus the in-flight bias. Neither is perfect against a burst; both are honest,
  and the bias is only possible because M1 lands first. ~~The bias also counts
  **run-lane** runs only — chat always spends the owner default (`chat.go:177`)
  and the judge lane its own binding, so a default token that is also a pool
  member carries concurrent spend the bias cannot see. An acknowledged limit,
  not corrected here.~~ **That limit was corrected — see D18.** It was a
  consequence of having no per-run token record, and M1 removes exactly that:
  once every lane records the token it used, counting chat and judge runs is
  strictly more accurate than excluding them, so the bias counts all lanes. The
  residual limit is now only the poll-interval lag itself.
- **R4 — Resolution must stay in one place.** PRD #104's R4 warns that three
  copies of credential resolution drift and a wrong fallback spends the wrong
  account silently. M4 adds the selector *behind* `claimSecretID`, not beside it;
  `openAnthropic` and `assembleClaim` are untouched.
- **R5 — CLI parity.** Per CLAUDE.md, a worker-binding or token change that only
  updates `web/` leaves the CLI stale. M2/M3/M5 each carry their `cmd/uzi`
  change in the same milestone.
- **R6 — Attribution is improved, not perfected.** The per-run record names the
  token at **claim** time. A mid-run worker restart re-claims and could, under a
  future extension, land on a different token; M1 records the claim-time choice,
  which is the same granularity `run_usage` already has. Good enough to answer
  "which account paid," and the honest limit is documented.
- **R7 — A pool token that never polls is a silent no-op.** A credential the
  usage poller cannot read (definitive refusal → 15-min backoff, or a token kind
  the endpoint refuses with no working header-probe fallback) never has a fresh
  gauge, so opting it into the pool changes nothing while *looking* active. D11's
  per-token eligibility rendering is the mitigation — the token shows as
  refused/stale, not silently idle — and M6 verifies whether OAuth setup-tokens
  hit this.

## Success criteria

- A worker in `auto` mode, with two opted-in tokens whose gauges differ, claims
  against the emptier one; when they are within `T` points, against the one that
  resets sooner.
- Opting a token *out* of the pool immediately removes it as a candidate on the
  next claim; pinning a worker still overrides auto.
- Every run — run, judge, and chat lane — names in its view (web + CLI) the
  token it used, and the name survives that token being later renamed or deleted.
- With the poller disabled, `auto` workers behave exactly as `default` workers —
  no failures, a recorded fallback reason.
- `run-e2e.sh` green including the new auto-selection phase, **and** the auto
  path validated on dev-cluster hosted workers (CLAUDE.md's k8s-primary rule).
