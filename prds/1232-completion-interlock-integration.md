# PRD #1232: Completion interlock structural assurance and epic close-out

**Issue:** [#1232](https://github.com/vtmocanu/uzi/issues/1232)
**Parent epic:** [#1225](https://github.com/vtmocanu/uzi/issues/1225)
**Status:** Rewritten 2026-10-23 for the re-scoped epic (structural only); revised the same day after lead and peer review. Anchors verified against origin/main `12f6df6edd6ff4e9812b9fd8bb84209fee5a0b80`.
**Priority:** Normal.
**Depends on:** PR #3356 merged. It records #1230's m6 evidence, moves the #1230 PRD to prds/done/1230-semantic-completion-contract.md, closes #1230, and edits `api/internal/workersvc/semantic_transport_doc_test.go`, `adr/2149-cross-check.md` and the #1225 epic PRD, all of which M4 also edits. The run must start from a base that contains #3356.
**Execution:** Keep `Planned` without `uzi` until this rewrite is approved and #3356 has merged. One gated run, one PR, four serial milestones. The run needs Docker: `./e2e/run-store-it.sh` for the LiveDB gate, and a session-owned throwaway Postgres for the M3 mutation controls. No implementation or validation may create or modify `.github/workflows/**`.

## Problem

The structural completion interlock shipped in four pieces: #1226 (frozen contract, hard claim clause, exact-head permit, same-lead rework, completion hold), #1227 (owner continue/partial/accept), #1626 (default on, admin kill-switch, same-branch guard) and #1627 (Codex harness). Each child tested its own seams. Nobody has checked the shipped lifecycle as a whole against the #1220 failure (run `b0ffd0ea`: five frozen milestones, four declared, closing PR opened anyway), and the epic's records still describe a semantic audit that will not ship.

Re-scope (user decision 2026-10-23, not reopened here): #1231 and #1233 are closed not planned. #1230's semantic contract stays merged and dormant (neither activated nor removed), and its m6 check is evidenced by #3356. #1228 is detached and parked. #1229 is not planned.

## Outcome

1. An evidence table mapping every structural lifecycle seam to the tests that already cover it, with the real gaps named.
2. Tests for those gaps only, plus an isolated red/green mutation proof for each load-bearing guard.
3. ADR-1225, docs, specs and the epic PRD state what shipped (structural interlock on both harnesses, on by default) and state plainly that semantic completion auditing did not ship.
4. The epic closes: its PRD moves to `prds/done/` in this PR, the remaining hosted checks are carried into the acceptance ledger #3346 unticked, and #1225 is then closed.

No production behavior changes. If a test or mutation exposes a real defect, the run stops and reports it as a material plan change. It does not fix the defect inside this PRD.

## Included

- One LiveDB lifecycle test file that drives the #1220 shape through real service entry points, including continue, partial and accept owner paths (M1).
- A recorded worker-wire fixture for completion attempt and permit responses with value-level fake-api parity, a handler test for `/completion/hold` status mapping, and a Codex arm of the #1220 executor regression (M2).
- An isolated mutation audit of the guards listed in D4, with any surviving mutation closed by a test (M3).
- Records consolidation, PRD archival and epic close-out (M4).

## Excluded

- Semantic enrollment, coverage, audit, permits, blocker dispositions, governed MR rework, provider-context generations and any semantic default-on or rollback proof.
- Removing or activating #1230's dormant code, its capabilities or its tests (other than rewording the doc sentences pinned by `TestSemanticTransportLimitsDocumented`, D6).
- A new compose E2E phase, a new stub-executor sentinel, or a cross-process real-agent harness (D2).
- Production code, schema, migration, setting, capability or wire changes.
- Editing reserved-slot comments that mention #1231 in migrations (`00211_completion_contract.sql`, `00212_completion_attempts_permits.sql`), `api/internal/store/queries/runtime.sql` and its generated code. They describe reserved JSON slots that still exist, and applied migrations are immutable.
- `specs/ai.md` (frozen).
- Release and deploy. Executing or ticking the hosted maintainer checks.

## Evidence inventory (origin/main `12f6df6ed`)

Test names are the `func Test…` name (Go) or the `it(…)` title prefix (TypeScript). "LiveDB" tests run only under `./e2e/run-store-it.sh` (or CI `test-api-store-it`), which selects them with `-run 'LiveDB$'`.

| Seam | Existing coverage (file: test) | Gap |
|---|---|---|
| Structural denial before any closing PR (#1220, 5 frozen / 4 declared), Claude | `agent/test/runner-completion-1220-regression.test.ts`: "b0ffd0ea shape: 5 frozen / 4 declared", "a held incomplete run makes ZERO forge-create calls", "mutation control (positive): a fully-complete run opens exactly ONE MR" (mutation recorded in `prds/done/1226-structural-completion-interlock.md` M6). `agent/test/runner-completion-permit.test.ts`: "permit DENIED → never creates the MR". `api/internal/workersvc/completion_permit_livedb_test.go`: `TestPermitMissingMilestonesLiveDB`, `TestCompletionWithoutPermitStaysNonTerminalLiveDB`. `api/internal/workersvc/completion_permit_test.go`: `TestComputeUnmetCriteria` | Agent side runs against `agent/test/fake-api.ts`; API side seeds runs with direct SQL (`seedFrozenRun`, `parkCompletionBlocked`). No test drives one run from create to completion through the real service entry points. **M1** |
| Same, Codex | `agent/test/codex-executor.test.ts` (`describe("Codex completion interlock")`): "checkpoints before the server attempt and reworks on the same thread", "asks at three identical attempts, continues with guidance, then holds on expiry" | No b0ffd0ea-shaped (5/4) Codex case and no positive control. **M2** |
| Same-session rework, stall, post-attempt budget routing | `agent/test/runner-completion-attempt.test.ts`: "(b) a decreasing unmet set re-prompts the SAME session", "(c) three identical (unmet, head, worktree) attempts route to enterCompletionHold", "(e) a POST-attempt WALL trip routes to enterCompletionHold", "(m5) a 'continue' at the stall resumes the SAME session". `agent/test/runner-completion-stall.test.ts`: "stall attempt then …" | None |
| Exact-head permit | `completion_permit_livedb_test.go`: `TestPermitHeadNormalizationLiveDB`, `TestCompletionWrongHeadStaysNonTerminalLiveDB`, `TestPermitBoundToBranchLiveDB`, `TestPermitRevisionDriftLiveDB`, `TestCompletionRevisionBumpUnderLockStaysNonTerminalLiveDB`. `runner-completion-permit.test.ts`: "granted but PR head != H", "head matches at verify but CHANGES before the post-add re-verify". `agent/test/forge.test.ts`: `getMergeRequestHead` for GitLab, Forgejo and GitHub | None (mutation proof only, M3) |
| Permit/attempt wire (Go DTO vs agent decode vs fake-api) | `api/internal/handler/completion_scope_cap_test.go`: `TestWorkerCompletionPermitScopeCapJSON` (substring checks). `api/internal/handler/completion_non_closing_test.go`: `TestWorkerCompletionAttemptNonClosingJSON`. Permit grant crosses the real wire only in `e2e/phases/52-api-outage-outbox.sh` cases 6 to 8 | Attempt `unmet` and permit denial never cross between modules; `fake-api.ts` is an unpinned second implementation whose values (for example `unmet: []` versus `["m5"]`) nothing compares with the API's. **M2** |
| Response loss or death before permit | `agent/test/client-completion-permit-retry.test.ts`: "a transport failure (fetch failed) is retried until the api answers", "a permanent HTTP error (409 stale claim) throws immediately". `runner-completion-permit.test.ts`: "permit request hits an api OUTAGE" | None |
| Response loss or death after permit, before or after PR | `TestPermitIdempotentReissueLiveDB`, `TestPermitRequeueRebindsWorkerLiveDB` (`completion_permit_livedb_test.go`); `TestFinalizeResumeAllowanceCompletesOnlyAtNewGenerationLiveDB` (`issue1742_finalize_resume_permit_livedb_test.go`); `agent/test/issue-1742-finalize-hardening.test.ts`; `e2e/phases/52-api-outage-outbox.sh` case 6 (interlocked finish during outage) and cases 7 to 8 (agent SIGKILL across finalize, interlock on) | None |
| PR create/adopt, closing line | `runner-completion-permit.test.ts`: "granted + PR head matches H → opens the MR WITHOUT Closes…", "PR head != H AND the non-closing strip write FAILS", "opts into a capped permit and preserves a non-closing partial MR", "LEGACY (non-interlocked) run" | None |
| Hold report (service) | `completion_permit_livedb_test.go`: `TestSetRunCompletionHoldLiveDB`, `TestEmptyDeclarationAttemptThenHoldLiveDB`; `api/internal/store/completion_hold_reclaim_livedb_test.go`: `TestCompletionHoldHistoricalAttemptAfterReclaimLiveDB`; `api/internal/workersvc/major5_completion_fence_livedb_test.go`: `TestMajor5CompletionHoldGenerationFenceLiveDB` | A repeat hold after an applied hold (lost ACK) is not asserted on the server. **M1** |
| Hold report (HTTP mapping and worker ACK) | `agent/test/runner-completion-hold.test.ts`: "verified capture + paused ACK → held", "ambiguous ACK + exact running snapshot retains clone and HOME", "completion hold ambiguous wire acknowledgements", "refused hold ACK (non-paused) → false". `agent/test/client-completion-send-gate.test.ts`: "a 409 refusal is NOT an error and NOT a strict-decode trigger" | `WorkerRunCompletionHold` (`api/internal/handler/worker_protocol.go`) has no Go handler test; its 200/409/404 contract is pinned only against `fake-api.ts`. **M2** |
| Owner decision continue / partial | `api/internal/workersvc/completion_decision_livedb_test.go`: `TestCompletionDecisionPausedResumesLiveDB`, `TestCompletionDecisionForeignUserRefusedLiveDB`, `TestCompletionDecisionNotBlockedRefusedLiveDB`. `api/internal/workersvc/completion_owner_decision_livedb_test.go`: `TestDecideCompletionPartialHappyPathLiveDB`, `TestDecideCompletionNonOwnerHiddenLiveDB`, `TestDecideCompletionConflictLiveDB`, `TestDecideCompletionIdempotentRepeatLiveDB`. `api/internal/workersvc/completion_scope_reduced_stamp_livedb_test.go`: `TestCompletionScopeReducedStampLiveDB` (resume simulated by SQL UPDATE). `api/internal/handler/completion_decision_test.go`: `TestCompletionDecisionHandlerStatusMappingLiveDB`. `api/cmd/uzi/run_decide_test.go`: `TestRunDecidePartial`. `web/src/pages/RunView.test.tsx`: "hides every Resume control on a completion-blocked run" | A decision followed by a real reclaim, permit and completion is never chained. **M1** |
| Owner decision accept | `completion_owner_decision_livedb_test.go`: `TestDecideCompletionAcceptHappyPathLiveDB` (stops at the contract ledger and the unmet check); `api/cmd/uzi/run_decide_test.go`: `TestRunDecideAccept`; `agent/test/runner-mr-completion-scope.test.ts`: the `mrCompletionBlock` owner ACCEPT describe block (PRD #1227 M3) | No accept decision is followed through reclaim, revision-bound permit and completion; nothing asserts that an accepted criterion stays owner-accepted (never added to `milestones_completed`) or that an accepted delivery is distinct from `scope_reduced`. **M1** |
| Owner decision `authorize_maintainer` on a structural run | Guard `prior.Profile != "semantic"` in `api/internal/workersvc/completion_decision.go`; only semantic-profile tests exist (`semantic_completion_livedb_test.go`, `semantic_completion_test.go`); `TestValidateDecision` has no `authorize_maintainer` case | Structural refusal untested. **M1** |
| Stale claims and generation fences | `TestPermitStaleClaimLiveDB`, `TestCompletionGenerationFenceLiveDB` (`completion_permit_livedb_test.go`); `TestMajor5CompletionAttemptGenerationFenceLiveDB`, `TestMajor5CompletionPermitGenerationFenceLiveDB` (`major5_completion_fence_livedb_test.go`); `agent/test/runner-completion-revision-ack.test.ts`: "a STALE-claim ACK" | None (M1 re-asserts them after a real reclaim) |
| Old/new worker skew, Claude | `api/internal/workersvc/completion_interlock_livedb_test.go`: `TestClaimInterlockHardClauseBlocksIncapableWorkerLiveDB`, `TestClaimLegacyRunClaimableByAnyWorkerLiveDB`, `TestClaimInterlockPeerSpreadMirrorLiveDB`; `api/internal/workersvc/health_completion_capability_livedb_test.go`: `TestQueuedReasonNoCompletionCapableWorkerLiveDB`; `client-completion-send-gate.test.ts`: "old→new→old→new skew: strips+retries ONCE"; `runner-completion-permit.test.ts`: "old API falls back to claim scope"; `runner-completion-revision-ack.test.ts`: "claim WITHOUT contract_revision + ACK carries completion_revision 1"; `agent/test/worker.test.ts`: "completion_interlock_v1 always present" | None |
| Old/new worker skew, Codex | `api/internal/workersvc/codex_completion_interlock_livedb_test.go`: `TestCodexCompletionClaimLiveDB`, `TestLegacyCodexCompletionClaimLiveDB`, `TestCodexCompletionPeerMirrorLiveDB`, `TestCodexCompletionHealthIntersectionLiveDB` | None |
| Admin kill-switch at create | `api/internal/workersvc/completion_default_on_livedb_test.go`: `TestCompletionInterlockDefaultOnStampsClaudeLiveDB`, `TestCompletionInterlockDefaultOnStampsCodexLiveDB`, `TestCompletionInterlockExplicitFalseKeepsClaudeLegacyLiveDB`, `TestCompletionInterlockDefaultOnKeepsSeededCodexLegacyLiveDB`; `api/internal/settings/settings_completion_interlock_test.go`: `TestCompletionInterlockRolloutColdErrorReturnsDefaultAndError`, `TestCompletionInterlockRolloutStaleOnRefreshErrorKeepsCachedValue`; `web/src/pages/AdminSettings.test.tsx`: "saves the flag OFF sending only completion_interlock_rollout"; phase 52 toggles it | Flipping the switch after a run exists is untested. ARCHITECTURE.md says the switch "is read once, at create"; nothing pins that a stamped run keeps its clause and permit requirement, or that a legacy run stays legacy. **M1** |
| Served budget-exhausted flag | `api/internal/store/completion_sweep_carveout_livedb_test.go`: `TestSetRunRunningPreservesCompletionBudgetExhaustedLiveDB`, `TestCompletionInterlockSweepCarveOutLiveDB` | None |

Not reused, by decision (D2): the opt-in cross-process proof `TestHeartbeatProtocolRefreshE2E` (`api/internal/handler/heartbeat_protocol_refresh_e2e_test.go`) and the compose E2E stub executor, which never makes a completion attempt.

## Decisions

### D1: Evidence first, gaps only

The inventory above is the scope. Each milestone adds tests only for a row marked as a gap. Before writing, the run re-checks each row at its own base. A row that is already covered by then is recorded as covered and skipped, never duplicated.

### D2: Prove integration at the store, wire and executor seams, not with a new harness

The cross-service risk sits in three places. (1) Server ordering across transactions: create, approve, claim, attempt, permit, hold, decision, reclaim, completion. (2) The worker wire between the Go DTOs, the agent decoder and the agent's fake API. (3) Executor behavior. M1 covers (1) with one real-Postgres test file through the production `Service` methods. M2 covers (2) with recorded fixtures read by both modules, following `fixtures/worker-state-request/README.md`, and covers (3) with the Codex arm. Phase 52 already crosses the real stack for permit grant, finalize and agent death.

Rejected: a compose E2E phase reproducing #1220. It needs a new completion-declaring stub sentinel in `agent/src/executor.ts`, which is shipping worker code, and minutes of race-sensitive runtime per case, to prove what M1 and M2 prove in seconds. Also rejected: a cross-process real-agent proof modeled on `TestHeartbeatProtocolRefreshE2E`. It is opt-in, builds pinned exports, and adds nothing for skew, because the hard SQL claim clause already decides skew and is pinned by LiveDB tests.

### D3: The kill-switch is read once, at create

`completionInterlockOn` (`api/internal/workersvc/health.go`) is the only production reader of `completion_interlock_rollout`, and run creation is its only caller. M1 pins the consequence both ways. A stamped run keeps its claim clause and permit requirement after an admin turns the switch off. A legacy run stays legacy after the switch is turned on. This guards against a future change that reads the switch at claim or permit time.

### D4: Isolated, independently targeted mutation proofs

Each mutation runs in its own frozen scratch export, never in the run's working checkout.

1. Commit the M1 and M2 tests first. Record the commit SHA S and confirm the working tree is clean.
2. For each guard, export S into a fresh session-owned scratch directory (`git archive S | tar -x`; agent tests reuse the checkout's `agent/node_modules` by symlink, as `refreshExport` in `heartbeat_protocol_refresh_e2e_test.go` does). Apply exactly one mutation and record its diff. Run only the focused target tests, serially. Delete the export.
3. A mutation counts as red only when the named test fails on its intended assertion; record that assertion's message. A compile error, setup failure, timeout or a different test failing does not count. Rework the mutation until it compiles and reaches the guard.
4. Go LiveDB targets run against one session-owned throwaway Postgres container, named outside the `uzi-` namespace (for example `mut1232-pg-<pid>`) and removed by exact name at the end, with `UZI_TEST_DATABASE_URL` set and `go test -race -count=1 -p 1 -run '^<Test>$'`. Do not run the full LiveDB sweep per mutation.
5. After the last mutation, verify the working checkout is still exactly S (`git status` clean, `git rev-parse HEAD` = S). Then run the unmodified `./e2e/run-store-it.sh` sweep once. That sweep is the integration gate. Each mutation also records the same focused target passing on its unmodified source SHA. The final LiveDB sweep may supply that control for Go LiveDB targets. Agent mutation targets (G2a, G2b, G9) require their own unmodified focused green runs. Record source SHA, command, intended red assertion and matching green result.
6. When a mutation survives, add the missing test (test-only), commit, and repeat steps 2 to 5 for that guard.

For SQL guards, mutate the generated query string in the export's `api/internal/store/runtime.sql.go` (the code the tests execute), so no `sqlc generate` round trip is needed. Where a predicate appears more than once in one query (the `SetRunCompletionHold` CTE and its UPDATE both carry the G7 predicates), remove it from every occurrence in that query.

| # | Guard (file: symbol) | Focused target, expected red |
|---|---|---|
| G1 | missing-milestone denial: `computeUnmetCriteria` (`api/internal/workersvc/completion_permit.go`) | `TestComputeUnmetCriteria`, `TestPermitMissingMilestonesLiveDB`, M1 continue case |
| G2a | Claude executor gate: the `unmet.length === 0` branch after `recordAttempt` in `agent/src/sdk-executor.ts` (origin/main lines 3949 to 3965) | `runner-completion-1220-regression.test.ts` Claude arm "b0ffd0ea shape: 5 frozen / 4 declared" |
| G2b | Codex executor gate: the `unmet.length === 0` branch after `recordAttempt` in `agent/src/codex/codex-executor.ts` (origin/main lines 4398 to 4400) | the M2 Codex arm of the same file |
| G3 | Claude claim clause `completion_interlock_v1` (ClaimRun in `runtime.sql.go`) | `TestClaimInterlockHardClauseBlocksIncapableWorkerLiveDB`, M1 continue case |
| G4 | Codex claim clause `codex_completion_interlock_v1` | `TestCodexCompletionClaimLiveDB` |
| G5 | exact-head consume in the completion transaction | `TestCompletionWrongHeadStaysNonTerminalLiveDB` |
| G6 | permit claim/generation fence | `TestPermitStaleClaimLiveDB`, `TestMajor5CompletionPermitGenerationFenceLiveDB` |
| G7a | hold guard `completion_contract_version IS NOT NULL` (`setRunCompletionHold` in `runtime.sql.go`) | `TestSetRunCompletionHoldLiveDB` case 6 (legacy run refused) |
| G7b | hold guard `completion_attempts > 0` (same query) | `TestSetRunCompletionHoldLiveDB` case 3 (zero attempts refused) |
| G8 | owner-only decision authority | `TestCompletionDecisionForeignUserRefusedLiveDB`, `TestDecideCompletionNonOwnerHiddenLiveDB` |
| G9 | worker adds `Closes` only after head verification | `runner-completion-permit.test.ts` "granted + PR head matches H → opens the MR WITHOUT Closes…" |
| G10 | `SetRunRunning` keeps `completion_budget_exhausted_at` | `TestSetRunRunningPreservesCompletionBudgetExhaustedLiveDB` |
| G11 | stamping honors the switch (`completionInterlockOn`) | `TestCompletionInterlockExplicitFalseKeepsClaudeLegacyLiveDB`, M1 kill-switch case |
| G12 | `authorize_maintainer` refused on structural (`completion_decision.go`) | the M1 `TestValidateDecision` case |

The coder picks the smallest mutation that disables each guard (for example, inverting the predicate or returning the pass value) and records it in the PR.

### D5: Close out honestly and close the epic in this change

M4 rewrites the records so that a reader of ADR-1225, the docs or the specs learns three things: the structural interlock shipped and is on by default on both harnesses; semantic completion auditing did not ship (#1231 and #1233 not planned); and #1230's semantic path is dormant code with no production enrollment. Historical design text stays where it is and is labelled historical, never deleted.

#1225's closing conditions are #1232 merged and #1230's m6 evidenced; #3356 supplies the second. So this PR moves the epic PRD to `prds/done/`, following #3346's rule that live acceptance never keeps a PRD open. The hosted checks that remain are not ticked. They are carried into the ledger #3346 before #1225 closes (see "Hosted checks carried to #3346").

### D6: Reword the test-pinned #1231 preconditions

`TestSemanticTransportLimitsDocumented` (`api/internal/workersvc/semantic_transport_doc_test.go`) pins two sentences that name #1231 as a live obligation. After #3356 they live in:
- "#1231 must establish enforceable bounds before activation.": ADR-1225, `docs/cross-check.md`, `docs/autopilot.md`.
- "#1231 must verify maximum-size owner reads and decisions against production transport deadlines before activation.": ADR-1225 and prds/done/1230-semantic-completion-contract.md.

Reword both to "Any future semantic activation must establish enforceable bounds before activation." and "Any future semantic activation must verify maximum-size owner reads and decisions against production transport deadlines before activation." Change every pinned document and the test's literals in one commit, so the test passes before and after that commit and is never red in between. The preconditions still bind any reactivation of #1230's dormant path, so the pin keeps its purpose. Then run `task docs:sync`, because the two docs are mirrored into `api/internal/uzidocs/embed/`.

### D7: Maintainer proof stays outside the worker contract

Hosted-k8s worker replacement, image roll, API restart under GitOps and live forge rendering need maintainer authority. The run never executes or ticks them. They move to the ledger #3346, which is where the repo tracks owed validation of archived PRDs.

## Milestones

M1 → M2 → M3 → M4, serial in one run. M1 and M2 own disjoint files, but M3 consumes both.

- [ ] **M1: structural lifecycle on the real store.** Create `api/internal/workersvc/completion_lifecycle_livedb_test.go`. Every test name ends in `LiveDB`, or the sweep's `-run 'LiveDB$'` never selects it. After seeding users, repo, issue and workers through existing helpers, drive every state change through production `Service` methods (`CreateRun`, the plan-gate `SetState` and `SubmitInput` approve path, `Claim`, `SetState`, `RecordCompletionAttempt`, `RequestCompletionPermit`, `SetRunCompletionHold`, `ContinueCompletionDecision`, `DecideCompletion`). Do not write runs status, worker, milestone, contract or permit columns with SQL; the `app_settings` row is the one allowed direct write. Cases:
  - (a) #1220 continue path. Default-on Claude run freezes m1..m5. An incapable worker claims nothing; a capable worker claims. An attempt declaring m1..m4 returns unmet `[m5]`. A permit at head H is denied (`missing_milestones`). `completed` without a permit leaves the run non-terminal. Identical attempts then a hold at H gives `paused` / `completion_blocked`. A repeated hold returns not-applied with status `paused` and leaves the attempt count unchanged. A foreign user's continue is refused; the owner's continue requeues. Reclaim gives a new generation, and old-generation attempt, permit and state writes are refused. Declaring m5 gives a granted permit at H2, and a repeat request returns the same permit. `completed` at H2 terminates the run with one consumed permit and a NULL `stop_kind`, and a replayed `completed` changes nothing.
  - (b) Partial path. Same up to the hold, then the owner keeps m1..m4 with a reason. After reclaim, the revision-2 permit is granted without m5 and its `completion_scope` lists m5 as deferred. Completion stamps `stop_kind = scope_reduced`.
  - (c) Accept path. Same up to the hold, then the owner accepts `m5.c1` with a reason. After reclaim, an attempt still declaring m1..m4 returns no unmet criteria. The revision-2 permit is granted at the exact head and revision; its `completion_scope` carries `m5.c1` as accepted and nothing deferred, and a permit request at revision 1 is refused. Completion terminates the run with a NULL `stop_kind`, not `scope_reduced`. Afterwards `milestones_completed` still excludes m5 and the contract's `accepted` block still records `m5.c1` with the owner's reason: owner-accepted, never worker-completed.
  - (d) Harness parity. For a Codex-harness run, cover (a)'s first denial and final grant, reusing the claim fixtures in `codex_completion_interlock_livedb_test.go`.
  - (e) Kill-switch after creation (D3). A stamped run with the switch then set `false` still refuses the incapable worker and still needs a permit. A run created under `false` and then set `true` stays legacy: any worker claims it and it completes with no permit.
  - (f) `authorize_maintainer` on a structural held run returns `ErrCompletionDecisionInvalid`, leaves `contract_revision` unchanged and writes no `completion_decision` input. Add the matching invalid case to `TestValidateDecision` and a 4xx row to `TestCompletionDecisionHandlerStatusMappingLiveDB`.
  - Gates: `./e2e/run-store-it.sh` to a log, with every new test listed as RUN and PASS, not skipped; `task gate:api` once to a log.
- [ ] **M2: wire, hold mapping and Codex #1220 arm.**
  - Add `fixtures/completion-wire/` with recorded `attempt-unmet.json`, `permit-denied.json`, `permit-granted.json` and a README in the style of `fixtures/worker-state-request/README.md`. The README defines the normalization: permit and run UUIDs and RFC 3339 timestamps are replaced with fixed placeholders, and nothing else is normalized.
  - Go side: a test in `api/internal/handler/` drives `WorkerRunCompletionAttempt` and `WorkerRunCompletionPermit` over the existing `protocolStore` fake and asserts byte-equal normalized JSON. The fixtures are recorded from this output, never hand-authored.
  - Agent side: `agent/test/client-completion-wire.test.ts` decodes each fixture through the real client. It also configures `fake-api.ts` for the same scenario and asserts the fake's normalized response deep-equals the fixture, values included: unmet IDs (`["m5"]`, not `[]`), denial reason, grant identity, head, contract revision and `completion_scope`. Matching keys and types alone does not pass.
  - Add a Go handler test for `WorkerRunCompletionHold` over the `protocolStore` fake: 200 with `paused`; 409 with the real status when refused (`running`) and on a repeat after an applied hold (`paused`); 404 when not owned; 409 when a capability worker omits `claim_generation`.
  - Extend `agent/test/runner-completion-1220-regression.test.ts` with a `CodexExecutor` arm of the b0ffd0ea shape (checkpoint first, m5 named on the same thread, never finalizes) and its positive control.
  - Gates: `task gate:api`, `task gate:agent`, each once to a log.
- [ ] **M3: mutation audit.** Run D4 for every guard in its table. Record a results table in this PRD's Decision Log: guard, source SHA, mutation diff, command, focused test that went red with its intended assertion text, and the matching unmodified green result (the final sweep's log line for Go LiveDB targets; a separate unmodified focused run for agent targets G2a, G2b and G9). A guard without both a recorded red and its matching green is not proven. Add a test for every survivor. A guard that turns out to be absent or broken in production code is a stop-and-report, not a fix. Without Docker, mark the LiveDB rows (G1, G3 to G8, G10 to G12) "not run" and leave this milestone unchecked; never record them as passed.
- [ ] **M4: records, archival and epic close-out.**
  - ADR-1225 (`adr/1225-structural-completion-interlock-invariants.md`): set Status to say the epic delivered the structural interlock only. Retitle the "Semantic default-on amendment" so it records that semantic auditing did not ship. Keep "Dormant-implemented by #1230" as is. Replace "Still planned (#1231)" with a short "Not shipped" note: #1231 and #1233 were closed not planned on 2026-10-23 because no supported credential path gives an enforceable whole-call bound for subscription credentials; the listed design is historical; any reactivation needs a new approved design. Drop the claim that #1232 proves semantic rollout and rollback. In I8, replace "#1228's job" with "#1228 is detached and parked; this epic plans no durable cross-worker context". Add a dated "Close-out (#1232)" addendum naming the M1 and M2 tests and the M3 results.
  - Apply D6 across ADR-1225, `docs/cross-check.md`, `docs/autopilot.md`, prds/done/1230-semantic-completion-contract.md and `TestSemanticTransportLimitsDocumented`, in one commit.
  - `adr/2149-cross-check.md`, "Enrollment boundary for #1231": add one sentence saying #1231 is closed not planned and the boundary binds any future enrollment.
  - Docs: in `docs/run-completion-hold.md`, `docs/autopilot.md`, `docs/cross-check.md`, `docs/scheduling.md` and `docs/worker-upgrades.md`, replace "until #1231" and "#1231 … is still open" with the not-planned fact. Run `task docs:sync` and commit the mirror.
  - `specs/human.md`: under Feature #1226, mark the two "Planned, not shipped (#1231)" entries and the #1231 mention in the #3090 paragraph as not planned, tagged `(AI-synced YYYY-MM-DD)`. Add one line stating the epic's delivered scope.
  - `ARCHITECTURE.md`: one sentence in the completion-hold note saying semantic completion auditing did not ship.
  - Epic PRD (prds/1225-completion-interlock.md): rewrite Status ("Closed 2026-10-23: structural interlock delivered; semantic auditing not shipped") and Execution ("None"). Update the child table: #1226, #1227, #1230 and #1627 link to their done PRDs, add #1626 (issue only), mark #1228 detached, and mark #1229, #1231 and #1233 not planned. Mark binding contracts 3, 10, 11 and 12 and the semantic acceptance rows as not delivered. Remove the #1214-consumes-#1228 acceptance row (it now belongs to #1228 and #1214). Replace its maintainer section with a pointer to #3346. Add a Review record entry. Then move it to prds/done/1225-completion-interlock.md.
  - Move prds/1231-completion-auditor.md to prds/done/1231-completion-auditor.md. Replace its stale Status line ("Blocked on D7/P1, undispatched …") with "Closed not planned (2026-10-23): no supported credential path gives an enforceable whole-call bound for subscription credentials; see #1225. The body below is the historical design." and set Execution to "None. Not planned; never dispatch.", matching `prds/done/1233-structured-blockers-mr-rework.md`.
  - Move this PRD to prds/done/1232-completion-interlock-integration.md with its milestones checked.
  - Fix every inbound link and backticked reference to the three moved files; `task check-docs:web` names any that are missed.
  - In the PR description, include the exact lines for #3346 from "Hosted checks carried to #3346" below, so the landing step can paste them unchanged.
  - Gates: `task check-docs:web`, `task gate:repo`, `task gate:api` (embedded-docs equality and D6), each once to a log.

## Acceptance criteria

- Every inventory row is either covered by a named, passing test or carries a recorded reason it is not.
- The M1 tests run (not skip) under `./e2e/run-store-it.sh`. Cases (a) to (f) pass, and no case writes runs lifecycle columns with SQL.
- The accept case shows an owner-accepted criterion that never enters `milestones_completed` and a completion with NULL `stop_kind`. The partial case shows `scope_reduced`.
- The M2 fixtures are read by one Go test and one agent test. Changing a field name or a meaningful value in the Go DTO or in `fake-api.ts` (for example returning `unmet: []` for the unmet scenario) reddens one of them.
- Every D4 guard (G2 and G7 split into separate proofs) has a focused red on its intended assertion, in an isolated export, plus a matching green of the same target on its unmodified source SHA (the single unmodified LiveDB sweep for Go LiveDB targets; a separate unmodified focused run for agent targets G2a, G2b and G9), with source SHA, command and assertion recorded. Or it has an explicit "not run" with its reason. The run's working checkout was never mutated. Survivors have tests.
- No file outside `*_test.go`, `agent/test/**`, `fixtures/**`, `docs/**`, `api/internal/uzidocs/embed/**`, `adr/**`, `prds/**`, `specs/human.md` and `ARCHITECTURE.md` changes. No changelog fragment is needed; docs commits use the `docs(...)` type. If a shipping path does change, add a fragment.
- No current doc, ADR or spec says semantic auditing is planned, live or pending #1231. Dormant #1230 code is described as dormant.
- The epic, #1231 and #1232 PRDs sit in `prds/done/`; `check-docs:web` passes with no stale link or backticked path.
- No `.github/workflows/**` change.

## Hosted checks carried to #3346

These are owed maintainer validation, not worker milestones. The run neither executes nor ticks them. At landing, before #1225 closes, append them unticked to the ledger #3346 under a "#1225 completion interlock" heading, in the ledger's own one-proof-per-line format:

- **#1225 (#1226)**: on hosted k8s, an incomplete completion reworks in the same provider session on the same worker.
- **#1225 (#1226)**: deleting a worker with a held run, then resuming on another worker, restores exact Git; provider context reports `same_worker_only`.
- **#1225 (#1626/#1627)**: a worker image roll with a mixed old/new fleet leaves interlocked runs visibly queued and never misclaimed, for both harnesses.
- **#1225 (#1226)**: an API restart between permit and terminal report recovers idempotently.
- **#1225 (#1227)**: full, partial and accepted-criteria PRs render the right closing behavior on the live forge.
- **#1225 (#1626)**: turning the admin Completion check off and on changes only runs created afterwards.

Landing order: merge after #3356; append these lines to #3346; then close #1225 with a comment linking the merged PR and the ledger lines. An implementation run cannot edit issues, so the landing session performs the #3346 append and the closure.

## Decision Log

- 2026-10-23: Revised after lead and Codex peer review. Decisions on the four open questions:
  - Q1: reword the pinned preconditions to "Any future semantic activation must …" and update every literal and the test together (D6).
  - Q2: #1230 m6 is evidenced by #3356, so this PR archives the epic PRD and #1225 closes after the remaining hosted checks are carried, unticked, into #3346 (D5). #3356 is a stated dependency.
  - Q3: keep the shared fixture, with value-level fake-api parity.
  - Q4: Codex first denial plus final grant is enough for the real-store parity case.
  Folded peer findings: an accept lifecycle case (M1 c); isolated per-guard mutation exports with focused serial LiveDB targets and one final unmodified sweep, G2 and G7 each split into two proofs (D4); value-level fixture parity (M2); the #1231 PRD's stale Status line fixed during its move (M4).
- 2026-10-23: Rewritten after the epic re-scope (user decision; semantic auditing cancelled). The old D1 to D5, its M1 to M5 and its semantic maintainer rows are superseded, not carried forward. The scope is the evidence inventory, gap tests, a mutation audit and records close-out. The architect inventoried coverage at origin/main `12f6df6ed`; every cited file and test name was checked to exist there.
- 2026-09-09: Split from #1225 as the integration and close-out child (historical).
