# PRD #3251: Show the Codex catalog's default reasoning effort beside "Runtime default"

**Issue:** #3251 (child B2 of epic #1703)
**Sibling issues (children of #1703):** A #3255, B #3247, B2 #3251, C #3245, D #3248, E #3257, F #3254, G #3249, H #3244, I #3263, J1 #3256, J2 #3260, K #3261, L #3262, M #3259, N #3258, O #3264, P #3250, Q-spike #3246, Q-claude #3252, Q-codex #3253.  
**Status:** Draft r3 2026-10-21, redesigned to the smaller Codex-only catalog design (user decision 2026-10-21). Source anchors checked against origin/main `522aab1ba`. Codex facts from the pinned native package source (`agent/codex/codex-package.lock`: 0.162.1, `092d3acd6bec3e3a14bdc7e7a2810ab628ab759d`).
**Priority:** Normal.
**Parent:** #1703. **Blocked by:** #3247 (the `runtime` preference, the Codex `model/list` probe, `apitypes.EffortSettingCellDTO`), #3245 (the settings label table and the "Model settings" CLI table). **Blocks:** #3254 (reads `CodexCatalogDefault` and the codex cell's `catalog_default`), #3263 (Models tab shows the label).
**Related:** #2073 (its "workers report each runtime's default effort" item, absorbed into F by the epic, moved here and narrowed to Codex), shared design v5 §3 option (b). No dependency on child H: a catalog lookup is runtime-and-account metadata, not a run observation.
**Mock:** none; the design mock predates the label. Presentation follows child B's Run defaults option and C's "In effect" line.
**Delivery:** one PRD, one gated implementation run, one PR, one milestone. Default harness. Needs Docker for LiveDB tests. No implementation or validation writes under `.github/workflows/`. Migration 00354 is a draft; renumber at landing with `task migration:renumber`.

## Scope gate

One outcome: "a user choosing Codex Runtime default sees the level the Codex catalog would use for their model and credential, or an honest unknown". One small table, one worker route, one claim field, one read. One milestone, because every layer is needed for the first visible value and none is valuable alone.

## Problem

After child B, a Codex user can choose **Runtime default**, but the option only says "Codex decides". What Codex decides is its catalog's `defaultReasoningEffort` for the run's model, which differs by model (0.162.1 bundled catalog: `gpt-6.1-sol` and `gpt-6-astra` `low`, `gpt-6-sol` `medium`), by runtime version (the 0.153.2 catalog in ADR-1106 had `gpt-6-astra` at `medium`) and by account (the catalog can refresh from the provider per account). A value reported for one credential or version must not be shown for another, and a value reported before the user replaced their credential must not show as current. Claude has no catalog API and its init `effort` is host-dependent, so Claude keeps child B's "Runtime default · value not reported".

## Verified baseline (origin/main 522aab1ba, plus sibling drafts)

| Seam | Anchor | Current or planned behavior |
|---|---|---|
| Runtime preference and effort cell | child B: `agenttmpl.EffortRuntime`, `ResolveHarnessEffort` → `{Mode, Level, Source}`; `apitypes.EffortSettingCellDTO {harness, effort, default_effort, default_source, resolved_effort, source, mode}` as `effort_settings` on GET/PUT `/api/me/settings` | B labels the Codex option "Runtime default · Codex decides", the Claude option "Runtime default · value not reported". |
| Codex catalog probe | child B §4: `CodexHarness` option `catalogProbe { model, timeoutMs, onEntry }` delivering `{ defaultEffort?, supportedEfforts }` from one bounded `model/list` after the root thread starts; awaited before the first turn; supplied on the claim's first root epoch for explicit-level claims | `defaultEffort` is parsed but unused in B. |
| Codex default semantics (0.162.1) | `build_reasoning` (`core/src/client.rs`): `effort.or_else(model_info.default_reasoning_level)`; `Model.defaultReasoningEffort` from `ModelPreset::from(ModelInfo)` (`protocol/src/openai_models.rs`), which maps an absent `default_reasoning_level` to `ReasoningEffort::None` | With no explicit effort, a request uses the catalog default. A reported `"none"` means either the level `none` or "the catalog has no default level, so no effort is requested". |
| Credential rotation under one secret id | `BumpCodexMaterialRevision` (`store/queries/codex_credentials.sql`; only caller `handler/secrets.go`, the material-replace path) advances `codex_credential_state.material_revision` and drops the account link; `codex_provider_account.generation` advances on every token refresh of the same account (00199) | A user replacing an API key or re-logging under the same alias bumps `material_revision`; a routine refresh does not. |
| Run binding | `freezeCodexBinding` (`workersvc/codexauthz.go`) freezes `runs.codex_secret_id`, `codex_auth_mode`, `codex_material_revision`, and for a linked subscription `codex_account_key` (via `SetRunCodexFrozenIdentity`) at run creation; `codexCheckMaterialRev` refuses a claim whose frozen `material_revision` differs from the alias's current one | The material a claim releases always carries the run's frozen `material_revision`. |
| Credential a new run would bind | `resolveUsableCodexCredential` (`workersvc/harness_resolver.go`) → `codexCredentialChoice{SecretID, Label, AuthMode}`: one default slot, subscription only when linked, no fall-through to an API key | Deterministic. |
| Codex worker model | lane `default_codex_model`, else `agenttmpl.DefaultCodexModel` (`gpt-6.1-sol`); child G adds a lead-template Codex pin before the provider default (`agenttmpl.PinForHarness`) | |
| Worker write precedent | decisions-memo routes in `handler/handler.go` (`r.With(h.refuseJobRuns)`, `jobRefusedRoutes`), `httpx.DecodeJSONStrictBounded(r, &req, limit)` (`httpx/respond.go`), `wantRouteMounts` / `TestEveryRouteCarriesItsExpectedPerUserLimiter` (`handler/route_limiter_mounts_test.go`), `laneWorkerRouteGuard` | The precedent child H also follows (cross-review nit 16). |
| Per-user serialization precedent | `HostedProvisionLockClass` two-int advisory xact lock (`store/migrate.go`) | |
| Settings labels and CLI table | child C: `fixtures/model-settings/contract.json` `source_labels` keyed (field, token), `modelSettingSource` / `modelSettingSourceLabel`; "Model settings" table `SETTING HARNESS STORED DEFAULT RESOLVED SOURCE` fed by neutral `settingRow` | |
| Version ordering | `golang.org/x/mod/semver` (already in `api/go.mod`) | |

## Outcome

The codex effort cell gains `catalog_default {state, effort, model, runtime_version, observed_at}`. The Codex Runtime default option reads "Runtime default · catalog default: low (gpt-6.1-sol, Codex 0.162.1)", "Runtime default · catalog default unknown until a Codex run reports it", or, when versions disagree, "… · may differ on workers still on another version". A value counts only for the exact credential revision and account a new run would use.

### Acceptance examples

1. **Known.** A user with one linked Codex subscription runs a Codex issue run on `gpt-6.1-sol`. The claim carries `codex_catalog_context`; B's probe returns `defaultReasoningEffort: "low"`; the worker posts it with `runtime_version: "0.162.1"`. `GET /api/me/settings` returns the codex cell with `catalog_default: {state: "known", effort: "low", model: "gpt-6.1-sol", runtime_version: "0.162.1", observed_at: …}`, whatever effort that run sent.
2. **Rotation before the report arrives.** The user replaces the credential material under the same alias (material revision 4 → 5) after the claim was assembled at revision 4 and before the report lands. The route answers 409 `stale_context` and stores nothing; the label stays as before.
3. **Rotation after the report.** The report for revision 4 was stored; then the user replaces the material (revision 5). The predicted context is now revision 5, the stored row no longer matches, and the label reads unknown until a run on revision 5 reports. The old row is never relabelled.
4. **Mixed versions.** Fresh rows exist for one context and model from 0.162.1 (`low`) and 0.163.0 (`medium`). The cell shows `medium` from 0.163.0 with `state: "varies"`.
5. **No default level.** The catalog reports `"none"` for a model. The label reads "catalog default: none (no reasoning level requested)".

## Out of scope

- Claude runtime defaults (user decision 2026-10-21: "Runtime default · value not reported" stays).
- Per-run effective effort (child H); the Runtime default choice and the probe itself (child B).
- Settings setters (O), the Models tab (I), the resolver endpoint (F).
- Sharing values across users (user decision 2026-10-21: per user).

## Modules and seams

### 1. Catalog context (claim assembly; `api/internal/workersvc`)

`codexCatalogContext(userID, secretID, authMode, materialRevision, accountKey) string`: lowercase hex SHA-256 of `"uzi-codex-catalog-ctx-v1" 0x00 user_id 0x00 secret_id 0x00 auth_mode 0x00 decimal(material_revision) 0x00 account_key`, with `account_key` empty for `api_key`. Identifiers and counters only, never credential material; hashing gives a fixed opaque value, not secrecy.

- **At claim**: `assembleClaim` computes it from the run's frozen binding (`codex_secret_id`, `codex_auth_mode`, `codex_material_revision`, `codex_account_key`) and ships it as `ClaimConfig.codex_catalog_context` (omitempty) on Codex claims executed by the Codex issue-lane executor (not review, cross-check, judge, chat, job or advice claims). Because `codexCheckMaterialRev` admits a claim only when frozen equals current, this is the revision the claim actually releases. Omitted when the binding is incomplete (a subscription run with NULL `codex_account_key`).
- **Revision semantics**: `material_revision` covers rotation under the same secret id (replaced API key, re-login), and `account_key` covers a link to another account. The subscription refresh counter (`codex_provider_account.generation`) is deliberately excluded: a refresh rotates the access token of the same account, while the catalog is account metadata, so including it would discard nearly every report on a long-lived subscription (D3).
- **Predicted context** (for the read): the `resolveUsableCodexCredential` choice, its alias's current `material_revision`, and for a subscription the identity `freezeCodexBinding` would freeze now. No usable credential, or an unlinked subscription → no context → `unknown`.

### 2. Storage (migration 00354, draft)

```
CREATE TABLE codex_catalog_defaults (
  user_id         uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  context_id      text NOT NULL CONSTRAINT codex_catalog_defaults_context_check CHECK (context_id ~ '^[0-9a-f]{64}$'),
  model           text NOT NULL CONSTRAINT codex_catalog_defaults_model_check CHECK (octet_length(model) BETWEEN 1 AND 100),
  runtime_version text NOT NULL CONSTRAINT codex_catalog_defaults_version_check CHECK (octet_length(runtime_version) BETWEEN 1 AND 32),
  default_effort  text NOT NULL CONSTRAINT codex_catalog_defaults_effort_check
                  CHECK (default_effort IN ('none','minimal','low','medium','high','xhigh','max','ultra')),
  reported_at     timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT codex_catalog_defaults_pkey PRIMARY KEY (user_id, context_id, model, runtime_version)
);
CREATE INDEX codex_catalog_defaults_user_recent_idx ON codex_catalog_defaults (user_id, reported_at DESC);
```

New and empty, so inline named constraints validate instantly. The effort set is the Codex wire subset of shared design v4.1 item 4 (no `other`: anything else is never stored). Down drops the table (derived data, nothing to preserve).

**Retention bound (concurrency-safe):** at most 32 rows per user, none older than 30 days. `RecordCodexCatalogDefault` runs in one READ COMMITTED transaction: `pg_advisory_xact_lock(CodexCatalogLockClass, objid(user_id))` (new two-int class in `store/migrate.go`, distinct from `HostedProvisionLockClass` and `SecretMutationLockClass`, same objid derivation), then the upsert (`ON CONFLICT (user_id, context_id, model, runtime_version) DO UPDATE SET default_effort, reported_at = now()`), then one `DELETE … WHERE user_id = $1 AND (reported_at < now() - interval '30 days' OR (context_id, model, runtime_version) NOT IN (the 32 newest by reported_at))`. The lock serializes every writer for that user, so each prune sees all committed rows and the count can never exceed 32 after commit. Reads take no lock.

### 3. Worker report (agent)

- **Probe use**: child B already runs the probe, awaited, on the first root epoch of explicit-level claims. B2 also supplies it for Runtime default claims, non-blocking (B2 adds an `await: false` option to B's `catalogProbe`; the first turn does not wait). Only when the claim carries `codex_catalog_context`.
- **Report**: once per claim, on the first `onEntry` with an entry whose `defaultEffort` maps into the stored set (`none` stays `none`; anything else, including `persistent` and custom strings, → no report). Body `{claim_generation, catalog_context, model, runtime_version, default_effort}`: `catalog_context` echoed verbatim from the claim; `model` the requested root model sent on `thread/start` (≤ 100 bytes); `runtime_version` = `CODEX_PROBE_EXPECTATION.version`. `WorkerClient.recordCodexCatalogDefault(runId, body, timeoutMs)` beside `saveDecisionsMemo` in `agent/src/client.ts`; one attempt, no retry, every error (including 404 from an older API and 409) swallowed with one bounded log line; never affects the run.

### 4. Worker route (`api/internal/handler`)

`POST /api/worker/runs/{id}/codex-catalog-default`, mounted with `r.With(h.refuseJobRuns)` beside the decisions-memo routes; rows added to `jobRefusedRoutes` (`job_worker_routes_livedb_test.go`) and to `wantRouteMounts` as `{"POST", "/api/worker/runs/{id}/codex-catalog-default", noLimiter}` (bounded by one report per claim from an authenticated claim holder, as decisions-memo); not in `laneWorkerAllowlist`. Body via `httpx.DecodeJSONStrictBounded(r, &req, 1024)`.

| Status | When |
|---|---|
| 400 `claim_generation_required` | missing or < 1 |
| 400 `invalid_catalog_default` | unknown field, bad hex, model or version outside bounds, version not `semver.IsValid("v"+v)`, effort outside the set, run harness not `codex` |
| 404 (typed stale) | run not owned by this worker |
| 409 `{"disposition":"stale_claim"}` | generation not current or claim released |
| 409 `{"disposition":"stale_context"}` | `catalog_context` differs from the context derived from the run's frozen binding, or the alias's current `material_revision` / account link no longer matches it (rotated since the claim) |
| 204 | stored |

Service `(*Service).RecordCodexCatalogDefault(ctx, wkr, runID, req) error`: ownership and fence as `RecordRunUsage`; `run.UserID == wkr.UserID`; derive the frozen context from the run row and the current one from the alias; both must equal the echo, else `stale_context`; then §2's locked transaction. A delayed report is attributed only to the revision its claim released; the server never recomputes it against the latest revision and stores it there. Errors logged as values, never with worker-supplied fields.

### 5. Read (`api/internal/workersvc`)

`(*Service).CodexCatalogDefault(ctx, userID uuid.UUID, model string) (CatalogDefault, error)`: the seam F calls, taking the Codex model as an argument so F's schedule preview can ask for a schedule's pinned model. Returns `{State: known | unknown | varies, Effort, Model, RuntimeVersion, ObservedAt}`. Hides the predicted context (§1), the query (rows for `user_id`, `context_id`, `model`, `reported_at >= now() - 30 days`; at most 32 by §2) and the selection (pure function: highest `runtime_version` by semver, ties by latest `reported_at`; `varies` if any other version's effort differs; none → `unknown`).

`codexWorkerDefaultModel(lane pgtype.Text, leadCodexPin *string) string` (same package): lane, else (if child G is on the base) the lead template's Codex pin via `agenttmpl.PinForHarness`, else `agenttmpl.DefaultCodexModel`. The settings GET uses it for the user's own cell; F reuses it (cross-review 12).

### 6. Settings cell, web and CLI

- The codex `EffortSettingCellDTO` (child B) gains optional `catalog_default` = §5 for `codexWorkerDefaultModel(...)`; the Claude cell never carries it. Present whenever the user has a Codex credential (so the option label is right before they pick Runtime default); `state: "unknown"` otherwise. Mirror in `apitypes`, `web/src/lib/apiTypes.ts`; record the api-contract fixture pairs.
- Web: the Codex Runtime default option label in `EffortSelect` on Run defaults: "Runtime default · catalog default: {effort} ({model}, Codex {version})", `none` as "catalog default: none (no reasoning level requested)", "Runtime default · catalog default unknown until a Codex run reports it", and "· may differ on workers still on another version" for `varies`. Claude unchanged ("value not reported"). Mock (`web/src/mocks/mockApi/settings.ts`): static `catalog_default` per state; it does not reimplement §5.
- Labels: C's `fixtures/model-settings/contract.json` gains field `effort` rows for the catalog wording (token `catalog_default`: "catalog default"); rendered by `modelSettingSourceLabel` / `modelSettingSource`.
- CLI: the codex effort row of C's "Model settings" table shows RESOLVED `runtime default (catalog default: low, gpt-6.1-sol, Codex 0.162.1)` when the preference is runtime; server strings through `crossCheckPlain`. `api/cmd/uzi/demo.go` gets one sample per state.

## Testing decisions

- **Route and service (LiveDB, follows the decisions-memo and `worker_usage_livedb_test.go` tests):** owned, current generation, matching context → stored; missing generation, unknown field, bad hex, bad semver, over-long model, `persistent`, Claude run → 400 and nothing stored; not owned → typed 404; stale generation and released claim → 409 `stale_claim`; echo ≠ frozen context → 409 `stale_context`.
- **Rotation, both orders (required):** (a) claim assembled at revision 4, `BumpCodexMaterialRevision` to 5, then the report → 409 `stale_context`, no row; (b) report stored at revision 4, then bump to 5 → `CodexCatalogDefault` returns `unknown` and the row's `context_id` is unchanged. Same pair for a subscription re-link to another account (account key changes). A token refresh (generation bump only) keeps the label known.
- **Retention under concurrency (required):** 64 concurrent reports for distinct models from one user, each in its own connection → exactly 32 rows after all commit; a row older than 30 days is pruned on the next write; another user's rows untouched. Mutation check (watched red): with the advisory lock removed, a test hook between the upsert and the prune holds every transaction until all have upserted, and the count then exceeds 32; with the lock the hook is unreachable concurrently and the count is 32. No sleeps.
- **Claim field:** LiveDB: the hash for an API-key run and a linked subscription run equals the fixture value; omitted for an unlinked subscription, Claude runs and non-issue-lane Codex claims; claim goldens that pass through `assembleClaim` gain the key.
- **Selection (pure):** none; one; two versions (highest semver wins even when older reported later); tie; `varies`; 31-day-old row ignored; another context ignored; `none` known.
- **Read:** `CodexCatalogDefault` with an explicit model argument (a schedule-pinned model) returns that model's row; `codexWorkerDefaultModel` for lane, provider default and (if G is on the base) a lead Codex pin.
- **Worker:** fake transport: runtime claim posts once without delaying the first `turn/start`; explicit claim posts from B's awaited probe; no `codex_catalog_context` → no post; `defaultReasoningEffort: "persistent"` → no post; a 404 or 409 is swallowed and the run proceeds; method-recording fakes see `model/list` once.
- **Web/CLI:** `RunDefaults.test.tsx` per label state incl. `none` and `varies`; C's renderer test gains the codex runtime row with a hostile model string; `apiContract.test.ts` via fixtures; `fixtures/model-settings/contract.json` label case.
- No E2E literal changes expected (`git grep -n -i catalog -- e2e` at implementation time decides; the PR names unrun phases). CI time: no real waits (B's injected probe timeout; barrier-based concurrency test).

## Milestones

### M1: Codex catalog default, end to end

Blocked by: #3247, #3245.

Files: `api/internal/store/migrations/00354_codex_catalog_defaults.sql` (draft), `api/internal/store/queries/codex_catalog_defaults.sql` + `sqlc generate`, `api/internal/store/migrate.go` (`CodexCatalogLockClass`), `workersvc` (`codexCatalogContext`, `claim.go` field, `claim_assembly.go`, `RecordCodexCatalogDefault`, `CodexCatalogDefault`, `codexWorkerDefaultModel`), `handler/worker_protocol.go`, `handler/handler.go`, `handler/route_limiter_mounts_test.go`, `handler/job_worker_routes_livedb_test.go`, `handler/user_settings.go`, `apitypes/user_settings.go`, `fixtures/api-contract/{user_settings,settings_user_settings}.{zero,full}.json`, `fixtures/model-settings/contract.json`, `agent/src/protocol.ts`, `agent/src/client.ts`, `agent/src/codex/{codex-harness,codex-executor}.ts`, `agent/test/fake-api.ts` (route), `web/src/components/EffortSelect.tsx`, `web/src/pages/RunDefaults.tsx`, `web/src/lib/apiTypes.ts`, `web/src/mocks/mockApi/settings.ts`, `api/cmd/uzi/{settings,demo}.go`, `docs/worker-effort.md` (what "catalog default" means, the `none` ambiguity, per-credential scope), `docs/cli.md`, `specs/human.md`, one `changelog.d/<uuid>.md`.

Acceptance: acceptance examples 1 to 5 through the LiveDB, agent and web tests; both rotation orders; the retention test. Gates: `task docs:sync`, `task gate:api`, `task gate:web`, `task gate:agent`, `task gate:repo`.

## Risks

- **Rollout orders.** API first (new API, old workers): workers ignore `codex_catalog_context` and never post; the label reads unknown. Worker first (new worker, old API): the claim lacks the field, so nothing is posted. API rollback below B2: claims lose the field and workers stop posting; a report from a claim assembled before the rollback gets a 404, which the worker swallows; the table is inert until Down drops it.
- **Delayed and racing writes.** A report after a reclaim or release is `stale_claim`; a report after rotation is `stale_context` (example 2). Accepted race: a rotation that commits after the route's current-revision check and before its upsert stores a row under the released (now old) revision; that row can never match the new predicted context, so it is ineligible, never relabelled.
- **Mixed versions.** Highest semver wins, so an old worker cannot flip the label back during a roll; `varies` warns. After a worker image rollback, the newer version's value can stay the label for up to 30 days.
- **Catalog freshness.** A per-account catalog refresh can change the default without a version or credential change; the label is "as last reported" within 30 days and the docs say so.
- **Readers, writers, surfaces.** Writer: the worker route only. Readers: the settings GET and later F. Surfaces: Run defaults, mock, `uzi settings get`, demo. `context_id` appears in no read DTO and no log.
- **Second implementations.** None: the mock returns static states; the selection rule exists once in Go.
- **Schema.** One new additive table; no change to existing rows.

## Decision Log

- **D1. User decision 2026-10-21: Codex-only catalog defaults, no H dependency.** The value is runtime-and-account metadata read with B's probe, stored in its own small table, with its own route. Claude keeps "value not reported". Rejected: the r2 design on H's observation rows (per-run evidence questions, phase binding, a frozen-contract change on H).
- **D2. User decision 2026-10-21: per user.** Rows are keyed by `user_id`; no value crosses users.
- **D3. Context = (user, secret id, auth mode, material revision, account key), fixed at claim from the run's frozen binding and shipped on the claim; delayed reports are attributed to it or discarded. Excluding the subscription refresh `generation` is a coordinator ruling (2026-10-21, accepted).** Reason: the coordinator's rule that a report identifies the credential revision actually released; `codexCheckMaterialRev` makes the frozen revision the released one. Rotation under the same secret id bumps `material_revision`; a re-link changes the account key. The refresh counter is excluded because a refresh rotates the token of the same account and the catalog is account metadata. Rejected: recomputing against the latest revision (relabels old data); including the refresh generation (discards almost every report on long-lived subscriptions); `updated_at` (also moves on status writes that change no material).
- **D4. Retention: 32 rows and 30 days per user under a per-user advisory transaction lock.** Reason: keys grow with rotations, versions and models; the lock makes the bound hold under concurrent reports. Rejected: an unbounded table; prune without a lock (two concurrent writers each miss the other's row); locking the `users` row (contends with settings writes, including Q's combo check).
- **D5. Store the Codex wire subset; `none` kept and labelled for its ambiguity.** Reason: `ModelPreset` maps a missing default to `None`, so `none` can mean "no default level"; both meanings request no reasoning level beyond `none`, so one label covers both honestly. Rejected: dropping `none` (hides a real catalog value).
- **D6. Route follows the decisions-memo precedent** (`refuseJobRuns`, strict bounded decode, `claim_generation_required`), the same precedent H uses (cross-review nit 16).
- **D7. The read takes the model as an argument (F's owner note).** Reason: F's schedule preview needs the catalog default for a schedule-pinned Codex model, not only the lane model. `codexWorkerDefaultModel` supplies the user's own key and includes G's lead Codex pin when G is on the base (cross-review 12).
- **D8. Cell and labels.** `catalog_default` is an optional member of child B's `EffortSettingCellDTO`; labels and the CLI row use child C's table (coordinator ruling, round 3). Rejected: a second cell DTO or CLI layout.
- **D9. Mixed-version rule: highest fresh runtime version, `varies` on disagreement.** Rejected: latest report wins (an old worker flips the label back during a roll).
- **D10. #2073's runtime-default item moves from F to B2, narrowed to Codex.** The epic's F and B notes need an AI-synced update at filing.
