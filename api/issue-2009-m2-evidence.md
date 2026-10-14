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
