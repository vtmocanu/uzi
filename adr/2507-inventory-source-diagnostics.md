# ADR-2507: Inventory source refusal diagnostics

Status: Accepted
Refs: #2507

## Context

Guarded recovery previously recorded only `inventory_source_not_quiescent`, obscuring which source proof refused FINAL. The observed automatic hold-release failure still requires live evidence. This change adds diagnostic evidence while preserving verified/unknown decisions, custody protection, immutable archive and FINAL identities, and the existing #2458 cleared-journal and #2433 missing-parent fixes.

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

Older workers can authenticate these records, but their needs-action allowlist does not recognize the new suffixes, so retry of those records stops on downgrade. This compatibility risk is accepted. No protocol, API, server DTO or CLI transport changes are made. CLI presentation is deferred because the server DTO carries no local diagnostic; any later exposure requires its own contract.

## Validation and follow-up

Focused reader, runner and coordinator regressions cover failure classification, initial/final proof failures, observer isolation, sibling ordering, authenticated persistence, completed issue and MR-rework provenance, exact retry matching, repeated backoff and immutable replay. Live worker evidence and the original automatic release fix remain follow-up work outside this diagnostic change.
