# Issue 2009 M3 display integration evidence

Base revision d501d6e89895121cd32fb12b3ff8699d2ebd9193.
Changes are limited to additive worker DTO/handler overlays, CLI/TUI consumers,
shared owner/admin full fixtures, web contracts/badge/mocks, and focused tests.
No M2 storage, lifecycle, migration, dependency or workflow changes.

Two independent batched reads preserve actual reported_runs while adding optional
pending_outcome_count and pending_overflow pointers. Aggregate query failure logs
and omits both fields; snapshot read failure can retain independently valid
aggregate authority. Nil store is unavailable, never authoritative zero.
User/admin list and patch reuse the overlay. Old zero fixtures retain omissions;
full fixtures record count 1 / overflow true, and explicit pointer zero/false
marshals in focused Go wire cases. TypeScript checks zero/false and omission
compatibility at compile time, alongside actual badge behavior.

CLI and web use aggregate counts when available, otherwise listed pending counts.
Fresh overflow renders exactly "N confirmed pending; more may be pending", even
zero with empty/missing rows. Ordinary singular/plural wording applies otherwise.
CLI suppresses aggregate oldest age whenever authoritative count is present,
including equal counts. TUI adds only aggregate attention/detail text; actual
listed rows, known per-row ages, cursor/navigation and live phase counts remain.
Legacy optional-field omissions retain the existing rendering path.

## Lead executed integration checks

The cancelled Go delegation returned no handoff. The lead inspected its partial
diff and logs, then ran the final integrated files with a fresh owned Postgres
fixture. Logs below are scratch-only, foreground, with recorded exit/wall time.
The Go unit and JS commands used runtime-provisioned dependencies; no JS install.

| Selection | Exit | Wall seconds | Package or Vitest seconds | Scratch log |
|---|---:|---:|---:|---|
| Handler LiveDB routes/overlay | 0 | 12.741501478 | 7.406 | .uzi/scratch/gate-log.dEZ3Hq |
| Go DTO fixture and explicit wire contract | 0 | 2.047066147 | 1.120 | .uzi/scratch/gate-log.2d2tj0 |
| CLI/TUI affected selection | 0 | 5.536607887 | 1.549 | .uzi/scratch/gate-log.VRK60C |
| Web badge/parity/mock selection | 0 | 8.034679123 | 5.46 | .uzi/scratch/gate-log.dgOh0c |
| Web typecheck | 0 | 5.731406347 | — | .uzi/scratch/gate-log.xLRtLC |

Exact commands from api (unit checks unset UZI_TEST_DATABASE_URL):
~~~sh
go test -buildvcs=false -count=1 -v -race -p 1 -timeout 240s ./internal/handler -run '^(TestIssue2009PendingAggregateRoutesLiveDB|TestReportedRunsOverlayLiveDB)$'
env -u UZI_TEST_DATABASE_URL go test -buildvcs=false -count=1 -v -race -p 1 ./internal/apitypes -run '^(TestContractFixturesMatchMarshal|TestIssue2009PendingAggregateWire)$'
env -u UZI_TEST_DATABASE_URL go test -buildvcs=false -count=1 -v -race -p 1 ./cmd/uzi -run '^(TestIssue2009PendingAggregateJSON|TestReportedRunsCell|TestWorkerListShowsReportedRuns|TestIssue2009PendingAggregateTUI|TestWorkersNavigationFilterAndSplit|TestWorkerDetail.*)$'
~~~

Exact commands from web:
~~~sh
npx --no-install vitest run src/components/WorkerPendingOutcomeBadge.test.tsx src/lib/apiContract.test.ts src/mocks/data/workers.pending.test.tsx src/mocks/data/workers.custody.test.ts
npm run typecheck
~~~

Handler tests actually executed, with zero skips or race reports:
- Real authenticated owner/admin lists and owner patch return distinct eligible
  union count 2 despite only one listed snapshot row, without changing its age,
  generation, phase or ID. An empty worker returns explicit count 0 / false.
- Expired overflow closure ignores raw historical true; fresh closure yields true.
- Expired/wrong-generation/wrong-owner/released/terminal identity records do not
  inflate counts.
- Temporarily renamed identity table forces aggregate failure in all three routes;
  fields are omitted and original listed rows/ages remain intact.
- Temporarily renamed snapshot phase column forces snapshot read failure while
  aggregate authority survives; nil store yields [] and omitted fields.
- Both schema names are restored by serial test cleanup. No concurrent package
  tests ran against this fixture.
- Existing reported-run overlay assertions pass after the internal result wrapper
  change; they still assert actual rows and nonnil empty arrays.

All fourteen M1 CLI JSON cases now pass, including empty overflow, authoritative
zero, equal-count oldest suppression, larger count, unchanged live phase tally,
legacy omissions and overflow fallback. Seven TUI cases pass, checking attention
and actual rendered worker detail, known row ages and absence of invented rows or
navigation. DTO tests pass for both WorkerDTO and embedded AdminWorkerDTO with
legacy/zero/count-only/overflow-only payloads; generic fixture byte parity passes.
Web selection executes 224 tests across four files, with no skips. The full web
typecheck verifies the unchanged key-set parity assertions against final fixtures.

Docker fixture: postgres:17, session-owned cdr-2009-m3-lead-* name, trust auth for
synthetic user/database uzi on a random loopback-only published port. Mounted
e2e/test-database-marker.sql, checked TCP readiness, and set TMPDIR to absolute
worktree .uzi/scratch. EXIT trap removed only that exact container and anonymous
volumes. No paused fixture was reused.

Initial cancelled-agent attempts left failed setup logs: invalid patch field
(xAiI5v), test response decoder expecting wrong patch wrapper (muj81f), missed
test helper argument after signature change (pS6npL), and TUI fixture-age/render
assertions (Odi1KC). These are not behavioral mutations and no claim relies on
them; final files and final lead run correct those setup errors.
Its final handler log FQRgeI and TUI log 1HVSca pass, but the lead run above is the
integration evidence. Full-fixture mismatch log aZoO7Y prints actual populated
Go JSON; only the two owned worker/admin full fixtures change, and final byte
parity passes. Old zero/legacy fixtures are unchanged.

No CI or full-stack timing is claimed. This lead CLI selection includes additional
TUI/navigation tests, so its timing is not a matched M1 before/after measurement.
Exact matching regression timing and broader acceptance/mutations/canonical gates
remain M4. Web implementer evidence records its own targeted before/after selection.

At a8d01d69 reviewer and tester found no blocking M3 behavior defects. The reviewer
suggested removing the redundant TypeScript local-object JSON round-trip test;
the lead removed its runtime assertions and retained explicit zero/false and
omission type checks in the existing compile-time worker contract block.
Go wire serialization and real badge behavior remain the runtime evidence.

Broader committed-tree Go apitypes/CLI package tests passed (jWI8WS), along with
web Workers/Runs page integration (hkIOG0) and touched Go formatting (uBXLUQ).
The cleanup commit requires its own scoped review and TypeScript/contract checks
before marking M3 complete. No final delivery is claimed here.
