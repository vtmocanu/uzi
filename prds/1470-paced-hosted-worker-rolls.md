# PRD #1470 — Paced hosted worker rolls

Status: in progress; hosted verification remains open.

## Outcome

A worker image or template roll replaces one canary per owner-specific coverage pool first, then progresses with bounded concurrency after current-pod registration and heartbeat proof. A stuck replacement holds remaining healthy workers on their old image. Holding is not rollback.

The implementation follows the approved singleton design. Worker Deployments remain Recreate with RWO volumes; worker images and release pin guards do not change. Ephemeral workers remain outside pacing. No workflow files change.

## Milestones and acceptance

- [x] M1: reproduce whole-idle-fleet rolling, premature busy-worker cordon and deadline containment failures on the base revision; record offline mid-roll run-health fallback.
- [x] M1: additive registration pod UID, proven heartbeat, drain provenance, poll/status contracts and bounded decoder; both shared wire goldens; executed poll handler and live-DB queries.
- [ ] M2: enforced chart singleton, default-one concurrency, exact expected-version mapping and Deployment metadata reservations.
- [ ] M2: canary promotion, owner/lane/capability coverage, independent pools, frozen coverage, stuck holds, provenance-aware release and explicit forceRoll.
- [ ] M2: restart, delayed/lost mutations, pin changes, partial completion cleanup, same-version and A→B→A registration ambiguity regressions.
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
8. **Limits and verification:** decompressed polls are bounded before typed worker allocation and reject incomplete snapshots atomically. Existing KinD smoke is envelope coverage; maintainer hosted verification stays open.

## Execution evidence

M1 evidence (2026-10-11):

- On base revision 2c6b3393, the tagged TestRollPacingRegression fixture compiled and failed all three behavior assertions: three idle replacements, two premature busy cordons, and two sibling replacements despite a stuck canary and expired deadline. The tag intentionally excludes these pending M2 acceptance tests from the M1 default suite.
- Focused controller apiclient/protocol checks passed, including actual gzip/chunked byte bounds, worker-count overflow, atomic rejection, token-safe errors and shared wire goldens. A 4,134,925-byte / 512-worker poll allocated 31,900,920 bytes in the near-limit allocation check.
- A throwaway PostgreSQL run executed TestRollEvidenceDrainWritersLiveDB, TestRollEvidenceRegisterNamesAndPollLiveDB, TestRollEvidencePollOverflowLiveDB, TestRollEvidenceRegistrationHeartbeatLiveDB, TestRollEvidencePollEligibilityDrainLiveDB and TestRollEvidenceOfflineLadderLiveDB, all passing without skips. It exercised migration/query execution, handler registration/poll, actual heartbeat proof, coverage matrix and independent drain fences.
- The observed fully offline mid-roll fallback is exactly: "no worker is online to pick up this run".
- task check-docs:web passed. Full integration-gate and immutable-range review outcomes are recorded in the milestone checkpoint report; they are distinct from the focused evidence above.
