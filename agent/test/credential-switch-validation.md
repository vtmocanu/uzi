# Live Codex switch: recovered validation record

This is a partial evidence ledger for issue #4202, recorded on 2026-11-17.
It does not declare m3 or m4 complete. Earlier scratch logs were absent after
recovery; historical commit messages are not treated as passing checks.

Candidate tested: 73e662c846ac9df92356dd61831967167c44d8ae.
The primary worktree remained unchanged during the controls below. Both mutants
were applied only to disposable exports of this exact candidate, with provisioned
agent dependencies linked into each export. No dependencies were installed.
Each export was removed after its command exited.

## Historical refusal and proof classification

Command:

```sh
git show a233883de3c36bae2e940f999cd9d582889dad00:agent/src/runner.ts
```

At that revision, lines 25735–25738 in `enterCredentialSwitch` read:

```ts
    if (this.isCodexCapture(claim, flight)) {
      this.retainCaptureSource(flight);
      runLog.warn("Codex credential switch refused; source and session retained");
      return "retained_stop";
```

The retained entry also refuses at lines 27130–27133. These are refusals of
the whole operation, not a prior allowed-switch ordering. The following two
proofs are **ordering controls**, class (c) of the owner proof-method amendment.
They are not pre-feature baseline evidence.

## Real Runner ordering controls

Both exports first ran `npm run typecheck` from their agent directory, exit 0.
Each then ran:

```sh
node --import tsx --test --test-timeout=120000 \
  --test-name-pattern='M2 actualRunner .*account resumes saved thread' \
  test/codex-executor.test.ts
```

The cases exercise the actual Runner/Codex switch path: subscription to API key,
API key to subscription, and subscription to subscription, each during factory
preparation and an already running body.

| Control | Exact candidate change | Observed result |
| --- | --- | --- |
| Reconciliation ordering | Move the existing awaited `a.reconcileA(bounds, remaining())` ready check from immediately after executor suspension to after the rollout-persistence try/catch. | Exit 1; 0 passed, 6 failed; assertion `A-reconcile-ready precedes A-persist`. |
| Now drain ordering | Move the existing awaited `executor.suspendCodexEpoch!(bounds)` call ahead of `nowSummary?.suspendCodexSwitch(...)`, rather than after its awaited drain. | Exit 1; 0 passed, 6 failed; assertion `Now switch suspension precedes executor suspension`. |

The unchanged candidate then ran that same command: exit 0, 6 passed, 0 failed.
The controls therefore demonstrate sensitivity to these two specific call
relocations through the real Runner path, not merely through a helper.

Candidate anchors (line numbers at the tested SHA): Runner ordering at
`agent/src/runner.ts:17386`; completed A reconciliation assertions at
`agent/test/codex-executor.test.ts:33259`; Now drain observations at
`agent/test/codex-executor.test.ts:33857`.

Initial experimental setup failures were excluded: a relocation between
`try` and `catch` did not compile, and an export without its scratch directory
failed with ENOENT. Neither counts as regression evidence.

## Additional checks observed on this candidate

From agent:

```sh
node --import tsx --test --test-timeout=120000 \
  --test-name-pattern='M2 actualRunner credential replacement|Codex repeated actualRunner switching' \
  test/codex-executor.test.ts
```

Exit 0: 85 passed, 0 failed, 211491.665 ms. This includes repeated A→B→C
switching plus the actualRunner replacement matrix. It is a focused run,
not the full agent suite.

From the worktree root:

```sh
UZI_DEADCODE_AGENT_REQUIRED=1 task check:agent
git diff --check HEAD
```

Both exited 0. The task's observed stages were dependency check, lint,
deadcode and TypeScript checks.

## Transactional recovery and compatibility repair

On 2026-11-17, the lead recreated a unique throwaway PostgreSQL 17 container
using the already available image, with no persistent volumes and a loopback
port. The container was removed by exact name after the command exited.
With its database URL in UZI_TEST_DATABASE_URL, the lead ran from api:

```sh
GOFLAGS=-p=1 GOMAXPROCS=4 go test -race -count=1 -v ./internal/workersvc \
  -run 'Test(Codex.*(Switch|Apply|Transactional|RefreshAdmitted|InitialSubscription)|OwnerCodex|SetRunCredentialCodex)'
```

At 2f1808dbadc98a3d7980c5d75af6fc08333d7de7 this passed, exit 0:
49 top-level tests, no skips, package execution 64.278 seconds.
This includes apply rollback, lost replies, account-lock ordering, refresh
admission, policy races, stopped settlement and replay fences. It is not the
full API suite.

Independent DBA review then found that migration 00464 had replaced the
supplied default branch's worker-roll migration. Commit 7cd40f3b1 had renamed
the Codex additions into the occupied version instead of preserving it.
The repair restores the default branch's 00464_worker_roll_marker.sql
byte-for-byte and moves the Codex snapshot and pending-target additions to
465 and 466 respectively. Final landing-time renumbering is still required
if the default branch advances.

The same focused command passed again with this repair and the following
test changes present, exit 0, package execution 72.306 seconds:

- The transaction snapshot compares complete ordered base/switch journal
  rows, rather than row counts.
- The spend join requires the exact original A credential at ordinal zero.
- TestCodexSwitchMigrationUpgradeLiveDB checks a fresh schema, downgrades
  only its independently created fixture database to version 464, confirms
  the preserved worker-roll columns and absent Codex additions, then upgrades
  and checks both sets of columns. Migrations at or below 464 match the
  supplied default-branch schema after the repair.

The same new migration test was copied into a disposable export of pre-repair
2f1808dbadc98a3d7980c5d75af6fc08333d7de7, leaving that revision's defective
migration set intact. With a separate fresh PostgreSQL fixture, the focused
command was:

```sh
GOFLAGS=-p=1 GOMAXPROCS=4 go test -race -count=1 -v ./internal/workersvc \
  -run '^TestCodexSwitchMigrationUpgradeLiveDB$'
```

It failed with exit 1 at
`worker_upgrade_reports: found 0 migration columns, want 2`.
The export and container were removed after the command exited.
The repaired candidate passed that test in the repeated focused run above
(7.93 seconds for this test). This is pre-repair regression evidence for the
migration collision; it is not a live-switch ordering control or evidence
against a233883de.

### Review scope and remaining merge regressions

Read-only reviewer, tester and DBA inspected the supplied-base range
9d62bc7925371e10d6e2833b43d469367842f8d7..2f1808dbadc98a3d7980c5d75af6fc08333d7de7.
The tester's two assertion findings are addressed by the changes above.
The DBA's migration blocker is addressed at the schema level, but broader
compatibility required further reconciliation: that reviewed revision had not
integrated the supplied default at all. Merge 5b16bb133 had integrated older
default 912661599 instead, leaving 20 later default commits absent. The
apparent removals included roll-marker reporting, hibernation roll history,
usage-poller refresh and Board coverage. A subsequent merge of supplied
9d62bc7925371e10d6e2833b43d469367842f8d7 restores these changes rather than
reimplementing or omitting them. Integration checks after that merge remain
separate from the pre-merge results recorded above.

Non-blocking notes from those reviews remain recorded for disposition:
the reviewer identified coordinator orchestration called only from tests,
and no comparable CI before/after critical-path measurement; the DBA noted
that interrupted post-refresh cleanup may leave switching busy until another
refresh recovers the committed account. No production failure was executed
for these notes, and no new cleanup subsystem is introduced by this repair.

## Outstanding evidence

This ledger is deliberately limited to checks re-observed after recovery.
It does not replace the frozen requirement-to-verification map, transactional
and LiveDB evidence, the rest of the required regression controls, independent
committed-range review, `task test:codex-m4`, or m4 security and contract gates.
Those items remain to be reconciled or executed; no scope is waived here.
Full suites remain pending exact-head PR CI under the approved boundary.
Live cross-account thread resume with two real accounts remains owner acceptance,
unverified and non-closing.
