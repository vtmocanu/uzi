# ADR-2507: Inventory source refusal diagnostics

Status: Accepted
Refs: #2507

## Context

Guarded recovery previously recorded only `inventory_source_not_quiescent`, obscuring which source proof refused FINAL. The diagnostic stage did not establish the live cause of the observed automatic
hold-release failure. The 2026-10-09 amendment below records the subsequently
implemented release policy; live acceptance remains unproven here. This change adds diagnostic evidence while preserving verified/unknown decisions, custody protection, immutable archive and FINAL identities, and the existing #2458 cleared-journal and #2433 missing-parent fixes.

## Decision

The inventory reader returns an optional cause only on `unknown`. The real reader always supplies one; legacy producers may omit it. Verified results carry no cause. Classification comes from fixed failure sites, never raw exception text:

| Cause | Failure site |
| --- | --- |
| `attribution_unreadable` | Nonempty recovery attribution or attempt ledger cannot be validated |
| `clone_ancestor_invalid` | Existing ancestor is unsafe or its probe fails |
| `clone_path_invalid` | Clone identity, ownership, normalized path or existing leaf is unsafe |
| `clone_head_unreadable` | Bounded HEAD, git directory or loose/packed ref verification fails |
| `git_or_filesystem_error` | Git, attribution discovery, ownership-path operation or clone-leaf IO throws |
| `other` | Entry guard, unclassified failure, missing or unrecognized legacy cause |

A scoped reporter records the first refusal per boundary invocation. The closed check vocabulary is `execution_tail_present`, `boundary_exception`, `inventory_read_not_verified`, `non_linux_host`, `live_attempt_path`, `process_not_quiescent_before_git`, `process_not_quiescent_after_git`, `worktree_status_dirty`, `worktree_status_unreadable`, `source_identity_changed` and `physical_sources_without_boundary`. Existing status-before-after-process decision order is preserved, including the after-process probe for dirty or unreadable status. Sibling proof stops at the first failure.

Observer delivery is synchronous and best effort. Throws, promise rejection and hostile thenables cannot affect the proof decision; observer promises are consumed without awaiting them. A never-settling observer does not delay FINAL. Logging is independent of journal writes, including boot discovery with no original record.

The coordinator emits `recovery: inventory source boundary retained` with run id, generation, optional capture id, `source_boundary_reason`, and `inventory_read_cause` only for the inventory-read check. These new diagnostic fields and reasons contain no raw errors, clone paths, attribution bytes or process details. Existing exception logging is unchanged.

Only the two existing retention writes encode the reason:

- `inventory_source_not_quiescent:<check>`
- `inventory_source_not_quiescent:inventory_read_not_verified:<cause>`

A callback-free boundary still requires positive discovery of an empty clones array. Missing, unknown, malformed and physical-source inventories retain custody. Diagnostics add no runner release guards.

## Retry and compatibility

The exact live retry set retains `capture_error`, `restart_upload_failed`, `upload_transient`, `credential_rejected` and `inventory_source_not_quiescent`, and adds every non-read check encoding and all six read-cause encodings above. Both the legacy unsuffixed reason and the check-only `inventory_source_not_quiescent:inventory_read_not_verified` remain accepted. Matching uses complete strings; unknown checks, causes and extra suffixes confer neither needs-action eligibility nor transient backoff. Uploaded records keep their existing eligibility even with an unknown reason.

Recognized source refusals retain exponential live backoff, its existing ceiling, and the same request and archive. Diagnostic reasons remain MAC-covered by the existing journal authentication. They describe the last failed attempt, not a claim about the source's current state, and can remain after a later successful FINAL.

Older workers can authenticate these records, but their needs-action allowlist does not recognize the new suffixes, so retry of those records stops on downgrade. This compatibility risk is accepted. The original diagnostic stage made no protocol, API, server DTO or CLI transport
changes. CLI presentation of the local source-boundary diagnostic was deferred
because the server DTO carried no such diagnostic. The later completed-publication
receipt and reason below are a separate contract, not exposure of local diagnostics.

## Validation and follow-up

Focused reader, runner and coordinator regressions cover failure classification, initial/final proof failures, observer isolation, sibling ordering, authenticated persistence, completed issue and MR-rework provenance, exact retry matching, repeated backoff and immutable replay. At the original diagnostic stage, live worker evidence and the automatic release
fix remained follow-up work. The release policy is now implemented as recorded
below; this ADR does not claim production, live acceptance or deployment evidence.

## Amendment 2026-10-09 — maintainer decision: completed-publication release

**Status**: Implemented in API and worker. The archive diagnostics above remain
in force, including failed/cancelled foreground `execution_tail_present`.
They describe source-boundary refusal on the archive path, not a prerequisite
for successful completed-publication custody release.

With `recovery_completed_publication_v1` negotiated by worker and API, a
guarded COMPLETED `issue`, `mr_rework` or `self_improve` run can release
its exact completing-generation hold synchronously. Completion and immutable
identity commit first. The API proves that the recorded MR in the own repository
names the server-derived branch, BranchHead H equals MR HeadSHA, and
`CompareAncestry(H, finalSHA)` proves containment or equality of the durably
reported final SHA. Issue authority is `agentIssueBranch(issue)`, MR-rework
authority is the frozen `pipeline_ref` candidate, and self-improvement
authority is `selfImproveBranch(runID)` (`uzi/self-improve/<run-id>`), not
its tracking issue, pipeline or a worker-reported branch.

Success atomically stores and ACKs `completed_publication_receipt`.
Missing, mismatched, unknown, erroring or drifting evidence retains custody
with `completed_publication_reason`. Exact stored-receipt replay precedes
mutable run/forge eligibility. A matching own-repository copy qualifies without
a non-fork or branch-protection guarantee; later user deletion or rewriting
is outside scope. Healthy release adds no heartbeat prerequisite.

The worker preserves its fixed original terminal outcome before preparation
when durable outbox storage is available. A valid receipt skips capture, WIP,
freeze and clean-source proof, including with existing available/thin/needs-action
captures. It must be persisted and read back in an existing MAC-authenticated
generation recovery record before protected report retirement. Other responses
use original capture fallback, including mixed versions or an API that
advertises the feature but excludes the kind. Lost ACK plus capture refusal
cannot fail/reopen completion or authorize cleanup; original completion replay
recovers the receipt. No-outbox/reserve attempted-send guards prevent false
failure without a new durable journal.

Physical destruction stays separate: quiescence, credential isolation,
exact canonical owned path, expected SHA and attribution, execution/adoption
exclusion through destruction and bare locks remain required. Eligible
dirty/untracked completing-source leftovers may be discarded; siblings, shared
pins, consumed metadata, foreign/unknown inventory and quarantine are retained.
Metadata follows actual disposal with one bounded EIO retry; the existing
retired-attempt ledger can finish exact old-journal clearing, while lost proof
retains remaining artifacts without new crash discovery. No standalone
receipt or tombstone is introduced and archive retention is unchanged.

Owner hold DTOs and state ACKs expose the receipt/reason fields. CLI recovery
shows bounded refusal reasons and exact-run final SHA, observed head, branch
and MR independently of archive availability and physical retirement; JSON
fields are additive and older APIs may omit them. Older/successor holds and
unstamped older completions, including earlier self runs, gain no authority.
Failed/cancelled/parked runs and `ci_fix`/`prompt`/`task` remain unchanged.
This adds no intent, endpoint, discovery, driver or E2E mechanism. #2544 quota,
#2545 failed publication and #2506 failed thin release are excluded.

See [ADR-1296](1296-durable-run-recovery.md#amendment-2026-10-09--2507-synchronous-completed-publication-release)
for proof bindings and [ADR-2417](2417-guarded-local-retention.md#amendment-2026-10-09--completed-publication-receipt-authority)
for physical cleanup and protected report retirement.
