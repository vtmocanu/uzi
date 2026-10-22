---
title: Issue 3031 delivery status
order: 3031
audience: contributor
---

# Issue 3031 delivery status

Refs #3031. This is an incomplete delivery under the supplied plan's R3.
The terminal producer remains disabled. S and N are implemented in the seed;
their independent validation results are recorded below when available.
No plan revision or producer enablement is included.

## Reachable-behaviour changes

- S: background recovery passes cancel their Git, lock-wait and HTTP work on
  shutdown and join actual settlement. Foreground preservation retains its
  caller-owned grace. An uncertain remote request retains its original identity.
- N: an accepted ordinary guarded archive upload schedules saved-evidence replay
  after confirmed process, command, steering, auxiliary and execution-tail
  settlement. Unconfirmed settlement and shutdown suppress notification.
  Replay does not call producer admission or charge a producer slot.
- No provenance write at P, table-v2 initialization, attempt-v1 lineage,
  rediscovery map, continuation or boot/live ordering change is delivered.
  The ledger remains v1. Existing capture and transfer helpers remain.

The published seed is retained. Default branch revision
`a077e42b9b08cc3e4a24ff59d456d13342ee897f` was integrated with merge
`3e7fdced754f75395dc3ee365dc776144a258a77`; no published commit was rewritten.

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

No W0/U4b helper is replaced or deleted. There are no superseded test cases
or changed charge categories to map: `git-maintenance-object-snapshot.test.ts`,
`git-maintenance-private-verification.test.ts` and the original WIP cases
remain. The requested port and old → new accounting tables remain incomplete
with their implementation. No caps, timeouts or test timeouts are raised.

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

All MO items remain maintainer-owned and unperformed. The local Git fixture
has sufficient history (2,110 commits at merge, non-shallow), but the composed
measurement cannot be built under the conflicting W0 contract. MO7 is not
a substitute for resolving that conflict. Local environment limits and MO
do not waive worker obligations.

## Validation record

Pending independent reports and frozen gate results; this section will be
updated only with observed results. No approval or completion is inferred.
