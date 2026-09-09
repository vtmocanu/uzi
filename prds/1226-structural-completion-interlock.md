# PRD #1226: Structural completion interlock and exact-head permit

**Issue:** [#1226](https://github.com/vtmocanu/uzi/issues/1226)
**Parent epic:** [#1225](https://github.com/vtmocanu/uzi/issues/1225)
**Status:** Ready for implementation. First child in the #1225 sequence.
**Priority:** High.
**Execution:** Queued for the next Planned sweep with `PRD`, `Planned` and `uzi`. No other #1225 child or #1214 may carry `uzi` while this runs.

Implement on a new branch from current `main`. This child is structural containment for the exact failure observed in PR #1220. It does not claim semantic completion review or cross-worker provider-session durability. No implementation or validation may create or modify `.github/workflows/**`. Assign migrations above the live head at merge time.

## Problem and outcome

An issue run can freeze five milestones, declare only four complete, and still open a PR containing `Closes #N`. `SetRunCompleted` accepts a milestone subset; the worker opens the PR before that terminal report; the normal issue body closes the issue. Existing `required_capabilities` cannot safely gate this feature because an owner override clears the set and the admin capability kill-switch bypasses matching.

Add a non-bypassable completion protocol for new issue runs. `signal_done` becomes a structural completion attempt inside the same lead session. Missing milestones return to that lead after a checkpoint. A server permit bound to the current contract revision and exact final head is required before PR creation and terminal completion. Repeated no progress enters a recoverable completion hold rather than failing the run.

The first delivery provides honest `same_worker_only` session durability. Git work is positively captured before a hold; the provider HOME/session remains on the current worker. Cross-worker context durability is child [#1229](https://github.com/vtmocanu/uzi/issues/1229), built on [#1228](https://github.com/vtmocanu/uzi/issues/1228).

## Included

- A versioned structural completion contract frozen with `milestones_frozen` at approval.
- A dedicated, unconditional worker-claim capability clause for interlocked runs.
- Same-lead structural rejection and progress-sensitive completion attempts.
- Claim-fenced, idempotent permits bound to contract revision, branch and exact head.
- PR creation only after a permit, plus PR-head verification for GitHub, GitLab and Forgejo.
- Atomic permit consumption and terminal completion through a service transaction.
- A worker-authored completion question and a dedicated `SetRunCompletionHold` transition.
- Continue-with-guidance as the first owner decision.
- Honest web/CLI status and `same_worker_only` context durability.
- Structural partial/accept protocol fields reserved for #1227 without activating them.

## Excluded

- Owner partial-scope and criterion-accept decisions, delivered by #1227.
- Durable provider-context export/restore, delivered by #1228/#1229.
- Semantic criteria, independent completion audit and structured findings, delivered by #1230/#1231.
- Gating `mr_rework` or non-issue run kinds.
- Moving the full ADR-456 alignment implementation out of `phasePublish`.
- Changing secret-scan or GitHub Push Protection failure classification.
- Hosted-k8s deployment proof or rollout-default flip, delivered by #1232 and maintainer follow-up.

## Resolved implementation facts

Checked on 2026-09-09 at `df51d629`:

- Run `b0ffd0ea` froze `m1..m5`, declared `m1..m4`, had no scope cap and opened PR #1220 with `Closes #1171` after 8 of 25 iterations.
- `agent/src/sdk-executor.ts` already has `turn.done`, same-session follow-up injection, worktree fingerprints, budgets and `STALL_LIMIT = 3`.
- The existing #281 detector throws `REASON_NO_PROGRESS`, which becomes `failed`. Once a gated completion attempt exists, that outcome must enter the completion hold. Before the first attempt, legacy stall/budget behavior remains unchanged.
- `SetRunPaused` admits only `running` with an owner pause request. It cannot implement `awaiting_input -> paused`; do not loosen it.
- `ClearRunRequiredCapabilities` can clear the entire capability set, and the capability-aware kill-switch can bypass ordinary required-capability matching. The completion capability requires its own positive `ClaimRun` clause outside both mechanisms.
- `phasePublish` currently scans, conditionally performs GitHub workflow-tree alignment, pushes, opens the PR, then reports completion. The structural predicate is head-independent; after alignment, request/reissue the permit for the final head before PR creation. Child #1231 handles semantic evidence invalidated by head drift.
- No forge client currently reads a PR head SHA. Add the neutral method and all three drivers in this child.

## Design decisions

### D1: Version and freeze the structural contract

When the temporary rollout switch is on, `CreateRun` stamps `completion_contract_version = 1` on a new issue row before its first claim. The contract content is still absent until approval, when the server freezes `profile = structural` with one server-minted criterion `m<N>.c1` per frozen milestone; the criterion text is the milestone title and has no semantic evidence slots. `contract_revision` begins at one. Approval does not re-claim, so stamping only at approval would make the hard claim clause vacuous for the plan-phase worker.

The contract and permit wire shapes reserve optional semantic audit/finding fields from day one. #1230/#1231 fill them without replacing the protocol. A nullable contract version is the explicit legacy discriminator; never exempt a one-milestone run by cardinality.

### D2: Make worker compatibility non-bypassable

Add a dedicated positive claim predicate equivalent to:

```sql
completion_contract_version IS NULL
OR worker capabilities contain 'completion_interlock_v1'
```

It is outside `required_capabilities`, `ClearRunRequiredCapabilities` and the `capability_aware` kill-switch. Those existing controls retain their semantics for optional inferred tools/capabilities but cannot authorize a worker that does not implement the completion protocol.

### D3: Rework in the same session before terminal break

On gated `signal_done`, checkpoint the current work before emitting any denial. Compare accepted `report_progress` plus the terminal declaration with every in-scope structural criterion. A missing milestone emits a bounded `completion_attempt` event and becomes a structured follow-up to the same lead session. No PR call occurs.

Continue while the unmet set shrinks or the head/worktree fingerprint changes. The completion-attempt fingerprint is `(unmet IDs, head, worktree fingerprint)`, distinct from the #281 prose-response fingerprint but sharing `STALL_LIMIT = 3`.

After attempt one exists:

- #281 `REASON_NO_PROGRESS` routes to the completion hold rather than the generic failed catch.
- `REASON_MAX_ITERATIONS` routes to the hold.
- `REASON_WALL` and `REASON_IDLE` route to the hold.
- `SweepRunningTimeout` excludes rows with `completion_attempts > 0` and a live worker heartbeat. The running-state acknowledgement serves a bounded one-shot `budget_exhausted` flag, following the existing served steering-flag pattern; acting on it, parking or a new owner decision clears it so stale acknowledgements cannot re-arm a hold. The worker enters the verified hold. A stale worker still follows the existing requeue path.

Before any completion attempt exists, existing stall, wall and budget outcomes remain unchanged.

### D4: Persist attempts and permits

Add bounded additive storage for contract identity, latest attempt/hold summaries and `run_completion_attempts`. A permit request includes run/claim fence, contract revision, branch and exact head. Its optional audit block and finding IDs are null/empty under `profile=structural`.

The server recomputes its owned predicates, issues an idempotent permit for the exact identity, and denies stale claims, missing milestones, invalid decisions, head drift or revision drift. Repeating an unchanged accepted request returns the same permit.

Completion runs through a Go service transaction that consumes the permit and writes `completed`. Do not encode the whole operation as an isolated SQL update: #1229 and #1214 later join generation activation to this transaction. A gated report without a valid permit updates nothing and remains non-terminal. Legacy rows complete unchanged.

### D5: Keep current alignment, bind the final head

Do not move the large ADR-456 alignment implementation in this child. After the lead's structural screen, the runner follows the existing scan/alignment/push path. Define `H` as the exact source-branch tip that this path successfully landed. If alignment rewrites the earlier candidate, structural milestone membership is head-independent, so the permit request simply uses `H`. Keep `push_secret_blocked` and existing secret-safe preservation behavior unchanged.

After the final push, request the permit for `H`, create/adopt the PR, then query its head SHA through a new forge-neutral method implemented by the GitHub, GitLab and Forgejo client classes in `agent/src/forge.ts`. A mismatch invalidates the permit and cannot report completed. No `Closes` body is rendered without a current structural permit.

### D6: Add a dedicated completion hold

Add `SetRunCompletionHold`, not a widened `SetRunPaused`. It admits only an owned gated run in `running` or `awaiting_input`, requires at least one completion attempt, stamps `hold_reason=completion_blocked` and `hold_captured_head`, clears the completion question, and transitions to `paused` with a positive returned-status acknowledgement.

The worker-authored completion question does not use `question_timeout_seconds` or throw `REASON_QUESTION_TIMEOUT`. Add claim-delivered `completion_hold_window_seconds`, default 900 seconds. An owner `continue` inside that live window resumes in place. Expiry invokes the verified park order below; it never becomes `failed`.

Park ordering is fixed for later children:

1. reap the agent tree;
2. WIP-commit dirty work;
3. fetch into the worker bare and verify its tracking ref covers the clone head;
4. publish the checkpoint and receive a positive acknowledgement;
5. call `captureHoldContext()`;
6. write `SetRunCompletionHold` and require returned status `paused`;
7. only then release/cleanup as a parked run.

This child ships `captureHoldContext()` as an explicit `same_worker_only` result. If Git capture cannot be verified, retain the live clone/HOME/worker and retry; never park or cleanup. A multi-day hosted hold will usually re-claim elsewhere after the existing affinity ceiling, so this limitation is visible and is not presented as durable cross-worker recovery.

### D7: Reserve one decision path

Add `run_user_inputs.kind = completion_decision` and one owner/admin endpoint. This child supports only `{decision: continue, guidance?}` in both the live `awaiting_input` window and `paused` hold. It records the decision, resumes when parked, carries `hold_reason` and last attempt on the claim, injects guidance and clears the hold on the first accepted running report.

#1227 extends the same kind and endpoint with `partial` and `accept`; it does not create another control plane.

### D8: Make state honest

The run detail/feed/web/CLI distinguish **Checking completion**, **Reworking unmet milestones** and **Completion blocked**. They show bounded unmet IDs, attempt count and `hold_context = unavailable(same_worker_only)`. They do not expose raw model output, repository text, credentials or arbitrary logs.

The temporary server rollout switch defaults off until the API and capable worker image are deployed. New enabled issue runs are interlocked; old/preexisting runs remain explicitly legacy. This is a rollout control, not a permanent owner opt-out.

## Milestones and dependency plan

Six sequential milestones because API and agent contracts cross at each boundary. All are worker-runnable; hosted-k8s observation is not frozen here.

| Phase | Milestone | Dependencies | Primary files/areas | Validation outcome |
|---|---|---|---|---|
| 1 | M1: structural contract, schema and hard claim clause | D1-D2 | migrations, `runtime.sql`, run creation/planning/workersvc, DTOs/protocol | Version is stamped before claim; contract freezes atomically; overrides and kill-switch cannot bypass the worker requirement. |
| 2 | M2: attempts, permit route and completion transaction | M1 | completion store/service, worker handler, LiveDB tests | Exact idempotent permits issue; missing/stale permits never complete. |
| 3 | M3: same-lead attempt loop and stall/budget routing | M2 wire contract | `sdk-executor.ts`, runner checkpoint callbacks, agent tests | Missing work returns to the same session and never calls the forge. |
| 4 | M4: final-head publication and recoverable hold | M2-M3 | `runner.ts`, `forge.ts` interface plus its three client classes, hold writer/runner tests | Permit binds final head; PR head matches; incomplete no-progress parks without losing Git work. |
| 5 | M5: owner continue, web and CLI | M4 | user handler, run DTOs, web run view, `api/cmd/uzi/` | Owner can continue with guidance; durability limitation is visible. |
| 6 | M6: docs, specifications and adversarial regression | M1-M5 | docs, embedded mirror, ADR 1225, specs, integration tests | #1220 is deterministically blocked; structural contract is documented honestly. |

- [ ] **M1: structural contract, schema and hard claim clause.** Add additive fields/tables and bounds; stamp `completion_contract_version=1` in `CreateRun` before the first claim when rollout is enabled; freeze `profile=structural` content with `milestones_frozen` on human approval and autopilot running reports. Add the unconditional ClaimRun compatibility clause outside both override paths. Surface a capable-worker wait. LiveDB tests must prove a fresh interlocked row is not claimable by an incapable worker before approval, and that owner override plus `capability_aware=false` cannot bypass it, while a legacy run remains claimable. Gates: `task gate:api`, `task gate:agent`, `task scan:secrets`.
- [ ] **M2: attempts, permit route and completion transaction.** Implement claim-fenced idempotent permit requests, exact head/revision identity, bounded denial summaries and the extensible optional audit/finding fields. Implement transactional permit consumption plus completed state, with a seam for later generation activation. LiveDB tests cover duplicate request, stale claim, wrong head/revision, decision race, retry after response loss and legacy passthrough. Gates: `task gate:api`, relevant LiveDB tests, `task scan:secrets`.
- [ ] **M3: same-lead attempt loop and stall/budget routing.** Turn gated `signal_done` into a checkpoint-first structural attempt before `turn.done` breaks. Feed missing IDs back through the existing follow-up path. Add the distinct completion fingerprint; route `REASON_NO_PROGRESS`, `REASON_MAX_ITERATIONS`, `REASON_WALL`, `REASON_IDLE` and the served post-attempt `budget_exhausted` flag to `enterCompletionHold(reason)`; exempt live post-attempt rows from `SweepRunningTimeout`; keep pre-attempt legacy failures unchanged. Tests prove decreasing unmet sets continue, three identical attempts hold, and a live post-attempt row past its wall is not server-failed. Mutating away the sweeper carve-out must redden that LiveDB assertion. Gate: `task gate:agent`, `task gate:api`.
- [ ] **M4: final-head publication and recoverable hold.** Integrate the permit after today's scan/alignment result, keep security failures typed, add final PR-head reads for all three forges, and make `Closes` conditional on the permit. Add the dedicated completion-hold writer and fixed capture hook order. A failed/unverified Git capture keeps the worker/session live. Tests cover alignment head change, PR-head race, worker death around permit/PR, refused hold ACK and no cleanup before positive capture. Gates: `task gate:api`, `task gate:agent`, relevant LiveDB tests, `task scan:secrets`.
- [ ] **M5: owner continue, web and CLI.** Add the completion question discriminator, claim-delivered `completion_hold_window_seconds` (default 900), exemption from the normal question-timeout failure, `completion_decision=continue`, resume semantics and guidance injection. Expiry enters the verified hold and never reports failed. Render interlock/attempt/hold/capability/context states in web and CLI JSON; add `uzi run decide <id> --continue [--guidance]`. Carry hold state through resume and clear it only when the decision is acted on, not prematurely in `ResumePausedRun`. Gates: `task gate:web`, `task gate:api`, `task gate:agent`.
- [ ] **M6: docs, specifications and adversarial regression.** Add ADR 1225 for the structural contract, hard capability clause, permit, exact head, same-lead rejection and hold ordering. Update run lifecycle/autopilot/pause/CLI docs and run `task docs:sync`. Add the user-approved structural requirement to `specs/human.md`: "An issue run may open a closing PR only after every in-scope approved milestone is declared complete against a frozen contract for the exact final head, or the owner records an explicit later decision. An incomplete attempt returns to the same lead and otherwise holds without discarding its work. [user, #1226]" Add technical requirements to `specs/ai.md`. Reproduce run `b0ffd0ea`: frozen five, declared four, no scope cap, 17 iterations remaining. The fixed path checkpoints then names M5 to the same session and makes zero forge-create calls; deleting the missing-milestone denial makes the mutation call create with `Closes`. Run relevant component gates once to logs.

## Acceptance criteria

- The #1220 fixture cannot call `createMergeRequest` when one frozen milestone is missing.
- Same-lead rework receives the exact missing milestone after a verified checkpoint.
- An incapable worker cannot claim an interlocked run through either owner override or the capability kill-switch.
- Gated completion without a matching unconsumed permit/head/revision remains non-terminal; legacy completion remains unchanged.
- The final PR head matches the permit, and no unmatched head reports completed.
- Three identical incomplete attempts or post-attempt budget exhaustion reach the completion hold, not `failed`.
- Worker wall/idle exhaustion and the server running-timeout sweep cannot terminal-fail a live run after its first completion attempt; stale workers still requeue.
- The dedicated 900-second completion-question window expires into the verified hold, never `REASON_QUESTION_TIMEOUT`.
- `SetRunCompletionHold` works from the intended owned `running|awaiting_input` sources and does not weaken `SetRunPaused`.
- No hold releases/cleans the only Git work copy when capture or acknowledgement is uncertain.
- The UI/CLI state calls provider context `same_worker_only`; it does not claim cross-worker durability.
- Only a permitted structural full delivery contains `Closes #N`.
- All regression mutations fail in the intended assertion channel and pass when restored.
- No workflow files, real credential-shaped literals or open-web implementation dependencies are introduced.

## Risks

- **Structural declarations can lie.** This child catches missing IDs and explicit contradictions, not arbitrary semantic incompleteness. #1230/#1231 close that gap and the UI/docs state it.
- **Alignment remains late.** This avoids a risky rewrite of the large ADR-456 finalizer. Structural membership is head-independent; semantic evidence drift is handled later by #1231.
- **Other finalize failures remain typed.** Base-align conflict, non-fast-forward and push-secret paths retain today's typed failure/preserved-patch behavior. This child fixes incompleteness-driven completion, not every finalization failure, and never exposes secret material to a rework prompt.
- **Same-worker context can disappear.** Git is protected, but provider conversation survives only while that worker survives until #1229. The state is visible and #1226 does not claim the epic complete.
- **Held worker capacity.** Unverified capture holds a slot by design; owner cancel and later durable export bound the operational cost.
- **Wedged live worker.** The running-timeout carve-out cannot silently become infinite: use the dedicated completion transition/window deadline for health visibility, keep stale-worker requeue active and never convert the preservation exception into a terminal failure. The run page must explain the 900-second live window and same-worker/affinity limitation.
- **Worker trust.** A compromised PAT-holding worker is outside scope. The hard claim clause prevents ordinary version skew, not compromise.
- **Cross-component size.** Six milestones span API, agent, web and CLI, comparable to the shipped pause slice; later semantic/context work is explicitly excluded.

## Review record

- 2026-09-09: Split from epic #1225 after `@help1` and `@brainstorm` independently verified the original PRD was too large. Their blocking corrections define the hard claim clause, dedicated hold writer, stall routing and worker-runnable boundary. No implementation started.
- 2026-09-09: Final peer review verified the pre-claim version stamp, wall/idle/sweeper routing, dedicated 900-second owner window and semantic-capability extension. No blocking finding remains; same-worker context duration and one-shot served-flag handling are explicit residual risks.
