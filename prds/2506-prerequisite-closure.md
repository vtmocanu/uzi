# PRD #2506 (option A): a pinned prerequisite closure lets a thin guarded capture release custody

**Issue:** #2506 (option A only). Delivery under this PRD is **non-closing**: every PR uses `Refs #2506` and closes only its own child issue. #2506 stays open until the whole acceptance below is met. Option D shipped separately (ADR-2506) and does not complete this.
**Current execution (2026-11-01):** C2 (#3727) merged in [PR #3748](https://github.com/vtmocanu/uzi/pull/3748); source at `a07f813708ced206cd7b8c3f9d09e259b2fff64b` is known. This current-state update makes no claim about unobserved hosted CI or full store-it results (see the historical [C2 execution record](#c2-execution-record-2026-11-01-3727)). C1a and C1b source are merged. C1b landed in [PR #3655](https://github.com/vtmocanu/uzi/pull/3655) as `b6fab6b7`, with migration 00387; exact-head hosted checks and timing were collected before merge. The local handoff records below remain historical evidence, not the current hosted-gate status. C1b itself added no thin FINAL release, feature advertisement or agent change; C2 adds them. C3 M1–M4 functional delivery is locally implemented and validated; product/documentation units are reviewed at `10ff078c` and the minimal integration-fixture repair at `287b04b9`. Required exact-head landing checks remain unobserved here. E5 is measured locally as recorded below; broad C3 and parent acceptance remain open; E6 is post-roll reporting only. M6 operator observation is not claimed performed. See [current execution and checkbox disposition](#c1b-current-execution-2026-10-29-child-3591).

**Historical design status:** DRAFT, revision 18 (2026-10-28), approved C1a/C1b split and review folds, pending exact-text final review. C1a and C1b are API-only; no filing or dispatch follows from this draft. Based on the exact revision-11 artifact (SHA256 `2205e1146950263b2aa90f35863cadec3f3debfbd350590b9e65bd560a9640ec`, whose header still said revision 10). The earlier paired architecture reviews and owner decisions remain the design history; this new evidence fold requires recov6 review and is not filing, dispatch or implementation approval. **E2 ran on GitHub on 2026-10-27 and returned FINDING (one measured workflow-scope refusal, zero probe malfunctions), conclusive for the tested production credential class.** The anchor strategy is feasible in that tested case; other forges and protected/ruleset configurations remain unverified. Existing source line anchors are historical at `91647464c02bd12ae30d4fce6aa656f2245e88e1`, not refreshed line numbers for current main. The new E2 section states the exact observed scope and remaining gates.
**Priority:** High (inherits #2506). The independent architecture review CLAUDE.md requires for priority::high applies to this PRD and to each child before dispatch; no waiver. M0 is evidence-only and is never auto-dispatched.
**Related:** ADR-1296 D5 (thin-capture rule) and its 2026-10-09 amendment (the "Later user deletion or rewriting is outside the proof" residual), ADR-1197 (local thin proof grants no custody release), ADR-2417 (guarded local retention), ADR-2545 (terminal held publication, `create_unknown` precedent), ADR-1810 and ADR-1867 (create-only uzi ref namespaces), ADR-2213 (residue quarantine), ADR-2506 (option D), ADR-456 (`adr/0456-rebase-before-finalize-push.md`, GitHub workflow-scope rule), the #2746 programme (`prds/2746-automatic-hold-settlement.md`), #2476, #2494, #2507, #2544, #2809, #2884 (open; another `recovery_inventory_hold_guard` rebuild), #3030 (PR #3058), #3105, **#2969** (re-scoped behind this PRD on 2026-10-23), **#3031 U1** and its children #3443 and #3444.
**Delivery shape:** M0 (maintainer-owned evidence and probes, not a uzi run), then four child issues, one gated run/PR each: C1a, C1b, C2 and C3. C1b waits for C1a; C2 waits for C1b; C3 may parallel C2 only with disjoint file ownership. No uzi run's implementation or validation creates, modifies or commits any path under `.github/workflows/`; the single exception is E2's maintainer-only disposable scratch repository. Do not reserve a draft migration number here: choose the next free number above the live head at landing (`task migration:renumber`). PR #3580 landed at `b4f7d5ff` with migration 00372. C1a was audited on base `2085c751` after vault moved to 00373; its additive guard migration is 00374 at `991df8b3`. Rebase and re-audit overlapping recovery files and migration numbering on any later landing. Existing code anchors remain historical at `91647464`; new split/review anchors explicitly use `c750b802`.

## Problem

An inventory-guarded recovery capture first builds a self-contained bundle. When that exceeds the 64 MiB cap (`RECOVERY_MAX_BUNDLE_BYTES`, `agent/src/git.ts`), it falls back to a thin bundle (#2476) whose prerequisites are the merge bases of the captured source H with the worker bare's cached `refs/remotes/origin/<default>` tip, or H's parents when a merge base equals H (`GitCache.produceRecoveryBundleWithinOperation`, the `RecoveryBundleTooLargeError` catch). The worker runs `git bundle verify` in its trusted bare and re-reads the recorded prerequisites from the produced header. If the default branch is later force-pushed and those commits are pruned, the bundle no longer imports into a clean clone, so a thin archive never authorizes the guarded archive FINAL and the hold stays open until another disposition or an owner discard.

For this repository the fallback is the normal case. A self-contained bundle of `origin/main` measured **70,712,858 bytes** (`git --no-replace-objects bundle create - origin/main | wc -c` at main `90d172978`, Git 2.55.0, local maintainer clone; not worker-bare evidence) against the 67,108,864-byte cap. Two reruns of the same command at the same commit and Git version on a different maintainer clone gave 70,702,169 and 70,566,347 bytes; multi-threaded delta search is a possible explanation for the variation, not experimentally verified. The over-cap result stands independently of it: every run exceeds the cap by more than 3.4 MB.

The refusal is enforced at four places, all keyed on literal emptiness of the prerequisite list:

| Layer | Anchor (symbol; line at `91647464c`) | Behaviour |
|---|---|---|
| Worker, FINAL mint and replay | `agent/src/recovery.ts`, `finalizeInventoryWithinBoundary` (1871; thin check 1886-1890; status read 1897-1900) | returns without FINAL unless `selfContained === true`, `prerequisiteShas` empty and the header agrees; this check also runs before replaying a saved archive FINAL |
| Worker, cycle ordering | `agent/src/recovery.ts`, `frozenOperationCycle`: saved-FINAL first branch (2285-2286), v2 missing-metadata guard (2343), exact observation (2349-2359), `eligible` (2370-2372), archive-first `captureCycle` (2376-2385), held fallback return `archived ?? captureCycle` (2461); `captureCycle` (2630) calls `finalizeInventory` at 2663 and 2713 | only a self-contained uploaded archive takes the FINAL branch |
| API, Go check | `api/internal/recovery/final_inventory.go`, `releaseFinalInventory` (141) | `len(cap.PrerequisiteShas) != 0` returns `ErrManifestConflict` (409) |
| API, SQL | `api/internal/store/queries/recovery.sql`, `ProtectFinalInventoryCapture` (907, predicate 913), `ReleaseFinalInventoryHold` (917, predicate 939) | both require `COALESCE(cardinality(c.prerequisite_shas), 0) = 0` |

The database guard `recovery_inventory_hold_guard` (latest body in `00340_terminal_held_publication.sql`; archive branch at line 111) does not check prerequisites.

What the server checks about an archive today (`api/internal/recovery/service.go`): `Service.stream` verifies the streamed length and sha256 against the manifest, then `BindCaptureManifest` (~450), `MarkCaptureReady` (~463), `tx.Commit` (~473); `prerequisite_shas` is a worker declaration of at most 64 SHAs (`maxPrerequisiteShas`), immutable once bound (the "trusted worker declaration is immutable" refusal in `lockCapture`, ~306). The server never parses bundle bytes. The self-contained archive FINAL already rests on worker-produced, worker-verified bytes bound to an immutable manifest.

### Freeze paths and the metadata they carry

| Freeze path | Caller | `repoId` → `exactHoldBinding` | `heldPolicy` | Thin-capture settlement today |
|---|---|---|---|---|
| Terminal | `Runner.settleGuardedInventory` (`agent/src/runner.ts`, ~14535) | passed; set when exactly one guarded hold resolves and is observed open | passed; kept when valid | held publication when held-eligible; otherwise manual, ADR-2506 discard, or a later self-contained capture |
| Retained-claim capture | `Runner.recoverRetainedClaim` (`agent/src/runner.ts`, ~21670) | not passed | not passed | archive path only; local `verifyRecoveryClosure` is not release evidence (ADR-1197) |
| Boot reconstruction | `RecoveryCoordinator.materializeBootInventory` (`agent/src/recovery.ts`, ~2575) | not passed | not passed | archive path only (#2494) |

A later freeze matching an existing identity adds `exactHoldBinding` but never `heldPolicy` (`matching` in `freezeInventory`). With held v2 negotiated, records lacking either field are refused held work ("Missing new held metadata cannot grant publication authority", 2341-2343). **That refusal stays unchanged.** Once a record journals an archive `finalRequest`, every later cycle takes the first branch (2285) and never attempts held publication; this is why closure-backed FINAL minting needs an explicit permission (Worker behaviour).

Producer records (#3031) are not on main as a freeze path, but their identity class is: `classifyRecoveryOrigin` (`agent/src/recovery-origin.ts`) classifies a capture id as `ordinary`, `producer` (`maintenance-v1-<uuid4>`) or `malformed`, and `deferProducerFinalization` (recovery.ts ~4606) defers producer finalization only inside the live slot.

### Measured evidence (stated narrowly)

- **Zero verified standalone A1 cases.** E1 v2 (2026-10-24, on #2506): 257 guarded holds with confirmed-thin captures, all in this repository; unknown 144, B4 82, B1 30, B2 1, **A1 0**; 24 unproven A1 candidates.
- Earlier producer-log sample (2026-10-18): 59 thin producer events across 12 runs in 72 hours (events, not distinct captures).
- Reported, not independently rechecked (#2506 comment, 2026-10-25): run `dd902e43` (thin-only generation-1 hold, no own MR, manual discard); run `180e35c0` (records without held metadata; generation 1 settled only through ADR-2506).

### Premise check

1. Held publication (ADR-2545 V2) releases eligible terminal holds for records carrying both held fields; failed and cancelled try held first; completed try the archive first and take the held alternative when the archive is thin or refused.
2. `push_secret_blocked` and `worker_residue_blocked` exclude held publication (`eligible`) but not the archive FINAL; the residue quarantine blocks every FINAL while latched (ADR-2213). In a non-foreground v2 pass the fail origin is not observed (`terminal.fail_origin` comes only from `options.terminal`).
3. Legacy holds release on any available archive (`ListReleasableCustodyHolds`).
4. Hold outcomes are classifiable from `release_evidence` (migration 00338).
5. GitHub's workflow-scope rule also applies to the tested create-only prerequisite namespace (E2, 2026-10-27). The default-tip anchor and older tips with the same workflows tree were accepted; the tested older tip with a different workflows tree was refused. The E2 section qualifies this result by credential and repository configuration.
6. Records without held metadata are archive-only by design.

## Goal and acceptance (from #2506, qualified)

- [ ] A thin guarded capture on a large-history repository reaches released custody without owner action, with prerequisite-lifetime evidence and native-Git restore **demonstrated by tests** (P1 replaces the issue's per-release restore verification with the trust contract plus tests; see "Trust contract"). If preservation cannot be shown, custody is retained with a specific typed refusal.
- [ ] After a force-push that prunes the cached prerequisites, a released archive still imports into a clean clone plus a fetch of its pin ref, or release was refused. **Qualified:** this holds against default-branch rewrites and uzi's own writers; deletion of `refs/uzi-prereq/*` or rewriting of that history by a party outside uzi **after** a closure release is an accepted preservation residual (user decision 2026-10-25), following ADR-1296's existing "Later user deletion or rewriting is outside the proof" (`adr/1296-durable-run-recovery.md`, ~589-590).
- [ ] No runner-clone or agent-writable ref is used as a prerequisite or as release evidence.
- [ ] Records produced by each real freeze path have a stated and tested outcome; no record gains held-publication authority; no binding or policy is reconstructed; the v2 refusal is unchanged; producer-origin records never mint a closure FINAL.
- Option C is not an accepted completion outcome. Option D does not complete this.
- The capture stays credential-free on the worker and keeps covering every owed root.

## E1 scope gate and decision rule

The E1 classes are unchanged from revision 4. E1 v2 found **zero verified standalone A1 cases**. **User decision (2026-10-25): "Proceed"** to design review on the measured enabling need (#2969's re-scope and #3031 U1, with the over-cap measurement). The standalone value case remains unproven; the decision does not waive E2, approve implementation or approve dispatch.

## Approach

### Hard points

- The prerequisite closure of a default-branch merge base is essentially the whole history (about 70.7 MB here); storing it per capture repeats the cost thin capture avoids.
- The API is distroless with go-git only; a full go-git fetch of this repository once OOM-killed the 512Mi API pod (`fetchWantedRefs` comment). This design fetches no objects.
- GitHub's workflow-scope rule (premise 5).
- Authority must not leak across classes (premise 6), and producer records must not settle through this PRD.
- A journaled archive FINAL pre-empts held publication in later cycles, so the worker must not journal a closure-backed FINAL until held is proven unavailable or has fallen back in the same pass.
- New lock dependencies must respect the existing order (FINAL: worker → run → hold → capture; upload: hold → capture, no run lock).
- #2969 mints predecessor FINALs inside a bounded window and must never extend it.

### Chosen design

1. **Anchor pin, created eagerly.** When a qualifying thin capture becomes `available`, a `pending` closure row is written in the same transaction. A background lane then reads the default tip D, proves every recorded prerequisite P is an ancestor of or equal to D (`forge.Forge.CompareAncestry`), and creates `refs/uzi-prereq/<capture-id>` at D with one create-only command and a read-back.
2. **Release.** The guarded archive FINAL gains one alternative: a `pinned` closure with its pin observed at FINAL time releases the exact hold with `release_evidence = 'prerequisite_closure'`.
3. **Intended retention.** uzi's own writers keep the pin while the capture is `available` and remove it after the capture is `expired` or `discarded` (S7). Cleanup is eventual and can fail durably (S7 ceiling); a pin of a hold settled by another route stays while its archive remains available.

### E2 result and GitHub anchor constraint (new in revision 12)

Evidence: [the E2 report on #2506](https://github.com/vtmocanu/uzi/issues/2506#issuecomment-6436354106), recorded 2026-10-27. A disposable private repository had no rulesets or branch protection. The probe account had exactly Write (push true, admin/maintain false) and a classic PAT with exactly the `repo` scope and no `workflow` scope. The probe used go-git v5.19.3 from API baseline `91647464` and the production pushbroker create-only, read-back and CAS-delete helpers. It bypassed the existing namespace allowlist only; it did not certify the future public entry points. Native Git independently reproduced the provider behavior. No credential, role or workflow-scope expansion is proposed.

| Tested case | go-git/pushbroker helpers | Native supplement |
|---|---|---|
| Anchor at current default tip D | Create succeeded; two read-backs matched; CAS delete and absence confirmed | Same |
| Direct pin at older A, workflows tree different from D | `workflow_scope_rejection`; both read-backs absent | Remote refusal without workflow scope |
| Direct pin at older B or C, workflows tree equal to D | Create, read-back and CAS delete succeeded | Same |

The verdict was `FINDING exit=3 findings=1 malfunctions=0`, not a probe failure and not a claim that all possible direct pins fail. Final listing showed no remaining prerequisite refs and the default branch was unchanged. Private raw-record SHA256 values: go-git TSV `c15f9ad2c4b1d738353634b90230c374d82bf91d4db2b9272c4e3cab85ad58d5`; native TSV `5906f1509ca4ed7f06d083758d23af336ca48d022697678e4d1e05ef7046ab05`. Raw files stay private; they contain setup identities and native HTTP traces and are not publication artifacts.

**Design consequence:** retain the chosen anchor D at the freshly read default tip, after proving every prerequisite P is an ancestor of D. This anchors existing forge history, not run content. Do not replace it with arbitrary direct pins at historical prerequisites: a fork point can have an older workflows tree. GitHub acceptance requires handling workflow-tree compatibility with the current default; E2 establishes the tested equality/difference cases, not a universal necessary-and-sufficient rule across all repository policies. Same-tree historical acceptance is useful evidence but does not create a need for another anchor-selection algorithm.

If the default changes its workflow tree between preparation and send, D can become incompatible. A provider refusal leaves custody retained under the existing S3 classification: explicit refusal plus absent read-back is `pin_refused`; an ambiguous send remains listing-only `create_unknown`. Do not widen the PAT, realign or rewrite the archive, create another ref, or retry at a new D after the marker. A default rewrite can also make a prerequisite unavailable before preparation; ancestry still must be positively proved. The accepted later external pin-deletion/history-rewrite residual is unchanged.

**What #2969 can rely on:** the tested transport/credential class can create and retain a prerequisite anchor at a compatible default tip without workflow permission. Once the implementation exists, #2969 may consume only the exact capture-bound `pinned` status and ordinary archive-class FINAL route, under its existing grant 1/4 rules and reservation deadline. It cannot assume upload implies pinning, promise pin latency inside its window, use a thin archive as release evidence today, grant held-publication policy to a reconstructed record, or bypass quarantine, ended-generation, custody, manifest and lifetime checks. If a required pin is absent, refused, late or unknown, this thin-closure mint route remains unavailable and its record/custody stay retained. Existing self-contained evidence and unchanged saved-FINAL replay keep their existing rules. #2969 may classify the attempt as skipped only before entry or after all admitted work settles cleanly under its existing Time/Outcomes contract; incomplete or failed capture scopes stop reclaim before successor creation, and lifecycle stops propagate unchanged. Its re-scope must measure pin readiness and the action/settlement margins within `iteration.deadline - R` at the actual candidate.

**Still not established:** GitLab/Forgejo behavior, ruleset/protected configurations, a real capture restore across a rewrite, per-capture prerequisite-count distribution E4, actual release implementation and end-to-end #2969 integration. Enable only a reviewed/tested forge configuration; leave the other forges unlisted pending their own evidence. E2 is feasibility evidence, not final architecture approval, implementation acceptance or dispatch authority. Producer-origin FINAL under #3031 remains excluded until the separate #3444 amendment and its proofs pass.

### Trust contract

| # | Fact | Established by | Checked at |
|---|---|---|---|
| T1 | The archive bytes were produced by the worker from its trusted bare, passed `git bundle verify` there, and their header prerequisites are the recorded list | worker (ADR-1296 D5) | production; header re-read before minting |
| T2 | The stored bytes are exactly the manifest's, and the manifest (checksum, size, source, prerequisites, coverage digest) is immutable once bound | server | `Service.stream` checksum and length check; immutable-declaration refusal |
| T3 | Every recorded prerequisite is an ancestor of, or equal to, the default-branch tip D | server, from the forge | S1 `CompareAncestry(D, P)` |
| T4 | A ref the server created create-only points at D | server, from the forge | S3 read-back; S6 listing outside DB locks |
| T5 | The closure, capture and hold are exactly bound, and uzi's own writers do not remove the pin while the archive is available (cleanup requires the capture to have left `available`, which is one-way) | server | release transaction rechecks; S7 predicate |

**Pinning proves prerequisite durability, not bundle validity.** Bundle validity rests on T1 and T2, the same trust the self-contained archive FINAL already relies on. The server does not parse, apply or verify bundle contents; nothing may describe the result as independent server verification. **T5 does not cover external writers:** a person or token outside uzi that deletes the pin or rewrites its history after release defeats restore; that is the accepted residual above, and it is undetected at release. P1 replaces the per-release restore verification the issue originally asked for with this contract plus the native-Git tests in C1b.

### Rejected alternatives

| Alternative | Trade-off that killed it |
|---|---|
| (1a) Store the closure in Postgres | Full-history pack per capture (about 70.7 MB, growing); #2544's storage pressure. |
| (1b) Server-side re-bundle | Full go-git fetch (OOM), over-cap result, (1a)'s cost. |
| (1c) Raise the cap | History grows (54.7 MB under #2476, about 70.7 MB now); the cap bounds worker memory, upload and storage. |
| (2b) Direct create-only pins at each prerequisite | Not a general GitHub strategy after E2: the tested older differing-workflow tip was refused, although same-workflow older tips succeeded. Retain the default-tip anchor; direct pins on another forge require their own reviewed evidence. |
| (3) Push the archive pack to a forge ref | ADR-2545 held publication; refused by design for records without held metadata. |
| (4) Give retained-claim and boot records held metadata | Reconstructs authority (#2494: material plan change). |
| (5) Server-side bundle verifier | Owners adopted P1 (2026-10-25); a second implementation of git import, exact-SHA fetch support per forge and a 512Mi envelope to check what self-contained archives are not checked for either. |
| (6) Lazy pin started by FINAL | Circular with "mint only when `pinned`", or needs a new worker-triggered endpoint; a journaled archive FINAL pre-empts held. |
| Option B / Option C | B: a later rewrite strands the archive. C: not an accepted outcome. |

## File map

Entry point: the closure lane in `api/internal/workersvc/prerequisite_closure.go`, kicked through a hook injected into the recovery service (as `Service.SetHeldPublication` injects held wiring, `api/internal/recovery/held_final.go`) and bounded by one process-wide slot (see "Concurrency bound"), and driven by a dedicated, always-registered `sweeper.Pass` named `prerequisite_closure` in `api/cmd/server/main.go` (beside `run_salvage` and `held_publication`).

| File | Change | Child |
|---|---|---|
| New prerequisite-closure migration, next free number at landing | `recovery_prerequisite_closures` table, FKs and constraints below, identity and transition triggers, guarded Down | C1b |
| `api/internal/store/queries/prerequisite_closure.sql` (new) | insert-pending, lane selection per lane with `next_attempt_at`, marker, outcome, reconcile, cleanup, FINAL lookup, removal counts | C1b, C2 |
| `api/internal/recovery/service.go` | in `Service.stream`, after `MarkCaptureReady` and before `tx.Commit`, insert the `pending` row (S0); kick after commit | C1b |
| `api/internal/store/queries/recovery.sql` | `MarkCaptureReady` gains `AND state = 'uploading'` (O12) | C1a |
| capture guard (separate C1a migration) | `recovery_inventory_capture_guard` (created in `00305_recovery_inventory.sql`, latest body in `00322_recovery_archive_redundancy.sql`, `BEFORE UPDATE ON recovery_captures`) rebuilt from its live body with one added refusal: `NEW.state = 'available' AND OLD.state NOT IN ('uploading', 'available')`; same-state updates of an `available` row (expiry, redundancy proof, protection) stay allowed | C1a |
| `api/internal/store` and `api/internal/recovery` LiveDB fixtures | audit direct and parameterized available writes; stamp uploading where needed, preserving assertions; include reconcile and stored-files fixtures rather than relying on the old two-file list | C1a |
| `api/internal/pushbroker/pushbroker.go` | `PrereqRefPrefix`/`PrereqRef`; `PreparePrereqCreate` and `SendPrereqCreate` mirroring `PrepareHeldPack`/`SendHeldCreate`; namespace accepted by `validManagedRef`; **`Delete` refuses an empty `ExpectedOldTip` for `refs/uzi-prereq/` as it does for recovery, salvage and held refs (~589-590)**; own create validation rather than widening `validateCreateRef` | C1b |
| `api/internal/workersvc/prerequisite_closure.go`, `prerequisite_closure_cleanup.go` (new) | lanes S1 to S4 and S7, bounded fair pass, audit | C1b |
| `api/cmd/server/main.go`, `api/internal/handler/handler.go` | the `prerequisite_closure` sweeper pass and the kick hook wiring | C1b |
| `api/internal/apitypes/recovery.go` | worker status `prerequisite_closure` (C1b); owner `RecoveryArchiveDTO.PrerequisiteClosure` (C3) | C1b, C3 |
| `api/internal/config/config.go` | knob `UZI_PREREQUISITE_CLOSURE_FORGES` (default empty, parsed like `UZI_SALVAGE_FORGES`) | C1b |
| `api/internal/capability/capability.go` | capability constant `recovery_prerequisite_closure_v1`, protocolVocabulary and protocolOrder entries (C1b, used for S0 provenance); no protocolFeatures entry until C2 | C1b |
| `api/internal/handler/worker_protocol.go` | advertise the protocol feature `recovery_prerequisite_closure_v1` **only from C2** | C2 |
| `api/internal/handler/forge.go` | the two removal paths, connection delete (pre-count then `DeleteForgeConnectionForUser`, ~531-552) and repository delete (pre-count then `DeleteRepoForUser`, ~1018-1033), each gain a live-closure count and catch the new RESTRICT by constraint name beside `isHeldRestrict` / `heldRestrictConstraint` (~1160) | C1b |
| `api/internal/recovery/exact_observation.go` | `exactHoldObservationSQL` (archive branch ~143-148) and `validExactClosedObservation` (~74-76) admit `release_evidence` in {`archive`, `prerequisite_closure`} for disposition `archive`, `prerequisite_closure` only with a non-null `final_closure_id` | C2 |
| New closure-release migration, next free number at landing | hold `final_closure_id`, release-evidence domain rebuilt from 00338 plus `prerequisite_closure`, tie CHECKs, guard branch rebuilt from the live guard at landing | C2 |
| `api/internal/store/queries/recovery.sql`, `api/internal/recovery/final_inventory.go`, `api/internal/handler/recovery.go` | closure alternative in the FINAL predicate; typed refusals; replay unchanged | C2 |
| `api/internal/recovery/reconcile.go` | closed-hold echo for a closure release: unchanged shape (kind `archive`, no `release_evidence`), pinned by a test | C2 |
| `agent/src/recovery.ts`, `agent/src/client.ts`, `agent/src/protocol.ts`, `agent/src/worker.ts` | closure-mint permission, origin check, mint-versus-replay split, `validExactClosedProof` accepting `prerequisite_closure`, capability advertisement | C2 |
| owner surface (`convert.go`, `fixtures/api-contract/*`, `web/src/lib/apiTypes.ts`, web panel, `api/cmd/uzi/`) | owner DTO and rendering | C3 |
| `docs/run-recovery.md`, `docs/configuration.md`, `docs/cli.md` (+ `task docs:sync`) | restore procedure, trust contract, accepted residual, per-forge notes, knob, purge caveat | C1b, C2, C3 |
| new ADR (numbered by the C1b child issue); amendments to `adr/1296-durable-run-recovery.md`, `adr/2417-guarded-local-retention.md`, `adr/1197-transient-recovery-park.md`; `ARCHITECTURE.md` link | decision record | C1b, C2 |
| `specs/human.md` | update the Feature #1349 sentence on archives with prerequisites | C2 |

`/api/v1` (`api/openapi/v1.yaml`) is untouched.

## Contracts

### Split review amendments at c750b802

The owner approved C1a/C1b, both API-only, on 2026-10-28. C1a is independently useful and contains no closure pinning. C1b cannot dispatch before C1a is merged; the entire production lifecycle is present before S0 can create a row. Removing guard/fixture churn does not waive any C1b state/race/restore test. C2 depends on C1b and owns all agent changes, feature/worker advertisement and release.

Capability registration in C1b includes protocolVocabulary and protocolOrder (`capability.go:292-346`), which FilterProtocol requires; it stays out of protocolFeatures (`worker_protocol.go:403`) until C2. Test both boundaries. Parse the forge knob like UZI_SALVAGE_FORGES (`config.go:1481-1501`): known github/gitlab/forgejo kinds, trimming/lowercase/deduplication, empty off, unknown boot refusal. This parser is not proof support: docs authorize only the probed GitHub configuration for initial enablement, other kinds need their own reviewed evidence.

S2's precheck treats a deleted run or closed/ineligible hold as archive_unavailable before setting a live pointer, not a recurring 23503. C1b's thin-FINAL rejection test must redden on deletion of the nonempty-prerequisites clause (`final_inventory.go:141`). C1a's query mutation asserts pgx.ErrNoRows rather than any error so the trigger cannot mask it. PR #3580 landed at `b4f7d5ff` (migration 00372); the C1a audit below includes that base. Its migration/query/FINAL/protocol/service seams remain re-audit boundaries for C1b, not permission to change its authority.

### Ref namespace

`refs/uzi-prereq/<capture-id>`: outside `refs/heads/` and `refs/uzi-checkpoints/`, keyed by the server capture id, created only create-only by the API, removed only by CAS delete at the recorded tip (never an unconditional delete; enforced in `pushbroker.Delete`), never moved or forced.

### Closure row, keys and constraints

- `capture_id` (unique; FK to `recovery_captures`, RESTRICT), `hold_id` (FK to `recovery_custody_holds`, RESTRICT), `run_id`, `user_id`, `repo_id` (plain provenance, no FK, as in `run_held_publications`).
- **Live pointer:** `live_run_id uuid CONSTRAINT recovery_prerequisite_closures_live_run_id_fkey REFERENCES runs(id) ON DELETE RESTRICT`, with `CHECK (live_run_id IS NULL OR live_run_id = run_id)`. **Null in `pending`.** Set only by S2; non-null in `invoked`, `create_unknown` and `pinned`; cleared on entering `refused`, `foreign`, `lost` or `deleted`. The connection-delete and repository-delete paths in `api/internal/handler/forge.go` refuse while any row of theirs has a non-null pointer, by pre-count and by matching the constraint name on the RESTRICT error (the held precedent, `00333_held_publication.sql`, `heldRestrictConstraint`). Runs are removed through those cascades; whether any other run-deletion path exists was not audited (C1b checks).
- **Frozen forge binding**, written by S2 together with `pin_tip`: connection id, forge kind, normalized base URL, project id and default-branch name. Cleanup and FINAL listings use only the frozen binding; a connection whose kind, base URL or project changed fails closed (no send, no delete; the row waits).
- Copied and immutable from insert: `prerequisite_shas`, `checksum`, `pin_ref`. `pin_tip` and the binding immutable from `invoked`.
- Bookkeeping: `attempts`, `next_attempt_at`, `last_error` (bounded), `cleanup_attempts`, `cleanup_exhausted_at`, `absent_since` (the O11 absence candidate).

### Lock order

| Transaction | Order | Note |
|---|---|---|
| FINAL (existing) | worker → run (`FOR UPDATE`) → hold (`FOR NO KEY UPDATE`) → capture (`FOR UPDATE`) → closure | closure read added last |
| Upload `stream` with S0 (existing plus insert) | hold (`FOR NO KEY UPDATE`) → capture (`FOR UPDATE`) → closure insert | **no run lock and no run FK value**: the row's FKs reference the already-locked hold and capture, and `live_run_id` is null, so S0 adds no lock on `runs` |
| S2 marker | run (`FOR KEY SHARE`, the lock the FK check needs, taken first) → hold → capture → closure | taking the run lock first matches FINAL's order, so an S2 waiting for FINAL holds nothing FINAL needs |
| S3/S4/S6/S7 outcome writes that clear the pointer | capture → closure (S7 also rechecks the capture under its lock) | clearing an FK value takes no lock on `runs` |

The S2 order closes the inversion Astra identified: with a run FK set while holding hold and capture, S2 could wait for the run's key-share lock that FINAL holds while FINAL waits for the hold.

### The closure sequence (one sequence; bounded; idempotent)

Every forge call runs with no transaction open and its own per-call timeout.

| Step | Actor | What happens | Bound and failure behaviour |
|---|---|---|---|
| S0 Trigger | `Service.stream` | After `MarkCaptureReady`, before commit, insert one `pending` row (unique on `capture_id`, `ON CONFLICT DO NOTHING` as backstop) when: hold inventory-guarded; bound `prerequisite_shas` nonempty; knob lists the repository's forge kind; and the authenticated uploading worker (proven by `lockCapture` to be the capture's original worker) has `recovery_prerequisite_closure_v1` in its registered `ProtocolCapabilities`. The request carries no capability field. After commit, submit a kick to the bounded executor. | Atomic with availability: a rollback leaves no row; a lost-ACK retry is answered from the existing receipt (`admit` or `stream`) without re-inserting; a concurrent upload serializes on the capture lock. The sweep never creates rows, so earlier captures never become eligible after an upgrade or a knob change. Producer captures are pinned too (the server cannot tell them apart); cost accepted. |
| S1 Prepare | lane | Short read of row and capture; outside transactions: list the remote once (target absent; D taken from that same advertisement's `refs/heads/<default>`, the single source of D), resolve the binding, then `CompareAncestry(D, P)` per P (at most 64). | Nothing sent. Not-ancestor: `refused` (`prerequisite_not_on_default`). Target present: `refused` (`pin_foreign`). Unknown, timeout, rate limit: `attempts`+1, capped backoff; at the attempt ceiling `refused` (`prerequisite_ancestry_unknown`). Ineligible capture or hold: `refused` (`archive_unavailable`). |
| S2 Marker | lane | Transaction in the S2 lock order: recheck capture `available`, hold open, binding unchanged since S1, repository and connection present; CAS `pending → invoked` with `pin_tip = D`, frozen binding, `live_run_id = run_id`, `invoked_at`. | Single winner; losers stop. A changed binding returns the row to the S1 schedule without sending. No path returns to `pending` after `invoked`. |
| S3 Send | marker winner | Outside transactions: one create command (Old = zero hash, empty pack) and one read-back. CAS on `invoked`: read-back at D → `pinned`; explicit `ng` and no ref → `refused` (`pin_refused`); anything else → `create_unknown`. | Exactly one create per capture, ever. A crash or deadline before the outcome write leaves `invoked`; the pass treats `invoked` older than the send deadline as `create_unknown`. Never resent, never at another tip. |
| S4 Reconcile | lane | For `create_unknown`: listing only. At D → **commit `pinned` before anything else** (including any delete); at another tip → `foreign`; absent → unchanged. | Capped backoff, indefinitely (ADR-2545 `create_unknown` precedent); the live pointer stays set, so removal stays blocked. Elapsed time is never proof. |
| S5 Status | worker | Reads capture status; additive `prerequisite_closure` reports `pending` (covers `pending`, `invoked`, `create_unknown`, **and `pinned` with `absent_since` set**, the last with reason `pin_absence_unconfirmed`), `pinned` (only with `absent_since` null), `refused`, `lost`, `unsupported`. The mint gate accepts only `pinned`. | Read-only; the worker never triggers or waits on the lane, except #2969's bounded poll. |
| S6 FINAL | worker, API | Worker mints only under the closure-mint permission (Worker behaviour). API: exact replay first; existing preconditions; one listing of the pin outside transactions behind a per-worker limiter; release transaction in FINAL lock order rechecks `pinned` and binding. | Pin absent: the absence candidate is recorded in **its own short transaction** (capture then closure lock; CAS on `pinned` and `absent_since IS NULL`), committed before the response and before any release transaction is opened; FINAL is denied with **409 `prerequisite_pin_missing`**; the pointer is kept until a confirming listing (see States). Pin present at `pin_tip` but an absence candidate is recorded: still denied until a later pass clears the candidate. Foreign: `foreign`, 409. Listing failure or **limiter denial**: 503 `prerequisite_pin_unavailable`; the journaled FINAL retries through `final_retry_unknown`. |
| S7 Cleanup | lane | Only for rows whose capture is `expired` or `discarded` (rechecked under the capture lock). For `pinned`: listing; absent → `deleted`; at `pin_tip` → CAS delete, confirming listing, `deleted`; foreign → `foreign`, untouched. `create_unknown` is first resolved by S4 (persisting `pinned` before any delete). | An ambiguous delete (sent, no answer) leaves the row `pinned`; the next listing finding the ref absent ends it `deleted`. **Ceiling:** after the cleanup attempt ceiling, set `cleanup_exhausted_at`, keep the row `pinned` with its live pointer (removal stays blocked), surface `pin_cleanup_exhausted` to the owner and audit, and continue listings at the maximum backoff so later observed ref absence is recorded under S7. This does not authorize an out-of-band delete or reset the delete-attempt ceiling. No timeout or label clears the pointer. |

**Bounded, fair pass.** The `prerequisite_closure` sweeper pass is always registered (knob gates S0 only, never S1 to S7 on existing rows). Per pass: a whole-pass budget, a per-item timeout with a reserved tail for the outcome write, a per-lane scan limit and a total item cap (initial values follow the salvage pass's documented share of the tick, not the held cleanup pass's 30 s: **10 s pass budget, at most 5 items per pass, plus up to 5 s for the outcome write of the item in flight**, per the `run_salvage` registration comment in `api/cmd/server/main.go`; per-lane scan limit 10 as `heldCleanupScanLimit`; tuned from E6).

**Sweep latency cost.** `sweeper.Engine.runOnce` runs every registered pass sequentially **before** `Service.Sweep` (the run-liveness sweep) on each tick (`api/internal/sweeper/sweeper.go`), so this pass can delay run-liveness by up to its budget plus the outcome tail, about 15 s per tick in the worst case, on top of the existing passes' shares. The budget above is chosen to match salvage's share for that reason.

**One item drives a new row to the pin.** A `pending` row is processed as one bounded item: S1, S2 and S3 run consecutively in that item, so a single slot acquisition can take a fresh capture to `pinned`. Before S2 the item checks that the remaining item deadline covers the send and read-back plus the outcome tail; if it does not, it stops **before** the marker (nothing sent, row stays `pending`, `next_attempt_at` strictly later than now under preparation backoff), so the lane never knowingly starts a marker it lacks the time to follow with a send. This does not guarantee a send after every marker: a crash, cancellation or shutdown after the S2 commit and before the S3 send can still leave a row `invoked` with nothing sent. The recovery path for that case is unchanged: the pass moves an `invoked` row older than the send deadline to `create_unknown`, which is resolved by listing only (S4) and never resent; the C1b "crash after S2" test covers it. Lane fairness is unchanged: the new-row lane is one of the round-robin lanes. Lanes (prepare, stale-invoked, reconcile, cleanup, cleanup-retry) are interleaved round-robin, and every skipped or failed item gets a `next_attempt_at`, so no lane or row head-of-line blocks the others.

**Repeated preparation-budget exhaustion (C1b).** Start S1 only when the remaining pass budget covers the full per-item budget (proof plus send/read-back plus outcome reserve). Otherwise decline before S1: no attempt spent, no marker/send, no terminal refusal; the row retains eligibility for a later full-budget turn under the existing fair selection. The plan must show numerically that the full per-item budget fits inside the pass budget. An item that starts S1 with that full budget consumes a preparation attempt even if its positive proofs finish too late for the required send/read-back/outcome reserve. Persist a positive backoff and next_attempt_at strictly later than now, without caching partial proofs across attempts. Use the same finite preparation-attempt ceiling as S1 unknown proofs; the plan pins that numeric ceiling/backoff and per-call contexts within the unchanged ten-second pass and outcome tail. At the ceiling a no-marker budget shortfall is terminal refused with prerequisite_prepare_budget_exhausted. An incomplete/timeout ancestry proof retains prerequisite_ancestry_unknown instead. No-slot or before-S1 deferral spends no attempt; it cannot consume repeated item selections that starve due rows. Required repeated-exhaustion test mixes other pending rows and lane classes, proves progress/backoff/ceiling for full-budget attempts, zero markers/sends for the exhausted row and no budget increase. Extend it with earlier items repeatedly draining the pass: the late row never starts S1, spends no attempt and is not refused; it remains eligible and can start on a later full-budget turn. Each broker call uses a shorter lane child context; its 30/60-second outer ceiling is not the lane budget.

**Concurrency bound (numeric).** At most **one** closure item (any of S1 to S4, S7 or an audit listing, whether started by a kick or by the pass) performs forge work at a time **per API process**. Mechanism: one process-wide slot, `prereqSlots = make(chan struct{}, 1)` on `workersvc.Service`, acquired without waiting, the same pattern as the held step-A semaphore (`heldSlots`, capacity `heldPublishMaxConcurrent = 4`, `AcquireHeldPublishSlot` in `api/internal/workersvc/held_publish.go`) but a separate slot, so closure work never consumes held publication capacity. Sweep work reuses the existing executor, the `sweeper.Engine` loop (`api/internal/sweeper/sweeper.go`), which runs registered passes sequentially on one goroutine; the pass holds the slot for one item at a time and stops early when the slot is busy. A kick is a single goroutine started after the upload commit that tries the slot once: if the slot is busy it exits immediately (the kick is dropped), otherwise it runs that one row's next step and releases the slot. **A dropped kick loses nothing:** the S0 row is durable and the next pass selects it by `next_attempt_at`. The S6 FINAL-time listing is not lane work: it runs on the FINAL request, outside transactions, behind the per-worker limiter, and does not take the slot.

**Cancellation and shutdown.** Run cancellation does not touch the lane. Capture or hold ineligibility before S2 ends `refused`; after a pin exists it stays until S7. API shutdown leaves durable rows that the pass resumes; S3's ambiguity resolves through S4.

**#2969's bounded window.** Inside `recoverRetainedClaim`, #2969's attempt may poll the capture status (bounded reads under its own cap and abort signal) and mint only if it observes `pinned` before the cap and holds the closure-mint permission. Otherwise it skips: no FINAL journaled, custody retained, reservation never extended. A FINAL already journaled whose send does not complete stays journaled for later replay. #2969's re-scope must measure pin latency (upload commit to `pinned`, including kick and pass scheduling) against its actual remaining window, `iteration.deadline − R`, not against the nominal 300 s.

### States

| From | To | Only when |
|---|---|---|
| `pending` | `invoked` | S2 |
| `pending` | `refused` | S1 refusal or ineligibility; pointer null |
| `invoked` | `pinned` / `refused` / `create_unknown` | S3 outcome; `refused` clears the pointer |
| `invoked` (stale) | `create_unknown` | pass, after the send deadline |
| `create_unknown` | `pinned` | S4 listing at `pin_tip` (committed before any delete) |
| `invoked` / `create_unknown` / `pinned` | `foreign` | a listing shows another tip; pointer cleared; never touched |
| `pinned` | `pinned` with `absent_since` set | the first non-cleanup listing (S6 or audit) that shows the ref absent records the absence candidate; FINAL is denied from that moment; pointer kept |
| `pinned` with `absent_since` | `pinned` (candidate cleared) | a later listing shows the ref at `pin_tip` |
| `pinned` with `absent_since` | `lost` | a fresh confirming listing in a later bounded pass (never the same pass, and **at least 10 minutes after `absent_since`**) still shows the ref absent; pointer cleared |
| `pinned` | `deleted` | S7; pointer cleared |

`refused`, `foreign`, `lost` and `deleted` are terminal and hold no pointer. A `pinned` row whose hold closed by another route stays `pinned` until S7. Audit cadence: a listing of `pinned` rows at most once per row per 24 hours, inside the pass budgets; a row with an absence candidate is re-listed ahead of the audit cadence, once per pass at most. The FINAL predicate requires `absent_since IS NULL`.

### Typed refusals (bounded; each retains custody)

`closure_unsupported`, `prerequisite_not_on_default`, `prerequisite_ancestry_unknown`, `prerequisite_prepare_budget_exhausted`, `pin_absence_unconfirmed`, `pin_refused`, `pin_unknown` (reported as status `pending`), `pin_foreign`, `archive_unavailable`, `pin_lost`, `pin_cleanup_exhausted` (owner and audit only). An upload refused `507` never becomes `available`, so no row exists.

**Per-forge note.** Forgejo's `CompareAncestry` never answers "not an ancestor" (`api/internal/forge/forgejo_ancestry.go`, two-call proof): a genuinely pruned prerequisite on Forgejo ends `prerequisite_ancestry_unknown` at the attempt ceiling, not `prerequisite_not_on_default`. Custody is retained either way; the docs state it per forge.

### Worker-facing status

Additive optional `prerequisite_closure` on `RecoveryCaptureStatusResponse`: `state`, `reason`, `prerequisite_shas`, `checksum`. Mirrored in `agent/src/protocol.ts`.

### Complete worker status mapping (C1b)

For a manifest-bound self-contained capture (empty prerequisite list), omit the nested closure field. For every other capture, no closure row reports unsupported/closure_unsupported (including old worker, disabled knob, absent capability or unbound capture). Do not expose raw last_error. Rows return their frozen prerequisites/checksum; no-row bound captures may return existing manifest facts; unbound captures omit unavailable facts.

| Durable row | Status | Reason |
|---|---|---|
| pending | pending | absent, or prerequisite_ancestry_unknown during a scheduled ancestry retry; never the terminal prerequisite_prepare_budget_exhausted reason |
| invoked / create_unknown | pending | pin_unknown |
| pinned, absent_since null | pinned | absent |
| pinned, absent_since nonnull | pending | pin_absence_unconfirmed |
| refused | refused | persisted bounded refusal reason |
| foreign | refused | pin_foreign |
| lost | lost | pin_lost |
| deleted | unsupported | archive_unavailable |

Test every no-row/self-contained/terminal mapping and TestRecoveryWorkerRPCTags (api/internal/apitypes/wire_test.go:1362 at c750b802). The bounded vocabulary additionally includes prerequisite_prepare_budget_exhausted and pin_absence_unconfirmed; no unsupported/omitted status is positive authority. pin_cleanup_exhausted remains owner/audit-only and is not a worker status reason. C1b adds no agent protocol field; C2 mirrors the response and consumes it.

### FINAL authorization predicate (new)

Authorized when every existing condition holds **and either** (i) the prerequisite list is empty (`release_evidence = 'archive'`, unchanged) **or** (ii) a closure row for exactly this capture is `pinned`, with no absence candidate (`absent_since IS NULL`), with `prerequisite_shas` and `checksum` equal to the capture's; the API advertises the feature, the worker advertises the capability and the forge is listed; the S6 listing observed the pin at `pin_tip`; and the release transaction re-reads `pinned` and the binding. Then `release_evidence = 'prerequisite_closure'` and `final_closure_id` is set. The guard gains a matching branch; NULL-safe CHECKs tie the evidence, `final_disposition = 'archive'` and `final_closure_id`.

- **Exact replay is unchanged and bypasses every closure check.** `releaseFinalInventory` answers a released hold first and, for kind `archive`, compares kind, capture, source and digest but not evidence (`final_inventory.go`, ~94-100). A lost ACK is therefore recoverable after pin loss, capture expiry, knob-off, capability removal or a forge outage.
- **The predicate reads no worker-held metadata** and nothing writes or reconstructs `exactHoldBinding` or `heldPolicy`.
- **Listing limits.** The listing is not atomic with the commit and no cross-forge ref protection is assumed. Guaranteed: an observed missing or foreign pin refuses; uzi's cleanup cannot remove a pin a release relies on (one-way `available`, shared lock order). Not guaranteed: an external writer deleting or rewriting after the listing or after release; accepted residual (Goal 2), surfaced afterwards by the audit as `pin_lost` where it is observed, undetected otherwise.

### Closed-hold reconciliation

- **Server echo** (`reconcile.go`, ~61-73): a released hold with disposition `archive` is echoed as kind `archive` with capture, source and digest, and `release_evidence` only for `settled`. A closure release keeps exactly this shape. The worker's reconcile check (recovery.ts ~2120-2146) compares the saved request and `response.release_evidence` (absent for archive) and accepts it unchanged.
- **Exact closed proof, both ends.** Three checks reject the new evidence today and are widened narrowly in C2, for disposition `archive` only, to `release_evidence` in {`archive`, `prerequisite_closure`}:
  - server SQL `exactHoldObservationSQL` (`api/internal/recovery/exact_observation.go`, archive branch ~143-148: `h.final_disposition='archive' AND h.release_evidence='archive'`); the `prerequisite_closure` alternative additionally requires a non-null `final_closure_id` naming a closure row for `h.final_capture_id`;
  - server Go `validExactClosedObservation` (same file, ~74-76: `out.ReleaseEvidence != f.Kind` refuses);
  - agent `validExactClosedProof` (`agent/src/client.ts`, ~5289; archive case ~5305: `value.release_evidence === f.kind`).
  Every other kind keeps its exact evidence equality. Tested end to end through `getExactRecoveryHold` (worker client to API to SQL): a closure-released hold observes as `released`/`settled`, and a hold with any other evidence for disposition `archive` is still refused.

### Authority by freeze path (archive-class only)

| Record (produced by the real path) | Held publication | Closure-backed FINAL |
|---|---|---|
| Terminal, new, with binding and policy | unchanged | only with the closure-mint permission (proven ineligibility or same-pass held fallback) |
| Terminal-matched (binding, no policy) | refused (v2 guard) | permitted through the v2 missing-metadata grant |
| Retained-claim | refused (v2 guard) | permitted through the v2 grant, once the generation ended or inside #2969's window rule |
| Boot reconstruction | refused (v2 guard; #2494) | permitted through the v2 grant |
| Producer origin (`classifyRecoveryOrigin` ≠ `ordinary`) | refused | **never** in this PRD; needs the separately reviewed #3444 amendment |

### Worker behaviour

- **Closure-mint permission, default deny.** An explicit permission value is threaded `frozenOperationCycle` → `captureCycle` → `finalizeInventory` → `finalizeInventoryWithinBoundary`; every other caller (`resumeInventory`, the held reconcile at ~2106, the saved-FINAL first branch) passes deny. **Named second grantor (#2969):** `Runner.recoverRetainedClaim` → `RecoveryCoordinator.captureAndUpload` (recovery.ts ~2621-2626, which runs `captureCycle` under the capture-cycle lock) → `finalizeInventory` (~1544) may pass the permission, but it may evaluate **only grants 1 and 4** below, inside #2969's existing cap, abort signal and outcome rules; it never evaluates grants 2 or 3 and never waits past its cap. All other `captureAndUpload` callers pass deny. It is granted only:
  1. in the v2 missing-metadata branch (2343), and not while `heldCreateUncertain`;
  2. on **proven** ineligibility: `!isCodePublishingKind(record.kind)`; or a foreground terminal whose `fail_origin` is `push_secret_blocked` or `worker_residue_blocked`; or, under v2, an exact observation that succeeded, matched run, generation, hold, repository, guarded and open, and showed a run status other than failed, cancelled or completed. A failed, absent or mismatched observation is **unknown** and never counts as proof;
  3. after a held attempt in the same pass returned `fallback`: the completed-run path must then re-run `captureCycle`/finalize with the permission rather than return the earlier `archived` outcome (today's `archived ?? captureCycle` at 2461). The two other held declines in `frozenOperationCycle` are classified: (a) ~2299, a replayed saved held FINAL that no longer exists after `finalizeInventory` (the `replayedHeld` branch): grant 3 only when that call's outcome is `held === "fallback"`, otherwise explicit deny; (b) ~2329, a `heldCreateUncertain` reconcile that resolved to `create_refused`, or to `created` followed by `releaseHeldWithinCycle` returning `fallback`: grant 3; every other exit there already returns `kept` (deny).
  4. on **structural** ineligibility: one snapshot of the negotiated server feature set positively contains `recovery_prerequisite_closure_v1` and positively lacks **both** `recovery_held_publication_v1` and `recovery_held_publication_v2` (for example `UZI_HELD_PUBLICATION` off). The closure feature's presence proves the snapshot is a received advertisement; held publication need not be enabled.
  It is denied in every other case, including unknown eligibility, an unknown or cleared feature set (no positive closure feature), `heldCreateUncertain`, and a held `kept`/`settled` result.
- **Origin check.** Minting additionally requires `classifyRecoveryOrigin(record.captureId) === "ordinary"`.
- **Mint versus replay.** Minting requires the permission, the API feature, status `pinned` with prerequisites and checksum equal to the record, a `healthyInventoryStatus` capture, and the local header equal to the record.
- **Saved thin FINAL replay (verified gates at `91647464c`).** `finalizeInventoryWithinBoundary` gates replay today in two places: the thin check (1886-1890) and, for every non-settled, non-held request, a capture status read (1897-1900) that must pass `healthyInventoryStatus` (recovery.ts ~225-227: `available`, manifest bound, no reason, unexpired), otherwise it diverts to `reconcileInventory` or returns (1901-1904). For a record whose MAC-authenticated saved `finalRequest` is kind `archive` and whose record is thin, C2 specifies:
  1. the thin check is replaced by "the local bundle header equals the saved record";
  2. the capture status read is **not a gate**: neither a read failure nor an `expired`, `discarded` or otherwise unhealthy status prevents sending; the status may be read for diagnostics only;
  3. the worker sends the **unchanged saved request** through `releaseRecoveryCustody`; every other existing replay precondition stays (`recovery_inventory_v1`, `inactiveInventory`, `prove()`, quarantine checks, `heldOperationState`/`finalHoldAllowsReplay`);
  4. after a non-ACK answer or a failed send, the **existing** `reconcileInventory` runs unchanged (revision 8's "replaceable never taken" rule is reversed; its "read-only reconcile" wording was wrong). Its server side, `Service.Reconcile` (`api/internal/recovery/reconcile.go`, locks worker, run, hold, capture), answers `accepted` **first** for a released hold with a final receipt (~61-73); returns `replaceable` only after ruling out `hold_not_open` (~79-81), a non-original live worker and an unended generation, after `reconcileRetainedReason` finds no retained reason, and after `FenceUnacceptedInventoryCapture` has moved the unaccepted capture to `expired` (`unaccepted_capture_replaced`) and committed (~103-114). The worker takes `replaceable` only through the existing validated branch (recovery.ts ~2173-2191): response shape and `replaceableInventoryStatus` on the observed status, `openInventoryHold`, `newProductionAllowed`, `prove()`, `inactiveInventory`, quarantine, and the identity compare-and-set over coverage, roots, state, capture id, source, digest, checksum, size, idempotency key and saved request. Only then is the saved request cleared and the record returned to `bundled`;
  5. a timeout, a malformed response or a `retained` outcome never clears the saved request (unchanged: those return `unknown` or record the reason). Replay itself mints nothing new and attempts no held fallback; a replacement is a new capture under the same hold, through the normal upload, a new S0 row and a later mint under the permission rules. The fenced capture's closure row is no longer `available`-backed and is cleaned by S7.
  The server answers exact replay before any mutable check (`final_inventory.go` ~94-100; archive replay compares kind, capture, source and digest, not evidence), so a committed ACK is recoverable through the release call itself even when the capture has expired, and through the reconcile `accepted` echo when the release call fails; a committed release is never `replaceable` because `accepted` is answered first.
- The v2 held-metadata guard is unchanged; no new MAC-covered journal field; quarantine and covering-ACK cleanup unchanged.
- **Stuck-FINAL worst case (documented).** A journaled closure FINAL refused after minting (pin lost or foreign, knob turned off, API rollback) retries as `final_retry_refused` and keeps pre-empting held publication for that record while the capture is `available`. Once the capture has expired and reconcile answers `replaceable`, the existing validated path may replace it (new capture, new S0 row, a later mint under the permission rules); if reconcile instead answers `retained`, times out or is malformed, the hold stays open (custody and local source retained, an admission slot used) until an owner discard or another settlement route. No data is lost; the case is owner-visible.

### Sequence

```mermaid
sequenceDiagram
    participant W as worker
    participant A as api
    participant F as forge
    W->>A: upload thin guarded bundle
    A->>A: stream tx: bind, MarkCaptureReady, insert closure pending, commit
    A->>F: S1 list (target, D), CompareAncestry(D, P) per P
    A->>A: S2 run, hold, capture, closure: pending to invoked, pin_tip = D
    A->>F: S3 create refs/uzi-prereq/<capture> at D (Old = zero), read back
    A->>A: pinned / refused / create_unknown (S4 lists only)
    W->>A: GET capture status: pinned
    W->>W: closure-mint permission and ordinary origin checked
    W->>A: FINAL {archive, capture, source, digest}
    A->>F: list pin (no transaction)
    A->>A: worker, run, hold, capture, closure; recheck; release prerequisite_closure
    Note over A,F: capture expired or discarded: S7 CAS-deletes the pin
```

### Where tests exercise each interface

- **C1b native-Git restore, API-only implementation.**
  - Existing T1 shape evidence: `agent/test/recovery-guarded-producer.test.ts` at `c750b802` already drives the real producer (86), checks thin prerequisites and fresh imports (147-150,174-175,191-202). Cite those tests, add no duplicate agent test, protocol change or worker advertisement in C1a/C1b.
  - API (restore): a Go test with the `git` binary builds history and bundles in temporary repositories at test time, drives the lane against a file-backed remote and asserts: (1) valid archive, force-push after pinning, clean clone plus `git fetch origin refs/uzi-prereq/<capture-id>` then `git bundle verify` and fetch of `recovered-source` succeed; (2) mismatched manifest built to discriminate: bundle excludes `^P1 ^P2`, P2 reachable from no forge ref and not from P1, manifest declares `[P1]`; with a self-check that P2 is absent from the clean clone and the pinned history, native restore fails naming P2; separately, a second upload declaring a different list is refused `ErrManifestConflict`; (3) missing prerequisite: P not on the default, `refused` (`prerequisite_not_on_default`), no pin, native import fails; (4) force-push before pinning: as (3).
  - **C1b positively asserts thin FINAL stays refused** (`ErrManifestConflict`) for a `pinned` closure; FINAL success is C2's.
  - The worker header refusal for case (2) is asserted in C2's agent tests.
- **C1b S0 atomicity (LiveDB through `recovery.Service.Upload`):** rollback leaves no row; lost-ACK retry leaves one row and no second create; concurrent upload leaves one `available`, one row, at most one create; provenance (no capability, no row; re-registration does not back-fill); knob off then on does not back-fill.
- **C1a ready-query and guard tests:** MarkCaptureReady on a valid bound expired/discarded capture returns exactly pgx.ErrNoRows; remove only the query predicate and a trigger exception must fail that assertion. Raw UPDATE and parameterized MarkCaptureState entering available from an invalid state raise SQLSTATE 55000; remove only the new trigger branch and observe red. Place the new refusal before the identity branch's silent RETURN NULL. Uploading to available and existing available updates remain valid. Public Upload's existing expired/discarded refusal is a characterization, not claimed baseline red. Down restores the exact preceding live guard (00322 Up body at c750b802); re-audit after conflicting landings.
- **C1a writer/fixture audit:** include MarkCaptureState (`queries/recovery.sql:43-51` at c750b802), whose parameterized SQL can write available even without a current production caller. Inspect complete helpers in store/recovery_livedb_test.go, store/recovery_inventory_livedb_test.go, recovery/stored_files_rework_livedb_test.go and recovery/reconcile_livedb_test.go. The last calls e.upload before its state assignment; do not automatically claim it enters available from reserved. Repair actual invalid transitions by stamping uploading, never weakening tests.
- **C1b lock order and lane (LiveDB):** barrier-driven S2/FINAL and sibling-upload races; no deadlock or forge call under DB transaction; only the marker sets the pointer. A deleted run or closed hold before S2 refuses archive_unavailable without a recurring FK error. Every state transition, crash after S2, ambiguous outcomes, unknown listing-only and no resend, wrong-tip protection, S4 pinned commit before delete, ambiguous delete then absence, cleanup ceiling/pointer retention, and backlog fairness are tested. Existing C1a tests remain green; C1b does not duplicate the guard rebuild.
- **C2 worker (real freeze paths, real `GitCache`):** records from `settleGuardedInventory`, `recoverRetainedClaim`, `materializeBootInventory`, and a terminal-matched record; held refusal unchanged; closure FINAL per the authority table; no held metadata written.
- **C2 closure-mint permission failure cases:** (a) completed, held-eligible, thin, `pinned`: no closure FINAL is journaled in the archive-first `captureCycle`; after a held `fallback` in the same pass the closure FINAL is minted (not the earlier `archived` returned); (b) unknown eligibility (exact observation throws, returns nothing, or mismatches): no closure FINAL, held not pre-empted; (c) `heldCreateUncertain`: no closure FINAL. Positive cases for the v2 grant and each proven-ineligibility form.
- **C2 origin:** a producer-origin thin record with a `pinned` closure mints no FINAL (cross-referenced to #3444 acceptance 3, "Thin archive stays protected"); a malformed origin likewise.
- **C2 replay (worker and server, lost ACK):** a committed closure release replays to the same ACK after pin loss, capture expiry, knob-off, capability removal and a forge outage; the worker replays without reading closure status. Named worker cases: saved thin FINAL with (i) capture status `expired`, (ii) capture status read throwing, (iii) status `discarded`: each sends the unchanged request and records the ACK. Named replacement case: hold still open, capture genuinely unaccepted: the server fences it (`expired`, `unaccepted_capture_replaced`) and answers `replaceable`; the worker takes the validated branch, clears the saved request and returns the record to `bundled`; a fresh upload creates a new capture and a new S0 row. Named negative cases: a reconcile timeout, a malformed response and a `retained` outcome each leave the saved request unchanged; a committed release never yields `replaceable` (the `accepted` echo wins). Named server case: `releaseFinalInventory` answers the exact replay of a closure release with the original ACK after capture expiry, pin deletion and knob-off.
- **C2 structural ineligibility (O9):** feature set with the closure feature and neither held feature grants the permission; a cleared or never-received feature set denies; a set with either held feature does not grant through this route.
- **C1b absence status (S5):** while `absent_since` is set, the capture status reports `prerequisite_closure.state = "pending"` with reason `pin_absence_unconfirmed`, and the worker mints no FINAL for it; after the candidate is cleared it reports `pinned` again.
- **C2 exact observation:** through `getExactRecoveryHold` end to end, a hold released with `prerequisite_closure` (and its `final_closure_id`) observes as released; `prerequisite_closure` without a closure id, or any other evidence on disposition `archive`, is refused at the SQL and the Go check. The closure-id relation is server-enforced (CHECK, composite foreign key and guard); the wire carries no closure id, so the agent validator only checks evidence-by-kind.
- **C2 #2969 grantor:** a `captureAndUpload` call from `recoverRetainedClaim` with the permission mints only under grants 1 or 4; a record that would need grant 2 or 3 does not mint there.
- **C1b one-item pin:** a fresh `pending` row reaches `pinned` in one item; with too little remaining deadline the item stops before the marker (row `pending`, nothing sent).
- **C1b absence confirmation (O11):** first absent listing denies FINAL and keeps the pointer; a positive listing in between clears the candidate; a confirming absent listing in a later pass ends `lost` and clears the pointer; the same pass never confirms.
- **C1b concurrency:** with the slot held, a kick exits without forge work and the row is processed by a later pass; no two closure items do forge work concurrently in one process.
- **C2 reconciliation:** a closure-released hold reconciles as accepted through `reconcile.go`'s unchanged echo, and `validExactClosedProof` accepts it as `settled`; any other evidence for kind `archive` stays rejected.
- **C2 release:** LiveDB FINAL success with `prerequisite_closure`; every existing guard branch green; rollout rows below.

## Risks

### Riskiest assumptions

1. Value rests on the enabling need; zero verified standalone A1 cases (user decided "Proceed").
2. E2 now supports the compatible default-tip anchor on the tested GitHub credential/configuration. Differing-workflow historical tips were refused. Other forge/policy configurations and the workflow-change race remain explicit refusal paths; do not infer a general direct-pin strategy.
3. T1 holds in production; only the native-Git tests back it, no server check.

### Readers, writers and external surfaces

- Writers: `Service.stream` (S0), the lane and pass, FINAL (`final_closure_id`). Never the worker.
- Readers: worker status, FINAL, reconcile, owner DTO and CLI, removal handlers.
- External: one ref per qualifying capture (producer captures included), repository-visible.

### Rollout orders and mixed versions

| API | Worker | Behaviour |
|---|---|---|
| old | any | no rows; thin FINAL refused; identical to today |
| C1a | old | ready-query/guard invariant enforced, no closure row or pin machinery |
| C1b | old | no worker advertises the capability, so no rows; feature not advertised; custody as today |
| C1b | C2 | capability advertised, so S0 pins (knob permitting); **feature not advertised, so the worker never mints**; pins are cleaned at expiry; custody as today |
| C2 | old | no capability, no rows; identical to today |
| C2 | C2 | full behaviour |
| C2 → older (rollback) | C2 | the older API refuses thin FINALs (409); a journaled closure FINAL is `final_retry_refused` and replays after roll-forward (stuck-FINAL worst case meanwhile; **local source retained**); no cleanup on the older binary; Down refuses while rows or evidence exist |
| C2 | C2 → older (rollback) | no new MAC field; the older worker does not replay a thin FINAL (its local thin check refuses); custody retained |
| knob off | any | no new rows; no back-fill when turned on; the pass still runs S1 to S7 on existing rows; FINAL for an unlisted forge refused |

### Cost and lifetime (corrected)

- `UZI_RECOVERY_MAX_CAPTURES_PER_OWNER` (default 256) counts only captures not `expired` or `discarded` (`service.go` ~124-132). It bounds **active** captures and therefore pins with intended retention. It does **not** bound closure rows of expired captures, rows stuck in `create_unknown`, or `pinned` rows past the cleanup ceiling (leaked pins); those accumulate and each keeps its run, repository and connection removal blocked.
- **Accepted (O10, owners 2026-10-25):** unresolved rows and leaked pins accumulate. They get bounded, fair processing (pass budgets, one-slot concurrency), keep their removal blockers, and E6 reports their counts. **No total inventory bound is claimed**; no live-pin admission bound is added. Terminal rows are audit tombstones (like capture metadata).
- Forge: one ref per qualifying capture for its intended lifetime (`UZI_RECOVERY_READY_RETENTION`, default 168 hours), longer for leaked pins.

### Operator policy for accumulation (O10 clarification)

O10 accepts an unbounded total inventory, not automatic eviction. E6 reports the
counts and growth of unresolved rows, leaked pins and removal blockers; it is not
an admission bound or an overflow-prevention mechanism. No row, pin, live pointer
or tombstone ages out merely because the active-capture limit, an attempt ceiling,
a storage threshold or an operational deadline is reached. Retention is the
state-machine retention above: unknown rows and cleanup-exhausted pins may remain
indefinitely; terminal rows remain audit tombstones. A failure to persist S0 must
fail/roll back the upload transaction rather than publish availability without its
required row. It grants no release or deletion authority.

Before C1b dispatch, its plan must commit to carrying this procedure into
`docs/run-recovery.md` and `docs/configuration.md`, with tests for its existing
state-machine paths. This defines the operational response without a new quota,
garbage collector, override endpoint or permission:

1. **Contain incoming work.** The deployment operator can turn off the existing
   prerequisite-forge knob through normal configuration management to stop new S0
   admissions for the affected kind. This is not an instantaneous cancellation of
   in-flight transactions. Existing rows continue through the always-registered
   bounded pass; do not disable their reconciliation/cleanup or increase its limits
   to hide backlog. Ordinary capture quotas/failures keep their existing behavior;
   no separate closure-overflow eviction or promise of bounded storage is implied.
2. **Preserve and diagnose.** Record the exact capture/hold/row identity, checksum,
   frozen binding, pin_ref/pin_tip, state, outstanding attempts and fresh listings;
   preserve recoverable archive bytes before an owner chooses early expiry/discard.
   Restore failed connectivity or credentials only under the existing frozen
   repository identity. A changed/foreign binding is not permission to rebind,
   resend a create or reset attempts. Owner-visible/audit diagnostics must identify
   why removal remains blocked, without exposing credentials or raw forge errors.
3. **Use only the existing S7 cleanup route.** Only after the capture is confirmed
   expired or discarded may S7 clean a `pinned` row. It rechecks capture state and
   frozen binding, lists the exact ref, CAS-deletes only at its recorded expected
   tip, and records absence before clearing the pointer. A moved/foreign ref is
   left untouched; an ambiguous delete retains evidence for later listing. Once
   automatic deletes reach their ceiling, keep the pin and live pointer, perform
   only the scheduled listings, and escalate unresolved cleanup to a separate
   maintainer decision. This runbook grants no manual forge-delete procedure,
   owner/custodian-authorization shortcut, attempt reset or database override.
   Repository-write access is not product authorization to dispose of a capture.
   Pins of still-available archives are not capacity-reclamation targets, even if
   the hold settled by another route. Indefinite retention/removal blocking remains
   possible under O10; no eventual-cleanup promise is made.
4. **Unknown is not operator disposal authority.** For `create_unknown`, first let
   S4 observe the expected tip and durably commit `pinned` before any deletion.
   An absent ref, expired capture, elapsed time, owner pressure or storage pressure
   does not resolve that ambiguity. An absent `create_unknown` row can remain an
   indefinite removal blocker under O10. Do not manually clear its live pointer,
   delete its row, recreate/move its ref, or retry its create. If no existing proof
   path resolves it, retain it and escalate a separate maintainer contract decision;
   this runbook promises no override or eventual resolution.

An external history/privacy purge is outside this capacity-recovery procedure
and remains subject to the accepted external-writer residual. This PRD grants no
such out-of-band action. It may destroy archive restorability and does not itself
erase server evidence or authorize clearing blockers. C1b must document this distinction and verify that
knob-off processing, confirmed-pin cleanup, wrong-tip refusal and unresolved-row
retention obey these same boundaries. C1a is unaffected by this clarification.

### Accepted races and residuals

- Late create: one send ever; listing-only after.
- Ancestry: immutable once proven; rewrites before S1 refuse.
- External deletion or rewrite after release: accepted residual (Goal 2, ADR-1296 precedent).
- FINAL versus cleanup: one-way `available` and the shared lock order.

### Security and data

- Pins can keep pre-purge history reachable under `refs/uzi-*`, independently of the external deletion/rewrite restore residual. The run-recovery guide records this limitation without a purge recipe: O10/S7 automatic cleanup follows expiry/discard, expected-tip CAS and confirmed absence; unknown creates and cleanup exhaustion retain blockers.
- Pins point only at default-branch commits and publish no run content. No new worker credential, endpoint or authority; the four guardrail layers unchanged.

### Out of scope

Changing held ordering; deleting pins when another route settles the hold (pins stay while their archives remain available); clearing removal blockers by timeout or label; a server-side header or bundle parser; #2809; #2544; physical disposal; #2969's boundary; producer FINAL (#3444 amendment); #3105; server bundle verification; unrelated guard tightening (deferred).

## Measurements and evidence gaps

| Id | Gap | Status | Needed by |
|---|---|---|---|
| E1 | Population | done (zero verified standalone A1); user: Proceed | done |
| E2 | Maintainer-only same-scope/same-role probe; actual production go-git helpers plus native supplement; current-tip and old-tip workflow-tree cases. | **GitHub FINDING, 2026-10-27:** compatible anchor succeeded; differing-workflow old tip refused; same-tree old tips succeeded; zero malfunctions. Other forges/configurations unverified. | evidence fold review and final architecture disposition before filing |
| E4 | Per-capture prerequisites, guarded manifest-bound thin population, 2026-10-24 snapshot | 386 captures / 257 holds; min 1, median 1, nearest-rank p90 6, p99 16, max 16; 204 expired and 182 available (available p90 8). Source SHA256 349a1c59e4b1eac90c26b63178a73651c1bb2a79fa58ec7725d91708854747b5. Independent local recomputation; no fresh DB query. Not a latency measurement. | reported before C1b dispatch; retain max-64 tests |
| E5 | Owner restore time on this repository | [measured locally](#c3-local-e5-measurement-2026-11-01-3728) | C3 final validation/landing remains open |
| E6 | Two weeks hosted after the roll: releases, refusals per reason, live pins, unresolved and cleanup-exhausted rows | open | post-acceptance |

## Child issues (after architecture approval only)

Every child PR uses `Refs #2506`, closes only its child, adds a changelog fragment, and writes nothing under `.github/workflows/`. Each gets a spec review and the priority::high architecture review before dispatch.

**Validation recipe (2026-10-28 owner amendment).** C1a and C1b are API-only; any agent/** or web behavior edit there is a scope stop. The old API-only aggregate-gate exception wording is superseded for later children that actually touch agent/**: **full agent suite: exact-head PR CI**, with focused in-run checks and the repository timeout flag. This does not authorize adding agent work to C1a/C1b.
- API in the worker: no `task gate:api`; serial focused format/build/vet and `go test -race -count=1`, plus sqlc regeneration/drift when queries change.
- API on the exact PR head, outside the worker (CI or lander): every `gate:api` constituent green, `gate:repo` green, and named store-it DB tests PASS with zero SKIP. Every DB acceptance test function ends in `LiveDB` and is in store/handler/forgesvc/schedsvc/workersvc/recovery/poller, the existing sweep. Missing names/packages or skipped tests do not count as evidence; no workflow edit to expand it. File-backed broker/native-Git tests run in ordinary test:api.
- Web in the worker: `task gate:web` on the frozen tree when changed by an authorized later child.
- Always: `task check-docs:web` after PRD/ADR/docs changes; `task docs:sync` after docs/*.md; E2E literal audit plus `task test:e2e-lib` and `task test:forge-fake`; name compose phases left unrun. Report affected-job timing separately from queue/cache/reruns; no new real-time sleeps.

**E2E harness rule.** `git grep -F` the new literals (reasons, `prerequisite_closure`, `refs/uzi-prereq/`) across `e2e/phases/`, `e2e/lib.sh`, `e2e/lib.test.sh` and `e2e/forge-fake/`; add a forge-fake route for `refs/uzi-prereq/*` create, list and CAS delete if any compose phase reaches a thin guarded capture; name phases left unrun.

### M0 (maintainer-owned)

E1 done; user decided Proceed. E2 has now run for the tested GitHub production credential class: a compatible default-tip anchor succeeded, while direct differing-workflow history was refused. Review this evidence fold and the final anchor strategy before architecture approval or filing. E4 has been computed from the existing export (Measurements table); publish/review its record before C1b dispatch. Counts are not latency evidence. Any additional forge/configuration stays unlisted until its own probe supports the strategy; return to design if neither anchor nor another reviewed strategy works there.

### C1a: "Prevent recovery captures from re-entering available outside upload"

- **Depends on:** final exact-draft architecture/buddy approval. No pin, capability or feature rollout.
- **Scope:** MarkCaptureReady uploading predicate; separate capture-guard migration with SQLSTATE 55000 before the silent identity branch; generated query, writer audit and fixture repairs; changelog and this invariant's documentation slice. API-only.
- **Evidence:** independently discriminating query and raw/parameterized-trigger tests, positive stamped-upload and same-state controls, previous guard branches, migration rollback/reapply, exact-head LiveDB execution. Baseline query/raw update regressions fail at behavior, not missing symbols. Public Upload's already-refused retries remain characterizations.

#### C1a execution and evidence (2026-10-28, child #3590)

M1 is committed at `991df8b3a26027ffd162e45cadb3cd8da347d1b6`, based on `2085c751`; reviewer, auditor and DBA reported no blocking findings on that head. This record covers child #3590 only; the parent remains active and non-closing (`Refs #2506`). The revision-18 design and its historical anchors above remain design history. C1a adds no pins, closure lifecycle, release, cleanup, capability or TTL authority. C2 release, #2969 integration and producer enablement remain separate.

- [x] C1a ready-query and UPDATE entry guard implemented, with generated query mirror and three fixture repairs.
- [x] C1a focused baseline red/candidate green, independent query/trigger mutations, and migration round trip observed as below.
- [x] C1a reviewer, auditor and DBA review of `991df8b3` completed without blocking findings.
- [ ] C1a final PR head: exact-head CI/lander acceptance (M3), including full API constituents, repo gate and named store-it DB tests PASS with zero SKIP.

The four unchecked parent goals under “Goal and acceptance” were reviewed and remain unchecked: C1a demonstrates no prerequisite pin, custody release, native-Git restore or complete real-freeze-path authority coverage.

**Writer and fixture audit.** The production state-SQL inventory is an exhaustive enumeration of tracked current statements at `991df8b3`, including generated mirrors; it is not an exhaustive consumer or arbitrary-SQL reachability proof. Starting evidence: `.uzi/scratch/m1-state-writers.txt`, `.uzi/scratch/m1-ready-callers.txt` and `.uzi/scratch/m1-raw-writers.txt` (run-local artifacts, not durable documentation dependencies). Paths below are relative to `api/internal/`; each row states its audit verdict.

| Site | Verdict and boundary |
|---|---|
| `store/queries/recovery.sql`: ReserveCaptureExact | unchanged — `recovery.Service.Reserve` inserts preparing; conflict changes timestamp only. |
| `store/queries/job_files.sql:197`: StampCaptureReservation | unchanged — `recovery.Service.admit` enters uploading from preparing/uploading/needs_action. |
| `store/queries/recovery.sql`: MarkCaptureReady | updated — bound manifest AND uploading predicate; unmatched query returns exact `pgx.ErrNoRows`. |
| `recovery/service.go:463`: MarkCaptureReady caller | already accurate — sole production call found; stream checks uploading at 416 and answers an existing available receipt at 413. |
| `store/queries/recovery.sql`: MarkCaptureState | already accurate — parameterized state can request available; the trigger guards it. No production caller found. |
| `recovery/recovery.go:67`, `workersvc/service.go:614` | already accurate — MarkCaptureState interface declarations, not calls. |
| `store/queries/recovery.sql`: MarkCaptureFailed | unchanged — `recovery.Service.recordFailure` writes needs_action from preparing/uploading/needs_action. |
| `store/queries/recovery.sql`: ExpireStalledUploads | unchanged — `workersvc/sweep.go` writes needs_action. |
| `store/queries/recovery.sql`: DiscardCaptureForOwner | unchanged — `recovery.Service.Discard` writes discarded. |
| `store/queries/recovery.sql`: DiscardNonReadyCapturesForHold | already accurate — writes discarded; Service.DiscardHold/merged-predecessor path. |
| `store/queries/recovery.sql`: ExpireReadyCaptures | already accurate — writes expired; sweep path. |
| `store/queries/recovery.sql`: FenceUnacceptedInventoryCapture | unchanged — `recovery.Service.Reconcile` writes expired. |
| `store/queries/recovery.sql`: ExpireRedundantCapture | unchanged — `workersvc.archiveRedundancyProver.expire` writes expired. |
| `store/queries/recovery.sql`: BindCaptureManifest | already accurate — preserves state. |
| `store/queries/recovery.sql`: ProtectFinalInventoryCapture | already accurate — preserves state. |
| `store/queries/recovery.sql`: RecordArchiveRedundancyRefusal | already accurate — preserves state. |
| `store/migrations/00305_recovery_inventory.sql`: recovery_inventory_worker_delete | already accurate — preserves capture state. |
| `store/migrations/00322_recovery_archive_redundancy.sql`: Down reason rewrite | correct as history — preserves state; rollback rewrites the redundancy reason. |
| `store/recovery.sql.go`, `store/job_files.sql.go` | updated / already accurate respectively — generated mirrors audited with their query sources, not additional independent writers. |
| `store/recovery_livedb_test.go`: Ready fixture | updated — StampCaptureReservation before ready, asserts one affected row. |
| `store/recovery_inventory_livedb_test.go`: raw available fixture | updated — StampCaptureReservation before raw UPDATE, asserts one affected row. |
| `recovery/stored_files_rework_livedb_test.go`: TestMarkCaptureFailedNeverOverwritesADiscardLiveDB | updated — stamps only the available arm; discarded/expired retry setups and preparing/uploading/needs_action loop unchanged. |
| `recovery/reconcile_livedb_test.go:54`, `recovery/inventory_livedb_test.go:86` | already accurate — e.upload invokes real Upload before raw available-to-available metadata updates; no repair needed. |
| `recovery/merged_predecessor_livedb_test.go`, `poller/merged_predecessor_livedb_test.go` | already accurate — preparing/uploading/needs_action loops unchanged. |
| Raw fixture SQL in the tracked inventory | already accurate except the repairs above — direct INSERT available fixtures retain their authority because this is a BEFORE UPDATE trigger. JobFiles available fixtures are not recovery capture states. |

Reader surfaces inspected as starting points were summary/list/download/reconcile/final inventory/redundancy/sweep; this is not an exhaustive consumer claim. No wire contract or state vocabulary changed, so C1a required no CLI, web or API-v1 edit. Raw SQL, parameterized queries, interface declarations and generated mirrors were distinguished in the audit.

**Observed M1 checks (not rerun for M2).** Focused Go runs used `-buildvcs=false -race -count=1 -v -p 1 -timeout 20m` in store/recovery/workersvc as selected; the DBA run used `-race -count=1 -v -p 1`. Logs below are run-local under `.uzi/scratch/`.

| Evidence | Observed result and boundary |
|---|---|
| Format and whitespace, `gate-log.CGayyP` | `task fmt-check:api` and `git diff --check HEAD`, EXIT 0. Lead static comparison: Up adds the new branch only; Down exactly restores the preceding 00322 Up function body, PASS. |
| Baseline `2085c751`, `gate-log.8Jx111` | Existing selected guards/fixtures PASS, EXIT 0, zero SKIP. Wall 511s including cold compilation; package test times 7.049/6.411/8.283s. |
| Baseline plus only the new baseline-compatible `store/recovery_available_guard_livedb_test.go`, `gate-log.YOt8hi` | TestMarkCaptureReadyRequiresUploadingLiveDB and TestRecoveryAvailableTransitionGuardLiveDB expected FAIL, EXIT 1, wall 48s. Expired/discarded ready returned nil instead of ErrNoRows; raw/parameterized/combined-identity writes returned nil instead of 55000. Behavior failures, not build or missing-migration failures. |
| Candidate `991df8b3`, `gate-log.GCT4sm` | Serial build:api, vet:api, check:sqlc-drift (scratch TMPDIR), then focused Go tests PASS, EXIT 0, zero SKIP; wall 473s including dependency build/cache effects. New query/guard tests took 5.81/5.38s. |
| Candidate selected families in that run | New two tests plus TestRecoveryStoreLifecycleLiveDB, TestRecoveryInventoryClaimLiveDB, TestMarkCaptureFailedNeverOverwritesADiscardLiveDB, TestRecoveryReconcileDecisionLiveDB, TestRecoveryInventoryIdentityLiveDB, TestRecoveryInventoryExpiryHandoffLiveDB and TestArchiveRedundancy.*LiveDB. Not full gate:api or the full API unit suite. |
| DBA exact-head export, `gate-log.33JtOt` | TestRecoveryAvailableGuardMigrationRoundTripLiveDB (7.01s) and TestRecoveryArchiveRedundancyMigrationRoundTripLiveDB (7.05s) PASS, EXIT 0, zero SKIP, wall 91s. New round trip 373→374→373→374 exercises guard phases and restores exact captured pg_get_functiondef. |
| Lead query-only mutation, `gate-log.jwGNLn` | Removed only uploading query predicate and regenerated sqlc, retaining trigger: expected FAIL, EXIT 1, wall 87s. SQLSTATE 55000 for expired/discarded fails the exact ErrNoRows assertion. |
| Lead trigger-only mutation, `gate-log.fJkf2Y` | Removed only new Up branch, retaining query: expected FAIL, EXIT 1, wall 77s. Raw, parameterized and combined identity/state assertions receive nil instead of 55000. Mutation wrapper `.uzi/scratch/m1-mutations.sh` EXIT 0; both mutants killed in exports, primary unchanged. |
| Public Upload characterization, `gate-log.7kGUHV` | TestRecoveryDiscardedCaptureStaysDiscardedLiveDB PASS on baseline and candidate, EXIT 0, zero SKIP; 4.69s fresh schema vs 0.15s already migrated. Existing refusal, not a new baseline red or latency improvement. |
| Review | Reviewer/auditor/DBA clean on `991df8b3`; auditor reported secrets/SAST pass. govulncheck@v1.9.0 unavailable offline (task EXIT 201), so vulnerability checking remains pending, not waived. |

**Timing boundary.** The new query, trigger and migration tests measured 5.81s + 5.38s + 7.01s of test execution. Command wall times include different selections, compilation/cache conditions and reruns; database cache/schema conditions also differ. Comparable full-job delta, queue effects and CI critical-path impact are **UNMEASURED**, requiring external final-head evidence. Production function-replacement/catalog-lock contention, representative latency and mixed-version operation were not measured. No new sleeps were added.

**M2 supporting checks.** `task check:migration-numbering`, `task check:migration-additive`, `task test:e2e-lib` and `task test:forge-fake` passed serially (EXIT 0; `gate-log.jQTEBB`, 30s wall). The tracked literal audit across `e2e/phases/`, `e2e/lib.sh`, `e2e/lib.test.sh` and `e2e/forge-fake/` found no stale capture-state expectation requiring an edit: capture checks consume existing upload receipts, the library's capture SQL is read-only, and phase 72 seeds hold-only fixtures. No state vocabulary or public upload retry outcome changed. Compose E2E was not run, including relevant phases 52 (`52-api-outage-outbox.sh`) and 72 (`72-custody-lifecycle.sh`).

**Remaining gates.** M3 owner/lander must run every gate:api constituent (including the unavailable vulnerability check), gate:repo and the named store-it acceptance tests on the final PR head, require PASS/zero SKIP, and record the exact head and timing evidence before merging. These checks remain obligations, not accepted or waived work. C1b stays BLOCKED until C1a is merged AND exact-head CI/lander passes; C1a's local evidence does not complete any broader parent checkbox.

### C1b: "Pin the prerequisite closure of a thin guarded capture with a bounded lifetime"

- **Depends on:** **BLOCKED** until C1a is merged AND its exact-head CI/lander validation passes; M0 E2/final architecture disposition and reviewed E4 record remain required.
- **Scope:** closure table/FKs/live pointer/identity+transition triggers/guarded Down; S0; broker prepare/send/list/CAS contracts; all S1-S4/S5/S7 lifecycle, status/audit/removal/budgets; capability registration vocabulary/order only; knob; ADR/configuration/restore docs. API-only, no repeated guard rebuild. No protocolFeatures or worker advertisement and no release change.
- **Evidence:** native-Git restore/omission tests and all upload, lock, state, failure, scope-refusal, timeout/fairness tests below. Thin FINAL remains refused, pinned status alone grants no release. Existing agent shape tests are cited; no new agent fixture.
- **Regression:** baseline public List/Delete reject prerequisite refs; no-pin native control fails on a missing prerequisite. New prepare/send symbols use named behavior mutations, not baseline build failures.

#### C1b current execution (2026-10-29, child #3591)

C1b landed in [PR #3655](https://github.com/vtmocanu/uzi/pull/3655) on
2026-10-31 as `b6fab6b7`, migration 00387. Before merge, the final head
`edb29486` passed the required hosted checks. The full seven-package store-it
sweep recorded 3,696 named PASS and zero SKIP, including all 62 required
prerequisite names and both witness rollback tests. The combined lost-reply
proof completed in 48m33s under the separately reviewed 60-minute cap.
These results complete the hosted portion of M5, not M6 operator observation,
C2 release, C3 owner acceptance or the parent goals. C1a's evidence is preserved.
The detailed local records below describe the worker's earlier handoff state;
they do not assert that hosted checks were available at that earlier time.

- [x] C1b M1: immutable closure persistence, restrictive live pointer, state/reservation/absence fences, guarded Down and inherited C1a regressions implemented and checked locally.
- [x] C1b M2: bounded broker prepare/send/list/expected-tip CAS, shared one-send authority and native positive/negative restore fixtures checked locally.
- [x] C1b M3: bounded lane, shutdown, removal guards, frozen-binding/unknown/cleanup/audit lifecycle and real-driver/LiveDB/native integration checked locally.
- [x] C1b M4: atomic S0, original authenticated registration snapshot, no-backfill receipts, postcommit kick, knob normalization/admission and optional status mapping checked locally.
- [x] C1b M5 local evidence: documentation/mirrors, tracked E2E audit, E2E library/fake tests, focused build/vet/race checks, SQLC/migration/static/security checks and committed repair reviews recorded below.
- [x] C1b M5 overall acceptance: local deliverables and final exact-head hosted checks/timing collected at the #3655 landing boundary.
- [x] C1b final exact-head hosted acceptance: required API/repo checks and the full store-it sweep passed, all 62 required names PASS with zero SKIP; advisory timing recorded below. This is not a post-deployment runtime result.
- [ ] C2 thin FINAL release, feature/worker advertisement, permission/replay/reconciliation and real freeze-path authority acceptance. Sub-note: M1-M4 are implemented and checked locally (see the C2 execution record below); this line stays open for exact-head hosted acceptance, the full store-it sweep and the recorded deviations.
- [ ] C3 owner/web/CLI pin visibility and E5 restore-time acceptance.
- [ ] E6 two weeks of hosted post-roll counts and growth, including unresolved/exhausted pins and removal blockers (reporting only, not a closure gate).
- [ ] Parent release/acceptance: all child acceptance and remaining end-to-end/rollout criteria below.

**Hosted timing at landing.** The advisory report for CI run 41182433894
compared execution steps with a five-sample main median: store-it 2,794s
(+1,051s), combined lost-reply 2,913s (+593s). Whole-run wall time was 7,463s
against 3,134s; runner activation was 2,444s. Queue/activation, compilation
and cache effects prevent treating those differences as isolated test-cost
measurements. No causal performance improvement or production pin latency
is claimed. The agent-gap rebalance advisory was false. Post-merge CI,
deployment and M6 observation are not established by this pre-merge record.

**Direct evidence, not hosted CI.** Lead-owned run-local records were read in
`.uzi/scratch/m1/evidence.md`, `m2-*evidence.md`, `m3-*evidence.md` and
`m4-evidence.md` (including the unsuffixed M2 report). They record scoped
reviews, focused race/count=1 tests and isolated mutations/restored greens.
Early subunit reports saying M3/M4 were not started are historical; the final
M3 S7 report at `ca16afa1` and M4 report at `db652da5` state their local
completion. Scratch paths identify evidence for this run, not durable reader
setup dependencies.

| Slice | Observed local evidence and boundary |
|---|---|
| M1 at `415042a5` | `gate-log.j77NI8` EXIT 0: migration checks/sqlc drift/focused build and serial race selections; 16 new LiveDB plus six inherited C1a tests PASS, zero SKIP. Reservation/trigger/query/Down mutations have intended behavioral reds and restored greens. |
| M2 at `1533da72` | `gate-log.ufpAD4` EXIT 0: full broker package race/count=1, focused build/vet; 14 new top-level TestPrereq PASS, zero new skips. Two inherited bridge skips are not new proof. Copied prepared authority and raw-reason findings repaired/reviewed. |
| M3 native at `4ce682f3` | Public sweep → production broker → actual HTTP Git backend; fake persistence/native ancestry. TestPrerequisiteNativeSweepRestoresAfterDefaultRewrite restores exact source SHA/tree/file after fetching pin D, a strict descendant of P. The no-pin control fails naming P; rewrite-before-prepare refuses without send; omitted P2 fails native verify/import despite fetched P1. No server bundle validation or released-custody claim. |
| M3 integration at `4c394a52` / final S7 at `ca16afa1` | Repaired full 13-family LiveDB selection PASS/zero SKIP, focused build/vet; final four-family S7/affected-outcome run `gate-log.91wQdb` EXIT 0, zero SKIP/race. TestPrerequisitePinnedCleanupForeignTipUntouchedLiveDB and TestPrerequisiteAmbiguousCleanupRequiresLaterAbsenceLiveDB prove foreign untouched and later absence without resend. Earlier fixture/script errors are not counted as behavioral reds. |
| M4 at `db652da5` | `gate-log.8pCKA2` EXIT 0: 71 top-level PASS, 13 LiveDB PASS, zero SKIP/race; focused six-package build/vet. TestRecoveryPrerequisiteClosureUploadIntegrityLiveDB and TestRecoveryPrerequisiteClosurePinnedThinFinalLiveDB preserve immutable upload/continued thin FINAL refusal. TestPrerequisiteUploadKicksBrokerOnceLiveDB covers ACK/lost-ACK/request-cancelled committed kicks. A survived early cancellation mutant was repaired and killed; it was not counted as a red. |

Additional exact test names and limits are recorded in
[ADR-3591](../adr/3591-prerequisite-closure.md#local-evidence-and-remaining-acceptance).
Local command times include varying compile/cache/fixture costs; they are not
comparable before/after hosted job or critical-path measurements. No new live
forge probe was run. E2 remains the previously recorded tested GitHub case.
E4's 2026-10-24 snapshot, 386/257 population, nearest-rank method, counts and
source SHA256 in the Measurements table remain unchanged; no fresh DB query
or latency inference is added.

**Current authority and operations.** The API admits S0 only for listed
`github` at `https://github.com` with original-worker registered capability;
known GitLab/Forgejo knob syntax is not support. Feature and current worker
advertisement remain absent; pinned thin FINAL is still refused. T1–T5 is
conditional trusted-worker preservation, not server bundle validation.
Unknown is listing-only indefinitely without resend/rebind/reset. S7 needs
expired/discarded, frozen binding, expected-tip CAS and confirmed absence;
eight exhausted deletes retain the live blocker and list with at most
one-hour backoff. O10 accepts unbounded unresolved/leaked pins/tombstones;
active cap 256 does not bound them. Knob-off contains incoming S0 only;
connectivity repair preserves binding/diagnostics and cannot reset a ceiling.
No manual delete, DB override, owner shortcut or purge authorization is
introduced. See the [operator runbook](../docs/run-recovery.md#operating-an-o10-backlog).

**Every-checkbox disposition.** The four original parent goals remain open:
(1) custody release is C2; (2) local restore-after-pin is demonstrated but
released-custody end-to-end acceptance is not; (3) C1b uses API forge history
and has local negatives, but parent release-evidence acceptance awaits C2;
(4) real freeze-path/mint authority coverage belongs to C2, with producer
FINAL still excluded. The three checked C1a implementation/proof/review items
remain checked on their recorded evidence. Its one hosted exact-head item
remains open; local C1b regression runs do not substitute for it. The four
new C1b M1–M4 boxes and the M5 local-evidence box above are ticked on direct
local evidence only. M5 overall acceptance, C1b hosted gates/timing, C2,
C3/E5, E6 and parent release remain unchecked.
No other Markdown checkbox existed in the pre-edit PRD.

#### C1b M5 local validation and remaining hosted acceptance

Documentation and embeds were committed at `5bfd4668`; fixture/static-check
repairs were committed and reviewed through `60a94c89`. The parent remains
active. The E4 row and C1a execution block are unchanged.

- `task docs:sync` and `task check-docs:web` passed (`gate-log.WCneXt`);
  the embedded-docs race test also passed. Long-page warnings are not errors.
- A tracked audit covered 63 E2E files and 71 old/new literals. No shipped
  deterministic phase forces the oversized guarded thin fallback; the fake's
  existing native Git CGI bridge already carries advertisement/receive-pack.
  No E2E source change was required. `task test:e2e-lib` passed (including
  352/352 library cases); `task test:forge-fake` passed 10/10 with zero skips.
  Compose phases were not run: notably 06, 42, 45, 50, 51, 52, 70, 71, 72
  and 73; optional live SDK output remains unmeasured.
- Format, SQLC drift, migration numbering/additivity, token literals,
  changelog entry, no-binary-text and spec numbering checks passed at
  `5bfd4668`. Secrets and required Semgrep scans detected their canaries and
  reported zero tracked/tree findings; repeated scans at `e10e2323` were also
  clean. Dead-code analysis passed at `5bfd4668`; hosted current-head coverage
  of that constituent remains required. No in-worker aggregate API gate ran.
- Initial lint found 19 fixture diagnostics. The uncapped rerun exposed one
  additional unused suppression. Test-only repairs preserved assertions and
  were reviewed; the pinned repository wrapper's uncapped ratcheted lint at
  `60a94c89` reported zero issues (`gate-log.4CGELT`).
- The broader race run at `e1c2a260` (`gate-log.RcaS5I`, EXIT 1, 925 seconds)
  retained two failures: an instance-wide expiry count on the reused database
  and inherited capture deletion blocked by new fixture tombstones. Passing
  package results in that failed run are not an aggregate PASS. New upload
  fixtures now use disposable databases; no production guard or old C1a/SFEnv
  fixture was weakened. Fresh comparison reproduces the FK failure before
  isolation and passes afterward (`gate-log.7o8FXB` / `gate-log.BVc7E4`);
  the inherited store lifecycle passes at both revisions on fresh databases.
- Fresh focused tests at `e10e2323` passed 25 top-level tests, including 22
  LiveDB tests with zero skips/race warnings, then focused ten-package build
  and vet (`gate-log.bXaRYs`). Three fixture-isolation regressions fail at
  actual database-identity assertions when only the isolation calls are removed,
  and pass on restore (`gate-log.H82sDL` / `gate-log.YWC2Ty`). The later
  `60a94c89` change removes only an unused comment. All these commits received
  their scoped read-only reviews; no final implementation commit is unreviewed.

At that worker handoff these were local observations, not hosted acceptance. The last
pipeline read returned an upstream forge error; no CI verdict is inferred.
Every API constituent, repo gate, and named store-it PASS/zero-SKIP result at
the published head remains required. `govulncheck` was unavailable on PATH;
no dependency-scan PASS is claimed. Added per-fixture migrations have an
unmeasured CI cost. Affected-job and critical-path before/after timing,
separating queue/cache/reruns, remains outstanding; local fixture/compile
seconds are not a substitute. M6 maintainer/operator acceptance, E6, C2/C3
and parent completion remained separate and unperformed at that handoff.
The landing record above supplies the subsequently collected hosted results;
it does not claim M6, E5 or parent completion. E6 remains reporting only.

#### C1b cross-check repair (2026-10-30)

Four reported lifecycle defects were repaired within the existing executor and
upload transaction. The post-marker binding read has its own 500 ms context;
a 4.5-second pre-marker reserve covers commit, binding, send/read-back and
outcome persistence. Read errors still prevent sending. Cleanup selection
excludes available captures and retains the locked expiry/discard recheck.
Listing admission requires 3.5 seconds, including the caller's remaining
deadline, before consuming an item or changing audit/retry scheduling.
Upload status is decorated inside the ready transaction and returned after
commit without another request-context database read.

The first serial race run (`gate-log.Wbpmhv`, EXIT 1) passed the functional
regressions but failed cleanup query-plan assertions: the filtered selector
used the capture-ID index instead of the formerly required cleanup-due index.
The repaired test accepts either indexed access path, retaining full eligible
row/order/page checks and the executed Limit bound. The affected rerun
(`gate-log.2Vw4ap`, EXIT 0) passed both custom/generic plan tests and the
marker/direct-listing/sweep-admission regressions, with zero SKIP.

Lead-owned disposable exports independently restored each faulty behavior:
reuse the spent marker context; remove the capture-state selector predicate;
remove listing admission; restore the post-commit status read. Each failed at
its intended behavioral assertion (four mutation logs under crosscheck-mutation-*).
The script `crosscheck-mutations.py` completed EXIT 0 (`gate-log.1iQMi6`);
the fresh unmutated export then passed cleanup selection, marker context,
late direct/sweep admission and upload cancellation/ACK tests with race,
count=1, serial packages and zero SKIP (`crosscheck-restored-green.log`).
No mutant was copied into the candidate; the export was removed.

The initial focused static check passed format/docs validation but lint reported
G702 on the existing native fixture Git launcher (gate-log.34zKTd, EXIT 1).
Its literal executable/subcommands and runtime-owned test arguments use no
shell; the launcher now documents that narrow false-positive suppression.

At this pre-publication repair handoff, hosted acceptance and timing were still
outstanding and the latest forge-reader attempt returned an upstream error.
The landing record above now supplies M5's collected hosted results and timing.
M6 operator observation is not performed here, and parent acceptance remains
unchecked; the earlier local repairs alone did not establish hosted success.

### C2: "Release a guarded hold on a pinned prerequisite closure"

- **Depends on:** C1b.
- **Scope:** release migration; FINAL predicate and queries; S6 listing and limiter; typed refusals; protocol feature advertisement; worker closure-mint permission, origin check, mint/replay split, `validExactClosedProof`, capability advertisement; reconcile echo test; ADR-1296 D5, ADR-2417, ADR-1197 amendments; `specs/human.md`.
- **Evidence:** FINAL success with `prerequisite_closure` (including the force-push-after-pin case end to end); permission, origin, replay, reconciliation and real-freeze-path tests above; every existing guard branch green; the rollout rows.
- **Regression failing on current code:** archive FINAL for a thin capture with a `pinned` closure returns `ErrManifestConflict` and must release; a retained-claim thin record with status `pinned` mints no FINAL and must mint, ACK and clean up.

#### C2 execution record (2026-11-01, #3727)

C2 is implemented on branch `agent/issue-3727`; it has **not landed**. Migration
00393 (draft 00389, renumbered at landing).

What landed, by milestone (commit subjects):

- **M1 schema** (`api/internal/store/migrations/00393_prerequisite_closure_release.sql`):
  `1a751723d` feat(recovery): schema for closure-backed thin archive release (C2 M1);
  `c6e748b87` fix(recovery): closure release admits NULL-cause parks; isolate guard-layer tests (C2 M1);
  `aa469eb71` test(recovery): isolate closure disposition CHECK and release binding predicates (C2 M1).
  `final_closure_id`, a composite foreign key to the exact closure/capture/hold,
  release evidence `prerequisite_closure`, tie CHECKs and a guard branch; Down refuses
  (SQLSTATE 55000) while closure-backed rows exist. The DBA measured the combined `ALTER`
  on synthetic data at about 0.4 s per 500k holds (about 0.8 s per million) under
  `ACCESS EXCLUSIVE`, plus up to 5 s of `lock_timeout` waiting; live row counts and a
  cold cache are unmeasured.
- **M2 API release** (`api/internal/recovery/final_closure.go`):
  `ca4544ef8` feat(recovery): release a thin archive FINAL on a pinned prerequisite closure (C2 M2);
  `5e2d4b44a` fix(recovery): bound closure FINAL locks server-side and test the real listing seam (C2 M2);
  `25402622f` test(recovery): pin the lock_not_available mapping of closure FINAL errors (C2 M2).
  Unlocked precheck, per-worker limiter admission, fenced listing reservation, one listing
  outside any transaction, locked release (500 ms lock timeout per step, 6 s budget);
  refusals `manifest_conflict`, `prerequisite_pin_missing`, `prerequisite_pin_foreign`
  (409) and `prerequisite_pin_unavailable` (503).
- **M3 worker mint** (`agent/src/recovery.ts`):
  `aaf13f0ce` feat(worker): mint a thin archive FINAL only under a transient closure grant (C2 M3);
  `7d5ccbb0d` fix(worker): grant a same-pass closure mint only on an attested held fallback (C2 M3);
  `8e133c665` fix(worker): attest a held fallback only on a bounded server refusal (C2 M3);
  `5005d0fe2` fix(worker): an uncomputable CI diff or a bare 400 release never attests (C2 M3).
- **M4 advertisement and proof**:
  `97a180f83` feat(recovery): advertise prerequisite closure release and prove it end to end (C2 M4);
  `4c3459295` test(recovery): pin closure advertisement on heartbeat and isolate the port (C2 M4).
  The feature is advertised only while `UZI_PREREQUISITE_CLOSURE_FORGES` contains `github`
  and the port and limiter are wired; the combined proof `TestPrerequisiteClosureReleaseE2E`
  runs in the opt-in leg of `e2e/run-store-it.sh`.
- **M5 documentation** (`c990de73c` docs(recovery): document closure-backed thin archive release (C2 M5)): ADR-1296, ADR-1197, ADR-2417, ADR-2506 and ADR-3591 amendments,
  `docs/run-recovery.md`, `docs/configuration.md`, `docs/cli.md` and the embedded mirrors,
  `specs/human.md`, a changelog fragment, and the E2E literal audit (no matches in
  `e2e/phases/`, `e2e/lib.sh`, `e2e/lib.test.sh` or `e2e/forge-fake/`; `task test:e2e-lib`
  6 of 6 and `task test:forge-fake` 11 pass, 0 fail).

Local verification actually run: the store LiveDB C2 tests; the recovery, workersvc and
handler focused LiveDB tests; `lint:api` with 0 issues; `deadcode:api` clean; sqlc drift
clean; `check:agent` pass; the touched agent test files; the combined opt-in proof leg
PASS with no skips.

- [x] C2 M1 schema, guard branch, tie CHECKs and guarded Down implemented and checked locally.
- [x] C2 M2 API release, limiter, refusals and replay implemented and checked locally.
- [x] C2 M3 worker mint grants, replay and attestation implemented and checked locally.
- [x] C2 M4 advertisement predicate and the combined opt-in proof implemented; proof leg PASS locally with no skips.
- [x] C2 M5 documentation, ADR amendments, spec line, changelog fragment and E2E literal audit written and checked locally.
- [ ] C2 exact-head hosted acceptance (full agent suite, full store-it sweep, native git tests): not verified in this run.

**Not verified in this run.** The full agent suite is left to exact-head PR CI. The full
store-it sweep exceeded the local 60-minute cap and is left to exact-head CI. Native git
tests skip locally for lack of `git-http-backend` and run in CI. No live GitHub.com
observation was made; that is a parent/operator follow-up. Compose e2e phases were not run.
M6 operator observation, E5, E6 and C3 are not claimed.

**Worker-handoff deviations at `8fea87a` (historical).**

- The positive real-observer C2 case lives in `api/internal/handler/prerequisite_final_wired_livedb_test.go`.
- No `pg_locks` lock-order test and no explicit shutdown test were added.
- The exact-observation closure `EXISTS` branch is unreachable by tests, because the schema forbids forged rows.
- The specified downgrade/roll-forward journal test and the thin replacement-fence tests were not added.

**Landing-review follow-up, 2026-11-01.** The missing frozen acceptance is being
completed locally, with hosted acceptance above still unchecked:

- `TestRecoveryClosureFinalClosureLastLockOrderLiveDB` observes the blocked FINAL
  through `pg_locks` and checks actual earlier row-lock ownership, in one server
  round trip before the real 500 ms lock deadline.
- The existing `TestRecoveryClosureFinalCancelledRequestLeavesNothingOpenLiveDB`
  already covers paused and blocked-listing cancellation.
  `TestHandlerPrerequisiteFinalShutdownDrainsListingLiveDB` adds an authenticated
  HTTP shutdown proof through the real port and broker. Graceful HTTP shutdown
  drains handlers; the bounded listing ends without custody release, a detached
  listing or an open transaction. It does not assume that `http.Server.Shutdown`
  immediately cancels active request contexts.
  `TestHandlerPrerequisiteFinalShutdownAtomicCommitLiveDB` additionally starts
  drain mid-request with a valid pin: this deterministic positive control requires
  HTTP 200, `released=true` and the complete persisted release/capture protection.
  A load/deadline 503 fails explicitly rather than satisfying the positive case.
  The stalled control still requires no custody/capture effect. After HTTP shutdown and the
  prerequisite drain return, no handler, listing or transaction remains open.
  The agreed graceful-drain interpretation is recorded in the Decision Log below;
  no global server shutdown behavior is changed here.
- The worker journal test uses an explicit test-only
  `UZI_RECOVERY_COMPAT_READER` module path for the actual untouched pre-C2 reader
  at `cb6ebd8f`. That reader authenticated and retained the new signed thin journal,
  rejected a MAC mutation, and roll-forward recovered the unchanged request's ACK.
  Ordinary CI exercises feature rollback and persistence; it does not silently
  claim that the historical module ran when the input is absent.
- Thin replacement controls now exercise the committed API fence, the unchanged
  committed receipt, and worker handling of retained, malformed and timed-out
  reconciliation. Replacement still needs a fresh ordinary mint grant.
- Review found the inherited capture-health gate still prevented saved thin FINALs
  from sending when status was expired, discarded or unreadable. Three controls
  failed on that behavior. The local correction makes status diagnostic for an
  already-saved thin archive request only; header, source/coverage, inactivity and
  new-mint health checks remain. These controls passed after the correction.
  A stricter call-order control also requires zero pre-send capture-status reads:
  a slow diagnostic must not spend the replay budget. Only validated replaceable
  reconciliation fetches the additional status proof after its committed fence.
- Retained-claim grant 4 now denies an unresolved held create before either
  grantor is considered. The helper's previous grant under a closure-only feature
  snapshot failed a direct control. The actual caller's execution gate remains
  unchanged, and no #2969 exemption or observed production release is claimed.

These are local review proofs, not a full-suite or hosted PASS, an owner/operator
observation, E5 or parent completion. Exact-head CI and the lead's independent
delta review remain required. Added fixture/test time and critical-path impact
must be recorded at landing; local fixture times do not establish hosted cost.

Mixed-version, source-history, limiter and #2884 coordination boundaries are recorded in
the ADR amendments dated 2026-11-01.

### C3: "Show the prerequisite closure to the owner and document restore"

- **Depends on:** C1b (states). Parallel with C2 only with disjoint file ownership.
- **Scope and evidence:** owner DTO, fixtures, web and CLI (`cellText`), `pin_lost` and `pin_cleanup_exhausted` visibility, restore commands, `docs/cli.md`; E5 measured.
- **Local completion (#3728, 2026-11-01):** M1–M3 are implemented/reviewed at `60c71323`; M4 guide/product checks passed on `287b04b9` after reviewed documentation at `10ff078c` and a reviewed fixture repair. Exact-head API/repo/full store-it landing remains pending. Broad C3 acceptance and parent release remain open. Owner projection is advisory persisted history with eight states and independent absence/cleanup flags, not a fresh probe or release/cleanup authority. The [owner restore guide](../docs/run-recovery.md#restoring-with-a-confirmed-pin) uses existing export, a UUID-derived ref and full-tip comparison before native verify/import. E6 remains an unperformed post-roll observation.

#### C3 local E5 measurement (2026-11-01, #3728)

The explicit `TestRecoveryRestoreWorkflowLiveDB/E5FullHistory` invocation
passed with zero SKIP/FAIL. This is a local owner-workflow measurement, not
hosted GitHub latency, a C2 FINAL exercise or parent release acceptance.
The ordinary `SmallNative` controls also passed; they do not substitute for E5.
The parent and broad C3 landing acceptance remain open; completed local integration checks are recorded below.

Input was a `clone --no-local` of this checkout, selected at public revision
`a07f813708ced206cd7b8c3f9d09e259b2fff64b`: non-shallow, connectivity/full
object checks passed, 2,405 reachable commits. The captured source added
`recovery-generated.txt` (26 bytes, SHA-256
`5a96308b340168c758f0ad6da84524bc09893aa989bc88bccaa22c78dc86b43b`),
giving 2,406 commits at source
`eeffd4ccba0fc168d69068bbe0c71a0fb545a8a5`, tree
`b4ae331a571f7a7c46bba83bb1c442c552b76246`.

| Observed component | Actual result |
|---|---|
| Exported thin bundle | 507 bytes; SHA-256 `809a9636aaeb1fae98af11eb7663a46c1c5d54da3da5f48767b236af9981638a` |
| Pin fetch | 97,835,961 bytes of native upload-pack POST response bodies, including Git protocol framing, excluding advertisements, HTTP headers and transport overhead |
| Explicit-capture CLI export | 95.414 ms |
| Exact pin fetch to the same ref namespace | 13.741274 s |
| Recorded-tip comparison, bundle verify and recovered-source import | 79.169 ms |
| Owner clock: export start through import completion | 13.915937 s; includes exported-byte comparison, excludes final source/tree/file assertions |
| Fixture setup through native pinning | 47.765507 s |
| Owned remote rewrite/prune clock | 2.224715 s |
| Clean consumer clone | 45.426 ms |
| Entire case clock | 64.732682 s, including setup, prune, clone, source/tree/file assertions and negative controls |
| Containing test / Go package test time | 159.08 s / 160.441 s; includes full-input verification and CLI driver compilation outside the case/owner clocks |

The lane used real C1b sweep, broker prepare/send/readback, PostgreSQL
constraints/transitions, encrypted archive upload/storage, authenticated owner
metadata/download and the existing CLI export/SafeWriteVerifiedFile path.
Both owned bare remotes had their default branch rewritten, reflogs expired and
`gc --prune=now` run. The independently unpinned remote no longer contained
the frozen prerequisite. The clean consumer initially failed bundle verification
naming that prerequisite, then fetched its capture-specific pin, compared the
recorded tip, verified/imported the advertised recovered-source ref and matched
source SHA, tree and file content. No alternates or local-clone hardlinks supplied
the consumer's history.

Missing metadata/ref/tip, foreign capture/ref/tip, unavailable history and expired
download controls passed with the exported archive retained. Expiry required
HTTP 409 with `not_available`, not an arbitrary error. Read/export/import left
capture, closure and hold rows byte-identical before the fixture's explicit expiry
control; the native pin and captured source ref were unchanged.
Existing S0 no-backfill/admission controls passed separately, including the
unreviewed-host refusal. No production host policy was relaxed.

Substitutions were fixture identities/configuration, generated source content,
loopback forge discovery/URL policy, native ancestry in place of the hosted forge
API, and a test-only Main/Env/config driver in place of the installed CLI entry.
Upload first used the production GitHub.com S0 policy; only the fixture's pending
binding then moved to the owned loopback remote. The driver kept HOME unchanged,
used an owned Store/SkillHome and explicit nonsecret environment allowlist, and
proved inherited UZI settings/credential sentinels did not reach the API.

Environment: native Linux x86_64/Go linux-amd64 runner, Git 2.54.0, Go 1.27.3,
CLI version emitted by the test build `dev`, owned PostgreSQL 17 container.
The Alpine Git package lacked its CGI helper: the initial small control failed
for that setup reason. The matching git-daemon 2.54.0-r0 package was fetched,
signature-verified and extracted only under scratch (archive SHA-256
`b376522eadb4df8dc5274687ceabe07456924d415cfcc5683c4b198e97a6c855`).
Its native git-http-backend was added to the test PATH; no global install or
Git exec-path change occurred. A cancelled intermediate compile and an unfinished
component-static invocation are not passing checks.

Reproduce from `api/`, after preparing a marker-guarded owned test database and
the independently cloned/full-checked frozen source, with native git-http-backend
available on PATH:

```sh
env GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null \
  UZI_TEST_DATABASE_URL="<owned-test-database-url>" \
  UZI_TEST_RECOVERY_RESTORE_FULL_HISTORY_SOURCE="<absolute-owned-full-source>" \
  go test -buildvcs=false -race -count=1 -v -p 1 \
  -run '^TestRecoveryRestoreWorkflowLiveDB$/^E5FullHistory$' ./internal/handler
```

Require the exact E5FullHistory RUN/PASS, positive archive/fetch byte counts and
the LiveDB skip guard with zero SKIP/FAIL; an ordinary small-only pass is
insufficient. The full-input mode rejects missing/wrong/shallow/incomplete input
rather than falling back. Routine CI omits E5 registration when full mode is
not requested.

Evidence was collected from the candidate fixture sources with SHA-256
`6094c38936c0487a8c4f877d7db905005dee6f089f4ef6dcc18f85fad6629b23`
(native helper),
`0956a91715973e0e5ee25632219509688f430a3f6fd5175ef58697ba1c861466`
(workflow), and
`665eff881f1d5bfdfcbe94cf25f7c026314672cc589c0ef01a24086621a9b683`
(CLI driver). Local logs were read directly: gate-log.MnbjAQ (small),
gate-log.v1jaiY (E5), gate-log.k3D0Gr (S0). Scratch paths are run-local,
not durable artifacts; the sanitized results above are the committed record.

These are the latest successful local passes after setup/static-check repairs,
with warmed module and filesystem caches. An earlier full-history pass also
succeeded; these are individual observations, not a performance average. Cache hit rates, hosted queue/cache/rerun costs and CI
critical-path impact were not measured. The small containing test took 68.28 s
(case 6.40 s), exposing substantial driver-preparation cost outside the restore
clock. Full inherited store-it and required exact-head API/repo CI remain landing
obligations. External pin deletion/history rewrite remains the accepted residual;
this measurement grants no custody-release or cleanup authority.

#### C3 M4 integration record (2026-11-01, #3728)

The owner guide, CLI documentation, visibility ADR and human requirement now
describe persisted pin observations separately from fresh verification and
custody authority. The restore sequence selects an explicit available capture,
compares the fetched pin's full recorded tip before bundle verification/import,
then checks source/tree/content. Source/documentation review passed at
`10ff078c`; a test-only UUID fixture repair passed scoped review at
`287b04b9`. Both embedded mirrors match. The local checks below are complete;
the broader parent C3 checkbox remains open for landing, without claiming
hosted acceptance.

- [x] C3 local M1: owner projection/contracts and authenticated read-only LiveDB controls, with complete history/order/identity and constant Summary SQL round trips.
- [x] C3 local M2: safe existing CLI/web diagnostics, shared meanings, preserved actions and dawn/ember keyboard/layout controls.
- [x] C3 local M3: actual full-history E5, native prune/no-pin/tip controls and retained exports; explicit E5 and routine controls PASS with zero SKIP.
- [x] C3 local M4 guide/product checks: synced documentation, links, shipping fragment/spec note, full web gate, focused Go controls and E2E library/fake gates.
- [ ] C3 landing: exact-head required API/repo checks and full inherited store-it, including hosted timing; not observed or waived here.

Full `task gate:web` passed on frozen `287b04b9`: 386 unit files /
8,425 tests (502.82 s Vitest duration), then 10 real Chromium files / 35 tests
(19.63 s). Dependency, lint, dead-code, documentation/changelog/style and type
slots passed first. The four recovery browser cases cover dawn/ember at
375/1100 widths with keyboard/focus checks. Focused Go checks used
`-buildvcs=false -race -count=1` and clean Git config: apitypes, uzidocs
(including byte-equal embeds), owner-summary/status recovery tests and affected
CLI recovery/export/landing/docs/printed-instruction tests all passed.
The M1/M3 DB/native proofs remain applicable: their implementation sources are
unchanged by this documentation integration. The final restored SmallNative
after both M3 mutations passed at the committed fixture hashes, zero SKIP
(`gate-log.j6TRGU`, package 96.358 s).

The first web invocation stopped before tests because a relative TMPDIR was
invalid for the root dependency check. Narrow candidate/base checks reproduced
the setup failure and passed with an absolute owned scratch path. A scratch
wrapper runs the existing native Chromium with a short relative TMPDIR to avoid
Unix socket path limits; it changes no product code or test assertions. A
subsequent full run exposed one stale RunView fixture expecting an unsafe
`r1/cap-ok` copy command (8,424 other tests passed). Equivalent narrow base
PASS / candidate FAIL confirmed that cause. The minimal canonical-UUID fixture
repair kept the production guard and assertions intact; its three focused
cases, lint/typecheck and all allocated scoped reviews passed before the final
full gate rerun. These failed/setup attempts are not counted as passing gates.

Local log identifiers read directly: `msJBoD` (docs/E2E), `qTvnCp`
(focused Go), `dlqAaG` (full web), `GGeF6t` (repair), `RvY9VX` /
`uaRUbE` (narrow candidate/base). Logs are run-local scratch artifacts;
the sanitized results here are the durable record. Local M4 scanner callbacks
returned no verdict; no final-head scanner pass is claimed. Source audits
found no security defect. Required repository/API scanners and vulnerability
checks remain part of exact-head landing acceptance.

A targeted tracked E2E audit searched the changed retired output phrases and
all recovery/prerequisite identifiers in phases, library tests and the forge
fake. No retired phrase survived. Existing readers select capture id/state/
checksum/byte size or tolerate absent fields; the optional projection changes
no fake forge operation or route. No harness edit was needed. Compose phases
52 (outage/outbox) and 72 (custody), and general CLI smoke phase 18, are unrun;
library gates passed all six reported tallies (7/403/24/6/5/6), and the fake-forge gate passed 12 tests with zero skips. No compose acceptance is claimed.

Equivalent focused component commands were run on frozen
`a07f813708ced206cd7b8c3f9d09e259b2fff64b` and `60c71323` exports,
baseline first, with copied provisioned dependencies, Node 26.12.1, Vitest
4.1.12 and TZ=UTC:

| RecoveryArchives test file | Baseline | Candidate |
|---|---:|---:|
| Cases, all passing | 28 | 57 |
| File test time | 944 ms | 2,268 ms |
| Vitest total | 4.95 s | 8.35 s |
| Command wall | 7.553 s | 10.765 s |

Command: `npx --no-install vitest run src/components/RecoveryArchives.test.tsx`
from each export's web directory. Candidate includes 29 added cases; these are
single sequential samples, not an identical-case comparison or a CI benchmark.
Filesystem/dependency-copy warming and cache hit rates were uncontrolled.
Hosted critical path, queue, cache and rerun costs remain unmeasured. The new
M3 routine native fixture had no pre-existing case comparator; its driver
preparation cost is reported in the E5 record. Worker command callbacks can
assign fresh Go cache directories, so those preparation timings do not predict
hosted CI cache behavior. Full inherited store-it timing and required exact-head
API/repo checks remain landing obligations.

#### C3 advisory repair evidence (2026-11-01, #3728)

The follow-up confirmed and repaired D1–D5, C1 and C2. An omitted/empty
prerequisite list means self-contained only with valid bound byte-manifest
facts; otherwise it is unknown. CLI pending cleanup now requires a pinned
row on an expired/discarded capture. Human recovery details follow the hold
table with hold attribution, obey quiet mode, and warn on legacy unlinked
captures. Relevant mock runs/history use canonical UUIDs while the executable
command guards stay unchanged. Documentation/skill copy distinguishes byte
export from prerequisite verification, and JSON pin facts from the smaller
human capture-choice table. The shared table adds twelve expired/discarded
non-pinned cases, exercised by Go, CLI and web tests.

Owner-summary LiveDB seeds now use the generated invocation, cleanup,
reservation, absence and lost/deleted transition methods. A closure-bearing
capture is discarded with the owner store query and asserted in the complete
history. The test records the identity-schema limitation: valid sibling rows
prove isolation, but immutable constraints prevent constructing a mismatched
closure for the same capture without bypassing guards. Timing substitutions
remain fixture-only: make scheduling due, and age only the initial absence
observation through a narrowly checked DBTX clock adapter. That adapter changes
only the initial timestamp expression of RecordPrerequisiteAbsence; production
state/generation/pass predicates and the real schema guard remain intact.
An initial attempt to age an already recorded absence was rejected by the
immutable-candidate guard; the final fixture does not overwrite it.

Observed regressions failed on the unrepaired CLI and web for manifest,
cleanup, layout and demo command defects. The final focused Go selection
passed with race/count flags: apitypes (1.567 s), embedded docs (1.582 s),
recovery summary/status (1.550 s), CLI recovery/prerequisite/export/landing/
docs instructions (6.047 s), plus focused vet (`gate-log.viLgAF`). The expanded
authenticated owner-history LiveDB control passed with zero SKIP (11.238 s
package; `gate-log.iC7DKY`), including the unchanged two-query/read-only checks.
The tester callback was cancelled after this completed log; its unfinished
broad vet callback is not counted as passing. The lead read the result directly.

C3's coverage-uncertainty note does not invalidate the retained local evidence:
the lead reopened the explicit E5 PASS/measurements (`gate-log.v1jaiY`),
blackbox baseline missing-projection failure (`gate-log.TJ6kWR`), CLI/web
red–green logs and full-web PASS (`gate-log.dlqAaG`). All three native E5/driver
fixture hashes still match the measured record above; no duplicate E5 run is
claimed. C4 remains an exact-head hosted landing obligation: API/repo checks,
full inherited store-it, native backend availability and package timeout must
be observed before merge. Local output cannot promise those hosted results.

#### D5 / C1 documentation audit (2026-11-01, #3728)

This is a current-working-tree documentation audit, not new runtime, hosted,
restore or release evidence. No acceptance checkbox or earlier measurement is
changed. Tracked searches started with available/recover/export/prerequisite
claims in the CLI reference, recovery guide and bundled skill, then inspected
the matching availability/thin-archive contexts in adjacent live documentation.
This is not an exhaustive audit of historical PRDs, ADRs, changelog entries or
other skills.

The M2 product-copy audit covered `web/src/pages/RunView.tsx` (memory,
exhaustion and terminal recovery sections),
`web/src/components/RecoveryHoldsSurface.tsx`,
`web/src/components/RecoveryArchives.tsx`, `web/src/lib/recovery.ts`,
`api/cmd/uzi/run_export.go`, `api/cmd/uzi/run_recovery.go` and their directly
affected tests/mocks. The follow-up inspected `api/cmd/uzi/run_get.go` and
`api/cmd/uzi/run_wait.go` routing/help: terminal get uses the shared
summary/landing renderer; wait help points to inspection/export without an
import-success guarantee. This is an enumerated starting point for current
recovery claims, not a claim that every historical recovery mention was edited.

The documentation file set actually inspected is:
`docs/cli.md`, `docs/run-recovery.md`,
`api/internal/uzicli/skill/SKILL.md`, `docs/admin-health.md`,
`docs/configuration.md`, `docs/issue-3031-delivery.md`,
`docs/recovery-storage-diagnosis.md`, `docs/run-recovery-wait.md`,
`ARCHITECTURE.md`, this parent PRD (current execution/C3 integration context),
and the corresponding `api/internal/uzidocs/embed/cli.md` and
`api/internal/uzidocs/embed/run-recovery.md` mirrors. Adjacent-file inspection
covers the matching claim and surrounding paragraph, not every contract in
each file.

Source boundaries inspected: `api/cmd/uzi/run_export.go`
(`recoverySummaryLines`, `renderCaptureChoice`, selection, manifest precheck
and result), `api/cmd/uzi/run_prerequisite.go` (capture/pin detail rendering),
`api/cmd/uzi/run_recovery.go` (`recoveryHoldCapture`,
`holdsWithCaptures`, per-hold human details), `api/cmd/uzi/run_get.go`
(human summary versus JSON run object),
`api/internal/uzicli/client_recovery.go` (owner archive endpoint),
`api/internal/uzicli/safefile.go` (byte count/checksum before destination link),
`api/internal/recovery/service.go` (upload stream checks, manifest binding and
two-query Summary), and `api/internal/recovery/download.go` (owner/state/
manifest gate). `api/cmd/uzi/run_prerequisite_test.go` was inspected for its
JSON inspect-field assertions, not rerun as part of this documentation audit.
No new claim that all available metadata has a usable bound manifest follows:
export retains its own manifest precheck and download can fail. The server
checks uploaded length/checksum; it does not independently verify Git
dependencies. A thin export still requires destination-clone verification.

Validation for this audit: `task docs:sync` exited 0; `task check-docs:web`
exited 0 (80 docs validated, 49 line-budget warnings); both changed embed mirrors
compare byte-for-byte with their sources. `git diff --check` passed. No Go/web
runtime test, hosted check, new E5 measurement or acceptance decision is claimed.

Per-site verdicts below concern that availability/importability distinction,
not revalidation of unrelated retention, quota, release or cleanup mechanisms.
Mirror verdicts apply to the same named sections after byte-for-byte sync.

| Site | Verdict |
|---|---|
| docs/cli.md — recovery summary | updated: every retained capture, with state and recorded pin details; human view. |
| docs/cli.md — export-choice refusal | updated: pin facts preserved in JSON; human table has id/state/source/size/created only. |
| docs/cli.md — successful export | updated: byte verification, bound-manifest/download gate, no prerequisite guarantee. |
| docs/cli.md — owner ARCHIVE column | already accurate: presence of an available capture, not an import proof. |
| docs/cli.md — coverage paragraph | already accurate: an export can omit latest local work. |
| docs/cli.md — export hint | already accurate: availability offers export independently of attention. |
| docs/cli.md — retained-recovery exception | already accurate: available/in-flight captures do not settle owner custody. |
| docs/cli.md — latest versus older capture | already accurate: download and latest coverage are independent. |
| docs/cli.md — detailed recovery JSON | updated: checksum, ordered prerequisites, expiry, attempted head and optional pin snapshot; absent optional fields remain possible. |
| docs/cli.md — terminal-record rejection | already accurate: export requires its own available capture. |
| docs/cli.md — prerequisite-pin lifecycle | updated: recovery/get show persisted pin state; held refs remain separate. |
| docs/cli.md — completed held alternative | already accurate: thin archive describes fallback eligibility, not standalone importability. |
| docs/cli.md — worker-recovery exhaustion | already accurate: recorded availability is historical, not current export/coverage proof. |
| docs/cli.md — retained exhaustion coverage | already accurate: earlier available archive does not prove latest coverage. |
| docs/cli.md — adoption-failure export advice | already accurate: export before deciding does not promise verified import. |
| docs/run-recovery.md — opening recovery promise | updated: bytes can outlive the worker; thin prerequisites remain separate. |
| docs/run-recovery.md — opening guarded release | already accurate: confirmed pins are a distinct release condition. |
| docs/run-recovery.md — owner-held memory custody | already accurate: availability does not automatically release custody. |
| docs/run-recovery.md — held-publication-conflict export | already accurate: export can refuse. |
| docs/run-recovery.md — Available state | updated: archive bytes ready; prerequisite verification before import. |
| docs/run-recovery.md — needs-landing with no archive | already accurate: lack of diff/archive is unresolved, not losslessness. |
| docs/run-recovery.md — local thin proof | already accurate: local verified dependencies do not establish remote durability. |
| docs/run-recovery.md — source-only export limit | already accurate: manifest-bound archive required; no source download API. |
| docs/run-recovery.md — C2 lifecycle introduction | already accurate: pinning/release distinguished from byte download. |
| docs/run-recovery.md — confirmed-pin step 1 | updated: recovery JSON supplies inspect fields; missing linking requires owner archive metadata, not run JSON. |
| docs/run-recovery.md — thin FINAL worker grant | already accurate: release proof is separate from owner import verification. |
| docs/run-recovery.md — pin retention after alternate settlement | already accurate: available archive retains its pin lifecycle. |
| docs/run-recovery.md — ready owner quota | already accurate: accounts for archive bytes, not prerequisite storage. |
| docs/run-recovery.md — instance quota | already accurate: accounts for archive bytes/reservations. |
| docs/run-recovery.md — thin guarded disposition | already accurate: downloadable does not imply final release. |
| docs/run-recovery.md — selected archive after receipt | already accurate: download remains available under its retention policy. |
| docs/run-recovery.md — forge-park history caveat | already accurate: availability alone proves no complete history. |
| docs/run-recovery.md — prerequisite-release rollout | already accurate: API release guard precedes guarded-thin producer rollout. |
| docs/run-recovery.md — Option D archive survival | already accurate: bytes, recorded prerequisites and expiry survive hold settlement. |
| docs/run-recovery.md — same-issue witness archive survival | already accurate: preserves archive metadata/bytes, not external commit availability. |
| docs/run-recovery.md — terminal rejection older export | already accurate: earlier export may omit latest work. |
| docs/run-recovery.md — terminal rejection source-only state | already accurate: readiness is distinct from custody attention. |
| docs/run-recovery.md — terminal rejection export guidance | updated: a pin alone is not an archive; bound manifest/download and destination verification remain required. |
| api/internal/uzicli/skill/SKILL.md — ARCHIVE column | already accurate: reports capture availability. |
| api/internal/uzicli/skill/SKILL.md — recovery/export contract | updated: inspect fields and human/JSON pin visibility; bytes versus Git dependencies. |
| api/internal/uzicli/skill/SKILL.md — needs-landing export/diff | updated: download first, then verify prerequisites before import; preserved diff remains separate. |
| api/internal/uzicli/skill/SKILL.md — adoption-failure export advice | already accurate: export precedes owner decision without guaranteeing success. |
| api/internal/uzicli/skill/SKILL.md — source-only/available distinction | updated: pin alone is not an archive; thin exports require dependency verification. |
| api/internal/uzicli/skill/SKILL.md — held-ref expiry | already accurate: expiring a held ref does not delete an available archive. |
| docs/admin-health.md — per-owner recovery bytes | already accurate: byte utilization, not restore readiness. |
| docs/admin-health.md — marker clearance | already accurate: clearance does not prove archive availability. |
| docs/configuration.md — prerequisite forge knob | already accurate: pin admission/release has separate conditions. |
| docs/configuration.md — held-publication knob | already accurate: thin fallback and replay are qualified. |
| docs/configuration.md — proposal limiter | already accurate: thin FINAL listing budget, not import verification. |
| docs/issue-3031-delivery.md — conditional thin-release invariant | correct as history: explicitly qualified by the following C2 note. |
| docs/issue-3031-delivery.md — C2 note | already accurate: ordinary-origin closure release grants no producer authority. |
| docs/recovery-storage-diagnosis.md — hold discard | already accurate: preserves available archive bytes under archive policy. |
| docs/run-recovery-wait.md — adoption-failure export advice | already accurate: earlier archive can omit latest work. |
| docs/run-recovery-wait.md — local decoded-history verification | already accurate: local proof grants no custody release. |
| docs/run-recovery-wait.md — source-only export limit | already accurate: requires manifest-bound available archive. |
| docs/run-recovery-wait.md — exhaustion evidence | already accurate: recorded availability leads to a hold, not restore proof. |
| ARCHITECTURE.md — frozen-inventory archive identity | already accurate: digest identity is not server Git-ancestry proof. |
| ARCHITECTURE.md — same-issue discard TTL | already accurate: archive retention gives no source/PVC guarantee. |
| ARCHITECTURE.md — held-publication thin fallback | already accurate: fallback qualification, not import proof. |
| ARCHITECTURE.md — C1b lifecycle boundary | correct as history/component boundary: the later C2 release section is separate. |
| ARCHITECTURE.md — worker-exhaustion capture evidence | already accurate: historical evidence affects custody disposition. |
| api/internal/uzidocs/embed/cli.md — recovery/export sections above | updated: synchronized CLI-reference mirror. |
| api/internal/uzidocs/embed/run-recovery.md — availability/restore sections above | updated: synchronized recovery-guide mirror. |

#### Partial M3 evidence for owner completion decision (2026-11-02)

**Partial record, not completion.** Candidate `f584e2cb2b17277bc4de5b1aa3c76e76ac23ae31`;
initial `365d3f396175d038149b8970718aaed1d1f2e4dc`;
baseline `6d6e86bb2f9e275772bc3241fddb0c2a51a9cfd5`.
Reviewer and tester reviewed source tests on the original and repair ranges.
The primary tree was clean. M2's mandatory aggregate gate remains unmet;
neither M2 nor M3 is completed. No formal owner completion decision is recorded.
The parent remains active/open and goals 1/2 remain unchecked.
This evidence changes no production behavior, workflow or cap.

**Execution identity.** Native linux/amd64: Go 1.27.3, Git 2.54.0,
Node 26.12.1, tsx 4.23.15 and PostgreSQL 17.11.
The following sanitized artifact names and SHA256 values identify executed
tools and loaded artifacts; they do not assert an exact image/package pin for Git.
The matching Alpine `git-daemon 2.54.0-r0` backend was extracted from a
signature-verified package into owned scratch, without a global installation.

| Artifact | SHA256 |
|---|---|
| Go executable (launcher and effective toolchain) | `159888b397dac32306b4355c52d59ae5bc9b81515d387efa30f04657fc3a4cc7` |
| Go compiler, linux_amd64 | `ce30144a6c83f099ac67d9cce259949aaf562814c07250fb7a52dfcc78554d27` |
| Go linker, linux_amd64 | `54f1351cb72bd285269ccae8db5c992719c6e0394cd5d1fa52032cdc8241f158` |
| Git executable; git-upload-pack; git-receive-pack | `081e55dbb116bd5fce9b5e16df5c1bf843748f03cd5c2ddf0627a8eccda839c1` |
| git-remote-http; git-remote-https | `cabbcc77c46ca117c5f17ca2fbcc1708f95173ae74c4d1e5e2e690bb2747c0b6` |
| Native git-http-backend | `e217374667663b49146dc1eadeb5ba0cfc6cc8e916127e308365470683e75ce1` |
| Node executable | `c6fa3c4a915da799b59b908e74a34f8f3307b7ec3251b211bc88d452558b3351` |
| tsx package.json | `ab5f14c4251638844ade3905d3cfc52fc1c833644694d655a05f6d0ca746b83c` |
| Loaded tsx dist/loader.mjs | `17e2a92c5f1f70065b01c65d5ee9ac260839d4fc0ea78fdbb4db7b1155ccd32f` |
| agent/package-lock.json | `df076786f5bf20814f6b86e147ef25d71f5a6ec5357f50964e8216c28ce97a8f` |

Separately, `api/go.mod` declares floor `go 1.26.4` and
`toolchain go1.27.3`; the agent lock resolves tsx 4.23.15 from `^4.19.2`.
The run guarded its owned DB and absolute source environment without printing
DSNs/tokens; Git global/system configs were `/dev/null`,
`GOFLAGS=-p=1`, and `TMPDIR` was owned scratch. From `api`:

```sh
: "${UZI_TEST_DATABASE_URL:?owned database required}"
: "${UZI_TEST_RECOVERY_RESTORE_FULL_HISTORY_SOURCE:?absolute frozen source required}"
go test -buildvcs=false -race -count=1 -p 1 -timeout 20m -v ./internal/handler -run '^TestRecoveryRestoreWorkflowLiveDB$/^E5(Released|BeforePinRefusal)FullHistory$'
```

**Full-history proof.** EXIT 0; 11 RUN/PASS, zero SKIP/FAIL.
`TestRecoveryRestoreWorkflowLiveDB/E5ReleasedFullHistory` passed in 69.39s;
`TestRecoveryRestoreWorkflowLiveDB/E5BeforePinRefusalFullHistory` passed in 49.99s.
Positive: native listings 3 → 4, one create and one receive POST; real
WorkerClient FINAL released with `prerequisite_closure` for the exact capture
before prune; restore used that same capture and left its snapshot unchanged.
Negative: persisted `prerequisite_not_on_default`, no create/receive,
FINAL 409, open hold, no receipt, retained data and NULL closure binding.
Connection base remained `https://github.com`; project URL used loopback with an exact
two-origin policy.

Owned input froze `a07f813708ced206cd7b8c3f9d09e259b2fff64b`:
nonshallow, fsck/connectivity checked, 2,405 input commits and 2,406 with the
generated source. Copies used no-local/no-hardlinks and no alternates.
Original source/checkout stayed read-only; pruning targeted owned remotes.
The unpinned control actually lost the prerequisite. Restored source SHA was
`f56fe6e815675ba342bf7bb1c426fbca9cd15b1c`, tree
`b4ae331a571f7a7c46bba83bb1c442c552b76246`.

Positive archive: 509 bytes, exported byte-equal; generated content: 26 bytes.
SHA256: archive `a015c8b4be9500bf77ba94ce56907e0e108c1465c3c70d5a1cac8010d6c184c1`;
generated `5a96308b340168c758f0ad6da84524bc09893aa989bc88bccaa22c78dc86b43b`.
Upload-pack HTTP response body: 114,899,184 bytes (not pack size or full network).
Timings: setup 49.07015694s; prune 2.909362587s; clone 52.283312ms;
export 102.273204ms; fetch 15.229879906s; verify/import 53.543072ms;
owner total 15.385790858s; case wall 69.214092852s.
CLI build 84.970082166s was outside the owner clock; command wall 8m55.23s
included cold Go caches. Owner time runs from export start through import,
including byte comparison and excluding final source/tree/file assertions.
Negative setup including prune was 49.236757924s, prune 779.756346ms,
FINAL 703.285825ms and case wall 49.940044093s; owner restore is inapplicable.

**Omission controls.** Isolated candidate exports passed npm typecheck and Go
race compilation for both omissions; no primary mutations or synthetic ACK/receipt.
Omitting pin fetch kept valid declarations/full-tip checking: expected EXIT 1,
owner `pin_tip` mismatch, 55.03s; restored EXIT 0, positive 75.62s,
10 RUN/PASS, zero SKIP/FAIL.
Omitting FINAL send, ACK observations and replay retained the actual inspect-state
assertion: expected EXIT 1, actual=open/expected=released, 51.95s;
restored EXIT 0, positive 68.73s, 10 RUN/PASS, zero SKIP/FAIL.

**Aggregate gate remains failed.** Normal candidate seven-package
`./e2e/run-store-it.sh` failed after 21m12.80s: cumulative store timeout
20m (1200.106s), active `FrozenRecoveryPhasesHealthy`/readopt at 0s elapsed;
remaining packages did not run. Narrow healthy-test runs passed on candidate
7.97s and baseline 6.88s.
Equivalent baseline aggregate failed after 46m16.74s: store PASS 1174.749s,
then handler timeout 20m (1200.586s), active
`SemanticCompletionMaximumRenewal` at 23s.
The candidate store timeout was not reproduced on baseline; causality remains
unresolved. No successful full seven-package sweep was observed; caps stayed unchanged.

**Retained controls and static checks.** Latest controls EXIT 0:
171 RUN/PASS, zero SKIP/FAIL; candidate handler 232.488s,
workersvc 1.636s, recovery 227.048s. Narrow semantic completion passed on
candidate 30.09s/baseline 34.32s. These include new small Go FINALs and retained
auth/refusal/replay/owner-isolation controls.
`TestPrerequisiteClosureReleaseLiveDB` is a store test, outside that focused run:
it passed in 5.74s within the failed candidate aggregate (baseline 5.49s);
that individual PASS does not make the aggregate successful.
Observed PASS: `fmt-check:api`, `lint:api`, Go vet for handler/workersvc/recovery,
setup/native focused serial race/count=1 checks, agent npm lint/typecheck,
and `UZI_DEADCODE_AGENT_REQUIRED=1 task check:agent`.
No full agent unit run is claimed. Landing still requires full agent suite:
exact-head PR CI, required API/repo checks and exact published-head full store-it.

Old-case equivalent comparison: candidate 22 RUN/PASS, zero SKIP/FAIL;
baseline → candidate: C2 6.66s → 8.67s, Small 6.21s → 7.26s,
old E5 72.28s → 65.84s, total 171.46s → 176.73s.
Candidate CLI build was 68.927s; baseline build was not separately instrumented.
No causal CI-performance conclusion follows: full job censored; queue, cache,
rerun effects and hosted critical path unmeasured.

**Substitution and acceptance boundary.** Allowed substitutions were fixture
identities/data/tokens, loopback forge transport, native ancestry/project adapter
instead of hosted forge API, fixture-only forge binding/URL policy and generated
source content. Native listing/receive/create-only pinning, capture protection,
WorkerClient, authenticated API, DB receipt and owner CLI remained real.
Owner CLI used an explicit Store with allowed environment, without DefaultStore
or HOME changes; foreign-owner summary/download were refused; snapshots immutable.
No hosted GitHub latency/availability claim follows.
External post-release pin deletion remains an accepted, non-restorable residual.
E6 hosted reporting, rollout, #2969 and producer work remain out of scope.
This sanitized summary is the durable evidence; raw logs are not tracked and
scratch files are not durable artifact links. M2 is not waived; M3 remains partial.

### Parent acceptance

- C1a, C1b, C2 **and C3** complete (C3 may be dropped only by a separate owner decision adopting P5).
- LiveDB end to end: thin capture, force-push pruning the cached prerequisites, closure pinned, FINAL released with `prerequisite_closure`, clean clone plus pin fetch imports the bundle and `recovered-source` resolves to `source_sha`; the same with the force-push before pinning ends refused with the hold open. **Qualified:** demonstrated against default-branch rewrites with the pin intact; external deletion of the pin after release is the accepted residual and is not tested as restorable.
- No prerequisite or evidence path reads a runner-clone or agent-writable ref.
- Real-freeze-path, permission and origin cases pass; the v2 refusal is unchanged.
- Each fix test red on main and green on the exact PR candidate.

## Simplification proposals still open

- P3: drop the worker capability gate on S0.
- P4: merge `lost` and `foreign`; defer the audit (post-release deletion then wholly undetected).
- P5: fold C3 into C2 or defer it.

## Decision Log

| Date | Decision | Reason / boundary |
|---|---|---|
| 2026-10-21 | Option A direction over C; implementation gated by evidence. | Thin producer frequency. |
| 2026-10-21 | Option B rejected; option D separate. | Rewrite strands the archive; policy discard is not preservation. |
| 2026-10-21 | Q1 held first wherever eligible; Q2 separate ADR; Q3 no retrofit. | Ordering and compatibility. |
| 2026-10-21 (historical, then pending E2) | Anchor at a proven-descendant default tip. | The later E2 result below supports this choice and narrows the claim about direct pins. |
| 2026-10-23 (maintainer) | #2969 re-scoped behind option A. | Over-cap self-contained bundles. |
| 2026-10-25 (maintainer, #3031 U1) | Producer preservation-only above the cap. | Over-cap bundle. |
| 2026-10-25 (owners) | O1 archive-class only; real-freeze-path tests. | Archive FINAL authority is server-side. |
| 2026-10-25 (owners) | P1: no server bundle verifier; trust contract T1 to T5; pinning proves durability, not validity. | Same trust as self-contained archives. |
| 2026-10-25 (owners) | One eager sequence S0 to S7; #2969 skips on its cap. | Lazy pinning is circular. |
| 2026-10-25 (owners) | S0 atomic in `Service.stream`; capability from registered `ProtocolCapabilities`; no back-fill. | No orphan or missing rows. |
| 2026-10-25 (owners; corrects revision 6) | Validation: API-only exception; full API constituents and LiveDB PASS/zero-SKIP on the exact head via CI or the lander; agent and web gates in the worker. | Worker limits for the API aggregate. |
| 2026-10-25 (owners) | O4: a separately reviewed #3444 amendment is necessary; option A alone authorizes no producer FINAL and waives no producer proof. | D-U4d-1. |
| 2026-10-25 (owners) | O8 deferred: unrelated guard tightening. | Minimal change set. |
| 2026-10-25 (user) | O2: **Proceed** to design review; zero verified standalone A1 stays explicit. | Enabling need measured. |
| 2026-10-25 (user) | External deletion of `refs/uzi-prereq/*` or history rewriting after a closure release is an accepted preservation residual. | ADR-1296's "Later user deletion or rewriting is outside the proof"; no cross-forge ref protection. |
| 2026-10-25 (owners, Opus 1-3) | Default-deny closure-mint permission; ordinary-origin only (producer captures still pinned); protocol feature advertised only from C2. | Held-first ordering, #3444 protection, rollout safety. |
| 2026-10-25 (owners, Astra R1-R3) | Named live pointer null until S2, S2 takes the run lock first; corrected cost bound with a bounded fair dedicated pass and durable ambiguous-delete and ceiling outcomes; C1/C2/parent evidence separated. | Lock-order inversion; unbounded inventory; executable child acceptance. |
| 2026-10-25 (owners) | Replay requires no closure status; closure releases reconcile as `archive` and `validExactClosedProof` accepts `prerequisite_closure`. | Lost-ACK recovery; closed-hold reconciliation. |
| 2026-10-25 (owners, r8 review item 1; **corrected in revision 10**) | Saved thin FINAL replay is not gated by `healthyInventoryStatus` or a status read; it sends the unchanged MAC-authenticated request first. Revision 9 recorded "the `replaceable` reconcile outcome is not taken"; that is **reversed**: replacement is allowed only through the existing validated `replaceable` path, which the server returns only after `accepted` is ruled out and the unaccepted capture is fenced and committed. Timeout, malformed and `retained` never clear the request. | A committed ACK stays recoverable; a genuinely unaccepted capture can still be replaced through the existing guarded path instead of being stuck. |
| 2026-10-25 (owners, Opus final pass on revision 9) | S5 never reports a mintable state while `absent_since` is set; exact observation accepts `prerequisite_closure` for archive at both server checks and the agent validator; #2969's call site is a named grantor limited to grants 1 and 4; one bounded item drives a new row through S3; the capture guard refuses entering `available` except from `uploading`. | Close the remaining reviewer folds without widening authority. |
| 2026-10-25 (owners, r8 review item 2) | Concurrency: one process-wide slot for all closure forge work (kicks and pass); sweep work on the existing `sweeper.Engine`; a busy slot drops the kick, recovered from the durable row. | Explicit numeric bound; reuse of existing executor and semaphore pattern. |
| 2026-10-25 (owners) | O9: positively known absence of both held features, with the closure feature positively negotiated, is structural ineligibility; unknown capability state denies; held publication need not be enabled. | Held cannot run, so there is nothing to pre-empt. |
| 2026-10-25 (owners) | O10: accept accumulating unresolved rows and pins with bounded fair processing, removal blockers and E6 reporting; no total inventory bound claimed. | Astra R2's accepted-residual branch. |
| 2026-10-25 (owners) | O11: the first absent listing denies FINAL immediately; the pointer is kept until a fresh confirming listing in a later bounded pass; a positive listing in between cancels the candidate. | One negative listing is not proof of loss. |
| 2026-10-25 (owners) | O12: add `state = 'uploading'` to `MarkCaptureReady`; test that expired or discarded captures cannot become available again. | Enforce the one-way invariant T5 relies on. |
| 2026-10-28 (C1a execution, #3590) | Implemented O12's ready-query predicate and the separate UPDATE entry guard at `991df8b3` (00374); baseline and independent query/trigger mutants fail at their required assertions. | Ready requires a bound uploading capture; the guard raises 55000 before the identity no-op and allows same-state available updates under existing guards. This selected entry rule does not prove terminal states absorbing under arbitrary SQL; INSERT authority is unchanged. Exact-head CI/lander acceptance remains pending. |
| 2026-10-25 (owners) | Not folded: deleting pins on other-route settlement; clearing removal blockers by timeout or an `abandoned` label; a server header parser. | Keep pins while archives remain available; never clear a blocker without proof. |
| 2026-10-27 (E2 observation; fold drafted 2026-10-28, pending review) | GitHub, classic repo-only PAT, exactly Write: current-default anchor and same-workflow older tips accepted; tested differing-workflow old tip refused; native Git agrees. | FINDING, one provider refusal, zero probe malfunctions; no blanket direct-pin feasibility. |
| 2026-10-28 (draft consequence, pending review) | Keep the compatible default-tip anchor and all ancestry/binding/lifetime rules; no token expansion or retry at a new D after invocation. #2969 consumes implementation-proven pin status within its existing window only. | E2 closes the tested transport question, not implementation, integration, other-forge feasibility or #3031 producer FINAL authority. |
| 2026-10-28 (user; matching draft review pending) | Split C1 into API-only C1a ready-query/guard/fixture prefactor and dependent C1b complete pin lifecycle; C2 owns agent changes and release. Publish E4 distribution, clarify registration/status/retry contracts, use next free migrations at landing. | One reviewable invariant first; no partial pin lifecycle, capability advertisement or release. Agent changes in later children use full-suite exact-head PR CI; C1a/C1b stop on agent scope drift. |
| 2026-10-31 (lead/buddy scope agreement after paired architecture review) | Keep C2 as one release-proof child and one PR, implemented in reviewed serial commits; C3 remains the independently useful owner/restore child and is dispatched serially by default because files overlap. | No independently valuable further C2 split was identified under the supported-worker acceptance. A dark, nonadvertised API-first stage is a safe fallback if run size requires it, but does not by itself deliver the supported-worker release outcome. Advertisement before API acceptance is implemented remains forbidden; API-first acceptance without advertisement does not create that rollout hole. C1b already isolated pin creation/lifetime. The API, worker, schema and reader compatibility must agree on the same proof; size alone does not justify dropping tests or authority checks. This records the reviewed scope choice; dispatch still requires its gated plan review. |
| 2026-11-01 (lead/buddy C2 landing review) | Preserve existing graceful HTTP drain. Graceful drain lets in-flight requests finish or time out within their request-bounded context; shutdown does not cancel handler contexts; plan v4 wording was stronger than production and is superseded. | Safety comes from atomic release plus the full locked recheck, not cancellation on shutdown. A valid in-flight FINAL may commit fully during drain; a stalled listing ends by its bounded context without custody effect. After HTTP shutdown and DrainPrerequisiteClosures return, no detached listing/goroutine or open transaction remains. No global shutdown change or new cancellation subsystem is authorized. |

## Open questions (owner calls)

- O9 to O12 decided by the owners on 2026-10-25 (Decision Log).
- P3 to P5 above.
- E2 ran on the tested GitHub configuration and E4 was reported for C1a/C1b's reviewed delivery. Later child filing/dispatch still requires its own final text, architecture and plan reviews. Other forges/configurations remain unverified and unlisted.
