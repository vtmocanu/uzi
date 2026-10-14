# Issue 2009 M2 API contract evidence

Implemented against M1 revision 19c7e1ebe2b8c795a2f6840de55bb4204a7b1f20.
Worker wire changes are documented in agent/issue-2009-m2-evidence.md.

The API independently decodes and validates optional pending_all. Accepted enclosing
snapshots synchronize complete eligible identity tuples in a transaction; invalid,
missing, null or rejected enclosing snapshots do not renew coverage. API first-seen
continuity uses the earliest fresh eligible identity or legacy snapshot evidence.
All four snapshot application paths add identities only to lock capture/prelocking,
not active rows or finalize/readoption inputs. Identity upserts add exact reported
generation, current ownership, unreleased claim and nonterminal predicates to the
existing frozen kind/target/parent/association fence.

Migration 00334 creates worker_pending_outcome_identities with worker/run cascades,
a tuple primary key and an expiry index. Health takes the earliest fresh eligible
snapshot/identity age without changing its status/kind allowlist, warning rung,
threshold or reason. The aggregate query is prepared for M3; DTOs/consumers are not
wired in this milestone. Indexed expiry pruning is unconditional sweep maintenance
and best-effort; complete-list application also prunes worker-local expired rows.
No historical migration, lifecycle query, dependency or workflow changes.

## Initial executed contract

Fresh session-owned loopback-only Docker Postgres, postgres:17,
POSTGRES_HOST_AUTH_METHOD=trust, user/db uzi, random published loopback port.
e2e/test-database-marker.sql mounted at initialization. TCP readiness checked.
UZI_TEST_DATABASE_URL referenced only this fixture; TMPDIR was absolute worktree
.uzi/scratch. EXIT trap removed only the exact cdr-2009-m2-lead-* container created
by the lead. No fixture from the previous pause was reused.

From api:
~~~sh
go test -buildvcs=false -count=1 -v -race -p 1 -timeout 240s ./internal/workersvc -run '^(TestPendingAll.*|TestIssue2009PendingAllSustainsHealthAcrossRotationLiveDB|TestPendingSince.*LiveDB|TestPendingOutcomeHealthRequiresCurrentOwnerGenerationLeaseLiveDB|TestWorkerRecoveryLedgerEvaluation|TestHeartbeatStoresValidSnapshotLiveDB|TestHeartbeatIgnoresInvalidSnapshotLiveDB)$'
~~~

Scratch log .uzi/scratch/gate-log.sCOKGu records exit 0, elapsed wall
119.967962809s, package 10.187s. Named tests/subtests execute with zero skips or
race reports; new migration runs successfully to version 334.

- Four compatibility/heartbeat/register/claim paths insert exact identity evidence,
  leave active rows empty, preserve age without renewing on missing/invalid lists,
  renew complete lists, and clear explicit empty lists.
- Independent optional decoding covers missing/null/empty, zero/max-int generations,
  wrong shape/member types, missing/null required members, fractional/string/out-of-
  range generations, combined with missing/null/invalid/valid finalize_resume.
- Semantic UUID/generation/duplicate/ceiling errors preserve enclosing active
  acceptance without replacing or renewing identity evidence; duplicate active
  rejection blocks table changes.
- Distinct reported generations for the same run are accepted as tuples, but only
  exact current generation is stored. Omission deletes exact tuples.
- Fresh legacy age seeds continuity before snapshot replacement; expired identity
  reappearance without fresh snapshot resets age.
- Batched aggregates execute live: distinct snapshot/identity union counts once,
  empty worker count is zero, expired overflow closure ignores historical true flag;
  obsolete ownership/generation/release/terminal evidence cannot seed/count.
- Expiry pruning runs through Sweep with health disabled and no active snapshot rows,
  leaving the run running.
- M1 rotation/cap0 health regression now passes (0.69s total vs M1 0.85s in its
  original selection). These differing selections/migration/setup circumstances
  do not establish a performance improvement. Exact matched timing remains M4 work.
- Existing pending-since and snapshot/recovery-ledger controls pass.

The lead then strengthened the eligibility test to assert exact first-seen and
reported-until nonrenewal after each ineligible report. That additional assertion
must be executed by the committed-tree focused integration check/review; the
initial log above predates it and is not evidence of that extra assertion.

The cancelled API coder returned no handoff, so its partial source/tests were
inspected by the lead. Its decoder logs ruvYGN/77Dixd record passes; SQL generation
log C7kB0w shows an earlier failure (sqlc could not infer multi-array unnest), corrected
to paired SELECT unnest columns before generated code and live execution. No
completion claim relies on the cancelled agent or its interrupted LiveDB log.

M2 does not claim M3 display completion or M4's full acceptance, mutation,
lifecycle/locking concurrency matrix, canonical gates or CI execution.

## Committed contract integration and review disposition

At ca1eb4d3158926eb8ef1858a8b160b924557011a the lead repeated the initial
LiveDB command against a newly recreated fixture. Log
.uzi/scratch/gate-log.BhdER7 records exit 0, wall 146.439715515s, package 10.646s,
with zero skips/race reports. This run includes the strengthened exact-timestamp
nonrenewal assertion after ineligible identity reports.

Agent npm run typecheck passed at that revision (CgUUqz, exit 0, 6.683452302s).
task check:sqlc-drift passed (FlTCAE, exit 0, 4.986158544s), regenerating with
pinned sqlc v1.31.1 and leaving generated code unchanged.

The lead's broader workersvc unit selection at ca1eb4d3 failed:
~~~sh
env -u UZI_TEST_DATABASE_URL go test -buildvcs=false -count=1 -v -race -p 1 -timeout 600s ./internal/workersvc -skip 'LiveDB$'
~~~
zx8eqJ records exit 1, wall 6.624362851s, package 0.758s, at the sweep test-double
panic described below. It is a reported failed check, corrected by the test-only
repair; a green full selection over the repaired commit is still required.

Reviewer, tester and auditor reviewed 19c7e1eb..ca1eb4d3 without mandatory findings.
The reviewer independently passed decoder checks; the tester independently
passed them under race detection (zrSHD1). The auditor inspected byte-bounded
intake and frozen ownership/lifecycle separation, but ran no live concurrency
tests. Its unavailable govulncheck instrument is a limit, not a passed scan.

Open optional review suggestions for final accounting:
- Tester at ca1eb4d3: decoded UUID/generation value assertions (especially max-int64)
  are deferred to M4's optional-validation controls; coverage-threshold suggestion
  is deferred outside the approved scope (two suggestions total).
- Tester at M1 19c7e1eb: coverage-threshold suggestion is likewise deferred
  outside the approved scope (one suggestion).
- Reviewer at ca1eb4d3: extra DBA plan/locking review is deferred (one suggestion).
  The required auditor review ran, and planned M4 locking/concurrency controls
  remain required. No demonstrated locking defect was accepted or deferred.

## Integration test-double repair

The following identical focused command ran from each revision's api directory,
with UZI_TEST_DATABASE_URL unset:
~~~sh
time env -u UZI_TEST_DATABASE_URL go test -buildvcs=false -count=1 -v -race -p 1 ./internal/workersvc -run '^TestSweepClearsTheStreakWhenItGrantsAFreshAttempt$'
~~~

- Candidate ca1eb4d3158926eb8ef1858a8b160b924557011a:
  .uzi/scratch/gate-log.u2w8z9, exit 1, wall 6.33s, package 0.143s;
  stale-worker_requeue panicked in the promoted nil-interface method
  autoStopSweepStore.PruneExpiredPendingOutcomeIdentities.
- M1 base 19c7e1ebe2b8c795a2f6840de55bb4204a7b1f20: exported with git archive
  into .uzi/scratch/snap-m2-fake-base; .uzi/scratch/gate-log.NLeiW7, exit 0,
  wall 165.06s, package 1.147s; both named subtests passed. No Git commands ran
  inside the export. Timing includes compilation and is not a performance claim.

Inventory of full Service.Sweep unit callers found autoStopSweepStore in the two
Autostop sweep tests; fakeStore in service, chat, poolresume, recovery_expired,
recovery_stalled, recoverywait, limit_reeval, limitpark and persistfail tests; and
backfillClockErrStore embedding fakeStore in the checkpoint-retention error test.
The existing fakeStore already implements pruning. TestSweepPrunesTheTracker uses
fakeStore, not persistFakeStore, so the latter needs no repair. Salvage and
retention-pass unit tests call individual passes rather than full Service.Sweep.
Live-DB sweep fixtures use initialized query stores, directly or through wrappers,
and retain real pruning. Existing TestPendingAllSweepPrunesWithHealthDisabledLiveDB
covers unconditional cleanup with health disabled and no active snapshot rows.

The repair adds only an explicit zero-row/no-error prune stub to autoStopSweepStore,
whose fixture has no pending outcome identities. No existing assertion is weakened.

After repair, from the candidate api directory:
~~~sh
time env -u UZI_TEST_DATABASE_URL go test -buildvcs=false -count=1 -v -race -p 1 ./internal/workersvc -run '^(TestSweepClearsTheStreakWhenItGrantsAFreshAttempt|TestSweepReportsAutoStopsAndRunsAfterTheDetector|TestSweepPrunesTheTracker)$'
~~~

.uzi/scratch/gate-log.rlDNwe records exit 0, wall 140.29s, package 1.145s.
All three named tests and both fresh-attempt subtests passed with zero skips and
no race reports. Toolchain-selected gofmt check on autostop_test.go produced no
filenames and exit 0 (.uzi/scratch/gate-log.1x9zlJ). The owned base export was
removed after comparison. Main repeats the broader unit selection over the
repaired committed tree before M2 completion.

The lead restored this evidence from the original committed text after an
incorrect read-tool result access replaced it in intermediate commit d0017668.
No production or test source was affected by that documentation mistake.
No final delivery or canonical M4 gate completion is claimed here.
