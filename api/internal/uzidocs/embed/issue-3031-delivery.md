---
title: Issue 3031 delivery status
order: 3031
audience: contributor
---

# Issue 3031 delivery status

Refs #3031. This is an incomplete delivery under the supplied plan's R3.
The terminal producer remains disabled. S and N are implemented, with the
scoped acceptance, regression and mutation evidence recorded below. Full
frozen-gate results belong to the PR verification record; live acceptance
remains separate. No plan revision or producer enablement is included.

## Reachable-behaviour changes

- S: background recovery passes cancel their Git, lock-wait and HTTP work on
  shutdown and join actual settlement. Foreground preservation retains its
  caller-owned grace. Responses register cleanup at transport fulfillment,
  share the first body/reader cancellation promise, and join requested cleanup
  before an RPC failure unwinds caller locks. Pass teardown joins remaining
  responses without renewing the attempt clock. An uncertain remote request
  retains its original identity. A pass aborted after the transient bundle ref is created may leave it, potentially indefinitely until a later successful bundle on that bare replaces and deletes it; it is excluded from coverage proofs and only retains objects.
- N: an accepted ordinary guarded archive upload schedules saved-evidence replay
  after confirmed process, command, steering, auxiliary and execution-tail
  settlement. Unconfirmed settlement and shutdown suppress notification.
  Replay does not call producer admission or charge a producer slot.
- No provenance write at P, table-v2 initialization, attempt-v1 lineage,
  rediscovery map, continuation or boot/live ordering change is delivered.
  The ledger remains v1. Existing capture and transfer helpers remain.

The published seed and history are retained. The recovered record names
default branch revision `a077e42b9b08cc3e4a24ff59d456d13342ee897f` and merge
`3e7fdced754f75395dc3ee365dc776144a258a77`. In the current continuation,
the required default `e707e0b223328c734f1c6cc0bcc419e1b64adc4d` merged
without conflicts as `9c2b0ccb10baf725cbead7f2670ed28e795b99fc`;
no published commit was rewritten.

## Material change needed

W0 has two independent contract conflicts.

1. **Index transition.** W0 P1 requires the exact post-add index pin
   (dev/ino/size/mtime/ctime and trailing checksum) to survive commit and P5.
   An isolated Linux Git 2.54.0 fixture with `--no-optional-locks`,
   `GIT_OPTIONAL_LOCKS=0`, disabled signing and empty hooks instead changed
   the index from 174 to 193 bytes and replaced its inode/checksum during a
   successful normal commit. `ls-files --stage` stayed identical and status
   became clean. The observed checksums were
   `88f9ac4900f5211559744ad9f7bcfdc3493c11d1` before commit and
   `2d4d8e449815a6fcd5b4e9fa951d903164e746ed` afterward.
   ADR-1197 already records the predecessor's equivalent cache-tree finding.
   Re-pinning or changing the command sequence requires a revised contract.
2. **Content policy.** W0 P3 requires frozen parent caps “with no hidden lower
   cap”, while P4 captures final-worktree content with `add -A`.
   [specs/human.md](../specs/human.md) prohibits core dumps and files over
   8 MiB in WIP auto-save. [ADR-0759](../adr/0759-protect-run-work-usage-limit-park.md)
   explicitly applies that rule to `captureMaintenanceWip`.
   [The existing tests](../agent/test/git-maintenance-wip-capture.test.ts)
   require new/changed oversized/core content to refuse before commit,
   exactly 8 MiB to succeed, and unchanged already-HEAD content to remain
   eligible. A single new 8 MiB + 1 byte file is within the slot's aggregate
   byte cap but must refuse under this higher-authority policy.
   Keeping that refusal requires explicit reconciliation with W0's literal
   coverage contract; removing it would violate the policy and R2.

No workaround is implemented. W0, U4b, U4c, U4d, U5, F, M3 and M4 remain
incomplete. The composed measurement is unrun; no capacity projection is
verified. No delta-transfer ADR amendment is installed without its code.

## Requirement map

The owner of worker verification is this run. MO ownership is the maintainer
or landing owner. “Incomplete” means the requirement has not been established
for the requested producer pipeline; existing groundwork alone does not
complete it.

| Clause | Required milestone / evidence | Delivery and owner |
|---|---|---|
| A1 terminal blocked/orphan preservation; legacy execution intact | U4b–U5, M3.1–2; old-cap red/green, stopped zero-mutation | Incomplete; run |
| A2 decoded uniqueness and within-operation bounds | U4b 12–16 | Incomplete; run |
| A3 durable bounded pacing | M3.3 virtual-clock suite, MO1 real roll | Incomplete; run + maintainer |
| A4 positive exact terminal custody | Groundwork + U5 unique-open listing matrix | Groundwork terminal-origin tests retained; U5 incomplete; run |
| A5 immutable evidence first; replay proofs | U4d 2, U5 10, N 1–2 | N seed implemented; producer handoff/boot ordering incomplete; run |
| A6 finite discovery/capture/fallback for every terminal status | W0 + composed gate including archive | Incomplete; no MEASURE output; run, MO7 if unavailable |
| A7 R2401 and R1790 | M3.6–7; five diagnostics and independent durability | Producer acceptance incomplete; run |
| A8 closed holds stop new evidence | U4d 5, M3.5 | Incomplete; run |
| A9 no closure-based deletion | M3.7b, F | No new cleanup authority; producer proof incomplete; run |
| A10 retention, visibility, secret, workflow, residue | Existing reconciler + M4 | Existing rules retained; full requested validation incomplete; run |
| A11 initialization/history/compatibility/roll/restore | M3.9–12, MO1 | No format change; requested matrix incomplete; run + maintainer |
| A12 red/green, frozen gates, CI, ADR, E2E | R6, M4, MO5–6 | Only actually observed results below; incomplete; run + maintainer |
| S1–S8 origin controls | Groundwork + M3.5, M3.9 | Existing terminal-origin tests retained; complete producer matrix incomplete; run |
| R2971-01 old-cap persisted episode, cap raise and terminal preservation | M3.1 | Incomplete; run |
| R2971-02 execution blockers and fences unchanged | M3.1–2 | No execution reset introduced; producer proof incomplete; run |
| R2971-03 decoded-limit refusal, no renewed hashing | U4b, M3.3/5 | Incomplete; run |
| R2971-04 durable precharge/crash controls and legacy bootstrap | U5, M3.3/11 | Incomplete; run |
| R2971-05 cap/config identity and historical readers/backups | U5, M3.9–11 | No table-v2 delivery; incomplete; run |
| R2971-06 preservation-only ADR amendment | M4 | ADR documents actual partial delivery; producer contract incomplete; run |
| R2971-07 serial local gates and CI | M4 | Results below; no unperformed check accepted; run |
| R2022-03 exact identity, receipts and reconciliation before requests | M3.8 | S retains uncertain request identity; producer matrix incomplete; run |
| R2022-04 retained sources/journals/holds and bounded retry | M3.8 | No refusal becomes cleanup authority; producer matrix incomplete; run |
| R2401-01 five cheap-proof refusal dimensions | M3.6; `git-cancel-clean-head-diagnostics.test.ts` | Existing tests retained; requested carried red/green incomplete; run |
| R2401-02 positive clean-tree release control | M3.6 | Existing path retained; acceptance incomplete; run |
| R2401-03 specific diagnostics and realistic timing/RSS | M3.6, composed gate | Caps unchanged; representative measurements unrun; run |
| R2401-04 diagnostic regression red/green and frozen gates | M3.6, M4 | Incomplete; run |
| R1790-T01 local-only copy never released/deleted | M3.7b/c, F | No producer release authority introduced; acceptance incomplete; run |
| R1790-T02 independent restoration on another worker; missing volume unavailable | M3.7a/c | Incomplete; run |
| C1–C4, E1–E7 exclusion/family/stop controls | Groundwork + M3.2, MO2 | Groundwork retained; expanded matrix/live proof incomplete; run + maintainer |
| Effect addendum | R8, M3.4, U4b 21 | No new producer effects; expanded matrix incomplete; run |
| Restart discovery | U5 4b tests 7–8, 12–16 | Not delivered; run |
| Hash-attested WIP completeness | W0 tests 1–7 | Blocked by material conflicts above; run |
| Guard removal / enablement | Separate follow-up, MO1–4 | Deferred by binding guard-off decision; follow-up owner + maintainer |
| Item 11 shutdown joins | S acceptance and mutations, MO4 | Seed implemented; local evidence below; image acceptance unperformed; run + maintainer |

## Port and accounting mappings

No W0/U4b helper is replaced or deleted. `git-maintenance-object-snapshot.test.ts`,
`git-maintenance-private-verification.test.ts` and the original WIP cases remain.
Their producer-specific port and old → new accounting tables remain incomplete
with their implementation. No charge category, cap, timeout or test timeout
changes.

The ordinary replay compatibility map below is exhaustive for the 71 cases
run across the three legacy files. All case titles and assertions are
unchanged. Cases that explicitly own a LIVE, foreground or restart phase
defer automatic notification before `trackPass` registration, then exercise
the same original phase. Cleanup closes runner admission before releasing
and joining the deferred calls through the real shutdown guard. The dedicated
automatic-notification tests use no such deferral.

| Existing case | After this port |
|---|---|
| `runner-held-publication-fallbacks.test.ts`: a new WIP secret is never uploaded and is remembered as flagged | Same title and assertions; automatic notification deferred for the original explicit recovery phase. |
| `runner-held-publication-fallbacks.test.ts`: a remembered flagged ancestor is never uploaded, even when the fresh scan sees nothing | Same title and assertions; automatic notification deferred for the original explicit recovery phase. |
| `runner-held-publication-fallbacks.test.ts`: an untrusted scan (deadline) archives | Same title and assertions; automatic notification deferred for the original explicit recovery phase. |
| `runner-held-publication-fallbacks.test.ts`: an untrusted scan (range_invalid) archives | Same title and assertions; automatic notification deferred for the original explicit recovery phase. |
| `runner-held-publication-fallbacks.test.ts`: an untrusted scan (scan_untrusted) archives | Same title and assertions; automatic notification deferred for the original explicit recovery phase. |
| `runner-held-publication-fallbacks.test.ts`: an untrusted scan (too_many_merges) archives | Same title and assertions; automatic notification deferred for the original explicit recovery phase. |
| `runner-held-publication-fallbacks.test.ts`: an untrusted scan (count_failed) archives | Same title and assertions; automatic notification deferred for the original explicit recovery phase. |
| `runner-held-publication-fallbacks.test.ts`: a scan that throws archives | Same title and assertions; automatic notification deferred for the original explicit recovery phase. |
| `runner-held-publication-fallbacks.test.ts`: an unresolved range archives | Same title and assertions; automatic notification deferred for the original explicit recovery phase. |
| `runner-held-publication-fallbacks.test.ts`: a workflow change in the scanned range archives | Same title and assertions; automatic notification deferred for the original explicit recovery phase. |
| `runner-held-publication-fallbacks.test.ts`: a branch merely stale against the default branch's workflow files is still held | Same title and assertions; automatic notification deferred for the original explicit recovery phase. |
| `runner-held-publication-fallbacks.test.ts`: a pack over the cap is never sent and archives | Same title and assertions; automatic notification deferred for the original explicit recovery phase. |
| `runner-held-publication-fallbacks.test.ts`: step A refused archives on the same record | Same title and assertions; automatic notification deferred for the original explicit recovery phase. |
| `runner-held-publication-fallbacks.test.ts`: step A create_unknown archives on the same record | Same title and assertions; automatic notification deferred for the original explicit recovery phase. |
| `runner-held-publication-fallbacks.test.ts`: step A invoked archives on the same record | Same title and assertions; automatic notification deferred for the original explicit recovery phase. |
| `runner-held-publication-fallbacks.test.ts`: step A 409 refusal archives on the same record | Same title and assertions; automatic notification deferred for the original explicit recovery phase. |
| `runner-held-publication-fallbacks.test.ts`: step A 413 archives on the same record | Same title and assertions; automatic notification deferred for the original explicit recovery phase. |
| `runner-held-publication-fallbacks.test.ts`: step A ci_config_guard refusal archives on the same record | Same title and assertions; automatic notification deferred for the original explicit recovery phase. |
| `runner-held-publication-fallbacks.test.ts`: step A 422 archives on the same record | Same title and assertions; automatic notification deferred for the original explicit recovery phase. |
| `runner-held-publication-fallbacks.test.ts`: step A 429 archives on the same record | Same title and assertions; automatic notification deferred for the original explicit recovery phase. |
| `runner-held-publication-fallbacks.test.ts`: step A 503 archives on the same record | Same title and assertions; automatic notification deferred for the original explicit recovery phase. |
| `runner-held-publication-fallbacks.test.ts`: step A a different tip archives on the same record | Same title and assertions; automatic notification deferred for the original explicit recovery phase. |
| `runner-held-publication-fallbacks.test.ts`: step A a foreign ref archives on the same record | Same title and assertions; automatic notification deferred for the original explicit recovery phase. |
| `runner-held-publication-fallbacks.test.ts`: step A a timeout archives on the same record | Same title and assertions; automatic notification deferred for the original explicit recovery phase. |
| `runner-held-publication-fallbacks.test.ts`: step A a network error archives on the same record | Same title and assertions; automatic notification deferred for the original explicit recovery phase. |
| `runner-held-publication-fallbacks.test.ts`: an api without the held feature archives exactly as before | Same title and assertions; automatic notification deferred for the original explicit recovery phase. |
| `runner-held-publication-fallbacks.test.ts`: a feature withdrawn before the run archives exactly as before | Same title and assertions; automatic notification deferred for the original explicit recovery phase. |
| `runner-held-publication-fallbacks.test.ts`: push_secret_blocked and worker_residue_blocked failures never defer or publish | Same title and assertions; automatic notification deferred for the original explicit recovery phase. |
| `runner-held-publication-fallbacks.test.ts`: only a failed terminal can defer: cancelled, parked and completed never do | Same title and assertions; unchanged policy control. |
| `runner-held-publication-fallbacks.test.ts`: the held path never receives the forge credential | Same title and assertions; automatic notification deferred for the original explicit recovery phase. |
| `runner-held-publication-fallbacks.test.ts`: definitive held_publication_ref_missing falls back to the archive, custody held until its FINAL is ACKed | Same title and assertions; automatic notification deferred for the original explicit recovery phase. |
| `runner-held-publication-fallbacks.test.ts`: definitive held_publication_ref_mismatch falls back to the archive, custody held until its FINAL is ACKed | Same title and assertions; automatic notification deferred for the original explicit recovery phase. |
| `runner-held-publication-fallbacks.test.ts`: definitive held_publication_not_eligible falls back to the archive, custody held until its FINAL is ACKed | Same title and assertions; automatic notification deferred for the original explicit recovery phase. |
| `runner-held-publication-fallbacks.test.ts`: definitive held_publication_unsupported falls back to the archive, custody held until its FINAL is ACKed | Same title and assertions; automatic notification deferred for the original explicit recovery phase. |
| `runner-held-publication-fallbacks.test.ts`: definitive a downgrade 400 falls back to the archive, custody held until its FINAL is ACKed | Same title and assertions; automatic notification deferred for the original explicit recovery phase. |
| `runner-held-publication-fallbacks.test.ts`: a non-definitive held_publication_unavailable keeps the journaled FINAL and the hold across a restart | Same title and assertions; automatic notification deferred for the original explicit recovery phase. |
| `runner-held-publication-fallbacks.test.ts`: a non-definitive a transport error keeps the journaled FINAL and the hold across a restart | Same title and assertions; automatic notification deferred for the original explicit recovery phase. |
| `runner-held-publication-fallbacks.test.ts`: a non-definitive a 503 keeps the journaled FINAL and the hold across a restart | Same title and assertions; automatic notification deferred for the original explicit recovery phase. |
| `runner-held-publication-fallbacks.test.ts`: a client-side feature absence on replay is non-definitive: the FINAL is kept and the hold stays open | Same title and assertions; automatic notification deferred for the original explicit recovery phase. |
| `runner-held-publication-fallbacks.test.ts`: a held FINAL kept as unavailable and definitively refused after a restart reaches the capture on the resumed pass | Same title and assertions; automatic notification deferred for the original explicit recovery phase. |
| `runner-held-publication-fallbacks.test.ts`: a crash after the failed send resumes into the archive capture on the same record | Same title and assertions; automatic notification deferred for the original explicit recovery phase. |
| `runner-held-publication-fallbacks.test.ts`: freezes nothing, uploads nothing and sends no FINAL; the hold and the sources stay | Same title and assertions; automatic notification deferred for the original explicit recovery phase. |
| `runner-held-publication-fallbacks.test.ts`: a completed first winner at the journaled caller never defers, whatever this caller's body | Same title and assertions; unchanged policy control. |
| `runner-issue-2494-m1.test.ts`: M1 completion outage restart before sweep boot receipt | Same title and assertions; automatic notification deferred for the original explicit recovery phase. |
| `runner-issue-2494-m1.test.ts`: M1 completion outage restart before sweep boot reason | Same title and assertions; automatic notification deferred for the original explicit recovery phase. |
| `runner-issue-2494-m1.test.ts`: M1 completion outage restart before sweep boot neither | Same title and assertions; automatic notification deferred for the original explicit recovery phase. |
| `runner-issue-2494-m1.test.ts`: M1 completion outage restart before sweep live receipt | Same title and assertions; automatic notification deferred for the original explicit recovery phase. |
| `runner-issue-2494-m1.test.ts`: M1 completion outage restart before sweep live reason | Same title and assertions; automatic notification deferred for the original explicit recovery phase. |
| `runner-issue-2494-m1.test.ts`: M1 completion outage restart before sweep live neither | Same title and assertions; automatic notification deferred for the original explicit recovery phase. |
| `runner-issue-2494-m1.test.ts`: M1 cancelled gated issue LIVE archive | Same title and assertions; automatic notification deferred for the original explicit recovery phase. |
| `runner-issue-2494-m1.test.ts`: M1 cancelled gated issue LIVE held | Same title and assertions; automatic notification deferred for the original explicit recovery phase. |
| `runner-issue-2494-m1.test.ts`: M1 cancelled gated issue LIVE unknown | Same title and assertions; automatic notification deferred for the original explicit recovery phase. |
| `runner-issue-2494-m1.test.ts`: M1 completed ci_fix uploaded archive live after-exit | Same title and assertions; automatic notification deferred for the original explicit recovery phase. |
| `runner-issue-2494-m1.test.ts`: M1 completed ci_fix uploaded archive live during-exit | Same title and assertions; automatic notification deferred for the original explicit recovery phase. |
| `runner-issue-2494-m1.test.ts`: M1 completed ci_fix uploaded archive live report-before-reservation | Same title and assertions; automatic notification deferred for the original explicit recovery phase. |
| `runner-owner-cancel-v2.test.ts`: V2 owner cancellation preserves frozen inventory: wallpark | Same title and assertions; automatic notification deferred for the original explicit recovery phase. |
| `runner-owner-cancel-v2.test.ts`: V2 owner cancellation preserves frozen inventory: clarification | Same title and assertions; automatic notification deferred for the original explicit recovery phase. |
| `runner-owner-cancel-v2.test.ts`: V2 owner cancellation preserves frozen inventory: old-generation-foreground | Same title and assertions; automatic notification deferred for the original explicit recovery phase. |
| `runner-owner-cancel-v2.test.ts`: V2 owner cancellation preserves frozen inventory: late-quiescence | Same title and assertions; automatic notification deferred for the original explicit recovery phase. |
| `runner-owner-cancel-v2.test.ts`: #2901 before-model cancelled identical-tree inventory with no messages or checkpoint | Same title and assertions; automatic notification deferred for the original explicit recovery phase. |
| `runner-owner-cancel-v2.test.ts`: V2 owner cancellation preserves frozen inventory: dirty | Same title and assertions; automatic notification deferred for the original explicit recovery phase. |
| `runner-owner-cancel-v2.test.ts`: V2 owner cancellation preserves frozen inventory: closed | Same title and assertions; automatic notification deferred for the original explicit recovery phase. |
| `runner-owner-cancel-v2.test.ts`: V2 owner cancellation preserves frozen inventory: unknown | Same title and assertions; automatic notification deferred for the original explicit recovery phase. |
| `runner-owner-cancel-v2.test.ts`: V2 owner cancellation preserves frozen inventory: wrong-custodian | Same title and assertions; automatic notification deferred for the original explicit recovery phase. |
| `runner-owner-cancel-v2.test.ts`: V2 owner cancellation preserves frozen inventory: wrong-repo | Same title and assertions; automatic notification deferred for the original explicit recovery phase. |
| `runner-owner-cancel-v2.test.ts`: V2 owner cancellation preserves frozen inventory: wrong-live-run | Same title and assertions; automatic notification deferred for the original explicit recovery phase. |
| `runner-owner-cancel-v2.test.ts`: V2 owner cancellation preserves frozen inventory: malformed | Same title and assertions; automatic notification deferred for the original explicit recovery phase. |
| `runner-owner-cancel-v2.test.ts`: V2 owner cancellation preserves frozen inventory: live | Same title and assertions; automatic notification deferred for the original explicit recovery phase. |
| `runner-owner-cancel-v2.test.ts`: V2 owner cancellation preserves frozen inventory: retained | Same title and assertions; automatic notification deferred for the original explicit recovery phase. |
| `runner-owner-cancel-v2.test.ts`: V2 owner cancellation preserves frozen inventory: registry | Same title and assertions; automatic notification deferred for the original explicit recovery phase. |
| `runner-owner-cancel-v2.test.ts`: V2 owner cancellation preserves frozen inventory: late-writer | Same title and assertions; automatic notification deferred for the original explicit recovery phase. |

## F operation table

No new producer file operations are shipped. F remains incomplete, and no
auditor signoff of an implemented producer pipeline is claimed.

| Operation | Required invariant | Planned evidence, unperformed |
|---|---|---|
| Ledger/origin/freeze/scratch create or write | Pinned parent; O_NOFOLLOW/O_EXCL; file fsync; refuse nlink > 1 | Parent substitution, symlink target, hardlinked target |
| Rename/promote | Same pinned directory; directory fsync; parent dev/ino recheck | Inode swap before rename |
| Import | Pinned bare under lock; strict index-pack; no ref; identity recheck | Import and bare substitution controls |
| Walk/pack/ledger read | No-follow; pinned identity; complete-after-read check | Symlink/identity substitution |
| Disposal | Only slot-created dev/ino-matched entries; never recursive | Foreign entry retained |

## Deferred

The approved guard-off decision reserves enablement for a follow-up after MO.
The planned provenance scheme's accepted shortfall covers legacy,
pre-upgrade, canonical-reseed, adopted and unrecorded sources; it supplies
no guarantee here because U5 is not delivered. Root loss/rollback and
downgrade residuals remain as recorded in ADR-1197. No ledger export or
restore artifact is added. Related issues #2852, #3101/#3132, #2969, #2884,
#2494, #2506-A, #2544 and #3105 remain separately owned.

The recovered historical reviewer report at `3e7fdced` leaves one performance note:
CI before/after and critical-path timing remains unmeasured (M4).
Infrastructure suggestions from the tester reports are deferred: two
coverage/pre-commit suggestions at `3e7fdced`, two formatter/coverage
suggestions at `e1c7e3f5`, and two coverage/pre-commit suggestions at
`7ec239bc`. These are suggestions, not demonstrated product defects.
The auditor's one dependency note at `e1c7e3f5` was recorded for separate
triage; no dependencies are changed by these repairs. Existing Moderate
Vitest and ip-address advisories are not accepted as a waiver of required
security gates.

## MO: maintainer acceptance (final informative milestone)

The run cannot perform these; the responsible actor is the maintainer or landing owner.
- Items 1-4 run on the **branch-built image of the follow-up guard-removal PR** (appendix) and gate **that** PR's merge. Items 1-3 need a live producer.
- Items 4-7 apply to this run's candidate. Item 4 applies to both, because S ships here.
- Every item is rerun after relevant later changes. Process tests stay separately labelled and never substitute.

1. **Real SIGTERM → pod replacement on the same PVC (k8s), during charged slot 1.** After an outage, only slot 2 of the same admission is charged, after fresh validation. Record:
   - old and new pod, PVC and root;
   - original admittedAt and expiry, unchanged reservations;
   - no permit revival or image-version reset;
   - repeated restarts staying within bounds;
   - an expired-window protected refusal.
2. A production-image process tree with a live producer: exclusive-idle against the real tini → … → tsx → esbuild tree (E1-E7).
3. Writer exclusion under the real image:
   - the gate stays closed on unconfirmed settlement until restart;
   - retained-ops discard refuses while it is held;
   - claims resume after settlement.
4. A process-level SIGTERM exit time on the production image with a blocked bundle child and a hung reserve call: under 5 s (complements S).
5. Branch compose E2E across the three forges for the affected recovery and custody phases, plus hosted k8s dev-cluster acceptance.
6. `run-store-it.sh` with named LiveDB zero-skip, if the run recorded it unrun.
7. The U4b representative measurement, if the run recorded it unrun.

MO remains incomplete and maintainer-owned. MO1–5 and the composed
measurement are unperformed. At the validation capture below, store-it had
not yet run; the final PR records its actual outcome and retains MO6 if it remains
unrun, as the conditional item above requires. MO7 remains unperformed.
The current source is non-shallow with 2,123 commits; history availability
is not a measurement blocker. The composed measurement cannot be built under
the conflicting W0 contract. MO7 is not a substitute for resolving that
conflict. Local environment limits and MO do not waive worker obligations.

## Validation record

### Recovered historical evidence

The table and reports in this subsection are recovered records of earlier
revisions, not checks rerun or signoffs renewed in this continuation.
Their mutation results are historical reports, not declarations about current
mutation artifacts. Current-continuation evidence supplied by the lead is
recorded separately below.

Agent test commands use the repository's 120,000 ms timeout. Scoped
proofs ran on Linux, Node 24.20.0, with isolated Git configuration and the
hermetic process preload where appropriate. No production-image or PVC
result is inferred.

| Observed check | Result and revision |
|---|---|
| Seed terminal-origin admission and S shutdown files, same invocation | 50 passed, zero skips. The reported 22 admission failures did not reproduce. |
| Isolated exact seed terminal-origin file | 41 passed; confirms the seed result independently of the later merge. |
| Lint, typecheck and four focused recovery/WIP files | 132 passed, zero skips; this earlier focused run preceded the full gate. |
| First full `task gate:agent` | Failed at clean `c229f573`: 636/636 files, 19,521 tests, 29 failures, 15 skips, zero cancellations; unit stage 5,909 seconds. It also reported leftover test processes. The log is retained; this is a failed check. |
| Narrow equivalent base/candidate comparison | Three selected cases passed on `a077e42b` and failed on `c229f573`, through the FINAL/ACK/held-count assertions. |
| Legacy replay compatibility port | 71/71 passed, zero skips/cancellations at the `e1c7e3f5` source; lint and typecheck passed. Every existing assertion/title remains. |
| S response-handoff regression control | Seven selected tests on the repaired candidate with the original client failed through intended cleanup-order assertions; zero timeouts/cancellations. |
| S response-handoff focused proof | 120/120 passed, zero skips/cancellations at the `7ec239bc` source; typecheck, lint and knip passed. |
| E2E library, fake forge, watcher selected-source backups | `task test:e2e-lib`, `task test:forge-fake` and `task test:uzi-watcher-backups` passed. No new ledger backup artifact is added. |
| Documentation mirror | The recovered record reports that `task docs:sync` ran and planned another mirror before commit; that plan is not evidence of a current mirror. The lead owns integration and mirror synchronization. |

S's five acceptance cases are in
`recovery-pass-shutdown-join.test.ts`: real silent reserve/upload, real
bundle-child reaping before lock release, paired foreground grace, and the
ordinary no-shutdown control. Extra cases retain Git-inherited budget
independence and join the first response-body cancellation. The two late
header cases cover ownership with and without an active budget. In
`client-maintenance-budget.test.ts`, default POST, bounded GET, custom hold,
marker refusal and reader-abort cases cover the RPC cleanup fence.

Valid mutation controls reached behavioral assertions after typechecking:
unlinking pass ownership; linking shutdown to the caller budget; joining
a second cancellation instead of the first; disabling accepted-upload
notification; bypassing confirmed execution joins; and routing the replay
entry through `resumeTerminalSource`. The last uses the guard-substituted
eligible-source fixture and fails `ledger.admit`'s zero-call assertion
(`1 !== 0`). A preliminary recursive-budget mutation was invalid and is
excluded from the evidence.

The unlink control failed the bundle journal-retention assertion rather than
the child-lock-order assertion; it does not independently establish that
assertion's mutation sensitivity. The actual real-child acceptance still
checks child close before bare-lock release.

Independent reviewer, tester and auditor reports found no mandatory defects
in the N test port at `e1c7e3f5` or S repair at `7ec239bc`. All three S
reports close the original owned-response handoff finding. The tester also
ran two independent concurrent-RPC probes: an RPC joins its own cleanup,
leaves a live sibling untouched, and does not wait for a sibling's pending
cancellation. Both passed; replacing RPC-local membership with pass-wide
membership typechecked and failed the intended isolation assertion.

Exact-literal E2E contract sweeps for `execution_tail_present`,
`terminal_source` and `upload_rejected` found no matching expectations
in `e2e/phases/`, `e2e/lib.sh`, `e2e/lib.test.sh` or
`e2e/forge-fake/`. The recovery/custody API references identify
`e2e/phases/72-custody-lifecycle.sh`; that compose phase is **unrun** on
all three forges. Hosted and process-image checks remain MO-owned.

### Current-continuation observed evidence

These observations were captured for clean source `9c2b0ccb`, after
integration of required default `e707e0b2` and before the final documentation
commit. The PR records subsequent frozen-gate and final-review outcomes.

| Observed check | Result and revision |
|---|---|
| Full `task gate:agent` | **Failed**, exit 201 at clean `9c2b0ccb`, Linux, Node 24.20.0, umask 0022. All 637 files ran: 19,550 tests, 19,532 passed, 3 failed, 15 skipped, zero cancelled; unit duration 4,775 seconds. The launcher also reported LEFTOVER processes. Log: `.uzi/scratch/gate-log.65NQS6`. |
| Whole S/N-related files in that invocation | `recovery-pass-shutdown-join.test.ts`, `recovery-terminal-origin-admission.test.ts` and `runner-terminal-saved-final-replay.test.ts` passed; respective file durations 1.998552 s, 10.200042 s and 71.711223 s. All 22 named seed-regression cases have passing entries in this same invocation. These passes do not make the full gate green. |
| Immutable export comparison, three selected cases | Candidate `9c2b0ccb` and base `e707e0b2` each had 2 passes and 1 failure, using the same dependencies and lock SHA256 `91291c93d858cfc43abc435dfb24e9563e4bee1190341ca3d08d61f008017cf4`. Logs: `.uzi/scratch/gate-log.bCOWMi` and `.uzi/scratch/gate-log.f8Acov`. |
| Immutable export comparison, broader lifecycle/proc groups | Each revision had 25 passes and zero skips. Logs: `.uzi/scratch/gate-log.EB91NK` and `.uzi/scratch/gate-log.U1QXV7`. These comparisons remain inconclusive for the full-gate Codex/proc failures. |
| Continuation prerequisites | `task test:e2e-lib`, `task test:forge-fake` and `task test:uzi-watcher-backups` passed. |

Fresh mutation controls also used isolated exports of `9c2b0ccb`. The
lead-owned scope-unlink control typechecked and made both required reserve
and bundle tests fail through assertions, with zero cancellations or skips.
The reserve exceeded the 5,000 ms settlement bound; the bundle changed the
journal from `pinned` to `needs_action`. The latter detects journal retention,
not independent sensitivity of the child-order assertion. Logs:
`.uzi/scratch/gate-log.90BBkv` and `.uzi/scratch/gate-log.OcXmov`.

The lead checked the tester-owned mutation scripts and logs: foreground
budget sharing, owned-budget signal omission, late-header ownership,
first-cancellation replacement, disabled archive notification, bypassed
settlement receipts, and routing replay through source admission all
typechecked and failed through their intended assertions (10 selected cases,
zero cancellations or skips). The wrong-route control reached the real
`ledger.admit` zero-call assertion with `1 !== 0`; the notification control
failed its FINAL/ACK assertion. The initial foreground mutation that failed
typechecking is excluded. These mutants stayed in scratch exports; production
source remained unchanged. Their logs use the `.uzi/scratch/tester-1-` prefix.

The three full-gate failures were:

- `codex-executor`: “wall-during-dispose settles eligible Bash and retires roots before park/report”.
- `entrypoint-migration`: “nix store: every boot drops group write (reclaim, chmod 1755, hand back to runner:runner)”.
- `run-procs`: “this host's real proc tree: an unrelated NON-DUMPABLE process does not make the run busy”.

The Nix fixture fails identically on candidate and base because `STUB_NOOP`
leaves the data directory absent before the migration-marker write. This
off-task issue was recorded for triage. The selected Codex/proc cases both
pass in isolation; their comparisons, including the broader groups, are
**inconclusive** for the full-gate failures and do not establish that those
failures are unrelated to this change.

The reviewer at `9c2b0ccb` reported no mandatory S/N defect; optional CI
before/after and critical-path timing remains unmeasured. Final tester,
auditor and candidate-review outcomes belong to the PR verification record.
Recovered historical signoffs above do not supply current approval.

At this capture, `gate:api`, store-it and `gate:repo` had not yet run.
The final PR records the actual frozen serial gate revisions and results,
including store-it's outcome for conditional MO6. Both failed agent gates
remain failed records. The latest branch pipeline query returned `null`;
exact-candidate CI is unvalidated. Three-forge compose phases and hosted
acceptance remain unperformed; focused tests do not establish them.

## MEASURE lines

Unrun: the W0/U4b composed pipeline cannot be implemented under the conflicting
contract. No projected capacity is promoted to a measurement. The source
checkout is non-shallow and has sufficient history; history availability is
not the blocker.
