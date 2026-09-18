# PRD #1429: Atomic Codex public activation (#1106 M5B)

**Issue**: [#1429](https://github.com/vtmocanu/uzi/issues/1429)  
**Parent**: [#1106](1106-codex-harness-phase1.md), M5B only  
**Depends on**: completed M5A [#1332](done/1332-codex-routing-foundation.md), plus the remaining #1247 work in PR #1427 merged and green before implementation starts  
**Priority**: High  
**Status**: Planned. This child is one atomic public-enablement change; it must not be split into separately mergeable backend and frontend PRs.  
**Area**: `api/internal/workersvc`, `api/internal/handler`, `api/internal/apitypes`, `api/internal/store`, `api/internal/schedsvc`, `api/cmd/uzi`, `agent/src`, `agent/test`, `web/src`, `fixtures/api-contract`, `e2e/`

## Problem

Parent #1106 has completed the dark Codex foundation through M5A. The database can represent a run's harness and semantic cost status, the worker can prove `codex_harness_v1`, the D11 resolver is implemented and exhaustively tested, and internal Codex rows can execute safely. None of that is publicly reachable: every production origin still creates Claude, public create and schedule requests reject `harness` as an unknown field, the D11 resolver has no production caller, and public usage readers can still collapse subscription or unreported cost into a misleading numeric zero.

That dark boundary was deliberate. Enabling only the server would expose Codex to direct API clients without the controls and status surfaces users need. Enabling only the web or CLI would send a contract the server does not support. M5B must therefore land routing, public contracts, legibility and offline end-to-end proof together.

## Outcome

Users with one usable harness keep the simple path: no harness picker is shown and new runs resolve to the sole available harness. Users with both see a Run Defaults choice, a Start choice and a schedule choice. Every new run freezes the resolved harness; a Codex run also freezes the chosen named Codex credential and auth mode in the same transaction. Explicit selections and pinned schedules never fall back to the other harness.

The actual harness is visible on run detail/list, CLI/TUI and schedule surfaces. Claude remains visually unmarked where a badge would add no information; Codex is explicit. Model and effort controls use the selected harness's curated vocabulary. Usage surfaces distinguish `metered`, `subscription` and `unreported`; they never present a partial or unknown dollar sum as complete `$0`.

## Scope boundaries

In scope:

- Public harness selection and D11 resolution for manual issue starts, chat `start_run`, schedule fire, autopilot/sweep, prompt, self-improve, CI-fix, MR-rework, judge, handoff task, task review and then-fix creation. The Chat conversation run itself is an explicit Claude-only exception.
- Harness-authoritative claim assembly: a Codex run bypasses the Anthropic credential ladder entirely, while judge and task-review gain a real Codex advice-harness path.
- An own-user `default_harness` setting through the existing settings API and the web Run Defaults page. There is deliberately no CLI setter for the user default.
- Public request/response DTOs, web types, mock scenarios, CLI/TUI rendering and truthful cost-status/count presentation.
- One offline Codex-selected compose e2e phase using dummy credentials, the existing fake forge and stub executor. It proves routing, claim, completion and visible harness/cost semantics without a real provider call.

Out of scope:

- Parent M6: chart egress changes, worker-image rollout, dev-cluster live verification and the deferred real OpenAI API-key repeat.
- Parent M7: the full user-doc corpus, `docs:sync`, `ARCHITECTURE.md` repoint and #229 milestone rewrite.
- Changing the harness of an existing run. PRD #1247 switches only an Anthropic credential within a Claude run; a Claude-to-Codex handoff remains a later PRD.
- Running the Chat conversation lane itself on Codex. This child covers the Chat surface's `start_run` action, which creates an ordinary issue run.
- Account rate-limit meters from #1209, OpenAI-compatible custom endpoints, local models, device-code login or a per-harness worker fleet.
- Any `.github/workflows/**` edit in implementation or validation.

## Fixed contracts

### D1: one resolver, atomic run creation and binding

`workersvc.resolveRunHarness` is the only harness-selection policy. A creation path supplies an optional explicit harness and receives `{Harness, CodexChoice}`. Its D11 order stays unchanged:

1. An explicit request or pinned schedule harness is authoritative; unavailable returns `no_credential_for_harness` and never falls back.
2. Otherwise use a usable `users.default_harness`.
3. Otherwise use the sole usable harness.
4. Otherwise choose Claude when both are usable and no usable default is set.
5. Otherwise retain the existing no-credential refusal.

For every origin, resolution, the run INSERT and any Codex binding freeze are one database transaction. No committed `harness='codex'` row may exist without the matching immutable `codex_secret_id`, auth mode, material revision and account identity required by M1. A resolver or freeze failure rolls the INSERT back. `FreezeCodexBinding` remains an internal service/store primitive and gains no public route.

Use the existing workersvc `txBeginner` wired by `SetTxBeginner`: begin the transaction there, derive a transaction-bound `*store.Queries` with `WithTx`, re-read credential/default facts through it, INSERT through it, freeze through it and only then commit. Refactor the freeze path to accept the transaction-bound query surface rather than reaching back to `s.q`. If `txBeginner` is absent, Codex creation fails closed; it must never fall back to a non-atomic insert. Claude keeps the current cheap path when no atomic Codex freeze is needed.

The transaction helper must be usable by every activated run INSERT family rather than reimplementing D11 in each creator. External forge reads may happen before the transaction; the credential/default facts are read again inside the transaction that commits the run.

M1 also parameterizes every activated INSERT with `harness` and performs the one sqlc regeneration for the branch. That includes runtime, schedule, prompt, self-improve, CI-fix, MR-rework, judge and all task queries; only the two Chat conversation INSERTs retain a literal. This freezes the generated surface before parallel work. During Phase 2, M2 is the sole unit allowed to correct `api/internal/store/queries/**` or regenerated store code; M3-M5 consume the M1 surface.

### D2: public contract and typed failures

The closed public enum is `claude|codex` everywhere and the field name is `harness`.

- Manual issue creation and Chat `start_run` accept optional `harness`; absent means D11 implicit resolution.
- Schedule create/edit accepts nullable `harness`. A non-null value is an explicit pin resolved at fire time; null resolves from the then-current user default and available credentials.
- `RunDTO`, run-list rows, schedule DTOs, API-contract fixtures and `web/src/lib/apiTypes.ts` expose the actual/stored harness.
- The CLI gains `uzi run create --harness claude|codex`, schedule create/edit parity, and harness on `run get`/`run list`; it does not gain a user-default setter.
- An invalid enum is 400. An explicit valid harness without a usable credential is 422 with a stable `no_credential_for_harness` classification. Neither case creates a run or performs a forge write after the invalid fact is known.

### D3: defaults, availability and deletion

`GET/PUT /api/me/settings` adds nullable `default_harness` with the endpoint's existing absent/null/value patch semantics. The web writes it from Run Defaults. The stored preference is not rewritten when a credential is deleted. A stale preference falls through D11 for implicit creation, while an explicit run or pinned schedule selection fails.

Availability is not a raw credential count:

- Claude is usable when the user has an Anthropic credential under the existing server rule.
- A Codex subscription is usable only when its named default is linked to a verified provider account.
- An OpenAI API key is usable by existence when it is the one Codex default.
- A failed or missing default subscription never silently spends a non-default paid API key.

The server is authoritative. Web helpers may explain or hide controls, but a direct API request receives the same result.

### D4: every origin resolves, follow-on runs stay new runs

The joined acceptance inventory must name every production INSERT family and show how it gets its harness. Manual and schedule surfaces may carry an explicit selection. Background/internal origins use implicit D11 unless a stored schedule pin applies.

Derived runs with a target/source run, including judge, MR-rework, task review and then-fix, inherit the source run's harness as an explicit selection. They are fresh runs, not session resumes, but they must not silently alternate harnesses on one branch or review chain. If that harness is no longer usable, creation fails with the same explicit-harness classification; it does not fall back. CI-fix has no reliable target-run relationship and uses ordinary implicit D11.

Failure must also be visible: a manual derived-run request returns the typed error; schedule fire records the existing `last_fire` outcome; automatic MR-rework and then-fix record a static nonsecret skip/failure on their existing poller or source-run feed; best-effort judge enqueue logs the harness-specific skip. “No derived run created” without a visible reason is not accepted.

`CreateChatRun` and `CreateChatContinueRun` remain literal Claude exceptions because the Chat conversation executor has no Codex implementation. The Chat surface's `start_run` action creates an ordinary issue run and does use D11. Update the stale `chat.go` comment that currently says Chat Codex arrives in M5B.

Claim assembly branches on `runs.harness` before any credential ladder. For `harness='codex'`, it never calls `claimSecretID`, `openWithAutoRetry` or `recordRunCredential`, never populates `anthropic_oauth_token`, and never writes a `run_credential_epochs` Anthropic row. A Codex-only user must claim successfully through full assembly; a both-credential Codex run must carry no Anthropic secret or attribution.

The judge lane has its own `assembleJudgeClaim` fork and needs the same rule inside `judge.go`: branch on `runs.harness` before `judgeChoice`, build `Secrets.Codex` through the existing `codexClaimSecrets` authority path, and write no Anthropic epoch for Codex. Task-review uses ordinary `assembleClaim` and is covered by the shared repair.

Placement remains fail closed through M5A's `codex_harness_v1` claimant, peer-spread and fleet-count gates. A valid Codex run may queue with `no Codex-capable worker is online`; it never falls back to Claude. Claude claims remain byte-compatible.

### D5: PRD #1247 compatibility

PR #1427 must be merged before this child starts. Its schedule, web, CLI/TUI, usage-attribution and documentation changes are the baseline, not changes to overwrite.

The per-run `credential_override` remains Anthropic-only. A request combining an effective Codex harness with an Anthropic override returns the existing 422 and writes nothing. `run set-token` and `approve --token` keep the harness fixed. Harness selection must not weaken `claim_generation`, credential-switch fencing, epoch attribution or the held-state protocol. M5B adds no cross-harness resume path.

Replace #1247's raw `createRunEffectiveHarness` and `scheduleEffectiveHarness` checks with the one D11 result computed inside the creation transaction. Feed that exact resolved harness to `ResolveCredentialOverride`; do not infer it from `users.default_harness`. Thus an explicit Claude request with a Codex default accepts a valid Anthropic override, while an unusable Codex default that falls through to Claude also accepts it.

A null-harness schedule may carry a stored Anthropic override. At fire time, if D11 now resolves that schedule to Codex, the fire fails closed with a legible `last_fire` skip/error, creates no run and does not fall back to Claude or discard the override. A pinned Claude schedule keeps using its validated override.

### D6: model and effort vocabulary is harness-owned

The product-owned Codex picker remains exactly `gpt-6-astra` and `gpt-5.6-sol`; catalog discovery does not add models automatically. Claude keeps today's aliases. The effort contract remains `low|medium|high|xhigh|max`, mapped one-to-one for Codex.

The existing default model/effort storage remains shared. Widen server validation so the two closed harness vocabularies are accepted without treating the Claude alias set as universal. A UI selection is validated against the currently selected harness. When an inherited or later-resolved harness makes a stored model incompatible, the runtime uses that harness's default and implements the D9-required nonsecret fallback/status fact; it never sends a Claude alias to Codex or a Codex alias to Claude. Switching the Run Defaults harness in the UI resets an incompatible model to inherit rather than persisting a knowingly invalid pair.

### D7: cost status is part of the public truth

M5A already persists per-run `cost_status` and computes lifetime/last-seven-day `subscription_run_count` and `unreported_run_count`. M5B exposes and consumes them:

- Run Usage renders dollars only for `metered`; `subscription` is labelled as subscription usage; `unreported` shows tokens with cost unavailable.
- Dashboard and admin usage show the subscription/unreported counts beside numeric metered totals. A mixed aggregate never presents the numeric subset as the complete total.
- CLI/TUI run output follows the same rule and treats harness labels and provider/account text as untrusted terminal data.
- The public judge DTO and the prompt-side judge/review context both name the status. Neither may call subscription `$0` or omit the fact that an unreported component makes a total incomplete.

Backward-compatible numeric fields may remain on the wire, but readers must branch on the closed status/count fields rather than infer semantics from zero.

### D8: verified pricing input, resolved before offline handoff

Official OpenAI model pages were rechecked on 2026-09-17. They confirm M5A's `openai-standard-2026-09-13` table without a rate change:

| Model | Input <=272K | Cached input | Cache write | Output | Above 272K |
|---|---:|---:|---:|---:|---|
| `gpt-6-astra` | $10.00 | $1.00 | $12.50 | $50.00 | 2x input/cache rates, 1.5x output for the full request |
| `gpt-5.6-sol` | $4.00 | $0.40 | $5.00 | $20.00 | 2x input/cache rates, 1.5x output for the full request |

Sources: [GPT-6 Astra model page](https://developers.openai.com/api/docs/models/gpt-6-astra) and [GPT-5.6 Sol model page](https://developers.openai.com/api/docs/models/gpt-5.6-sol). Both state cache writes are 1.25x uncached input. The Sol page still states that promotional pricing is available at least through 2026-11-21. The Astra page explicitly applies the >272K multiplier to input and cache rates; the Sol page says 2x input and 1.5x output without separately spelling out its cached/cache-write rows. M5A already adopted the table above. Retain that versioned table and review boundary, and treat any observed unknown model/tier, irreconcilable cache evidence or post-boundary Sol use as unreported. The worker needs no open-web lookup.

### D9: atomic public enablement

All milestones land in one implementation PR. The final diff must contain the first Codex-producing public route and every required public reader/control together. Intermediate commits may be dark or incomplete inside the branch, but no partial milestone is merged or released independently. Parent M6 remains the deployment gate after this PR merges.

## Verified current anchors

- `api/internal/workersvc/harness_resolver.go` contains the pure resolver and store-backed Codex choice, with zero production callers.
- `api/internal/handler/harness_contract_test.go` currently proves manual run and schedule JSON reject `harness`; M5B replaces those dark negatives with positive contract and refusal tests.
- `api/internal/workersvc/codexauthz.go` owns the immutable binding/freeze and run-scoped authority. It remains internal.
- `api/internal/workersvc/claim_assembly.go` currently opens and records an Anthropic credential before its Codex branch. This makes a Codex-only claim fail and misattributes a both-credential Codex claim; M2 owns the harness-first repair and full-assembly regressions.
- `api/internal/workersvc/judge_read.go`, `agent/src/judge-runner.ts`, `agent/src/review-runner.ts` and `agent/src/main.ts` currently have no Codex advice selection. M3 owns that lane, including task-review.
- `api/internal/workersvc/judge_enqueue.go` currently gates on Anthropic-token presence before creating any judge. M3 replaces that with usability for the inherited target harness, so a Codex-only target can enqueue a Codex judge.
- All production INSERT families write Claude explicitly in `api/internal/store/queries/{runtime,chat,ci_fix,judge,mr_rework,schedules,selfimprove,task}.sql`.
- `api/internal/handler/user_settings.go` and `api/internal/store/queries/users.sql` read/write model and effort, while `default_harness` has only the M5A read.
- `web/src/lib/hasToken.ts` and `web/src/lib/runStream.ts` are Anthropic-only; Board, IssueView and Dashboard call them. `web/src/lib/api.ts` sends no harness in `createRun`.
- `web/src/lib/startRun.ts` from PR #1427 is the shared start dialog baseline after that PR merges. `web/src/pages/RunDefaults.tsx`, `web/src/components/ScheduleModal.tsx`, `ModelSelect.tsx` and `EffortSelect.tsx` are the other shared selection surfaces.
- Public usage shapes live in `api/internal/apitypes/usage.go` and `api/internal/handler/usage.go`; readers include `web/src/components/RunUsage.tsx`, Dashboard/admin usage, `api/cmd/uzi/run_render.go`, TUI cost/detail renderers and judge context construction.
- `e2e/run-e2e.sh` is phase-driven. The new phase belongs under `e2e/phases/` and must use a throwaway project/container namespace already owned by that harness.

## Milestones and dependency graph

All work is in the uzi repository. Phase 2 units may run in parallel only after M1 freezes the contract. Their file ownership is intentionally disjoint. M6 joins the complete tree and is sequential.

| Phase | Milestone | Depends on | Primary files/ownership | Repo |
|---|---|---|---|---|
| 1 | M1: atomic resolver/binding and complete public contract | PR #1427 merged; M5A | harness resolver/create transaction helper, every INSERT parametrization plus one sqlc regeneration, every public DTO shape including usage, user settings query/handler, contract fixtures and `web/src/lib/apiTypes.ts` | uzi |
| 2, parallel | M2: ordinary server origins, claim assembly and schedule fire | M1 | issue/chat-start/schedules/prompt/self-improve/CI-fix/MR-rework creators, claim assembly, schedsvc; sole Phase-2 owner of any store-query/generated correction; literal-Claude Chat exceptions | uzi |
| 2, parallel | M3: Codex advice lanes, derived task/judge origins and judge-cost context | M1 | judge/task creators, `judge_read.go`, judge handlers, agent judge/review/main advice selection, agent tests | uzi |
| 2, parallel | M4a: web controls, badges and model/effort legibility | M1 | start dialog, defaults, schedules, Board/IssueView/RunView/RunsList badges, shared selectors, mocks/tests | uzi |
| 2, parallel | M4b: web usage/admin/dashboard truthfulness | M1 | RunUsage, Dashboard/admin usage components, status/count helpers and tests | uzi |
| 2, parallel | M5: CLI/TUI parity | M1 | `api/cmd/uzi` run/schedule flags and renderers, TUI-safe rendering/tests, embedded CLI skill only if command help changes | uzi |
| 3 | M6: offline e2e and joined acceptance | M2-M5 | agent e2e-stub seam, e2e phase, cross-surface/live-DB tests, final gates, child/parent progress | uzi |

- [ ] **M1: Freeze the complete public contract and atomic creation seam.** Export the harness/error types needed by production callers without duplicating policy. Use `txBeginner` plus transaction-bound queries for a helper that resolves D11, performs one caller-provided INSERT and freezes a Codex choice before commit; nil transaction support refuses Codex. Parameterize every activated INSERT with harness in this milestone, retain literal Claude only for the two Chat conversation inserts, regenerate sqlc once and freeze that surface for Phase 2. Add nullable `default_harness` to settings with decode-all-then-write behavior and `SetUserDefaultHarness`. Freeze all request/response shapes now: harness, per-run cost status, lifetime/last-seven subscription/unreported counts, fixtures and web types. Replace the M5A unknown-field tests with positive enum, explicit-unavailable, implicit-fallback, #1247 effective-harness and rollback tests. Prove a forced freeze failure leaves no run row. Gate: `task gate:api`, `task gate:web`, `./e2e/run-store-it.sh`.

- [ ] **M2: Route ordinary origins, repair claim assembly and preserve explicit exceptions.** Convert manual issue, Chat `start_run`, scheduled issue/sweep/autopilot, prompt, self-improve, CI-fix and automatic/manual MR-rework. Derived MR-rework inherits its source harness explicitly; CI-fix uses implicit D11. Keep Chat conversation/continue INSERTs literal Claude and test them as exceptions. A schedule pin remains explicit; null resolves at fire time, and a Codex result plus stored Anthropic override records a legible failed fire with no row. Branch full claim assembly on harness before the Anthropic ladder. Live-DB tests must prove a Codex-only full claim succeeds and a both-credential Codex claim has no `anthropic_oauth_token` and no `run_credential_epochs` row. Preserve trigger source, #1247 generation fences, unique-index handling, notifications and Claude bytes. Put every `LiveDB` test in the existing store/handler/forgesvc/schedsvc/workersvc sweep set. Gate: `task gate:api`, `./e2e/run-store-it.sh`.

- [ ] **M3: Make judge/task-review real Codex advice lanes and project cost truth.** Make `assembleJudgeClaim` harness-first: Codex uses `codexClaimSecrets`, never `judgeChoice`/Anthropic open/epoch, with a Codex-only full-assembly live test. Make judge enqueue test usability of the target run's inherited harness rather than requiring an Anthropic row. Route judge and task-review through `makeCodexAdviceHarness` when that harness is Codex, while Claude stays on its existing path. Select and construct through the established `main.ts`/`codex-executor.ts` seams; do not construct Codex classes in judge/review runners or widen `semgrep/codex-fixed-constructor.yml`. Existing claimed/running authority already admits advice release, so add no judge-specific capability exception. A Codex-only user must receive a real advice/review result, not a missing-token fallback. Then-fix inherits the original task harness explicitly and records a legible skip if unavailable. Populate M1's public usage shapes and both public and prompt-side judge/review cost context. Preserve token totals when dollars are unavailable and re-run M5A pricing tests offline. Gates: `task gate:agent`, `task gate:api`, `task test:codex-m3b`, `./e2e/run-store-it.sh`.

- [ ] **M4a: Activate web controls and harness legibility.** Build the harness choice into PR #1427's shared start dialog, not a second modal. Make token/eligibility gates harness-aware. Show a harness control only when both harnesses are usable; Codex-only users start Codex without a redundant picker. Add Run Defaults and schedule create/edit choices, header/list badges and harness-filtered model/effort controls. Add a Codex-only mock scenario and direct-API failure copy. Preserve one-harness Claude flows unless the fact is relevant. Tests assert positive state changes, payloads, keyboard/label accessibility and the vacuous-negative traps from `.claude/rules/web.md`. Gate: `task gate:web`.

- [ ] **M4b: Render usage/admin/dashboard truthfully.** Consume M1's closed cost status and counts in Run Usage, Dashboard, admin usage and list/detail summaries. Metered may show dollars; subscription is labelled; unreported keeps token totals and marks cost unavailable; mixed aggregates disclose incompleteness. Add focused component/helper tests for all four cases and hostile/unknown future enum values. Gate: `task gate:web`.

- [ ] **M5: Add CLI/TUI parity without a user-default setter.** Add `--harness` to run create and schedule create/edit with closed-value validation and retain-on-omission edit semantics. Render actual harness on run get/list and schedule get/list; render cost status/counts honestly in CLI/TUI. Register every new untrusted field with the TUI D7 sanitizer and extend hostile-value rendering tests. Confirm every new CLI-backed route is in `RequireUser`, not cookie-only, with a router-level `uzc_` Bearer test. Update the embedded CLI skill only for commands that actually ship. Gate: `task gate:api`.

- [ ] **M6: Prove the joined activation offline.** Add an e2e-only harness-neutral stub seam so `UZI_EXECUTOR=stub` can complete a Codex-bound claim without releasing or calling the real credential; production selection must still choose the real `CodexExecutor`. The seam must not construct Codex classes outside the existing constructor allowlist. Add one registry phase with the full `driver.sh` header that saves a runtime-assembled dummy `openai_api_key` through the boot-unlocked vault, starts an explicitly Codex-selected issue run through public API/CLI, observes `harness='codex'`, reaches the plan gate, completes through the e2e stub, pushes and records its MR. On the same worker, run the Claude control and prove no fallback/cross-contamination. Cover a pinned Codex schedule fire, explicit-unavailable 422, Codex-only implicit resolution and cost-status rendering. Assert no dummy credential/capability in logs or disk and tear down only owned resources. Run joined gates once to logs: `task gate:api`, `task gate:agent`, `task gate:web`, `task gate:repo`, `task test:codex-m3b`, `./e2e/run-store-it.sh`, `./e2e/run-e2e.sh`. Record revisions/statuses, move this child PRD to `prds/done/`, update parent #1106 M5B complete, and leave M6-M7 unchecked.

## Success criteria

1. Every production run origin resolves and freezes one harness; a Codex run and its immutable credential binding commit together or not at all.
2. D11 passes for explicit, default, sole, both and neither credential states. Explicit selection and pinned schedules never fall back.
3. Claude-only behavior and payloads remain compatible. A Codex-only user succeeds through full claim assembly. A Codex-indicating run never claims on an incapable worker, never assembles as Claude, carries no Anthropic token and writes no Anthropic credential epoch.
4. Public API, web, CLI/TUI, schedules, run badges and mock mode agree on the actual harness and its model/effort vocabulary.
5. PRD #1247's Anthropic override, switch/generation fence and epoch attribution remain intact; Codex plus an Anthropic override is a typed refusal, not a partial write.
6. Metered, subscription and unreported costs remain distinguishable on every public reader and in judge context. No partial or unknown total is displayed as complete zero.
7. A Codex-only target enqueues and fully assembles a Codex judge claim with no Anthropic secret/epoch; judge and task-review execute through the Codex advice harness. Derived run chains preserve the source harness and never silently alternate providers or disappear without a visible reason.
8. The offline e2e phase proves one Codex-selected run and one Claude control on the same worker from public creation through MR recording, with no secret/capability leak.
9. The implementation and validation diff contains no `.github/workflows/**` path; all named gates pass on the joined revision.

## Risks and stop conditions

- **Partial activation**: one callable server path without every required reader/control blocks merge. This child is one PR.
- **Non-atomic freeze**: resolving/inserting and freezing in separate commits can expose an unbound Codex row. Stop and redesign around one transaction.
- **Origin drift**: a missed INSERT silently stays Claude. The live-DB inventory must name paths, not only count rows.
- **Anthropic-before-harness assembly**: any Codex claim that opens/records an Anthropic credential is a blocking regression, even if the later executor branch is Codex.
- **Advice fallback**: a Codex judge/review that returns the deterministic missing-token fallback rather than running the Codex advice harness blocks acceptance.
- **#1247 collision**: do not start from a base before PR #1427 merges. Adopt its schedule/web/CLI/usage changes verbatim and re-run its affected regressions.
- **Semantic zero**: any DTO/reader that lacks cost status or the incomplete-count signal blocks acceptance.
- **Model leakage**: a Claude alias sent to Codex, or the reverse, blocks acceptance. Runtime fallback must be visible and tested.
- **Old-worker skew**: a public Codex run reaching a worker without a verified `codex_harness_v1` receipt blocks acceptance.
- **Credential-shaped fixtures**: assemble them at runtime and include `task scan:secrets`; a later cleanup commit cannot repair a push-protection violation in earlier history.
- **Workflow scope**: use existing Task targets and e2e registry files only. No real workflow path may be created even temporarily.

## Execution handoff

This PRD is ready for an offline uzi worker only after PR #1427 is merged and the implementation base includes it. Dispatch child #1429 as a gated run with MR rework enabled. Review and steer the generated plan before approval. Do not dispatch parent #1106. Once the approved run is in `running`, the initiating sessions may stand down and let server-side execution continue.

Keep the seven milestones distinct at the plan gate so the gated run receives an implementation budget sized for the full integration/e2e tail. The diff is expected to exceed automated-review file caps; each milestone therefore needs its own immutable reviewer/tester/auditor wave inside the run, and the final plan must not rely on CodeRabbit appearing. MR rework remains enabled for findings that do arrive.

## Decision log

- 2026-09-17: Created as parent #1106 M5B after M5A #1332 completed. The backend/frontend split remains rejected; public server routing, web, CLI/TUI and offline e2e land atomically.
- 2026-09-17: Current-main recheck found PRD #1247 M1-M5 merged and PR #1427 carrying its overlapping M6-M10 remainder. #1429 therefore depends on PR #1427 merging before dispatch and treats its result as baseline.
- 2026-09-17: Official OpenAI model pages were rechecked before offline handoff. The existing M5A Standard price table and Sol 2026-11-21 review boundary remain current; D8 records the resolved facts and sources.
- 2026-09-17: Cross-session review with `fable` found two activation blockers in current code: Codex claims traverse the Anthropic ladder before the harness branch, and judge/task-review have no Codex advice path. This child now owns both repairs, the concrete transaction seam, #1247 effective-harness integration, schedule conflict behavior, derived-run inheritance and literal-Claude Chat exceptions.
