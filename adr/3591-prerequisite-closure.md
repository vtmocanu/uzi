# ADR-3591: API prerequisite pins retain declared thin-bundle dependencies

**Status**: Implemented locally through C1b M4 at
`db652da5ffb7139ce421dae88eb6c6f3a2d3437f`; M5 validation/hosted timing and
exact-head acceptance pending. This is an API-only child of
[PRD-2506](../prds/2506-prerequisite-closure.md), not completion of that parent.
C2 release, C3 owner visibility, #2969 integration and producer FINAL remain
separate. No feature advertisement, agent change or thin FINAL release is
included.

## Context

A guarded capture can exceed the self-contained bundle cap and fall back to a
thin bundle. Local verification in the trusted worker bare does not guarantee
that a clean consumer will retain the prerequisites after default-history
rewrites. The accepted design preserves their declared closure through an
API-owned ref without fetching objects into the API or uploading run content.
The E2 probe and its credential/policy limits remain recorded in the PRD;
this implementation adds no new live forge probe.

## Decision

### Admission and frozen identity

`recovery.Service.stream` calls `insertPrerequisiteClosure` after
`MarkCaptureReady` and before committing. The guarded SQL insertion requires
the exact open inventory hold and manifest-bound available capture with
1–64 prerequisites and original-worker provenance. The original authenticated
uploading worker must have registered `recovery_prerequisite_closure_v1`;
a body-supplied declaration cannot substitute for that snapshot.

`UZI_PREREQUISITE_CLOSURE_FORGES` is empty by default. Parsing normalizes a
comma list of known `github`, `gitlab`, `forgejo` kinds, ignores empty entries
and duplicates, and refuses unknown kinds at boot. Actual admission is narrower:
`github` must be listed and the connection base must be `https://github.com`
after trailing-slash normalization. E2 reviewed one GitHub.com configuration;
other hosts, forges and protected/ruleset configurations need their own reviewed
probe. Parser acceptance and synthetic driver coverage are not enablement proof.
The empty `.env.example` entry documents this default-off control and the
unknown-kind boot refusal. The bundled Compose API environment passes the value
through with an empty default, so setting it in `.env` affects incoming admission.

Prerequisite Git transport derives its clone URL from fresh project metadata
read by stable project ID, requiring the returned ID and WebURL to match the
stored repository binding and the namespace path to agree with that WebURL.
The prerequisite lane checks identity before and after Git observations and refuses Git redirects, including
same-origin path redirects. REST metadata and Git remain separate observations:
this does not make them atomic against a concurrent forge rename. These checks
do not widen the reviewed GitHub.com strategy or establish hosted acceptance.

A failed S0 insertion rolls back availability. Available/admit/stream-loser
receipts and sweeps do not backfill rows. A postcommit kick belongs to the
worker-service lifecycle, independent of upload-request cancellation; a dropped
kick is recoverable from the durable pending row. Knob-off stops incoming S0,
not in-flight transactions or processing of existing rows.

S1 obtains fresh default-tip D and target absence from one bounded
advertisement. It uses one driver for positive ancestry proof of each declared
P, at most 64: P must be ancestor/equal to D. Unknown proof backs off;
not-ancestor refuses. No partial proof cache survives an attempt.
D anchors existing default history; no pack of run content is sent.
The tested GitHub workflow constraint requires a compatible D at send time;
a changed workflow tree may refuse the frozen attempt.

S2 locks run → hold → capture → closure, rechecks eligibility and binding,
then commits the invocation marker before forge work. The marker freezes
connection ID, forge kind, normalized base URL, project ID, default branch and
D; capture/hold/run/user/repo identity, checksum and prerequisites are immutable
row facts. Migration `00387_prerequisite_closure.sql` guards transitions,
reservations and binding immutability; the inherited C1a available-entry guard
remains separate.

### One create and durable ambiguity

The prepared broker value shares a single-use send capability, even across
value copies. S3 sends one zero-old create with an empty pack at
`refs/uzi-prereq/<capture-id>`, then reads back. At D means pinned, including
lost/negative acknowledgement followed by matching read-back; explicit refusal
plus absence means refused; another tip is foreign; ambiguity is unknown.

There is no return from invoked to pending. A crash after S2, even before send,
eventually becomes `create_unknown`. S4 is listing-only indefinitely:
expected D → durably pinned; foreign tip → foreign untouched; absent/error →
retain unknown and its live pointer. Never resend, select another D, rebind,
reset attempts, recreate/move the ref or clear unknown because time elapsed.
An expired unknown first has to commit pinned on positive observation before a
later S7 item can delete.

### Conditional T1–T5 proof, no server bundle validation

| Fact | Enforcement or trust boundary |
|---|---|
| T1 | Trusted worker produces/verifies bytes in its bare and declares actual header prerequisites (ADR-1296 D5). |
| T2 | Upload verifies length/checksum and immutable manifest binding in `recovery/service.go`. No server parser, apply or native bundle verification is added. |
| T3 | S1 in `workersvc/prerequisite_closure.go` positively checks each declared P against fresh D. |
| T4 | Broker create-only and matching read-back establish the pin at D. Future S6 FINAL-time listing is C2 work. |
| T5 | Exact row identity, frozen binding and capture-locked S7 prevent uzi cleanup of available pins. Future release-transaction revalidation is C2 work. |

This proof is conditional on T1/T2; pinning cannot establish bundle validity or
completeness of the declaration. C1b's status is not release authority.
`releaseFinalInventory` still refuses nonempty prerequisites, and existing
archive SQL/worker guards remain. Registration vocabulary/order accepts the
capability without advertising a feature or changing the agent.

The optional worker `prerequisite_closure` field is read-only:

| Durable facts | Worker state / reason |
|---|---|
| Bound self-contained capture | Field omitted. |
| No row | unsupported / closure_unsupported; bound facts may be included, unavailable unbound facts omitted. |
| pending | pending; ancestry retry may carry prerequisite_ancestry_unknown. |
| invoked / create_unknown | pending / pin_unknown. |
| pinned, no absence candidate | pinned. |
| pinned, absence candidate | pending / pin_absence_unconfirmed. |
| refused | refused / bounded persisted refusal. |
| foreign | refused / pin_foreign. |
| lost | lost / pin_lost. |
| deleted | unsupported / archive_unavailable. |

Rows expose their frozen checksum/prerequisites, not raw `last_error`.
`pin_cleanup_exhausted` is not a worker status reason; owner pin display is C3.

### Retention, cleanup and O10 accumulation

S7 in `workersvc/prerequisite_closure_cleanup.go` requires a capture rechecked
as expired or discarded under capture → closure locks. Under the frozen binding,
list the exact ref; at expected tip, reserve a cleanup generation and CAS-delete;
confirm absence before deleted/pointer clearance. Absence observed on a later
listing can settle an ambiguous delete without resending. Foreign refs are left
untouched. Available pins survive settlement of their hold by another route.

Preparation and cleanup each have an eight-attempt ceiling. Cleanup exhaustion
keeps pinned/live state and `pin_cleanup_exhausted`; further work lists only,
with backoff capped at one hour. Later confirmed absence can clear the pointer;
it grants no ninth delete. Ordinary pinned audits use a 24-hour cadence;
a first absence candidate needs a different pass at least ten minutes later
for lost. SQL generations/pass IDs and leases fence delayed outcomes and replicas.

The live pointer blocks run deletion via a restrictive FK; repo/connection
handlers return a conflict with `prerequisite_closure_count`, including a
named-FK fallback. Terminal states retain immutable audit tombstones.
Migration Down refuses existing closure rows rather than erasing evidence.
The active-capture limit (default 256, excludes expired/discarded) does not
bound leaked pins, unknowns or tombstones. O10 accepts unbounded accumulation.

Processing is bounded, not total inventory: one process-wide forge-work slot;
ten-second pass, five items, raw scan limit ten per class; eight-second item,
five-second proof phase, 4.5-second marker/send/read-back/outcome reserve.
The reserve includes separate 500 ms budgets for the marker transaction and
post-commit binding check, then one second each for send/read-back and 1.5
seconds for outcome persistence. A binding-read error still prevents send.
Listing admission requires 3.5 seconds before scheduling an audit or retry;
cleanup selection excludes available captures and rechecks expiry/discard under
capture locks.
Before-S1 full-budget deferral spends no attempt; admitted late-positive proof
spends one and backs off, eventually prerequisite_prepare_budget_exhausted.
Incomplete ancestry remains prerequisite_ancestry_unknown. The in-flight
DB outcome tail is at most five seconds and clamped to the absolute pass tail.
The pass is registered regardless of knob state; shutdown joins the lane
before pool closure. These are code budgets, not measured production latency.

The operator procedure is in
[run recovery](../docs/run-recovery.md#operating-an-o10-backlog):
contain incoming S0 with knob-off, preserve bytes and exact diagnostics, repair
connectivity/credentials under the frozen identity, and use existing S7 after
confirmed expiry/discard. Unknown absence may block forever. No manual forge
delete, database override, attempt reset, pointer clearing or owner/custodian
shortcut is authorized. Escalation requires a separate contract decision.

External pin deletion/history rewrite can defeat restore, following ADR-1296's
accepted external-writer residual. Pins can retain pre-purge history under
`refs/uzi-*`; this ADR grants no history/privacy purge authorization or
permission to erase server evidence.

## Local evidence and remaining acceptance

The lead's M1–M4 evidence records focused race/count=1 checks, scoped reviews
and isolated behavioral mutations/restored greens. Those are local evidence;
no hosted exact-head API constituents, repo/store-it acceptance or comparative
CI timing has yet been collected. M5 stays open. Representative tests inspected:

| Tests | Claim and limit |
|---|---|
| `TestPrerequisiteClosureSchemaLiveDB`, `TestPrerequisiteClosureTransitionsLiveDB`, `TestPrerequisiteClosureMigrationRoundTripLiveDB` (store) | Identity, transitions and guarded rollback. |
| `TestPrereqCreateReceivePack`, `TestPrereqPreparedCopiesShareSend`, `TestPrereqDeleteWireCAS` (pushbroker) | Actual wire create/CAS and shared one-send authority. |
| `TestPrerequisiteNativeSweepRestoresAfterDefaultRewrite` (workersvc) | Public sweep → real broker/HTTP Git backend, fake persistence/native ancestry: pin D strictly descends from P; fresh no-local clone lacks P after rewrite; pin fetch restores exact source SHA/tree/file. No DB/upload/released-custody claim. |
| `TestPrerequisiteNativeWithoutPinCannotRestoreAfterDefaultRewrite`, `TestPrerequisiteNativeSweepRefusesRewriteBeforePreparation`, `TestPrerequisiteNativeSweepCannotCoverUndeclaredPrerequisite` | No pin fails naming P; pre-prepare rewrite refuses without send; omitted P2 still fails native verify/import after fetching declared P1. |
| `TestPrerequisiteFrozenBindingDriftNoTrafficLiveDB`, `TestPrerequisiteStoppedAfterMarkerNeverResendsLiveDB` | Frozen-binding drift and marker-crash ambiguity retain without new create. |
| `TestPrerequisiteCleanupReplicaRaceAtSevenLiveDB`, `TestPrerequisiteCleanupOldOutcomeAgainstPinnedLeaseLiveDB` | Replica attempt-eight reservation and stale-outcome fences. |
| `TestPrerequisitePinnedCleanupForeignTipUntouchedLiveDB`, `TestPrerequisiteAmbiguousCleanupRequiresLaterAbsenceLiveDB` | Foreign untouched; applied ambiguous delete retains pointer until later absence without another send. |
| `TestRecoveryPrerequisiteClosureUploadRollbackLiveDB`, `TestRecoveryPrerequisiteClosureUploadNoBackfillLiveDB`, `TestRecoveryPrerequisiteClosureUploadRegistrationSnapshotLiveDB` (recovery) | Atomic S0, no backfill and authenticated registration snapshot. |
| `TestRecoveryPrerequisiteClosureUploadIntegrityLiveDB`, `TestRecoveryPrerequisiteClosurePinnedThinFinalLiveDB` | Integrity/immutable-declaration refusal and continued thin FINAL refusal. |
| `TestPrerequisiteUploadKicksBrokerOnceLiveDB` (workersvc), `TestRecoveryPrerequisiteClosureNotAdvertised` (handler) | Lost/cancelled request does not lose committed kick; feature remains absent. |

M4's final local focused check at `db652da5` recorded 71 top-level PASS,
13 LiveDB PASS, zero SKIP and no race warning, plus focused build/vet
(`gate-log.8pCKA2`, EXIT 0). Its 58-second command wall includes compile/cache;
it is not a before/after CI delta. M3's final S7 checks at `ca16afa1` recorded
four top-level PASS, zero SKIP/race and focused build/vet (EXIT 0).
Broker evidence records zero new skips; two inherited bridge skips are not
claimed as new proof.

M5 documentation/embeds and focused local checks are recorded in the
[PRD execution evidence](../prds/2506-prerequisite-closure.md#c1b-m5-local-validation-and-remaining-hosted-acceptance).
The fresh-database repair isolates new immutable-closure fixtures without
weakening old guards; three database-identity mutations fail and restore green.
Pinned uncapped lint is clean through `60a94c89`. Earlier failed checks remain
recorded, rather than being represented as aggregate success.

Remaining: exact-head hosted API/repo/store-it PASS with zero SKIP, and
affected-job/critical-path timing with queue/cache/reruns separated. The added
per-fixture migration cost remains unmeasured in CI; local command times are
not a comparative CI delta. The latest pipeline read returned an upstream
forge error. No hosted verdict or dependency-scan PASS is inferred. M6 live/operator acceptance is a separate
informative milestone, not performed here. C2/C3, E5/E6, release and hosted
acceptance remain open; no source-disposal or parent-completion claim follows.
