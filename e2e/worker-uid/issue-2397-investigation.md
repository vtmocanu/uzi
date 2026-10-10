# Issue #2397: partial worker-UID investigation

**Current status (2026-10-10).** See the three dated sections at the end of this file:
- A controlled worker-UID RED was obtained on a worker's real lane for the owner-refusal,
  symlink-refusal and single-uid advice teardown leaves.
- A test-side readiness barrier made those leaves 40/40 GREEN on the same lane.
- Production code is unchanged.
- The other historical #2397 cases remain unproven, and the issue stays open (`Refs #2397`).

Everything between this paragraph and those sections is earlier history, kept verbatim. Its
statements were true when written. That includes the next paragraph, which records the earlier
diagnostics-only disposition, superseded by those sections.

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
was blocked by the unresolved contract mismatch at that point. Publication
subsequently completed in PR #2583, which merged the diagnostics-only delivery.

The next main run, [37943529532](https://github.com/vtmocanu/uzi/actions/runs/37943529532),
failed the owner-refusal leaf again. Node 24.21.0's JUnit reporter omitted the
helper's console error output, so the snapshot did not reach the job log.
Diagnosed leaves now emit through their active test context; Node stores that
diagnostic as an XML comment immediately after its test case. The lane checker
preserves those comments and prints exact-prefix diagnostics only for failing
required leaves, with control characters removed and a total limit of eight
lines and 16 KiB. The producer also limits each emitted snapshot to 16 KiB.
A controlled failing helper on the real Node 24 reporter proves transport;
passing leaves remain silent. This repairs diagnostic visibility, not the
unproven disposal cause, and does not replace the privileged lane's evidence.

## Controlled real-lane reproduction (2026-10-10, plan v3 M1)

This run obtained a controlled worker-UID RED with no forcing. It was recorded on this worker's
real lane, not in CI.

**Posture.**
- Image: `wuid-2134-base:local`, `sha256:300f2aa521c51894ee2cfc45ad8805c598af57d2a3922893d5c780a6b6a46057`, built from
  `agent/templates/base/Dockerfile` at `0e356b9f`. The build used `docker build --network host`: this
  worker's bridge network has no egress, and the default build hung at `apk add`.
- Runtime: Node v24.20.0 and Codex 0.159.3 (`/opt/uzi-codex/0.159.3`).
- Container: the `docker run` from `e2e/worker-uid/run.sh`, with the same `--network none`,
  `--cap-drop ALL` plus its five cap-adds, `no-new-privileges`, tmpfs mounts and
  `uzi-entrypoint` drop to the worker UID. It differed in these ways:
  - It mounted a scratch export of `0e356b9f` with scratch-only patches.
  - It passed only `/app/test/codex-executor.test.ts`, not `run.sh`'s four test files.
  - It used the projection's `--test-name-pattern`.
  - Some batches added `--cpus 0.25`.
  - It had its own container name, with a fixed 600s outer timeout.
- Each run selected four leaves through a labelled scratch projection of the unmodified
  `inventory.mjs` output: owner refusal, symlink refusal, single-uid, and the genuine-unclean
  control. The unmodified `pattern.mjs` and `check.py` ran over that projection. The tracked
  inventory, checker and `run.sh` were unchanged.

**Scratch patches** (never committed):
- On a passing leaf, the diagnostic snapshot was also emitted under a different prefix.
- A `probe-safety` line listed directory entries (names only): the victim directory for the
  symlink leaf, and the planted directory for the owner leaf.
- (a) Forced: the leaf's own mutation was moved inside a nested `child_process.spawn`
  intercept, physically before the supervisor spawn.
- (c) Barrier: each leaf awaited one `initialize` request on the advice transport before
  `mutation_start`, using the params shape from `agent/src/codex/appserver-auth.ts`.

| Batch | Iterations with any failing leaf | Wall total / mean |
| --- | --- | --- |
| (a) forced pre-spawn mutation, default CPU | 0 / 3 | 22.5s / 7.5s |
| (b) unprobed, default CPU | 4 / 10 (owner 3, symlink 2, single-uid 2) | 71.7s / 7.2s |
| (b) unprobed, `--cpus 0.25` | 6 / 20 (owner 2, symlink 2, single-uid 2) | 664.1s / 33.2s |
| (c) awaited readiness barrier, default CPU | 0 / 20 | 172.3s / 8.6s |
| (c) awaited readiness barrier, `--cpus 0.25` | 0 / 20 | 699.9s / 35.0s |

In every batch the genuine-unclean control passed, retaining data with the retained warning.

**Observations (b).**
- Every failing leaf failed with its original named assertion. Owner and symlink lacked the
  `Codex advice data cleanup failed` line. Single-uid failed `Missing expected rejection` from
  `assertGone(owned)`.
- Every failing record shows the supervisor's `started` observed before `mutation_start`, and a
  supervisor `child_exit` evidence event observed after `mutation_end`. That `child_exit` was
  followed by `abnormal` or a dispose report, and the warning was
  `retained / not_clean / other_unconfirmed`, `cleanup_attempted:false`. This matches the
  signature of the natural CI record from run 38001555502 (job 114063389279, quoted in full
  in the #2397 issue thread). That record shows the evidence order `started`, `child_exit`,
  `abnormal`, exit `nonzero`, with no `dispose` evidence, and the warning
  `retained / not_clean / other_unconfirmed` with `cleanup_attempted: false`. The CI record
  carries no mutation marks, so its mutation timing is not known.
- No passing leaf in any batch recorded a `child_exit`.
- In three failing records the `child_exit` was observed after `dispose_start` but before the
  dispose evidence. The derived `failure_signature.child_exit_before_dispose` reads `false` in
  those records, so that field alone does not discriminate the defect. The presence of any
  `child_exit` event does.

**Observations (a) and (c).**
- (a): mutating the root before the supervisor spawns did not disrupt the provider in these three iterations. All leaves
  were clean, and the victim directory held only `keep`.
- (c): with readiness confirmed before the mutation, all 40 iterations were clean. Every record
  for the three test leaves read `started > provider_ready > mutation_start > mutation_end > dispose_start >
  dispose > exit_zero > dispose_end`. Owner and symlink logged the cleanup-failure warning,
  and single-uid removed the root without it.
- Probe safety: the owner planted directory held only `keep` (`"keep"`), and the symlink
  victim held only `keep` (`"outside"`), identical to the clean unprobed runs.

**Physical order vs observed order.**
- `uidScript` is synchronous, so any evidence arriving during the mutation is only observed
  after `mutation_end`.
- Barrier-established: (c), where readiness physically preceded the mutation, and (a), where
  the mutation physically preceded the spawn.
- In (b), the provider's exit was observed during or after the mutation window; the exact
  physical order is ambiguous.
- **Inference, not proven:** the provider exits when its owned root is renamed after spawn but
  before it is ready. Renaming before spawn (batch (a), only n=3 at default CPU) or after
  readiness does not disrupt it. Which path the app-server touches at startup was not observed.
- **Confound:** the barrier changes two things at once. It adds an active `initialize`
  handshake, and it delays the mutation by about 300ms (median leaf time). The evidence cannot
  separate "after readiness is safe" from "later is safe", or from "the handshake itself moves
  startup work earlier". The fix relies only on the observed result: with the awaited answer
  before the mutation, no disruption occurred in 80 iterations (40 in (c) and 40 for the fixed
  leaves).
- **Medians:** the (b) medians include failing runs; the (c) and fixed medians are passing runs
  only. "Unprobed" in (b) means no readiness barrier; the `probe-safety` listing ran in every
  batch.

**Per-leaf gate (plan v3 M1 step 5).** Owner refusal, symlink refusal and single-uid each have
an unforced named RED carrying the natural signature, a 20/20 barrier GREEN under throttle,
and no probe-safety change. All three qualify for M2.

Focused median leaf time at default CPU, unprobed (b) to barrier (c):
- owner: 667ms to 963ms
- symlink: 619ms to 987ms
- single-uid: 670ms to 1029ms

## Readiness barrier fix: real-lane GREEN (2026-10-10, plan v3 M2)

Fixed commit: `1f64ebe3`. Base leaves for the RED: `0e356b9f` (batch (b) above). The fixed
leaves ran on the same image and focused projection as the RED batches, with the same scratch
diagnostic emission patch and no barrier patch: the barrier is now in the tracked leaves.

| Batch | Iterations with any failing leaf | Wall total / mean |
| --- | --- | --- |
| fixed leaves, default CPU | 0 / 20 | 167.5s / 8.4s |
| fixed leaves, `--cpus 0.25` | 0 / 20 | 675.7s / 33.8s |

- All 160 leaf records passed, including the genuine-unclean control.
- Every record for the three fixed leaves read `started > provider_ready > mutation_start >
  mutation_end > dispose_start > dispose > exit_zero > dispose_end`.
- The probe-safety observations were unchanged: owner `["keep"]` = `"keep"`, symlink victim
  `["keep"]` = `"outside"`.

Median leaf time at default CPU, base (b) to fixed:
- owner: 667ms to 929ms
- symlink: 619ms to 924ms
- single-uid: 670ms to 1000ms

Median leaf time at `--cpus 0.25`, base (b) to fixed:
- owner: 2660ms to 3049ms
- symlink: 2485ms to 3006ms
- single-uid: 2815ms to 3302ms

## Final verification and disposition (2026-10-10, plan v3 M3)

Code under test: `1f64ebe3`. The later commits change only this file.

Ran on this worker, each to a scratch log with its exit status recorded:

| Command | Result |
| --- | --- |
| `task test:worker-uid-harness` (`check_test.py`, `run_test.py`, `gate_test.py`) | EXIT=0 |
| `WORKER_UID_SKIP_BUILD=1 task test:agent:worker-uid` | EXIT=0, 34s wall; the unmodified wrapper, full inventory and checker report "29 required leaves passed" |
| `task gate:agent` (umask 027) | EXIT=201 after 1917s. Passed: deps-check, lint, typecheck, deadcode. Unit stage: 11430 pass, 2 fail, 0 cancelled, 3 skipped. M4 was not reached |
| `task test:codex-m4`, run separately because of that stop | EXIT=0, run-completeness OK |

- **Prebuilt image:** `WORKER_UID_SKIP_BUILD` is `run.sh`'s existing knob. It reused the
  `--network host` build described above, because a default build cannot reach the network on
  this worker. `run.sh` mounts the commit's `agent/src`, `agent/test` and `agent/templates`
  over the image, and no non-test file changed after the image's source commit.
- **The two unit failures** are `codex-launcher.test.ts` "M1 controlled advice disposal ordering
  probe": `exit_first` and `evidence_first through actual executor owner_refusal`, each failing
  with `ENOENT` reading `.../codex-advice-data/<uuid>/keep`. This branch does not change that
  file. The same focused test run on a `git archive` export of base `40caea1d` failed the same
  two leaves (4 pass, 2 fail), so the failures pre-exist on this host. They match the hosted-worker
  case recorded in the #2397 thread and are not addressed here.
- **CI:** the PR's ordinary CI (`test-agent-worker-uid` and the agent shards) is observed by the
  maintainer. This worker did not run or edit any workflow.

**Status of each #2397 case:**
- **Symlink refusal (the run 38001555502 leaf), owner refusal and single-uid
  `assertGone`/`Missing expected rejection` in these advice leaves:** controlled RED and GREEN on
  this worker's real lane, as above.
  - **Caveat:** older single-uid and owner-refusal CI failures carried no ordering record. That
    they share this cause is likely but unproven.
- **"real worker-UID initialization" `assertGone` failures** (a different test): unproven, and
  not addressed.
- **Hosted-worker `codex-launcher.test.ts` probe `ENOENT`:** reproduced on this host at base and
  head. Separate from this fix, and unresolved.
- **#2532 runtime-upgrade `EACCES` / `child launch failed`:** out of scope.

**Known limits:**
- A provider that stays alive but never answers `initialize` would hang until the 120s test
  timeout without a named message. No new timer was added.
- `providerChildExitObserved()` reports what the bounded diagnostic observed. If that observer
  were blind (truncated or no supervisor observed), the original assertions still catch an
  unclean disposal.

Because the cases above remain open, #2397 stays open and the PR uses `Refs #2397`.
