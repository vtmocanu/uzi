# PRD #2149: Plan cross-check for auto-approved runs (Claude lead, Codex checker)

**Status**: Implemented; web and documentation completion pass validated, hosted acceptance pending. Child 1 of 5 under umbrella #2148 (Cross-check). Followed by PRD #2169 (dedicated cross-check slots), PRD #2150 (automatic revise rounds and the Codex-lead direction), PRD #2151 (checker model and effort pins per stage) and PRD #2170 (code cross-check).

Resolved facts below were read at `main` `8a5f138e`; the naming and D4 revision at `main` `8c35d39b`.

## Naming

**Cross-check** is the feature: a second opinion from the other model family. It has two stages: **Plan cross-check** (this PRD; required before implementation) and **Code cross-check** (PRD #2170; advisory before publication). The model doing the checking is the **cross-checker**. "Plan review" is not used for this feature: it already names the human pre-execution gate (`web/src/pages/RunDefaults.tsx`), and "second opinion" / "diff-review" name `uzi handoff --review` (`docs/handoff.md`, `api/cmd/uzi/handoff.go`). Internally the run kind is `cross_check` and the table `cross_checks`, keyed by `stage`.

## Problem

On an auto-approved run nobody checks the plan. The autopilot branch of `RunRunner.gatePlan` (`agent/src/runner.ts`) reports `running` with the plan and approves at once. The only automated checks are the history-rewrite nudge (`planProposesRewrite`) and the forced human gate for a CI-config `ci_fix` plan (`isCIConfigPlan`). Auto-approve is the default for every catalog schedule and sweep, for `self_improve`, CI autofix and `mr_rework`, so most unattended work implements whatever the lead planned.

When a maintainer steers plans by hand with a second session on the other model family reviewing each one, the reviewer asks for changes on most plans. In five such pairings (2026-09-30 to 2026-10-02, 11 plan gates) the cross-family reviewer caught gate commands with inlined environment variables, regression tests that could not fail on the unfixed code, unrequested behaviour changes, an overclaimed security property, stale ADR text, a missing changelog entry, and non-deterministic acceptance procedures. Unattended runs get none of this, although they are the runs with no human at the gate.

## Outcome

A user with both model families usable can opt in to **Plan cross-check**. An eligible new auto-approved run that reaches the auto-approval gate on the Claude harness has its plan checked by a separate, read-only run on Codex. Automatic implementation requires a pass (APPROVE) of the exact candidate and an acknowledged handoff; a forced human gate instead requires the human's decision on its current presentation. Changes requested (REVISE), blocked (BLOCK), or a failed check normally forces the human plan gate with the reason and available findings; the human then approves, revises or rejects as on any manual gate. Irrecoverable preparation receipts and human-presentation ACK loss instead fail terminally; unresolved preparation ACKs after three attempts and an acknowledged forced human gate also fail (see Lead side). Automatic revise rounds and Codex-lead runs come in PRD #2150; until then an opted-in Codex-lead run parks for a human instead of implementing unchecked. With the setting off, behaviour is identical to today.

Acceptance examples:

1. Plan cross-check on; Claude and Codex both usable; a worker with a free slot that runs Codex. The nightly sweep starts issue run R on the Claude harness. R's lead submits its plan; a `cross_check` run on Codex reads the repository read-only and returns APPROVE. The server stores the plan and R implements. R's activity feed shows "Plan cross-check: passed".
2. Same setup; the cross-checker returns REVISE with two items (or BLOCK, or no slot frees up before the deadline). R parks at `awaiting_approval` with reason `plan cross-check: changes requested` (or `blocked`, `timed out`); the findings show on the run page and in `uzi run get`; the Slack gate card shows only the reason line. The owner revises, approves or rejects as on a manual gate.
3. The same user's sweep starts a run on the Codex harness. It parks with reason `plan cross-check: not yet supported for a Codex lead`. It never implements an unchecked plan.
4. Plan cross-check off. No `cross_check` run is created and every write path is today's.

## Out of scope

- Automatic revise rounds and the Codex-lead direction: PRD #2150.
- Checker model and effort pins (the per-stage grid, and the epic #1703 Settings → Models rows): PRD #2151. Here the cross-checker uses what its claim resolves today (below).
- Dedicated cross-check slots on workers, so a child never competes with leads for a run slot: PRD #2169. Here the child uses ordinary run slots (D4).
- A wait that releases the lead's worker slot.
- The code stage (`stage = 'code'`): PRD #2170.
- Human-gated runs, seeded plans (`plan_source='seeded'`), gateless kinds (`task`, `chat`, `judge`, `job`), and isolated-lane runs (the isolated lane refuses Codex, `errIsolatedClaimRefused`; such a run parks with `plan cross-check: checker unavailable`).
- Custom cross-check guidance (an owner overlay on the checker's brief): PRD #2170 adds it for the code stage only; plan-stage guidance is a separate policy decision.
- An admin instance-wide switch. Only the user's own credentials are spent, as with `self_improve`.
- Co-planning by two leads.

## Modules and seams

### Opt-in setting

- `users.plan_cross_check_enabled BOOLEAN NOT NULL DEFAULT false`, on the user DTO.
- `PUT /api/me/cross-check` (body `{"plan": bool}`; PRD #2170 adds `code`) in the cookie-only `RequireAuth` group in `api/internal/handler/routes_me.go`, beside `/me/judge` and `/me/autopilot`: a consent switch that spends credentials must not be flippable with a CLI token. A field absent from the body is left unchanged.
- Enabling is refused unless both families are usable for the user (`harnessAvailability` in `api/internal/workersvc/harness_resolver.go`). The response warns, without refusing, when no online worker advertises `cross_check_v1` or can run Codex and ephemeral workers are off for the user. Disabling is always allowed.
- Web: a "Plan cross-check · Required before implementation" toggle in a "Cross-check" section on Settings → Run defaults (`web/src/pages/RunDefaults.tsx`), helper text "A second opinion from the other model family", mock-mode fixture. CLI: no write verb (consent toggles stay cookie-only, `docs/cli.md`); the account view shows the value.

### Run snapshot

- `runs.plan_cross_check_required BOOLEAN NOT NULL DEFAULT false`, computed inside each INSERT from `users.plan_cross_check_enabled` and never updated: true when the run is inserted with `auto_approve = true`, its kind is in a new `runkind.PlanCrossCheckable` set, and the plan is not seeded.
- `PlanCrossCheckable` = `issue`, `prompt`, `self_improve`, `ci_fix`, `mr_rework`: the kinds whose executors reach the plan gate on an auto-approved run. `runkind.PlanningCapable` is the wrong set; it admits `task`, which is auto-approved but gateless (`docs/handoff.md`). The implementation confirms each member against the executors, and a test pins the set.
- A parity test enumerates every SQL INSERT in `api/internal/store/queries/` that can write `auto_approve = true` on a `PlanCrossCheckable` kind and asserts it computes the flag, in the style of `runkind_sql_test.go`. (The Go callers `CreateAutopilotRun` in `poller/autopilot.go` and `CreateScheduledAutopilotRun` in `schedsvc/scheduler.go` reach those INSERTs.)
- Toggling the setting never changes an existing run; a resume or requeue cannot clear the flag.

### Claim gates

- A run with `plan_cross_check_required = true`, and every `cross_check` run, is claimable only by a worker advertising the new protocol capability `cross_check_v1` (`api/internal/capability`). An older worker would route an unknown kind to `RunRunner.execute` (`agent/src/worker.ts`, `resolveRunKind` passes unknown kinds through) and implement and push it; the gate makes that impossible.
- The predicate is mirrored wherever claimability is computed: ClaimRun, `CountOnlineWorkersClaimableForRun`, the peer mirrors in `runtime.sql`, `ListUnplaceableQueuedRunsForEphemeral` and `ListSaturationQueuedRunsForEphemeral`.
- `health.go` gains a queued reason naming the missing capability, so a run created before the worker fleet rolls is visibly waiting, not silently stuck (the worker image pin is decoupled from app releases).

### Candidate and cross-check records

- New table `cross_checks`, one row per check:
  - `lead_run_id`, `stage TEXT NOT NULL` (`CHECK (stage IN ('plan'))` here; PRD #2170 widens it to `'code'` with that stage's own constraints), `round` (1 in this PRD; PRD #2150 adds more), unique on `(lead_run_id, stage, round)`; `lead_claim_generation` at submit.
  - The candidate: `plan_md`, `milestones` (jsonb), `required_capabilities`, `required_tools`, `size_class`, `base_commit` (the immutable SHA the lead planned on), `planning_diff` (bounded, scanned, below), and `candidate_digest`: sha256 over the canonical JSON of those fields, canonicalised exactly as `gatePresentedPayload.digest` (`api/internal/workersvc/gate_revision.go`), computed by the server from the stored, already-scrubbed values. The server returns the digest; the worker never supplies one. The plan-stage columns are nullable in the schema and required by `CHECK (stage <> 'plan' OR (plan_md IS NOT NULL AND milestones IS NOT NULL AND size_class IS NOT NULL AND base_commit IS NOT NULL AND candidate_digest IS NOT NULL))` (written as an implication so PostgreSQL's NULL-passes-CHECK rule cannot admit a half-filled plan row), so the code stage can carry its own shape.
  - `checker_run_id`, `checker_harness`, `checker_model`, `checker_effort`: the latter two as actually delivered on the child's claim, recorded at claim time.
  - `verdict` in `pending | approve | revise | block | failed`, `reason_class`, `findings` (jsonb, bounded), `decided_at`, `deadline_at`, `created_at`.
- At most one `pending` row per lead and stage (partial unique index).

### Child run kind `cross_check`

- A new run kind via the runbook in `api/internal/runkind` (migration widening `runs_kind_check` and `runs_kind_shape`, the const and property helpers, `fixtures/run-kinds/registry.json`, agent `RUN_KINDS` and `RUN_KIND_PROFILES`, web `RUN_KINDS`). Not judge-eligible, not `Listed` in the Runs list and never a board card, not planning-capable, and not wall-timed by the run budget (it has its own cap). A new kind rather than the task-review precedent's `task` row with `review_target_run_id` (`CreateTaskReviewRun`, created via `createRunResolved` in `workersvc/task.go`): a `task` row is listed, wall-timed and planning-capable, and the worker routes `task` claims with a review target to the diff `ReviewRunner`, so reuse would need a discriminator threaded through all of those. The kind is stage-neutral; the child reads its stage from its `cross_checks` row.
- Created through `createRunResolved` / `createRunAtomic` with the checker harness as an explicit selection, the task-review precedent; the checker harness is always the opposite of the lead's `runs.harness`. Credential selection, disabled-credential refusal, the Codex capability mint, epoch and revocation, claim fences and `run_usage` attribution then come from existing code unchanged, under the child's own harness. No claim ever carries two model credentials. The child's claim carries the forge PAT for the clone, as task-review runs do today; the worker holds it, never the model.
- Created with `runs.priority = 2`, the existing expedite rank in `fn_run_priority` (`api/internal/handler/priority.go`), so a child is claimed ahead of queued leads from the same sweep.
- Repo-ful, report-only: it checks out `base_commit` under a clone key derived from its own run id (never the lead's `issue-<iid>` slug), pushes nothing, opens no merge request, and spawns no review.
- The `cross_checks` row and the child are inserted inside the same create closure, so they commit together.
- Lifecycle: when the lead parks, is requeued, reclaimed, cancelled or reaches a terminal state, its pending child is cancelled and its row marked `failed`, `reason_class = superseded`.
- Interrupted attempt (one check per lead): a changed lead claim generation cannot start a second checker round or reuse the prior candidate's approval. Submission with a prior mismatched generation/digest is refused as `interrupted`; the worker normally forces a human gate for the execution's plan. A reclaimed generation cannot reuse an older APPROVE in the guarded plan write. This interrupted-attempt fallback does not authorize reconstructing a parked human gate from status proof: D16 below governs an already established presentation/revision, and unrecoverable delivery losses fail terminally. A retried submit within the same claim generation and with the same digest (a lost submit ACK) returns the existing row. PRD #2150 replaces the interrupted park with a fresh round.

### Worker routes (Bearer)

- `POST /api/worker/runs/{lead}/cross-checks` (`stage = "plan"`): the lead's worker submits a candidate, decoded with `httpx.DecodeJSONStrictBounded` (8 MiB envelope). The server checks the lead is owned by this worker at its current claim generation, is `plan_cross_check_required` and `auto_approve`, recovers a matching existing round before credential resolution, and re-checks that the Codex family is still usable when creating a new child (else it returns a refusal the worker turns into a `checker unavailable` park at once). A same-generation retry with the same digest returns the existing immutable row and child, including a decided attempt; it does not create another round. The candidate response includes `result = "candidate"`, round, candidate generation/digest, normalized candidate, child id, deadline, verdict/reason and reconciliation metadata (`api/internal/handler/cross_check_worker.go`). A `no_row` observation releases no transport authority; a `parked` status proof cannot establish a human presentation (D16).
- `GET /api/worker/runs/{lead}/cross-checks/plan/{round}?claim_generation=...`: current candidate/verdict and reconciliation metadata for the owning worker's claim. Status proof does not create approval or presentation authority.
- `POST /api/worker/runs/{child}/cross-check-verdict`: the cross-checker posts `{claim_generation, verdict, reason_class, items[], summary}`, validated and scrubbed like `validateAndScrubTaskReview` (`api/internal/handler/task_review.go`). Accepted only when the child is `kind = 'cross_check'`, owned by this worker at its current claim generation, its row is `pending` and before `deadline_at`, and the lead's current claim generation still equals `lead_claim_generation`. Anything else is refused (409) and stored nowhere. The server, not the worker, then emits the `cross_check` run message on the lead from the stored row.

### Cross-checker runner (agent)

- `CrossCheckRunner`, routed by claim kind, structured like `ReviewRunner` (`agent/src/review-runner.ts`): report `running`, one bounded check, post one structured result, report `completed`; any model error posts `failed`. It dispatches on the row's stage; this PRD implements `plan` only.
- The Codex cross-checker runs on the Codex executor's broker lane, not the tool-less advice harness (which takes no tools and no working directory by contract, `codex-advice-harness.ts`). Its immutable `RunGrants` expose `Read` and bounded `Search` over the checkout (`agent/src/codex/cross-check.ts`, `agent/src/codex/dynamic-tools.ts`). No `Bash`, no `apply_patch`, no skills or delegation. The native shell stays disabled (`shell_tool = false`, `agent/src/codex/config.ts`), `project_doc_max_bytes = 0` so `AGENTS.md` is not loaded, and both fileop and direct provider filesystem access use required cross-check confinement. The checkout is read-only; explicitly granted private home/Codex/auth/config/sessions/XDG/tmp state remains writable. Broad shared access, sibling files and escaping symlinks are denied. This is not network-isolation proof. See [ADR-2149](../adr/2149-cross-check.md) for enforcement and proof boundaries.
- Inputs: the candidate (plan, milestones, required capabilities and tools, size class), the issue title and body, the planning diff, and a fixed brief: verify cited anchors, flag regression tests that cannot fail, scope beyond the issue, gate commands with inlined environment variables, missing changelog or docs, and security or data-integrity claims stronger than their evidence. The issue body, repository content and diff are rendered as nonce-fenced untrusted data.
- Output is exactly one JSON object; anything else is `failed`, reason `malformed`.
- Usage: the runner posts usage frames for every model call, as `judge-runner.ts` does, so the child's spend lands in its own `run_usage` rows. `ReviewRunner` is not the model here: its model passes discard `AdviceResult.usage` (`agent/src/model-pass.ts`) and it posts no usage frame.
- Model and effort: whatever the child's claim resolves through today's lanes (`GetUserHarnessModelDefaults`, the per-harness effort in `claim_effort.go`), recorded on the row as delivered. Claim assembly currently skips model lanes for Codex task reviews (`isReviewRun` in `claim_assembly.go`); `cross_check` claims take the ordinary lane path, and a test asserts it.

### Lead side (agent)

- In the autopilot branch of `gatePlan`, when `claim.plan_cross_check_required` is true:
  1. Capture the planning diff relative to `base_commit`: tracked and untracked, excluding ignored and binary files, at most 200 untracked files, read through the hardened `runGit` with `--no-ext-diff --no-textconv`, stream-stopped at 512 KiB + 1 byte actually read (never buffered to `GIT_MAX_BUFFER` and truncated afterwards), and scanned with gitleaks before upload, as `preserved_patch` is. Over a bound, or a gitleaks hit, parks with reason `plan cross-check: planning diff refused`.
  2. Submit the candidate, then poll the verdict every 15 s until it is decided or `deadline_at` passes.
  3. APPROVE: send the `running` report with the exact server-normalized candidate, carrying the server-returned `candidate_digest`; implementation requires an acknowledged handoff. REVISE, BLOCK, timeout, `failed`, refusal and Codex lead normally take the existing forced-human-gate path (auto-approve off, as for a CI-config `ci_fix` plan), reporting `awaiting_approval`; the server clears `auto_approve`. D16 preserves an already established execution-local human presentation/revision instead of reconstructing one from status.
- Delivery exceptions in `agent/src/plan-cross-check-gate.ts` and `agent/src/runner.ts` are terminal: `plan cross-check: preparation receipts irrecoverably lost` for receipt loss; `plan cross-check: human-presentation ACK unrecoverable` for unrecoverable presentation ACK loss. After three preparation attempts and an acknowledged forced human gate, unresolved ACKs fail with `plan cross-check: preparation ACKs unrecoverable`. There is no indefinite retry, `recovery_wait`, fabricated receipt or fabricated approval on these paths.
- The wait does not consume the lead's run budget. While a row is pending, the live wall-deadline computation behind `RequestWallParks` (`runtime.sql`) excludes the elapsed pending time, so a lead near its budget is never wall-parked mid-check; when the wait ends (decided, superseded or past deadline), the wait is banked into `budget_paused_seconds` exactly once, as human-gate parks are. While a row is pending, `health.go` reports `waiting for plan cross-check` instead of the no-updates stall, and no stall nudge is sent.

### Server enforcement

`SetRunRunning` and `SetRunAutopilotPlan` (`api/internal/store/queries/runtime.sql`) change for a run with `plan_cross_check_required = true` and `auto_approve = true`:

- `SetRunAutopilotPlan` stays write-once and gains a `@candidate_digest` parameter. Its guarded UPDATE requires, in the same statement, the run's latest plan-stage `cross_checks` row to have `verdict = 'approve'`, `checker_harness <> runs.harness`, a `checker_run_id` whose run is `kind = 'cross_check'` with that harness, `lead_claim_generation = runs.claim_generation`, and `candidate_digest = @candidate_digest`, and the submitted plan, milestones, capabilities, tools and size class to equal the stored candidate's. An APPROVE of an older round never satisfies a newer one.
- `SetRunRunning` writes inferred capabilities, tools, size class and frozen milestones only in the transaction of a guarded plan write that passed, and refuses any change to them once `plan_md` is set.
- `SetRunCompleted` and milestone-progress writes refuse while `plan_md` is NULL.
- A refused plan write is today's rowcount-zero refusal, which the worker already treats as fatal ("autopilot plan not durably stored"), so the run fails rather than implements. The guard is a backstop on what the server stores and completes; it cannot stop a worker that ignores the refusal from running code, which is why the claim gate keeps unaware workers out and the worker obeys the refusal.

### Visibility

- Web (delivered): `ActivityFeed` renders plan-stage `cross_check` messages even without an agent lane. `PlanCrossCheck` in the plan panel and run detail distinguishes the current gate reason from the checked-candidate outcome; unknown/inconsistent verdict-reason pairs show unavailable. Findings use hardened Markdown capped at 16,384 source characters and 20 items with truncation disclosure; metadata is bounded/sanitized and checker links require valid UUIDs. Recorded model/effort, reported tokens and API-equivalent cost appear; a checker with no cost estimate or with unreported cost shows no dollar figure (PRD #2559). Human revisions label original findings as earlier-plan evidence, not certification of the current plan. Newly observed cross-check events trigger bounded detail refresh; refresh failure does not start indefinite retries.
- CLI: `uzi run get` prints the reason and findings through `renderer.Plain` (`.claude/rules/tui.md` D7).
- Slack: `gateBlocks` (`api/internal/slacksvc/gate.go`) shows `Plan cross-check: <reason>` with bounded, sanitized reason text and no checker findings.

### Resource bounds (plan, issue body, planning diff, repository content and cross-checker output are untrusted)

| Resource | Bound | Enforced in |
|---|---|---|
| Plan cross-checks per lead | 1 in this PRD | submit route predicate |
| Concurrent cross-checks per lead and stage | 1 | partial unique index |
| Candidate plan_md / milestones | 256 KiB / 64 items; over cap parks, never retries | submit route (`DecodeJSONStrict`) |
| Planning diff | 512 KiB read, 200 untracked files, no binaries, gitleaks-clean | lead capture; submit route re-checks size |
| Wait for a verdict | `PLAN_CROSS_CHECK_TIMEOUT`, default 30 min (half of `RUN_TIMEOUT` when that is 30 min or less), max 2 h, stored as `deadline_at` | submit, verdict route, lead poll |
| Lead poll rate | every 15 s | lead |
| Cross-checker model turn | `CROSS_CHECK_MODEL_TIMEOUT`, default 15 min (the text-only `REVIEW_MODEL_TIMEOUT_MS` is 5 min; a tool-using check needs more) | `CrossCheckRunner` |
| Cross-checker tool calls | 200 per check | broker grant |
| Findings | 20 items, 2 KiB each, 32 KiB total, summary 4 KiB | verdict route |
| Worker slots | normal run-lane caps, no exception (PRD #2169 adds a dedicated lane) | existing claim path |

Boot validation refuses malformed or out-of-range knobs, and `PLAN_CROSS_CHECK_TIMEOUT` must be below `RUN_TIMEOUT`; an unset value derives a valid default so a short-`RUN_TIMEOUT` install still boots.

## Testing decisions

- **Server enforcement (LiveDB via `./e2e/run-store-it.sh`):** on a cross-check-required run, `SetRunAutopilotPlan` refuses with no cross-check, pending, REVISE, BLOCK, an APPROVE whose candidate differs in milestones, tools or size class only, an APPROVE from a same-family checker, an APPROVE for a superseded lead claim generation, and a mismatched digest; it accepts only the matching APPROVE. `SetRunRunning` refuses to set capabilities, tools, size class or milestones without a passing plan write; `SetRunCompleted` refuses with `plan_md` NULL. Not-required runs are unchanged (follow `setrunautopilotplan_livedb_test.go`, `TestAutopilotPlanWriteAtomicWithRunningLiveDB`). Each guard has a mutation that removes it and reddens a case.
- **Snapshot:** the INSERT parity test, and the `PlanCrossCheckable` set pinned.
- **Claim gates:** neither a cross-check-required run nor a `cross_check` run is ever claimed by a worker without `cross_check_v1`, and every mirror agrees; the health reason appears.
- **Lifecycle and fences:** verdict after deadline, after lead park, cancel, requeue or reclaim, and from a stale child or lead claim generation are refused and change nothing; a retried submit with the same digest reuses the row; the pending child is cancelled on every lead exit; the child sorts ahead of queued leads.
- **Worker (`agent/test/runner-plan-gate.test.ts` style):** APPROVE implements; REVISE, BLOCK, timeout, malformed, refused, Codex lead and planning-diff refusal each park through the forced-gate path with the right reason; a not-required autopilot claim behaves exactly as today.
- **Wall budget (LiveDB):** a lead with less budget left than the wait is not wall-parked while its row is pending; the wait is banked into `budget_paused_seconds` exactly once on each way the wait ends (decided, superseded, deadline), with a mutation that double-banks or skips the live exclusion reddening a case.
- **Cross-checker isolation:** built configuration assertions cover Read/Search grants, native shell off and `project_doc_max_bytes = 0`. Separate fileop and direct-provider proofs cover read-only checkout access and denied sibling/symlink access while private home/Codex/sessions/XDG/tmp grants remain writable. Packaged config/auth/session proof is not authenticated model-turn or network-isolation proof; see [ADR-2149](../adr/2149-cross-check.md).
- **Custody unchanged:** a Claude lead's claim carries no Codex secret; the child's claim carries no Anthropic token; the child's usage lands in its own `run_usage` rows under Codex; the child claim takes the ordinary model lane and the recorded model and effort equal the claim's.
- **Planning diff:** the stream stop at the byte bound, the file-count cap, binary exclusion, and a gitleaks-detected fixture assembled at runtime (`.claude/rules/prds.md`).
- **Web and CLI:** feed and panel rendering of findings, including hostile Markdown; `uzi run get` through `renderer.Plain`; mock-mode fixture.

## Implementation and validation evidence (2026-10-06)

The milestones below are the original delivery/gate bundles, not a claim that
this pass reran their inherited runtime gates. Their broad boxes remain open as original delivery bundles; this completion
pass does not assert that every original runtime gate was rerun. Hosted
acceptance remains outstanding. Current behavior supersedes M1's checker-unavailable
scaffolding and M2's blanket “anything else parks” shorthand.

| Scope | Evidence / remaining boundary |
|---|---|
| Runtime and confinement | Maintainer-provided provenance: PR2358 reviewer/tester/auditor at `631b28a1`, 2262 LiveDB tests and capable-host confinement PASS run by the lander. Not observed or rerun in this documentation pass. |
| Web M1 | Committed at `e45d6cb` (implementation `bb0efb31`); dispatch records observed `gate:web` PASS: 5641 unit tests / 299 files, 8 Chromium tests / 2 files, docs checker PASS, and `npm run build` PASS. Initial gate lint failure was fixed and the gate rerun PASS. This is web evidence, not a new runtime validation claim. |
| M2 documentation | Docs, architecture, spec and [ADR-2149](../adr/2149-cross-check.md) committed at `ba73044a`; `task docs:sync` completed. Reviewer found no mandatory or optional findings. Observed `task gate:web` PASS (5641 unit tests / 299 files, 8 Chromium tests / 2 files, including `check-docs:web`), `npm --prefix web run build` PASS, `task gate:repo` PASS and `task gate:api` PASS (including the embedded-docs package). Secrets scan: zero tracked-file findings, both canaries DETECTED; SAST: zero findings, canary DETECTED. These component gates do not rerun the prior LiveDB or capable-host confinement proof. |
| Hosted acceptance | Acceptance issue 2152 remains maintainer-owned and pending. Keep this PRD in `prds/`; implementation evidence does not complete hosted acceptance. |
| Deferred scope | Automatic checker revision, Codex-lead checking, dedicated slots, stage-specific pins and code cross-check remain out of scope, as listed above. |

Completed in this approved web/docs pass:

- [x] Honest web outcomes, bounded untrusted findings, historical evidence, checker metadata and detail refresh; reviewer plus tester rounds completed at `bb0efb31` and `e45d6cb` with the mandatory lint finding fixed.
- [x] Documentation, ADR, spec and embedded mirror updated; component gates observed passing at `ba73044a`.

## Milestones

- [ ] **M1 (original milestone; implementation present, full gate bundle not rerun here): Opted-in auto-approved runs wait at a fail-closed plan cross-check gate.** Setting and route, run snapshot with its parity test and `PlanCrossCheckable`, the `cross_checks` table (its migration lands here because the guards below read it; no row is written until M2), both claim gates with mirrors and the health reason (the `cross_check` kind's claim gate lands with the kind in M2), server enforcement in `SetRunAutopilotPlan` / `SetRunRunning` / `SetRunCompleted`, and the historical scaffolding branch (superseded by the integrated checker) that, with no cross-checker yet, parked cross-check-required runs with `plan cross-check: checker unavailable` through the forced-gate path, with the Slack reason line, feed message and CLI line. Docs: a new `docs/cross-check.md` (audience `user`, introducing Cross-check and its two stages, stating this PRD checks Claude-lead plans only and Codex-lead runs park), `docs/autopilot.md`, `docs/scheduling.md`, `docs/configuration.md`, `docs/slack.md`, then `task docs:sync`; `specs/human.md` (amend the l.201 "zero uzi interaction" autopilot promise for opted-in users); CHANGELOG. Blocked by: none. Gates: `task gate:api`, `task gate:agent`, `task gate:web`, LiveDB via `./e2e/run-store-it.sh`, `task gate:repo`.
- [ ] **M2 (implementation and docs sync/validation present; hosted acceptance pending): A Claude lead's plan is cross-checked on Codex; an exact-plan pass implements, non-pass normally forces a human gate with available findings, and irrecoverable delivery losses fail.** Spike the read-only broker grant set first. Then the first `cross_checks` writes, the `cross_check` kind (full runbook), the three worker routes, `CrossCheckRunner` (Codex, plan stage), planning-diff capture, priority, lifecycle and fences, wait-time crediting, `PlanPanel` findings and run-page child link, `uzi run get` findings, `docs/cross-check.md`, `docs/run-activity.md`, `docs/cli.md` then `task docs:sync`, `ARCHITECTURE.md` (autopilot section), an ADR for the server-enforced cross-check seam (adr/2149-cross-check.md), CHANGELOG. Blocked by: M1. Gates: as M1, plus `task scan:secrets` named explicitly with its canaries-detected line in the run evidence (the planning-diff gitleaks fixture is assembled at runtime, `.claude/rules/prds.md`).

No `.github/workflows/**` change in implementation or validation.

## Acceptance (hosted k8s, maintainer-owned)

Tracked in the `acceptance` issue #2152: an opted-in Claude-lead sweep run is cross-checked by a Codex child on a Codex-capable worker and implements after a pass; changes requested or blocked parks it with the reason visible on web, CLI and Slack; with no free Codex-capable slot it parks on timeout; an ephemeral worker provisioned for the child after the saturation debounce checks normally; the child's usage is attributed to the user's Codex credential. Implementation may merge before it; the PRD moves to `prds/done/` only after it.

## Decision Log

- **D1. A child run on the other harness, not a second credential on the lead's claim.** Rejected: one claim carrying both credentials. `runs_codex_harness_coherence_check` (migrations 00226/00227) forbids Codex binding columns unless `harness = 'codex'`; the worker picks its executor by the presence of `claim.secrets.codex`; the claim-time disable re-check in `claim_recovery.go` switches on the lead's harness, so a Codex lead's Anthropic checker would go unchecked; `run_usage` derives harness and cost from `runs.harness` (`usage_fold.go`); a Claude-lead run never reaches the `codex_harness_v1` claim gate. About twenty harness branch sites would change. A child run reuses that custody unchanged.
- **D2. Check, not co-planning.** The hand-steered shape; about one extra turn per plan.
- **D3. The cross-checker reads the repository through read-only broker tools under Landlock.** Rejected: a text-only check like `ReviewRunner`, since checking anchors against code was most of the value; and the tool-less advice harness, which cannot read files by contract. Read-only tools alone do not stop network use or instruction loading, so the native shell and `AGENTS.md` loading are off and asserted.
- **D4. Run-lane caps unchanged here; park on timeout; expedite the child. Dedicated cross-check slots are PRD #2169.** Rejected: letting the lead's worker claim its child past its run cap, because a SQL eligibility exception does not prove the worker's scheduler can run another run or that it runs Codex. PRD #2169 instead adds a separate worker lane with its own cap and advertisement (the chat-lane precedent), so a lead's check does not wait for a run slot. Rejected for now: a durable wait that releases the lead's slot. User decisions 2026-10-03 (caps kept in this PRD; the lane as its own PRD).
- **D5. The candidate is stored before the child exists; approval binds to the latest round's server-computed digest and the lead's claim generation.** The approved `plan_md` stays write-once, so the candidate lives in `cross_checks`; the digest covers every approval-bearing field (`gatePresentedPayload` includes size class) plus the base commit and planning diff.
- **D6. The server is the backstop on what is stored and completed.** It guards the plan write, the approval-bearing fields `SetRunRunning` would otherwise write on any report, and completion. The claim gate keeps unaware workers out; a refused write is fatal to the run.
- **D7. Non-pass outcomes normally force the human gate; irrecoverable delivery losses fail terminally.** Receipt loss, human-presentation ACK loss and preparation ACK exhaustion after three attempts and an acknowledged forced gate are the named exceptions in Lead side. Nothing proceeds with dissent. Automatic revision is PRD #2150, which adds the fenced untrusted-feedback prompt it needs: today's revise prompt tells the lead the feedback is an authoritative human instruction (`buildRevisePlanPrompt`, `agent/src/prompt.ts`).
- **D8. Snapshot the requirement on the run, computed inside the INSERT.**
- **D9. Per-user opt-in, cookie-only, refused without both families.** Follows the judge and autopilot consent switches. The plan and code stages are separate toggles (PRD #2170), because one is a gate on auto-approved runs and the other is advisory on all runs.
- **D10. Slack shows the reason only.** Findings can quote repository content; the Slack content-minimisation rules in `ARCHITECTURE.md` allow only whole-plan exceptions.
- **D11. A new run kind, not a `task` row.** See the child run kind section.
- **D12. Split into PRDs under one umbrella.** User decisions 2026-10-03: this PRD (one check, Claude lead), PRD #2169 (dedicated slots), PRD #2150 (automatic rounds, Codex lead), PRD #2151 (pins), PRD #2170 (code stage).
- **D13. User-facing name "Cross-check", with stages "Plan cross-check" and "Code cross-check".** User decision 2026-10-03. Rejected: "Plan review" (names the human gate), "Second opinion" (handoff wording, and reads as optional for a required gate), "Xcheck" (reads as a failure mark beside status icons). A survey of agent products found no product using "cross-check" as a feature name; "second opinion" is common descriptive copy, so the docs use it as the one-line description.
- **D14. One stage-neutral kind and table, keyed by `stage`.** The code stage (PRD #2170) reuses the kind, claim gate, custody, lane and records without adding a second kind or table (it still needs a migration to widen the stage domain and add its own fields). `stage` is `NOT NULL`; each stage's row shape and verdict domain are enforced by stage-conditional `CHECK`s written as implications (`stage <> 'x' OR …`), while the cross-row rules (latest round, claim generation, one pending per lead and stage) stay in guarded writes and the partial unique index.
- **D16. A parked lead keeps its existing live human plan gate; no reclaim contract.** Maintainer decision 2026-10-05 supersedes the parked-result recovery/reclaim clause of plan seq 466 and rejects its proposed addendum. On `result="parked"`, do not release, reclaim, transition to `recovery_wait`, send a new `awaiting_approval` report, mint a presentation from the checker candidate, or adopt a gate from status proof. Continue only with this execution's established presentation/revision context; current human approve/revise/reject uses the existing gate and revision path, and the original checker candidate remains historical evidence. Lost park ACK followed by human revision must preserve both safety and liveness: no overwrite, stale approval or fresh check, and the current human verdict can still resolve the gate. If an encountered case lacks that context and cannot be handled under these constraints, stop and ask the maintainer before wiring it. No new migration, recovery cause, feature flag/capability, or recovery/promotion/requeue/budget writer change. Exact-plan handoff and the sequence barrier remain required. Packaged confinement proofs were NOT RUN by the original worker; subsequent maintainer-provided provenance reports capable-host confinement PASS run by the lander. They remain NOT RUN in this documentation pass; that provenance does not establish authenticated model-turn or network-isolation proof. The lander must still execute the capable-host confinement gate against the PR head before merge.
- **D15. Not split further; the large PR is accepted.** The scope gate flags this PRD as an expected large PR (api, agent, web, CLI, Slack, migrations, docs). No split yields independently valuable pieces: M1 alone parks every opted-in run with `checker unavailable`, which is only safe scaffolding for M2, and M2 cannot ship without M1's server enforcement. The deeper splits (lane, automatic rounds, pins, code stage) are already separate PRDs (D12). Recorded 2026-10-03 at dispatch.
