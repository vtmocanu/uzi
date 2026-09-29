# PRD #1908: Repo-less jobs: a `job` run kind and a versioned jobs API

**Issue**: #1908
**Status**: Draft
**Priority**: High
**Depends on**: PRD #1907 (`/api/v1` authentication via `RequireV1Caller`, the OpenAPI spec file, the route/spec parity check, product tokens and the `products` table)
**Related**: PRD #1906 (egress site lists and fetch service: its job-integration milestone gives `research` jobs web access), PRD #1909 (job files, source log and product skill sets), PRD #1910 (connect flow), PRD #46 (judge: the repo-less run-lane precedent), PRD #400 (handoff tasks and their structured review findings), PRD #529 (ephemeral run-bound workers), PRD #1429 (atomic harness resolution at run create)

## Problem

External products and users need to run agent jobs that are not tied to a forge repo, and read structured results over a stable API. Example: "research this question and return a report with findings", with the inputs supplied in the request rather than in a git branch.

Today every way to start agent work in uzi either needs a repo or is not a general job:

- `issue`, `prompt`, `task`, `self_improve`, `ci_fix` and `mr_rework` runs all require `repo_id` (the `runs_kind_shape` CHECK).
- `chat` is repo-less but conversational, on its own claim lane, and never claimed by an ephemeral worker.
- `judge` is repo-less but reviews another run's trace; it is not a caller-defined job.
- There is no stable, versioned API for jobs: PRD #1907 introduces `/api/v1` and its compatibility contract, but no job endpoint.
- A task run's report text is dropped at completion (only `issue` runs may store `report_md`), so there is no place to return a job's result.

## Current state (verified on `main` at f1249fe4, 2026-09-29)

- **Run kinds are a closed set of eight**, pinned in three places that tests keep in sync:
  - `api/internal/runkind/runkind.go:28-35` (constants) and `:39-40` (`All()`), checked against the DB by `runkind_migration_test.go`;
  - DB `runs_kind_check`, last redefined in `api/internal/store/migrations/00167_run_mr_rework_kind.sql:17-19`;
  - agent mirror `agent/src/protocol.ts:279` (`RUN_KINDS`, parity test `agent/test/run-kind-db-parity.test.ts`) and web mirror `web/src/lib/runKind.ts:4` (`runKindContract.test.ts`).
- **Per-kind shape** is `runs_kind_shape` (`00167_run_mr_rework_kind.sql:21-30`). `chat` and `judge` are the repo-less precedents (`repo_id IS NULL AND issue_iid IS NULL AND branch IS NULL`).
- **Judge is the right precedent for a repo-less job, not chat.**
  - A judge run rides the normal run lane (`ClaimRun`, `api/internal/store/queries/runtime.sql:760`), which excludes only `chat` (`:799`).
  - `assembleClaim` forks judge before the repo/forge context and before any PAT is decrypted (`api/internal/workersvc/claim_assembly.go:178-189`); its claim carries only the model credential.
  - The worker routes it to a slim runner by kind (`agent/src/worker.ts:720-724` to `agent/src/judge-runner.ts`), with no clone, no git and no MR.
  - Chat instead has a dedicated lane (`ClaimChatRun`, `api/internal/store/queries/chat.sql:71`) that an ephemeral worker never claims (`chat.sql:87-89`).
- **Docker-tier workers fail closed on repo-less kinds other than judge.** `fn_worker_can_claim` (`api/internal/store/migrations/00151_effective_worker_caps_fold.sql:47-49`) lets a docker worker claim a run only if the run is a repo-less judge or its repo is on the docker allowlist. A new repo-less kind is therefore never claimable by a docker-tier (open-egress) worker unless this function is changed.
- **Ephemeral provisioning** triggers on queued non-chat runs with a non-empty `required_capabilities` that no online persistent worker satisfies (`ListUnplaceableQueuedRunsForEphemeral`, `runtime.sql:6797`, predicates at `:6844-6845`), plus a saturation sibling (`:6863`). A run bound to an ephemeral worker is the only run that worker may claim (`runtime.sql:999`).
- **Harness resolution is atomic at create.** Every activated non-chat origin inserts through `createRunResolved` (`api/internal/workersvc/harness_create.go:128`), which resolves the harness inside the insert transaction (`resolveRunHarnessQ`, `harness_resolver.go:215`). An explicit harness pin that the user has no enabled credential for is refused with a typed error (`explicitHarnessRefusal`, `harness_resolver.go:245-247`), never downgraded.
- **Usage accounting is kind-agnostic.** Every delivered message batch folds result frames into `run_usage` (`api/internal/workersvc/usage_fold.go:509`), per SDK leg; the fold excludes only `chat` (`usage_fold.go:567-574`), so a new work kind folds by default.
- **Report text is issue-only.** `clampWireReportOnly` drops `report_only` for any non-issue run and `clampWireReportMd` stores `report_md` only when `report_only` survived (`api/internal/workersvc/report_only.go:19-28`, `:40-58`), so the column's invariant is "non-NULL only on a report-only issue run".
- **Structured findings already exist for task reviews:** `task_reviews` and `task_review_findings` (`api/internal/store/migrations/00135_task_review.sql:36`, `:57`), with a closed severity enum `info | warning | error`, and a worker POST `/api/worker/runs/{id}/task-review` (`api/internal/handler/handler.go:1204`) that enum-validates, caps, control-strips and secret-scrubs the text.
- **Worker routes that deliver third-party content or act on a forge** are reachable by any claimed run today: publish (`handler.go:1154`), memory (`:1160-1161`), forge issues, MRs and pipelines (`:1168-1173`), MR thread reply and resolve (`:1181-1182`), trace and review (`:1197-1198`), task review (`:1204`).
- **File tools are path-guarded by a hook** (`buildPathGuardHook`, `agent/src/guardrails.ts:1568`, used by chat at `agent/src/chat-executor.ts:268`). ADR-1719 records that this is a tool policy, not whole-process confinement.
- **Display-string validation exists:** `termsafe.Validate` (`api/internal/termsafe/termsafe.go:247`) rejects terminal control, ANSI and bidi characters; CLI token names already go through it.
- **Cancel** is a run input of kind `cancel` (`POST /api/runs/{id}/inputs`, `handler.go:890`; CLI `api/cmd/uzi/run.go:25`).
- **Naming:** `uzi task` is already an alias of `uzi handoff` (`docs/handoff.md`), so the new CLI verb cannot be `task`.
- **The pinned-binary wrapper pattern** for gate tools is `scripts/golangci-lint.sh` (version as an argument, per-arch sha256-verified release archive, retried download).

## Decisions

1. **One new run kind, `job`, plus a `job_type` column.** `job_type` is a closed enum with one value today, `research`. A later type (for example a code-review type) adds an enum value, not a kind, a lane or a migration of the kind domain. Shape: `kind = 'job' AND issue_iid IS NULL AND branch IS NULL AND job_type IS NOT NULL`; `repo_id` is **allowed but NULL for every type this PRD ships** (the column exists so a repo-bound type needs no shape change). `job_type IS NULL` for every other kind.
2. **Jobs ride the run lane, like judge.** No new claim lane. `assembleClaim` forks `job` exactly where it forks judge, before `GetRunClaimContext` and any PAT decrypt, and assembles a repo-less payload: model credential, prompt, inline inputs, job type. `ClaimRun` needs no kind change (it already excludes only `chat`).
3. **Docker-tier workers never claim jobs.** `fn_worker_can_claim` is left unchanged, so its existing fail-closed rule keeps `job` off open-egress docker workers. This is deliberate, stated in the migration comment, and pinned by a live-DB test.
4. **Jobs reach users without a persistent worker through ephemeral provisioning.** Every job is created with `required_capabilities = {'job_v1'}`, advertised by workers whose agent implements the job runner. That reuses the capability-gap trigger unchanged: a user with no capable online worker gets a run-bound ephemeral worker (if they opted in); older workers never claim a job they cannot run. PRD #1906 adds its own server-owned lane placement for jobs that use a site list.
5. **No web or network tools in C1, and a closed tool set.** The job runner offers only `Read`, `Write`, `Glob`, `Grep` rooted at a per-run job workspace directory (created at claim, removed at terminal) through `buildPathGuardHook`, plus a `submit_job_result` tool: no `Bash`, no `WebSearch`/`WebFetch`, no forge or memory tools, no subagents, `settingSources: []`, and the existing guardrail hook. The worker secret mount and every path outside the workspace are denied. A `research` job in C1 therefore works only from its supplied inputs. C1 rejects an `egress_profile` field with 422 `not_supported`; PRD #1906's job-integration milestone lifts that and adds the fetch tool. Reason: C1 must never be a way to reach arbitrary sites or third-party content, whatever tier the worker runs on.
6. **The api refuses third-party-content and forge routes for job runs.** The worker routes listed in Current state (publish, memory, forge issues/MRs/pipelines, MR threads, trace, review, task review) return 403 `not_for_job` when the path's run is a `job`. This is a server-side check keyed on `runs.kind`, so the agent tool set is not the only barrier. A later job type that needs one of these routes opts in explicitly per type.
7. **Claude harness only, pinned at create.** A job is created through `createRunResolved` with an **explicit** Claude harness, so the harness is frozen in the insert transaction and a user without an enabled Anthropic credential gets the resolver's typed refusal, mapped to 422 `no_model_credential`. There is no create-versus-claim ambiguity: the run row carries `harness = claude`. The job runs as the token's user on **that user's own model credential** configured in uzi. Codex's web search runs server-side and is out of scope for research.
8. **Results live in two new tables, not in `report_md`.** `job_results` (one row per job run: `report_md`, `status`, timestamps) and `job_findings` (`severity` in the existing `info | warning | error` enum, `message_md`, optional location as either `url` or `file` + `line`). Both mirror `task_reviews` / `task_review_findings`, including the ingest discipline (enum-validate, cap, control-strip, secret-scrub). `report_md`'s issue-only invariant stays intact.
9. **Inputs and job metadata.** The prompt is stored in `issue_description` (the `task`/`prompt` precedent) under the same byte cap; inline text inputs go in a `job_inputs` table (`name`, `content_md`, bounded count and total bytes). A `job_origins` row per job records who created it: `product_id` and `product_token_id` (NULL for a `uzc_` caller) and `requested_by_label`, an **untrusted end-user audit label** supplied by a product (capped at 200 bytes, `termsafe.Validate`d, stored and displayed as attribution only, never used for any authorization decision). PRD #1909 adds file inputs.
10. **API: `/api/v1/jobs`, behind PRD #1907's `RequireV1Caller`.** `POST /api/v1/jobs`, `GET /api/v1/jobs`, `GET /api/v1/jobs/{id}`, `GET /api/v1/jobs/{id}/result`, `GET /api/v1/jobs/{id}/messages?after=<seq>` (a narrowed progress feed over `run_messages`: seq, time, type, text), `POST /api/v1/jobs/{id}/cancel`.
    - Callers are `uzc_` user tokens and `uzp_` product tokens only (B1's rule); `uza_` tokens and cookies are refused on `/api/v1`. `RequireV1Caller` clears admin authority, so every v1 read is owner-scoped; admin views of job runs stay on the existing non-v1 run routes.
    - Scopes (B1): create and cancel need `jobs:run`; reads need `jobs:read`.
    - **Visibility:** a `uzc_` caller sees all of its user's jobs. A `uzp_` caller sees only the jobs created through **its own product** for that user, so one product cannot read another product's results.
    - **Per-product job types:** C1 adds `products.allowed_job_types` (a `text[]` of known job types, empty by default) and enforces it at create for a `uzp_` caller: a type not in the list is 403 `job_type_not_allowed`, and an empty list allows nothing (fail-closed). A `uzc_` caller may create any job type.
    - The public status is a small mapping of run status: `queued`, `running`, `waiting` (limit or recovery waits), `completed`, `failed`, `cancelled`.
    - C1 extends PRD #1907's OpenAPI document with these endpoints and a contract test that fails when a response drifts from it.
11. **Breaking-change gate.** C1 adds `check:api-v1-compat` to `gate:repo`: a pinned OpenAPI diff tool (oasdiff) acquired by a `scripts/oasdiff.sh` wrapper following the `scripts/golangci-lint.sh` pattern (version as an argument, sha256-verified release archive), run against the spec on `origin/main`, failing on any breaking change. No in-repo diff implementation, and not in `devbox.json` (that file is worker config).
12. **No plan gate.** A job is created `queued` and runs to completion with no approval step; it never enters `awaiting_approval`.
13. **Bounded spend:** a per-user create rate limit (the existing per-user limiter) and a cap on non-terminal jobs per user (default 10, admin-tunable), returning 429 over the cap. Timeouts use the existing sweeper and `RUN_TIMEOUT`, with an optional per-job wall clock clamped to the existing 8h ceiling.
14. **A revoked or disabled product stops its jobs.** A sweeper pass cancels (through the existing cancel input) every non-terminal job whose `product_token_id` is **explicitly revoked** (single revoke, "Revoke all", or its OAuth grant revoked), whose product is disabled, or whose owner is deactivated. **Ordinary token expiry does not cancel a job**: an expired token only stops new API calls, and a job authorized while its token was valid runs to completion (PRD #1910 issues one-hour access tokens, and jobs can run longer). A pass, not a hook in B1's revoke handlers, so every revoke path is covered by one idempotent rule; a test pins that an expired-but-not-revoked token leaves its running job alone. Jobs a user created with `uzc_` are unaffected.
15. **Display strings are validated at write time.** The job title, input names, `requested_by_label` and finding text go through `termsafe.Validate` (or the existing control-strip for markdown bodies) plus byte caps, and the CLI renders them plain, so a job's text cannot rewrite a terminal or fake a row.
16. **Visible in the product:** job runs appear in the web runs list and run detail with a `job` kind label and a result panel (report + findings); `uzi job create|get|result|cancel|list` mirrors the API. `uzi task` is taken, so the verb is `job`.

## Out of scope

- Any code-review job type, repo-bound jobs, PR diff input, or posting to a forge.
- Web access for jobs, site lists, the fetch service and the per-product site-list allowance (PRD #1906, its job-integration milestone).
- Product tokens, product registration, `/api/v1` authentication and the compatibility ADR (PRD #1907); the OAuth connect flow (PRD #1910).
- File inputs, downloaded files, the source log, retention rules and product skill sets (PRD #1909).
- Callbacks or webhooks to the caller; steering a job mid-run; Codex-harness jobs; judging job runs.

## Milestones

Gate lines: `task gate:api` (live-DB tests run, not skipped), `task gate:agent`, `task gate:web`; `task gate:repo` for the migration and the compat gate. No milestone creates or edits anything under `.github/workflows/`.

- [ ] **M1: Schema.** Migration (draft number, renumbered at merge): widen `runs_kind_check` with `job`; add `runs.job_type` (CHECK: non-NULL iff `kind = 'job'`, enum `research`); extend `runs_kind_shape` with the `job` clause; create `job_inputs`, `job_origins`, `job_results`, `job_findings`; add `products.allowed_job_types`. Update `runkind.go`, the agent and web mirrors, and their parity tests; grep every `kind IN (` / `kind NOT IN (` site and record per site whether `job` belongs. Live-DB tests: a repo-less `job` row inserts; a `job` row with an issue or branch is rejected; `job_type` on a non-job kind is rejected; `fn_worker_can_claim` returns false for a docker worker and a `job` run. Gate: `task gate:api`, `task gate:agent`, `task gate:web`, `task gate:repo`.
- [ ] **M2: Create and read in the service layer.** `workersvc.CreateJobRun` through `createRunResolved` with an explicit Claude pin (owner, caps, NUL-strip, termsafe on display strings, per-user non-terminal cap, `required_capabilities = {'job_v1'}`, `job_origins` row, per-product `allowed_job_types` check), the status mapping, result and inputs reads with the per-product visibility rule, cancel through the existing cancel input. Live-DB tests for each refusal (over cap, over size, no Anthropic credential, unknown type, type not allowed for the product, empty allow-list) and for owner-only and same-product-only reads. Gate: `task gate:api`.
- [ ] **M3: Claim assembly and route refusals.** `assembleClaim` forks `job` before `GetRunClaimContext` (mirroring judge); the payload carries the model credential, prompt, inputs and job type, and no PAT or repo. The worker routes of Decision 6 refuse a `job` run. Live-DB tests: a capable worker claims a job; a worker without `job_v1` does not; an ephemeral worker bound to a job claims it; a docker worker never does; no custody hold is opened; each refused route returns 403 for a job run and is unchanged for an issue run, with a mutation run showing the test catches the guard's removal. Gate: `task gate:api`.
- [ ] **M4: Worker job runner.** `agent/src/job-runner.ts`, routed by kind in `worker.ts` next to the judge branch; advertises `job_v1`. Creates the per-run job workspace, writes inputs into it, runs one SDK session with the tool set of Decision 5, streams messages through the normal run message path (so usage folds into `run_usage` per leg), and posts the result to a new worker endpoint `POST /api/worker/runs/{id}/job-result` (idempotent upsert, ingest discipline of Decision 8) before reporting `completed`. Removes the workspace at terminal. A run that ends without a submitted result is reported `failed` with a clear reason. Tests: the effective tool list (not just the requested one) has no Bash, Web*, forge, memory or subagent tool; a path outside the workspace or into the secret mount is denied; result schema validation; limit-death handling. Gate: `task gate:agent`, `task gate:api`.
- [ ] **M5: `/api/v1/jobs` endpoints, spec and compat gate.** Mount the routes of Decision 10 behind `RequireV1Caller` and B1's scope check; extend the OpenAPI document; add the contract test (every endpoint's success and error shapes validate against the spec); add `scripts/oasdiff.sh` and `check:api-v1-compat` in `gate:repo`, with a canary proving it reddens on a removed field. Handler tests for 401 (cookie, `uza_`), 403 (missing scope, type not allowed), 404 (another user's job, another product's job), 422, 429. Gate: `task gate:api`, `task gate:repo`.
- [ ] **M6: Product-revoke sweep.** The sweeper pass of Decision 14. Live-DB tests: revoking the creating token, disabling the product, and "Revoke all" each cancel that product's queued and running jobs on the next pass; a `uzc_`-created job is untouched; a re-run of the pass is a no-op. Gate: `task gate:api`.
- [ ] **M7: CLI and web.** `uzi job create|get|result|cancel|list` over `/api/v1/jobs`, with `--input name=@file` and `--json`, rendering job text plain. Web: job runs listed with a `job` label; the run detail shows the report and findings panel (findings rendered as inert text, URL locations as plain links with `rel="noopener noreferrer"`) and the untrusted `requested_by_label` marked as reported by the product. Gate: `task gate:api`, `task gate:web`.
- [ ] **M8: Docs.** New `docs/jobs.md` (audience `user`: what a job is, the API, the CLI, limits, what C1 does not do yet); `docs/cli.md` section; `ARCHITECTURE.md` run-lifecycle line for the `job` kind; `task docs:sync`. Gate: `task check-docs:web`, `task gate:api`.
- [ ] **M9: Hosted k8s acceptance** (maintainer-owned; part of completion, the PRD is not done without it). On a hosted cluster with no persistent worker: `uzi job create` provisions an ephemeral worker, the job completes, `uzi job result` returns the report and findings; a `uzp_` token for a product whose allow-list lacks the type gets 403; a docker-tier worker leaves the job queued; disabling the product cancels its running job.

## Dependency plan

- **Phase:** C1 is phase 2. It needs PRD #1907 merged (middleware, spec file, `products` table). PRD #1906's job-integration milestone, PRD #1909, and PRD #1910's live acceptance come after C1 (phase 3).
- Inside C1: M1 blocks everything. M2 and M3 can proceed in parallel after M1; M4 needs M3's payload shape; M5 needs M2; M6 needs M2; M7 needs M5; M8 last; M9 after deploy.
- When PRD #1906's job-integration milestone lands, it lifts the `egress_profile` 422, adds the fetch tool and the per-product site-list allowance, and moves site-list jobs to its lane.

## Success criteria

- A user with only a `uzc_` token and no persistent worker can create a `research` job over `/api/v1/jobs`, see it progress, and read a structured result.
- A `uzp_` token can create only its product's allowed job types and read only its own product's jobs.
- No job is ever claimed by a docker-tier worker, no job run can reach the network or a forge through an agent tool, and the api refuses third-party-content routes for job runs.
- The eight existing kinds behave exactly as before (existing live-DB and agent suites pass unchanged).
- Every `/api/v1/jobs` response validates against the committed spec, and a breaking spec change reddens `gate:repo`.
- Hosted k8s acceptance (M9) passed.

## Risks

- **Kind-domain churn.** Adding a kind touches the DB CHECK, three mirrors and their parity tests, plus kind lists hard-coded in SQL (for example the Codex account gate at `runtime.sql:967` and its peer mirror at `:1086` enumerate the repo-bound kinds). Mitigation: M1's per-site audit; a live-DB test covers the claim path end to end.
- **Sweeper and recovery paths assume a repo for non-judge kinds.** Custody holds, checkpoints and publish paths are repo-oriented. Mitigation: `job` produces no branch and no checkpoint; M3 and M4 tests assert that no custody hold is opened for a job claim (as for judge), and failure paths are exercised.
- **Prompt injection through inputs.** Inputs are untrusted. Mitigation: closed tool set, workspace-rooted file tools, server-side route refusals, guardrail hook, `settingSources: []`, and inputs fenced as data in the prompt.
- **Spend.** A caller can queue many jobs on a user's model credential. Mitigation: the per-user non-terminal cap and create rate limit (Decision 13), and the revoke sweep (Decision 14).
- **Compat tool acquisition.** The wrapper downloads a release archive like `scripts/golangci-lint.sh`; it works wherever that wrapper already works.

## Decision Log

- 2026-09-29: Drafted. Chose the judge precedent (run lane, repo-less fork in `assembleClaim`) over a chat-style dedicated lane, because it reuses ephemeral provisioning and the docker fail-closed rule unchanged. Chose `job` + `job_type` over one kind per job type so later types need no kind migration.
- 2026-09-29: Revised after cross-PRD review and security review.
  - C1 now depends on PRD #1907: it uses `RequireV1Caller`, B1's scopes and B1's spec file, and drops its own Bearer-only auth variant and the "whichever merges first" spec rule. `/api/v1` accepts `uzc_` and `uzp_` only; `uza_` is refused, and admin views stay on non-v1 routes.
  - C1 owns per-product `allowed_job_types` (moved from B1, fail-closed when empty) and the untrusted `requested_by_label`. The per-product site-list allowance lives in PRD #1906's job-integration milestone, which is also what lifts the `egress_profile` 422, so no window exists where a product token uses a site list without that check.
  - Added: server-side refusal of third-party-content worker routes for job runs; workspace-rooted file tools with the secret mount denied; same-product-only visibility for `uzp_` callers; a sweeper pass that cancels a revoked or disabled product's jobs; termsafe validation of display strings; the OpenAPI breaking-change gate via a pinned-tool wrapper.
  - Resolved open questions from the code: the harness is pinned to Claude explicitly at create through `createRunResolved`, so the check is at create and frozen on the row; usage folds into `run_usage` through the normal message path, which excludes only `chat`.
  - Hosted k8s acceptance (M9) is part of completion.
