# Worker UID calibration: preload regression and open validation blockers

**Status: partial repair only. The canonical task still fails; M3 is incomplete.**
The landing owner's later retrieval of the hosted log established an unexpected
primary exit with Node worker-heap errors in the first calibration leg. The
preload explanation is supported by the bounded local reproduction below; it
was not separately instrumented in that hosted leg. This note does not establish
repaired CI or merge readiness; the canonical lane remains an unresolved blocker.

## Preload regression and bounded repair

[calibration-producer.mts](calibration-producer.mts) creates a CommonJS load worker
with 16/4/1 MiB old-generation/young-generation/stack limits. The shared
[worker-memory-load.cjs](../../agent/test/fixtures/worker-memory-load.cjs) creates
up to four sequential nested CommonJS churn workers with 8/2/1 MiB limits.
Under Node 24.20.0 and locked tsx 4.23.15, inherited TypeScript preload exhausts
the nested worker's old heap.

The existing repair sets `execArgv: []` on the nested worker, clearing its
unnecessary TypeScript preload. The outer worker retains its inherited loader.
Heap limits, load budgets, iteration bounds, lifecycle handling and offline
proof requirements remain unchanged; neither PSI requirements nor deadlines
were relaxed.

The existing [calibration-producer.test.mts](calibration-producer.test.mts)
regression, “CJS load worker ACKs four churn steps under the locked tsx preload”,
launches the actual fixture under the locked loader and original outer limits.
It requires four real ACKs and checks that three files each reach 4 MiB.
The original fixture fails at step 1 with `ERR_WORKER_OUT_OF_MEMORY`; the fixed
fixture passes. One 7-second ACK deadline bounds the sequence, and a 10-second
hard child deadline bounds the subprocess. The worker is terminated and the child
reaped before owned directory cleanup. These are ordinary directories: this
regression provides neither tmpfs nor PSI proof.

Reproduce from the repository root with the existing agent dependencies:

```sh
(cd agent && node --import tsx --test --test-timeout=120000 --test-name-pattern="CJS load worker ACKs four churn steps" ../e2e/worker-uid/calibration-producer.test.mts)
```

Recorded focused test durations were 565.326 ms failing before the repair and
794.299 ms passing afterward. This is a correctness result, not a performance
improvement. Hosted critical-path, queue, rerun and cache costs are unmeasured;
local validation reused the verified image.

## Recorded local validation — 2026-10-16

These are historical results, bound to the stated revisions, not a green CI claim.
Runtime validation used `faedbe089b4e550df1c3b21ba95c0910b4db3da4`:

| Check | Recorded result |
| --- | --- |
| `task gate:agent` | Exit 0 |
| Canonical `task test:agent:worker-uid` | Exit 201; calibration command exit 1 |
| UID leaves | 40 passed: 38 worker and 2 command leaves |
| Producer / inventory / scoped TypeScript checks | 17 producer tests passed; 1 inventory test passed; tsc passed |
| Real 1 GiB and 2 GiB calibration | PSI absent before enrollment; both failed |

Both calibration reports recorded null `/sys/fs/cgroup/memory.pressure` and zero
enrolled handles. Host checks confirmed removal of each exact owned container.
No real calibration acceptance or response measurement was obtained.
Separately, `task gate:repo` exited 0 at
`6626ae2ea599d3c4b14c2a6b0d9b00f71f48fb2c`; actionlint was unavailable and skipped.

### Image identity and mounted source

In that canonical run, `wuid-ci-repair-38903954187:proof` **resolved to**
`sha256:6c33153981f5ae2d7347c1bc6106aa1ef39236b3441cd237f15a550320261f47`.
This was the same M1 proof image: the branch's three helpers rebuilt offline from
vendored source on the public rc.18 runtime. It is not asserted to be the exact
hosted image.

The repaired fixture was mounted read-only from the checkout: see the source
mounts in [calibration.py](calibration.py)'s `Host.create`. An image identity
predating the fix is correct here because the runtime mount supplies the changed
fixture; the preload repair did not change the helpers.
The invocation set `WORKER_UID_SKIP_BUILD=1` and `GITHUB_ACTIONS=false`, with
explicit local run identity. Runtime metadata was Node 24.20.0, Python 3.14.7,
Docker client 29.5.3/server 29.8.0, cgroup v2 with cgroupfs, and Task 3.54.0.

## Open validation and settlement blockers

Published pipeline 38903954187 tested
`104faf8060190ba19e5a74ee83b5753b703cba9b`: its image build passed, then
“Require worker UID leaves” failed with underlying exit 1 / task exit 201.
Full hosted logs returned 403 during the worker investigation. The landing owner
subsequently retrieved [the failed job log](https://github.com/vtmocanu/uzi/actions/runs/38903954187/job/116983050340):
the 38 worker and two command leaves passed, then the 1 GiB report failed with an
unexpected primary exit. Both broker outputs contained `ERR_WORKER_OUT_OF_MEMORY`;
the cgroup `oom`, `oom_kill` and `oom_group_kill` counter deltas were zero. PSI was
available in this hosted leg. The 2 GiB leg was aborted after the first failure;
both host cleanups were confirmed. Neither report is accepted calibration proof.
The loader inheritance mechanism is independently reproduced locally, while the
hosted report itself establishes the Node worker-heap failure, not its exact heap
or preload internals.
The published revision's local canonical baseline timed out during its cold
image build; a full-target before/after comparison is therefore inconclusive.
Real PSI-capable validation and hosted failure evidence or a subsequent hosted
run remain necessary. Missing PSI is not grounds to weaken proof or raise deadlines.

A scratch-only diagnostic substituted synthetic zero PSI after the preload fix
and reached `calibration cleanup failed; no next leg`. Both owned trees had clean
disposal evidence, but broker replies remained pending and primary exit records
were empty; final reply joins failed. The second limit was not run. Reports were
synthetic and unaccepted, not calibration acceptance; owned container removal
was subsequently confirmed.

Source tracing identifies the settlement boundary:
[supervisor.go](../../agent/codex/supervisor/supervisor.go)'s normal child-readiness
path emits `child_exit`, while `opDispose` drains and returns without that event.
[main.go](../../agent/codex/supervisor/main.go)'s `realReap` discards wait status.
The [launcher](../../agent/src/codex/launcher.ts)'s `waitChild` can consequently
remain pending after disposal; [makeDefaultSpawnCommand](../../agent/src/codex/codex-executor.ts)
awaits ordinary termination after memory feedback (the `cleanupReason` branch).
[calibration-fixture.mts](calibration-fixture.mts) also creates abort controllers
without passing their signals to the broker, disconnecting cleanup aborts.

The worker investigation did not provide a real-helper regression for this seam.
On 2026-10-17, the landing owner reproduced it with the actual supervisor under
the cap-less command-UID profile: the primary's input remained open, disposal
reported drained with ECHILD authority and the helper exited successfully, but
no `child_exit` event was emitted. The new control-first regression failed on
the published source. The user-approved scoped repair preserves the observed
terminal wait status and reports the primary exactly once through either
readiness or disposal, independently of drainage authority. The control-first
real-helper regression then passed. Fixture cleanup also binds separate owned
broker abort signals; its behavioral isolation regression fails without that
wiring and passes after restoration. These targeted proofs are not PSI
calibration or a green canonical lane; M3 and the canonical lane remain incomplete
until all original composed CI and calibration gates pass.
