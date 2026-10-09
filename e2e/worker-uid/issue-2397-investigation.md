# Issue #2397: partial worker-UID investigation

The approved M3 diagnostics-only disposition is complete; cause diagnosis and
fix are **not completed**. No controlled defect RED was found, M2 was not
attempted, and there is no cause regression test. Privileged image acceptance
remains pending post-finalize ordinary PR CI, observed by the maintainer.

## Explicit human decision

On 2026-10-07, after the evidence question, the human supplied this decision:

> The detail you asked for does not exist anywhere retrievable.
>
> Finish M1 (the diagnostics that make the lane print the failing assertion or cancellation reason), commit it, and signal completion. The PR's own CI run of test-agent-worker-uid is the evidence lane from now on.
>
> Do not start M3 and do not claim a cause or fix. The PR must use "Refs #2397", not "Closes".

Historical milestone disposition under that decision (the approved M3
diagnostics-only exit is recorded below):

- **M1 — diagnostics: completed.** This delivery is diagnostics-only; the quoted
  instruction records the human decision, not proof of a cause or fix.
- **M2 — investigation: partial/deferred; original M2 unmet.**
- **M3 — verified-fix: not started, deferred by explicit instruction; M3 unmet.**

The local environment reached the 900s image-build timeout before worker-UID
integration tests ran, as recorded below. Per the human/lander check, historical
JUnit existed only in runner `/tmp`, not in retrievable artifacts.

For #2390, the lander reported `[test:agent:worker-uid] context canceled` after wrapper
unit tests and before entrypoint output. This observation was absent in the
passing comparison and the #2377 failure; it does not establish a cause.

#2397 stays open. The PR uses `Refs #2397`; diagnosis and fix are deferred until
enhanced CI supplies a diagnostic and a controlled reproducer exists.

## Observed local attempt

At diagnostic-only `752f4c85` atop `d0b98093`:

- `docker info`: Docker 29.8.0, exit 0; no cached `wuid-2134-base:local` image.
- `npm --prefix agent ci --ignore-scripts`: exit 0.
- `timeout --kill-after=30s 900s task test:agent:worker-uid`: exit 124.
  Docker-free tests passed (11 checker, 7 wrapper, 1 truth-table); the inventory
  parser yielded 29 required leaves, including 7 production advice teardown leaves.
  The image build reached runtime step 3/31 (`apk add`) and stopped at the outer
  deadline. No completed image identity is available; **zero real worker-UID
  executions** occurred. The lane did not pass.

The supplied record cites `.uzi/scratch/gate-log.39IqrJ`, lines 56–62. Scratch is
ephemeral; the commands and results above do not depend on its persistence.

## CI evidence and supplied reports

These historical assertions were supplied, not locally reproduced:

- #2377: run `37542424541`, job `112538741355`, reportedly failed the symlink
  case; an unchanged-head rerun at `224890fc` passed. This supports classifying
  #2377 as intermittent; its underlying cause remains unresolved.
- #2390: run `37568169160`, job `112620705472`, failed at `2cb00271`;
  run `37569194788` passed at different head `62749048`. There is no unchanged-head
  proof. A shared cause versus a separate regression remains unresolved.
- A new human issue comment reports main run `37575030278`, job `112641997912`,
  at `876e3b5f` failing the single-UID case generically. This expands reported
  scope, without establishing a root cause.

Granted `get_pipeline_jobs(37542424541)` currently returns successful rerun job
`112543141140`, without the old assertion. Granted jobs for #2390 confirm failure
but supply no diagnostics. Original heads `224890fc`, `2cb00271` and `62749048`
are absent locally; merged commits are not controls at those exact heads.

## Source findings and unverified lead

This is a starting point, not an exhaustive production dependency enumeration.
In `agent/test/codex-executor.test.ts`, owner/symlink refusal tests at 6113/6133
and single-UID at 6191 use `runnerTeardownFixture`. The `makeFixture` import at 48
has calls at 946/1131 in unrelated decoded-usage-limits and approved-policy-flow
suites (940/1127). `pattern.mjs:6` anchors full inventory names, which exclude
those suites.

`agent/test/fixture-repo.ts` contains Node builtin imports and declarations, with
no import-time execution of `makeFixture`. That function (102) copies the local
environment (114) and filters `GIT_` keys (116–117). Merged #2390 helper diff
`e74ae641` changes that function body. No direct link to the selected fixtures
is established; this does **not** prove independence or absence of regression.

In `agent/src/codex/codex-executor.ts:1123`, disposal checks `outcome.clean`;
`rmRunnerTeardownTree` runs at 1129–1130 after confirmed clean disposal, otherwise
1134 warns that data is retained. Both refusal tests assert the cleanup-failed
warning. Setup, assertion or finalization failures can produce different failures.
This gate is an unverified investigation lead, not evidence of a timeout, order
or race cause. Preserve forensic retention.

## Next evidence

Accepted lander guidance is to use PR CI `test-agent-worker-uid` with enhanced
reporting. Capture the failing diagnostic, originating file, stack, type and
status, immutable head, image identity, runtime and inventory; then establish a
controlled reproducer before a behavioral fix. Repeated passes are insufficient.

## Approved M1 extension (2026-10-09)

This extension is diagnostic-only. **No controlled local defect RED was found;
M2 was not attempted.** Other reported cases and the cause of the natural CI
failure remain unproven. Earlier diagnostic history above remains intact.
At this M1 checkpoint, no behavioral fix, worker-image validation, CI retry, or
M3 completion was claimed. The approved diagnostics-only delivery's M3 validation
and reporting are recorded below; privileged image acceptance remains
pending post-finalize ordinary PR CI, observed by the maintainer.

The affected path was traced before editing: the real
`makeProductionLaunchAdviceRoot` closure awaits the launcher's `dispose` outcome;
`withOwnedTreeCleanup` removes its data tree only on clean disposal, and the
executor's fallback has the same condition. Public advice disposal returns
`Promise<void>`. The existing retained warning now carries only fixed
classification/reason/state/authority categories and `cleanup_attempted=false`.
No production interfaces, decisions, transport reads, timers, awaits, or
filesystem operations were added.

The controlled probe uses the existing fake supervisor and spawn/provisioning
mocks, through the real executor closure and real `launchCodexRoot` lifecycle.
Portable parent metadata and privileged provisioning are scripted; these tests
do not replace the real security inventory. All six cases passed:

- Exit before final dispose evidence and pipe completion: the actual single-UID
  data assertion passed; clean owner-refusal disposal emitted the expected
  not-owned cleanup warning. Observed exit ordinal 3, dispose evidence 4,
  pipe-end ordinals 5/6/7.
- Evidence before exit: the same two assertions passed, with evidence ordinal 3
  and exit 4.
- Genuinely unclean control (drained evidence followed by nonzero exit): both
  fixtures retained data. The not-owned warning was absent because cleanup was
  skipped; the retained warning reported not-clean/drained/ECHILD+__WALL and
  cleanup not attempted. This is legitimate retention, **not defect RED**.
  Its selected fields were also verified in captured diagnostic failure output.

Persistent diagnostics are test-side, enabled for the existing advice teardown
leaves. The original failure is printed before fixture removal and rethrown
without replacement by a later cleanup failure. Snapshots include Node version, root
existence/uid/gid/mode/type/device/inode, bounded event ordinals, and selected
warning categories, with cleanup inference distinguished from observation.
Bounds: four supervisors, four root records, 64 event records, 64 evidence
frames per supervisor, 8 KiB partial/frame limit, 64 KiB total evidence per
supervisor, and eight selected warnings from the last 64 in-memory logger
records. Overflow stops that observer's parsing; it does not claim clean
disposal. Only evidence fd4 is parsed; transport contents are never consumed
by diagnostics. No raw evidence, paths, error text, supervisor reasons,
model/config/private content, or environment dumps enter snapshots.
Existing ENOENT, refusal, outside-content, UID/cap/entrypoint assertions,
inventory names and timeouts remain intact.

Local commands ran serially from `agent/`. Test prefix:
`node --import tsx --import ./test/setup/hermetic-proc.ts --test --test-timeout=120000`.
Every command wrote a distinct worktree scratch log with recorded exit status.

| Command after test prefix | Result | Log under .uzi/scratch/ |
| --- | --- | --- |
| --test-name-pattern='M1 controlled advice disposal ordering probe' test/codex-launcher.test.ts (first probe) | exit 0; six passed | gate-log.nVJMyI |
| test/advice-teardown-diagnostic.test.ts (final) | exit 0; two passed | gate-log.qX9XD4 |
| --test-name-pattern='M1 controlled advice disposal ordering probe' test/codex-launcher.test.ts (final) | exit 0; six passed | gate-log.R9r2dx |
| test/codex-launcher.test.ts | exit 0; 107 passed, one privileged skip | gate-log.4KKqYM |
| --test-name-pattern='production advice data teardown' test/codex-executor.test.ts | exit 0; no selected teardown leaves exercised under this host UID | gate-log.SwGbqJ |

Focused lint used
`npx oxlint --config .oxlintrc.json -D correctness --report-unused-disable-directives-severity=error`
with only the six changed TypeScript paths: exit 0, zero warnings/errors,
`.uzi/scratch/gate-log.Wr3egL`.
The full launcher file check preceded the final diagnostic error-preservation
adjustment; the final focused probe and diagnostic tests ran after it.
Repo-wide gates/builds/tests, project typecheck/deadcode, dependency installation,
and workflows inspection were excluded by that M1 dispatch. Subsequent lead
verification is recorded below.
The executor selection is not worker-UID validation: this host is UID 10003,
while the real suite requires WORKER_UID and the required group memberships.
The next natural worker-UID CI failure remains the evidence source.
After the delegated child was cancelled before returning its final report, the
lead inspected the remaining edits and recorded scratch logs. The lead refined
the failure-only reason categories to distinguish drain/exit/channel/evidence
failures without exposing reason text, and added safe Node-version provenance.
The focused probe plus diagnostic tests then passed (eight executed tests,
zero skips/cancellations, exit 0; `.uzi/scratch/gate-log.YoPgen`). No controlled
defect RED was produced. An additional inherited-key guard on the reason lookup
is included in the integrated revision reviewed by the lead's gate.

The source-derived inventory from the immutable base and edited tree was
byte-identical (`node e2e/worker-uid/inventory.mjs` and `cmp`, exit 0).
All existing required leaf identities remain selected; the six portable probe
cases and two diagnostic tests are ordinary host tests, not privileged proofs.

Owner-refusal, symlink-refusal and initialization/session-cleanup incidents are
each **unproven** as sharing the natural failure's cause. The legitimate unclean
control demonstrates a possible symptom path, not attribution of those incidents.
Image identity and CI head/run/attempt correlation must come from the future
natural failure's ordinary job/build record; none is claimed from this host.

## M3 verification and disposition (2026-10-09)

**Approved diagnostics-only exit; no cause or fix claimed.** No controlled defect
RED was found; M2 was not attempted. Owner refusal, symlink refusal and
initialization/session cleanup are each unproven as a shared cause. The PR uses
`Refs #2397`; the issue remains open. Persistent diagnostics remain for the
**next natural failure**.

The lead supplied verification at code commit
`bf53e0aafc0326b18678c41d581ab86e2250163c`:

| Verification | Result | Log under .uzi/scratch/ |
| --- | --- | --- |
| `task gate:agent` | EXIT=0; full unsharded unit suite + M4, both with zero failures/cancellations; M4 run-completeness OK. Dependency check, lint, deadcode and typecheck prerequisites succeeded. | gate-log.fZgqfo |
| Isolated final probe + diagnostic tests | EXIT=0; eight executed, zero skipped/cancelled. Exit-first and evidence-first controls were clean; nonzero-exit controls showed legitimate retention, not defect RED. | gate-log.YoPgen |
| `python3 e2e/worker-uid/check_test.py` | EXIT=0; ran locally under M3. | gate-log.iKbFmv |
| `python3 e2e/worker-uid/run_test.py` | EXIT=0; ran locally under M3. | gate-log.5erwZm |

Immutable M1 review of `c9107fc79d201f57b1510514a2878ef5c5bc20bf..bf53e0aa`
reported no blocking, correctness, acceptance-criteria or privacy findings from
reviewer or tester. Their focused tests passed: eight executed, zero
skipped/cancelled (review logs `MUCt5w` and `dnZKGs`). Tester focused lint
(`HmNPGI`), syntax (`cNetGU`) and inventory (`NgAt8a`) checks each recorded
EXIT=0. The lead inspected the report logs and diff.

One nonblocking coverage note is deferred: the existing bounds test reaches the
64-frame limit before oversized input, so the 8 KiB and 64 KiB byte limits are
not independently exercised. The implemented caps were inspected; no runtime
defect was demonstrated. The tester's every-bound/mutation residual duplicates
this limitation and adds no independent optional note.

The source-derived immutable base/current inventory was byte-identical. The
existing parser derives leaves from the current recognized suites, including new
leaves added there; acceptance does not hardcode 29.
The agent `npm test` glob `test/*.test.ts` still includes the changed/new tests,
and the Task shard target delegates to unit; selection and scripts are unchanged.
The unsharded gate covers ordinary tests, but did not run the two CI shards or
their union check; post-finalize ordinary CI owns that proof. Host skips of
privileged tests do not count as validation.

The privileged image lane was **not run by this worker**. Acceptance remains
pending post-finalize ordinary PR CI, which the maintainer observes; there is no
CI waiting or rerun in this delivery. A first green lane does not establish
cause. The composite `task test:worker-uid-harness` and `gate_test.py` were
excluded because a component reads the workflow file; they are deferred to
normal CI, and the existing target is untouched. No CI/image identity was
observed here, and no timing savings or runtime speedup is claimed.

ENOENT, refusal and outside-content assertions, inventory, UID/cap/entrypoint
posture and diagnostic caps remain unchanged. The code gate ran once at the
commit above; M3 changes documentation only, so the unchanged code gate needs
no repeat. Scratch logs are ephemeral; the commands and results recorded here
are the persistent verification record.

## Explicit conditional-milestone disposition

The human explicitly exempted M2 from the frozen completion contract after the
structural interlock rejected the diagnostics-only finalization:

> Exempt M2. This matches the approved plan v2 exit: no controlled local RED was found, so finalize the reviewed M1/M3 diagnostics with M2 not attempted, no cause or fix claimed, other #2397 cases unproven, privileged CI acceptance pending post-finalize, and the PR body using Refs #2397 (not a closing keyword).

Decision: M2 is **exempted and not attempted**, not completed. Its controlled-RED
entry condition was not met. M1 and M3 deliver the approved diagnostics-only
scope; this exemption does not assert a verified fix or issue acceptance.
The completion interlock repeated after this human decision, so publication
remains blocked by the unresolved contract mismatch. No further completion
signal is made while that mismatch persists.
