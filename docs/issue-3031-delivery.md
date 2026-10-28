---
title: Issue 3031 delivery status
order: 3031
audience: contributor
---

# Issue 3031 delivery status

Refs #3031. This is an incomplete delivery under the supplied plan's R3.
The terminal producer remains disabled. S and N are implemented, with the
scoped acceptance, regression and mutation evidence recorded below. C1/U4b is
implemented with the guard off; its focused acceptance, test ports, accounting and
conditional ADR amendment are recorded below. C2–C8 are not delivered. The M2
resource refusal is resolved (see the owner diagnosis and category correction
below; all ten slot dimensions are under their unchanged caps for a single-member
family, while a two-source family exceeds `sources`, recorded below as a C2 input). The remaining U4b
boundary is verification: the full frozen gate is substituted by exact-head CI per
the maintainer decision of 2026-10-26 (recorded in the PR for #3441: GitHub CI's
full agent suite on the exact PR head counts as the frozen-candidate `gate:agent`),
and the review and mutation evidence is refreshed at the final head. Full
frozen-gate results belong to the PR verification record; live acceptance
remains separate. The U4b contract revision is recorded below; no producer enablement is included.

## W0 delivery note (#3349)

W0-H is delivered: hash-attested maintenance WIP capture with a plumbing
commit (`add -A`, `write-tree` with a post-write-tree pin, a pre-commit
`diff-tree` proof, `commit-tree`, then an `update-ref` compare-and-swap). The
per-file core/8 MiB policy is the named cap. The terminal producer guard stays
off. U4b, U4c, U4d, U5, F, M3, M4 and MO remain as recorded below.

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
  The ledger remains v1. W0-H capture is unchanged; U4b replaces the dormant
  copy-store/private-verification helpers with an objects-only delta importer.

The published seed and history are retained. The recovered record names
default branch revision `a077e42b9b08cc3e4a24ff59d456d13342ee897f` and merge
`3e7fdced754f75395dc3ee365dc776144a258a77`. In the historical S/N continuation,
the required default `e707e0b223328c734f1c6cc0bcc419e1b64adc4d` merged
without conflicts as `9c2b0ccb10baf725cbead7f2670ed28e795b99fc`;
no published commit was rewritten.

## Material change needed

W0 had two independent contract conflicts. **Both are resolved (#3349)** by
the contract revision recorded in ADR-1197; the original evidence is kept
below as history.

- **Resolved (#3349), index transition.** The porcelain commit is replaced by
  plumbing. Two sanctioned pins apply, in the `add -A` and `write-tree` scope,
  and the P5 reference is the post-write-tree pin.
- **Resolved (#3349), content policy.** The per-file core/8 MiB policy is
  applied as the named only-lower cap, with the HEAD-tuple exemption.

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

Historical status (before #3349): no workaround was implemented, and W0, U4b,
U4c, U4d, U5, F, M3 and M4 were incomplete. W0 is now delivered (see the note
at the top); U4b, U4c, U4d, U5, F, M3 and M4 remain incomplete. The representative composed measurement remains C2 work; no capacity projection is verified.
The guard-off U4b implementation and its conditional ADR contract follow below.

## Material change needed: two-source family exceeds `sources` (fixture-injected diagnostic)

Diagnostic, not C2's measurement. A scratch copy of the runner `origin-admission`
mode at candidate `db8e010b` injected a sibling into the scanned family: it pushed a
second entry into `known` (the `originals` argument of the family scan), created
the sibling directory with `mkdir`, and inserted an abandoned-attempt ledger entry.
It created no second Git clone. Substitutions, as in the existing `origin-transfer-*`
modes: the terminal-origin guard method, a faked server, `passingExclusion()` for
the writer exclusion, and an injected quiescent verdict. The slot was minted
normally. It ran only to the first refusal and was not committed. It demonstrates
the observed descriptor-budget refusal; it does not measure the complete real
two-clone topology that C2 (#3442 item 5) requires.

Observed:

- 25 attempted `readLockedFamily` calls = 3 discovery reads (uncharged) +
  21 successful paid reads at 6 `sources` each (4 for a single-member family) +
  1 refused paid read. Two further `sources` units were charged elsewhere.
- The refused read fails with `maintenance_resource_exhausted` at `sources`
  128/128 (+1), in `count()` called from `readLockedFamily`, reached from the
  importer's `supervised` read in `beforeSpawn` inside `importMaintenanceTip`, after
  the transfer was entered and before any canonical import.
- Counters at the refusal point (not complete-path totals): launches 177/512,
  adminEntries 2,384/4,096, rpcAttempts 14/32 (plus 4 discovery), sourceEntries
  5,976/65,536, sourceBytes 198,747, metadataBytes 17,159,953, artifactBytes 142,
  closureEntries 4, decoded 0. Everything after the refusal is unmeasured.
- Fail-closed at the refusal: no canonical import (tip absent from the bare, refs
  unchanged), no output index, source clone and both attempt directories retained,
  no custody release and no bundle upload.

Projection, not measured: if the single-member path's 31 paid reads also applied to
a two-member family, the complete path would need about 31 x 6 + 2 = 188 `sources`
against 128. The single-member path measures 126/128.

The overrun binds only when the producer is enabled. `sources` is charged at two
sites: `consumeIfActive` inside `withMaintenanceFamilyLock` (the per-read charge),
and `MaintenanceResources.consume("sources", 2)` inside `importMaintenanceTip` (the
two further units above). Both apply only inside a slot, and slots are minted only by
terminal-origin admission, which returns early while
`TERMINAL_ORIGIN_ADMISSION_ENABLED` is false; `importMaintenanceTip` is reached only
from that admission path.

The earlier two-source success requirement (#3442 Design 5 / acceptance 4) is
outside the current single-attempt PASS conjunction under the maintainer's
[scope amendment](https://github.com/vtmocanu/uzi/issues/3442#issuecomment-6463625538).
Its fixture-injected descriptor-budget refusal remains diagnostic evidence, and
multi-source preservation remains undelivered. Reachability and capacity are
independent unresolved requirements: the two-member diagnostic refused at 128
`sources`; completing 31 paid reads at six units plus two would project to about
188 against 128, not a measured complete-path total. Any future multi-source design
must resolve both constructor/coverage authority and the unchanged aggregate
budget. #3440 does not absorb or waive either. C2 remains UNRUN; C3-C8 and producer
enablement remain blocked. No cap, accounting or scheduling change is made here.

## Delivery status of #3441 (non-closing)

Implemented, pending acceptance validation, guard off: design items 1 to 10, the
acceptance 1 to 22 and F-row tests as mapped in the port and acceptance tables, the
ADR-1197 amendment, the accounting old-to-new mapping, and the review-fold tests
(walk, importer and stage/index charge categories; unconditional owner join before
any ledger index; proof-phase range refusal before import; custodian mismatch
refusal).

Recorded, not delivered: the two-source `sources` conflict above (C2 input); the
decompression-amplification and split-UID runner-tree enabling blockers; mutation layers n1, rf and x1 left as
unproven for exact-head adjudication.

Open before #3441 can close: exact-head CI on the final candidate (the
maintainer-approved substitute for the local `gate:agent`); fresh reviewer, tester
and auditor passes over the fold range, including the remaining mutation
adjudication; and the mandatory E2E literal audit with `task test:e2e-lib` and
`task test:forge-fake` run at the final head. The PR refers to #3441 without a
closing keyword.

## Requirement map

The owner of worker verification is this run. MO ownership is the maintainer
or landing owner. “Incomplete” means the requirement has not been established
for the requested producer pipeline; existing groundwork alone does not
complete it.

| Clause | Required milestone / evidence | Delivery and owner |
|---|---|---|
| A1 terminal blocked/orphan preservation; legacy execution intact | U4b–U5, M3.1–2; old-cap red/green, stopped zero-mutation | Incomplete; run |
| A2 decoded uniqueness and within-operation bounds | U4b 12–16 | C1 selected-body, original-C and cache controls below; composed archive accounting remains C2/C4 work. |
| A3 durable bounded pacing | M3.3 virtual-clock suite, MO1 real roll | Incomplete; run + maintainer |
| A4 positive exact terminal custody | Groundwork + U5 unique-open listing matrix | Groundwork terminal-origin tests retained; U5 incomplete; run |
| A5 immutable evidence first; replay proofs | U4d 2, U5 10, N 1–2 | N seed implemented; producer handoff/boot ordering incomplete; run |
| A6 finite discovery/capture/fallback for every terminal status | W0 + composed gate including archive | W0 measured (see MEASURE lines); composed gate still incomplete; run, MO7 if unavailable |
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
| Effect addendum | R8, M3.4, U4b 21 | C1 import/no-op and per-effect pair controls below; broader freeze/archive effects remain C3/C4 work. |
| Restart discovery | U5 4b tests 7–8, 12–16 | Not delivered; run |
| Hash-attested WIP completeness | W0 tests 1–7 | W0 delivered (#3349) by `agent/test/git-maintenance-wip-capture-h.test.ts` (acceptance 1–11); run |
| Guard removal / enablement | Separate follow-up, MO1–4 | Deferred by binding guard-off decision; follow-up owner + maintainer |
| Item 11 shutdown joins | S acceptance and mutations, MO4 | Seed implemented; local evidence below; image acceptance unperformed; run + maintainer |

## Port and accounting mappings

### W0 port (#3349)

Historical W0-only port: W0-H replaced the capture's raw-tree input with
hash-attested plumbing and did not then replace a U4b helper. The later U4b
port below supersedes the copy-store assertions and retires the old object
snapshot file after exact inventory/order checks and 154 green cases.
No cap, timeout or test timeout changes.
`maintenanceWipInput` (the old raw-tree helper) is no longer called by the
capture. It is retained (no longer `private`) and its direct tests,
`git-maintenance-wip-input.test.ts`, are unchanged.

| Existing case | After W0 |
|---|---|
| `git-maintenance-wip-capture.test.ts`: fixture labeler | Maps write-tree, commit-tree and update-ref to `commit`, and adds pin, census and payload helper labels. |
| Same file: "dirty capture uses three matched 16 MiB raw metadata envelopes without reducing source caps" | Renamed "dirty capture needs no fixed raw metadata envelope and does not reduce source caps". Asserts commit succeeds with 196 MiB metadata pre-spent, zero old envelope launches, one payload pass, and sourceBytes nearly free. |
| Same file: "real tracked/untracked/binary WIP, ignored bytes and ordered owned effects" | New exact effect order: three `before:commit:dispatch`, each followed by a commit-group launch. The 192 MiB no-refund tail is replaced. |
| Same file: per-step config-join assertions for add and commit | Moved to the acceptance file's "config turned unsafe before write-tree/commit-tree/update-ref" cases. |
| Same file: "cached index stat cannot hide changed tracked bytes" (both) | Now refused with no add launch and the index byte-identical (stronger, R2). |
| Same file: "monotonic deadline between proof and dispatch refuses the effect" | `join:proof` is replaced by `join:pin` plus two `join:ls-files`. |
| Same file: "command failure <step>" | Intercepts by label. |
| Same file: "unsafe source and exhausted quota refuse without a Git effect" | The exhausted half asserts no add or commit launch, with HEAD and index unchanged. Status now runs before payload bytes are known; this is design-forced by P3. |
| Same file: all other cases | Unchanged. |
| `runner-maintenance-terminal-source.test.ts`: chargedTransports | 12/14 become 16/18 (two extra commit dispatches x 2 fetches). |
| `runner-maintenance-wip-boundary.test.ts`: "real owned boundary captures tracked/untracked/binary" | Phases now end `commit:prepare, commit:dispatch` x3. The classifier maps the commit group to `commit`. |

Accounting map for the W0 capture (the 65 launches of the measurement above):

| Counter | What is charged |
|---|---|
| launches | 1 per `execScoped` launch: metadata and config checks before status, add, write-tree, commit-tree and update-ref, at P0 and at the start of P5; pin reads around every launch, callback and stage; P0 plumbing; `ls-files` x2; `status` x2; `diff-index`; census x4 including race rechecks; payload; add; write-tree; `diff-tree` x2; commit-tree; pre-CAS `cat-file`; update-ref; P5 reads. |
| sourceEntries | Every Git output row; census 1 per path; payload 5 per path, the reservation for a regular file's open, fstat, read completion, post-read fstat and post-read lstat (no pre-open lstat: the opened fd must match the census identity before any read; a symlink's lstat, readlink and lstat are conservatively overcharged at 5); race recheck 1 per path; 1 for the `for-each-ref` row. |
| sourceBytes | Sum of current sizes, charged before the payload launch. |
| adminEntries | 20 x (clone depth + 3) per metadata check; config rows; distinct directories + clone depth + 1 per census, payload and recheck stage. |
| metadataBytes | Metadata-check precharge; all helper and Git output bytes; index size per pin read; sum of (path + 1 KiB) per stage. |
| rpcAttempts | Not charged by the capture. Caller fresh observations add about 8 per capture. |

### Earlier ports

At this historical capture the object-snapshot, private-verification and
original WIP cases remained. The current U4b replacement is mapped case by
case below; the original WIP cases remain.

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

C1/U4b implements the disposable-scratch, import, source-walk and disposal rows
below. Broader durable ledger/freeze/archive F work remains separately owned.
U2 removes fsync only for disposable private scratch that is never reused after a
crash. Existing durable origin/journal handling is unchanged. Independent auditor
and integration-gate results belong to the PR verification record.

| Operation | Invariant | C1 evidence / remaining owner |
|---|---|---|
| Durable ledger/origin/freeze create or write | Pinned parent; O_NOFOLLOW/O_EXCL; nlink checks; durable file fsync | Existing origin durability/fault controls retained; freeze/archive additions belong to C3/C4. |
| Disposable private view/pack/index create or write | Pinned parent; O_NOFOLLOW/O_EXCL; nlink1; dev/ino checks; no fsync required | Parent symlink, pre-created symlink, hardlinked write target, short-write retry and held-I/O tests in delta/port suites. |
| Scratch rename | Same pinned directory; final parent dev/ino recheck; no directory fsync required | Parent inode swap after destination-absence probe prevents rename dispatch. |
| Durable rename/promote | Same pinned directory; parent recheck and directory fsync | Existing producer-origin directory-sync controls retained; new archive promotion belongs to C4. |
| Canonical import | Pinned bare under lock; strict index-pack; no ref; post-import range/identity recheck | Direct and real-owner post-import failure tests retain imported and unknown bytes, source and custody, with no accepted output. |
| Source/staged-pack read | No-follow; pinned identity; complete-after-read check | Link/type/device/layout, descriptor/named swaps, short reads, late controls, corruption and cancellation cases. Excluded canonical closure is not rehashed. |
| Disposal | Only slot-created dev/ino-matched entries; finite and nonrecursive; uncertain entries retained | Foreign replacement names and entries survive; all admitted I/O/children/descriptor closes join before owned disposal. |

## Deferred

The approved guard-off decision reserves enablement for a follow-up after MO.
The planned provenance scheme's accepted shortfall covers legacy,
pre-upgrade, canonical-reseed, adopted and unrecorded sources; it supplies
no guarantee here because U5 is not delivered. Root loss/rollback and
downgrade residuals remain as recorded in ADR-1197. No ledger export or
restore artifact is added. Related issues #2852, #3101/#3132, #2969, #2884,
#2494, #2506 option A, #2544 and #3105 remain separately owned.

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
The historical source capture was non-shallow with 2,123 commits; that
observation is not a current capacity result. The W0 contract conflicts are resolved (#3349),
but representative composed measurement remains C2 work and archive handoff remains C4 work. MO7 is not a substitute for either delivery. Local environment limits and MO do not waive worker obligations.

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

### Historical S/N continuation observed evidence

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

### W0 (#3349) evidence

Focused W0 results, including `git-maintenance-wip-capture-h.test.ts`
acceptance 1 to 11 and the amended existing files, are recorded in the PR.
Gate results are likewise recorded there; none is claimed here.

## MEASURE lines

Final composed W0/U4b/archive capacity remains unvalidated. The U4b fixture
measurements later in this report cover the preservation path, not the C2
representative-repository capacity gate. The lines below are the historical **W0-only** measurement
(#3349), taken on a representative clone of this repository at base
`a1eba31831f9799a29857471514369d854ec8e72` (7,528 tracked files) with a
realistic WIP: 4 tracked edits, 2 deletions, 3 untracked files and one
staged-plus-unstaged file. It used a real slot with no injected allowances,
and W was byte-compared independently.

```
MEASURE sourceEntries=60315
MEASURE sourceBytes=135183189
MEASURE metadataBytes=40607905
MEASURE adminEntries=1684
MEASURE launches=65
MEASURE rpcAttempts=0
MEASURE elapsed=1354
```

- Rerun after the review fixes (P5 config check, five-unit payload reservation)
  in a Linux container (Node 24, Git 2.39.5) under the unchanged frozen caps:
  `sourceEntries` is 60,315 of 65,536. The in-run measurement before those fixes
  read sourceEntries=52786, adminEntries=1908, launches=62, elapsed=4241.
- `adminEntries` and `metadataBytes` scale with the clone path's depth, and
  `metadataBytes` varies slightly between runs.
- `rpcAttempts` is 0 because the test callbacks make no RPCs. The caller's
  R8 pairs add about 8 per capture (4 mutating dispatches x 2); this is
  accounting, not a measurement.
- The old capture on the same input refused with
  `maintenance_wip_input_refused` after about 1 s, having reserved its full
  64 MiB raw sourceBytes envelope.
- Frozen caps: sourceEntries 65,536; sourceBytes and metadataBytes 256 MiB
  each; adminEntries 4,096; launches 512; rpcAttempts 32.
- The composed W0+U4b+archive figure remains an **unverified projection**.
  No projected capacity is promoted to a measurement.
- The helpers keep one directory handle open per distinct directory. An
  `EMFILE` under a low `RLIMIT_NOFILE` refuses safely; the ceiling is
  unmeasured.

### Composed measurement harness (C2, #3442)

`agent/test/measure-3031-composed.test.ts` is the harness for the composed capacity
gate. A diagnostic run has been recorded (see "Official diagnostic run" below), but no
qualifying C2 measurement and no PASS exist, and only a recorded PASS releases C3. The producer guard stays `false`; the harness
changes no production code.

It drives one complete pipeline per measurement through the real `RunRunner` and the
real `RecoveryCoordinator` admission, in one charged slot with the production caps:
W0-H capture, `importMaintenanceTip` (walk, selection, pack, proof, import), then the
archive phase (`produceRecoveryBundle`, self-contained first, guarded thin fallback only
on `RecoveryBundleTooLargeError`). The writer-exclusion proof and the quiescence scans are
the real ones (three exclusion passes and the family proofs are charged inside the slot).
The harness interventions are these, and every MEASURE line prints all of them: the guard
method; a faked server behind the real strict client (the harness answers `hasFeature` and
`listRecoveryHolds` directly); `isIdle`, a harness check (sole execution in the container and
a settled runner, verified before admission, else UNRUN) passed to the real writer exclusion,
which does not prove the production Worker-idleness integration; and a refusal of every
`produceRecoveryBundle` call outside the archive window with a `RecoveryBundleTooLargeError`,
which models the runner's ordinary pre-slot capture refused as oversize (without it the ordinary
pass journals a real bundle, a saved-source candidate, and admission is refused as
`source_protected_or_ambiguous`; measured on the shared-layout shape-B fixture, not diagnosed
further). That suppression is not a Design-7 substitution, so
every run is `eligible_for_gate: false` ("pre-slot production bundle path suppressed by the
harness"), its counters are diagnostics, and the gate-level `c2_gate_summary` line is `UNRUN`
(no qualifying measurement is possible) regardless of the counters.
With the two-source family variant `UNRUN` at current production and the pre-slot suppression
non-qualifying, C2 cannot record a PASS at this code: this is an R3 stop-and-report for the
maintainers. Open reachability question, not a confirmed blocker and not something the harness
proves: production may refuse `worker_not_idle` if the failed run's lane is still active at
admission, which the harness's own sole-execution check does not exercise. The archive phase is
harness-placed: it has no production caller yet. Every limit is read from the admitted slot
reservation in the ledger, never from a table, and `decoded` follows the configured C. A
charged dimension with no admitted limit fails.

Run it as the container's main process on Linux, on the worker image's Git and Node,
from a full (non-shallow, at least 1,000 commits) checkout of this repository:

```sh
UZI_MEASURE_3031=1 UZI_MEASURE_DOCKER_HOST=tcp://<dind>:2375 \
  node --import tsx --test --test-timeout=120000 test/measure-3031-composed.test.ts > measure.out
```

- `UZI_MEASURE_DOCKER_HOST` is the Docker daemon the measurement is wired to. It must be a
  throwaway dind sidecar reachable only from the measurement container: an unauthenticated,
  TLS-off `tcp://...:2375` must never be exposed or shared, and it is never a real worker's
  daemon. It must have zero running containers. Only the exclusion proof is read-only
  (`GET /containers/json`, which requires zero running containers). The real quiescence
  scans are not: they may force-remove (`DELETE /containers/<id>?force=1&v=1`) containers
  that bind the fixture clone paths, and the `/proc` scan may SIGKILL in-scope processes.
  Unset, the run reports UNRUN.
- `TMPDIR` must be a case-sensitive, container-internal directory whose ancestors are not
  group-writable, world-writable or sticky. Otherwise UNRUN with the reason. This rule rejects
  the real Kubernetes `/data` layout (mode 2775), so the measurement runs on a scratch
  directory, not on `/data`. It is single-UID (`UZI_UID_SPLIT` unset); split-UID is not
  covered, and each MEASURE line records the mode.
- The measurement must be the only process in its container: the exclusive-idle scan and the harness
  idleness check refuse any other writer, so redirect output to a file (a `| tee` or `| grep` is a
  foreign process) and run it as PID 1 (or a child of PID 1 through the test runner; a deeper ancestry is `UNRUN`), not through a long-lived `docker exec` shell.
- `UZI_MEASURE_DEVIATION` states any difference from the worker image's environment. It
  becomes `elapsed_fidelity: "unverified: <text>"`, and a would-be PASS is then reported as
  `counters_within_limits_elapsed_unverified`. It never relaxes a limit or the slot deadline.

Output is one `MEASURE {json}` line per layout (shared, dissociated) by shape (A, B, C) and
per two-source family variant, a loose-object ladder above the shape-C count, and a ceiling
summary. Each line carries per-phase counters for every slot dimension, the limit compared
with, real `rpcAttempts`, elapsed time, RSS, Git and Node versions, substitutions, archive
kind, bytes and the per-bundle cap, and `count-objects -v` per fixture clone. Shape C uses
196 loose objects, the largest observed in a read-only sample of hosted worker attempt
clones (not a fleet maximum). A refusal (layout, quiescence, admission, resource exhaustion)
is recorded with its phase and the phases left unrun and is never a counter PASS. The
two-source family variant is reported UNRUN: at this commit no production path produces a two-member
family (the family list is `[genesis]` or the inherited list, which is built the same way), and the
measurement injects no member. This is a reachability conflict between #3442 Design 5 and current
production, left for maintainer disposition.

The verdict controls (`measure-3031-composed.test.ts`, "verdict controls") run without the
environment variable, on a small fixture, and are the only runs that stub exclusion and
quiescence; their lines are marked as not eligible for the gate.

#### Official diagnostic run (harness `91a8ad16`)

Environment: the rc.26 worker image `agent-base@sha256:74154d5bf9f738278b65db058c3116d74b8cc7dfababd33e1f2bdff46bccbf07`, Git 2.54.0, Node v24.20.0,
emulated linux/amd64 on an arm64 host, single-UID 10001 (split-UID not covered), candidate SHA
`91a8ad16b50c31618b6070cfdf7a30ad8f1078ed`. The harness ran the candidate's `agent/src` with
lockfile-installed `node_modules`, not the image's `/app` (rc.26 predates C1). Elapsed time is
unverified (emulation). Evidence: ten JSON lines extracted from the run output, sha256
`a03cd83f28afb82a85c1dac61aeb9898383ebda495c79db64ca54520bcbb582a`.

| Run | Verdict | Refused phase | sourceEntries (W0 share) | sources | adminEntries | rpcAttempts | launches | Transfer entered | Archive |
|---|---|---|---|---|---|---|---|---|---|
| shared-A | FAIL | after_walk | 65,536 (62,980) | 58 | 3,371 | 25 | 162 | yes | none |
| shared-B | FAIL | selection | 65,536 (62,745) | 70 | 3,341 | 25 | 167 | yes | none |
| shared-C | FAIL | walk | 65,536 (63,300) | 58 | 3,366 | 25 | 162 | yes | none |
| dissociated-A | FAIL (incomplete) | none attributed | 62,845 (partial) | 44 | 2,429 | 17 | 113 | no | none |
| dissociated-B | FAIL | selection | 65,536 (62,745) | 66 | 3,341 | 25 | 168 | yes | none |
| dissociated-C | FAIL | walk | 65,536 (63,300) | 58 | 3,366 | 25 | 162 | yes | none |
| shared-family, dissociated-family | UNRUN | not reached | n/a | n/a | n/a | n/a | n/a | no | none |

Results, stated exactly:

- C2 remains UNRUN. The gate summary is UNRUN with 0 of 8 runs eligible: every run depends on the
  suppressed pre-slot production bundle path.
- The two-source family variants are UNRUN: a two-member family is unreachable through production code.
- Five representative runs (shared A, B, C and dissociated B, C) show diagnostic `sourceEntries`
  refusals at 65,536; W0 alone uses 62,745 to 63,300.
- dissociated-A is incomplete and unattributed: the 120 s test timeout fired under emulation, the transfer
  never entered, no quota was violated, and its W0 counters are partial. Its `post_archive` label is a
  bookkeeping label, not evidence that archive execution occurred.
- No representative run reached an archive outcome. The small controls did produce self-contained archives.
- The loose-object ceiling is not established: 196 loose objects failed locally, which is not a proven
  universal ceiling.

These results are diagnostic only and are not C2 evidence. R3 conflicts for maintainer disposition: family
reachability, the pre-slot bundle suppression, and `sourceEntries` capacity. No redesign, waiver or
C3 to C8 unblock follows from them.


## U4b delivery (#3441), producer disabled

C1/U4b delivers the guarded delta-transfer implementation, ADR amendment, complete
test ports and focused implementation evidence on baseline
`76cd0a6638397d4e9be14c5f53a50192ac0b451e`. The producer guard remains false
(`agent/src/recovery.ts`, `TERMINAL_ORIGIN_ADMISSION_ENABLED`). All worker review,
mutation and full-gate obligations remain required; their frozen-revision results
belong to the PR, and are not replaced by these focused checks. This does not
deliver C2–C8, the complete #3031 pipeline, FINAL/release/cleanup authority or
producer enablement.

The contract below is shared with
[ADR-1197](../adr/1197-transient-recovery-park.md#amendment-2026-10-25-3441-u4b-verified-delta-transfer).
Its writer audit is owner-attributed: decision 9ab05c2f on owner source 735d3734; this
worker did not execute that owner-composed test. Baseline citations remain labelled;
current symbols identify the implementation in this code/ADR unit.

### U4b contract

C1/U4b implements the objects-only delta transfer with focused acceptance and
coordinator controls recorded in the delivery report. The terminal producer remains
disabled. Independent candidate reviews, mutation proofs and full integration gates
remain required and are reported in the PR; these focused results do not waive them.
C2–C8 and the complete #3031 producer pipeline are separate deliveries.
The guarded terminal producer transfers selected objects absent from an eligible
canonical boundary. TERMINAL_ORIGIN_ADMISSION_ENABLED remains false. The importer writes
objects, never refs. A successful imported or already_present result proves W is
connected relative to the trusted-boundary invariant; neither result grants FINAL,
release, cleanup, tracking/owed or accepted-ledger authority. The coordinator must join
the actual owner and prove final family continuity before a fresh observation and output
indexing. Real coordinator-to-runner-to-importer tests establish a fresh
complete-list/exact pair immediately before canonical import and after the actual owner
join before accepted output indexing, with no intervening metadata Git. A custody
refusal at either dispatch prevents its effect; a post-import range failure retains
imported and unknown bytes and prevents accepted output indexing. A crash after canonical import but
before maintenance-wip-v1 indexing leaves the tip imported in bare, not indexed; C6 owns
reconciliation.

The canonical bare, objects, pack and ancestors are no-follow dev/ino-pinned. Canonical
alternates/http-alternates are refused. Producer admission and post-proof recheck
require absent bare/shallow by lstat (any present entry/non-ENOENT error refuses), and
canonical rev-parse --is-shallow-repository=false. The query uses plain replacement
gitEnv, no GIT_SHALLOW_FILE or private object-directory overrides. Both checks are
charged even on already_present. Existing shallow-source writer behavior remains
unchanged and maintainer-owned.

All canonical ref namespaces depend on complete connected inputs, not only the three
eligible anchors. Fetch negotiation/connectivity excludes existing ref history in any
namespace. The eligible minimization anchors are exactly
`refs/remotes/origin/<defaultBranch>`, `refs/heads/<defaultBranch>` and
`refs/uzi-runner/<branch>`. recovered-source as default refuses. RECOVERY_BUNDLE_REF and
every other namespace are excluded. Candidates from original.baseCommit,
record.sourceSha and eligible tips become boundary only on positive bare-only ancestry.
Empty boundary refuses. A nonempty selection must uniquely include W. Empty selection
requires positive W-to-eligible-anchor ancestry and returns already_present after the
same shallow/identity/settlement checks, without pack/index/import/ref writes.
Non-anchor-reachable empty selection refuses. This resolves the
includes-tip/boundary-candidate conflict.

The clone object store is read in place under private controls, restricted environment
and sole pinned canonical alternate. No clone config, fetch-back, graph/MIDX
acceleration or hardlink trust is used. Every nlink>1 file refuses protected with
custody/source retained. Legitimate hardlinked clones therefore refuse; Git interrupted
between loose-object link() and unlink() can leave a permanent nlink2 refusal. The
bounded walk counts encoded entries without copying or rehashing excluded objects.

Selected object sizes are prechecked against inherited C; pack-objects cannot reuse
encoded objects/deltas. Strict staged indexing admits canonical prerequisites only as an
alternate, and pack membership must equal the selected OID set exactly. Every newly
staged selected body is decoded and hashed, even after canonical cache hits.
Same-operation positive reuse binds unchanged staged pack/index provenance plus
canonical identity; partial/failed/cancelled batches publish nothing. Ordinary
full-closure verification remains a full walk. The selected range is checked in
staged-only proof plus pinned canonical alternate, and again in canonical objects after
strict import. Post-import failure retains all imported and unknown objects,
source/custody, and returns no accepted result; no rollback/deletion or output/FINAL
authority follows.

The rev6 acceptance 4 full-prerequisite claim is conditionally replaced by the weaker
4/4a/4b contract: missing selected-range objects refuse before import; post-import range
failure may leave inert objects; missing/corrupt excluded closure is not proved by the
range walk. A defect may predate and persist across operations with directory identities
unchanged. No unbounded full walk is added to the maintenance slot.

For self-contained archives only, missing/unreadable excluded objects block bundle
creation before pack emission. The archive-backed FINAL proof requires
`selfContained === true` and an empty `prerequisiteShas` array
(`agent/src/recovery.ts`, the archive FINAL predicate); it does not verify pack bodies. Only RecoveryBundleTooLargeError permits guarded thin
fallback; thin never authorizes archive FINAL. Existing independently proved
held-publication/settled FINAL paths remain unchanged. This backstop does not detect
well-formed object bytes inconsistent with their names: pack-objects does not rehash and
FINAL reads bundle header. Same-uid trust remains. No agent core.fsync setting exists;
Git default committed,-loose-object permits host-crash loose-object loss and ref
rollback, with the same backstop limits. If #2506 option A authorizes release on
thin archives, re-review this invariant as authority-bearing.

Owner R8 clarification: read-only Git queries/config acquisition and supervised private
pack/index/decoded proof commands do not independently need remote pairs. They retain
original operation/slot/deadline/cancellation/accounting/pin checks; fixed private
per-file preparation remains deny-only, with no Git/network. Fresh complete-list/exact
pairs remain immediately before canonical strict import, accepted output indexing after
joined owner, producer-origin indexing/publication, journal temp creation/rename and
every other existing listed shared/durable/server-mutating effect. No Git metadata
acquisition occurs after the pair before its effect. All W0 pairs stay unchanged. Final
measured complete-path resource totals and both import/output custody-refusal traces are
required; no cap increase or omission is authorized.

Disposable private scratch is never reused after crash. O_NOFOLLOW/O_EXCL, nlink1,
dev/ino matching, pinned-parent rechecks before rename, full read-completion checks and
finite nonrecursive owned-only disposal remain mandatory. File/directory fsync is not
required by U2. Unknown entries and uncertain close/disposal identities are retained.
Scratch paths are borrowed, never journaled.

Current enforcing symbols are `GitCache.importMaintenanceTip` in `agent/src/git.ts`:
`literalBranch` and `valid` enforce input bounds; `pin`, `walk` and
`sourceUnchanged` enforce no-follow source/canonical identities; `shallow` performs the
canonical admission and recheck; the closed eligible-ref set, `parseOids`, `metadata`,
`stageMaintenancePack`, `indexMaintenancePack`, `verifyClosureFrames` and `range` enforce
selection, packing and proof. The ordinary `verifyRecoveryClosureBound` retains its
full walk. Read/proof commands remain charged supervised operations; zero read/proof
RPC pairs does not mean unmetered work.

The import dispatch pair follows family/config acquisition; only local deny checks,
artifact/parent checks and dispatch follow it. `admitTerminalOrigin` accepts the transfer
only after the runner's actual child join and final family proof, followed by fresh
observation/revalidation/indexing. Both import/output custody-refusal controls passed
through real coordinator/runner wiring. W0 pairs are unchanged. A successful no-op
still joins owner settlement before an accepted index; it writes no import/ref.

Disposable stage parent-mode equality is conditional on `options.disposable` and reuses
its already charged stat; the ordinary stage policy is unchanged. Importer `rename`
repeats `scratchCheck(to)` after target absence and before rename, charging each actual
parent stat as `sourceEntries`. The historical complete-path table below predates
the category correction; current measurements appear in
[the M2 correction](#m2-owner-diagnosis-and-category-correction). Neither fixture
is a C2 representative-repository capacity result.

### Complete-boundary writer induction and per-pinner acquisition chains

Owner decision 9ab05c2f (owner baseline 735d3734) resolves the previously reported R3
prerequisite by audit, not by a writer change. Owners report a composed test through
real worker code at origin/main 735d3734, substituting direct RecoveryCoordinator.pin
for the private runner journaling wrapper: failed fetchAgentBranch leaves an
unreferenced tip commit with missing ancestors; the journal records it; boot coverage
refuses with "recovery coverage proof failed" at its ancestry check, no coverage ref is
pinned and custody is retained. Owners traced the remaining writer chains to successful
fetch connectivity, connected synthesis or existing explicit full walks. No reachable
producer of a root missing its tree/blob was found. This is owner-reported evidence on
735d3734, not a test run by this worker or on its baseline 76cd0a66. Proceed under the
existing non-shallow precondition, record the three named residuals and leave live
writers unchanged; any OTHER unresolved writer gap still triggers R3.

The trusted-boundary invariant is conditional on initially healthy non-shallow
worker-owned storage, normal Git fetch connectivity and the stated same-uid/crash
residuals. It covers **all canonical ref namespaces**, including staging/transient refs;
eligible anchors are selection minimization only. Fetch negotiation/connectivity can
exclude history under every existing ref. Mere presence, journal authenticity, commit
type or ancestry alone is never the acquisition proof. The following acquisition table
is part of this ADR and the delivery/PR record.

Exhaustive tracked production-writer/caller sweeps covered
clone/fetch/update-ref/commit-tree/write-tree/index-pack, other import verbs, canonical
paths and constructed ref namespaces. The SHA/acquisition/quote column preserves
baseline line citations; the added current-pointer column maps unchanged source lines
into this working tree. Paths below are `agent/src/` unless stated; source baseline
76cd0a66 differs from owner test 735d3734.

| Writer / presence-only pinner | Baseline SHA source, acquisition chain and implementing quote | Current working-tree source sites |
|---|---|---|
| Initial cloneBare, heads/tags | `git.ts:15054` uses `["clone", "--bare", "-c",` (not mirror/no depth/filter). Successful ordinary full clone establishes initial population. Owner-required producer non-shallow checks exclude shallow-derived stores without changing this writer. | Source/caller symbols and quoted acquisition chain in the baseline-evidence column; production writers unchanged. |
| Warm fetch/origin/checkpoint refs | `git.ts:15085` `["fetch", "--prune", "origin"]`; `:15098` `["fetch", "origin", "+refs/uzi-checkpoints/*:refs/uzi-checkpoints/*"]`; exact task checkpoint `:2770` awaits fetch then verifies advertised SHA. Connectivity is relative to all existing refs, not unreferenced objects. | Source/caller symbols and quoted acquisition chain in the baseline-evidence column; production writers unchanged. |
| Publication/workflow/secret-scan/default temporary refs | Fetch writers `git.ts:2991/:11576–11577/:13032/:13258–13259` use `"fetch", "--refmap="` or `"fetch", "--", "origin"`, with explicitly constructed publication-floor/workflow-target/secret-scan-floor/origin-default destinations. Successful transport supplies closure before later semantic validation; abandoned refs remain induction roots. | Source/caller symbols and quoted acquisition chain in the baseline-evidence column; production writers unchanged. |
| fetchAgentBranch → staged incoming → promotion | `git.ts:4853–4854` supplies `"fetch", "--refmap=", "--no-tags",` and `"--no-write-fetch-head"` with file transport and branch-to-stage refspec; stage is `refs/uzi-incoming/` (:4849). Await transport before resolveCommitStrict (:4858); then `promoteTrackingUnderLock` writes runner tracking (:17282, `"update-ref", runnerTrackingRef(branch)`). Thrown transport bypasses incoming pinning; connected staged refs differ from unreferenced residue. | Source/caller symbols and quoted acquisition chain in the baseline-evidence column; production writers unchanged. |
| updateTrackingRef / bridge synthesis | Sole live adoption caller `runner.ts:14987`; before it, :14981–14982 execute `await this.git.scratchPublicationPreflight(barePath, branch, H);` and the same call for bridge. `git.ts:15461` constructs `["-c", "commit.gpgsign=false", "commit-tree", tipTree, "-p", tip]`; floors supply further parents (:15462), from acquired canonical published/checkpoint history. Existing full walk is `"rev-list", "--objects", "--missing=error", "--quiet", candidate` (:2919). Not merely requireOwedCommit. | Source/caller symbols and quoted acquisition chain in the baseline-evidence column; production writers unchanged. |
| Workflow wrappers / checkpoint-chain synthesis | `git.ts:10547` `["read-tree", realTip]`, :10565 `["read-tree", "--prefix=.github/workflows",`, :10569 `["write-tree"]`, :10577 `["-c", "commit.gpgsign=false", "commit-tree", newRoot]`, :10578 adds existing connected parents. realTip is imported canonical candidate; workflowSource is freshly fetched default (:10449), previous checkpoint is prior connected published/synthesized state. checkpointPack :9602–9603 explicitly preflights source and transformed candidate before publication, including any claim-provided prior checkpoint input. | Source/caller symbols and quoted acquisition chain in the baseline-evidence column; production writers unchanged. |
| anchorRecoveryHead | Sole caller `runner.ts:17137`. transferRestorePointToTrustedBare awaits fetchTracking (:17102; calls fetchAgentBranch at :14724 and rejects not_updated at :14727), verifies tracking coverage, then :17127 rejects `if (head === null \|\| head !== fetched.candidateSha) {` before anchoring its private clone HEAD. Writer `git.ts:5463` uses `["update-ref", pinRef, sha]`. No arbitrary present-object discovery. | `runner.ts:17141`, `runner.ts:17106`, `runner.ts:14728`, `runner.ts:14731`, `runner.ts:17131`, `git.ts:5463` |
| createQuarantineArchiveRef / uzi-archive run-generation pin | Sole caller `quarantine-archive.ts:124–127`: `const head = await git.trackingTip(barePath, branch);` then archive that exact head. trackingTip resolves runnerTrackingRef's commit (`git.ts:5017`), from successful fetch/promotion or preflighted bridge. Writer :5498 uses `["update-ref", ref, sha, ""]`. | Source/caller symbols and quoted acquisition chain in the baseline-evidence column; production writers unchanged. |
| pinCodeSnapshot | Capture `runner.ts:11833–11839` checks branch HEAD equality and `await this.requireTrackingOwned(flight, barePath, runnerClone.branch, head);`; defaultCommit comes from originBranchTip. Capture pin=false follows successful fetch (:11979–11982 or :13426–13432); writing call :13442 uses that captured head. Writer `git.ts:5193` `["update-ref", ref, head, ""]`. A previously acquired head may legitimately no longer be current tracking tip. | Source/caller symbols and quoted acquisition chain in the baseline-evidence column; production writers unchanged. |
| pinOwedUnderLock | All five call sites in `git.ts`: :4870 successfully staged incoming; :17239 displaced observed tracking SHA; :17244 incoming promoted SHA; :4973 conflicting ancestor's observed SHA; :17603 retained current observed SHA. Incoming follows fetch or preflighted bridge; others are exact ref/receipt-derived SHAs. Writer :17049 `["update-ref", ref, sha]`. :16885 `"cat-file", "-t", sha` is type evidence only. | Source/caller symbols and quoted acquisition chain in the baseline-evidence column; production writers unchanged. |
| retainCurrentOwedCandidate | Live callers `runner.ts:5168/:14441` supply trusted owed context. `git.ts:17596–17603` observes tracking under lock, requires matching owner run, and pins that observed SHA. Receipt :17103 checks `receipt.trackingSha === sha` and committed phase; :17107–17108 bind branch/run/generation/context. Attribution binds the ref; it does not certify arbitrary dangling objects. | Source/caller symbols and quoted acquisition chain in the baseline-evidence column; production writers unchanged. |
| clearConflictingAncestorTrackingRefs / uzi-archive branch-SHA pin | Only promotion calls it (`git.ts:17264`). :4968 observes each conflicting tracking ancestor, :4969 refuses unknown observation, :4975 records `sha: observed.sha`; :4973 optionally owed-pins same SHA. :4984 `["update-ref", archive, sha]` precedes :4986 deletion of the old tracking ref. Existing ref closure is copied, not manufactured by commit-type check. | Source/caller symbols and quoted acquisition chain in the baseline-evidence column; production writers unchanged. |
| pinSettlementRefs — source | `runner.ts:16814` requires successor seededFrom tracking/checkpoint; :16824 excludes guarded predecessor; :16831 `if (!(await this.git.isAncestorRef(barePath, pred.sourceSha, clone.baseCommit))) continue;`; :16832–16835 pins source/adopted. Source closure follows membership in independently complete adopted history, not journal authentication/ancestry alone. | Source/caller symbols and quoted acquisition chain in the baseline-evidence column; production writers unchanged. |
| pinSettlementRefs — adopted | Same call supplies clone.baseCommit. `git.ts:3991–3997/:4022–4054` chooses tracking/owner checkpoint refs; effectiveBase returned at :4502 may peel wrapper/marker to acquired ancestors (:4176/:4215–4233). Recovery override :7328–7329 binds episode pin (:7310–7311), explicitly closure-proved before creation (:7094–7097). | Source/caller symbols and quoted acquisition chain in the baseline-evidence column; production writers unchanged. |
| pinSettlementRefs — pushed | Sole writing caller `runner.ts:16979–16992` uses successfulPushedSha or valid completion body.head. Field set only after awaited pushBranch (:8519–8527) over tracking-owned publication.candidate (:8485). CompletionHead (:9944) derives from same success field (:9400–9402). Scope-capped zero-slice body (:7853–7857) is `report_only`, skipped by journalTerminalReport (:7113), and never reaches this pin. | Source/caller symbols and quoted acquisition chain in the baseline-evidence column; production writers unchanged. |
| pinSettlementRefs — published | `runner.ts:2988` maps pinPublished to `{ published: sha }`; `recovery-settlement.ts:820–822` checks source/adopted ancestry then pins. Sole producer `runner.ts:16895` supplies branch successfulPushedSha (:9321/:16913–16915) or checkpoint packed.tipOid only after publication ack AND successful pack-child exit (:16436–16457). checkpointPack full preflights (git.ts:9602–9603) establish transformed tree/parent closure before publication. Shared pin writer `git.ts:6146` uses `"update-ref"`; local commit presence is supplemental. | Source/caller symbols and quoted acquisition chain in the baseline-evidence column; production writers unchanged. |
| Recovery episode pin | `git.ts:7094`: `if (!await this.verifyRecoveryClosure(barePath, tip)) throw new Error("captured object closure is invalid");`, before episode update-ref :7097. Explicit full-closure verification supplies proof for later recovery-successor references. | Source/caller symbols and quoted acquisition chain in the baseline-evidence column; production writers unchanged. |
| buildRecoveryCoverage / coverage ref | `recovery.ts:1254–1261` adds originalSourceSha to roots; `git.ts:17781–17782` builds `"commit-tree", tree` over those parents; :17797 requires root ancestry or throws `"recovery coverage proof failed"`, before `"update-ref", coverageRef, sha` (:17804). **Owner audit/test resolves earlier concern:** reachable source chains are connected, or tested missing-ancestor failed-fetch root refuses before pin. This ancestry protection is incidental and NOT a tree/blob closure proof; no reachable producer of a root missing tree/blob was found. Keep the residual, not a claim that the builder independently proves closure. | Source/caller symbols and quoted acquisition chain in the baseline-evidence column; production writers unchanged. |
| RECOVERY_BUNDLE_REF / transient recovery bundle pin | `git.ts:13369` `["update-ref", RECOVERY_BUNDLE_REF, h]` precedes bundle production. Quarantine caller (:150) uses tracking-derived archive head; coordinator `recovery.ts:2771/:3970` supplies record.sourceSha from acquired/proved sources or connected coverage above. Owner failed-transfer composed test refuses before the coverage ref, so that partial root never reaches this transient pin. Never an eligible boundary anchor. | Source/caller symbols and quoted acquisition chain in the baseline-evidence column; production writers unchanged. |
| Explicit index-pack writers | `git.ts:5565` `["index-pack", "--stdin", "--strict", "--check-self-contained-and-connected"]` targets temporary bundle-verification bare; :14477 `"index-pack", "--strict"` indexes private staging; :14788 `spawnGitAsRunnerWithStdin(clonePath, ["index-pack", "--stdin"])` targets runner clone. Canonical transport imports are clone/fetch above. | Source/caller symbols and quoted acquisition chain in the baseline-evidence column; production writers unchanged. |
| Delete-only transaction | `git.ts:5998–5999` prepares `update-ref --stdin` with `delete`; deletes cannot enlarge closure. Include in exhaustive classification, not as acquisition. | Source/caller symbols and quoted acquisition chain in the baseline-evidence column; production writers unchanged. |
| importMaintenanceTip — objects only, never refs | Nonempty: selected exact-OID/decoded proof + trusted-boundary invariant → canonical import → bare-only range check and identity/control revalidation (`validate()`; the shallow query is checked before boundary formation and at the post-proof recheck preceding import, and is not re-run after import) → scoped imported result. Empty: eligible-anchor ancestry proof and same admission/rechecks → already_present. Owner joins/fresh observation precede accepted output index. Failed post-import residue has no accepted result; successful connected but not-yet-indexed imports are legitimate unnamed objects, not forbidden merely for lacking a ref. | Source/caller symbols and quoted acquisition chain in the baseline-evidence column; production writers unchanged. |

**Three named residuals required in ADR-1197 and PR:** (1) the trusted-boundary
invariant relies on fetch connectivity checks and existing explicit walks;
buildRecoveryCoverage's protection against partial roots is incidental (ancestry), not a
tree/blob closure proof; (2) failed agent-branch fetch may leave unreferenced partial
objects in the bare—fetch does not trust unreferenced objects; (3) maintainers track
writer hardening separately and this child changes none of those writers. The owners'
tested failure retained custody; do not generalize it into an independently verified
full-closure guarantee for the builder.

**Residue versus provenance:** failed-fetch unreferenced objects and U4b
post-import-failure residue are not trusted by presence/type or a journal alone and
authorize no pin/output/FINAL/release/cleanup. Successful
fetch-connected/synthesized/full-walk-verified objects can be connected but unnamed
(verified import before its index write, preflighted bridge, prior verified snapshot
head). Current pinners use the acquisition chains above; do not add a rule banning
legitimate unnamed verified objects. A new caller/acquisition path not covered here
needs its own proof; any other unresolved writer gap is still R3. The ordinary-journal →
coverage concern is resolved by owner audit/test evidence, not waived by plan approval
or repaired in this child.

Automatic maintenance: `git.ts:4745–4746` sets `maintenance.auto=false` and `gc.auto=0`;
per-invocation gitEnv pins at :19714–19715 reinforce them. `:4674` `await git(["repack",
"-a", "-d", "-q"]);` is runner-clone materialization only. Tracked searches found no
explicit canonical repack/prune or agent core.fsync setting; no other issue is cited as
establishing the invariant.

### Canonical history-decoration writer audit

Record in audit/ADR: at baseline 76cd0a66 no tracked production writer creates canonical
`info/grafts` or `refs/replace/*`. Tests do create replacements in temporary bare
fixtures (`recovery-guarded-producer.test.ts:277`,
`repo-instructions-at-commit.test.ts:142`), so do not say “no writer” without the
production qualification.

Exhaustive tracked grep: `git grep -n -i -e graft -e 'refs/replace' -e 'refs\\/replace'
-e GIT_NO_REPLACE_OBJECTS -e GIT_REPLACE_REF_BASE -e '"replace"' -e "'replace'"`;
complete hits at `.uzi/scratch/point7-exhaustive-grep.txt`. Destination/plumbing
enumeration: `git grep -n -E
'update-ref|symbolic-ref|pack-refs|fetch.*refspec|refs/\\*|refs/heads/\\*|--mirror|fast-import|unpack-objects'
-- agent/src`; evidence at `.uzi/scratch/point7-writer-hits.txt` and expanded variants
`.uzi/scratch/point7-expanded-hits.txt`.

Matches are defensive env pins (`git.ts:2441` `GIT_GRAFT_FILE: "/dev/null"`, :13556
`GIT_REPLACE_REF_BASE: "refs/disabled/"`), clone-side refusal (:11931), tests/docs or
unrelated replacement. Clone is bare, not mirror (:15054); refspecs are heads→origin
(:15059), checkpoints (:15098), and branch→incoming (:4853–4854). External/same-uid
actors and config remain trust residuals.



Current production-only repeat: `git grep -n -i -e graft -e 'refs/replace' -e
'refs\/replace' -e GIT_NO_REPLACE_OBJECTS -e GIT_REPLACE_REF_BASE -e '"replace"' -e
"'replace'" -- agent/src` returned defensive pins, refusals and comments, with no
production history-decoration writer found. The current hits are
`agent/src/git.ts:1101`, `:2439`, `:2441`, `:2511`, `:2512`, `:5528`, `:5529`, `:5567`,
`:7020`, `:7021`, `:7022`, `:11500`, `:11502`, `:11677`, `:11872`, `:11945`, `:12344`,
`:12658`, `:12664`, `:13683`, `:13684`, `:14516`, `:14517` and `:16547`. Verdict for
each listed site: already accurate defensive configuration, refusal or explanatory
comment; none creates canonical grafts/replacements. The broad historical
plumbing/ref-path/filesystem sweeps are recorded in
`.uzi/scratch/point7-exhaustive-grep.txt`, `point7-writer-hits.txt`,
`point7-expanded-hits.txt`, `point7-ref-path-hits.txt`, `point7-filesystem-writers.txt`
and `point7-other-writers.txt`. Current repeat evidence:
`.uzi/scratch/u4b-history-decoration-production-grep.txt` and
`.uzi/scratch/docs-u4b-current-history-grep.txt`. These scratch files are supporting run
evidence; the findings, acquisition chains and limits are recorded here and in the
delivery record.

The unchanged-writers comparison records `captureMaintenanceWip`, `cloneBare`,
`disableAutoMaintenance`, `fetchAgentBranch`, `updateTrackingRef`, `anchorRecoveryHead`
and `pinSettlementRefs` unchanged against 76cd0a66. No production writer or W0-H change
is part of U4b. Same-uid actors, storage loss, shallow-source writers and separate
maintainer hardening remain outside this change.

### U4b accounting inventory

No W0-H charge changes. Original sourceBytes reservation is conservative st_size
capacity without refund, not a bound on subsequent reads. All actual reads separately
reserve requested capacity, including EOF and retries. Encoded reads, emitted artifact
bytes, metadata/control capacity, graph traversal rows and decoded bodies remain
distinct.

| Site / operation | Prior dormant composition | Delta importer | Current source sites (agent/src/) |
|---|---|---|---|
| Literal worker branch validation / anchor strings | No delta-boundary literal validation in the dormant composition | `literalBranch` reserves `metadataBytes = 10 * UTF8 bytes + 1024` per input name, then validates literal Git-ref syntax without a local name cap or Git launch. Exact eligible-ref membership preserves the closed list, including Unicode whitespace. | `git.ts`: `literalBranch` and eligible ref-row parsing |
| Public ordinary family options | Existing ordinary reader/lock/qualification | Copies only `signal` and `maxSources`; ignores a caller's extra private adapter property as before. No new RPC, launch or filesystem charge. Only the importer supplies the private config reader. | `git.ts`: `matchMaintenanceSourceFamily`, `withMaintenanceFamilyLock` |
| Producer locked-family reader filesystem validation | Ordinary scanner preserves its original accounting | Only the private importer adapter charges every source ancestor, canonical-parent and clone lstat attempt to `sourceEntries` plus bounded path `metadataBytes`; its existing canonical-directory enumeration entry charge changes from `adminEntries` to `sourceEntries`. Failures/retries consume the same attempt charge; config Git launch retains existing launch/output metering. | `git.ts`: `withMaintenanceFamilyLock`, producer-only `chargeSource` and enumeration branch |
| importMaintenanceTip valid/childPath/pin | Snapshot source/output derived bounds and copy-store setup | Bounded absolute/component checks reserve metadataBytes8448 per attempt; source/canonical stat/open/fstat pins charge sourceEntries plus path capacity; shared roots pinned once per transfer | `git.ts`: importer `valid`, `charge`, `stat`, `fstat`, `pin` |
| walk opendir/read/stat/open/fstat/recheck | Copy source and full canonical stores | In-place clone-only sourceEntries per actual lookup/validation;255 sourceBytes per raw name request including EOF; each regular st_size precharged separately; alternates reads reserve remaining requested sourceBytes including short-read retries/EOF; no object-store copy | `git.ts`: importer `walk`, `sourceUnchanged`, alternate read loop |
| bare shallow lstat initial/postproof | Absent | sourceEntries1 plus bounded path/stat metadataBytes per attempt, including ENOENT/failure; present entries never opened | `git.ts`: importer `shallow`, `absent`, `stat` |
| canonical shallow rev-parse initial/postproof | Absent | launches1 via existing canonical execution path, stdout/stderr metadataBytes; no manual duplicate launch, no private/GIT_SHALLOW_FILE overrides | `git.ts`: importer `shallow` / existing `runGit` metering |
| canonical config/family and anchor/ancestry commands | Full source/base snapshot plus private full closure | Existing launches/metadata metering preserved; new private canonical raw NUL rows sourceEntries before filtering via explicit sixth runGit argument; ordinary/W0 rows retain adminEntries; supervised local currency/pins before/after; no read-only RPC pair | `git.ts`: importer `supervised`, `readCanonicalConfig`, `launch` |
| selection parseOids and range rows | Full-closure rev-list | closureEntries per selected/range row and bounded metadata capacity; no excluded-closure walk | `git.ts`: importer `parseOids`, `range` |
| declared selected metadata | Full-closure batch metadata | closureEntries per row, metadata stdout/input reservations; decoded C precheck excludes only same-operation positively verified artifact provenance | `git.ts`: importer `metadata` |
| disposable private mkdir/control write | Copied object-store administrative hierarchy | adminEntries for private mkdir and exclusive HEAD/config creation; existing-name/fd/parent validation and each retry sourceEntries; known control artifactBytes reserved before open, once across short writes | `git.ts`: importer `mkdir`, `write`, `scratchCheck` |
| private own/cleanup registry | Owned snapshot entries | Finite sourceEntries parent/identity/unlink/rmdir cleanup reserves at registration, including directory/control and staged/renamed entries; original separate own calls and amounts retained; never canonical object rollback | `git.ts`: importer `own`, `closePins`, `cleanup` |
| stageMaintenancePack | Isolated self-contained staging regular operations adminEntries | Ordinary default unchanged; importer disposable branch regular staged-file checks/open/cleanup and parent checks/reservations sourceEntries; emitted chunks artifactBytes once, retry only unwritten tail; closureEntries pack header count | `git.ts`: `stageMaintenancePack` disposable branch, write loop, header readback |
| indexMaintenancePack | Isolated environment; input/output file checks adminEntries | Ordinary default unchanged; importer binding regular input/output/cleanup and parent checks/reservations sourceEntries; same Imax envelope artifactBytes, encoded sourceBytes, fanout/OID closureEntries; canonical prerequisites are not extra pack members | `git.ts`: `indexMaintenancePack` producer binding, metered read, exact OID comparison, cleanup |
| staged checksum/read/rename | Snapshot proof artifacts | Actual regular file operations sourceEntries;20-byte checksum sourceBytes; pinned-parent sourceEntries, no disposable fsync | `git.ts`: importer checksum read, `rename`, `scratchCheck` |
| decoded selected proof | Full closure decoded hash verification | Same operation-wide unique decoded budget; no clone/prior-operation trust; every new artifact body verified despite canonical cache hits; publish only complete settled identity-checked batches | `git.ts`: importer proof phase, `verifyClosureFrames`, cache publication |
| canonical strict import | No wired importer | launches once, artifact file stat/open/fstat/read sourceEntries; sourceBytes requested streaming capacity, metadata output; fresh complete-list/exact pair rpcAttempts2 immediately before dispatch | `git.ts`: importer import phase, stream loop; `recovery.ts`: guarded `beforeSemantic` |
| final owner settlement/output index | Dormant transfer seam | Joined owner/final family proof and current fresh/revalidate/index wiring; fresh output pair prevents indexing on custody refusal; W0 pairs unchanged, no read/proof pairs | `runner.ts`: transfer owner settlement; `recovery.ts`: joined continuation and `maintenance-wip-v1` index |

The stage parent-mode equality is disposable-only and reuses its existing stat
(`stageMaintenancePack` disposable parent check); no extra filesystem charge is invented. The repeated final pre-rename
scratchCheck in importer `rename` does perform additional actual parent stats, now charged
sourceEntries in importer `scratchCheck`. Both repeated checks remain. These operations
were absent from the earlier dirty fixture measurement.

The following table is **HISTORICAL**, before the category correction and the
failing wrapped diagnosis. Its measurements are retained; current wrapped results
appear in the M2 correction below.

| Slot dimension | Cap | Dirty import | Already present | Import custody refusal | Output custody refusal | Post-import range failure |
|---|---|---:|---:|---:|---:|---:|
| `launches` | 512 | 200 | 93 | 189 | 200 | 192 |
| `metadataBytes` | 256 MiB | 31,778,887 | 17,293,788 | 28,330,918 | 31,770,585 | 30,021,407 |
| `adminEntries` | 4,096 | 3,983 | 1,612 | 3,800 | 3,983 | 3,874 |
| `sources` | 128 | 126 | 70 | 114 | 126 | 118 |
| `rpcAttempts` | 32 | 18 | 8 | 16 | 18 | 16 |
| `sourceEntries` | 65,536 | 11,204 | 6,246 | 9,791 | 11,204 | 10,536 |
| `sourceBytes` | 256 MiB | 200,996 | 164,468 | 200,424 | 200,836 | 200,947 |
| `artifactBytes` | 256 MiB | 2,044 | 142 | 2,042 | 2,042 | 2,040 |
| `closureEntries` | 65,536 | 18 | 0 | 16 | 18 | 16 |
| `decoded` | 8 GiB (tighter inherited C still applies) | 478 | 0 | 478 | 478 | 478 |


These historical observations are actual worker-run complete preservation-path fixtures: the four main
columns are from `gate-log.AY6aQz` (762 affected tests passed, no skips), and the
post-import fault column is from `gate-log.7DrNmN` (nine focused follow-up cases
passed). They include W0 and the coordinator's origin index, journal temp create,
rename and accepted-output effects. Each path additionally uses 4 discovery RPCs
(2 pairs) outside the charged slot. Total physical RPCs/pairs are therefore:

| Path | Charged slot RPCs / pairs | Discovery RPCs / pairs | Complete physical RPCs / pairs | Accepted WIP output |
|---|---:|---:|---:|---|
| Dirty import | 18 / 9 | 4 / 2 | 22 / 11 | yes, only after joined owner |
| Already present | 8 / 4 | 4 / 2 | 12 / 6 | yes, with no pack/import/ref write |
| Import custody refusal | 16 / 8 | 4 / 2 | 20 / 10 | no import or WIP index |
| Output custody refusal | 18 / 9 | 4 / 2 | 22 / 11 | import retained; no WIP index |
| Post-import range failure | 16 / 8 | 4 / 2 | 20 / 10 | imported/unknown bytes retained; no WIP index |
| Final-family / changed-head refusal | 16 / 8 | 4 / 2 | 20 / 10 | no WIP index |
| Shallow file inserted after proof | 14 / 7 | 4 / 2 | 18 / 9 | fixed layout refusal; no import/index |

Reads and private proof commands add zero remote pairs, as asserted by actual
supervised-dispatch traces. Import and accepted indexing each follow their own
new complete-list/exact pair without intervening metadata Git. Import/output
custody refusals prevent their respective effects. The historical dirty fixture had only
113 admin entries and 2 source descriptors left: those were tight margins, not a
representative repository capacity proof. Quiescence is injected and trusted
ancestor DAC is explicitly modeled; this is not live DAC, the C2 measurement
gate, or the W0/U4b/archive pipeline. C2 owns representative measurement using
the phase seam. No cap, timeout, original reservation or W0-H charge changed.
The earlier F8Q09H figure remains historical pre-final-rename evidence, not the
historical table. Timing, cache warmth and path lengths can affect observed byte
reservations; the recorded figures are observations, not universal costs.

### U4b complete test-port inventory

The two tables below contain every expanded baseline key, one row per key: 123 O cases
in `agent/test/git-maintenance-snapshot-ports.test.ts` and 31 P cases in
`agent/test/git-maintenance-private-verification.test.ts`. Exact key/order equivalence
was checked against baseline 76cd0a66 before deleting
`agent/test/git-maintenance-object-snapshot.test.ts`, and the combined 154-case run
passed before retirement. Excluded-base copying and full excluded-closure proof
assertions are explicitly superseded by the in-place/range contract; title equivalence
alone does not preserve those old semantics. Phase callbacks in fixtures are test
observations, not production authority.

#### Snapshot case-for-case port map (#3441)

Titles below are historical inventory keys. The assertion column states the approved
in-place/delta replacement; no row promises copying or a full excluded-closure proof.
All 123 keys and registration order were checked mechanically against the unchanged
original before retirement. The combined O/P run passed 154 cases in O8JtAH; final
runtime/equivalence review and mutation evidence remain pending.

| Original expanded key | Destination expanded title | Preserved / superseded assertion |
|---|---|---|
| `derived source directory paths enforce 4096 bytes: true` | `git-maintenance-snapshot-ports.test.ts`: `ported snapshot: derived source directory paths enforce 4096 bytes: true` | Preserved absolute4096-byte derived-directory guards before lookup/create. Positive observes real bounded traversal/create and stops locally before longer controls; no tip/authority accepted. |
| `derived destination directory paths enforce 4096 bytes: true` | `git-maintenance-snapshot-ports.test.ts`: `ported snapshot: derived destination directory paths enforce 4096 bytes: true` | Preserved absolute4096-byte derived-directory guards before lookup/create. Positive observes real bounded traversal/create and stops locally before longer controls; no tip/authority accepted. |
| `derived source directory paths enforce 4096 bytes: false` | `git-maintenance-snapshot-ports.test.ts`: `ported snapshot: derived source directory paths enforce 4096 bytes: false` | Preserved absolute4096-byte derived-directory guards before lookup/create. Positive observes real bounded traversal/create and stops locally before longer controls; no tip/authority accepted. |
| `derived destination directory paths enforce 4096 bytes: false` | `git-maintenance-snapshot-ports.test.ts`: `ported snapshot: derived destination directory paths enforce 4096 bytes: false` | Preserved absolute4096-byte derived-directory guards before lookup/create. Positive observes real bounded traversal/create and stops locally before longer controls; no tip/authority accepted. |
| `derived regular source file paths cannot exceed 4096 bytes` | `git-maintenance-snapshot-ports.test.ts`: `ported snapshot: derived regular source file paths cannot exceed 4096 bytes` | Preserved derived regular-file bound before open/rename; source bytes retained. Destination targets actual staged-to-final pack name, replacing copy output. |
| `derived regular destination file paths cannot exceed 4096 bytes` | `git-maintenance-snapshot-ports.test.ts`: `ported snapshot: derived regular destination file paths cannot exceed 4096 bytes` | Preserved derived regular-file bound before open/rename; source bytes retained. Destination targets actual staged-to-final pack name, replacing copy output. |
| `real shared clone descendants traverse offline through completed private source and base` | `git-maintenance-snapshot-ports.test.ts`: `ported snapshot: real shared clone descendants traverse offline through completed private source and base` | Real shared clone imports selected bodies and canonical prerequisites, ignores poisoned clone config, preserves all source identities/bytes, no fetch/upload-pack or refs. Canonical independence superseded by pinned alternate. |
| `known base omits non-authoritative source alternates file without opening` | `git-maintenance-snapshot-ports.test.ts`: `ported snapshot: known base omits non-authoritative source alternates file without opening` | Superseded omission with strict file/link/FIFO layout refusal per control parameter. External target never followed/read; source/custody retained; zeroGit/pairs. |
| `known base omits non-authoritative source alternates symlink without opening` | `git-maintenance-snapshot-ports.test.ts`: `ported snapshot: known base omits non-authoritative source alternates symlink without opening` | Superseded omission with strict file/link/FIFO layout refusal per control parameter. External target never followed/read; source/custody retained; zeroGit/pairs. |
| `known base omits non-authoritative source alternates FIFO without opening` | `git-maintenance-snapshot-ports.test.ts`: `ported snapshot: known base omits non-authoritative source alternates FIFO without opening` | Superseded omission with strict file/link/FIFO layout refusal per control parameter. External target never followed/read; source/custody retained; zeroGit/pairs. |
| `known base omits non-authoritative source http-alternates file without opening` | `git-maintenance-snapshot-ports.test.ts`: `ported snapshot: known base omits non-authoritative source http-alternates file without opening` | Superseded omission with strict file/link/FIFO layout refusal per control parameter. External target never followed/read; source/custody retained; zeroGit/pairs. |
| `known base omits non-authoritative source http-alternates symlink without opening` | `git-maintenance-snapshot-ports.test.ts`: `ported snapshot: known base omits non-authoritative source http-alternates symlink without opening` | Superseded omission with strict file/link/FIFO layout refusal per control parameter. External target never followed/read; source/custody retained; zeroGit/pairs. |
| `known base omits non-authoritative source http-alternates FIFO without opening` | `git-maintenance-snapshot-ports.test.ts`: `ported snapshot: known base omits non-authoritative source http-alternates FIFO without opening` | Superseded omission with strict file/link/FIFO layout refusal per control parameter. External target never followed/read; source/custody retained; zeroGit/pairs. |
| `known base refuses bad identity and never hands off a partial source` | `git-maintenance-snapshot-ports.test.ts`: `ported snapshot: known base refuses bad identity and never hands off a partial source` | Preserved parameterized path/identity/control/type/private collision refusal before Git/accepted handoff. badidentity substitutes actual pinned canonical objects; ordinaryFIFO targets required canonical pack pin (arbitrary excluded base files no longer walked); sibling targets private proof, retains foreign entry (EEXIST). |
| `known base refuses relative and never hands off a partial source` | `git-maintenance-snapshot-ports.test.ts`: `ported snapshot: known base refuses relative and never hands off a partial source` | Preserved parameterized path/identity/control/type/private collision refusal before Git/accepted handoff. badidentity substitutes actual pinned canonical objects; ordinaryFIFO targets required canonical pack pin (arbitrary excluded base files no longer walked); sibling targets private proof, retains foreign entry (EEXIST). |
| `known base refuses unnormalized and never hands off a partial source` | `git-maintenance-snapshot-ports.test.ts`: `ported snapshot: known base refuses unnormalized and never hands off a partial source` | Preserved parameterized path/identity/control/type/private collision refusal before Git/accepted handoff. badidentity substitutes actual pinned canonical objects; ordinaryFIFO targets required canonical pack pin (arbitrary excluded base files no longer walked); sibling targets private proof, retains foreign entry (EEXIST). |
| `known base refuses outside and never hands off a partial source` | `git-maintenance-snapshot-ports.test.ts`: `ported snapshot: known base refuses outside and never hands off a partial source` | Preserved parameterized path/identity/control/type/private collision refusal before Git/accepted handoff. badidentity substitutes actual pinned canonical objects; ordinaryFIFO targets required canonical pack pin (arbitrary excluded base files no longer walked); sibling targets private proof, retains foreign entry (EEXIST). |
| `known base refuses nested and never hands off a partial source` | `git-maintenance-snapshot-ports.test.ts`: `ported snapshot: known base refuses nested and never hands off a partial source` | Preserved parameterized path/identity/control/type/private collision refusal before Git/accepted handoff. badidentity substitutes actual pinned canonical objects; ordinaryFIFO targets required canonical pack pin (arbitrary excluded base files no longer walked); sibling targets private proof, retains foreign entry (EEXIST). |
| `known base refuses basename and never hands off a partial source` | `git-maintenance-snapshot-ports.test.ts`: `ported snapshot: known base refuses basename and never hands off a partial source` | Preserved parameterized path/identity/control/type/private collision refusal before Git/accepted handoff. badidentity substitutes actual pinned canonical objects; ordinaryFIFO targets required canonical pack pin (arbitrary excluded base files no longer walked); sibling targets private proof, retains foreign entry (EEXIST). |
| `known base refuses repos symlink and never hands off a partial source` | `git-maintenance-snapshot-ports.test.ts`: `ported snapshot: known base refuses repos symlink and never hands off a partial source` | Preserved parameterized path/identity/control/type/private collision refusal before Git/accepted handoff. badidentity substitutes actual pinned canonical objects; ordinaryFIFO targets required canonical pack pin (arbitrary excluded base files no longer walked); sibling targets private proof, retains foreign entry (EEXIST). |
| `known base refuses bare symlink and never hands off a partial source` | `git-maintenance-snapshot-ports.test.ts`: `ported snapshot: known base refuses bare symlink and never hands off a partial source` | Preserved parameterized path/identity/control/type/private collision refusal before Git/accepted handoff. badidentity substitutes actual pinned canonical objects; ordinaryFIFO targets required canonical pack pin (arbitrary excluded base files no longer walked); sibling targets private proof, retains foreign entry (EEXIST). |
| `known base refuses objects symlink and never hands off a partial source` | `git-maintenance-snapshot-ports.test.ts`: `ported snapshot: known base refuses objects symlink and never hands off a partial source` | Preserved parameterized path/identity/control/type/private collision refusal before Git/accepted handoff. badidentity substitutes actual pinned canonical objects; ordinaryFIFO targets required canonical pack pin (arbitrary excluded base files no longer walked); sibling targets private proof, retains foreign entry (EEXIST). |
| `known base refuses unsafe repos and never hands off a partial source` | `git-maintenance-snapshot-ports.test.ts`: `ported snapshot: known base refuses unsafe repos and never hands off a partial source` | Preserved parameterized path/identity/control/type/private collision refusal before Git/accepted handoff. badidentity substitutes actual pinned canonical objects; ordinaryFIFO targets required canonical pack pin (arbitrary excluded base files no longer walked); sibling targets private proof, retains foreign entry (EEXIST). |
| `known base refuses unsafe bare and never hands off a partial source` | `git-maintenance-snapshot-ports.test.ts`: `ported snapshot: known base refuses unsafe bare and never hands off a partial source` | Preserved parameterized path/identity/control/type/private collision refusal before Git/accepted handoff. badidentity substitutes actual pinned canonical objects; ordinaryFIFO targets required canonical pack pin (arbitrary excluded base files no longer walked); sibling targets private proof, retains foreign entry (EEXIST). |
| `known base refuses unsafe objects and never hands off a partial source` | `git-maintenance-snapshot-ports.test.ts`: `ported snapshot: known base refuses unsafe objects and never hands off a partial source` | Preserved parameterized path/identity/control/type/private collision refusal before Git/accepted handoff. badidentity substitutes actual pinned canonical objects; ordinaryFIFO targets required canonical pack pin (arbitrary excluded base files no longer walked); sibling targets private proof, retains foreign entry (EEXIST). |
| `known base refuses alternates and never hands off a partial source` | `git-maintenance-snapshot-ports.test.ts`: `ported snapshot: known base refuses alternates and never hands off a partial source` | Preserved parameterized path/identity/control/type/private collision refusal before Git/accepted handoff. badidentity substitutes actual pinned canonical objects; ordinaryFIFO targets required canonical pack pin (arbitrary excluded base files no longer walked); sibling targets private proof, retains foreign entry (EEXIST). |
| `known base refuses http-alternates and never hands off a partial source` | `git-maintenance-snapshot-ports.test.ts`: `ported snapshot: known base refuses http-alternates and never hands off a partial source` | Preserved parameterized path/identity/control/type/private collision refusal before Git/accepted handoff. badidentity substitutes actual pinned canonical objects; ordinaryFIFO targets required canonical pack pin (arbitrary excluded base files no longer walked); sibling targets private proof, retains foreign entry (EEXIST). |
| `known base refuses control symlink and never hands off a partial source` | `git-maintenance-snapshot-ports.test.ts`: `ported snapshot: known base refuses control symlink and never hands off a partial source` | Preserved parameterized path/identity/control/type/private collision refusal before Git/accepted handoff. badidentity substitutes actual pinned canonical objects; ordinaryFIFO targets required canonical pack pin (arbitrary excluded base files no longer walked); sibling targets private proof, retains foreign entry (EEXIST). |
| `known base refuses control FIFO and never hands off a partial source` | `git-maintenance-snapshot-ports.test.ts`: `ported snapshot: known base refuses control FIFO and never hands off a partial source` | Preserved parameterized path/identity/control/type/private collision refusal before Git/accepted handoff. badidentity substitutes actual pinned canonical objects; ordinaryFIFO targets required canonical pack pin (arbitrary excluded base files no longer walked); sibling targets private proof, retains foreign entry (EEXIST). |
| `known base refuses control directory and never hands off a partial source` | `git-maintenance-snapshot-ports.test.ts`: `ported snapshot: known base refuses control directory and never hands off a partial source` | Preserved parameterized path/identity/control/type/private collision refusal before Git/accepted handoff. badidentity substitutes actual pinned canonical objects; ordinaryFIFO targets required canonical pack pin (arbitrary excluded base files no longer walked); sibling targets private proof, retains foreign entry (EEXIST). |
| `known base refuses ordinary FIFO and never hands off a partial source` | `git-maintenance-snapshot-ports.test.ts`: `ported snapshot: known base refuses ordinary FIFO and never hands off a partial source` | Preserved parameterized path/identity/control/type/private collision refusal before Git/accepted handoff. badidentity substitutes actual pinned canonical objects; ordinaryFIFO targets required canonical pack pin (arbitrary excluded base files no longer walked); sibling targets private proof, retains foreign entry (EEXIST). |
| `known base refuses existing sibling and never hands off a partial source` | `git-maintenance-snapshot-ports.test.ts`: `ported snapshot: known base refuses existing sibling and never hands off a partial source` | Preserved parameterized path/identity/control/type/private collision refusal before Git/accepted handoff. badidentity substitutes actual pinned canonical objects; ordinaryFIFO targets required canonical pack pin (arbitrary excluded base files no longer walked); sibling targets private proof, retains foreign entry (EEXIST). |
| `known base refuses sibling symlink and never hands off a partial source` | `git-maintenance-snapshot-ports.test.ts`: `ported snapshot: known base refuses sibling symlink and never hands off a partial source` | Preserved parameterized path/identity/control/type/private collision refusal before Git/accepted handoff. badidentity substitutes actual pinned canonical objects; ordinaryFIFO targets required canonical pack pin (arbitrary excluded base files no longer walked); sibling targets private proof, retains foreign entry (EEXIST). |
| `both-store final revalidation: source mutation` | `git-maintenance-snapshot-ports.test.ts`: `ported snapshot: both-store final revalidation: source mutation` | Preserved source/base x mutation/replacement/root substitution final check. Base file contents replaced by pinned canonical config control, whose original custody bytes remain; excluded object bodies not rehashed. No Git or accepted output. |
| `both-store final revalidation: source replacement` | `git-maintenance-snapshot-ports.test.ts`: `ported snapshot: both-store final revalidation: source replacement` | Preserved source/base x mutation/replacement/root substitution final check. Base file contents replaced by pinned canonical config control, whose original custody bytes remain; excluded object bodies not rehashed. No Git or accepted output. |
| `both-store final revalidation: source root replacement` | `git-maintenance-snapshot-ports.test.ts`: `ported snapshot: both-store final revalidation: source root replacement` | Preserved source/base x mutation/replacement/root substitution final check. Base file contents replaced by pinned canonical config control, whose original custody bytes remain; excluded object bodies not rehashed. No Git or accepted output. |
| `both-store final revalidation: base mutation` | `git-maintenance-snapshot-ports.test.ts`: `ported snapshot: both-store final revalidation: base mutation` | Preserved source/base x mutation/replacement/root substitution final check. Base file contents replaced by pinned canonical config control, whose original custody bytes remain; excluded object bodies not rehashed. No Git or accepted output. |
| `both-store final revalidation: base replacement` | `git-maintenance-snapshot-ports.test.ts`: `ported snapshot: both-store final revalidation: base replacement` | Preserved source/base x mutation/replacement/root substitution final check. Base file contents replaced by pinned canonical config control, whose original custody bytes remain; excluded object bodies not rehashed. No Git or accepted output. |
| `both-store final revalidation: base root replacement` | `git-maintenance-snapshot-ports.test.ts`: `ported snapshot: both-store final revalidation: base root replacement` | Preserved source/base x mutation/replacement/root substitution final check. Base file contents replaced by pinned canonical config control, whose original custody bytes remain; excluded object bodies not rehashed. No Git or accepted output. |
| `second store cannot renew shared sourceBytes and cleans both copies` | `git-maintenance-snapshot-ports.test.ts`: `ported snapshot: second store cannot renew shared sourceBytes and cleans both copies` | Original shared resource counter exhausted after walk before next phase. Actual exhaustion observed, no refund/renewal, owned scratch disposed, source/custody retained. No second copied store under new contract. |
| `second store cannot renew shared sourceEntries and cleans both copies` | `git-maintenance-snapshot-ports.test.ts`: `ported snapshot: second store cannot renew shared sourceEntries and cleans both copies` | Original shared resource counter exhausted after walk before next phase. Actual exhaustion observed, no refund/renewal, owned scratch disposed, source/custody retained. No second copied store under new contract. |
| `second store cannot renew shared adminEntries and cleans both copies` | `git-maintenance-snapshot-ports.test.ts`: `ported snapshot: second store cannot renew shared adminEntries and cleans both copies` | Original shared resource counter exhausted after walk before next phase. Actual exhaustion observed, no refund/renewal, owned scratch disposed, source/custody retained. No second copied store under new contract. |
| `second store cannot renew shared metadataBytes and cleans both copies` | `git-maintenance-snapshot-ports.test.ts`: `ported snapshot: second store cannot renew shared metadataBytes and cleans both copies` | Original shared resource counter exhausted after walk before next phase. Actual exhaustion observed, no refund/renewal, owned scratch disposed, source/custody retained. No second copied store under new contract. |
| `second store cannot renew shared artifactBytes and cleans both copies` | `git-maintenance-snapshot-ports.test.ts`: `ported snapshot: second store cannot renew shared artifactBytes and cleans both copies` | Original shared resource counter exhausted after walk before next phase. Actual exhaustion observed, no refund/renewal, owned scratch disposed, source/custody retained. No second copied store under new contract. |
| `a hostile declared alternate contributes no real Git objects` | `git-maintenance-snapshot-ports.test.ts`: `ported snapshot: a hostile declared alternate contributes no real Git objects` | Hostile alternate real object remains absent from canonical; target never opened, source control retained, zeroGit/pairs. |
| `both stores retain alternate metadata consistency: control mutation` | `git-maintenance-snapshot-ports.test.ts`: `ported snapshot: both stores retain alternate metadata consistency: control mutation` | Parameterized control mutation/replacement/type/late-base insertion refuses at source/canonical pin revalidation; no Git/output. Canonical alternate no longer omitted. |
| `both stores retain alternate metadata consistency: control replacement` | `git-maintenance-snapshot-ports.test.ts`: `ported snapshot: both stores retain alternate metadata consistency: control replacement` | Parameterized control mutation/replacement/type/late-base insertion refuses at source/canonical pin revalidation; no Git/output. Canonical alternate no longer omitted. |
| `both stores retain alternate metadata consistency: source control directory` | `git-maintenance-snapshot-ports.test.ts`: `ported snapshot: both stores retain alternate metadata consistency: source control directory` | Parameterized control mutation/replacement/type/late-base insertion refuses at source/canonical pin revalidation; no Git/output. Canonical alternate no longer omitted. |
| `both stores retain alternate metadata consistency: late base control` | `git-maintenance-snapshot-ports.test.ts`: `ported snapshot: both stores retain alternate metadata consistency: late base control` | Parameterized control mutation/replacement/type/late-base insertion refuses at source/canonical pin revalidation; no Git/output. Canonical alternate no longer omitted. |
| `second-store pending read settles before revocation cleanup` | `git-maintenance-snapshot-ports.test.ts`: `ported snapshot: second-store pending read settles before revocation cleanup` | Actual alternate read/private HEAD write held while cancellation occurs; no early descriptor close/cleanup/return; released work joined, finite owned cleanup, source/custody retained. |
| `second-store pending write settles before revocation cleanup` | `git-maintenance-snapshot-ports.test.ts`: `ported snapshot: second-store pending write settles before revocation cleanup` | Actual alternate read/private HEAD write held while cancellation occurs; no early descriptor close/cleanup/return; released work joined, finite owned cleanup, source/custody retained. |
| `both stores consume the original logical source quota` | `git-maintenance-snapshot-ports.test.ts`: `ported snapshot: both stores consume the original logical source quota` | Both logical source reservations consume original quota;127 pre-spent refuses2, zero continuation/Git/output. |
| `derived base sibling is bounded before concatenation` | `git-maintenance-snapshot-ports.test.ts`: `ported snapshot: derived base sibling is bounded before concatenation` | Retired base sibling maps to longer derived private control path. First bounded directory created; overlong child refused before filesystem call; cleanup/retention verified. |
| `detaches actual loose objects, pack/index and nested encoded bytes without source writes or launches` | `git-maintenance-snapshot-ports.test.ts`: `ported snapshot: detaches actual loose objects, pack/index and nested encoded bytes without source writes or launches` | Real loose + valid pack/index and allowed commitgraph/MIDX bytes remain identical with inode/mtime/ctime/nlink; graph/MIDX never read. In-place walk creates no copy/Git. Unknown nested bytes now separately refuse and remain retained. |
| `accepts bounded caller-family paths beyond canonical two-level shape` | `git-maintenance-snapshot-ports.test.ts`: `ported snapshot: accepts bounded caller-family paths beyond canonical two-level shape` | Bounded deep caller path admitted through real encoded walk only; later family metadata qualification remains strict. No accepted tip/ref/output. |
| `refuses wrong identity` | `git-maintenance-snapshot-ports.test.ts`: `ported snapshot: refuses wrong identity` | Preserved each source identity/path/output/name pre-effect refusal, zeroGit/pairs and retained custody/source. Occupied output EEXIST is not deleted. |
| `refuses rounded identity` | `git-maintenance-snapshot-ports.test.ts`: `ported snapshot: refuses rounded identity` | Preserved each source identity/path/output/name pre-effect refusal, zeroGit/pairs and retained custody/source. Occupied output EEXIST is not deleted. |
| `refuses relative source` | `git-maintenance-snapshot-ports.test.ts`: `ported snapshot: refuses relative source` | Preserved each source identity/path/output/name pre-effect refusal, zeroGit/pairs and retained custody/source. Occupied output EEXIST is not deleted. |
| `refuses outside source` | `git-maintenance-snapshot-ports.test.ts`: `ported snapshot: refuses outside source` | Preserved each source identity/path/output/name pre-effect refusal, zeroGit/pairs and retained custody/source. Occupied output EEXIST is not deleted. |
| `refuses unnormalized source` | `git-maintenance-snapshot-ports.test.ts`: `ported snapshot: refuses unnormalized source` | Preserved each source identity/path/output/name pre-effect refusal, zeroGit/pairs and retained custody/source. Occupied output EEXIST is not deleted. |
| `refuses relative output` | `git-maintenance-snapshot-ports.test.ts`: `ported snapshot: refuses relative output` | Preserved each source identity/path/output/name pre-effect refusal, zeroGit/pairs and retained custody/source. Occupied output EEXIST is not deleted. |
| `refuses outside output` | `git-maintenance-snapshot-ports.test.ts`: `ported snapshot: refuses outside output` | Preserved each source identity/path/output/name pre-effect refusal, zeroGit/pairs and retained custody/source. Occupied output EEXIST is not deleted. |
| `refuses root output` | `git-maintenance-snapshot-ports.test.ts`: `ported snapshot: refuses root output` | Preserved each source identity/path/output/name pre-effect refusal, zeroGit/pairs and retained custody/source. Occupied output EEXIST is not deleted. |
| `refuses existing output` | `git-maintenance-snapshot-ports.test.ts`: `ported snapshot: refuses existing output` | Preserved each source identity/path/output/name pre-effect refusal, zeroGit/pairs and retained custody/source. Occupied output EEXIST is not deleted. |
| `refuses unsafe name` | `git-maintenance-snapshot-ports.test.ts`: `ported snapshot: refuses unsafe name` | Preserved each source identity/path/output/name pre-effect refusal, zeroGit/pairs and retained custody/source. Occupied output EEXIST is not deleted. |
| `rejects source ancestor` | `git-maintenance-snapshot-ports.test.ts`: `ported snapshot: rejects source ancestor` | Preserved each symlink/node/rawname attack without following/writing target; hardlink uses explicit alias reason+nlink2, source bytes and canonical custody retained, zeroGit/pairs. |
| `rejects clone` | `git-maintenance-snapshot-ports.test.ts`: `ported snapshot: rejects clone` | Preserved each symlink/node/rawname attack without following/writing target; hardlink uses explicit alias reason+nlink2, source bytes and canonical custody retained, zeroGit/pairs. |
| `rejects .git` | `git-maintenance-snapshot-ports.test.ts`: `ported snapshot: rejects .git` | Preserved each symlink/node/rawname attack without following/writing target; hardlink uses explicit alias reason+nlink2, source bytes and canonical custody retained, zeroGit/pairs. |
| `rejects objects` | `git-maintenance-snapshot-ports.test.ts`: `ported snapshot: rejects objects` | Preserved each symlink/node/rawname attack without following/writing target; hardlink uses explicit alias reason+nlink2, source bytes and canonical custody retained, zeroGit/pairs. |
| `rejects leaf` | `git-maintenance-snapshot-ports.test.ts`: `ported snapshot: rejects leaf` | Preserved each symlink/node/rawname attack without following/writing target; hardlink uses explicit alias reason+nlink2, source bytes and canonical custody retained, zeroGit/pairs. |
| `rejects target parent` | `git-maintenance-snapshot-ports.test.ts`: `ported snapshot: rejects target parent` | Preserved each symlink/node/rawname attack without following/writing target; hardlink uses explicit alias reason+nlink2, source bytes and canonical custody retained, zeroGit/pairs. |
| `rejects hardlink` | `git-maintenance-snapshot-ports.test.ts`: `ported snapshot: rejects hardlink` | Preserved each symlink/node/rawname attack without following/writing target; hardlink uses explicit alias reason+nlink2, source bytes and canonical custody retained, zeroGit/pairs. |
| `rejects FIFO` | `git-maintenance-snapshot-ports.test.ts`: `ported snapshot: rejects FIFO` | Preserved each symlink/node/rawname attack without following/writing target; hardlink uses explicit alias reason+nlink2, source bytes and canonical custody retained, zeroGit/pairs. |
| `rejects raw invalid UTF8` | `git-maintenance-snapshot-ports.test.ts`: `ported snapshot: rejects raw invalid UTF8` | Preserved each symlink/node/rawname attack without following/writing target; hardlink uses explicit alias reason+nlink2, source bytes and canonical custody retained, zeroGit/pairs. |
| `alternates refuses empty without reading control bytes` | `git-maintenance-snapshot-ports.test.ts`: `ported snapshot: alternates refuses empty without reading control bytes` | Preserved control x initial/late/handoff insertion crossproduct. Empty/link/directory controls not read; actual completed walk/proof insertion blocks every later Git/import, source/custody/control bytes retained. |
| `alternates refuses link without reading control bytes` | `git-maintenance-snapshot-ports.test.ts`: `ported snapshot: alternates refuses link without reading control bytes` | Preserved control x initial/late/handoff insertion crossproduct. Empty/link/directory controls not read; actual completed walk/proof insertion blocks every later Git/import, source/custody/control bytes retained. |
| `alternates refuses directory without reading control bytes` | `git-maintenance-snapshot-ports.test.ts`: `ported snapshot: alternates refuses directory without reading control bytes` | Preserved control x initial/late/handoff insertion crossproduct. Empty/link/directory controls not read; actual completed walk/proof insertion blocks every later Git/import, source/custody/control bytes retained. |
| `alternates refuses late insertion without reading control bytes` | `git-maintenance-snapshot-ports.test.ts`: `ported snapshot: alternates refuses late insertion without reading control bytes` | Preserved control x initial/late/handoff insertion crossproduct. Empty/link/directory controls not read; actual completed walk/proof insertion blocks every later Git/import, source/custody/control bytes retained. |
| `alternates refuses handoff insertion without reading control bytes` | `git-maintenance-snapshot-ports.test.ts`: `ported snapshot: alternates refuses handoff insertion without reading control bytes` | Preserved control x initial/late/handoff insertion crossproduct. Empty/link/directory controls not read; actual completed walk/proof insertion blocks every later Git/import, source/custody/control bytes retained. |
| `http-alternates refuses empty without reading control bytes` | `git-maintenance-snapshot-ports.test.ts`: `ported snapshot: http-alternates refuses empty without reading control bytes` | Preserved control x initial/late/handoff insertion crossproduct. Empty/link/directory controls not read; actual completed walk/proof insertion blocks every later Git/import, source/custody/control bytes retained. |
| `http-alternates refuses link without reading control bytes` | `git-maintenance-snapshot-ports.test.ts`: `ported snapshot: http-alternates refuses link without reading control bytes` | Preserved control x initial/late/handoff insertion crossproduct. Empty/link/directory controls not read; actual completed walk/proof insertion blocks every later Git/import, source/custody/control bytes retained. |
| `http-alternates refuses directory without reading control bytes` | `git-maintenance-snapshot-ports.test.ts`: `ported snapshot: http-alternates refuses directory without reading control bytes` | Preserved control x initial/late/handoff insertion crossproduct. Empty/link/directory controls not read; actual completed walk/proof insertion blocks every later Git/import, source/custody/control bytes retained. |
| `http-alternates refuses late insertion without reading control bytes` | `git-maintenance-snapshot-ports.test.ts`: `ported snapshot: http-alternates refuses late insertion without reading control bytes` | Preserved control x initial/late/handoff insertion crossproduct. Empty/link/directory controls not read; actual completed walk/proof insertion blocks every later Git/import, source/custody/control bytes retained. |
| `http-alternates refuses handoff insertion without reading control bytes` | `git-maintenance-snapshot-ports.test.ts`: `ported snapshot: http-alternates refuses handoff insertion without reading control bytes` | Preserved control x initial/late/handoff insertion crossproduct. Empty/link/directory controls not read; actual completed walk/proof insertion blocks every later Git/import, source/custody/control bytes retained. |
| `no producer token or discovery scope can authorize a snapshot` | `git-maintenance-snapshot-ports.test.ts`: `ported snapshot: no producer token or discovery scope can authorize a snapshot` | No token/discovery cannot authorize importer; zeroGit/pairs/output, source/custody retained. |
| `rejects changed source: mutation` | `git-maintenance-snapshot-ports.test.ts`: `ported snapshot: rejects changed source: mutation` | Mutation/replacement/growth after actual alternate read and late earlier-control/directory replacement after walk invalidate pins; every case reached; no Git/output. Opaque object bodies no longer copied. |
| `rejects changed source: replacement` | `git-maintenance-snapshot-ports.test.ts`: `ported snapshot: rejects changed source: replacement` | Mutation/replacement/growth after actual alternate read and late earlier-control/directory replacement after walk invalidate pins; every case reached; no Git/output. Opaque object bodies no longer copied. |
| `rejects changed source: late earlier leaf` | `git-maintenance-snapshot-ports.test.ts`: `ported snapshot: rejects changed source: late earlier leaf` | Mutation/replacement/growth after actual alternate read and late earlier-control/directory replacement after walk invalidate pins; every case reached; no Git/output. Opaque object bodies no longer copied. |
| `rejects changed source: late directory replacement` | `git-maintenance-snapshot-ports.test.ts`: `ported snapshot: rejects changed source: late directory replacement` | Mutation/replacement/growth after actual alternate read and late earlier-control/directory replacement after walk invalidate pins; every case reached; no Git/output. Opaque object bodies no longer copied. |
| `rejects changed source: growth` | `git-maintenance-snapshot-ports.test.ts`: `ported snapshot: rejects changed source: growth` | Mutation/replacement/growth after actual alternate read and late earlier-control/directory replacement after walk invalidate pins; every case reached; no Git/output. Opaque object bodies no longer copied. |
| `original shared quota precharges sourceBytes` | `git-maintenance-snapshot-ports.test.ts`: `ported snapshot: original shared quota precharges sourceBytes` | Original slot precharges resource before private file create/write. Zero private file opens/writes and Git; original token cannot renew. |
| `original shared quota precharges sourceEntries` | `git-maintenance-snapshot-ports.test.ts`: `ported snapshot: original shared quota precharges sourceEntries` | Original slot precharges resource before private file create/write. Zero private file opens/writes and Git; original token cannot renew. |
| `original shared quota precharges adminEntries` | `git-maintenance-snapshot-ports.test.ts`: `ported snapshot: original shared quota precharges adminEntries` | Original slot precharges resource before private file create/write. Zero private file opens/writes and Git; original token cannot renew. |
| `original shared quota precharges metadataBytes` | `git-maintenance-snapshot-ports.test.ts`: `ported snapshot: original shared quota precharges metadataBytes` | Original slot precharges resource before private file create/write. Zero private file opens/writes and Git; original token cannot renew. |
| `original shared quota precharges artifactBytes` | `git-maintenance-snapshot-ports.test.ts`: `ported snapshot: original shared quota precharges artifactBytes` | Original slot precharges resource before private file create/write. Zero private file opens/writes and Git; original token cannot renew. |
| `a different output path cannot renew the original artifact quota` | `git-maintenance-snapshot-ports.test.ts`: `ported snapshot: a different output path cannot renew the original artifact quota` | Opaque transport35-byte PACK consumes remaining artifact quota once; second path refuses and cleans owned second output; first bytes/source/custody retained, no authentication/import authority. |
| `source DAC refusal never repairs permissions or launches a privileged reader` | `git-maintenance-snapshot-ports.test.ts`: `ported snapshot: source DAC refusal never repairs permissions or launches a privileged reader` | Actual no-follow source alternate open EACCES is propagated internally; no chmod/privileged helper/Git or accepted result; source bytes/custody retained. Producer coordinator sanitizes failure separately. |
| `source request accounting includes raw names, EOF and short read capacity` | `git-maintenance-snapshot-ports.test.ts`: `ported snapshot: source request accounting includes raw names, EOF and short read capacity` | Actual source alternate short reads reserve remaining capacity through EOF; one rawinfo entry+EOF each255; conservative st_size distinct. Consume exact remaining original quota, next byte refuses. |
| `partial writes retry only unwritten tail and charge the chunk once` | `git-maintenance-snapshot-ports.test.ts`: `ported snapshot: partial writes retry only unwritten tail and charge the chunk once` | Opaque PACK abc chunk retries offsets[0,1,2] with lengths[3,2,1]; active file/parent counters advance each retry; emitted chunk charged once; bytes exact; disposable fsync0. No verification/import authority. |
| `settles serial filesystem I/O and cleans owned output: zero write` | `git-maintenance-snapshot-ports.test.ts`: `ported snapshot: settles serial filesystem I/O and cleans owned output: zero write` | Each held real source read/private control write (zero/fault/cancel) prevents return, close and cleanup until released; all settled, only owned output removed, source/custody retained. |
| `settles serial filesystem I/O and cleans owned output: read fault` | `git-maintenance-snapshot-ports.test.ts`: `ported snapshot: settles serial filesystem I/O and cleans owned output: read fault` | Each held real source read/private control write (zero/fault/cancel) prevents return, close and cleanup until released; all settled, only owned output removed, source/custody retained. |
| `settles serial filesystem I/O and cleans owned output: write fault` | `git-maintenance-snapshot-ports.test.ts`: `ported snapshot: settles serial filesystem I/O and cleans owned output: write fault` | Each held real source read/private control write (zero/fault/cancel) prevents return, close and cleanup until released; all settled, only owned output removed, source/custody retained. |
| `settles serial filesystem I/O and cleans owned output: read cancel` | `git-maintenance-snapshot-ports.test.ts`: `ported snapshot: settles serial filesystem I/O and cleans owned output: read cancel` | Each held real source read/private control write (zero/fault/cancel) prevents return, close and cleanup until released; all settled, only owned output removed, source/custody retained. |
| `settles serial filesystem I/O and cleans owned output: write cancel` | `git-maintenance-snapshot-ports.test.ts`: `ported snapshot: settles serial filesystem I/O and cleans owned output: write cancel` | Each held real source read/private control write (zero/fault/cancel) prevents return, close and cleanup until released; all settled, only owned output removed, source/custody retained. |
| `cleanup preserves a foreign replacement name` | `git-maintenance-snapshot-ports.test.ts`: `ported snapshot: cleanup preserves a foreign replacement name` | Write-failure replaces private HEAD with foreign bytes; original moved artifact and literal foreign name survive finite nonrecursive disposal; source/custody retained. |
| `modeled supported split-worker 2775 strict ancestor keeps private output` | `git-maintenance-snapshot-ports.test.ts`: `ported snapshot: modeled supported split-worker 2775 strict ancestor keeps private output` | Modeled worker10001/runner10002 ownership with strict2775 ancestor; real import succeeds and private controls700/600. Explicit stat/uid model, not live DAC. |
| `refuses unsupported modeled parent profile {"split":false,"mode":1533}` | `git-maintenance-snapshot-ports.test.ts`: `ported snapshot: refuses unsupported modeled parent profile {"split":false,"mode":1533}` | Every unsupported UIDsplit/group/world-write ancestor profile refuses before Git/output; no permission repair or source/custody loss. Explicit model. |
| `refuses unsupported modeled parent profile {"split":true,"mode":1533,"gid":10002}` | `git-maintenance-snapshot-ports.test.ts`: `ported snapshot: refuses unsupported modeled parent profile {"split":true,"mode":1533,"gid":10002}` | Every unsupported UIDsplit/group/world-write ancestor profile refuses before Git/output; no permission repair or source/custody loss. Explicit model. |
| `refuses unsupported modeled parent profile {"split":true,"mode":1535}` | `git-maintenance-snapshot-ports.test.ts`: `ported snapshot: refuses unsupported modeled parent profile {"split":true,"mode":1535}` | Every unsupported UIDsplit/group/world-write ancestor profile refuses before Git/output; no permission repair or source/custody loss. Explicit model. |
| `refuses a source info change during the last control probe` | `git-maintenance-snapshot-ports.test.ts`: `ported snapshot: refuses a source info change during the last control probe` | Source info insertion during final actual absent-http control probe2 invalidates frozen directory before later Git/output; inserted bytes retained. Probe count adapted from retired copy helper3. |
| `new private directories retain harmless inherited setgid` | `git-maintenance-snapshot-ports.test.ts`: `ported snapshot: new private directories retain harmless inherited setgid` | Actual inherited2700 scratch dirs retain harmless sgid; regular control600; positive import/no refs and source/custody retention. |
| `refuses unexpected bits on a newly created directory: 4700` | `git-maintenance-snapshot-ports.test.ts`: `ported snapshot: refuses unexpected bits on a newly created directory: 4700` | Each newly created private directory unexpected special-bit mode refuses before Git; source/custody retained. |
| `refuses unexpected bits on a newly created directory: 1700` | `git-maintenance-snapshot-ports.test.ts`: `ported snapshot: refuses unexpected bits on a newly created directory: 1700` | Each newly created private directory unexpected special-bit mode refuses before Git; source/custody retained. |
| `private root never inherits the group-writable ancestor exception` | `git-maintenance-snapshot-ports.test.ts`: `ported snapshot: private root never inherits the group-writable ancestor exception` | Recovery private root770 does not receive strict-ancestor exception; zeroGit/output, source/custody retained. |
| `fixed refusal strips cause and properties: maintenance_resource_exhausted` | `git-maintenance-snapshot-ports.test.ts`: `ported snapshot: fixed refusal strips cause and properties: maintenance_resource_exhausted` | Actual staging helper sanitizes callback errors: exact3 maintenance reasons preserved, arbitrary/suffixed prose mapped fixed staging refusal, no enumerable properties/cause/stderr. Retired snapshot reason superseded. |
| `fixed refusal strips cause and properties: maintenance_token_invalid` | `git-maintenance-snapshot-ports.test.ts`: `ported snapshot: fixed refusal strips cause and properties: maintenance_token_invalid` | Actual staging helper sanitizes callback errors: exact3 maintenance reasons preserved, arbitrary/suffixed prose mapped fixed staging refusal, no enumerable properties/cause/stderr. Retired snapshot reason superseded. |
| `fixed refusal strips cause and properties: maintenance_clock_invalid` | `git-maintenance-snapshot-ports.test.ts`: `ported snapshot: fixed refusal strips cause and properties: maintenance_clock_invalid` | Actual staging helper sanitizes callback errors: exact3 maintenance reasons preserved, arbitrary/suffixed prose mapped fixed staging refusal, no enumerable properties/cause/stderr. Retired snapshot reason superseded. |
| `fixed refusal strips cause and properties: maintenance_resource_exhausted private` | `git-maintenance-snapshot-ports.test.ts`: `ported snapshot: fixed refusal strips cause and properties: maintenance_resource_exhausted private` | Actual staging helper sanitizes callback errors: exact3 maintenance reasons preserved, arbitrary/suffixed prose mapped fixed staging refusal, no enumerable properties/cause/stderr. Retired snapshot reason superseded. |
| `fixed refusal strips cause and properties: private diagnostic` | `git-maintenance-snapshot-ports.test.ts`: `ported snapshot: fixed refusal strips cause and properties: private diagnostic` | Actual staging helper sanitizes callback errors: exact3 maintenance reasons preserved, arbitrary/suffixed prose mapped fixed staging refusal, no enumerable properties/cause/stderr. Retired snapshot reason superseded. |
| `short write revalidates active source before retry` | `git-maintenance-snapshot-ports.test.ts`: `ported snapshot: short write revalidates active source before retry` | Actual importer staged short-write invalidates each named active source/destination/ancestor mode pin; second write blocked, no later Git/import/pair; all children/source/custody retained. Producer-only parent-mode predicate repaired; ordinary helper default unchanged. |
| `short write revalidates active destination before retry` | `git-maintenance-snapshot-ports.test.ts`: `ported snapshot: short write revalidates active destination before retry` | Actual importer staged short-write invalidates each named active source/destination/ancestor mode pin; second write blocked, no later Git/import/pair; all children/source/custody retained. Producer-only parent-mode predicate repaired; ordinary helper default unchanged. |
| `short write revalidates source ancestor before retry` | `git-maintenance-snapshot-ports.test.ts`: `ported snapshot: short write revalidates source ancestor before retry` | Actual importer staged short-write invalidates each named active source/destination/ancestor mode pin; second write blocked, no later Git/import/pair; all children/source/custody retained. Producer-only parent-mode predicate repaired; ordinary helper default unchanged. |
| `short write revalidates destination ancestor before retry` | `git-maintenance-snapshot-ports.test.ts`: `ported snapshot: short write revalidates destination ancestor before retry` | Actual importer staged short-write invalidates each named active source/destination/ancestor mode pin; second write blocked, no later Git/import/pair; all children/source/custody retained. Producer-only parent-mode predicate repaired; ordinary helper default unchanged. |

#### Private-verification port map

Baseline original: 76cd0a6638397d4e9be14c5f53a50192ac0b451e. All 31 exact expanded keys
and original ordering verified by port-inventory.mjs. The earlier 31-case pass in
gate-log.ybeHU3 is superseded by the complete O8JtAH run: all 31 P cases, including
canonical-poison refusal, passed together with all 123 O cases. Red/green mutation
evidence remains M2, not claimed here.

Destination file for every row: agent/test/git-maintenance-private-verification.test.ts.
Borrowed artifact callbacks are test-only fixed-phase observations and never change
importMaintenanceTip result/wire state. Canonical full closure exclusions use the
approved invariant/range amendment; ordinary verification keeps full walk.

| Original expanded key | Destination exact key | Preserved / explicit replacement assertions |
|---|---|---|
| private verification fixture clears inherited setgid before creating descendants | ported private: private verification fixture clears inherited setgid before creating descendants | Fixture clears inherited setgid on private root before descendants; os.tmpdir assertion retained. |
| private maintenance refuses genuine capture with valid resources before snapshot effects | ported private: private maintenance refuses genuine capture with valid resources before snapshot effects | Genuine signed capture refuses with exact capture private verification refused, before filesystem reads, callbacks or launched specs. |
| real shared-base private proof survives unavailable originals and ignores poisoned config | ported private: real shared-base private proof survives unavailable originals and ignores poisoned config | Real selected pack/index and canonical-only proof env; source config poison ignored, canonical poison refuses; source/canonical disappearance/substitution refuses, original stores restored and untouched by producer. Full copied-base independence explicitly superseded. |
| pack-only proof refuses missing before consumer | ported private: pack-only proof refuses missing before consumer | Separate missing/corrupt staged-pack injection before proof; no consumer or canonical import; originals retained. |
| pack-only proof refuses corrupt before consumer | ported private: pack-only proof refuses corrupt before consumer | Separate missing/corrupt staged-pack injection before proof; no consumer or canonical import; originals retained. |
| genuine below-C limit refuses before decoded producer or consumer | ported private: genuine below-C limit refuses before decoded producer or consumer | Inherited constructor C refuses selected declared size before decoded child or consumer; complete cleanup. |
| ordinary and nested private verification share cumulative original C | ported private: ordinary and nested private verification share cumulative original C | Ordinary canonical full verification consumes original operation C, nested importer cannot renew it; no consumer/import. |
| all pipeline work remains in the original charged reservation | ported private: all pipeline work remains in the original charged reservation | All nine existing dimensions charged in the same original slot; decoded equals selected delta total; private work zero RPC, canonical import one pair (2 RPCs); launch remainder proves no renewed reservation. Old zero-import pipeline RPC total explicitly replaced. |
| private pipeline refuses exhausted original decoded | ported private: private pipeline refuses exhausted original decoded | Exhaust this exact original resource before continuation; no consumer/import, source and canonical config retained, cleanup joined. |
| private pipeline refuses exhausted original closureEntries | ported private: private pipeline refuses exhausted original closureEntries | Exhaust this exact original resource before continuation; no consumer/import, source and canonical config retained, cleanup joined. |
| private pipeline refuses exhausted original sourceBytes | ported private: private pipeline refuses exhausted original sourceBytes | Exhaust this exact original resource before continuation; no consumer/import, source and canonical config retained, cleanup joined. |
| private pipeline refuses exhausted original sourceEntries | ported private: private pipeline refuses exhausted original sourceEntries | Exhaust this exact original resource before continuation; no consumer/import, source and canonical config retained, cleanup joined. |
| private pipeline refuses exhausted original artifactBytes | ported private: private pipeline refuses exhausted original artifactBytes | Exhaust this exact original resource before continuation; no consumer/import, source and canonical config retained, cleanup joined. |
| private pipeline refuses exhausted original metadataBytes | ported private: private pipeline refuses exhausted original metadataBytes | Exhaust this exact original resource before continuation; no consumer/import, source and canonical config retained, cleanup joined. |
| private pipeline refuses exhausted original adminEntries | ported private: private pipeline refuses exhausted original adminEntries | Exhaust this exact original resource before continuation; no consumer/import, source and canonical config retained, cleanup joined. |
| private pipeline refuses exhausted original sources | ported private: private pipeline refuses exhausted original sources | Exhaust this exact original resource before continuation; no consumer/import, source and canonical config retained, cleanup joined. |
| private pipeline refuses exhausted original launches | ported private: private pipeline refuses exhausted original launches | Exhaust this exact original resource before continuation; no consumer/import, source and canonical config retained, cleanup joined. |
| fresh callback is awaited again at a later helper launch | ported private: fresh callback is awaited again at a later helper launch | Later helper dispatch awaits local freshness callback again; actual launch witness and consumer absence. |
| consumer rejection is awaited, cleaned and propagated without touching originals | ported private: consumer rejection is awaited, cleaned and propagated without touching originals | Await borrowed-artifact test phase callback rejection; original error propagates only after callback/handles settle and owned scratch cleanup; no import. |
| cancellation during consumer waits for callback settlement | ported private: cancellation during consumer waits for callback settlement | Cancellation while borrowed test callback held cannot settle/clean early; release then refusal, no import, originals untouched. |
| custody callback refusal before helper launch never calls consumer | ported private: custody callback refusal before helper launch never calls consumer | Local owner/custody denial before helper launch prevents consumer and all launches. |
| owned completion and raw stderr disposal both settle before continuation | ported private: owned completion and raw stderr disposal both settle before continuation | Actual streaming pack child exit and raw asynchronous stderr close independently held; both join before continuation and cleanup, observed exit/cleanup witnesses, no consumer. |
| composed snapshot sole cleanup follows all handle closes: write failure | ported private: composed snapshot sole cleanup follows all handle closes: write failure | Distinct write-failure/cancel/callback-rejection injection at actual staging; held pin close blocks return and every unlink; all opened handles close, each entry removed once, no completed pack/consumer/import. |
| composed snapshot sole cleanup follows all handle closes: cancel | ported private: composed snapshot sole cleanup follows all handle closes: cancel | Distinct write-failure/cancel/callback-rejection injection at actual staging; held pin close blocks return and every unlink; all opened handles close, each entry removed once, no completed pack/consumer/import. |
| composed snapshot sole cleanup follows all handle closes: callback rejection | ported private: composed snapshot sole cleanup follows all handle closes: callback rejection | Distinct write-failure/cancel/callback-rejection injection at actual staging; held pin close blocks return and every unlink; all opened handles close, each entry removed once, no completed pack/consumer/import. |
| snapshot final proof blocks every Git launch after earlier source sibling | ported private: snapshot final proof blocks every Git launch after earlier source sibling | Distinct earlier source leaf / earlier private sibling / source alternate / private control mutation witness; unchanged config/source restored, every later Git launch blocked and consumer never called. |
| snapshot final proof blocks every Git launch after earlier output sibling | ported private: snapshot final proof blocks every Git launch after earlier output sibling | Distinct earlier source leaf / earlier private sibling / source alternate / private control mutation witness; unchanged config/source restored, every later Git launch blocked and consumer never called. |
| snapshot final proof blocks every Git launch after source control | ported private: snapshot final proof blocks every Git launch after source control | Distinct earlier source leaf / earlier private sibling / source alternate / private control mutation witness; unchanged config/source restored, every later Git launch blocked and consumer never called. |
| snapshot final proof blocks every Git launch after output control | ported private: snapshot final proof blocks every Git launch after output control | Distinct earlier source leaf / earlier private sibling / source alternate / private control mutation witness; unchanged config/source restored, every later Git launch blocked and consumer never called. |
| snapshot final proof refuses a source control change with info first in the listing | ported private: snapshot final proof refuses a source control change with info first in the listing | Actual source alternate change between real loose-leaf visits under explicit info-first/info-last enumeration; witness, no Git or consumer, source restored. |
| snapshot final proof refuses a source control change with info last in the listing | ported private: snapshot final proof refuses a source control change with info last in the listing | Actual source alternate change between real loose-leaf visits under explicit info-first/info-last enumeration; witness, no Git or consumer, source restored. |

### U4b additional ports and interim evidence

- `agent/test/credential-free-capture.test.ts:96` ports the reflective
  capture-private refusal to importMaintenanceTip. The exact message stays
  `capture private verification refused`; genuine signed capture refuses
  before filesystem reads, effect callbacks or launched specs. This test is
  outside the 154 O/P inventory and passed in the 762-case affected run.
- `agent/test/code-cross-check-snapshot.test.ts:16` updates the explanatory
  symbol to deleteCodeSnapshot, the actual retirement method at
  `agent/src/git.ts:5310`. This is a comment correction with no runtime effect.
- `agent/src/recovery.ts:3341` updates admitTerminalOrigin's docstring to
  describe objects-only transfer/no-op and imported-not-indexed crash
  residue. The docstring itself has no runtime effect; C6 reconciliation
  remains incomplete.

| Recorded worker check | Actual result / boundary |
|---|---|
| Combined affected tests, AY6aQz | EXIT=0; 762 tests, 7 suites; 762 passed, 0 failures/cancellations/skips/todo; 837111.614716 ms, with fixed `--test-timeout=120000` and concurrency 2. Includes ordinary recovery/index defaults, both port suites and real handoff paths. |
| Additional handoff/cache/shallow/ref controls, 7DrNmN | EXIT=0; nine focused cases, including real post-import range failure preventing accepted indexing; 27275.477455 ms. |
| Selected proof isolation, tSOeir | EXIT=0; positive asserts strict index, decoded bodies and range use staged-only objects and sole pinned canonical alternate. |
| Late observed hardlink refusal, qwdnL1 → 8qiE2W | EXIT=1 at two intended alias-reason assertions, then EXIT=0 on the focused alias/quota/positive controls after fd/named nlink rechecks preserved the fixed alias reason. No additional filesystem operations. |
| Literal ref support, j8wzaM → 2K66EN | EXIT=1 for three valid refs at intended import-success assertions, then EXIT=0 with six controls. Removes an unintended ASCII/1024 name restriction; no extra Git launch or cap change. |
| Ordinary config-reader isolation, t6ln7z → 7qrYNY | Default ordinary call qualified; extra private option incorrectly changed it. Minimal public option whitelist restores default behavior; two focused controls passed. |
| Combined O/P port run, O8JtAH | EXIT=0, 154 tests: 123 O + 31 P; 154 passed, 0 failed/cancelled/skipped/todo; duration 186475.369727 ms. No full agent gate implied. |
| Agent typecheck, HhhsnI | EXIT=0 after the test-only required process UID type cast. |
| Earlier typecheck, Ro5opf | EXIT=1, TS2769 at snapshot-ports lines 856–857; superseded by HhhsnI, not a final green gate. |
| Candidate-absent boundary, eXO6Ls → TAnnKn | EXIT=1 at intended success assertion (merge-base 128), then EXIT=0 with 8 passing controls after negative ancestry handling. |
| Source-device and rename-parent controls, dAPfr9 → 7VGSSS | EXIT=1 at 3 intended assertions, then EXIT=0 with 5 passing cases. |
| Short-write destination/ancestor controls, f754O5 → YWZaVG | EXIT=1 (37/39 passed; intended second-write refusals failed), then EXIT=0 with 7 cases after disposable stage-parent mode comparison. Unchanged scoped repeats MqQRTZ/cWvTGK add no new proof. |
| Final mutation proof, full integration gates and independent range reviews | Required M2 obligations, reported on the frozen revision in the PR. These implementation checks do not replace or waive them. |

Logs named above are `.uzi/scratch/gate-log.<id>`. The lead inspected actual results;
red/green assertions name the pre-fix defect, not a missing method/import. These
observations are separate from the final tester's revision-bound mutation evidence.


### U4b clause / assertion map

D = `git-maintenance-delta-transfer.test.ts`, A = `git-maintenance-delta-admission.test.ts`,
P = `git-maintenance-delta-proof.test.ts`, O/V = the two complete port tables above,
R = `runner-maintenance-terminal-source.test.ts`, W = `runner-maintenance-wip-boundary.test.ts`.

| Acceptance | Actual mechanism / tests |
|---|---|
| 1, 2 | A canonical alternates/http/top-info controls, source format/gitfile, ancestor symlink/inode swap after proof; O known-base matrix; D canonical config/pin controls. Refusal before accepted import; post-import identity failure retains residue. |
| 3, 5, 5a | D non-anchor-reachable and canonical-absent candidates dropped/delivered; nonlisted-only boundary_missing. A stale heads/default excludes current origin closure without enlarging the selected set. |
| 5b, 5c, 5d | A exact RECOVERY_BUNDLE_REF and transient-tag-only cases assert maintenance_transfer_boundary_missing; D positive per each eligible family and recovered-source default refusal. Literal ref positives include punctuation, Unicode NBSP and >1024 characters; malformed refs refuse before Git. |
| 4 | P missing new selected blob in clone and missing selected member in staged pack refuse before canonical import; source/custody retained. |
| 4a | P removes new index after canonical strict import and retains pack/unknown bytes; R origin-transfer-post-import-range exercises actual coordinator/owner propagation and asserts no accepted WIP index, ref/release/bundle authority or rollback. |
| 4b | P documented-residual case: deleted excluded blob still permits range walk; full closure exits128. This is not transfer success evidence. C4 owns the paired self-contained-archive backstop test. |
| 6–8 | D symlink loose/fanout/objects/pack; A real gitfile/SHA256 source, foreign source device and canonical controls; O unknown/raw-name/FIFO/control/type/profile cases. Commitgraph/MIDX bytes counted but unread. |
| 9–11 | D all hardlinked file families; A differently named base alias, interrupted-finalize nlink2 and fd/read-time alias observations preserve fixed alias reason, source AND custody. A sourceEntries exhaustion during actual loose walk; O supported/unsupported UID-split models and ordinary actual-worker positives. Models are not live DAC proof. |
| 12 | P compressible selected object fails declared C before pack/decoded child; compressible decoded body fails remaining original C after nested positively verified base work, with no C renewal. V constructor-C/nested/original-slot ports retained. |
| 13, 14 | P same-count substituted expected OID fails at actual strict index exact-set check before decoded phase; D corrupt new loose bytes and P staged/frame corruption refuse. D positive asserts staged-only proof environment, no clone fallback. Ordinary isolated-index controls retained. |
| 15, 16 | P repeated unchanged staged proof reuses settled same-operation entries under exact C; fresh operations start empty and hash again; canonical cache hits cannot authenticate differing new same-size bodies; held child completion plus cancellation seeds no partial cache and joins actual work. |
| 17 | A shared and dissociated layouts both exclude physical base objects and import the exact selected set without refs. |
| 18, 19 | D direct custody denial at import; R real strict-client import pair refusal prevents actual import, output pair refusal prevents index, positive/no-op returns only after owner join/final family proof. Tip tree connected in bare; no tracking/owed/ref/remote/bundle/FINAL authority. |
| 20, 21 | D/O/V held-I/O/close/foreign cleanup and borrowed-artifact observations; R authenticated records/ledger never contain scratch output paths. Reads/private proof add no pair; per-file preparation invokes neither Git nor RPC. |
| 22 | All123 O +31 V expanded keys and original order preserved, with explicit in-place/U2/range replacements. Reflective credential-free case additionally preserves exact capture refusal, zero reads/effects/spawns. |
| F rows | A parent symlink/hardlinked write target; D pre-created symlink target, parent inode swap after target-absence probe, complete-after-read identities, uncertain close and foreign disposal; O/V each corresponding fault/ordering case. Disposable scratch has no fsync requirement under U2. |
| Owner shallow additions | A actual shallow-source-derived bare, existing/present hostile shallow entries, lstat error and independent true/unknown/empty/error query; D between-check insertion. Query env omits GIT_SHALLOW_FILE/private overrides; both checks and every stat attempt charged, no duplicate launch. R projects fixed layout reason through real owner. |
| Owner zero-delta | D positive already_present, P forced empty non-anchor case, R actual clean W handoff. No pack/index/import/ref write; same shallow/pin/owner/fresh-output checks; no FINAL/release/cleanup authority. |
| Owner R8 clarification | R dispatch/start/join/index traces and all-dimension table above. Reads/private proofs no pairs; import/output pairs immediate; both custody refusals prevent effects. W existing cancellation/escaped callback/remaining-budget cases retained, fixture now os.tmpdir/Linux. |
| Ordinary callers | A public family scanner ignores extra private adapter and has no producer hold RPCs; R ordinary producer-shaped error is not projected by guarded catch; ordinary pack/index and recovery transfer fallback suites retained. |

The decoded C applies to verified delivered object bodies, not Git-internal
decompression memory. The fixed phase observer is test-only and default pass-through:
walk, selection, pack (including private strict index), proof (decoded/range), import.
Admission and between-phase overhead remains charged and observable in operation
counter snapshots; the phase seam resets no budget and is not the C2 harness.

### U4b E2E literal audit

Each current literal was checked with `git grep -n -F` across `e2e/phases/`,
`e2e/lib.sh`, `e2e/lib.test.sh` and `e2e/forge-fake/`. Each row below returned exit 1
(no hits), in the final 22-literal audit (`m1-final-e2e-literal-audit.txt`). No
emitted E2E expectation was changed; no compose execution is implied.

| Literal | Current verdict |
|---|---|
| `withVerifiedMaintenancePack` | already accurate: no hits; no E2E literal port needed |
| `snapshotMaintenanceObjects` | already accurate: no hits; no E2E literal port needed |
| `importMaintenanceTip` | already accurate: no hits; no E2E literal port needed |
| `maintenance_source_layout_unsupported` | already accurate: no hits; no E2E literal port needed |
| `maintenance_source_alias_refused` | already accurate: no hits; no E2E literal port needed |
| `maintenance_transfer_boundary_missing` | already accurate: no hits; no E2E literal port needed |
| `maintenance_object_snapshot_refused` | already accurate: no hits; no E2E literal port needed |
| `maintenance_private_verification_refused` | already accurate: no hits; no E2E literal port needed |
| `maintenance_pack_staging_refused` | already accurate: no hits; no E2E literal port needed |
| `maintenance_pack_index_refused` | already accurate: no hits; no E2E literal port needed |
| `capture private verification refused` | already accurate: no hits; no E2E literal port needed |
| `withMaintenanceTransfer` | already accurate: no hits; no E2E literal port needed |
| `admitTerminalOrigin` | already accurate: no hits; no E2E literal port needed |
| `maintenance-wip-v1` | already accurate: no hits; no E2E literal port needed |
| `TERMINAL_ORIGIN_ADMISSION_ENABLED` | already accurate: no hits; no E2E literal port needed |
| `already_present` | already accurate: no hits; no E2E literal port needed |
| `observeMaintenanceTransferPhase` | no hits; no E2E literal port needed |
| `maintenanceOriginFromConfig` | no hits; no E2E literal port needed |
| `withMaintenanceFamilyLock` | no hits; no E2E literal port needed |
| `literalBranch` | no hits; no E2E literal port needed |
| `readCanonicalConfig` | no hits; no E2E literal port needed |
| `sourceUnchanged` | no hits; no E2E literal port needed |

The final offline `task test:e2e-lib` passed all 6 cases (`gate-log.v1a9pD`, exit 0),
and `task test:forge-fake` passed all 10 cases (`gate-log.5bQRYe`, exit 0). Compose
phases `70-handoff.sh`, `72-custody-lifecycle.sh` and
`73-forge-unreachable-park.sh` remain UNRUN;
hosted and image/PVC acceptance remain maintainer-owned. Worker gates/reviews/mutations
remain worker obligations; MO and environment limits do not waive them.

### U4b PR diff map

| Area | Changed files / symbols and review pointers | Review boundary |
|---|---|---|
| Admission / boundary / walk | `agent/src/git.ts`: `importMaintenanceTip`, `literalBranch`, canonical/source pins, `shallow`, eligible anchors and bounded `walk` | Trusted metadata only; complete-boundary/non-shallow precondition; no-follow/alias/device controls; literal ref support without a new name cap. |
| Selection / pack / proof / import | `agent/src/git.ts`: selected OIDs, `stageMaintenancePack`, private `indexMaintenancePack` binding, staged provenance/cache, proof/range, imported/already_present result | Exact members, selected-body hashes and original C; canonical prerequisites only; no ref; inert post-import failure residue retained. |
| R8 / F / accounting | `agent/src/runner.ts`: `withRetainedSourceBoundary` transfer operation/owner; `agent/src/recovery.ts`: `admitTerminalOrigin` binding/fixed-error projection/output index; `agent/src/git.ts`: private family-reader adapter, cleanup/rename and charge sites | Read/private proofs add no pair; import/output fresh pairs, actual owner join, local preparation, no unsafe cleanup or budget renewal. Ordinary defaults preserved. |
| Tests | Delta transfer/admission/proof suites; all 123 snapshot and 31 private ports; shared fixture; real terminal-source, owned WIP-boundary and coordinator fixtures; credential-free reflective port | Focused checks and complete old-key maps below; ordinary caller controls; source/custody, no-ref, exact OIDs, cache/C and effect-order assertions. `code-cross-check-snapshot` comment and admission docstring have no runtime effect. |
| Docs | ADR-1197, this delivery report and generated mirror, UUID-v4 changelog fragment | Conditional range-proof contract, acquisition chains, residuals, accounting/port/clause maps, complete-path counters and separately owned downstream work. |

### Documentation verification

`task check-docs:web` passed (exit 0, `gate-log.dr2cmg`): 79 documents
validated, with 48 house-style warnings. Explicit UUID-v4 fragment validation passed
(exit 0, `gate-log.c53mL0`). The final `task docs:sync` and mirror equality checks
accompany this commit. No full agent gate or independent candidate review is claimed
by these M1 checks; M2 owns them.

Final agent lint, typecheck and knip passed (`gate-log.O5LjmD`, exit 0). An earlier
lint check rejected a redundant compressibility comparison; the next typecheck found
optional-method mock typing in the added token regression. Both test-only problems
were corrected before the green static run. The focused post-correction token and
compressibility checks passed (`gate-log.yBa3yW`, exit 0). The original token-lifetime
control was already green: a current newly returned token cannot revive the discovery
and charged tokens invalidated by registration. The coordinator-only fixture models
that existing lifetime behavior; real import/output custody refusal is covered
separately by the composed worker paths.

All seven named unchanged-writer/capture methods were byte-compared against baseline
76cd0a66 after implementation. The flag remains false. Final old-helper sweeps contain
no owned present-tense source/test callers; historical inventory titles remain in docs.

## U4b M2: historical disposable-scratch administration-budget failure

The implementation is committed at `81993269`. The mandatory full
`task gate:agent` run on that revision did **not** pass: the command reached its
120-minute tool bound and its process tree was reaped. The last unit report remained
`running`, with 631/690 files done, 20,415 tests observed, 20,296 passed, 104 failed
and 15 skipped. No final unit summary was produced and Codex M4 was not reached.
This is incomplete failed validation, not a complete CI or gate result.

Four real dirty preservation-path cases refused with
`maintenance_resource_exhausted` at the unchanged `adminEntries=4096` limit.
A narrowed reproduction under the same existing process/tmpdir wrappers reproduced
it without full-suite contention (`gate-log.eaP9IG`, exit 1, one test,
35,885.902498 ms). The next charge was one admin entry in importer `scratchCheck`,
called by the pre-spawn guard before the post-import range command. The proof-phase
snapshot had already consumed 3,926 admin entries; import-phase entry was 3,979.
The import/handoff could not complete within the reservation. The earlier direct
fixture's 3,983-entry successful observation is retained as history, not substituted
for this required gate's refusal. The deeper wrapper path adds real parent checks;
no path was shortened to make the failing example fit.

(Path-depth dependent: see the note under the corrected wrapped table; only `launches`, `sources`, `rpcAttempts`, `closureEntries` and `decoded` are reproducible.)

| Dimension | Refused wrapped path, including W0 and coordinator effects | Limit |
|---|---:|---:|
| launches | 191 | 512 |
| metadataBytes | 31,070,720 | 256 MiB |
| adminEntries | 4,096; next 1 refused | 4,096 |
| sources | 118 | 128 |
| rpcAttempts | 16 charged + 4 discovery = 20 physical attempts (10 pairs) | 32 |
| sourceEntries | 10,789 | 65,536 |
| sourceBytes | 200,998 | 256 MiB |
| artifactBytes | 2,042 | 256 MiB |
| closureEntries | 16 | 65,536 |
| decoded | 478 | 8 GiB, or tighter inherited C |

This was R3 evidence requiring owner direction. The resource-category decision is
now **RESOLVED** by the authorized correction and actual complete wrapped pass
[below](#m2-owner-diagnosis-and-category-correction). No cap, timeout, test timeout,
selected object set, proof, writer, guard or disposal rule changed. Paths were not
shortened and scratch-parent checks were not removed. The historical failed gate
remains failed/incomplete. Candidate review, full per-test mutation mapping,
integration/full-gate and hosted/downstream obligations remain mandatory.

### Independent M2 fixture repairs

The full run also exposed two omitted coordinator test-fixture ports. Their duck-typed
Git objects lacked the new importer and therefore refused at the capability check
before the observations the tests intended to exercise. They are repaired without
production-source changes:

| Existing cases | Port and retained assertions |
|---|---|
| `recovery-maintenance-resources.test.ts`: terminal caller shares both observations' metadataBytes/adminEntries/rpcAttempts/candidates allowance before charge | Throwing importer seam plus explicit zero-import counter; both observations still share the original allowance, capture remains zero and no ledger is created. |
| All 91 cases in `recovery-terminal-wip-coordinator.test.ts` | Explicit coordinator-only imported-result model calls local preparation and family/origin validation, then the semantic pair immediately before modeled import. It claims no object or child-settlement proof. Original status, generation, journal, source, custody, ledger and callback assertions remain. New counters distinguish denied import from denied output. WIP fixtures use Linux-only options and container-internal `os.tmpdir()`. |
| The three exact-status transition titles ending `transfer entry`: failed to running; cancelled to parked; completed to unknown | Same three cases now end `canonical import dispatch`, accurately locating observation 8. Observation 9 remains post-settlement output acceptance. No read-only command acquires an extra pair. |
| `git-maintenance-wip-capture-h.test.ts`: clone HEAD=W / plain clone and bundle positive | Native bundle verification now explicitly uses `cwd: f.root`, the owned fixture repository. Every clone/bundle/connectivity assertion and W0-H production pin sequence, P5, 65 launches and 5-unit payload accounting remain unchanged. |

The stale coordinator fixture reproduced 90 failures and one pass
(`gate-log.nIiRLF`); the four resource cases reproduced `0 !== 2`. Both full
fixture files now pass all 111 tests (`gate-log.4OY1Gl`), and the W0 positive passes
(`gate-log.gYsOl0`). The existing charged RPC assertions remain 14/16/18 for
0/1/2 cold journal directories, plus 4 discovery attempts. These coordinator-only
figures are not the real W0-H three-dispatch commit path or C2 capacity evidence.
Lint, typecheck, knip and diff checking passed for these repairs
(`2UYOWE`, `NhHQTJ`, `0fDUDt`, `LvjT8O`).

### Baseline triage and review boundaries

Five environment-allowlist assertions (chat, Codex and SDK executor) reproduce on
baseline `76cd0a66` with inherited `UZI_SQLC_DIR`, and pass on both base and candidate
when only that inherited toolchain variable is unset. Four-case raw comparison:
candidate `VN0T9Z`, base `KuXAkm`; sanitized comparison `yeZQou`/`HliMcX`.
The SDK single case is `fsuVik`/`AFtHir` raw and `OtMMUu`/`A7cOFo` sanitized.
Those initial failures remain failed-check evidence. No environment allowlist or
production dependency was changed. These failure-triage timings are not a healthy
before/after test-matrix performance comparison.

The W0 bundle check also failed on baseline because its implicit cwd discovered
the unrelated runner checkout after stripping Git safety settings. A native thin-env
repo setup probe confirmed dubious ownership (`ooBd2M`, exit 128). Selecting the
owned fixture repository corrects the test context rather than weakening Git safety.

The initial reviewer found no blocking source defect and requested the missing
baseline/candidate timing record. Its four focused Linux checks were rerun with
TMPDIR/TMP/TEMP unset: native `/tmp` was case-sensitive and all four passed in
21,288.913 ms. The earlier worktree-TMPDIR invocation is superseded. The initial
tester and auditor dispatches were cancelled; partial mutation artifacts are not
complete sign-offs. Surviving or invalid mutants still require adjudication, and
completion is not claimed by this record.

An additional healthy comparison ran the unchanged ordinary pack-index and
pack-staging matrix on baseline `76cd0a66` and candidate `754f1329`. Both passed
all 156 tests with Node 24.20.0, concurrency 2, timeout 120000, the same fixture
filesystem, hermetic process preload and explicit Git environment, unsetting only
inherited `UZI_SQLC_DIR`. Wall times were 38.994692179 s (baseline, `K0FDnD`) and
37.308433792 s (candidate, `5CXyvP`). The exact commands and revisions are retained
in `m2-shared-helper-timings.json`. This measures the affected ordinary helper step,
not the new complete producer path, C2's composed measurement or hosted CI.
Queue/cache/rerun effects and hosted critical-path timing remain unmeasured.
The failed full gate's 120-minute bound is not a successful duration comparison.

The PR diff map's test area additionally includes these two coordinator fixture
ports and the W0 verification-cwd correction. Their changes have no new production
charge sites. All parent issue and C2–C8 boundaries remain unchanged.

### M2 security repair and mutation adjudication

The full-range auditor at `754f1329` reproduced a Medium CWE-117 defect: an
importer default-branch name containing U+202E reached the raw debug log. The
producer-only literal validator now rejects C0/C1/DEL, bidi marks, embeddings,
overrides and isolates, and line/paragraph separators before filesystem work,
callbacks or Git dispatch. Both branch fields are revalidated, including values
loaded from storage. The fixed refusal does not echo the value. Unicode names,
NBSP and names over 1 KiB retain literal anchor behavior. No ordinary writer,
storage schema or global logger changes. The new six-case branch-safety suite
captures the real logger, checks zero effects for unsafe inputs and exercises
valid inputs through import. Independent repair review was subsequently completed
at `f4dcfd8f`, as recorded below; the new category candidate needs its own review.

The tester also reproduced a false-positive shallow-output test: its default
object-mode `Readable.from` failed even for an injected `false` response with
`subprocess output closed before end`; both children settled. Byte mode fixes the
injection. The export-only positive then imported successfully, with two queries,
one import/pair and 27/27 children settled. The primary harness now uses byte mode
with a permanent positive and a second-query false-to-true negative, without a
shallow file, so the post-proof Git-state check has an independent assertion.

| Named mutation | Corrected evidence at `754f1329`, before primary harness repair |
|---|---|
| M08 config pin | Removing only the canonical-config leaf pin typechecks and turns its post-proof mutation test red (`Missing expected rejection`). The earlier controls-map mutation did not disable this pin and is not counted. |
| M14 shallow state | Bypassing the importer query predicate typechecks; with byte-mode injection, true/unknown/empty responses turn three refusal assertions red. Pipe-error refusal remains green. The earlier object-mode survivor was invalid evidence. |
| M16 second query | Removing only the dirty-path post-proof query typechecks and turns the existing two-query accounting assertion red (`1 !== 2`). File insertion alone remains caught by the independent absence pin. |
| M04 all-ref anchors | Broadening the query/anchor set typechecks and turns both bundle-head/non-listed-tag refusal cases red. Earlier compile failure is not red evidence. |
| M15 canonical environment | An importer-only private-directory override typechecks and turns the omitted-`GIT_DIR` assertion red. The earlier three-hit instrumentation failure is not counted. |

The repaired focused export baseline passed all ten selected tests, without skips.
Exact edits, commands, exits, assertions and logs are in
`tester-1-adjudication-evidence/results.json`; the original results are retained.
These five proofs do not establish a complete per-test mutation map. Export lint
enumerated zero ignored files and failed; it is not a source-lint verdict. The
primary repair run passed all 50 admission/branch-safety cases with zero skips
(`7GiJFH`, 148,883.086877 ms); lint, typecheck and knip passed (`ya5lfk`). Docs
validation and mirror equality passed (`gfZ6Hu`); the preceding `ZuB8uV` invocation
used a nonexistent docs task name and is not a successful docs check. Independent
review subsequently confirmed the committed repairs as recorded below.
The then-open R3 category block is now resolved by the owner-directed correction
below; the incomplete full gate and unperformed proofs remain open.
No formal partial or acceptance decision has waived them.

At `f4dcfd8f`, the scoped reviewer, tester and auditor found no introduced repair
defect; the auditor closed the original importer CWE-117 finding. The tester's
17-case focused baseline passed. Restoring the old branch predicate, dropping
byte mode, bypassing the shallow predicate, omitting the second query and
substituting source objects for the private index's canonical alternate each
compiled and turned its intended assertions red. These add applied-candidate
proof, not a complete per-test mutation map. Global ordinary storage/logging
sanitation remains unchanged; the repair closes the new importer input path only.

The tester retained two actual green close-join mutants: dropping the outer
`await closePins()` was masked on the selected paths by the helper's independent
join; dropping the actual per-pin close promises also survived because the tests
released their latch before later asynchronous cleanup reached its assertions.
Neither green mutant is counted as red evidence. The held-close fixture now wraps
the real completion promise with a consumption observer and retains all prior
terminal, cleanup, handle and child assertions. A third early-walk-failure case
exercises the outer join before any helper/private view exists. Failure cleanup
also releases the latch and awaits the operation. All three focused cases, lint,
typecheck and knip passed (`gmvVqf`). Independent reviewer, tester and auditor
confirmation at `6f8f0b7c` subsequently found no introduced blocking repair defect.
Both formerly surviving mutants typechecked (exit 0) and turned the intended
assertions red (test exit 1): the outer-await omission fails the early-walk
held-terminal assertion, and discarding per-pin close completions fails the
completion-consumption assertion in all three cases. These scoped proofs close
those two survivors; they do not complete the per-test mutation map or review the
new uncommitted category candidate.

Standalone `env -u UZI_SQLC_DIR task test:codex-m4` passed on `f4dcfd8f`
(`WNwUTs`, exit 0): 189 tests, zero skips/failures/cancellations, 180 seconds for
that stage. The completeness checker satisfied the required Claude/U, Codex/U and
Codex/P matrix (37 clauses, 64 executed evidence results). The full gate did not
reach this stage; this standalone success does not turn that incomplete run green.
No production-source change follows this M4 result. Hosted CI, affected compose
phases and C2–C8 remain separately owned and unrun as stated above.

## M2 owner diagnosis and category correction

**The resource R3 category decision is resolved; M2 and M3 remain incomplete.**
The owner required diagnosis before redesign and authorized C1 existing-file
stat/fstat/open/pin validation and its cleanup reservations as `sourceEntries`,
logical object rows as `closureEntries`, and administrative creation as
`adminEntries`. No actual duplicate operation was established (NONE removed).
Operations, consume calls, amounts, reservation timing, checks, scheduling, caps
and timeouts remain. W0-H and ordinary defaults remain frozen. No ref, FINAL,
release or cleanup authority is granted.

The failing measured baseline is
`6f8f0b7c718f2036d02ea0202f69b64c2baf0b85`. The corrected candidate was committed
afterwards (`3bef25138`) in `agent/src/git.ts`, `agent/src/recovery-maintenance.ts`,
`agent/test/git-maintenance-delta-accounting.test.ts` and the one adjusted
assertion in `agent/test/git-maintenance-snapshot-ports.test.ts`.
Its successful results must not be attributed to `6f8f0b7c`.

### Historical wrapped baseline: every admin charge by phase and operation

The diagnosis and full grouped JSON (`m2-admin-diagnosis.md` and
`m2-admin-summary.json`, run-local scratch evidence) reconcile 119 JSON admin
rows to the phase/operation table below. **Every code line in this table belongs
to SHA `6f8f0b7c`, not current line numbering.** Units are measured ledger charges;
calls are consume invocations. Observed JS APIs are fs/promises/FileHandle calls,
not kernel syscalls or Git/internal child physical I/O. Reserved helper/cleanup
work and logical rows must not be added to API observations as executed calls.

| Phase | Charge site | Operation served | Consume calls | Successful units | Refused units | Direct API correlation (first call after charge) |
|---|---|---|---:|---:|---:|---|
| coordinator/admission | git.ts:6515 via maintenance.ts:765 | canonical config name rows | 26 | 26 | 0 | reservation / logical row |
| coordinator/admission | git.ts:16175 via maintenance.ts:765 | canonical config NUL rows | 26 | 26 | 0 | reservation / logical row |
| coordinator/admission | git.ts:6359 | family sibling directory read including EOF | 2 | 2 | 0 | Dir.read=2 |
| W0 P0 | recovery.ts:3732 | journal directory read including EOF | 3 | 3 | 0 | Dir.read=3 |
| W0 P0 | git.ts:6515 via maintenance.ts:765 | canonical config name rows | 78 | 78 | 0 | reservation / logical row |
| W0 P0 | git.ts:16175 via maintenance.ts:765 | canonical config NUL rows | 78 | 78 | 0 | reservation / logical row |
| W0 P0 | git.ts:6359 | family sibling directory read including EOF | 6 | 6 | 0 | Dir.read=6 |
| W0 P0 | client.ts:3056 | remote hold-list rows | 3 | 3 | 0 | reservation / logical row |
| W0 P0 | client.ts:3073 | remote exact-hold row | 3 | 3 | 0 | reservation / logical row |
| W0 P0 | recovery.ts:4877 | producer-origin ancestor check | 7 | 7 | 0 | fs.lstat=7 |
| W0 P0 | git.ts:11921 | W0 config helper reservation | 1 | 220 | 0 | reservation / logical row |
| W0 P0 | git.ts:11890 via11954 | W0 config output rows | 32 | 32 | 0 | reservation / logical row |
| W0 P2 + P3 | git.ts:6515 via maintenance.ts:765 | canonical config name rows | 52 | 52 | 0 | reservation / logical row |
| W0 P2 + P3 | git.ts:16175 via maintenance.ts:765 | canonical config NUL rows | 52 | 52 | 0 | reservation / logical row |
| W0 P2 + P3 | git.ts:6359 | family sibling directory read including EOF | 4 | 4 | 0 | Dir.read=4 |
| W0 P2 + P3 | git.ts:11921 | W0 config helper reservation | 1 | 220 | 0 | reservation / logical row |
| W0 P2 + P3 | git.ts:11890 via11954 | W0 config output rows | 32 | 32 | 0 | reservation / logical row |
| W0 P2 + P3 | git.ts:11995 | W0 census/race recheck directory reservation | 1 | 10 | 0 | reservation / logical row |
| W0 P2 + P3 | git.ts:11995 | W0 payload hash directory reservation | 1 | 10 | 0 | reservation / logical row |
| W0 P4a | git.ts:6515 via maintenance.ts:765 | canonical config name rows | 78 | 78 | 0 | reservation / logical row |
| W0 P4a | git.ts:16175 via maintenance.ts:765 | canonical config NUL rows | 78 | 78 | 0 | reservation / logical row |
| W0 P4a | git.ts:6359 | family sibling directory read including EOF | 6 | 6 | 0 | Dir.read=6 |
| W0 P4a | client.ts:3056 | remote hold-list rows | 1 | 1 | 0 | reservation / logical row |
| W0 P4a | client.ts:3073 | remote exact-hold row | 1 | 1 | 0 | reservation / logical row |
| W0 P4a | git.ts:11921 | W0 config helper reservation | 1 | 220 | 0 | reservation / logical row |
| W0 P4a | git.ts:11890 via11954 | W0 config output rows | 32 | 32 | 0 | reservation / logical row |
| W0 P4a | git.ts:11995 | W0 census/race recheck directory reservation | 1 | 10 | 0 | reservation / logical row |
| W0 P4b | git.ts:6515 via maintenance.ts:765 | canonical config name rows | 130 | 130 | 0 | reservation / logical row |
| W0 P4b | git.ts:16175 via maintenance.ts:765 | canonical config NUL rows | 130 | 130 | 0 | reservation / logical row |
| W0 P4b | git.ts:6359 | family sibling directory read including EOF | 10 | 10 | 0 | Dir.read=10 |
| W0 P4b | client.ts:3056 | remote hold-list rows | 3 | 3 | 0 | reservation / logical row |
| W0 P4b | client.ts:3073 | remote exact-hold row | 3 | 3 | 0 | reservation / logical row |
| W0 P4b | git.ts:11921 | W0 config helper reservation | 3 | 660 | 0 | reservation / logical row |
| W0 P4b | git.ts:11890 via11954 | W0 config output rows | 96 | 96 | 0 | reservation / logical row |
| W0 P4b | git.ts:11995 | W0 census/race recheck directory reservation | 1 | 10 | 0 | reservation / logical row |
| W0 P5 | git.ts:11921 | W0 config helper reservation | 1 | 220 | 0 | reservation / logical row |
| W0 P5 | git.ts:11890 via11954 | W0 config output rows | 32 | 32 | 0 | reservation / logical row |
| W0 P5 | git.ts:11995 | W0 census/race recheck directory reservation | 1 | 10 | 0 | reservation / logical row |
| admission/boundary | git.ts:13555 via13810 | initial scratch-parent lstat | 8 | 8 | 0 | fs.lstat=8 |
| admission/boundary | git.ts:13640 | owned-entry identity/cleanup reserve | 16 | 217 | 0 | reservation / logical row |
| admission/boundary | git.ts:13555 via13650 | scratch-parent chain lstat | 215 | 215 | 0 | fs.lstat=215 |
| admission/boundary | git.ts:13555 via13658 | private directory mkdir | 8 | 8 | 0 | fs.mkdir=8 |
| admission/boundary | git.ts:13555 via13670 | private control open/create | 4 | 4 | 0 | fs.open=4 |
| admission/boundary | git.ts:13555 via13674 | private control initial fstat | 4 | 4 | 0 | FileHandle.stat=4 |
| admission/boundary | git.ts:13555 via13679 | private control pre-write lstat | 4 | 4 | 0 | fs.lstat=4 |
| admission/boundary | git.ts:13555 via13680 | private control pre-write fstat | 4 | 4 | 0 | FileHandle.stat=4 |
| admission/boundary | git.ts:13555 via13685 | private control post-write fstat | 4 | 4 | 0 | FileHandle.stat=4 |
| admission/boundary | git.ts:13555 via13615 | owned-directory revalidation lstat | 89 | 89 | 0 | fs.lstat=89 |
| admission/boundary | git.ts:16175 via maintenance.ts:765 | canonical config NUL rows | 39 | 39 | 0 | reservation / logical row |
| selection | git.ts:13555 via13615 | owned-directory revalidation lstat | 105 | 105 | 0 | fs.lstat=105 |
| selection | git.ts:16175 via maintenance.ts:765 | canonical config NUL rows | 65 | 65 | 0 | reservation / logical row |
| selection | git.ts:13555 via13650 | scratch-parent chain lstat | 50 | 50 | 0 | fs.lstat=50 |
| pack | git.ts:13555 via13615 | owned-directory revalidation lstat | 48 | 48 | 0 | fs.lstat=48 |
| pack | git.ts:16175 via maintenance.ts:765 | canonical config NUL rows | 26 | 26 | 0 | reservation / logical row |
| pack | git.ts:13555 via13650 | scratch-parent chain lstat | 10 | 10 | 0 | fs.lstat=10 |
| pack | git.ts:14188 via14229 | stage/index cleanup-parent reservation | 12 | 12 | 0 | reservation / logical row |
| pack | git.ts:14188 via14233 | stage/index initial-parent lstat | 12 | 12 | 0 | fs.lstat=12 |
| pack | git.ts:14188 via14202 | stage/index parent recheck lstat | 120 | 120 | 0 | fs.lstat=120 |
| pack | git.ts:14396 via14464 | stage/index cleanup-parent reservation | 12 | 12 | 0 | reservation / logical row |
| pack | git.ts:14396 via14467 | stage/index initial-parent lstat | 12 | 12 | 0 | fs.lstat=12 |
| pack | git.ts:14396 via14402 | stage/index parent recheck lstat | 168 | 168 | 0 | fs.lstat=168 |
| proof | git.ts:13555 via13615 | owned-directory revalidation lstat | 80 | 80 | 0 | fs.lstat=80 |
| proof | git.ts:16175 via maintenance.ts:765 | canonical config NUL rows | 39 | 39 | 0 | reservation / logical row |
| proof | git.ts:13555 via13650 | scratch-parent chain lstat | 30 | 30 | 0 | fs.lstat=30 |
| import | git.ts:13555 via13650 | scratch-parent chain lstat | 42 | 41 | 1 | fs.lstat=41 |
| import | git.ts:13555 via13615 | owned-directory revalidation lstat | 48 | 48 | 0 | fs.lstat=48 |
| import | git.ts:16175 via maintenance.ts:765 | canonical config NUL rows | 26 | 26 | 0 | reservation / logical row |
| import | client.ts:3056 | remote hold-list rows | 1 | 1 | 0 | reservation / logical row |
| import | client.ts:3073 | remote exact-hold row | 1 | 1 | 0 | reservation / logical row |

W0 P1 and walk measured zero admin charges. Including ordinary callbacks within
subphases, historical W0 totals are P0=430, P1=0, P2+P3=380, P4a=426, P4b=1042,
P5=262: **2540 unchanged**. Other successful totals are coordinator/admission=54,
admission/boundary=596, selection=220, pack=420, proof=149, import=117:
4096 successful units plus 1 refused. This 2540 is the pre-correction measurement,
not a post-fix W0 subphase remeasurement. W0-H byte preservation (lead-verified
hash prefix `77c072d`) and the exact-accounting test prove preservation.

The refused existing-ancestor lstat was in old `git.ts:13650` `scratchCheck`,
via `launch.beforeSpawn` :13712, before post-import range :14048/:14122.
The command TMPDIR base (parent of the wrapper root) was next; consume refused
before filesystem dispatch. Strict canonical index-pack had already completed;
the range child did not launch and no output was accepted. Recovery-root depth
was 6; the wrapper appended one guard component to actual Linux `os.tmpdir()`.
No path shortening was used.

Historical reconciliation: 1831 reserved units (1540 W0 config helpers + 50 W0
directory helpers + 217 own registry + 24 stage/index cleanup parents), 1163
logical rows (224 W0 config + 364 canonical names + 559 canonical NUL + 16 hold),
and 1102 immediate API units (31 Dir.read + 1047 lstat + 8 mkdir + 4 open +
12 fstat) total 4096. The refused unit dispatched no API. No immediate
admin-linked API failed; the complete filesystem ledger separately recorded
1276 lstat ENOENT results. Failed probes retain their serving-category charge.

W0 config helpers reserved 7 x 220 = 1540 units plus 7 x 32 = 224 config rows.
Literal helper source estimates five explicit realpathSync and sixteen explicit
lstatSync attempts per helper, 147 direct child calls across seven helpers,
excluding realpath internals. This is an estimate, not measured kernel work.
The older comment's 15-lstat/20-visit bound is not grounds to reduce units.
Five census/hash/race helpers reserved 5 x 10 = 50 units; child open/stat internals
were not instrumented. Cleanup observed 155 lstats (141 parents,14 owned names),
six unlinks and eight rmdirs; pin closure separately observed 28 FileHandle.close
calls. Prepaid cleanup generates no fresh consume or refund.

### Site-by-site old-to-new accounting verdicts

Site numbers below identify **baseline SHA `6f8f0b7c`**; current symbols identify
the enforcing code. Each validation attempt retains its original precharge,
including ENOENT/failure, identity rejection, repeated checks and short-write
iterations. The original audit proposed these mappings; the candidate now
implements the authorized changes.

| Baseline site / current symbol | Old → current category | Verdict | Operation / preserved boundary |
|---|---|---|---|
| 13554-13560 `charge/stat` | parameter/default sourceEntries -> same default | already accurate | Common per-attempt dispatcher; default source and metadata reservation unchanged. Its individual callers select the category. |
| 13615 `validate` owned-directory arm | adminEntries -> sourceEntries | updated | One lstat per owned directory per validation pass. The regular owned-file arm already uses sourceEntries. Both authenticate existing names; retain every invocation. |
| 13640 `own`: depth term, all entry kinds | adminEntries -> sourceEntries | updated | Reserve the same depth units for future existing-parent lstats. Reservation at creation time is not creation work. Includes directory, HEAD/config, staged and renamed artifact registrations. |
| 13640 `own`: administrative +3 | adminEntries -> sourceEntries | updated | Retain all three units for identity acquisition and bounded owned-name cleanup/removal allowance. For mkdir the immediate post-mkdir identity lstat at13660 is pre-reserved here, not an uncharged extra. Cleanup target lstat and unlink/rmdir are not creation. Controls also reserve this conservative amount; do not delete spare capacity on suspicion of overlap with an immediate fstat charge. |
| 13641 `own`: nonadministrative +3 | sourceEntries -> sourceEntries | already accurate | Existing regular-artifact reservation stays three. Combined parent+target reserve remains depth+3 for every entry; only category changes. |
| 13650 `scratchCheck` | adminEntries -> sourceEntries | updated | One actual existing-parent lstat per ancestor, per invocation. Applies before mkdir, control creation/write, Git spawn, both rename checks, and each import-stream iteration. All repeats retained, including root. |
| 13658 `mkdir` | adminEntries -> adminEntries | already accurate | Actual private directory creation attempt; all eight directory creations on tester's path are correctly admin. The separate identity lstat is covered by own's source reserve. |
| 13670 `write` open | adminEntries -> adminEntries | already accurate | O_CREAT with O_EXCL creates worker HEAD/config controls; actual creation is admin. Do not generalize this exception to read-only opens or pack/index artifacts. Artifact byte reservation stays once before open. |
| 13674 `write` initial fstat | adminEntries -> sourceEntries | updated | File already exists after exclusive open. Validates created identity/mode/nlink; fstat is validation even while the creation helper is still active. |
| 13679 `write` pre-write named lstat | adminEntries -> sourceEntries | updated | Existing HEAD/config name validation on every short-write iteration. |
| 13680 `write` pre-write fstat | adminEntries -> sourceEntries | updated | Independent fd validation on every iteration; not the same actual operation as preceding lstat. |
| 13685 `write` post-write fstat | adminEntries -> sourceEntries | updated | Independent post-write validation on every iteration. Keep every retry's charge, even for the same control. |
| 13810 initial scratch ancestry | adminEntries -> sourceEntries | updated | Existing outDir-parent chain acquisition/validation before any private creation; actual lstat, not pin creation. Store identities exactly as now. |
| 14188 stage `chargeStat`, disposable=true, target | sourceEntries -> sourceEntries | already accurate | Existing correct producer target fstat/lstat and target cleanup-stat reservations. |
| 14188 stage `chargeStat`, disposable=true, parents | adminEntries -> sourceEntries | updated | All hierarchy reads/reservations serve the staged pack. Covers parentCheck14202, cleanup reservation14229, and initial parent lstat14233; unchanged count for every parent every time. |
| 14230 stage cleanup unlink | sourceEntries when disposable -> same | already accurate | One potential regular staged-pack unlink, pre-reserved independently of later revocation. Ordinary=false remains adminEntries. |
| 14240 stage open | sourceEntries when disposable -> same | already accurate | Pack artifact create/open is already charged to sourceEntries. Ordinary=false remains adminEntries. |
| 14396 index `chargeStat`, proof bound, staged/output targets | sourceEntries -> sourceEntries | already accurate | Existing correct input/output stats, absence checks, fd/name authentication and cleanup-target reserves. |
| 14396 index `chargeStat`, proof bound, parents | adminEntries -> sourceEntries | updated | Hierarchy reads/reservations serve the pack/index files. Covers parentCheck14402, cleanup reservation14464, initial parent lstat14467, and all repeated inputCheck/effect/return validations. |
| 14465 index cleanup unlink | sourceEntries when proof -> same | already accurate | Regular index cleanup reservation; ordinary unbound remains adminEntries. |
| 14475 index input open | sourceEntries when proof -> same | already accurate | Read-only pack open. Ordinary unbound remains adminEntries. |
| 14561 index Git unlink+create | sourceEntries x2 when proof -> same | already accurate | Two logical artifact operations, including absent-name unlink attempt; preserve both, even if unlink sees ENOENT. Ordinary unbound remains adminEntries x2. |
| 14582 index output open | sourceEntries when proof -> same | already accurate | Existing read-only index open. Ordinary unbound remains adminEntries. |
| 13895/13899 readCanonicalConfig → runGit16175 / recovery-maintenance765 | adminEntries → sourceEntries on new private acquisition | updated | Explicit sixth private runGit argument selects source; each nonempty raw NUL row charged once before filtering. Same argv/cwd/env/executor/launch/byte/error path. |
| runGit / consumeMetadataEntriesIfActive defaults | adminEntries → adminEntries | already accurate | New optional category defaults admin in both helpers; ordinary/W0 callers do not opt in. |
| 6344–6356,6377 producer chargeSource | sourceEntries → sourceEntries | already accurate | Source ancestor lstat, parent opendir and clone lstat stay distinct attempts; no new ordinary charges. |
| 6359 producer directory rows/EOF | sourceEntries → sourceEntries | already accurate | Private configRead branch remains source; ordinary family scanner remains admin. |
| 6393–6410 origin interpretation | ordinary admin → unchanged | already accurate | Already-metered snapshot interpretation adds no acquisition/charge; ordinary owner callbacks retain defaults. |
| runner3468–3506 transfer binding | no direct admin caller | already accurate | Same original reservation/deadline, owner/action/join/final family proof and scheduling. |
| recovery3338–3625 admission/transfer | no direct admin caller | already accurate | Existing hold-list/exact pair and both RPC charges; prepare remains local deny-only. |
| recovery3732 /4877 | inherited admin → admin | already accurate | Journal enumeration and origin ancestor reads outside C1 correction remain frozen. |
| git6515 /16175 ordinary name/NUL rows | inherited admin → admin | already accurate | Includes W0 and transfer-owner ordinary rechecks; enclosing C1 operation does not opt them in. |
| recovery-maintenance caps / maintenance-budget / dind-maintenance | unchanged | already accurate | Only optional row-category selector added in resource helper; no cap or reservation renewal. |
| git9151 /17418 /17545 /17625 | inherited admin → admin | already accurate | Unrelated baseline literal sites outside C1 correction. |

`own` keeps sixteen registrations: eight directories, four HEAD/config controls,
two staged and two renamed artifacts. Combined depth+3 remains per entry.
The first consume stays depth+(administrative?3:0), now source; the separate
nonadministrative three-unit source consume stays separate. Metadata remains
`(depth+3)*6144`. Rename registers the destination without refunding the old reserve.
Stage keeps hierarchy plus two target-stat reserves and separate unlink; index
keeps independent reserves. Unused capacity is not proof of duplicate work.
Post-mkdir identity lstat and finite cleanup retain prepaid allowances after
revocation; uncertain/unregistered identities remain retained.

The stage selector covers paths served by `options.disposable` and the index
selector paths served by `proof`, including parents and reserved cleanup stats.
Omitted options retain ordinary admin accounting, fsync, parent policy and
isolated environment. Private canonical rows opt in through `readCanonicalConfig`;
no global replacement or disabled shared charge plus refund is used.

Existing pin/absolutePin stat/open/fstat, leaf/control/absence validation, clone
walk/read/rechecks, checksum, rename I/O and strict-import streaming stay source.
Object rows stay `closureEntries`: selected/range OIDs (:13961–13970/:14047),
selected metadata (:13988), pack header count (:14288), expected OIDs (:14453),
index OIDs (:14601) and CRC rows (:14633), using baseline line numbers.
Repeated traversals of the same OID remain distinct charges. Metadata, encoded
reads, artifact emission, decoded bodies, launches, sources and RPCs remain
independent dimensions.

### Accounting regression evidence

`agent/test/git-maintenance-delta-accounting.test.ts` adds 15 cases: four
positive/ordinary/raw-row controls and eleven original-slot precharge refusals
at mkdir, create, initial/named/pre/post-write stats, scratch ancestry, initial
ancestry, owned validation, stage and index. The positive forces seven-byte
HEAD/config writes and checks per-attempt categories, sixteen own reserves,
four separate artifact reserves, eight mkdirs and four creates. Raw-row controls
include an ignored extra key and exhaustion before filtering. Ordinary controls
check admin stats, fsync and isolated environment. Refusals retain refs,
source/custody and child joins; refused initial fstat retains the already-created
unregistered HEAD rather than claiming disposal.

The existing ported partial-write assertion now requires admin constant and
source increasing per retry. Attempts `[[0,3,0],[1,2,1],[2,1,2]]`, exact bytes,
artifactBytes=35, syncs=0, requests=0 and custody assertions remain.
No case, quota allowance or cap changed.

Coder's final typecheck/lint/knip passed (`.uzi/scratch/gate-log.fUu2jb`, exit 0).

Final focused tests passed 17, skipped zero (`gate-log.yBqg8p`, exit 0):
15 new cases, adjusted partial write and exact W0 accounting. W0-H bytes match
HEAD (lead-verified hash prefix `77c072d`). The own-category mutant typechecked
then failed the intended category assertion (`gate-log.CdH6K7`, test exit 1),
in a disposable export. Earlier focused failures (12/13 and 73/74) remain in
the implementation report: a new cleanup expectation and stale partial-write
assertion were corrected. These focused proofs do not supply the full per-test
mutation map or independent review of the new candidate.

### Actual corrected wrapped command: eight cases passed

Main's `gate-log.Wjznwz` recorded **EXIT=0, 8 passed, 0 skipped,
duration 154614.593962 ms**. It used actual real-procfs and tmpdir-leakguard
wrappers, primary agent cwd and actual Linux `os.tmpdir()` wrapper depth,
unsetting only `UZI_SQLC_DIR`. No path shortening, cap or timeout change.
The source was the uncommitted four-file category candidate above.

The full `m2-category-wrapped-results.json` contains sixteen rows, two per case.
Final rows include transferCalls=1, transferEntered=1 and effect traces.
Each pair's totals agree. Missing no-op decoded usage is filled as 0.
These are measured slot charges; launches/sources are not measured physical
processes/distinct clones. HTTP fixture transports are not hosted API evidence.

| Slot dimension | Limit | `origin-admission` | `origin-already-present` | `origin-transfer-final-family` | `origin-transfer-head` |
|---|---:|---:|---:|---:|---:|
| `launches` | 512 | 200 | 93 | 193 | 200 |
| `metadataBytes` | 268,435,456 | 32,870,476 | 17,872,179 | 32,596,328 | 32,599,934 |
| `adminEntries` | 4,096 | 2,664 | 931 | 2,608 | 2,662 |
| `sources` | 128 | 126 | 70 | 122 | 126 |
| `rpcAttempts` | 32 | 18 | 8 | 16 | 16 |
| `sourceEntries` | 65,536 | 13,017 | 7,167 | 13,017 | 13,017 |
| `sourceBytes` | 268,435,456 | 201,091 | 164,528 | 201,021 | 201,118 |
| `artifactBytes` | 268,435,456 | 2,046 | 142 | 2,040 | 2,042 |
| `closureEntries` | 65,536 | 18 | 0 | 18 | 18 |
| `decoded` | 8 GiB (or tighter inherited C) | 478 | 0 | 478 | 478 |

| Slot dimension | Limit | `origin-transfer-revoked` | `origin-transfer-import-refused` | `origin-transfer-layout-refused` | `origin-transfer-post-import-range` |
|---|---:|---:|---:|---:|---:|
| `launches` | 512 | 200 | 189 | 186 | 192 |
| `metadataBytes` | 268,435,456 | 32,862,078 | 29,340,160 | 25,978,326 | 30,524,023 |
| `adminEntries` | 4,096 | 2,664 | 2,608 | 2,606 | 2,608 |
| `sources` | 128 | 126 | 114 | 106 | 118 |
| `rpcAttempts` | 32 | 18 | 16 | 14 | 16 |
| `sourceEntries` | 65,536 | 13,017 | 11,440 | 9,942 | 12,021 |
| `sourceBytes` | 268,435,456 | 201,032 | 200,629 | 200,182 | 200,487 |
| `artifactBytes` | 268,435,456 | 2,042 | 2,044 | 2,042 | 2,044 |
| `closureEntries` | 65,536 | 18 | 16 | 16 | 16 |
| `decoded` | 8 GiB (or tighter inherited C) | 478 | 478 | 478 | 478 |

Path-depth dependence: `adminEntries`, `sourceEntries`, `metadataBytes` and
`sourceBytes` include per-path-component reservations, so they vary with the depth
of the temporary directory the run executes under; the values in these tables were
measured at the wrapped Linux `os.tmpdir()` depth and are not portable. `launches`,
`sources`, `rpcAttempts`, `closureEntries` and `decoded` do not depend on depth and
are reproducible.

Each of the eight variants is under every listed cap. The 32 RPC limit applies
to charged slot attempts; discovery remains separate.

| Variant | Slot RPCs / pairs | Discovery RPCs / pairs | Physical RPCs / pairs | Observed effect boundary |
|---|---:|---:|---:|---|
| origin-admission | 18 / 9 | 4 / 2 | 22 / 11 | Import and accepted WIP index after owner join; no ref/FINAL/cleanup authority. |
| origin-already-present | 8 / 4 | 4 / 2 | 12 / 6 | Accepted after owner join; no pack/import/ref write. |
| origin-transfer-final-family | 16 / 8 | 4 / 2 | 20 / 10 | Import occurred; final-family refusal prevents accepted WIP index. |
| origin-transfer-head | 16 / 8 | 4 / 2 | 20 / 10 | Import occurred; changed HEAD prevents output pair/index. |
| origin-transfer-revoked | 18 / 9 | 4 / 2 | 22 / 11 | Output pair observed but custody revoked: no accepted WIP index. |
| origin-transfer-import-refused | 16 / 8 | 4 / 2 | 20 / 10 | Import custody refusal prevents canonical import and output index. |
| origin-transfer-layout-refused | 14 / 7 | 4 / 2 | 18 / 9 | Post-proof layout refusal prevents import and output index. |
| origin-transfer-post-import-range | 16 / 8 | 4 / 2 | 20 / 10 | Strict import occurred; range failure prevents accepted WIP index; inert residue retained. |

The full dirty path now measures launches=200/512,
metadataBytes=32870476/268435456, adminEntries=2664/4096, sources=126/128,
RPC=18/32 +4 discovery=22 physical, sourceEntries=13017/65536,
sourceBytes=201091/268435456, artifactBytes=2046/268435456,
closureEntries=18/65536 and decoded=478/8 GiB (or tighter inherited C).
Accepted indexing follows actual owner join. Refusals block their respective
effects. Quiescence remains injected and trusted ancestor DAC modeled;
this is not the C2 representative W0/U4b/archive measurement.

### Enabling blocker: decompression amplification (auditor High)

The decoded counter (8 GiB aggregate, no per-object cap) does not bound Git-process
memory before the first inflation. Demonstrated: a roughly 1 MB loose tree object
declaring 1 GiB drove source-environment `rev-list --objects` to about 1.08 GB RSS;
`pack-objects` and both `index-pack --strict` invocations also inflate (inferred,
not measured; Git may stream blobs above `core.bigFileThreshold`). The transfer
is production-unreachable today (`terminalOriginAdmissionEnabled()` returns the false
`TERMINAL_ORIGIN_ADMISSION_ENABLED` constant; tests override it). This is an explicit
blocker to enabling it: the enabling design must prove a memory bound before the first
inflation. No specific fix is prescribed here, and none is implemented.

### Candidate mutation adjudication at `04d8f363` (review fold)

Red/green for the tester's survivors, each closed by a behavioural test (mutant
applied, test red on the intended assertion, restored, green):

| Mutant | Test | Red message | Green |
|---|---|---|---|
| w1 walk per-entry stat uncharged | `delta walk10b` (admission) | preceding charge is `sourceBytes`, not `sourceEntries` | restored |
| s1 importer `charge()` default category | `delta walk10b` (admission) | `adminEntries` observed before the loose-entry open | restored |
| s2/s2b stage and index category | `delta accounting` central and ordinary stage/index tests | `adminEntries` where `sourceEntries` is required (observed by call window, not source text) | restored |
| ojdup index before owner join | runner `origin-*` modes | `indexesBeforeJoin` 1, expected 0 | restored |
| p1 proof/import order swap | `delta proof range` (proof file) | a canonical import was dispatched | restored |
| N3 custodian comparison in `beforeSemantic` | `a semantic pair naming a different original worker` | imported 1, expected 0 | restored |

The proof-phase refusals decoded-hash mismatch, remaining-C exhaustion and
cancellation after proof already fail under the p1 swap in the proof test file; the
tester's partial run covered only the four tests that legitimately fail before the proof phase.

Survivors left as **unproven layer, for exact-head adjudication** (no observed test
plus mutation establishes the property, so none is claimed covered elsewhere):

| Survivor | Tester evidence |
|---|---|
| n1 both walk `nlink` checks removed | targeted run (13 tests, including the hardlink alias cases) stayed green; the descriptor-level alias check in the walk's `sourceUnchanged` still refuses, so the walk-level checks are likely redundant, not proven |
| rf explicit `revalidate()` before output index removed | targeted run (3 tests) stayed green; the fresh observation and the indexing guard may subsume it, not proven |
| x1 importer `check()` made a no-op | targeted run (2 tests) stayed green; resource accounting still calls `check()`, not proven to cover the importer sites |

Re-test at `58137dd4` (Linux, Git 2.53.0): the fold tests redden under w1, s1, s2,
s2b, ojdup, p1 and the custodian-deletion mutant and pass on restore. For n1, the walk
layer alone stays green on 98 hardlink tests, while removing every importer `nlink`
layer (both walk checks, the `validate()` leaf check, the `unchanged()` comparison and
the in-walk alias refusal) reddens 12 of them: the hardlink property is pinned
collectively and the walk-level checks are redundant defence in depth; the other
layers were not tested one by one. rf and x1 stay unproven: no test makes only the
post-join `revalidate()` refuse, and cancellation is pinned through the resource
checks rather than the importer's own `check()`.

### Enabling blocker: split-UID runner tree refuses at the source pin

`importMaintenanceTip` pins every component of the source clone path from the
filesystem root (`absolutePin(source.clonePath, true)`), and each directory must pass
`safeDir`. The production split-UID layout does not: the entrypoint creates the runner
root as `worker:runner` mode 3775 (setgid, group-write, sticky), and
`seedRunnerClone` creates the repository directory under it as worker-owned, group
`runner`, mode 2775 (umask 002). `safeDir` rejects the runner root on its sticky bit,
and both directories on group write (the group-write exception covers only
runner-owned paths and strict ancestors of the runner root). With the guard enabled,
every split-UID transfer would therefore refuse `maintenance_source_layout_unsupported`
at the source pin, before any Git launch: fail-closed, nothing lost, but no transfer
succeeds. Unreachable today: the transfer runs only behind the false
`TERMINAL_ORIGIN_ADMISSION_ENABLED` guard.

Acceptance 11 gap: the split-UID models used by the ported snapshot tests model only
the strict ancestor (2775) and runner-owned clone paths; the runner root (3775) and
repository directory (2775, group `runner`) keep the fixture's default modes, so no test
exercises the production layout. Supporting the production layout requires a separately
reviewed layout/admission design that preserves component-replacement protection, plus
tests using the exact entrypoint ownership and modes. No permission broadening is
presumed safe or authorized here.

### Current exact-revision validation status (`f7442c49`)

This supersedes the historical boundary below for the current candidate. Guard off;
non-closing partial delivery of #3441 (see "Delivery status of #3441" near the top).

- Full gate: exact-head CI on `f7442c49` is green on every required check, including
  both full agent suite shards and the worker-uid suite (the maintainer-approved
  substitute for the local `gate:agent`).
- Reviews over the review-fold range: reviewer approval at `f7442c49`, tester re-test
  and auditor re-audit at `58137dd4` (agent sources unchanged since `04d8f363`).
- Mutation: the review-fold survivors are pinned; the walk-level `nlink` checks are
  redundant within the collectively pinned hardlink property; `rf` and `x1` remain
  unproven layer-specific gaps.
- E2E literal audit: no hits; `task test:e2e-lib` and `task test:forge-fake` pass;
  compose phases unrun.
- Still open: the two-source `sources` conflict (C2 input, including the per-chunk
  revalidation scaling to measure), the decompression
  and split-UID runner-tree enabling blockers, live acceptance and C2 to C8. #3441
  stays open.

### Remaining verification boundary (historical, before the review fold)

Historical full `task gate:agent` log `gate-log.vNpy2y` timed out at 120 minutes,
631/690 files, with 104 fixture/environment/category failures. Focused repairs
and eight wrapped passes do not establish a full-suite green. Full per-test
mutation/applicability mapping and integration/full gate remain mandatory and
unwaived; the full gate is satisfied only by a green exact-head CI full agent suite
at the final candidate, the maintainer-approved substitute recorded in the status
above. Reviewer, tester and auditor must review the new combined category
candidate; their clean `6f8f0b7c` reports cover the earlier committed fixture repair.
M2 is incomplete, M3 is not done, guard remains false. No partial acceptance
has been supplied.

Standalone Codex M4 at `f4dcfd8f` remains 189 tests, 37 clauses and 64 executed
evidence results (`WNwUTs`); the timed-out gate did not reach it.
Later `6f8f0b7c` offline E2E library/fake checks (`paqj2` / `D40CnK`) and the
25-literal no-hit audit are separate proofs, not compose execution.
C2–C8 retain their existing boundaries. Compose70/72/73 remain unrun and hosted
critical-path timing unmeasured. Maintainers own the separately recorded live
image/PVC acceptance and can run those named compose phases against an isolated
stack. Workers still owe gates, security, reviews and mutation evidence.
