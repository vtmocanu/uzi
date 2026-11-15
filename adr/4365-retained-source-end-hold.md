# ADR-4365: Owner hold after a definitive retained source-end decline

**Status**: Implemented (M1–M3); exact-head CI and hosted owner acceptance pending
**Date**: 2026-11-15
**Related**: Issue #4365; [ADR-1783](1783-run-quiescence-and-attempt-clone-paths.md), [ADR-2396](2396-worker-memory-guard.md)

## Decision

A definitive source-end decline can hold a retained continuation at
`recovery_wait` / `retained_recovery_adoption_failed`, with
`recovery_wait_detail: source_end_declined` and no automatic retry.
This adds a classification, not a trigger, source-use permit or disposal authority.

The worker's evidence is process-local: a retired owner sink, the exact normalized
source key and current lease epoch, and a definitive decision not to end source use
because tails remain unjoined or remediation prevents it, after the admission
barrier. `AuthorityRuntime.markSourceEndDeclined` and `sourceEndDeclined`
(`agent/src/authority-runtime.ts`) bind and recheck the sink, lease, epoch,
current active slot, absence of pending operations and absence of a queued end.
Sink retirement alone, a pending operation, a queued end or an unconfirmed end
does not qualify. Those cases retain the existing transient bounded retry.
A restart loses the declined-end witness; it does not prove the old source reusable.

## Server contract and compatibility

The API advertises `retained_source_end_hold_v1` paired with
`run_registration_evidence_v1`. Reporting requires the current registration
nonce and claim generation and evidence durably recorded from a running report. A refused running transition may leave the row `claimed` while recording that evidence; its exact identity-fenced 409 can enable the hold capability without authorizing a running/model turn. The park and its server
evidence become visible together at transaction commit. That atomic visibility
does not make the server transaction atomic with independent worker source fsyncs.

Deploy the API before relying on this classification. A worker deployed first
against an API missing the feature keeps legacy behavior. An older API serving a
migrated row may clear its detail and requeue it on Resume under the existing cap;
it does not acquire the new API's refusal or counter-preservation semantics.
Worker source guards still prevent unsafe reuse. There is no new database trigger.

Compatibility proof scope: `TestSourceEndDeclinedLegacyResumeClaimFenceLiveDB`
uses the unchanged baseline Resume SQL and real `ClaimRun`: legacy Resume clears
the detail and queues, but the preserved released pair blocks the same registration
under D19. Repeated refusals preserve generation, requeue/park counters and open
custody. Changing only the worker nonce permits a generation-2 claim while preserving
the original hold; that admission grants no source-use authority. Worker tests at
three separate feature-absent seams cover held-source refusal for a differently
admitted claim. These are separate SQL-admission and worker-source proofs, not an
end-to-end claim that legacy Resume admits the same incarnation. The new SQL
regression awaits the lead's full store race and LiveDB runs.

On the new API, owner Resume is refused without writes when the original
registration is unchanged, or worker/registration identity is unavailable, even
if the worker heartbeats. A different, nonempty current registration permits
an explicit owner guarded attempt under the existing lifecycle, budget and
custody checks. It proves neither process death nor source usability.
No timer, heartbeat or registration change resumes the run by itself.

Cancel remains available and leaves retained-source custody intact. If the
worker or its identity cannot be recovered, Resume may remain refused permanently.
Separately authorized recovery from retained storage, an available archive or
verified restore is lawful; none promises the latest work. Discard is a separate
decision after backup verification, not an implication of Cancel or Resume.

Migration 00462 Down checks active and historical rows for
`source_end_declined` and refuses narrowing while evidence remains; it does
not erase that evidence. Migration 00463 only validates the widened constraint.

## Sanitized incident record

The supplied generation-3 trace records these events:

| Time | Observation |
|---|---|
| 03:33:49.441 | Checkpoint skipped: `secret_remediation_pending`. |
| 03:33:49.486 | Wall park: `published: false`, `degraded: false`. |
| 03:33:49.583 | Flight timeout final skip. |
| 03:33:53.018 | Plugin HOME preserved. |

Remediation holds had been present since 02:34. The supplied generation-3
trace has no disposal or source-active warning. Later generations 4–7 show
skipped disposal and an unjoined tail. These later observations do not establish
generation 3's cause.

**Inference boundary:** generation-3 causation is UNPROVEN. Silent held
remediation or an earlier exit is plausible; neither is established. No
historical settlement receipt or definitive declined-end witness is inferred.
The tests below discriminate the new branch, not the historical cause.

## Verification record

Evidence was read from the lead's M1–M3 records and bounded log excerpts.
These are local results at the recorded milestone revisions, not a full-suite
or final-head acceptance claim.

| Milestone | Recorded result |
|---|---|
| M1, `6814e978a` | Full workersvc, handler and store package race runs exited 0. Focused LiveDB runs reported 12 workersvc, 2 handler and 1 store tests, zero skips, exit 0. |
| M2, `120d4a634` | 471 affected tests and 87 ownership/completion tests passed, zero skips, exit 0. Codex M4: 209 passed, zero skips; completeness matrix satisfied (37 clauses, 64 executed results, Claude/U, Codex/U, Codex/P), exit 0. |
| Mutation | In a disposable export, restoring the legacy branch failed the first-hold assertion (actual 0, expected 1; exit 1). The export was removed; the unmutated final affected suite passed. |
| M3, `3fd2b948a` | CLI full package race run, web/API checks and targeted web/browser tests exited 0, including source-end owner actions at narrow and wide viewport sizes. |

The focused LiveDB evidence includes registration-lock ordering, state/Resume
and migration/legacy-Resume checks. Process restart and injected fault tests
exercise loss of local witnesses and protection of old active slots; they do
not establish physical power-loss durability.

Recorded M1 package wall durations were 273s, 478s and 135s respectively;
the API check took 733s. M2 affected tests took about 1272s; Codex M4 took
131s. These are separate measurements with differing cache states and reruns.
CI critical-path impact is unmeasured; no equivalent baseline was exported.

Exact-head PR CI, including LiveDB and the full affected component suites,
remains pending. Hosted post-merge observation of a two-source wall park
remains a maintainer responsibility: observe the park, retained sources,
owner actions and subsequent guarded attempt on the deployed build.
Record the exact deployed image and commit, selected source, claim-generation and hold-count deltas, and continuity of the retained work. Verify either same-worker continuation after every exact durable end is proved or one actionable owner hold. Include a second retained source and show that restart does not make it eligible without its independent guards. That acceptance is unverified and issue #4365 remains open.

The client retains one acknowledged running witness per distinct run for its registration lifetime; re-registration clears the map. Terminal pruning is not implemented. This avoids revoking an active flight's evidence when unrelated reports cross an arbitrary size threshold, at the cost of registration-lifetime memory growth. It grants no end or reuse authority.

## Cross-check repair evidence

The client also accepts recorded registration evidence on an exact claimed-409
response to its running report. Foreign or absent run/worker identity, a stale
claim, a credential-switch disposition or a changed registration does not supply
that capability. It changes neither the refused running ACK nor source admission.
Runner tests cover the claimed-state hold and absent hold-feature behavior at
all three refusal seams; missing registration-evidence support stays legacy.

Healthy `ended`, `no_sources` and `no_authority` teardown summaries use INFO;
non-end outcomes retain WARN. Both retain once-only, bounded secret-free fields,
and logging failures cannot change source disposition.

The new legacy Resume control runs the unchanged baseline SQL, then the real
`ClaimRun`. Same-registration claim attempts are excluded without generation,
requeue or custody changes; a changed-nonce positive control demonstrates that
the released-incarnation fence is the cause. It would be incorrect to fabricate
a subsequent same-incarnation claim on that path. Separately, the absent-feature
runner controls prove source protection at the three worker refusal seams.
These are complementary SQL and worker proofs, not a claim of one end-to-end
legacy same-incarnation execution.

## Related-site dispositions

- #3638 / ADR-2396: already accurate as the reused same-process memory source guard; the residual no-record restart wording is updated for #3644.
- #3639 / ADR-2396: already accurate as settled memory-park receipt delivery/consumption; this classification fabricates no receipt.
- #3644 / ADR-1783: already accurate as eligible independent verified-pin restore with two-source guards; no new restore authority.
- #4177 / #4254: already accurate as reused detail/remediation guards; secret-remediation refusal remains separate.
- #3831: already accurate as the unaffected bare-writer exclusion boundary.
- #4158: positive declined-end classification is covered here; the broader issue remains open.

These are implementation-scope dispositions, not remote issue-closing claims.
