# PRD #3244: Record and show the resolved lead model and effort per claim attempt and phase on lead-agent runs

**Issue:** #3244
**Sibling issues (children of #1703):** A #3255, B #3247, B2 #3251, C #3245, D #3248, E #3257, F #3254, G #3249, H #3244, I #3263, J1 #3256, J2 #3260, K #3261, L #3262, M #3259, N #3258, O #3264, P #3250, Q-spike #3246, Q-claude #3252, Q-codex #3253.  
**Status:** Draft r3 2026-10-21 (child H of epic #1703). Every anchor below was re-read at origin/main `522aab1ba`. The commits since r1's pin `a50b0366c` touch agent test tooling (`agent/test/fake-api.ts` among them), CI, `schedsvc` ci-scout, one workersvc test and `specs/human.md`; no product anchor this PRD cites moved, and the migration head is still `00346_checkpoint_attested_source.sql`.
**Priority:** Normal.
**Parent:** #1703. **Blocks:** child #3250, child #3252, child #3253, child #3249 (all consume section 9). Child #3251 no longer depends on #3244 (user decision 2026-10-21: Codex-only catalog defaults in #3251's own table). **Blocked by:** none. **Extended later by:** child #3254 (its resolver extends `resolveLeadSettings` instead of extracting a second copy), child #3258 (template effort source).
**Related:** PRD #300 (schedule model freeze, `runs.model`), PRD #1551 (per-harness lanes), #2060 (per-harness effort), issue #2083 (claim-fenced worker write precedent), PRD #362 (summary persist refreshes the run page). Shared design v4.1 and the round-3 rulings (H owns lead precedence and provenance, the run-level lead label table and the one lead request matrix `request_cases`; settings surfaces use child C's label table; observed effort set in v4.1 item 4). Deliberate differences from shared design v3 are recorded as D-entries: D9 (`codex_thread_response`), D10 (observed effort set and Claude "not sent"), D17 (`fixed_at` derived, not stored), D18 (`reported_at` instead of `observed_at`).
**Delivery:** one PRD, one gated uzi run, one PR, three serial milestones. Default harness, `mr_rework=false`. The run needs Docker for LiveDB tests. No implementation or validation touches `.github/workflows/**`. Migration `00351` is a draft (main ends at `00346_checkpoint_attested_source.sql`); rename it to the next free number at landing with `task migration:renumber`.
**Mock:** `prds/mockups/1703-model-config/AfterStartSurfaces.dc.html`, section C "Run page header". The mock is the presentation reference. Small deviations for app consistency are fine; larger ones go in the PR under `Mock deviations`; this PRD's text wins where it specifies; mock parity is part of M1's and M2's acceptance. The mock's "every run" wording is narrowed by the user decision in D1.

**Scope gate:** one independently valuable outcome (a lead-agent run says which lead model and effort it ran with), one run, one PR. Large (one table, one worker route, one resolver extraction on each side, two harness adapters, one read overlay, web, CLI) but every piece is the same seam; splitting would ship a write path nobody reads or a header with nothing to show. No split proposed.

## Open questions for the user

None. The r1 question (which runs count) is answered: D1, user decision 2026-10-21.

## Problem

A user opening a lead-agent run cannot tell which model and reasoning effort its lead is using. The run page header shows a model only when a schedule froze one (`model fable`); a run that took its model from the user's defaults, the lead template or the built-in shows nothing, and effort is never shown. When a user changes their defaults between a run's first attempt and a resume, nothing records that the attempts ran differently. When the runtime uses something other than what uzi asked for (the Claude SDK can downgrade an unsupported effort; a resumed Codex thread keeps an effort set earlier), nobody can see it.

Children P (per-run override), Q-claude and Q-codex (plan on one pair, implement on another) and G (per-harness template pins) all need "what was asked for, by whom, and what did the runtime report". Without one seam each would invent its own reporting and its own copy of the lead precedence.

## Verified baseline (origin/main `522aab1ba`)

| Seam | Anchor (symbol, file) | Current behavior |
|---|---|---|
| Run header model | `RunView` header, `web/src/pages/RunView.tsx`, `{run.model && (<Badge ... title="Model this run was fired with (frozen by its schedule)">` | Shown only when `runs.model` is set, which only schedules do today. No effort anywhere. Pinned by the describe block titled with "the frozen per-schedule model badge (PRD #300)" in `web/src/pages/RunView.test.tsx`. |
| Run DTO | `RunDTO.Model`, `api/internal/apitypes/run.go` | Frozen schedule model only; nil means "inherited", with no hint of what was inherited. No claim-time model or effort. |
| CLI run detail | `renderRunDetail`, `api/cmd/uzi/run_render.go` | No MODEL, EFFORT or override-subagent row; both values only in `--json`. `docs/worker-model.md` "Per-schedule model" says the model and the fleet-wide flag are "shown ... by `uzi run get`", true only of `--json`. |
| TUI run detail | `detailHeaderLines`, `api/cmd/uzi/tui_detail.go` | Shows no model or effort. Treats a `state` frame's status as authoritative (`applyEvents`, "The authoritative status"). |
| Claim lead resolution | `assembleClaim`, `api/internal/workersvc/claim_assembly.go`, the block after `GetUserHarnessModelDefaults` | Inline: lane for the frozen harness; a compatible `run.Model` wins; an incompatible one records `recordHarnessModelFallbackNote` and keeps the lane; empty lane ships no `default_model`. Cross-check runs override through `checkerResolution`; Codex review runs ship no model. Profile-bound (isolated-lane) Claude runs also pass this block. No source is shipped. |
| Claim effort | `readUserDefaultEffort`, `api/internal/workersvc/claim_effort.go`; `resolveHarnessEffortPtr`; `ResolveDefaultEffort` / `ResolveDefaultCodexEffort`, `api/internal/agenttmpl/effort.go` | Per-harness user column, else `medium`; always non-nil on lead-lane claims. Whether it was the user's choice is not shipped. |
| Claim wire | `ClaimConfig.DefaultModel` / `DefaultEffort`, `api/internal/workersvc/claim.go`; `default_model` / `default_effort`, `agent/src/protocol.ts` | One model, one effort, no provenance, no phase. |
| Claim goldens | `claim_skills_wire.json` from the hand-built `sampleClaimPayloadWithSkills` (`claim_wire_contract_test.go`); `claim_ci_fix_wire.json` (`ciFixClaimPayload` in `ci_fix_test.go` runs the real claim path), `claim_ci_repair_wire.json` (`ci_repair_claim_wire_test.go`), `claim_ci_artifacts_wire.json` (`ci_artifacts_claim_wire_test.go`), all in `api/internal/workersvc/testdata/`; `agent/test/claim-skills-contract.test.ts`, `agent/test/agents.test.ts` | The skills golden does not see `assembleClaim` changes; the three ci goldens do. No golden exists for cross-check, review, judge, chat or job claims. |
| Claude lead model | `resolveLeadModel`, `agent/src/sdk-executor.ts` (`configModel \|\| templateModel \|\| undefined`); `assembleAgents`, `agent/src/agents.ts` (`if (t.model) result.leadModel = t.model`, first `LEAD_NAME_RE` match) | Claim model, else the lead template's model (`opus` by default), else nothing (SDK default). |
| Claude turn chokepoint | `driveTurn(ctx, turnConfig, phase: "plan" \| "implement", ...)`, `agent/src/sdk-executor.ts` | Every lead turn (plan, revise, implement) passes through it with a phase. |
| Claude init frame | `decode`, `agent/src/claude-harness.ts`, branch `type === "system" && rec["subtype"] === "init"` | Emits `initialized` with `model: asString(rec["model"])`; reads no effort. |
| Claude SDK evidence | pinned `@anthropic-ai/claude-agent-sdk` `0.3.293` (`agent/package.json`); tarball read via `npm pack` | `SDKSystemMessage` (subtype `init`): `model: string` (required) and `effort?: ('low'\|'medium'\|'high'\|'xhigh'\|'max') \| null`, documented as "the same value get_settings reports as applied.effort", `null` meaning "no effort parameter will be sent", and "Present on Remote Control bridge init frames ...; absent on hosts that do not publish it". `SDKControlGetSettingsRequest {subtype:'get_settings'}` is declared; the public `Query` interface declares no `getSettings()` and no response type; the runtime class in `sdk.mjs` / `core.mjs` implements `async getSettings(){return(await this.request({subtype:"get_settings"})).response}`. |
| Claude control-request precedent | `readLeadContext`, `agent/src/claude-harness.ts` | Feature-detects an optional method on the live query and races it against a timeout, `undefined` on any failure. |
| Codex lead model and effort | `buildRunRequest`, `agent/src/codex/codex-executor.ts` (`model: ctx.config?.default_model ?? this.opts.provider.model`, `effort = codexEffort(ctx)`); `CODEX_PRODUCTION_PROVIDER.model` = `gpt-6.1-sol` | Same pair for both phases; the lead template's model is never used for the Codex root today. |
| Codex turn chokepoint | `driveCodexTurn`, `agent/src/codex/codex-executor.ts` | Every lead turn builds its request through `buildRunRequest(ctx, phase, ...)` here. |
| Codex wire | `startThread` / `resumeThread` / `startTurnRpc`, `agent/src/codex/codex-harness.ts` | Sends `model` on `thread/start`, `thread/resume` and `turn/start`; `effort` on `turn/start` only when defined. Decodes only `thread.id` from the responses. `initialized` is emitted once per claim (`emitClaimInit` latch), so not after the approval epoch's `thread/resume`. `agent/src/codex/config.ts` sets no `model_reasoning_effort`. |
| Codex response semantics | pinned Codex `0.162.1` (`agent/codex/codex-package.lock`, commit `092d3acd6bec3e3a14bdc7e7a2810ab628ab759d`): `codex-rs/app-server-protocol/src/protocol/v2/thread.rs`; `codex-rs/app-server/src/request_processors/thread_processor.rs`; `codex-rs/core/src/session/session.rs` and `session/mod.rs` | `ThreadStartResponse.model` and `.reasoning_effort` are filled from `config_snapshot.model` / `config_snapshot.reasoning_effort` (thread_processor.rs); `ThreadResumeResponse` from `session_configured`. The snapshot's effort is the session's `collaboration_mode.reasoning_effort()`, built from `config.model_reasoning_effort` and, on resume, a persisted collaboration mode updated with it (session/mod.rs). So both fields describe the thread's resolved configuration, not the model catalog default and not an independent check of what each provider turn used. `TurnStartParams.effort` overrides "for this turn and subsequent turns". `ReasoningEffort` wire values: `none`, `minimal`, `low`, `medium`, `high`, `xhigh`, `max`, `ultra`, `persistent`, or a custom string (`codex-rs/protocol/src/openai_models.rs`). |
| Claim generation | `runs.claim_generation` (`00223_recovery_archive.sql`), incremented by every claim (`runtime.sql`) | One holder per generation; every resume or requeue is a new generation. |
| Claim-fenced worker write precedent | `WorkerSaveDecisionsMemo` (`api/internal/handler/worker_decision_memo.go`), `SaveDecisionsMemo` (`api/internal/workersvc/decision_memo.go`), `UpsertRunDecisionMemoFenced` (`api/internal/store/queries/decision_memos.sql`) | `runOwnedByWorker` for 404, then one statement locking the run `FOR NO KEY UPDATE` with worker, generation, unreleased-claim, status and kind predicates; 0 rows is 409 `claim_not_current`. Mounted with `h.refuseJobRuns` (`handler.go`); listed in `jobRefusedRoutes` (`job_worker_routes_livedb_test.go`) and `wantRouteMounts` (`route_limiter_mounts_test.go`, the complete route table, `noLimiter`). |
| Immutability precedent | `00305_recovery_inventory.sql` (`recovery_inventory_hold_guard BEFORE UPDATE`), `00273_run_fetch.sql` (`BEFORE UPDATE OF egress_profile_id`) | Append-only or immutable columns are enforced by triggers. |
| Live refresh | `Hub.PublishState` / `PublishHealth` / `PublishInput`, `api/internal/hub/hub.go` (handler holds `h.hub *hub.Hub`); `publishRunState`, `api/internal/workersvc/summaries.go`, publishes through `MultiBroadcaster` (hub plus Slack notifier); `applyFrame`, `web/src/lib/runStream.ts`; `knownRunEventTypes`, `api/internal/uzicli/stream.go` | Data-less frames make the web re-read the run. A `state` frame also reaches Slack through the broadcaster and is applied as authoritative status by the TUI. |
| GetRun-only overlay precedent | `GetRun`, `api/internal/handler/runs_lifecycle.go` (`overlayNowNote`, `HomeBytes` "SINGLE-RUN READ ONLY") | Best-effort, error logged, absent on list rows. |
| Agent fake API | `agent/test/fake-api.ts` (decisions-memo routes, "Issue #2083: the decisions-memo routes") | Unknown worker paths answer 404, so runner-level tests cannot see a new route until it is added. |
| DTO fixtures | `fixtures/api-contract/run.{zero,full}.json`, `web/src/lib/apiTypes.ts` (`.claude/rules/web.md` "DTO contract fixtures") | Re-recorded from the Go contract test. |
| Usage frames | ADR-0195 per-model usage fold | Carry models used, never the configured lead pair; this PRD does not read them. |

## Outcome

For every claimed lead-lane run, the API holds an append-only record, per claim attempt, phase and phase entry, of what the lead was configured to use (requested model and effort, each with a source) and what the runtime reported (observed model and effort, each with evidence, or an explicit unknown). The run page header, `uzi run get` and `uzi run get --json` show the latest record per phase of the latest attempt; `uzi run lead-config <run>` and its endpoint show the history. Before a claim the header says what is already fixed and what resolves at claim. Lead precedence (value plus source) has one home on each side: `workersvc.resolveLeadSettings` on the server and `resolveLeadRequest` in the worker. Today plan and implement record equal values; P, Q-claude, Q-codex and G extend those two functions and this record instead of adding their own.

### Acceptance examples

1. **Default case, Claude.** User's Claude lane is `opus`, Claude effort unset. An issue run is claimed. Before the first planning turn the API holds `(gen 1, plan, entry 1, seq 1)` with `requested_model=opus/user_default`, `requested_effort=medium/explicit/builtin`. Header: `Claude · lead: opus · medium`; tooltip: `opus: your Claude default · medium: built-in · runtime: claude-opus-... (Claude SDK init) · effort: not reported by the runtime` (or `high (Claude SDK settings)` if the probe answered, which renders the chip effort as `high (requested medium)`). After approval the API holds an implement row with the same requested side. `uzi run get` prints `LEAD  opus · medium` and `LEAD_SOURCE` with the same source text.
2. **Schedule pin, Codex.** A schedule pins `gpt-6-astra`; the user's Codex effort is `high`. Before claim: `Codex · lead: gpt-6-astra · effort resolves at claim` (tooltip `gpt-6-astra: fixed by schedule`). After claim: `lead: gpt-6-astra · high`, sources `fixed by schedule` and `your Codex effort`, runtime `gpt-6-astra (Codex thread config)`, effort `high (sent to Codex)`.
3. **Reclaim after a settings change.** A Claude run with no lane falls back to the lead template (`requested_model=opus/template`). It is requeued; the user sets their lane to `sonnet`; the reclaim (gen 2) records `sonnet/user_default`. The header shows gen 2; `uzi run lead-config <run>` lists gen 2 then gen 1. No value anywhere came from usage frames.

## Out of scope

- Runs without a lead agent (D1).
- Changing which model or effort a run uses. This PRD reports; B, P, Q-claude, Q-codex and G change behavior. Executors send exactly what they send today (the extraction in section 6 is behavior-preserving).
- The runtime-default effort choice (child B) and the Codex catalog-default label (child B2, its own table and route, independent of H). The schema represents `requested_effort_mode=runtime_default`; nothing here writes it. Settings-surface labels (Run defaults, Admin, `uzi settings get`) use child C's table, not H's.
- Subagent models and efforts. The header is labelled as the lead agent's configuration (existing `model on all agents` badge unchanged).
- The TUI (shows no model today, D16).
- Judge, summary, cross-check and settings-cell provenance and labels: those use child C's settings label table. H's table (section 8) labels run-level lead sources only; child N's template-effort source already has its row.

## Modules and seams

### 1. Storage: `run_lead_configs` (migration `00351`, draft; M1)

Additive new table. The trigger function's plpgsql body is wrapped in `-- +goose StatementBegin` / `-- +goose StatementEnd` (the `00305_recovery_inventory.sql` precedent); Goose Down drops the table and then the trigger function (`DROP FUNCTION run_lead_configs_append_only()`, as 00305's Down does for its guards).

| Column | Type and constraint | Meaning |
|---|---|---|
| `run_id` | `uuid NOT NULL REFERENCES runs(id) ON DELETE CASCADE` | |
| `claim_generation` | `bigint NOT NULL CHECK (>= 1)` | the attempt |
| `phase` | `text NOT NULL CHECK IN ('plan','implement')` | |
| `phase_entry` | `smallint NOT NULL CHECK (BETWEEN 1 AND 8)` | ordinal of this phase's entry within the attempt (a later re-plan is entry 2) |
| `seq` | `smallint NOT NULL CHECK (BETWEEN 1 AND 32)` | worker-assigned, per (attempt, phase) |
| `harness` | `text NOT NULL CHECK IN ('claude','codex')` | copied server-side from `runs.harness` |
| `requested_model` | `text NULL`, `octet_length` 1..100 | NULL only for the Claude SDK default |
| `requested_model_source` | `text NOT NULL CHECK IN ('run_override','schedule_pin','user_planning_default','user_default','template','builtin')` | |
| `observed_model` | `text NULL`, `octet_length` 1..100 | |
| `observed_model_evidence` | `text NULL CHECK IN ('claude_init','codex_thread_response','unknown')` | NULL = not yet observed |
| `requested_effort` | `text NULL CHECK IN ('low','medium','high','xhigh','max')` | |
| `requested_effort_mode` | `text NOT NULL CHECK IN ('explicit','runtime_default')` | explicit = an effort is sent on the wire |
| `requested_effort_source` | `text NOT NULL CHECK IN ('run_override','user_planning_default','user_default','template','builtin')` | `template` is reserved for child N |
| `observed_effort` | `text NULL CHECK IN ('none','minimal','low','medium','high','xhigh','max','ultra','other')` | v4.1 item 4 set; `none` is the Codex level |
| `observed_effort_evidence` | `text NULL CHECK IN ('claude_init','claude_get_settings','codex_sent','codex_runtime_default','unknown')` | NULL = not yet observed |
| `truncated` | `boolean NOT NULL DEFAULT false` | the reporter's budget for this (attempt, phase) is spent; later changes were not recorded |
| `reported_at` | `timestamptz NOT NULL DEFAULT now()` | server receipt time (D18) |

Primary key `(run_id, claim_generation, phase, seq)`. Every CHECK, column-level ones included, is declared as a named table constraint so a later migration can cite it and the handler can map a 23514 by name (dba 3). Column CHECKs are named `run_lead_configs_<column>_check` (for example `run_lead_configs_observed_effort_evidence_check`, `run_lead_configs_phase_entry_check`). Table checks:

- `run_lead_configs_effort_mode_check`: `(requested_effort_mode = 'explicit') = (requested_effort IS NOT NULL)`.
- `run_lead_configs_requested_model_check`: `requested_model IS NOT NULL OR (requested_model_source = 'builtin' AND harness = 'claude')` (a Codex root always sends a model).
- `run_lead_configs_observed_model_pairing_check`: `(observed_model IS NULL) = (observed_model_evidence IS NULL OR observed_model_evidence = 'unknown')`.
- `run_lead_configs_observed_effort_pairing_check`: `observed_effort IS NOT NULL` implies evidence is neither NULL nor `unknown`; `observed_effort IS NULL` with evidence `claude_init` or `claude_get_settings` means the runtime reported that it sends no effort parameter (D10); `observed_effort IS NULL` with evidence `codex_sent`, `codex_runtime_default` is forbidden (Codex null maps to `unknown`).
- `run_lead_configs_truncated_check`: `NOT truncated OR seq = 32`.

Append-only, enforced three ways: a `BEFORE UPDATE ON run_lead_configs` trigger (`run_lead_configs_append_only`) raising an exception; a Go test that scans `api/internal/store/queries/*.sql` and the hand-written `api/internal/store/*.go` files (excluding generated `*.sql.go`) case-insensitively for `\bUPDATE\s+(public\.)?run_lead_configs\b` and `\bDELETE\s+FROM\s+(public\.)?run_lead_configs\b`, after first asserting both patterns match an in-test canary string; deletion only through the `runs` FK cascade (tests use `TRUNCATE ... CASCADE`; a `BEFORE DELETE` guard is not added, since the cascade is the only intended delete path). Rows per run are bounded by generations x 2 x 32.

### 2. Vocabulary, lane predicate and labels: `api/internal/leadcfg` (new package; M1)

Caller-facing interface (dependency-free, no store import):

- Exported closed sets for phases, model sources, effort sources, effort modes, model evidence, effort evidence and observed efforts.
- `FixedAt(source) string`: `create` for `run_override` and `schedule_pin`, `claim` otherwise (D17).
- `LeadLane(kind string, reviewTarget bool, egressProfileBound bool) bool`: true exactly for `issue`, `ci_fix`, `prompt`, `self_improve`, `mr_rework`, `task` with no review target and no egress profile. The one predicate used by claim provenance (section 3), the GetRun overlay (section 5) and mirrored in the SQL fence (section 4).
- `ValidateReport(apitypes.LeadConfigReportRequest) (Report, error)`: every bound (closed enums; `1 <= phase_entry <= 8`; `1 <= seq <= 32`; `claim_generation >= 1`; model strings non-empty and passing `agenttmpl.ValidateModel`; the section 1 table checks except the harness one, which needs `runs.harness` and stays in the DB). Errors are `*leadcfg.FieldError{Field, Reason}`, answered as 400 `invalid_lead_config` naming the field, never echoing the value.
- `SourceLabel(setting, token, harness) string` and `EvidenceLabel(token) string`: the one Go rendering of the run-level lead label table (section 8), used by the run header, `uzi run get` and `uzi run lead-config` only. Settings surfaces (Run defaults, Admin, `uzi settings get`, F's cells) use child C's `fixtures/model-settings/contract.json` through `modelSettingSource` / `modelSettingSourceLabel`, keyed by (field, token); F adds no rows to H's table.

All of it is pinned to `fixtures/lead-config/contract.json` (keys `vocabulary`, `fixed_at`, `labels`, `display_cases`) by Go unit tests, a LiveDB test that inserts every allowed value and one disallowed value per column (SQLSTATE 23514), and a table test over `fixtures/run-kinds/registry.json` crossed with review target and egress binding asserting that claim keys, overlay presence and fence outcome agree with `LeadLane`. The agent and web unions are pinned to the same file.

### 3. Server lead resolver and claim provenance (M1)

Extract the inline lead block of `assembleClaim` into a pure function, behavior-preserving, in `api/internal/workersvc/lead_settings.go`:

```go
type LeadSettingsInput struct {
    Harness    Harness
    RunModel   pgtype.Text // runs.model, frozen at create
    LaneModel  pgtype.Text // the user's lane for Harness
    UserEffort pgtype.Text // the user's per-harness effort column
}
type LeadSettings struct {
    Model           pgtype.Text           // invalid = ship no default_model (worker decides)
    ModelSource     leadcfg.ModelSource   // "" exactly when Model is invalid
    Effort          *string               // as resolveHarnessEffortPtr returns today
    EffortSource    leadcfg.EffortSource  // user_default iff the value came from UserEffort, else builtin
    DroppedRunModel pgtype.Text           // an incompatible RunModel, for the existing fallback note
}
func resolveLeadSettings(in LeadSettingsInput) LeadSettings
```

`assembleClaim` reads the inputs, calls it, records `recordHarnessModelFallbackNote` when `DroppedRunModel` is valid, and sets `default_model`, `default_effort` and, only when `leadcfg.LeadLane(...)` holds, the two new omitempty `ClaimConfig` fields `default_model_source` and `default_effort_source`. The cross-check `checkerResolution` path, review runs and the custom-Codex capability re-check stay where they are. Effort values are byte-identical to today. Effort source handover with child B (both in wave 1, either may land second): if B is on the base when H lands, `EffortSource` is taken from B's `EffortResolution.Source` (`agenttmpl.ResolveHarnessEffort(...)`), and `requested_effort_mode` follows B's `runtime_default` outcome; otherwise H uses today's "non-blank column" rule and B, landing second, wires `EffortResolution.Source` into `resolveLeadSettings`. Child B states the same line.

`default_model_source` describes only `default_model` (schedule pin or lane, later P's override). The lead template's model is not a server source: it reaches the worker as the lead template's delivered `ClaimAgent.Model` and becomes `template` in `resolveLeadRequest` (section 6). After child G that delivered `model` is per-harness (`agentsFromTemplates` projects it with G's `agenttmpl.PinForHarness`), so the worker's `templateModel` is already the pin for the run's harness; H's claim fields need no change for G.

`default_effort_source` is always present on a lead-lane claim, which makes it the worker's activation signal. Wire pins: add both keys with non-default values to `sampleClaimPayloadWithSkills` and regenerate `claim_skills_wire.json`; regenerate `claim_ci_fix_wire.json`, `claim_ci_repair_wire.json` and `claim_ci_artifacts_wire.json` (real claim path, both keys appear); keep `agent/test/claim-skills-contract.test.ts` and `agent/test/agents.test.ts` green. Explicit absence assertions for cross-check, Claude review, Codex review and isolated-lane Claude claims.

### 4. Worker write: `POST /api/worker/runs/{id}/lead-config` (M1)

Mounted beside the decisions-memo routes with `h.refuseJobRuns`; added to `jobRefusedRoutes` and to `wantRouteMounts` as `{"POST", "/api/worker/runs/{id}/lead-config", noLimiter}`, and the mount-adder updates the prose route count above `wantRouteMounts` (the "THIS SENTENCE IS DOWNSTREAM OF ANY NEW ROUTE" paragraph in `route_limiter_mounts_test.go`). No limiter, as for decisions-memo: rows are bounded at 64 per attempt by `seq`, but requests are not, and each request holds the run row lock briefly, so the bound relies on the worker being the authenticated claim holder (a misbehaving worker can already load the API through its other routes). This follows the decisions-memo precedent rather than `WorkerRunUsage` because it is a claim-fenced, strictly decoded, job-refused write. Body: `httpx.DecodeJSONStrictBounded(r, &req, 4096)`.

Request fields: `claim_generation`, `phase`, `phase_entry`, `seq`, `truncated`, `requested_model`, `requested_model_source`, `requested_effort`, `requested_effort_mode`, `requested_effort_source`, `observed_model`, `observed_model_evidence`, `observed_effort`, `observed_effort_evidence` (observed four nullable). The request schema is closed: a later field is added to `apitypes.LeadConfigReportRequest`, `leadcfg.ValidateReport` and this table together; no consumer of this PRD needs one today.

| Status | When |
|---|---|
| 400 `claim_generation_required` | missing or < 1 |
| 400 `invalid_lead_config` | any `leadcfg.ValidateReport` failure, oversized or malformed body, unknown field; also a 23514 from the insert, matched by constraint name (the harness-dependent `run_lead_configs_requested_model_check` is only evaluated in the DB), reported as field `requested_model` |
| 401 | no worker |
| 404 | `runOwnedByWorker` refuses (incl. lane mismatch) |
| 409 `claim_not_current` | the fence admits nothing, whether or not a row with that key exists |
| 409 `lead_config_key_conflict` | fence admits, key exists, stored body differs |
| 500 | the invariant breach below (fenced, not inserted, no row): logged, never expected |
| 204 | inserted, or fence admits and the identical row already exists |

**Transaction (dba review, reproduced on PostgreSQL 17).** A single statement cannot decide the duplicate: an identical retry that waited on the run-row lock sees the committed row through the unique index (`inserted=false`) but not through its own start snapshot, so a same-statement comparison answers a false 409. The write is therefore two statements in one explicit READ COMMITTED transaction, as a hand-written store method following `(q *Queries) RecordLiveCheckpointPublishAttempt` (`api/internal/store/checkpoint_publish_admission.go`, which opens `pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})`): new `(q *Queries) RecordRunLeadConfig(ctx, RecordRunLeadConfigParams) (RecordRunLeadConfigResult, error)` in `api/internal/store/lead_config_record.go`, running two sqlc queries from new `api/internal/store/queries/lead_configs.sql`:

1. `InsertRunLeadConfigFenced :one`. CTE `r` selects the run `FOR NO KEY UPDATE` where `id`, `worker_id`, `claim_generation`, `claim_released_at IS NULL`, `status IN ('claimed','running','awaiting_approval','awaiting_input','awaiting_followup')`, the `LeadLane` mirror (`kind IN ('issue','ci_fix','prompt','self_improve','mr_rework','task')`, `review_target_run_id IS NULL`, `egress_profile_id IS NULL`); CTE `ins` inserts selecting `r.harness`, `ON CONFLICT (run_id, claim_generation, phase, seq) DO NOTHING RETURNING 1` (the target is named so a later unique index cannot be absorbed as a duplicate); the outer select returns `EXISTS (SELECT 1 FROM r)::boolean AS fenced, EXISTS (SELECT 1 FROM ins)::boolean AS inserted`.
2. `RunLeadConfigSameBody :one`, run only when `fenced && !inserted`, as a second statement of the same transaction: a fresh snapshot, with the run row still locked by statement 1, so the fence still holds. It returns one `::boolean` `same_body` comparing the stored row with the request: `phase_entry`, `truncated`, `requested_model_source`, `requested_effort_mode`, `requested_effort_source` with `=`, and `requested_model`, `requested_effort`, `observed_model`, `observed_model_evidence`, `observed_effort`, `observed_effort_evidence` with `IS NOT DISTINCT FROM` (each with an explicit cast for sqlc). `reported_at` (server time) and `harness` (copied from the run) are excluded.

Outcomes: not `fenced`: roll back, 409 `claim_not_current`. `inserted`: commit, 204, then publish. Otherwise `same_body` true: commit, 204, no publish; false: commit, 409 `lead_config_key_conflict` (first row kept); `ErrNoRows` from statement 2: roll back, 500 and log (cannot happen: the run lock serializes writers and the conflict was against a committed row). The run-row lock is held from statement 1 through the comparison and the commit. The transaction must stay READ COMMITTED: REPEATABLE READ fails the waiting retry with SQLSTATE 40001 and fixes the snapshot at statement 1. Rejected alternatives: re-running the single statement once (hides a retry loop, evaluates the fence twice); `ON CONFLICT DO UPDATE ... WHERE` (fires the append-only trigger and writes a dead tuple for a no-op).

Service `workersvc.(*Service).RecordLeadConfig(ctx, wkr, runID, leadcfg.Report) (RecordResult, error)`, `RecordResult{Inserted bool}`: `runOwnedByWorker` (404), then `s.q.RecordRunLeadConfig`, mapping `ErrLeadConfigClaimNotCurrent`, `ErrLeadConfigKeyConflict` and the named-constraint 23514. Nothing in the body is logged.

**Refresh after commit only.** When `RecordRunLeadConfig` returned `Inserted` (so the transaction committed), the handler calls a new data-less hub frame, `h.hub.PublishRefresh(runID)` (`Event{Type: "refresh"}`): "a GetRun-derived field changed; re-read the run". A rolled-back, refused, conflicting or failed write publishes nothing. It is published to the hub only, never through `MultiBroadcaster`, so Slack sees nothing, and it carries no status, so it cannot overwrite a newer status in the TUI (D12). `PublishRefresh` is nil-safe (a nil `*Hub` is a no-op), since handler unit tests build a `Handler` without a hub. Consumers: `applyFrame` (`web/src/lib/runStream.ts`) re-reads the run on it (no replay), and because `useRunStream` refetches inputs whenever `effects.refreshRun` is set, each refresh also re-reads `/inputs` (cheap, stated); the web frame union in `web/src/lib/apiTypes.ts` (`type: "message" | "state" | "health" | "input"`) gains `"refresh"`; `uzicli.knownRunEventTypes` gains it and the TUI ignores it (older CLIs normalize it to `RunEventTypeUnknown`). The `broadcast` comment in `hub.go` ("only one of the four self-heals") is rewritten for five types: a dropped `refresh` does not self-heal; the next run re-read (state frame, reconnect, or any later refresh) picks the row up.

### 5. Read side (M1 overlay, M3 history)

`RunDTO.LeadConfig *RunLeadConfigDTO` (`json:"lead_config,omitempty"`), set by `GetRun` only (best-effort overlay, error logged, absent on list rows), present iff `leadcfg.LeadLane` holds for the run:

- `applicable` (always true when present), `claim_generation` (0 when no row), `current` (generation equals `runs.claim_generation`).
- `phases`: 0 to 2 `LeadPhaseConfigDTO`, the highest-`seq` row per phase within the highest generation that has rows, ordered plan, implement. Each carries `phase`, `phase_entry`, `seq`, `harness`, the eleven requested/observed fields, derived `requested_model_fixed_at` / `requested_effort_fixed_at` (`leadcfg.FixedAt`), `truncated`, `reported_at`.

Query `ListLatestRunLeadConfig(run_id)`: `DISTINCT ON (phase) ... ORDER BY phase, seq DESC` within `max(claim_generation)`, PK-indexed, wrapped in an outer `ORDER BY CASE phase WHEN 'plan' THEN 0 ELSE 1 END` because a plain `ORDER BY phase` puts `implement` before `plan` (dba 4). Known limit (v3 §3, stated for Q-claude): a reclaim that resumes already approved records only implement rows in the new generation, so the header then shows no plan row; the plan row of the earlier attempt stays in history.

M3 adds `GET /api/runs/{id}/lead-config`, mounted in the `RequireUser` runs group so a `uzc_` CLI token works, authorized by `GetRunForViewer` (404 otherwise); `wantRouteMounts` row `{"GET", "/api/runs/{id}/lead-config", noLimiter}` (as `GET /api/runs/{id}/inputs`), the prose route count above `wantRouteMounts` updated again, and a `cli_auth_livedb_test.go` bearer case beside the `/review` ones. Response `{entries: [...], truncated: bool}`, each entry a `LeadPhaseConfigDTO` plus `claim_generation`, ordered `claim_generation DESC, CASE phase WHEN 'plan' THEN 0 ELSE 1 END, seq`; capped at 256 rows.

### 6. Worker resolver and reporter: `agent/src/lead-config-report.ts` (new; M1, observation in M2)

`resolveLeadRequest(input: LeadRequestInput): LeadRequest` (pure), the worker's single home for lead precedence:

```ts
interface LeadRequestInput {
  harness: "claude" | "codex";
  phase: "plan" | "implement";      // ignored in H; Q-claude selects planning values by it
  config: ClaimConfig | undefined;
  templateModel?: string;           // the lead template's delivered model; H: Claude passes assembleAgents' leadModel, Codex passes nothing (G supplies selectLeadTemplate(ctx.agents)?.model for both)
  providerDefaultModel?: string;    // Codex: CODEX_PRODUCTION_PROVIDER.model
}
interface LeadRequest {
  model?: string;
  modelSource: LeadModelSource;
  effort?: HarnessEffort;
  effortMode: "explicit" | "runtime_default";
  effortSource: LeadEffortSource;
}
```

Claude: model = `config.default_model || templateModel || undefined` (exactly `resolveLeadModel`); source = the claim's `default_model_source` when the claim model won, `template` when the template model won, `builtin` otherwise. Codex: model = `config.default_model ?? providerDefaultModel`, exactly today's expression; `templateModel` is accepted but unused on Codex in H. Source = the claim's `default_model_source` when the claim model won, else `builtin`. Child G extends this branch to `config.default_model ?? templateModel ?? providerDefaultModel` with source `template` when the template wins (G owns the lead-template selection rule, `selectLeadTemplate`, the first `lead|orchestrator` template in delivered order; the role-model filter is child C's `isCodexRoleModel`, applied by G). The render-time `modelSource: "worker_default"` provenance stays tied to a present `default_model`, unchanged. Effort: `config.default_effort` gives `explicit` with the claim's `default_effort_source`; absent gives `runtime_default`. `resolveLeadModel` and the Codex `buildRunRequest` model and effort expressions call it, so what is reported is by construction what is sent.

One request matrix, in the fixture from M1: `fixtures/lead-config/contract.json` key `request_cases`, each case `{id, harness, phase, config, templateModel?, providerDefaultModel?, expect: LeadRequest}`. `agent/test/lead-config-report.test.ts` is its reader: it loads the cases into `LEAD_REQUEST_CASES` and one loop asserts both `resolveLeadRequest(case)` and the value the executor actually sends (through `resolveLeadModel` and the Codex `buildRunRequest` seam) equal `expect`, with every id asserted visited. H's cases cover claim model present (each source), Claude template fallback, Claude SDK default, Codex provider default, effort present and absent. G appends its cases ("Codex lead pin, no `default_model`" giving `template`, "Codex lead with a Claude alias" giving `builtin`, "`default_model` present", "`orchestrator`-only roster" on both harnesses, "apply model to agents on"); Q-claude appends plan-phase cases; P appends `run_override` cases; F later adds a Go reader of the same key for its resolver. No consumer adds a second matrix, and no case lives only in a test file. Review runs never reach `resolveLeadRequest` (they run in the ReviewRunner), so they are not matrix cases; F tests them on its own seam.

`class LeadConfigReporter`, exactly one per claim generation: constructed by the runner in the `const ctx: RunContext = {` literal of `agent/src/runner.ts` that sets `claimGeneration: claim.claim_generation`, carried as `RunContext.leadConfig`, and never constructed by an executor, harness or epoch (review 7). Interface:

- `enterPhase(phase, request: LeadRequest): Promise<PhaseEntry>`, where `PhaseEntry = {phase, entry: number}`. A new entry (and one requested-side row, observed side NULL) only when `phase` differs from the previous call's phase in this claim, or on the first call; repeated calls in the same phase return the current entry. Awaited by the executor before the turn starts, bounded to 3 s through an injectable timer; on timeout the post continues in the background.
- `observe(entry: PhaseEntry, obs: RuntimeConfigObservation): void`. The executor captures the `PhaseEntry` when it starts the turn and closes the adapter callback over it (buddy H2). The observation is posted (a full-snapshot row repeating the requested side) only if `entry` is still the latest entry of its phase and the observed side differs from the last row posted for that entry. A late plan observation after the implement phase began still attaches to the plan phase if no newer plan entry exists; one from a superseded plan entry is dropped and logged.
- Budget per (attempt, phase): `seq` 1..32, at most 3 observation rows per entry, at most 8 entries. Further observations of one entry beyond its 3 are dropped and logged; the entry's last posted row stays its latest, and later entries of the phase still get their rows (review G: one chatty entry must not hide Q-claude's later re-plan). Only when the next row would need `seq` 32, or a 9th entry begins, does the reporter post one final row at `seq` 32 with `truncated: true` carrying the latest known values, then stop for that phase (buddy H3). The display shows truncation (section 8 rule 7).
- Fire-and-forget delivery: at most one request in flight per reporter, a pending queue of at most 4 (oldest pending dropped, logged), one retry on a transport error. Never throws.
- Activation and disablement: inactive unless the claim carries `default_effort_source`. Disabled for the rest of the claim on 400, 404 or `claim_not_current`; `lead_config_key_conflict` is logged at error level (a worker bug) and disables too.
- `WorkerClient.recordLeadConfig(runId, body, timeoutMs)` in `agent/src/client.ts` beside `saveDecisionsMemo`; `agent/test/fake-api.ts` gains a recording route beside the decisions-memo routes.
- Call sites: Claude `driveTurn` and Codex `driveCodexTurn` only. Both build one `LeadRequest` per phase from `resolveLeadRequest` at claim start (equal in H).

### 7. Runtime observation, the single probe (M2)

Neutral contract: `RunTurnRequest` (`agent/src/harness.ts`) gains `onRuntimeConfig?: (obs: RuntimeConfigObservation) => void`, with `RuntimeConfigObservation = {model?: string; modelEvidence: "claude_init" | "codex_thread_response" | "unknown"; effort?: ObservedEffort | null; effortEvidence: "claude_init" | "claude_get_settings" | "codex_sent" | "codex_runtime_default" | "unknown"}`. No new `HarnessEvent` kind; reducer and projection untouched. Adapters call it from their own paths and never let it throw into the turn. This is the one Claude settings probe in the codebase. Child B2 reads Codex `model/list` through child B's catalog probe into its own table and does not consume these observations.

- **Claude** (`claude-harness.ts`): on the first init frame of a turn whose request carries the callback, model = `rec.model` (`claude_init`). If the frame has an `effort` key: a known level gives that level with `claude_init`; `null` gives "not sent" (`effort: null`, `claude_init`). If the key is absent, a probe modelled on `readLeadContext` feature-detects `getSettings` on the live query, races it against 2000 ms (injectable), reads only `response.applied.effort` (same mapping, `claude_get_settings`); absence, timeout or shape mismatch gives `unknown`. At most one probe per turn; the executor passes the callback only on the first turn of a phase entry, so the probe runs at most once per phase entry. It never blocks the event stream and never logs the response.
- **Codex** (`codex-harness.ts`): `startThread` / `resumeThread` also decode `model` and `reasoningEffort` from the response (bounded strings; the rest ignored). Model = response `model`, evidence `codex_thread_response` = the thread's resolved `config_snapshot` model, which equals the sent model unless Codex rewrote it; it is not independent evidence of what each provider turn used (D9). Effort: if `turn/start` carries `effort`, that value with `codex_sent`; otherwise the response `reasoningEffort` with `codex_runtime_default` (the thread's configured effort, including a persisted effort on a resumed thread; null gives `unknown`). Because approval and checkpoint epochs `thread/resume`, the implement phase gets its own observation without the claim-init event. Every root `thread/start` and `thread/resume` invokes the callback, including a checkpoint epoch inside the same implement entry; the reporter's per-entry dedupe makes an unchanged observation post nothing, and a changed one (for example a resumed thread whose configured effort differs) post one row within the entry's budget. Claude, by contrast, gets the callback only on the first turn of a phase entry. The executor-sequence tests pin both choices (review H).
- **Mapping to the stored set:** known levels map to themselves; Codex `persistent` and any custom string map to `other`; Claude `null` maps to stored `observed_effort` NULL with real evidence ("not sent", D10); model tokens failing `agenttmpl.ValidateModel` rules become `unknown`.

### 8. Presentation and the run-level lead label table (M1, extended in M2)

One display rule, two implementations pinned together: `web/src/lib/leadConfig.ts` `leadConfigView(input)` and Go `runpresentation.LeadConfigView(input)`. Both take a neutral `LeadConfigViewInput {status, harness, preClaim: {model?: string, modelSource: "schedule_pin"}, leadConfig?: RunLeadConfigDTO}`, built from the run by one adapter per implementation (`leadConfigViewInput(run)` / `runpresentation.LeadConfigViewInputFromRun(run)`). `runs.model_source` and `runs.effort` do not exist when H lands (child P adds them), so H's adapter sets `preClaim.model = run.model` with `modelSource = "schedule_pin"`, the only writer today, and H's view renders the pre-claim model label from the label table by `modelSource`, never from a hard-coded string. `display_cases` is an array of `{id, input, expect: {chip, lines}}` that both test files iterate in full, each id asserted visited, so a consumer extends display behavior by widening the input type, branching in the adapter and appending cases, without touching the runners.

Run-level lead labels (`labels` key; Go `leadcfg.SourceLabel` / `EvidenceLabel`, TS twin in `leadConfig.ts`; `{H}` is Claude or Codex). Used by the run header, `uzi run get` and `uzi run lead-config` only; settings surfaces use child C's (field, token) table, so the same token may read differently there (for example `user_default` is "your choice" in C's cells):

| Token | Model label | Effort label |
|---|---|---|
| `run_override` | this run | this run |
| `schedule_pin` | fixed by schedule | (none: schedules pin no effort) |
| `user_planning_default` | your {H} planning default | your {H} planning effort |
| `user_default` | your {H} default | your {H} effort |
| `template` | lead template | agent template (child N) |
| `builtin` | built-in (Claude with no model sent: Claude SDK default) | built-in |
| mode `runtime_default` | | runtime default |

| Evidence | Label |
|---|---|
| `claude_init` | Claude SDK init |
| `claude_get_settings` | Claude SDK settings |
| `codex_thread_response` | Codex thread config |
| `codex_sent` | sent to Codex |
| `codex_runtime_default` | Codex thread config |
| `unknown` | not reported by the runtime |
| NULL (not yet observed) | not reported yet |
| effort NULL with Claude evidence | not sent |
| observed effort `other` | an unrecognized level |

Rules (each a fixture case, plus the M2 cases noted):

1. No `lead_config` (not a lead-lane run): no chip, header unchanged.
2. Pre-claim (`status = queued`), today's rule: `model and effort resolve at claim` when `run.model` is unset; `lead: gpt-6-astra · effort resolves at claim` with `gpt-6-astra: fixed by schedule` when it is set (the label comes from `modelSource = schedule_pin` through the table). A requeued run adds "previous attempt: ..." from its rows. Child P extends this rule (section 9): its adapter branches on `run.model_source` so "fixed by schedule" appears only for `schedule_pin`, it adds the pre-claim effort and "model resolves at claim" forms, and it appends DC-P1 to DC-P7 (including DC-P6, a run held before any report exists) to `display_cases`.
3. Claimed, lead-lane, no rows: `lead: not recorded`, tooltip "the worker did not record it (an older worker, or the run ended before its first lead turn)", plus the rule 2 create-time values when present, so PRD #300's "visible on every status" holds.
4. Rows present: `lead: <model> · <effort>`. Model text = `requested_model`, else `observed_model` when the requested side is `builtin` with no model, else `default`. Effort text: `runtime default: <observed>` when the mode is `runtime_default` and a concrete observed level exists, `runtime default` otherwise; for `explicit`, the observed level rendered `high (requested xhigh)` when its evidence is a runtime report (`claude_init`, `claude_get_settings`, `codex_runtime_default`) and it differs, else the requested level. `other` is never rendered as a level.
5. Tooltip and `LEAD_SOURCE`: `<value>: <source label>` for model and effort, the runtime lines (`runtime: <observed model> (<evidence label>)`, `effort: <observed> (<evidence label>)` or the NULL/unknown/not-sent label), "recorded by an earlier attempt; this attempt has not recorded yet" when `current` is false, and "This is the lead agent; subagents with their own pin may differ."
6. Both phases present and differing in any displayed value: `lead: plan <m> · <e> → implement <m> · <e>`. Unreachable in H; Q-claude gets it subject to the section 5 known limit.
7. `truncated` on a shown row: suffix "(later changes not recorded)" in the tooltip and `LEAD_SOURCE`.

Requested-side cases (rules 1 to 7 with NULL observed sides) land in M1; observed, mismatch, not-sent, `other`, unknown and runtime-default-with-observation cases land in M2.

Web: `RunView` replaces the PRD #300 badge with the chip beside `<HarnessBadge>`, a focusable span (`tabIndex={0}`, `title` = tooltip, explicit `aria-label` = chip text plus sources), the `RunBudgetElapsed` pattern; model strings pass `stripUnsafeChars`; mock data (`web/src/mocks/data/runs.ts`) gains rule 2 to 4 cases. CLI: `uzi run get` adds `LEAD` and `LEAD_SOURCE` rows through `sanitizeTTY`, emit-only when `lead_config` is present; `--json` carries `lead_config`. P adds no MODEL or EFFORT rows; it uses these. M3 adds `uzi run lead-config <run-id> [--json]`: `GEN PHASE ENTRY SEQ REQUESTED SOURCE OBSERVED EVIDENCE REPORTED`, `uzicli` client method plus `FakeClient`.

### 9. Seam contract for consumers (frozen by this PRD)

Every consumer extends these, adds no second lead resolver, run-level lead label table, request matrix or reporting path, never infers from usage frames, and never updates rows. Child B2 is not a consumer (user decision 2026-10-21): its Codex catalog defaults use B's `model/list` probe, its own table and its own route. Settings surfaces use child C's label table.

| Consumer | Edits | Reads |
|---|---|---|
| **P** (per-run override) | Server: `LeadSettingsInput` gains `RunModelSource` (`runs.model_source`), `RunEffort`, `RunEffortSource`; `resolveLeadSettings` returns `ModelSource` / `EffortSource = run_override` for them (model before the schedule branch). `LeadSettings` gains an `Unhonorable` result field (not an error return, so the function stays pure) that `assembleClaim` maps to P's `run_override_unavailable` hold; an unhonorable `run_override` is never soft-dropped through `DroppedRunModel`, and a dropped schedule pin yields the lane's source, not `schedule_pin`. Display: P extends pre-claim rule 2 with a `run.model_source` branch in both adapters ("fixed by schedule" only for `schedule_pin`, "this run" for `run_override`), widens `preClaim` with `effort` / `effortSource` and the "model resolves at claim" form, and appends DC-P1 to DC-P7 to `display_cases`, including DC-P6 (paused on `run_override_unavailable` before any report exists, so rule 3's create-time values show) and DC-P7 (queued schedule pin, the unchanged regression guard). Appends `run_override` cases to `request_cases`. No agent change for reporting (the worker forwards `run_override`). P adds no `MODEL` / `EFFORT` rows. | `lead_config.phases[].requested_*_source`, `requested_*_fixed_at`, observed fields; label `this run` (already in the table); rows `LEAD` / `LEAD_SOURCE`. |
| **Q-claude** | Its own planning claim fields; Q-claude's pure `resolvePlanPhase` (new `api/internal/workersvc/claim_phase.go`) is called from inside H's `resolveLeadSettings`, which gains a `Plan` result (`*ClaimPhaseSettings`, source `user_planning_default`) so lead precedence keeps one entry point; `resolveLeadRequest` selects plan values when `phase = "plan"`; the Claude executor passes the plan `LeadRequest` to `enterPhase("plan", ...)`. A post-approval re-plan is a phase change: entry 2, new rows. Appends plan-phase cases to `request_cases`. Q-claude's planning settings rows are C's settings cells, not H labels. | Per-phase rows, `phase_entry`, rule 6; the section 5 known limit applies. |
| **Q-codex** | Same `resolveLeadRequest` plan selection for Codex and `enterPhase` in `driveCodexTurn`; relies on `codex_sent` vs `codex_thread_response` semantics (section 7); a resumed thread's persisted effort shows as `codex_runtime_default` when no effort is sent. | Same as Q-claude. |
| **G** (per-harness template pins) | Owns the single lead-template rule (`selectLeadTemplate` / `agenttmpl.SelectLead`: the first `lead\|orchestrator` template in delivered order) and the per-harness projection of the delivered `ClaimAgent.Model` (section 3). Passes `selectLeadTemplate(ctx.agents)?.model` as `templateModel` on both harnesses and extends the Codex branch of `resolveLeadRequest` to `default_model ?? templateModel ?? providerDefaultModel` with source `template` (section 6); appends its cases to `request_cases`. No schema or claim-field change: `template` is already an allowed source for Codex rows (D20). | Label `lead template`. |
| **N** (template effort) | `requested_effort_source = template` (already allowed); its label row exists. | |
| **F** (resolver, lands last) | Composes `resolveLeadSettings` from its prediction primitives (`api/internal/workersvc/effective_settings.go`, composed in `api/internal/handler/effective_settings.go`) instead of extracting a second copy; adds a Go reader of `request_cases`. Adds no rows to H's label table: F's cells use C's `fixtures/model-settings/contract.json`. | `request_cases`, `leadcfg.FixedAt`. |

### 10. Rollout orders and races

- **API first** (normal on k8s: the app tier follows the chart, the worker fleet is pinned by `workers.image.tag`): old workers ignore the two claim fields and never post; lead-lane runs show rule 3. No error path.
- **Worker first** (possible on compose): the claim lacks `default_effort_source`, so the reporter is inactive. If the API is rolled back after rows exist, the table is inert; a re-upgrade keeps them.
- **Delayed write after the actor finishes:** a post after terminal status, claim release or reclaim fails the fence (409), including a retry of a row that was stored before the reclaim; the reporter disables itself. Accepted: that attempt's record simply ends. A crash between `enterPhase` and `observe` leaves a requested-only row ("not reported yet" while current, "not reported by the runtime" once terminal).
- **Late requested row:** if the 3 s bound elapses, the first model turn starts before the row lands. Accepted, display-only.
- **Late observation:** attached to its own phase entry or dropped (section 6); never attached to a newer entry.
- **Refresh frame ordering:** the `refresh` frame carries no status, so a frame published after a concurrent status change only triggers a re-read.
- **Trust:** rows are worker-attested. The server validates shape, bounds and the claim fence and copies `harness` itself; it does not verify truth. A compromised worker can misreport its own config, but could already run any model; the rows never feed billing, auth or scheduling.

## Testing decisions

- **Fence and storage (LiveDB):** follow `api/internal/workersvc/decision_memo_livedb_test.go`. Stale generation, released claim, other worker, terminal status, non-lead kind, review run, isolated-lane run (each 409, nothing written); an existing key after release and after reclaim (409 `claim_not_current`, buddy H1); identical duplicate (204, not republished); differing duplicate (409 `lead_config_key_conflict`, first row kept); every vocabulary value accepted and one out-of-set value per column rejected (23514); every table check including the harness and Claude-not-sent checks; the trigger rejects an UPDATE; a run delete cascades. Every named constraint is exercised by name. Ordering: with both phases present, the overlay and the history return the plan row first.
- **Concurrent identical retry (LiveDB, dba 1):** `TestRecordLeadConfigConcurrentIdenticalRetryLiveDB` in `workersvc` (already in both store-it lists). Fixture: a claimed lead-lane issue run at generation 1 held by worker W. `txA := pool.BeginTx(ReadCommitted)` runs `InsertRunLeadConfigFenced` with body X through `store.New(txA)` and asserts `inserted`, uncommitted. A goroutine calls `svc.RecordLeadConfig(ctx, W, run, X)` on the pool. The test waits event-driven, with a 10 s context deadline and no fixed sleep, until `pg_stat_activity` shows a backend with `cardinality(pg_blocking_pids(pid)) > 0` on a `run_lead_configs` query; past the deadline it fails (never skips), so the interleaving is proven. Then `txA.Commit()`: the goroutine's result is a duplicate success (`Inserted=false`, mapped to 204), exactly one row exists, and no `refresh` frame was published for it. Variant under the same interleaving: B differs only in a nullable field (`observed_model_evidence` NULL vs `unknown`) and gets `ErrLeadConfigKeyConflict`, first row kept. Sequential cases: a requested-only row (all four observed columns NULL) and a Claude not-sent row (`observed_effort` NULL, `claude_init`) posted twice each give 204 the second time. Mutations to watch red: the equality moved back into statement 1's snapshot (concurrent case answers 409); `IS NOT DISTINCT FROM` changed to `=` (sequential cases answer 409); the transaction opened at REPEATABLE READ (concurrent case fails with 40001).
- **No UPDATE/DELETE query:** a Go test scanning `api/internal/store/queries/*.sql` and the hand-written `api/internal/store/*.go` (not `*.sql.go`) with the two case-insensitive patterns of section 1, calibrated first against an in-test canary containing `update  public.run_lead_configs` and `DELETE\nFROM run_lead_configs`.
- **Lane predicate agreement:** the `registry.json` x review x egress table test (section 2).
- **Handler status mapping:** follow `api/internal/handler/worker_decision_memo_test.go`: unknown field, trailing value, 4097-byte body, model with a control or bidi character, 101-byte model, bad enum, missing generation, `phase_entry` 9, `seq` 33, `truncated` with `seq` 31. Route rows in `jobRefusedRoutes` and `wantRouteMounts`. A Codex report with `requested_model` NULL passes `ValidateReport`, hits `run_lead_configs_requested_model_check` (23514) and answers 400 `invalid_lead_config` field `requested_model`, not 500.
- **Refresh frame:** a handler test with a hub subscriber sees exactly one `refresh` frame per insert and none for an identical duplicate; a recording `Broadcaster` (Slack side) sees nothing. `web/src/lib/runStream.test.ts` gains the `refresh` case; `uzicli` normalization test gains the type. A refused (409), conflicting, rolled-back (statement 2 error injected through a failing `Querier` double) or 500 write publishes nothing; publishing happens only after commit. A `Handler` built with a nil hub records a row without panicking (`PublishRefresh` nil-safe). `hub.go`'s drop-cost comment covers five frame types.
- **Server resolver:** unit table for `resolveLeadSettings` (schedule pin applied; incompatible pin dropped with `DroppedRunModel` and `user_default`; lane set; lane empty gives invalid model and empty source; effort user vs builtin) plus the existing `harness_model_compat_test.go` / `harness_claim_repair_livedb_test.go` staying green unchanged; goldens and absence assertions as in section 3.
- **GetRun overlay and history:** LiveDB handler tests with two generations, both phases and a second plan entry: latest-per-phase-of-latest-generation, `current`, `truncated`, derived `fixed_at`; history ordering, the 256 cap, owner and admin read, another user 404; the `cli_auth_livedb_test.go` bearer case. Re-record `fixtures/api-contract/run.{zero,full}.json`.
- **Bug-shaped regression, watched red:** a RunView test rendering a claimed default-model Claude run (`run.model = null`, `lead_config` present) asserting the `lead: opus · medium` chip; on `522aab1ba` no chip renders. Repoint the PRD #300 describe in `web/src/pages/RunView.test.tsx` to rules 2 and 3; sweep the retired literal with `git grep -F 'frozen by its schedule'` (today only `RunView.tsx` and `RunView.test.tsx`; `e2e/` has none, re-verified at `522aab1ba`).
- **Display and labels:** `web/src/lib/leadConfig.test.ts` and a `runpresentation` Go test iterate `display_cases` and `labels`, each id asserted visited (no snapshot of demo data).
- **Reporter:** unit tests with a fake client and injected timers (no real waits): activation, phase-change dedupe and entry numbering, seq allocation, per-entry observation cap (the 4th observation of one entry is dropped and logged while a later entry still posts), the truncation row only on `seq` exhaustion or a 9th entry, the 8-entry cap, observe dedupe, late observation attached vs dropped (buddy H2: a delayed plan observation after implement starts, and after another plan entry), queue bound, 400/404/409 disablement, never throws, bounded `enterPhase` await.
- **One reporter per claim:** a runner-level test (using the new `fake-api.ts` route) where a re-driven turn and a recreated Codex epoch (the #1782 refused-wall-park path) post rows with continuing `seq` from the same reporter.
- **Executor sequences:** `agent/test/sdk-executor.test.ts` (fake `queryFn`) and `agent/test/codex-executor.test.ts` (fake app-server): plan, revise, approve, implement and a pre-approved resume, asserting the posted rows; the `request_cases` matrix (section 6). For Codex, a checkpoint epoch's `thread/resume` inside one implement entry posts nothing when unchanged and one row when the configured effort changed; for Claude, the callback is passed only on the first turn of an entry.
- **Adapters (M2):** Claude init with `effort` present, `null`, absent with `getSettings` answering, missing, throwing, timing out, malformed (follow the init-decode tests in `agent/test/sdk-messages.test.ts` / `claude-isolated-init.test.ts`); probe runs once per phase entry. Codex in `agent/test/codex-harness.test.ts`: response with `model` and `reasoningEffort`, effort sent vs not sent, `persistent` and a custom value mapping to `other`, null mapping to `unknown`, the approval epoch's `thread/resume` producing an implement observation.
- **Mutation discipline** (`.claude/rules/go.md`): drop, one at a time, the `claim_generation` predicate, the kind predicate, the `ON CONFLICT` target, the trigger, and the three dba-1 mutations above, and watch the matching test fail.
- **CLI:** `api/cmd/uzi` render tests for `LEAD` / `LEAD_SOURCE` (absent for non-lead runs) and `run lead-config` (table and `--json`) against `uzicli.FakeClient`.
- **E2E:** the product contract changes (new claim keys, new worker route, new frame type). The sweep found no stale e2e literal; the PR names the compose E2E phases left unrun and runs `task test:e2e-lib`.
- No real-time waits; the PR states CI time impact as measured or unmeasured.

## Milestones

### M1: Requested lead config recorded and shown, both harnesses

Blocked by: none.

Layers: migration `00351` (draft) with trigger; `api/internal/leadcfg` and `fixtures/lead-config/contract.json` (vocabulary, `fixed_at`, labels, `request_cases`, requested-side display cases); `lead_configs.sql` (`InsertRunLeadConfigFenced`, `RunLeadConfigSameBody`, `ListLatestRunLeadConfig`) and the hand-written `api/internal/store/lead_config_record.go` transaction; `resolveLeadSettings` extraction and claim provenance with goldens; `RecordLeadConfig`, the worker route, its `wantRouteMounts` row and route-count sentence, and the nil-safe `Hub.PublishRefresh` (web `applyFrame` and frame union, `uzicli` type, `hub.go` drop-cost comment); `agent/src/lead-config-report.ts` (`resolveLeadRequest`, `enterPhase`), runner construction, `client.recordLeadConfig`, `protocol.ts` fields, `fake-api.ts` route; wiring in `driveTurn` and `driveCodexTurn`; GetRun overlay, `RunDTO.LeadConfig`, contract fixtures, `apiTypes.ts`; `leadConfigView` / `runpresentation.LeadConfigView`; RunView chip and mock data; `uzi run get` rows. Docs: `docs/worker-model.md` (new "See which model a run used" section; correct the stale "`uzi run get`" claims for the model and the fleet-wide flag to `--json`; correct "Separate from reasoning effort ... shared across both harnesses", false since #2060), `docs/worker-effort.md` (requested effort shown per run), `docs/cli.md` (`run get` rows), then `task docs:sync`; `ARCHITECTURE.md` "Agent runtime" one line linking this PRD; `specs/human.md` entry under "Feature #17" tagged `(AI-synced YYYY-MM-DD)`; one `changelog.d/<uuid>.md` fragment (`### Added`).

Acceptance:
- Examples 1 to 3, minus the runtime lines, pass as LiveDB plus executor tests; the header is correct for the default, schedule-pinned and template-fallback cases (GetRun LiveDB test plus RunView tests).
- The storage, fence, lane-agreement, handler, refresh, resolver, overlay, reporter (`enterPhase`, budgets, one-per-claim) and executor-sequence tests pass, and the requested-side display cases for rules 1 to 7 pass in both implementations.
- The regression test fails on `522aab1ba` and passes here.
- Explicit absence of the new claim keys for cross-check, Claude review, Codex review and isolated-lane claims; the three ci goldens and the skills golden carry them.
- Mock parity with section C (placement, text, pre-claim wording).
- `task gate:api`, `task gate:agent`, `task gate:web`, `task gate:repo` (migration numbering and additivity, changelog entry) and `task check-docs:web` green; `TestEmbeddedDocsMatchSource` green after `task docs:sync`.

### M2: Runtime-reported effective model and effort

Blocked by: M1.

Layers: `RunTurnRequest.onRuntimeConfig`; Claude init decode and the single `getSettings` probe; Codex response decode and sent-effort evidence; `LeadConfigReporter.observe` with entry binding and executor wiring; observed-side display cases; docs: `docs/worker-effort.md` replaces "effective effort reporting [is] planned follow-up work" with how to read requested vs effective (the runtime-default choice stays planned for child B) and states that Claude effective effort may read "not reported by the runtime" on the pinned SDK and that the Codex reading is the thread's configuration.

Acceptance:
- Adapter and late-observation tests pass; examples 1 and 2 including runtime lines pass at the executor seam.
- A Claude init carrying `effort: "high"` while `xhigh` was requested renders `high (requested xhigh)`; Claude `effort: null` renders "not sent"; Codex `persistent` renders "an unrecognized level" and is stored as `other`.
- No observation path reads usage frames (review check: the reporter imports nothing from `usage-recorder.ts` or the token accountant).
- Gates as in M1.

### M3: Per-attempt history

Blocked by: M1.

Layers: `ListRunLeadConfigHistory`; `GET /api/runs/{id}/lead-config` with its `wantRouteMounts` and `cli_auth_livedb_test.go` rows; `uzicli` client method and `FakeClient`; `uzi run lead-config <run-id> [--json]`; `docs/cli.md`, `task docs:sync`.

Acceptance:
- Example 3's history listing passes (LiveDB handler test plus CLI render test).
- Owner and admin read; another user gets 404; a `uzc_` bearer reads it; 257 rows return 256 with `truncated: true`.
- Gates as in M1.

## Decision Log

- **D1. Lead-lane runs only. User decision 2026-10-21.** `issue`, `ci_fix`, `prompt`, `self_improve`, `mr_rework` and non-review `task`, excluding isolated-lane (profile-bound) runs, which the IsolatedRunner executes without a lead executor. One predicate, `leadcfg.LeadLane`, in claim, overlay and fence. Rejected: every run kind.
- **D2. Provenance is split by who decides it; each side has one home.** `resolveLeadSettings` decides schedule, user, run and planning sources; `resolveLeadRequest` decides `template` / `builtin`, because only the worker selects the lead template and owns the Codex provider default. Rejected: the server computing the whole requested side (a mirror of lead selection and the provider default).
- **D3. Flat claim provenance in H.** Executors keep sending `default_model` / `default_effort`; a per-phase claim struct with equal values would be a second truth H never reads. Q-claude adds its own planning fields through `resolveLeadSettings`. Rejected: shipping `{plan, implement}` now.
- **D4. Requested is configuration; observed is the runtime's report.** The chip shows the requested alias (matching the mock); the runtime value goes to the tooltip and CLI; an effort mismatch the runtime reported surfaces in the chip. A sent value is never shown as a verified effective model. Rejected: the init model id in the chip (long; alias vs id would read as a permanent mismatch).
- **D5. Full-snapshot rows keyed by worker-assigned `seq`, bound to a phase entry; duplicates decided under the fence.** Latest row per phase is self-contained; retries are idempotent while the claim is current (null-safe comparison); a differing duplicate is a loud 409. One reporter per claim makes `seq` monotonic. Rejected: server-assigned `seq` (retries duplicate rows) and separate requested/observed tables.
- **D6. Phase-entry row awaited at most 3 s before the first turn, fail-open.** Meets "report before the phase's first model turn" without letting observability stall or fail a run. Rejected: blocking until acknowledged; fire-and-forget only.
- **D7. Observation via an optional `RunTurnRequest` callback, not a new `HarnessEvent`.** Codex emits its claim `initialized` once per claim; a new event kind would touch the reducer and projection for a display-only fact.
- **D8. Claude effort evidence is opportunistic; one probe for the codebase.** Init `effort` is typed in 0.3.293 but documented as host-dependent; `getSettings` exists at runtime but not in the public `Query` type. Both are feature-detected with a strict decode, else `unknown`; the probe runs at most once per phase entry, 2000 ms. No other child builds a Claude settings probe (B2 is Codex-only and uses B's `model/list` probe). Rejected: assuming requested equals effective; inferring from usage frames.
- **D9. `codex_thread_response` accepted (shared design v4).** It is the thread's resolved `config_snapshot` model from the `thread/start` / `thread/resume` response (traced at `092d3acd`), equal to the sent model unless Codex rewrote it, not independent evidence of each provider turn; labelled "Codex thread config". Codex effort: sent gives `codex_sent`; none sent gives the response's configured effort as `codex_runtime_default` (traced: `config.model_reasoning_effort` plus a resumed thread's persisted mode, not the catalog default; usually null today, so `unknown`). B keeps its own `model/list` catalog probe for supported levels and never uses this field for that (v4.1 item 3).
- **D10. Observed effort set = v4.1 item 4.** `none, minimal, low, medium, high, xhigh, max, ultra` plus `other` (Codex `persistent` and custom strings); `other` is never rendered as a level or used as a cached default; `unknown` evidence is distinct from an observed `none`. Claude's "no effort parameter will be sent" (`effort: null`) is not a level, so it is stored as NULL value with real Claude evidence and labelled "not sent", rather than as `none` (the r1 conflation, cross 8) or a new `not_sent` token (cross 8's proposal, which v4.1 item 4 does not list). Flagged to the lead.
- **D11. Vocabulary scope.** Sources include `run_override` and `user_planning_default` (P, Q) and `template` for effort (N, cross nit 21) now; exclude `cross_check_*` and `instance_default` (not lead sources).
- **D12. Live update is a new data-less hub frame `refresh`, published by the handler to the hub only, and only after the write's transaction committed.** Rejected: `publishRunState` (goes through `MultiBroadcaster` to Slack, and a status read before the locked insert can overwrite a newer status in the TUI, review 4); a fresh status from `RETURNING` still races the next state publish after commit; reusing `health` (meaning drift).
- **D13. GetRun-only overlay with an `applicable` flag.** No per-row lookup on lists; clients carry no lane mirror.
- **D14. Trust model: worker-attested, server-fenced, display-only.** `harness` is copied server-side; harness-conditional checks use it.
- **D15. No ADR.** The fixture plus this Decision Log are the contract; `ARCHITECTURE.md` links this PRD. F, which lands last, may promote it if it outgrows the epic.
- **D16. TUI unchanged.** It shows no model today; adding one is a separate UX decision (`.claude/rules/tui.md` D7 rendering would apply).
- **D17. `fixed_at` is derived, not stored.** It is a pure function of the source (`run_override`, `schedule_pin` give `create`; others `claim`), owned by `leadcfg.FixedAt`, pinned in the fixture and exposed on the DTO. Rejected: two stored columns that could disagree with their source.
- **D18. `reported_at` (server receipt) instead of v3's `observed_at`.** Worker clocks are not trusted; receipt lags observation by the reporter queue (seconds, bounded by one in-flight request and the 3 s / 2000 ms timeouts). Rejected: a worker-supplied timestamp.
- **D19. Early failure is honest, not special-cased.** A run that fails before its first lead turn reads "not recorded", with a tooltip naming both causes (review 6, option A). Rejected: a claim-acceptance row (the worker does not know the template fallback yet) and a "never started" rule (indistinguishable from an older worker without per-run worker capability history).
- **D20. Harness checks.** `codex` implies a non-NULL requested model. Not added: "Codex never uses `template`" (review nit 14's second half), because child G makes the Codex root use the lead template's Codex pin.
- **D21. H keeps today's pre-claim rule; consumers extend through the adapter and the fixture.** `runs.model_source` and `runs.effort` arrive with P, so H cannot branch on them and ships no code path it cannot exercise. The label lookup by `modelSource` makes "fixed by schedule" impossible for any other source once P's adapter supplies one; P's DC-P1 to DC-P7 pin it. Rejected: H authoring `run_override` cases against fields that do not exist yet (untestable in H).
- **D22. Two statements in one READ COMMITTED transaction for the write (dba 1, reproduced on PostgreSQL 17.11).** The fenced insert returns `fenced` / `inserted`; a second statement with a fresh snapshot, under the run-row lock held since statement 1 and until commit, compares the bodies with `IS NOT DISTINCT FROM` on the nullable columns (`phase_entry` and `truncated` included; `reported_at` and `harness` excluded). Reason: a single statement's start snapshot cannot see a row committed by the writer it waited on, so an identical concurrent retry would get a false 409; `=` on NULL columns would 409 every identical retry of a requested-only row. Rejected: re-running the single statement (a hidden retry loop), `ON CONFLICT DO UPDATE ... WHERE` (fires the append-only trigger), REPEATABLE READ (40001 on the waiting retry). Precedent: `(q *Queries) RecordLiveCheckpointPublishAttempt`.
- **D23. Named constraints.** Every CHECK is a named table constraint so the handler maps the harness-dependent 23514 to 400 by name and any later migration can cite names, not generated `_check1` suffixes.
- **D24. Per-entry observation cap drops, it does not truncate the phase (review G).** One chatty entry no longer hides later entries; the `truncated` row is reserved for `seq` exhaustion or a 9th entry, so a truncated record is still explicit.
- **D25. Two label domains (round-3 ruling).** H's table labels run-level lead sources; settings surfaces use C's (field, token) table. Rejected: one table for both (the same token needs different words on a settings cell and in a run header).
