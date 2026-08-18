# PRD #362 — Plain-English run summaries

**Issue**: #362
**Status**: Draft
**Priority**: Medium
**Author**: Vlad Mocanu
**Created**: 2026-08-18

## Problem

A run's card, detail view, and CLI output show status, worker, timestamps, tokens, and cost — but never say, in plain English, **what the run is actually setting out to do**. A user scanning the board reads an issue title and a status pill; to understand the work they must open the issue or read the raw plan markdown. And after the agent plans, nothing captures **how the proposed plan diverged from the original ask** — the reconsiderations, additions, and drops that are the most interesting part of the approval gate.

## Solution

Generate two short plain-English summaries per run, on the user's own Anthropic token, with a cheap default model (Haiku):

1. **Intent summary** — "What this run will implement." Compiled from the issue title + body + linked PRD (when present) as the worker starts, before it plans. Answers "what is this run doing?" at a glance.
2. **Plan summary + deltas** — "What the proposed plan will do", compiled the moment the agent produces a plan (run enters `awaiting_approval`), so it is decision-support *before* the human approves. Includes a tagged list of what changed from the original ask (`added` / `changed` / `dropped`). Once approved, the same artifact relabels from "proposed" to "approved" (label derived from run status, not regenerated). Regenerated only if the plan is actually revised (re-plan round).

Both summaries render as collapsible cards on the run detail view (**expanded by default**; the collapse choice is remembered per run, client-side), a one-line intent preview on the runs list, and rows in the CLI `run get` output. The generation model is an admin setting (global default `haiku`, optional per-user override), mirroring the existing Judge model setting. Summary generation is **advisory and never blocks the implementation turn or the run's terminal outcome**: on any failure or timeout it is skipped and the UI falls back to the issue title.

## Design decisions (Decision Log)

1. **Generation is INLINE inside the issue-run executor — summaries are NOT a run kind.**
   This corrects an earlier "mirror the Judge" framing that two PRD reviews (2026-08-18) flagged as a category error. The Judge is a *separate run kind* with its own claim, its own slim runner (no clone/worktree/git), a dedicated post-back endpoint (`POST /worker/runs/:id/review`), and it is enqueued only *after* the reviewed run reaches a terminal state. Summaries are the opposite shape: they are produced **inside a normal, in-flight issue run**, on a claim that already has the clone (needed to read the PRD) and the owner's token.
   - Hook homes (agent side): intent — after `runner.execute` provisions the clone and **before** the planning turn (`agent/src/sdk-executor.ts` `drivePlanningTurn`, ~:958); plan — at the plan gate / `awaiting_approval` post (`gatePlan`, ~:983) and again in the revise loop (~:985).
   - **What is reused from the Judge is ONLY the tool-less model-call recipe**: `buildSdkEnv`, `runModel`, a single tool-less turn, a wall-clock timeout (new `SUMMARY_MODEL_TIMEOUT_MS`, mirroring `JUDGE_MODEL_TIMEOUT_MS`), and JSON extraction (`extractJsonObject`, `agent/src/judge-runner.ts:649`, which tolerates a ```json fence). Each summary turn runs with its **own ephemeral SDK `homeDir`** — the run's HOME belongs to the main session and must not be shared.
   - Rationale for worker-side (vs. a net-new api-side generation client): the **PRD text is not in the database** — it is a `prds/*.md` file the worker reads from the clone — so only the worker can feed PRD into the intent summary; and the api has no Anthropic *generation* client today (`api/internal/anthropic/client.go` only does rate-limit probes).
   - **Consequence (accepted):** the intent summary appears once the worker has provisioned the clone (early in `running`), not while the run is `queued`. During `queued`, and until the intent summary lands, the card shows the issue title as fallback.

2. **Triggers and blocking semantics** (resolves the "before planning" vs. "never blocks" tension the reviews raised):
   - **Intent — async, non-blocking.** Kicked off after the clone is ready and before planning, but it does **not** delay the planning turn; planning proceeds immediately and the summary lands via the persist endpoint + WebSocket when ready (typically a few seconds on Haiku). It is a label, not decision-support, so async is correct.
   - **Plan — blocks the gate, not the work.** Because the plan summary *is* decision-support (the human reads it at the approval gate), its generation blocks the **transition into `awaiting_approval`** up to `SUMMARY_MODEL_TIMEOUT_MS`. If it times out, the run parks anyway and the summary lands async via WebSocket. It **never** blocks the implementation turn or the run's terminal outcome.
   - **Re-plan** re-enters the plan hook each revise round → regenerate, same gate-blocking rule.
   - **Approval does not regenerate** — the plan text is unchanged, so the UI relabels proposed→approved purely from run status.

3. **Idempotency and stale-write protection** (resumes and re-plan races are common here — affinity-grace re-claims, sweeper requeues, `limit_wait → execute()`):
   - Intent generation is **skipped when `summary_intent` is already set**, so a re-claim/resume does not re-spend the user's token or churn the summary.
   - The plan-summary POST carries the `plan_md` (or its hash) the summary was generated from; the persist endpoint writes `summary_plan`/`summary_deltas` **only if that still matches `runs.plan_md`**. A slower earlier generation therefore cannot overwrite the summary of a newer, revised plan (last-write-wins by *plan version*, not by completion time). No extra column needed.

4. **"planning" is a derived phase, not a status.** The runs state machine is `queued → claimed → running → awaiting_approval → running → completed | failed | cancelled` (plus `awaiting_input`, `limit_wait`). "Planning" is the derived predicate `is_planning` (running, iteration 0, no `plan_md` yet). No `planning` status is written and none should be added.

5. **Seeded / pre-approved runs get an intent summary only.** Seeded or pre-approved-resume runs skip the planning turn (`agent/src/sdk-executor.ts` `preApproved`, ~:864), so no plan is produced and the plan-summary hook never fires. These runs carry `summary_intent` and no `summary_plan`/`summary_deltas`; the UI shows only the intent card.

6. **Storage: three new columns on `runs`** (migration `00131_run_summaries.sql`), not a side table:
   - `summary_intent text` — the intent summary.
   - `summary_plan text` — the plan summary.
   - `summary_deltas jsonb` — a list of `{ "kind": "added" | "changed" | "dropped", "text": string }`.
   `summary_deltas` is **validated-and-rejected on persist** (must be an array of `{kind,text}` with `kind ∈ {added,changed,dropped}` and bounded lengths) and **tolerated-with-fallback on read** (a malformed or unexpected value renders as "no deltas", never crashes the web/CLI renderer).

7. **"Original ask" for deltas = the intent inputs** (issue title + body + PRD), compared against the produced plan — not plan-vs-prior-plan. (Prior plan versions already live in `run_messages` `kind='plan'`; cross-round plan diffing is out of scope.)

8. **Model selection mirrors the Judge's *settings*, but rides the ISSUE-RUN claim.** New `app_settings` key `summary_model` (default alias `haiku`), plus an optional per-user override `users.summary_model` (a **second migration**, mirroring `00125_user_judge_model.sql`), resolved admin-global-then-per-user. **Delivery differs from the Judge:** `judge_model` is present only on `kind=judge` claims (`api/internal/workersvc/claim.go:131`, set in `assembleJudgeClaim`). `summary_model` must be added to the **issue-run** claim assembly — `Service.Claim → assembleClaim` (`api/internal/workersvc/service.go:1029`) — and to the agent claim type (`agent/src/protocol.ts`, `claim.go`). Validation is `ValidateModel` (shape-only, `MaxModelLen=100`); aliases (`haiku`/`sonnet`/`opus`/`fable`) and full ids both accepted. **Default is `haiku`** (the Judge defaults to `opus`; summaries are lighter and per-run, so Haiku — fast and near-free — is the right default).

9. **Collapse is a client-side per-run preference**, stored in `localStorage` keyed by run id with a `savedAt` timestamp; entries older than 7 days are dropped on read (GC so the store does not grow). **Default expanded.** Not server-side: the pref is not worth a round-trip, and per-browser is acceptable. Net-new pattern (`web/src/lib/prefs.ts` is a generic wrapper with no per-run/GC pattern today).

10. **Advisory, never a control.** The runner is **tool-less** (untrusted issue/PRD/plan text cannot drive any action), all summary text is rendered as text (web) and routed through `cellText` (CLI). The deltas are the model's interpretation at a human approval gate, so the UI frames them as an at-a-glance heads-up, **not** a substitute for reading the plan — a crafted issue/PRD could bias the deltas (e.g. hide a dropped security step), and the human still reads the real plan to decide.

## Milestones

Each producer milestone **carries its own tests** (this repo validates per-milestone; deferring all tests to the end lets the H1/H2-class races land untested). M9 is cross-cutting/e2e only.

### Phase 0 — Foundation (sequential; blocks everything)

- [ ] **M1 — Data model, DTO, persist endpoint, live-update.**
  - Migration `00131_run_summaries.sql` (rename to next free number above the live head at landing — head is currently `00130_run_priority.sql`) adding `summary_intent text`, `summary_plan text`, `summary_deltas jsonb` to `runs`, all nullable. Regenerate sqlc; extend `api/internal/store/queries/` with `SetRunIntentSummary` and `SetRunPlanSummary`, and include the columns in run reads.
  - Add `SummaryIntent`, `SummaryPlan`, `SummaryDeltas` to `RunDTO` (`api/internal/apitypes/run.go`), populate in `runToDTO` (`api/internal/handler/workers.go`), add matching fields to the web `Run` interface (`web/src/lib/api.ts`).
  - **Worker persist endpoint(s)**, modeled on `api/internal/handler/worker_findings.go` `WorkerCreateFinding` (auth `mw.WorkerFromContext`; guards: run belongs to this worker's user, non-terminal, has a repo, bounded text) and `api/internal/handler/judge_worker.go`. The intent write is idempotent-on-set; the plan write enforces the **stale-write guard** (Decision 3) and **validates `summary_deltas`** (Decision 6).
  - **On successful persist, emit a run-updated WebSocket frame** so the hub broadcasts and `useRunStream` in `web/src/pages/RunView.tsx` refetches. Without this, an intent summary posted mid-`running` (no state transition) never surfaces — the client only refetches on state/health frames.
  - Tests: migration up/down, new queries, `runToDTO` population, endpoint auth + guards + stale-write rejection + deltas validation (store live-DB `*_integration_test.go` + handler tests). Success = all three fields travel end-to-end **and a WS frame fires on persist**.

### Phase 1 — Settings and consumers (after M1)

- [ ] **M2 — Summary model setting (admin global + per-user).** Runs **after M1** (both regenerate sqlc / touch `store/models.go` — must not run concurrently). New `summary_model` key in `api/internal/settings/settings.go` (const, default `haiku`, key→default map entry, `Validate` case, cache accessor `SummaryModel(ctx)`); wire into `GetSettings`/`UpdateSettings`; add a "Summary model" field on `web/src/pages/AdminSettings.tsx` (same free-text `Input maxLength={100}` as Judge model, placeholder `haiku`). Second migration `users.summary_model` (mirror `00125_user_judge_model.sql`) + `GetUserSummaryModel`; resolve admin-then-user and set `SummaryModel` on the **issue-run** claim in `assembleClaim` (`service.go`); add the field to `agent/src/protocol.ts` + `claim.go`. Co-located tests (settings validate/default/merge, per-user resolution, claim carries the field).
- [ ] **M4 — Web UI** (needs M1 only; parallel with M2/M5). RunView.tsx: intent card + proposed/approved plan card + deltas list + collapse toggles (**expanded by default**), state label ("proposed" vs "approved") derived from run status, empty deltas → "No deviations — the plan matches the original ask", malformed deltas tolerated (fallback to no-deltas). RunsList.tsx: one-line intent preview (currently renders only `run.issue_title`). New per-run collapse pref helper in `web/src/lib/prefs.ts` (localStorage, per-run key, 7-day GC). Live update rides the existing `useRunStream` once M1 emits the frame. Vitest: collapse pref (persist/reload/expiry), card rendering across states, malformed-deltas fallback.
- [ ] **M5 — CLI** (needs M1 only; parallel with M2/M4). `api/cmd/uzi/run.go` `renderRunDetail`: add intent, plan, and deltas rows (route all summary text through `cellText`). Render test (`render_test.go`) with and without summaries.

### Phase 2 — Worker generator (after M1 + M2)

- [ ] **M3a — Inline tool-less summary helper.** A worker-side helper reusing the Judge's model-call *mechanics* only: `buildSdkEnv` with its **own ephemeral homeDir**, one tool-less `runModel` turn, `Promise.race` against `SUMMARY_MODEL_TIMEOUT_MS`, `extractJsonObject` for the plan/deltas turn. Unit test with a stubbed query fn (precedent: `agent/src/judge-runner-stub.ts` `stubJudgeQueryFn`) asserting timeout/throw → returns "no summary" without throwing into the caller.
- [ ] **M3b — PRD-link resolver + input assembly** (security-sensitive; net-new). From `claim.issue_description`, extract the `prds/*.md` link (the Go detector `prdLinkRe` in `api/internal/forgesvc/service.go:82` is the reference for the URL shapes — blob-URL prefix, `#`/`?` suffix), reduce it to a clone-relative path, and **guard path traversal** (there is no TS equivalent of Go's `prdpath.Validate` — port its semantics; the clone is attacker-influenceable), then read the file from the clone. Fallback to title + body when no valid PRD link resolves; record which inputs were used in the summary provenance. Tests: valid link, blob-URL forms, traversal attempts rejected, missing-file fallback.
- [ ] **M3c — Wire the hooks + post-back** (needs M1 endpoints + M2 claim field + M3a + M3b). Intent hook before `drivePlanningTurn` (async, non-blocking, **skip if `summary_intent` present**); plan hook at `gatePlan`/awaiting_approval and the revise loop (**blocks gate entry up to the timeout**, sends the plan hash for the stale-write guard); post results via the M1 endpoints; seeded/pre-approved runs produce intent only. Tests (`node --test`): a throwing generator leaves the executor reaching implement/terminal unchanged; intent skipped on resume when already set; plan hook fires per revise round.

### Phase 3 — Validation & docs

- [ ] **M9 — Cross-cutting e2e + docs.** End-to-end: a run acquires an intent summary (UI updates live via WS), then a plan summary + deltas visible at the gate; **a forced generation error leaves the run outcome unchanged and the UI/CLI fall back to the issue title** (proven on ≥2 surfaces — an agent `node --test` that the executor still reaches implement, and a web/CLI render test that null summary → issue title). Docs: new `docs/run-summaries.md` (`audience: user`, valid frontmatter per `web/scripts/check-docs.mjs`), a one-line ARCHITECTURE.md pointer in the run-lane section, `docs/cli.md` update for the new `run get` rows, and the `summary_model` setting noted alongside the Judge model in admin docs.

## Success criteria

1. Every **claimed** issue run acquires an intent summary (or a logged fallback) shortly after the clone is provisioned; it renders on the card, detail view, and `run get`, and the detail view updates **live** (WS) without a manual refresh.
2. Every run entering **`awaiting_approval`** shows a plan summary + deltas at the gate, labeled "proposed", available *before* approval up to the timeout; after approval the same artifact reads "approved"; a re-plan regenerates it; a stale (older-plan) write is rejected.
3. An admin can change `summary_model`; a per-user override wins where set; a shape-invalid model id is rejected. The value is delivered on the issue-run claim.
4. A generation failure or timeout **never** changes a run's outcome — proven by a forced-error test on ≥2 surfaces.
5. Summaries are **expanded by default**; collapsing one and reloading keeps it collapsed for that run for up to 7 days, per browser.
6. Seeded/pre-approved runs show an intent summary and no plan summary, with no error.

## Risks & mitigations

- **Per-run token spend is higher than "one extra call".** It is 1 intent + 1 plan + one more per revise round (2..N), on the user's token. The Judge — which the settings mirror — has a cooldown + daily budget (`judge_cooldown_seconds`, `judge_daily_budget`) precisely because it spends per run; summaries ship **without** that guard in v1, relying on the Haiku default + intent idempotency + the stale-write guard (which avoids re-spending on superseded plans). **Follow-up:** reuse/extend the judge budget machinery if per-user spend proves material. Flagged, not silently accepted.
- **Intent generation adds a concurrent Anthropic call at run start.** It is async and does not delay planning, but it runs alongside the main session and needs its own ephemeral SDK home. Mitigated by Haiku + the timeout; on any error the run is unaffected.
- **PRD-link resolution is bespoke and security-sensitive** (M3b). Path-traversal against an attacker-influenceable clone; no existing TS guard. Mitigated by porting `prdpath.Validate` semantics and a fallback to title+body. Highest-risk code in the feature — reviewed as its own milestone.
- **Live update depends on the WS frame (M1).** If the persist endpoint does not broadcast, criteria 1–2 fail *silently* (card stays on fallback). Made an explicit M1 deliverable with a test.
- **Deltas are the model's interpretation at a security boundary.** Advisory framing; human reads the real plan; tool-less runner; validated-on-persist, tolerated-on-read.
- **Two migrations, numbered at landing.** `00131_run_summaries.sql` (runs columns) **and** the `users.summary_model` migration (M2) both renumber to the next free numbers above the live head on the landing rebase (strict goose, no allow-missing).

## Dependencies & ordering

- **Graph:** `M1 → M2` (sequential — both regen sqlc); `M1 → {M4, M5}` (parallel, DTO-only); `M1 + M2 → M3a/M3b → M3c`; then `M9`. M4/M5 can run alongside M2. M3a/M3b are agent-only and can start once M1's endpoint contract is fixed; M3c needs M1 + M2.
- Judge subsystem as the **mechanics** reference only (`agent/src/judge-runner.ts` `runModel`/`extractJsonObject`, `judge-runner-stub.ts`, `settings.JudgeModel`, `users.judge_model`) — not its lifecycle.
- sqlc + goose toolchain; `task gate` for validation.

## Out of scope

- api-side / queue-time generation (before a worker claims).
- Cross-round plan-vs-plan diffing (deltas compare plan against the original ask only).
- Server-side / cross-device collapse preferences.
- Summaries on the board card DTO (`latestRunDTO`) — list + detail are enough.
- A dedicated summary spend budget/cooldown in v1 (follow-up if needed).
- Judge/self-improve/chat/ci_fix run kinds beyond what falls out of hooking the issue-run executor.

---

## Appendix — resolved integration anchors (for an offline implementer)

Entirely internal to uzi; no open-web facts are load-bearing. Verified against the clone 2026-08-18 (two independent reviews).

**Agent-side (inline hooks — NOT a run kind)**
- Dispatch split: `agent/src/worker.ts:181` (`kind==="judge" ? judgeRunner : runner`). Summaries wire into the **issue** path (`agent/src/runner.ts` `execute` → `agent/src/sdk-executor.ts`).
- Hook sites: intent before `drivePlanningTurn` (`sdk-executor.ts` ~:958); plan at `gatePlan`/awaiting_approval (~:983) and the revise loop (~:985). Seeded skip: `preApproved` (~:864).
- Reused mechanics: `runModel`/`consumeModel`, `buildSdkEnv`, `JUDGE_MODEL_TIMEOUT_MS` (`agent/src/judge-runner.ts:272-359`), `extractJsonObject` (`:649`, tolerates a ```json fence), SDK mock `stubJudgeQueryFn` (`agent/src/judge-runner-stub.ts`). Each summary turn needs its **own** `homeDir`.

**Lifecycle / persistence (api)**
- Enqueue (`queued`): `Service.CreateRun` (`api/internal/workersvc/service.go`) ← `StartRunForUser` ← `Handler.CreateRun` (`api/internal/handler/workers.go`). Plan produced (`→awaiting_approval`, writes `plan_md`): `Queries.SetRunAwaitingApproval` (`store/runtime.sql.go`). Approval: `Queries.CreateApprovePlanInput` via `submitApproval` ← `Handler.CreateRunInput` (kind `approve_plan`).
- Worker POST precedent: `api/internal/handler/worker_findings.go` `WorkerCreateFinding` (auth `mw.WorkerFromContext`; guards run-owner / non-terminal / has-repo / bounded text) and `api/internal/handler/judge_worker.go`. `runToDTO` lives in `workers.go`.
- Live update: `web/src/pages/RunView.tsx` `useRunStream(id)` over `/api/ws?run=`; client refetches the run DTO on state/health frames (`web/src/lib/api.ts`). Persist must emit a frame.

**Data**
- `runs` defined in `00020_workers_runs.sql`; head migration `00130_run_priority.sql` (numbering has a gap at 76; head is 130). `store.Run` (`api/internal/store/models.go`). Existing columns of interest: `PlanMd`, `PlanSource`, `IssueTitle`, `IssueDescription`, `ReportMd`, milestones_*. DTO: `RunDTO` (`api/internal/apitypes/run.go`) built in `runToDTO`; list variant `RunListItemDTO`; slim board DTO `latestRunDTO` (`api/internal/handler/board.go`) — leave alone.

**Model / settings**
- Claim assembly: issue runs → `assembleClaim` (`service.go:1029`, via `Service.Claim`); judge runs → `assembleJudgeClaim` (`judge.go`). `judge_model` is judge-claim-only (`claim.go:131`). Add `summary_model` to `assembleClaim` + `agent/src/protocol.ts`/`claim.go`.
- Settings: `app_settings` KV (`00036_app_settings.sql`); consts/defaults/accessors in `api/internal/settings/settings.go` (`KeyJudgeModel`, `DefaultJudgeModel="opus"`, key→default map, `Cache.JudgeModel`); handlers `api/internal/handler/settings.go` `GetSettings`/`UpdateSettings` (admin-only, `Validate`/`ValidateMerged`). Admin UI: `web/src/pages/AdminSettings.tsx` (Judge model is a free-text `Input maxLength={100}`). Per-user judge model: `00125_user_judge_model.sql` (`users.judge_model`), `GetUserJudgeModel`. Judge spend guards precedent: `judge_cooldown_seconds`/`judge_daily_budget` (`settings.go:71-72`). Validation: Go `api/internal/agenttmpl/model.go` `ValidateModel` (+`MaxModelLen=100`); TS `agent/src/models.ts` `isValidModel`. Aliases: `web/src/components/ModelSelect.tsx` `MODEL_ALIASES`.

**Web / CLI consumers**
- Detail: `web/src/pages/RunView.tsx`. List: `web/src/pages/RunsList.tsx` (renders only `run.issue_title` today, ~:252). Shared type: `web/src/lib/api.ts` `interface Run`. localStorage prefs: `web/src/lib/prefs.ts` (generic; no per-run/GC pattern — add one). CLI: `api/cmd/uzi/run.go` `run get` → `renderRunDetail` (routes text through `cellText`; does not print `plan_md` today). CLI render tests: `api/cmd/uzi/render_test.go`.

**Summary inputs**
- Intent: `runs.issue_title` + `runs.issue_description` (snapshotted at queue time) + PRD text resolved worker-side from the clone (see M3b — **not** in the DB; `runs.prd_done_path` is an archive path, not content). Plan: `runs.plan_md` (+ `plan_source`); revise feedback on `run_user_inputs.body` for `revise_plan`/`reject_plan`. PRD link detector reference: `prdLinkRe` (`api/internal/forgesvc/service.go:82`); path guard reference: `api/internal/prdpath/prdpath.go` `Validate`.
