# PRD #1470 — Paced hosted worker rolls

Status: in progress; hosted verification remains open.

## Outcome

A worker image or template roll replaces one canary per owner-specific coverage pool first, then progresses with bounded concurrency after current-pod registration and heartbeat proof. A stuck replacement holds remaining healthy workers on their old image. Holding is not rollback.

The implementation follows the approved singleton design. Worker Deployments remain Recreate with RWO volumes; worker images and release pin guards do not change. Ephemeral workers remain outside pacing. No workflow files change.

## Milestones and acceptance

- [x] M1: reproduce whole-idle-fleet rolling, premature busy-worker cordon and deadline containment failures on the base revision; record offline mid-roll run-health fallback.
- [x] M1: additive registration pod UID, proven heartbeat, drain provenance, poll/status contracts and bounded decoder; both shared wire goldens; executed poll handler and live-DB queries.
- [x] M2: enforced chart singleton, default-one concurrency, exact expected-version mapping and Deployment metadata reservations.
- [x] M2: canary promotion, owner/lane/capability coverage, independent pools, frozen coverage, stuck holds, provenance-aware release and explicit forceRoll.
- [x] M2: restart, delayed/lost mutations, pin changes, partial completion cleanup, same-version and A→B→A registration ambiguity regressions.
- [ ] M3: honest held/rolling/offline run health, special placement lanes, admin fleet.roll and readiness-report preservation.
- [ ] M3: PostgreSQL register/heartbeat/cordon/claim proof that retained workers remain claimable past deadline.
- [ ] M3: docs, ADR, recorded decisions, values table and generated documentation sync.
- [ ] M3: existing KinD envelope smoke (controller disabled there; not a pacing proof).
- [ ] Maintainer: hosted Kubernetes bad-tag/good-tag pacing and claimability verification, including real-apiserver mutation ordering.

Tick only items supported by direct execution evidence. Keep this PRD active until maintainer hosted verification is recorded.

## Decision Log

1. **Supported topology:** the controller is a hard singleton, enforced by chart rendering, with Recreate deployment. No API pool journal, operation records, tickets, epochs or multi-replica coordination.
2. **Progress:** committed target Deployment state derives in-flight rolls. A metadata-only confirmed reservation covers busy draining and unknown patch outcomes. Freeze original owner/lane/capability coverage in the same annotation; retain a completed canary flag through cleanup/restart.
3. **Coverage:** owner, placement lane, registered scheduling/protocol capability sets, and ordinary/checker eligibility derived from nullable checker slots. Size/template do not split pools.
4. **Promotion:** current Deployment/ReplicaSet/pod UID owner proof, target image/version, registration carrying current pod UID, and a fresh subsequent current-nonce heartbeat. Existing registration name transport carries the UID through pod environment; the user-chosen worker name remains unchanged. Missing evidence fails closed.
5. **Holds:** any unresolved stalled participant blocks further admission, with remaining healthy old workers claiming. Drain deadlines apply only to admitted workers. Explicit workers.forceRoll bypasses pacing/hold/evidence with existing custody/maintenance protection; no automatic timeout override or rollback.
6. **Cordon ownership:** a reason beside draining_since allows only roll-owned release. Preserve independent maintenance, pressure and custody fences, including delayed drain writes.
7. **Skew:** deploy new API first, then the singleton controller, before changing the worker pin. Introducing pod UID environment itself causes a paced template roll. Old controllers retain legacy unpaced behavior.
8. **Limits and verification:** compressed and decompressed polls have independent 4 MiB limits, accept one gzip member only, and are bounded before typed worker allocation; incomplete snapshots are rejected atomically. Existing KinD smoke is envelope coverage; maintainer hosted verification stays open.

9. **Retained API capacity:** roll evidence adds nullable liveness_valid_until from online status and an ordinary heartbeat whose expiry is strictly after the API's evaluated_at, using the claim path's staleness bound. This distinguishes legacy registered workers with no pod UID/proven heartbeat; pod readiness alone does not establish API connectivity. Ordinary liveness never refreshes nonce-proven heartbeat_at or valid_until. M2 uses this evidence to retain the owner's last API-online provider; planner enforcement remains pending.

10. **Reconstruction durability:** legacy volume recycling keeps the Deployment at zero replicas until pods and selected volumes are safely stopped/recreated. DinD's existing fenced lifecycle requires deleting that Deployment and a different replacement UID (ADR-1759 D8–D9), so the same bounded reservation is temporarily mirrored on the retained /data PVC. This is necessary external evidence during that deletion, not another journal. Read it through bounded existing PVC lists; add only the namespaced PVC patch verb, and use UID/resourceVersion preconditions for handoff and cancellation. No PVC get or Secret read is added, and legacy /data self-heal is retained.
11. **Exact release mapping:** ADR-1720 stamps published workers with <release>+g<commit>; a bare SemVer image tag does not identify that full version. Configure workers.rollTargetVersion from the pinned image's verified stamp (also for published bare release tags). Unknown mapping fails closed; never infer a target version from a replacement or discard build metadata.

12. **Unabridged version proof:** ordinary registration retains its existing 64-byte display sanitization. If it alters a version, registration withholds the roll incarnation binding, so subsequent nonce-proven heartbeat cannot promote a different long build through the same truncated prefix. Supported exact roll versions must reach the API unchanged.

13. **Overlapping pools:** coverage pools retain separate canaries and concurrency, but admission also preserves physical usable capacity across the owner's entire placement lane. Distinct capability pools cannot each remove the last generic lane provider in the same tick. No capacity is borrowed from a different owner or lane, or assembled from multiple workers' partial capability sets.
14. **Completed history:** a completed canary first closes and cleans its original cohort. A later pin uses current registered coverage only after that cleanup; unrelated maintenance does not synthesize a second image-roll canary. Completed pacing metadata is omitted from authoritative fleet reports rather than turning ordinary maintenance into an active image hold.
15. **Report authority:** one fleet report merges observed roll health and pacing, retaining actual stuck causes and adding podless pacing rows. Partial readiness reports explicitly identify themselves. Diagnostic publication failures never control claims or admission.
16. **Fresh drain confirmation:** an expired busy image turn reasserts its reason-owned cordon and uses a bounded fresh poll before mutation. Compare current desired state and independent fences; compute age from the API evaluation and drain timestamp. Ordinary API liveness confirms an old worker's drain; only replacement promotion requires incarnation-proven heartbeat evidence. An old template on the same image remains usable retained capacity during the first pod-UID environment upgrade.
17. **No-op handoff:** a complete fresh PVC list can confirm an identical bounded non-nil reservation without another write. Changed markers and cancellations still use UID/resourceVersion preconditions and require a confirmed revision advance. Admission immediately removes a drifted old incarnation from retained capacity, before its reservation/template writes.

## Execution evidence

M1 evidence (2026-10-11):

- On base revision 2c6b3393, the tagged TestRollPacingRegression fixture compiled and failed all three behavior assertions: three idle replacements, two premature busy cordons, and two sibling replacements despite a stuck canary and expired deadline. The tag intentionally excludes these pending M2 acceptance tests from the M1 default suite.
- Focused controller apiclient/protocol checks passed, including actual gzip/chunked byte bounds, worker-count overflow, atomic rejection, token-safe errors and shared wire goldens. A 4,134,925-byte / 512-worker poll allocated 31,900,920 bytes in the near-limit allocation check.
- A throwaway PostgreSQL run executed TestRollEvidenceDrainWritersLiveDB, TestRollEvidenceRegisterNamesAndPollLiveDB, TestRollEvidencePollOverflowLiveDB, TestRollEvidenceRegistrationHeartbeatLiveDB, TestRollEvidencePollEligibilityDrainLiveDB and TestRollEvidenceOfflineLadderLiveDB, all passing without skips. It exercised migration/query execution, handler registration/poll, actual heartbeat proof, coverage matrix and independent drain fences.
- The observed fully offline mid-roll fallback is exactly: "no worker is online to pick up this run".
- task check-docs:web passed. Full integration-gate and immutable-range review outcomes are recorded in the milestone checkpoint report; they are distinct from the focused evidence above.

M2 evidence (2026-10-11–12; locally committed, reviewed and validated):

- The controller kube package passed `go test -race -count=1 ./internal/kube`, including the three M1 behavior regressions, bounded concurrency and stuck-cause reporting, exact-incarnation/same-version proof, Deployment/PVC reconstruction, completed-marker cleanup and overlapping owner/lane capacity regressions.
- Reconcile/apiclient reporting tests passed with `-race`: a single fleet POST preserves stuck/settled phases, includes podless pacing, and reports reconcile errors; readiness publication stamps its explicit partial-report kind.
- Controller configuration tests passed for the default concurrency, a positive override and malformed limits. Chart assertions executed successfully for singleton/default/zero/disabled behavior, invalid concurrency, exact-version and force-roll rendering, Recreate strategy and the restricted/Docker/isolated ReplicaSet/PVC verbs.
- A throwaway PostgreSQL run executed `TestRollEvidenceRegisterNamesAndPollLiveDB` without skips, including exact 64-byte registration versions and two different 65-byte versions with a shared truncated prefix. Altered/truncated versions supplied no pod binding or heartbeat promotion proof, while ordinary registration and user labels retained their existing behavior.
- Immutable review covered the initial M2 candidate and every repair range with reviewer, tester and auditor. The last code SHA, 65b61fff358e0ed1c8e7f0f03bd065b6ac245464, passed task gate:controller and task gate:repo (exit 0). task gate:api passed at b0f39b5e; API source has not changed in the later repairs.
- Repair regressions failed before their fixes for independent maintenance recovery/deadlines, healthy completed-roll drain release, unlabelled PVC mounts, peer-only completed-roll drift, same-image legacy capacity, implicit Docker coverage, stale drain deadlines, admitted same-image cross-pool capacity and repeated no-op PVC handoff. Focused final kube/apiclient race tests passed.
- A throwaway PostgreSQL execution of TestDBA1470LateCordonAndFreshPollLiveDB and TestRollEvidenceDrainWritersLiveDB passed both tests with zero skips. Authenticated cordon/poll/uncordon calls and a real claim proved a late cordon blocks a stale caller's claim, reason-owned release restores claimability, and clear/re-cordon resets the drain timestamp. The planned retained-N-1 fleet replay remains M3 work.
- Normal controller lint passed. Extra unratcheted preset lint failed on G301/G306 in size_display_golden_test.go; identical focused commands reproduced both findings in base and candidate exports. Recorded separately for off-task triage.
- These are fake-client optimistic-conflict regressions and local PostgreSQL evidence, not real-apiserver or hosted-fleet verification. Maintainer verification remains open.
