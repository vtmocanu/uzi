# Issue 2009 M1 API and CLI regression evidence

Production candidate: baa879cddab4ad52be6b50d4a7a70ce43601120b.
Pinned regression base: 25d59524d33d2b74156cad143956d04d9ca509e8.
Only test files were added/overlaid. No production, historical migration,
dependency or workflow changes. These enabled tests intentionally remain red
until M2 (health) and M3 (display).

## Final test results

Both revisions reach behavioral assertions, without compilation errors, timeout,
race reports or skips.

- TestIssue2009PendingAllSustainsHealthAcrossRotationLiveDB: both rotation and cap0
  fail. Initial aged pending rows produce the existing stalled/outcome-undelivered
  warning. The enclosing heartbeat epochs are accepted. After active omission,
  the warning becomes health="ok", reason="" despite a complete explicit
  pending_all identity list. Rotation has three such assertions; cap0 has four.
  The test uses two bounded heartbeats and SQL timestamp aging, without sleeps.
  Longer heartbeat/claim acceptance and exact persisted identity timestamps are
  M4 coverage, not proven by this M1 test.
- Six existing top-level LiveDB controls pass: valid heartbeat storage, invalid
  heartbeat rejection, stable pending-since renewal, legacy omission/reset,
  register continuity, and owner/generation/lease health fencing. The last
  includes four passing subtests.
- TestIssue2009PendingAggregateJSON: eleven of fourteen subtests fail identically
  on both revisions. Examples: empty rows render "-" instead of
  "0 confirmed pending; more may be pending"; an authoritative equal count still
  renders "1 pending outcome (oldest 7h 0m)"; aggregate-only positive counts render
  "-". The public worker-list command is exercised after JSON DTO decoding.
  Authoritative-zero/empty and two legacy fallback controls pass.
- Existing TestReportedRunsCell (ten subtests) and TestWorkerListShowsReportedRuns
  pass on both revisions.

## Exact commands and timings

Commands execute from each revision's api directory:

Health selection:
~~~sh
go test -buildvcs=false -count=1 -v -race -p 1 ./internal/workersvc -run '^(TestIssue2009PendingAllSustainsHealthAcrossRotationLiveDB|TestPendingSinceStableAcrossRenewalsAndFlagsLiveDB|TestPendingSinceResetsLiveDB|TestPendingSincePreservedByRegisterLiveDB|TestPendingOutcomeHealthRequiresCurrentOwnerGenerationLeaseLiveDB|TestHeartbeatStoresValidSnapshotLiveDB|TestHeartbeatIgnoresInvalidSnapshotLiveDB)$'
~~~

CLI selection:
~~~sh
go test -buildvcs=false -count=1 -v -race -p 1 ./cmd/uzi -run '^(TestIssue2009PendingAggregateJSON|TestReportedRunsCell|TestWorkerListShowsReportedRuns)$'
~~~

| Revision/selection | Exit | Wall seconds | Package seconds | New health test seconds | Scratch log |
|---|---:|---:|---:|---:|---|
| Candidate health | 1 | 160.375028518 | 9.130 | 0.85 | .uzi/scratch/gate-log.UBz9K2 |
| Candidate CLI | 1 | 86.299704804 | 0.410 | — | .uzi/scratch/gate-log.Uyh0V8 |
| Pinned health | 1 | 158.848445154 | 2.215 | 0.83 | .uzi/scratch/gate-log.TS0rZv |
| Pinned CLI | 1 | 58.004745462 | 0.481 | — | .uzi/scratch/gate-log.vt3T31 |

The new CLI top-level test took 0.06s candidate, 0.04s pinned.
Wall times include Go command preparation/build overhead and are not test-body
times. Candidate health includes fresh database migration (valid heartbeat test
7.14s); pinned health reuses the migrated database (valid heartbeat test 0.22s).
Thus package/wall differences are not a clean performance comparison. No
post-fix or CI measurement exists yet; do not infer a regression or improvement.
Both new health subtests use newly seeded fixtures; their bounded behavior is
comparable, but the database was reused sequentially.

Each command ran once over the final test files with this wrapper, from the
worktree root (tree identifies real checkout or archived base):
~~~sh
log=$(mktemp "$root/.uzi/scratch/gate-log.XXXXXX")
start=$(date +%s%N)
rc=0
(cd "$tree/api" && <selection>) > "$log" 2>&1 || rc=$?
end=$(date +%s%N)
printf '\nTREE=%s\nCOMMAND=%s\nEXIT=%s\nELAPSED_NS=%s\n' "$tree" "$command" "$rc" "$((end-start))" >> "$log"
~~~

## Setup and comparison limits

Docker server was available. A fresh session-owned container named with prefix
cdr-2009-m1-lead- was created from postgres:17, using POSTGRES_USER/DB=uzi,
POSTGRES_HOST_AUTH_METHOD=trust, and a random loopback-only host port. The
e2e/test-database-marker.sql file was mounted into its init directory. TCP
pg_isready verified readiness. UZI_TEST_DATABASE_URL pointed only at this
throwaway. TMPDIR was the absolute worktree .uzi/scratch directory.
The script's EXIT trap removed only that exact owned container and its anonymous
volumes; teardown did not target any other container. No long waits in tests.

Pinned source was extracted by git archive into
.uzi/scratch/snap-m1-lead-base (not a checkout/worktree). The final
issue_2009_pending_health_livedb_test.go and worker_test.go were copied into it.
api/go.mod and api/go.sum compared byte-identical with cmp. No baseline
production or helper adaptation was needed. Git commands ran only in the real
checkout. Both revisions used the same runtime toolchain/module cache and the
same sequential throwaway database at migration 333. No dependencies installed
manually. The named controls also used unique fixtures.

Earlier cancelled coder attempts left completed logs
.uzi/scratch/gate-log.loRr1K, .uzi/scratch/gate-log.Ct2gsF and
.uzi/scratch/gate-log.0hTsub, which independently show assertion failures.
They predate the final CLI column assertion and are not validation of the final
test file. Their command/setup metadata was not returned reliably; no timing
comparison or completion claim relies on them. Two delegation cancellations
required the lead to finish final checks.

Canonical gates and broader lifecycle/locking acceptance remain for M2–M4.
This milestone demonstrates the pre-fix failures, not a shipping fix.
