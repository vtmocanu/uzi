# ADR 2009: explicit pending outcome identities

Status: accepted for issue #2009.

Workers report optional `pending_all: [{run_id, claim_generation}]` from the once-captured terminal-journal list, independently of the existing active snapshot rotation and caps. Boot registration still has an empty active list. The list carries neither an outcome payload nor a worker clock. A worker release and fleet roll are required for coverage; pending_all itself has no capability gate and the API never infers pending outcomes from owned but unlisted runs. The later negotiated claim-only exception is recorded below; this base identity-reporting contract remains ungated.

The API decodes this field independently of strict active decoding and `finalize_resume`. Missing or null means unavailable. An authoritative empty array clears this worker's identity evidence. Each member requires a UUID and an explicit nonnegative integral int64 generation; tuple duplicates, missing members, malformed values and arrays exceeding `ActiveSnapshotMaxEntries` (default 256) invalidate the entire optional field. The API warns with worker context and drops it without truncation, replacement or renewal. Otherwise valid active and overflow evidence continues to apply.

Only an accepted enclosing snapshot can synchronize identities. All four application paths (register, heartbeat, claim and `ReplaceWorkerActiveRuns`) capture and prelock identity targets using the existing worker → ordered parents → ordered targets lock order. Identity writes require the current owner, exact reported generation, unreleased claim and eligible nonterminal run, and recheck frozen kind, target and cross-check association against the held parent set. The identity list is never active/readoption input or a finalize attestation.

`worker_pending_outcome_identities` stores API `first_seen_at` and `reported_until` keyed by worker, run and generation. A complete list atomically removes omitted tuples and renews eligible tuples using `TerminalPendingLease`, independently of overflow. Continuing fresh identities retain their age, using the earliest still-fresh valid prior identity or active-snapshot evidence captured before replacement. An expired identity without independently fresh valid snapshot evidence starts a new interval at API now. Missing, null, malformed or rejected snapshots do not refresh evidence. Fresh reads exclude obsolete ownership, generation, release and terminal state before cleanup. Worker-local pruning accompanies valid synchronization; a bounded indexed expiry batch runs best effort during unconditional existing sweep maintenance, including health-disabled and no-active-run cases. Foreign-key cascades clean up removed workers and runs.

Health uses the earliest fresh identity/snapshot age for the current owner and exact generation with an unreleased claim. The existing status/kind allowlist, warning rung, four-heartbeat threshold and reason remain unchanged: “the run's outcome is journaled on its worker but has not been delivered”. Under the original #2009 contract, this is display and health evidence only. It grants no recovery, custody, reconciliation, cancellation, parking or pause authority, and does not change existing overflow lifecycle exclusions or snapshot lease predicates. The scoped #3607 amendment below permits a fresh transactional claim to use complete identities as one part of its separate negotiated proof; identities alone still confer no claim authority.

Worker views expose optional `pending_outcome_count` and `pending_overflow`. The count is a distinct tuple union of fresh eligible snapshot and identity evidence; overflow uses the unexpired `workers.pending_overflow_until` closure rather than its historical raw flag. Query failure omits these fields, retaining legacy listed-row fallback; explicit zero and false are authoritative. Fresh overflow displays exactly “N confirmed pending; more may be pending”, including N=0. Otherwise the usual singular/plural pending-outcome wording applies. Live phase counts and actual per-run details remain unchanged. No identity, row, ID or age is fabricated. An authoritative aggregate count suppresses the aggregate oldest-age annotation even when it equals the listed count, because membership may differ; actual known row ages remain visible.

## Scoped amendment (#3607, 2026-10-31)

[ADR-3607](3607-compact-policy-hold-m1-isolation.md) records the owner-approved narrow
generic `pending_outcome_scoped_claim_v1` protocol exception. It adds optional
`pending_policy_protection: [{run_id, owner_generation}]` alongside complete,
untruncated `pending_all`. Original source generations stay in pending_all;
a group identifies a separately proven positive current owner generation. There
is no fabricated historical lease or new policy-readiness capability. The API
does not advertise `provider_policy_hold_v1` readiness.

Two-sided negotiation and the locked worker's capability, a freshly applied
nonce/epoch and worker → ordered parents → frozen targets locks are required in
the snapshot-carrying claim transaction. Fresh rows must still be owned, unreleased
and claimed/running at the stated owner generation. Each group counts one against
`WorkerOutboxMaxPending`; ordinary tuples outside groups count separately.
`ActiveSnapshotMaxEntries` remains the independent absolute inventory ceiling
(default 256), not a replacement for the real pending cap.

Historical grouped sources may legitimately insert zero identity rows. Ordinary
tuples must independently pass the exact-generation attribution/frozen SQL fences
and insert/update one row; otherwise the exception fails. The proof is a trusted
worker attestation of grouping/completeness, not cryptographic filesystem proof.
An UNKNOWN source with active=[] can be accounted for without inventing an owner
row lease. Opaque or incomplete source inventory cannot claim complete coverage.

Register, heartbeat and the compatibility replacement entrypoint do not grant the
exception, and no persisted exemption is added. Overflow persists, candidate SQL
still excludes work attributed to the overflowed worker, and bodyless ordinary,
ephemeral and no-transaction paths are unchanged. On feature loss/downgrade the
exception ceases while retained evidence and existing protection stay active.
Delivery ACK, custody, source protection and retirement remain separate.

## Accepted coverage limits

Coverage requires a complete explicit list within the independent ceiling and an otherwise valid accepted enclosing snapshot. Lists over the ceiling provide no new identity coverage. Independently valid legacy snapshot evidence may still contribute.

The human decision preserves active construction, rotation and caps, including duplicate-run rejection. Two installed terminal journals for the same run at generations 1 and 2 with active pending cap 2 can still emit both generations in `active`. The API rejects that enclosing snapshot as a whole with `duplicate run_id in snapshot`. Even a complete `pending_all` in that rejected snapshot provides no insertion, renewal or pending_all coverage. Existing mode-specific rejection behavior is unchanged; this issue includes no unique-run projection or generation-selection correction. The duplicate-active defect is tracked separately as incidental finding `bc1020a6-907e-47ff-b1da-1bc1be5b5c6a`. The captured real-journal fixture and API route tests preserve this limit explicitly.
