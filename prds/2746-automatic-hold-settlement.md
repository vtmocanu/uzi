# Automatic hold settlement

Status: programme/design record for epic #2746, 2026-10-15. This umbrella is not one implementation run or one PR. Each child is an independently useful slice; unresolved child design blocks that child, not the whole record.

## Problem and outcome

Terminal work can leave custody held until the maintainer manually discards it. Older generations, permanently blocked recovery episodes and refused FINAL retries have different paths despite sharing the same desired outcome: preserve recoverable work, settle automatically, and reclaim only when safe.

Successful and cancelled runs require no routine cleanup. Failed work is preserved automatically and may be recovered voluntarily. A rare preservation failure remains protected with a specific diagnostic. A live current generation stays protected. Intentional plan-rejection and merge-supersession policies are separate from proof-backed release.

## Decision Log

| Date | Decision | Reason / boundary |
|---|---|---|
| 2026-10-15 | The maintainer requests automatic settlement instead of routine manual discard. | Manual action is for optional failed-work recovery or exceptional repair, not ordinary cleanup. |
| 2026-10-15 | Cancelled work publishes to held refs with repository visibility. | Explicit maintainer approval; cancellation alone is not abandonment. Secret/workflow/residue restrictions retain fallback/protection. |
| 2026-10-15 | Authoritative plan rejection discards automatically without preservation in the scope S1 sets below. | This is explicit abandonment, not a technical assertion of empty or preserved output. |
| 2026-10-15 | Superseded by S1 (same day): never-approved run discarded all generations; earlier approval discarded only the rejected generation; unknown history refused discard. | Historical decision only; S1 below replaces this scope and its approval-history machinery. |
| 2026-10-15 | S1 adopted by the maintainer: discard only the exact finally rejected run/generation/gate revision; every earlier generation, approved or not, preserves automatically. | Ordinary held publication (#2796), else archive; no approval-history lookup, provenance storage or history-based refusal class. |
| 2026-10-15 | Unbound first-gate reject: option a adopted; authority exists only at the proven atomic closure of awaiting_approval R > 0 in G with no incoming-transfer veto. | Approve-wins, moved-on, R=0/legacy and transfer-veto cases preserve; no reconstruction from diagnostic stamps. |
| 2026-10-15 | Completed-without-receipt inventory stays archive-first; held publication only after a thin capture or a typed oversize/quota refusal. Failed and cancelled stay held-first. | Maintainer choice (a) over a uniform held-first order: successful work is not published by default. |

The maintainer's direction, as recorded by the lead: hold settlement is automatic with no routine manual discard; cancelled work is published to held refs that follow the repository's visibility; rejected plan work is discarded. The earlier scope refinement was subsequently superseded by the maintainer's S1 decision recorded above.

### Programme recommendations, not additional maintainer decisions

Reuse one reconciliation loop and shipped evidence producers; keep SETTLE and CLEANUP independent. Keep #2506-D's recorded merge-supersession policy separate from preservation. Merge #2971 into terminal preservation with its regressions intact. Retain #2506-A until equivalent prerequisite durability passes acceptance; reject B's weaker proof and retire C only as a routine cleanup outcome. Preserve exceptional explicit-loss intervention with its existing authorization and guards. These are the programme's recommendations, not new maintainer decisions.

## Policy and retention

| State | Behaviour |
|---|---|
| Live / parked / awaiting approval | Protect current work; earlier generations settle only on proven transfer or independent preservation. |
| Completed | Reconcile all custody-bearing kinds/generations; use valid evidence or preserve then release. Apply #2506-D only within its exact merge-discard scope. |
| Cancelled / failed | Prefer qualifying held publication; otherwise qualifying archive/verified-empty evidence. Settle only after durable complete-inventory evidence. |
| Final plan rejection | Only the exact rejected generation may discard; earlier work preserves. Final-rejection authority must be decided and recorded atomically in the terminal reject transition from its locked pre-transition state (awaiting_approval, gate_revision R > 0, claim generation G), when that transition settles the reject that actually closes that gate; rejected generation is G, never reconstructed later from stop_kind, fail_origin or gate_revision. If any earlier generation was released into the rejected generation's custody (release_evidence='live_ancestry' with release_successor_generation equal to G, or a future transfer-based release under #2969), refuse policy discard and preserve normally; derive this from existing hold rows, not approval history. Maintainer decision (2026-10-15, option a): an unbound first-gate reject is final-rejection authority only when the atomic reject transition proves awaiting_approval at gate_revision R > 0 in generation G, that particular reject closed that gate, and the live-ancestry/transfer veto permits discard; every other case preserves. Source/pin cleanup retains independent guards. |
| Preservation refusal / uncertainty | Keep protected, reconcile and retry/repair within durable bounds; surface reason and artifact expiry. No timeout discard. |

Retention is configured, default **168 hours (7 days)**. Display actual artifact expiry. Reusing an old artifact does not restart its window. If the product promises a minimum window after settlement, implement and test the extension before making that promise. Public held refs follow repository visibility; deleting them cannot revoke copies already fetched.

## Design and seams

One reconciliation loop reads authoritative lifecycle and recorded transition-time final-rejection authority, reconciles pending immutable receipts, applies exact policy discard if authorized, otherwise obtains complete-inventory evidence and settles, then independently requests cleanup. Prefer completed-publication receipts as the zero-write fast path, existing held publication, self-contained archive or verified-empty evidence as appropriate. A single head on origin is not a substitute for the frozen full inventory.

Reuse worker recovery journals/coordinator and held-publication helper; API recovery FINAL, held scope/receipt checks, retention and cleanup; existing forge broker, scanning, writer proofs and exact-source guards. Eligibility extensions must bind the exact owner/repository/run/generation/hold/source/coverage and authorized custodian, including earlier generations of a terminal run. Reuse archive FINAL custody: original_worker_id == caller, live_worker_id == caller, exact user/run/generation/repository/guarded hold and ended generation; additionally require terminal status for this widening. The run may now belong to a different worker, but another worker cannot impersonate the original custodian. Replace current-run ownership at worker heldEligibleTerminal/heldPublicationStep, heldAdmit runOwnedByWorker plus heldScope, heldStateEligible and the database guard. Predecessors of a live run remain #2969/existing live-pass work. #3030 uses one new additive migration.

Advertise the widening as a new server feature and worker capability (e.g. recovery_held_publication_v2). A widened worker facing a v1-only API keeps today's failed-current-generation behaviour; a v1 worker facing a widened API is unchanged. Test both rollout orders. Update the UZI_HELD_PUBLICATION row in docs/configuration.md (currently failed-only) and run task docs:sync. Amend ADR-2545 Negotiation and switch alongside Scope.

Order (completed runs stay archive-first unless the maintainer decides otherwise): Failed and cancelled use the existing held-first order (deferred capture, archive fallback). Completed-without-receipt keeps today's archive-first order and attempts held publication only after a prerequisite-bearing (thin) capture or a typed oversize/quota refusal, on the same frozen record.

Blocked episodes need a model-free discovery/producer path independent of execution recovery. Keep attempt/backoff/legacy semantics and decoded verification bounds; do not resume model execution or reset a permanently blocked episode merely to capture its source. Maintenance retries must have durable eligibility/backoff and no busy loop. Unknown publication/upload/FINAL results reconcile before replacement evidence is constructed.

Self-contained archive FINAL already accepts terminal ci_fix/prompt/task inventory. A separate completed-kind settlement issue is unnecessary: #3030's per-kind matrix exercises the archive path and publication fallback for thin/oversize/quota refusals. A zero-write completed receipt optimization is only a future priority::normal idea.

Already policy-discarded or concurrently released holds are settled. Known settlement must short-circuit publication, archive fallback and replacement FINAL; it does not fabricate a preservation ACK or physical deletion proof. The current releaseHeldPublication closed-hold fallback and settleHeldDeferred upload path need a race regression.

Final-rejection authority must be decided and recorded atomically in the terminal reject transition from its locked pre-transition state (awaiting_approval, gate_revision R > 0, claim generation G), when that transition settles the reject that actually closes that gate; rejected generation is G, never reconstructed later from stop_kind, fail_origin or gate_revision. If any earlier generation was released into the rejected generation's custody (release_evidence='live_ancestry' with release_successor_generation equal to G, or a future transfer-based release under #2969), refuse policy discard and preserve normally; derive this from existing hold rows, not approval history. The API reuses #2506-D's server policy-discard binding/audit/DB-guard pattern and may discard in the reject transaction or persist (G,R) there for a server reconciler; it must not wait for the original worker. The policy discard closes the hold; existing server non-open-hold refusals prevent held publication and archive FINAL. Reuse those refusals across the ADR-2545 paths rather than adding duplicate plan-rejection exclusions at all four sites. Add only an exact typed policy-settled reason so the worker stops deferred publication/archive fallback instead of treating it as an ordinary refusal; it is not a synthesized preservation ACK or local deletion proof. Keep the additive policy-evidence/guard migration, not an approval-provenance table or per-approval-site writes/tests.

Each replacement recovery_inventory_hold_guard body must be built from live head at landing. Prior migrations 00305/00318/00319/00333 replace it wholesale. Sequence #3030, #2506-D's merged_predecessor_discard and #2884's new additive policy evidence/guard migration; LiveDB controls for every existing disposition branch must remain green.

### ADR amendments

- ADR-1197: define preservation-only maintenance authority and bounded retry scheduling independently of execution attempts; document unchanged blocked/legacy execution semantics and retained decoded-history bounds.
- ADR-2545: amend Scope, which currently sends cancelled runs to archives; widen terminal and older-generation publication eligibility; bind original/live custody safely; amend Negotiation and switch for v2/v1 rollout compatibility and the marked status-specific capture order; document cancelled public/private visibility, fallback and actual configured retention.
- ADR-1296: preserve immutable FINAL semantics, describe completed-kind coverage and distinguish evidence-backed release from the explicit plan-rejection/merge-discard exceptions. Thin archive release still requires a separately accepted durable-prerequisite evidence class.
- Align any affected existing approval/retention decisions in the same child change; do not silently claim the new policy was already guaranteed.

## Milestones / child programme

The order is rollout/review order, not an instruction to implement every child in one run. Safety fixes gate only the affected disposal/publication seam. All slices include regression tests, relevant documentation, and product-contract E2E updates without authoring workflow files.

### M0: merged predecessors settle (#2506-D, in flight)

Blocked by: none beyond its existing readiness requirements. Accept exact merged-PR/run/owner/repository binding, strictly earlier generations, idempotent recorded policy discard, and refusal for unrelated/current/later holds. Physical cleanup remains separate.

### M1: orphaned revisions resolve (#3001)

Blocked by: none. Accept bounded retry/cancel handling and authoritative lifecycle resolution after transient revision failure. Feedback, pending revision or an error string cannot become plan-rejection discard authority.

### M2: existing terminal evidence reconciles (#2494 + #2987)

Blocked by: #3030 (M3 / PR #3058), whose shared recovery seams and exact-hold settlement representation this repair reuses. Accept restart/outage replay of exact evidence and already-settled hold retirement without fabricated ACKs, rewritten FINALs or local deletion authority. Conflicts remain protected with a diagnostic.

### M3: widen terminal held-publication eligibility (#3030)

Blocked by: none. Coordinate shared reconciliation seams with #2494 (M2), but do not require its separate pinned-only boot reconstruction repair before this eligibility widening. #3030 owns its already-settled/race controls independently. On existing terminal/live-pass paths, cover cancelled/failed/completed-without-receipt inventory and earlier generations of terminal runs with archive FINAL custody binding. Change all four ADR-2545 sites including heldAdmit and one additive migration built from live head. Accept both v1/v2 rollout orders, the marked status-specific capture order, the ci_fix/prompt/task archive/publication matrix, holdless-kind invariance, already-settled no-upload/no-replacement-FINAL races, complete-inventory refusals and every existing database guard branch. This slice adds no blocked/orphaned discovery producer.

Cancellation acceptance carried from #2884 (2026-10-14) is part of M3/#3030: cancelled-before-model-work red/green, every cancellation entry point and positive nonempty capture, existing source/fencing/refusal controls, mixed-version/lost-ACK/restart races, and one monotonic absolute 10-minute post-cancel maintenance deadline covering locks through FINAL. Cap each sub-operation by the remaining deadline; expiry stops no additional model work, preserves evidence/custody and returns to the existing loop for a later bounded attempt.

The maintainer's 2026-10-14 accepted cancellation late-writer residual must be recorded in ADR-2545 and docs/run-recovery.md: an undetected terminal late writer may lose post-capture bytes during existing cleanup after release, as in failed-run held publication. No new durable post-release source/pin retention across hosted GC/controller teardown/recycling is introduced. Record the alternative of lifecycle-wide retention, rejected for size and recreating custody, and the reviewing peer's preference for it. Detected/unproven writers still retain under existing guards; this is no claim of new exclusion. #2852 remains separate. Update embedded docs with task docs:sync.

### M4: plan rejection settles by policy (#2884 rescope)

Blocked by: #2506-D (M0, its server-side policy-discard pattern), #3001 (M1, orphaned-revision cases) and #3030 (M3). #3031 (M5) later preserves blocked/orphaned earlier work; it is not a blocker.

Final-rejection authority must be decided and recorded atomically in the terminal reject transition from its locked pre-transition state (awaiting_approval, gate_revision R > 0, claim generation G), when that transition settles the reject that actually closes that gate; rejected generation is G, never reconstructed later from stop_kind, fail_origin or gate_revision. Decide it in RejectRunServerSide and the stamped plan_rejected worker failed-report arm. If proof that this reject closed the gate needs an additive worker-report field (for example an applied input id), gate it on a new proof capability and test both rollout orders; a worker without that proof capability grants no report-based discard authority and preserves, and an unapplied legacy reject is never sufficient evidence. Discard atomically there or stamp (G,R) there for a server reconciler; never reconstruct authority from a later failed row, and never wait for the original worker. If any earlier generation was released into the rejected generation's custody (release_evidence='live_ancestry' with release_successor_generation equal to G, or a future transfer-based release under #2969), refuse policy discard and preserve normally; derive this from existing hold rows, not approval history. Serialize this veto with existing transfer/run/hold locks; earlier released work may depend on the successor's remaining custody.

Acceptance: preserve after an approve-wins race at R, reject-after-approval plus unrelated failure, stale/superseded reject plus failure/timeout at a newer revision, server-side reject from queued/paused/limit_wait/recovery_wait/running, or gate_revision 0. A diagnostic plan_rejected fail_origin or old reject input is not authority. Use named earlier-generation fixtures: one never reached approval; another was approved and implemented work before a later generation re-gates through #209 loss detection or dropped-session re-planning and is rejected. The later generation discards only when independently eligible; earlier work preserves in both fixtures. Real LiveDB controls cover live_ancestry into the rejected successor (including awaiting_approval) and transfer/reject races. A same-worker adoption test proves later discard cannot delete source/pins required by earlier pending preservation.

The policy discard closes the hold; existing server non-open-hold refusals prevent held publication and archive FINAL. Reuse those refusals across the ADR-2545 paths rather than adding duplicate plan-rejection exclusions at all four sites. Add only an exact typed policy-settled reason so the worker stops deferred publication/archive fallback instead of treating it as an ordinary refusal; it is not a synthesized preservation ACK or local deletion proof. Policy discard settles that hold's preparing/uploading/needs_action captures and partial chunks in the same transaction, following DiscardHold -> DiscardNonReadyCapturesForHold; available archives, their bytes and retention remain untouched. Add non-ready cleanup rollback/refusal, available-archive survival and upload-race controls. The API retains exact evidence and an additive policy-discard/transition-stamp migration, built from the live guard body with every disposition branch green. No approval-provenance table, writes or enumeration test. Retain physical deletion guards, observed red/green, E2E/ADR/docs and the required local serial API/agent/LiveDB/repo gates plus CI.

Maintainer decision (2026-10-15, option a): an unbound first-gate reject is final-rejection authority only when the atomic reject transition proves awaiting_approval at gate_revision R > 0 in generation G, that particular reject closed that gate, and the live-ancestry/transfer veto permits discard; every other case preserves. Named controls: unbound_first_gate_reject_closes_gate discards only G when all authority/transfer checks pass; unbound_reject_approve_won, unbound_reject_run_moved_on, unbound_reject_legacy_revision_zero, unbound_reject_transfer_veto, unbound_reject_replay_disposed, unbound_reject_ignored_after_approval and unbound_reject_superseded each preserve normally and mint no discard authority. ADR-1795 D3's existing stop-at-first-gate semantics remain unchanged.

### M5: model-free blocked/orphan terminal preservation (#3031)

Blocked by: M3. Discover terminal blocked/orphaned records independently of execution, reconcile dispositions, then use existing evidence producers. Accept #2971 stale-cap and genuine unique-content refusal controls, unchanged attempts/backoff/legacy execution semantics, unchanged refusal classification (#2022 remains open and Later under the 2026-10-11 decision), #2401 budget diagnostics and #1790 terminal durability. Already settled holds produce no new publication/archive/FINAL. Keep any live-park obligations separately tracked. This slice is model-free and bounded; no failed-run Resume or consent grant.

Source: the hold's retained local inventory under custody on its original worker, meaning the journal-named clone(s), protected descriptor and pinned refs that the terminal FINAL path already freezes. A worker maintenance pass reads it; it never uses the last published checkpoint or a salvage ref as the source. The execution episode stays blocked with its attempts and backoff unchanged. A capture that again exceeds the decoded-history bound is a preservation refusal that keeps custody (P4), not a reason to unblock or replay the episode.

### M6: predecessor handoff while successor is live (#2969)

Blocked by: its existing quiescence/evidence prerequisites, not completion of terminal work. Accept guarded predecessor FINAL at the proven transfer boundary; current live generation remains held. Thin/incomplete evidence still refuses release. The cited main-history pack fits below the cap but does not establish guarded-inventory bundle fit; measure representative bundles. No speculative shared-capture subsystem. Placed after #3031 to prioritize terminal cleanup, but independent implementation may proceed when its own prerequisites are met.

### Later independent children

- #2823/#2809/#2414: exact-source reclaim, pin dependency lifetimes and acknowledged metadata cleanup. Preserve worker and archive consumers independently.
- #2852/#1857, #2422, #2524: writer/process proof, API sender fence and bounded journal reads. Keep their refusal tests; no blanket implementation rewrite.
- #2544: quota refusal diagnosis and safe bounded recovery.
- #1790 (priority::high): live transient/vault-locked park durability, bounded join-token archive attempt and cross-worker/volume-loss controls. #3031 does not absorb these live-run obligations.
- #2022: parked Later under the maintainer's 2026-10-11 permanent untyped-400/403/404/409 decision; reconsider only on a real proxy/gateway incident.
- #2747: named live acceptance ledger, absorbing only outstanding #1349 M8/#1742/#1855/#1595 M4/#2362 acceptance. Preserve each scenario, exact tested build and no skipped proof.
- UX/state shrink after automation: no routine discard prompts; preserve exceptional diagnostics and explicit-loss controls.
- #2506-A: separately review prerequisite pin design, full dependency lifetime, default-branch race policy and fresh restore after rewrite. #2809's obligations survive until equivalent coverage exists.
- Thin-frequency measurement: representative sample with operation counts, bundle bytes, WIP/root inventory and refusal causes; no claim that all captures exceed the cap.

## Verification and risks

Every bug fix must have observed red/green at its actual seam. Worker tests cover crash/restart, legacy records, charged execution budgets, maintenance backoff, full inventory and same-type/same-length corruption controls where hashing is the differentiator. API tests cover exact identity, immutable evidence, status races, cross-generation authorization, atomic final-rejection authority, transfer dependencies and additive migration guards. Forge tests cover create/readback/CAS and unknown outcomes. E2E expectations change with the product contract; real workflow files are outside implementation and validation scope.

Use focused tests while developing. For #3030/#3031, after validators finish, freeze the candidate and run locally, serially: task gate:agent; task gate:api with UZI_TEST_DATABASE_URL unset; then ./e2e/run-store-it.sh showing the named LiveDB tests ran (zero skipped); then task gate:repo. #3030 LiveDB cases cover exact original/live custody, ended/terminal eligibility, per-kind matrix, immutable evidence, already-settled races, retention and every existing guard branch. #3031 retains those contracts and exercises maintenance disposition/replay against durable terminal state. Record exact commands/results and rerun affected validation after candidate changes. CI is also required; report environment limitations rather than silently substituting CI-only validation.

For #2884, after validators finish, freeze the candidate and run locally, serially: task gate:api with UZI_TEST_DATABASE_URL unset; task gate:agent; then ./e2e/run-store-it.sh showing the named LiveDB policy tests ran (zero skipped); then task gate:repo. Controls cover final exact rejection, atomic gate-rejection authority, incoming-transfer veto, named earlier-generation preservation fixtures, server policy outcomes, revision/successor races and every prior guard branch. Worker tests retain independent physical-deletion guards. CI is also required; report environment limitations explicitly. Live acceptance belongs to #2747 and its named cases. This draft changes no source code/workflows and claims no hosted acceptance.

## Non-goals / rejected mechanisms

Routine manual discard, owner consent grants, failed-run Resume for cleanup, manual reclaim UX, unbounded verification, new generic wait states, provider-conversation portability (#1229), and partial-delivery repair (#1297). Keep genuine live budget/approval decisions. Keep unrelated checkpoint/capability/outbox bugs separate. No new retention guarantee without implementation; no weakening of immutable evidence, secret scanning, source fencing or refusal on unexplained inventory.

## Inventory status correction (2026-10-15)

#2988 is fixed by PR #3029 (`5a712362a`) and closed. #1297 is closed as not planned; optional #1229 provider-session portability is closed as not planned (maintainer decision 2026-10-15). Neither is a prerequisite for source settlement or a claim that conversation continuity already exists.
