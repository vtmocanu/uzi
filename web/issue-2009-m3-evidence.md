# Issue 2009 M3 web evidence

Base: d501d6e89895121cd32fb12b3ff8699d2ebd9193 on agent/issue-2009.
This is the exclusively owned web slice, left uncommitted for main to integrate.
No dependency installation, shared fixture edits, or broad gates were performed.

## Changes

- web/src/lib/apiTypes.ts: optional numeric pending_outcome_count and boolean
  pending_overflow beside reported_runs; documents fresh evidence, authoritative
  zero, and unavailable evidence omissions.
- web/src/components/WorkerPendingOutcomeBadge.tsx: aggregate count takes
  precedence even at zero; omitted count falls back to actual listed pending rows.
  Fresh overflow renders exactly “N confirmed pending; more may be pending”,
  including zero and missing reported_runs. Otherwise ordinary singular/plural
  wording remains, and zero hides the badge. Tooltip identities, generations,
  sanitization, and ages still come only from actual reported rows. No aggregate
  age, oldest annotation, synthetic row, or navigation is added.
- web/src/components/WorkerPendingOutcomeBadge.test.tsx: original M1 assertions
  unchanged; three added cases cover missing rows with zero/omitted count and
  overflow, plus an aggregate-only ordinary count without overflow.
- web/src/lib/apiContract.test.ts: populated owner/admin fixture assertions,
  optional-field omissions in zero DTO fixtures, and a typed custom-object JSON
  check for explicit zero/false versus omission. Existing key-set parity checks
  remain intact.
- web/src/mocks/data/workers.ts: existing busy laptop carries count 1 and fresh
  overflow true without invented snapshot rows; the admin list inherits it.
  Other partial legacy mocks retain omissions.
- web/src/mocks/data/workers.pending.test.tsx: owner/admin badge rendering of
  aggregate evidence without rows and legacy mock omission behavior.
- web/issue-2009-m3-evidence.md: this evidence record.

## Executed focused checks

All commands ran in the foreground using provisioned dependencies. Logs are in
the worktree scratch directory, contain command/revision/start/exit/wall time,
and were read after completion. Times below are shell wall seconds.

| Check | Result | Wall seconds | Log |
| --- | --- | --- | --- |
| Original M1 selection on base, before edits | exit 1; 6 failed, 1 passed, 6 skipped | 5 | .uzi/scratch/gate-log.c9hAnA |
| Issue-2009 badge selection after edits | exit 0; 10 passed, 6 skipped | 5 | .uzi/scratch/gate-log.8gsRma |
| Inverse badge selection plus pending/custody mock files | exit 0; 11 passed, 10 skipped | 5 | .uzi/scratch/gate-log.tNWABc |
| Oxlint on all six changed TS/TSX files | exit 0; 0 warnings, 0 errors | 1 | .uzi/scratch/gate-log.Q4laHp |

The two final Vitest invocations together cover all 16 badge tests: original
seven M1 cases, six legacy cases, and three added cases; they additionally cover
three pending mock cases and two existing custody mock cases. git diff --check
passed. A tracked literal search across web/src and e2e found legacy page tests
still expecting ordinary labels on payloads without aggregate fields and no
affected E2E assertions requiring edits. No slow/browser/E2E scenario was run.

## Integration still required

At the final inspection, worker.full.json and admin_worker.full.json did not yet
contain the new fields. The concurrent Go owner owns those shared fixture edits.
The new full-fixture assertions expect count 1 and overflow true, matching the
populated pointer contract; zero DTO fixtures must omit both fields. No fixture
was edited here and no keys were exempted from parity.

Contract Vitest (including the custom zero/false check), typecheck, build,
canonical gates, integrated Go/TUI checks, review, commits, checkpointing, and
M4 mutation/concurrency validation remain for main after both slices land.
There is no CI, browser, full-stack, or Kubernetes proof from this slice.

## Lead review cleanup

After the shared Go fixtures landed, the lead executed integrated badge/parity/mock
checks (224 tests at a8d01d69) and TypeScript checking successfully. Reviewer at
that SHA identified the local-object JSON round-trip test above as redundant:
its runtime assertions exercised only JSON built-ins. The lead removed those
assertions and retained explicit zero/false and omission assignments inside the
existing compile-time Worker contract block. Go DTO wire tests and actual badge
zero/fallback cases provide runtime behavior evidence. This is a test-only
cleanup; no production badge or API behavior changed. The cleanup receives
scoped committed-range review and validation before M3 completion.
