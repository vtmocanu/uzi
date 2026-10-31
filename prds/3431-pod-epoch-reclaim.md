# PRD #3431: Delete settled attempt trees at a verified pod epoch

**Status**: Draft, refreshed for the owner decisions of 2026-10-31; independent architecture reviews completed, with final clarifications folded below. PRD A (#3430, `prds/3430-pod-epoch-lock-shadow.md`) supplies lifetime locking, controller handoff and shadow evaluation. This PRD extends that shadow before enabling deletion. Compose (#3709) and plain hosted (#3710) reuse the existing discard core with their own exclusion proofs.

**Prerequisites:** PRD A is merged; #2852 slice C is satisfied, not an open dispatch blocker. The existing #3621 shadow acceptance remains required and is widened under M1a below; it alone does not validate the new contract.

## Problem and outcome

Settled attempt trees fill Docker workers' data volumes (#3329). At a verified eligible epoch under EX, before admission, reclaim bounded, freshly authorized sources through the existing discard core (ledger `retired`); keep all other sources. Extend the shadow evaluation narrowly for strict plain-journal/live-ledger settlement instead of restricting candidates to PRD A's old would-delete list. Separate physical deletion of present trees from metadata-only settlement of missing trees.

The epoch proof covers the immediate predecessor, intervening pods and concurrent workers. It does not exclude every historical writer: an older pod's process may survive its termination and still write after a later evidenced epoch. The owner accepted that residual on 2026-10-24. Keep stopped containers and existing exit-137 refusals; no new residual is inferred from missing evidence.

Acceptance examples: eligible settled sources are removed and retired while held/shared sources remain; a settled permission refusal keeps the source and yields a partial pass; unknown destructive work prevents admission and exits non-zero with the one-shot consumed. A missing tree is not counted as physical reclaim or proof of authority.

## Default-on evaluation and attestation (2026-10-31 refinement)

Evaluation defaults on for new installs and existing installs on chart upgrade. Keeping trees needs no declaration. Hosted-Docker deletion requires `workers.docker.podEpochReclaim.runtime` as the operator's attestation of the **full audited runtime/storage contract** in PRD A, plus the verified epoch proof. This refines the earlier same-day no-declaration decision: the available namespaced controller API cannot establish all audited facts automatically. Compose/plain in-life proofs need no outer-runtime attestation; their lifetime-lock/storage and process-exclusion requirements still apply. Plain pre-epoch exclusion uses the same full-contract operator attestation, with its lane-specific evidence requirements in #3710.

Cross-check the attestation against trusted observations bound to the actual pod/PVC, including the container-ID runtime prefixes of both predecessor X (its per-container IDs in the evidence document) and successor S, plus `runtimeClassName`, kernel and filesystem where observable. S's kernel/statfs observations establish facts only about S's node; X's node remains covered by the fleet-wide full-contract attestation. Contradictions for an accepted runtime enum value or missing required proof defer with stable reasons. Matching observations alone do not prove unobservable OCI handler, crun/conmon, NRI, attachment or ownership-walk behavior. RWO and `attachRequired` never substitute for the audited storage contract. Do not add a ClusterRole, node agent or discovery subsystem.

Undeclared installs consume the evaluation opportunity and keep trees with a reason, taking no reclaim EX or evidence wait. Preserve mandatory lifetime SH participation and required startup lock/record bookkeeping: zero extra reclaim wait does not permit bypassing another reclaimer's lock. Preserve the existing enum guards: `deploy/chart/templates/worker-invariants.yaml` fails rendering and `controller/internal/config/config.go` refuses boot for values outside `""`, `containerd-runc`, `crio-crun`. An accepted enum value whose cross-check contradicts observations defers; an explicit value alone never acquires stronger authority silently. Reconcile PRD A's policy text, chart/docs, bootstrap and controller binding accordingly.

### Evidence and rollout compatibility

Introduce a new schema version distinct from declaration-derived v1, binding attestation/capability result and reason to the existing epoch identity, pod/PVC and freshness rules. New deletion workers reject v1, absent, incompatible or stale evidence as deletion authority; a runtime value alone never upgrades old evidence. Consume evaluation and permit ordinary startup after settled deferral under the existing lock rules. Unknown destructive settlement remains fatal.

At the plan gate specify the producer/consumer contract, observable inputs and missing-input behavior. Test old-controller/new-worker, new-controller/old-worker, rollback, accepted explicit values with contradictory observations, invalid enum refusals and chart defaults. Preserve safe shadow-only behavior for old workers; do not enable deletion across unproven lock participation or incompatible handoff. No new Kubernetes privilege is required.

## Shared discard and strict-plain settlement

- Extend `discardRetainedSource` in `agent/src/git.ts` with a typed, worker-owned exclusion-proof argument for the narrow automatic path; do not add a separate eligibility module or parallel disposal implementation. Reuse `evaluateRetained`, inspect confirmation, source-use guards and existing authority/run/bare lock order. A lane's proof must be established by its trusted caller, not model input. Startup epoch proof never authorizes in-life deletion.
- Freshly require positive terminal lifecycle for every source kind and a known exact-run server response positively establishing zero open holds. A 404, unreadable custody, absent record, timer or disk pressure grants no authority. Serialize against source adoption/consumers and recheck pod-entry evidence before each source. Keep all other disposition/refusal guards.
- A strict plain journal requires a **new exact-key-set check**: exactly `runId`, `clonePath`, `attemptId`, valid and matching the exact attributed attempt. Existing `journalState: plain` is insufficient. Under the bare lock compare the raw journal unchanged; do not rely on the shared inspect digest to cover journal contents and do not change that digest's shared contract.
- With full authority and exact writer exclusion, waive only `ledger_live` for that attempt and `named_by_journal` for that exact plain journal. Another record naming the source/path, a successor, transfer, retained source or any other consumer keeps it. Do not waive a category of journal blockers or use terminal status as source-consumer exclusion.
- Any extra journal field, including `restoreTip`, `retainedSources`, `recovery`, `completionSourceFence` or `peeledMarkers`, refuses unchanged. FINAL-, hold- and episode-bound records keep their existing owner disposition paths. This releases no hold and authorizes no other source.
- Present sources use in-place `discardRetainedSource` with the existing inspect `confirm` and a reclaim `quiesce` combining process proof and epoch verdict. Never call `teardownDocker` or `withRetainedSourceBoundary`, which can delete containers. Clear only the unchanged exact journal and append `retired` once disposal is settled. Missing sources need the same full authority for metadata-only settlement.
- Preserve recoverable metadata on retained/uncertain outcomes. Interrupted settlement must remain discoverable after tree removal and either metadata write; the gatherer currently skips retired ledger entries. At the plan gate choose the smallest existing-state recovery sequence that finishes only the exact authorized settlement, never a successor's journal. Reversing write order alone is not a proof. No new API, migration or ledger state without a concrete gap returned to review.

## Lock lifetime, settlement and bounds

- Every mutating child receives the reclaim lock descriptor until it ends, including `runRunnerUidDelete` and git children mutating refs/worktrees/records. Scope one spawn-option hook to the discard core's async context. Run/SDK spawns never inherit the descriptor, whose open-file-description owner could unlock it. Cite any excluded child's inability to mutate protected state.
- Classify settlement from per-source outcomes and actual pending-child/latch state, **not** `kind: incomplete` alone. An incomplete result containing only settled `retained`/`removed`/`missing` outcomes keeps required metadata and may continue. An unknown outcome, unresolved child, latch `stopped()`, `RunnerDeleteUnsettledError` or nonempty `unsettledDeletes` prevents lock release/conversion and admission.
- After all work settles, close EX and acquire SH boundedly with admission still closed; admit only after SH succeeds. On shutdown/deadline start no new source and join in-flight work within PRD A's total/settlement bounds, otherwise exit non-zero. Never release exclusion to meet a deadline.
- Retain bounded inventory/pass caps; M1a measures their widened scope before M1b. Report `would_delete` and `would_settle` separately with present/missing classification, then actual deleted versus metadata-settled counts and bytes as `measured`, `partial` or `unknown`. No bytes-freed claim for a missing source.

## Records and scope

- Amend ADR-1783 for the exact epoch scope and accepted historical-writer residual; ADR-1786 for narrow automatic disposal and strict-plain settlement; ADR-3153 for the verified-epoch exception to default retention.
- Update `specs/human.md`'s #1858 prohibition and #3404/persisted-source wording narrowly, preserving protections outside this proof; update `docs/run-recovery.md` and run `task docs:sync`. Correct `git.ts` comments describing `reclaimed` as never deleted to acknowledge authorized verified-epoch disposal. Keep operator-only/default-retention wording accurate outside the new exception.
- Synchronize #1860/PRD A's attestation/evidence policy, #3621's acceptance and #3710: its pre-epoch path requires the same full-contract operator attestation; in-life reclaim remains declaration-free. Use symbol anchors rather than stale line numbers. #2852 is closed/satisfied.
- **#3329 owns the later reclaim roll, not this PRD.** Reuse #3103's genuine-idle drift-roll machinery, not ADR-1759's DinD recycle.
- **Accepted tradeoff:** no hosted-Docker busy-pressure reclaim, including before a counted disk park. The roll may wait indefinitely for safe idle and cannot promise rescue at the park cap; explicitly replace #3329's old before-park acceptance. Existing cache/HOME/provision reclaim continues independently.
- No rename fence, Docker resource deletion, same-epoch hosted-Docker deletion, cross-worker disposal or `.github/workflows/**` edits in implementation or validation. Compose/plain lane implementations remain #3709/#3710.

## Testing decisions

Watch focused regressions fail without the fix and pass with it:
- authority refusal per guard; positive terminal and known zero holds versus unknown/404/open holds; shared consumers/successors and changed raw journal;
- one rejection test per extra journal field, including fields accepted by the existing broad plain classification; exact key-set/attempt mismatch; waived blockers limited to the exact journal and attempt;
- present-tree deletion versus missing-tree metadata settlement; crash points after physical removal, between journal/ledger writes and after both; retry discovery and no successor clearing;
- settled-retained `incomplete` continues partial versus unknown/pending-child/latch uncertainty preventing admission;
- Linux two-process lock coverage, including parent SIGKILL while a mutating child holds the descriptor; run spawns do not inherit it; preserve #1858's pending-create sentinel control;
- shadow reports widened candidates but never deletes or settles source records (ordinary epoch bookkeeping remains); GET-only daemon fake; no protected record mutations;
- v2/legacy evidence and all rollout/rollback combinations above; undeclared zero-extra-wait startup, invalid enum refusals, accepted values with observable contradictions and default chart behavior without bypassing mandatory locks.

In-run: `UZI_DEADCODE_AGENT_REQUIRED=1 task check:agent` and directly affected tests. **full agent suite: exact-head PR CI**. Run affected controller checks and `task check-docs:web`; update affected E2E expectations and report integration phases run/unrun. No workflow fixtures on disk.

## Milestones, live acceptance and closure

- **M1a: Widened shadow and contract.** Ship the new schema/attestation capability result/reason and bounded read-only `would_settle` evaluation alongside physical `would_delete` candidates. Widen #3621's live hand-check to verify exact strict-plain eligibility, protected/shared refusals, present/missing classification, declaration/contradiction/legacy-evidence behavior and measured caps on real rolls. Default undeclared installs must keep trees without added reclaim waiting. No source-record settlement or deletion yet.
- **M1b: Settlement and deletion.** Blocked by M1a's widened live acceptance. Use the same reviewed candidate rules, freshly reevaluated under EX, and existing discard core. Live acceptance on an attested supported hosted worker verifies actual deleted/kept/metadata-settled sources, byte reporting, held-source safety, interrupted recovery and undeclared/contradiction refusal controls. A default-settings undeclared install is expected to defer, not demonstrate deletion.

PRs reference #3329/#1860 without closing them. Keep delivery non-closing until applicable live acceptance passes; a formal partial names deferred milestones. Compose/plain depend on the landed shared core. #3329/#1860 close only after their amended acceptance, including the later roll, rather than an M1a shadow-only delivery.

## Decision Log

| Date | Decision | Why | Rejected |
|---|---|---|---|
| 2026-10-24 | Deletion follows PRD A shadow acceptance and #2852 C (now satisfied). | Owner-approved split; shadow evidence measures caps. | Shipping lock and deletion together. |
| 2026-10-24 | Every mutating child inherits the lock; uncertainty holds exclusion. | Helpers can outlive the parent. | Assuming inheritance or exempting metadata children. |
| 2026-10-24 | Reclaim quiesce sends only daemon GETs; authority is rechecked under EX per source. | Do not delete containers or trust an earlier candidate list. | `teardownDocker`, `withRetainedSourceBoundary`, rename fence. |
| 2026-10-31 | Initially approve default-on evaluation without an operator declaration; missing automatic runtime/storage proof keeps trees. Refined by option A below. | New installs should evaluate reclaim safely without enablement. | Guessing missing facts. |
| 2026-10-31 | Owner chose option A: evaluation on by default on new installs and upgrade; keeping needs no declaration; hosted-Docker deletion needs full-contract operator runtime attestation plus observable cross-checks and epoch proof. Compose/plain in-life need no outer-runtime attestation. | Namespaced API observations cannot establish the full audited tuple; no broader privileges. Refines the earlier same-day decision and restores #1860 condition 5 = B for hosted-Docker deletion only. | Automatic proof claimed from incomplete API facts; ClusterRole/node agent; deferral-only as the complete deletion deliverable. |
| 2026-10-31 | Version evidence; widen shadow in M1a and validate before M1b deletes/settles. Keep settlement inside existing discard with a typed proof. | Reviews found legacy-evidence ambiguity, excluded stale-live candidates and non-atomic record recovery gaps. | Silent v1 authority upgrade; separate eligibility subsystem; blanket blocker bypass. |
| 2026-10-31 | Strict plain journals only; real writer exclusion and source-consumer protections stay binding. | Lawful stale-record settlement without weakening recovery custody. | Non-plain automatic settlement; idle/snapshot proof alone. |
| 2026-10-31 | #3329 owns later genuine-idle roll with bounded retries, parked resumes and no force; no busy-pressure hosted-Docker reclaim. | Reuse #3103 instead of a separate in-pod exclusion system. Explicitly replaces before-counted-park acceptance. | Busy-run interruption or deadline-forced roll. |
| 2026-10-31 | Keep stopped containers and the accepted dead-shim residual, retaining cheap exit-137 refusal. | Owner reaffirmed #1860 condition 4 = B; ownership stays #3387. | Container deletion prerequisite or weakened exit refusals. |
