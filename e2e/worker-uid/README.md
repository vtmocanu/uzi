# Worker UID test lane

`task test:agent:worker-uid` builds the base worker image and runs the ownership and cleanup
leaves from nine test files through its root entrypoint, which drops to the worker UID with runner
group membership and the production ambient capabilities. It runs offline, with
disposable data and Nix mounts, and removes only its own containers and their
anonymous volumes and its two named tool volumes after checking exact resource
identity and invocation labels.

To reuse an image, set `WORKER_UID_SKIP_BUILD=1` and `WORKER_UID_IMAGE=<image>`.
The image must have an existing local tag; the harness never changes its tags.
`WORKER_UID_TIMEOUT` controls the outer watchdog (default 600 seconds).
The separate command proof has a 115-second watchdog plus five-second kill grace
(120 seconds total). All Docker operations use GNU timeout. Image metadata gets
five seconds plus five-second kill grace.
The optional local build gets 60 seconds plus five-second grace; the image
layout/dependency preflight gets 20 seconds plus five-second grace
(100 seconds total including metadata and the optional build). Phase diagnostics
report elapsed time and identify the command when a deadline fires. Cleanup
has a shared 30-second budget including
kill grace and reaping; each metadata/removal request gets at most 10 seconds,
including at most five-second grace. It tries each recorded resource once, continues
after failures while budget remains, and propagates failure or exhausted budget.

Every invocation uses an unpredictable 128-bit token in its names and ownership
labels. Creation requests are journaled before sending, so an ambiguous Docker
failure or lost acknowledgement still triggers ownership inspection. Cleanup
inspects
only attempted resources, verifies the invocation label, and removes containers
by their returned immutable IDs. Missing,
malformed or mismatching ownership metadata fails closed: planned names alone
never authorize removal. Failure before creation performs no resource cleanup.
The tool-image build and probe are journaled before creation; their failures still
require ownership verification and cleanup. Containers remain until the trap.
`WORKER_UID_SKIP_BUILD=1` requires the hosted image to exist locally; the harness
never pulls an image or installs runtime dependencies.

The host needs Docker, Node, Python, GNU timeout and the locked agent development
dependencies. Test inputs are mounted into `/app`, preserving their relative paths
and resolving runtime packages from the image.

A second, serial container starts directly as command UID/GID 10003, with all
capabilities dropped and no-new-privileges. It mounts this harness, agent source,
agent tsconfig/package metadata and vendored supervisor source read-only under
`/app/uzi-worker-uid-proof`. A preflight refuses an image where that directory
already exists, before any source is mounted there.
The per-container source parent is an owned tmpfs; a scratch symlink keeps
repository-relative test writes under `/proof/.uzi/scratch`. Read-only source
mounts remain separate. The package metadata preserves the source’s ESM boundary.
Runtime packages resolve directly from the image’s locked `/app/node_modules`,
without copying dependencies into a volume. All proofs and calibration mask the
image-declared `/data` and `/nix` volumes with tmpfs so Docker does not initialize
the full Nix store. The preflight builds an invocation-owned image offline to
record the supplied image's canonical Go and Task paths, verifies its parent
layers and platform, and validates those paths in a capless probe. Only the
static Go tree and Task directory are initialized into invocation-owned named
volumes and mounted read-only for the command proof. The preflight journals each
name before creation, rejects collisions, and verifies the exact create receipt
and invocation label. Named sources also support daemons that reject read-only
anonymous mounts. Its remaining dependencies stay in the image.
Cleanup removes the derived image without pruning its shared parent.
Scratch, Go cache and Go temporary files
stay in a command-owned executable tmpfs for the freshly built helpers. The fixture checks Linux, UID 10003, all five empty
capability sets and no-new-privileges before building only the two Go helpers with
`-mod=vendor`, `GOTOOLCHAIN=local`, `GOPROXY=off` and `GOSUMDB=off`.

The real supervisor and command sandbox still execute. The fixture asserts real
adopted detached descendants, protected and unattributed exclusions, injected
supervisor/root start-tick mismatch refusal before signaling, concurrent cleanup
joining one disposal, sibling and unattributed-tree survival, and the exact
structured pressure failure at the broker and output projection. Start-tick fault
injection does not claim actual kernel PID reuse. Starting at command UID does not
prove the production worker-to-command UID transition, M3 runtime wiring, or M5b
cgroup calibration. The [M5a validation record](../../agent/test/fixtures/worker-memory-m5a-validation.md)
records successful offline real-process execution, review, local timing and evidence
limits. Final composed-lane timing and real-cgroup calibration remain M5b work.

Before execution, the locked TypeScript parser derives the leaf inventory from
the current source. The four ownership/cleanup files must each contain their
recognized UID gate and at least one leaf. The fifth file,
`agent/test/git-planning-diff.test.ts`, contributes exactly nine literal leaves
inside the exact `Unit2 runner source capture` suite: two snapshot-copy
cancellation leaves and seven symlink capture/refusal leaves. The suite itself
is not targeted: generated titles, the command sandbox leaf, other capture
leaves and the bounded stdout suite stay excluded. Missing or changed required
titles, nonliteral replacements and duplicate identities fail closed. Targeted
tests must use literal titles and static `it` / `describe` declarations. The command
inventory also recognizes explicit `test(...)` in the `.mts` fixture and requires
its exact top-level proof title. Both inventories use this harness’s scoped
`tsconfig.json`, which includes `.mts` files and future calibration fixtures
without changing the agent project. A sixth file, `agent/test/git-retained-permissions.test.ts`, adds nine required literal leaves for real-UID retained recovery: loose and packed objects, captured-tip ordering, the four mixed worker/runner hardlink variants, command-owned hardlink mutation refusal, and unsuccessful recovery retention. Its required titles cannot disappear, become nonliteral, move outside the UID gate, or be duplicated without failing inventory validation. Owner-only command objects and both existing hardlink names are checked under the real distinct UIDs. Root, single-UID execution and skipped results do not prove these cases. The command-owned hardlink refusal must reach `hardlinked_mutation`; a generic capture failure does not satisfy it. Both negative cases compare every original private object and directory snapshot plus exact source refs, allowing only the specified partial permission additions; both hardlink names remain exactly unchanged. The corrupt-source case must reach a blob-specific real fetch-integrity refusal after real readiness and clean runner status, rather than merely stop without adoption.

A seventh file, `agent/test/claude-command-launcher.test.ts`, contributes its three literal leaves under the single `INIT_SKIP`-gated suite `real runner-uid command launcher` (issue #2471): the image supervisor launched at runner UID 10002 through the guarded-command wrapper, the memory-guard launcher preflight, and the image bash version. Its fake-supervisor and host-bash wrapper tests run in the normal host shards, not in this lane.

A ninth file, `agent/test/codex-capture-reaper-real.test.ts`, adds four required leaves for credential-free scoped reaping: real runner procfs and signal compatibility, descendant settlement before the verdict, own-mode residue cleanup, and report-only observation. The fixtures also check foreign-attempt and foreign-UID preservation. These run with the real worker/runner/command UID split; host mocks and skipped tests do not establish the proof.

An eighth file, `e2e/worker-uid/claude-command-real.test.mts`, contributes twelve required literal `it` leaves under the single `INIT_SKIP`-gated suite `real Claude guarded command lane` (issue #2471, M4). It runs in the same root-start worker-proof container (worker UID 10001, `UZI_UID_SPLIT=1`, runner UID 10002) and drives the real `ClaudeCommandLane` with the real launcher, image supervisor and `WorkerMemoryCommands`, entering each call through `lane.hook` then `lane.handle` as the SDK does. It proves: guard-style cancel and short-deadline expiry drain a detached command and its descendants; cancelling one run's command leaves a sibling run's command alive; another attempt's capture scan skips a live command's recorded root while the owner's reap still kills it; a candidate whose root start time differs from the enrolled one (simulating PID reuse by altering the observed start time, not by reusing a kernel PID) cancels nothing; a larger unattributed tree is never selected; the exact `worker_memory_pressure` result becomes the selected command's tool error; registrations from other sources, and unminted, cloned or foreign-context metadata, are never enrolled yet the command still runs; a command declaring its own worker marks gets no protection; a stopped supervisor, with a non-dumpable runner-UID descendant under it, cannot confirm disposal, so the lane is poisoned, exactly that supervisor is killed, and the owner's own reap reports `unverified` (never `quiescent`) while the descendant lives; and native Bash is kept when the guard is off. The non-dumpable descendant needs no prctl helper: the kernel marks a process non-dumpable when it execs a file it cannot read, so the case runs a worker-owned mode 0711 copy of the image's busybox (executable, not readable, by the runner UID) by its applet name. The inventory requires exactly one UID gate and all twelve literals under that suite; a missing, renamed, nonliteral, nested, duplicate or ungated leaf fails closed.

The default inventory retains the five old files and adds the retained recovery file, the claude-command-launcher file (its three literal leaves above) and the claude-command-real file (its twelve); `node e2e/worker-uid/inventory.mjs command` emits the separate command
inventory and `node e2e/worker-uid/inventory.mjs single` the single-uid inventory. `expected.json` / `results.xml`, `command-expected.json` /
`command-results.xml` and `single-expected.json` / `single-<n>-results.xml` are checked separately. The JUnit
checker requires each leaf exactly once and
rejects skip, failure, error and missing results. Node and container failures,
including exit 77 and watchdog timeouts, also fail the target even when the checker
fails too. Container stderr stays visible. Reports remain in a unique directory
under `.uzi/scratch/`, printed for diagnosis. Both JUnit stdout streams are captured
by the foreground bounded collector: each complete report may occupy at most
8 MiB (8,388,608 bytes), including diagnostics. The collector never writes past
that cap. Overflow terminates its own watchdog, retains the capped report and an
`EXIT=125` status sidecar, and fails the lane even if the retained XML parses.
TERM gets at most five seconds before the private producer group is killed;
inherited pipes cannot extend the capture deadline. The EXIT trap then performs
the bounded, ownership-verified container cleanup.

The standalone checker reads at most the same cap plus one sentinel byte before
parsing. XML is limited to 20,000 items (including comments), 4,096 testcases,
32 levels, 64 attributes per element and 256 KiB per element's text, comment or
attribute set; DTDs are rejected. These limits bound ingestion independently of
the printed diagnostic budget. Oversized or truncated reports fail closed.

Failures identify the leaf file and full suite/leaf names, and report each local
or inherited skip, failure or error with its originating suite or testcase.
JUnit messages, types, text and stacks are preserved; empty diagnostics say
`no reason supplied`. Node 24 JUnit includes assertion and cancellation reasons,
so no additional reporter is needed.

After the command proof, a single-uid phase runs `e2e/worker-uid/single-uid-preflight.test.mts` in three more
containers, each started directly as UID/GID 10001 with all capabilities dropped, no root entrypoint and
no UID split (the Kubernetes `runAsUser` posture), from the base image so its supervisor is used. They
differ only in the posture under test, named by `UZI_SINGLE_UID_EXPECT`, and each asserts from the process
status file that it really has that posture before calling the real `memoryLauncherPreflight()`:

- `ok`: `no-new-privileges`, empty bounding set; the preflight must return `{ ok: true }`.
- `profile:noNewPrivs`: started without `no-new-privileges`; the preflight must refuse with that reason.
- `profile:capBnd`: `no-new-privileges` plus one retained capability (`CHOWN`), so the bounding set is
  nonzero while the effective set stays empty; the preflight must refuse with that reason.

Each container gets a 65-second watchdog plus five-second kill grace (70 seconds), 60 seconds per leaf, and
its own `single-<n>-results.xml` checked against `single-expected.json`; any container or checker failure
fails the run. Worst case is 210 seconds on top of the other phases.

After the single-uid phase, a runner-shared phase (issue #3153) starts one more container, as root with
exactly the entrypoint's startup capability set (`CHOWN`, `DAC_OVERRIDE`, `SETPCAP`, `SETUID`, `SETGID`; no
`FOWNER`, no `FSETID`), no network and `no-new-privileges`, on tmpfs `/data` and `/nix`, from the
base image. It runs `e2e/worker-uid/runner-shared-real.test.mts`: nine required literal top-level
leaves (inventory lane `node e2e/worker-uid/inventory.mjs shared`) that exercise the runner-shared
subPath layout under real uids. Nothing is stubbed: the controller's golden runner-init script
(`controller/internal/kube/testdata/runner-shared-init.sh`, mounted read-only; only its
`/data-seed` and marker paths are substituted) and the working-tree
`agent/templates/entrypoint.sh` (its real root window, real `setpriv` drop and `tini`) run unmodified,
and every other identity is made with `setpriv` using the argv `agent/src/runner-uid.ts` builds (`setprivArgsForUid`
for runner 10002 and runner-cmd 10003 with `--init-groups`, the entrypoint's own final drop for the
split worker, a capless 10001 for the single-uid worker, uid 1000 with supplementary gid 10001 for the
dind uid, uids 0 and 101000 for daemon-written content). The argv is production's, but it is spawned from root (which holds SETPCAP), so the capability bounding set differs from production; no DAC outcome depends on it. The leaves are:

- the image layout marker gate: a missing, empty, wrong or symlinked marker, or a directory in its
  place, makes runner-init refuse before any write (the seed volume is byte-for-byte unchanged);
- runner-init as root on a fresh volume: `runner-shared` is worker:runner `3775`, `.tmp` is
  worker:worker `0755`, an old root-owned `0755` `runner` directory and sibling data are untouched, and a
  second run changes nothing;
- hostile `runner-shared` or `.tmp` symlinks (to `.`, to a sibling, to the parent) and files are refused
  without following or re-owning anything;
- runner-init as the capless worker uid: success only on a volume that uid owns, fail closed on a
  root-owned one, refusal of a symlink;
- the runner, runner-cmd and dind uids cannot rename, remove, symlink over or write inside `.tmp`, cannot
  move a worker-owned run directory, and (runner, runner-cmd) cannot replace `runner-shared` in its
  `worker:10001 2775` parent; their effective groups exclude 10001 when spawned the production way. The
  same leaf pins that a process holding only the fsGroup (the dind uid) cannot create entries at the top of
  `runner-shared` or inside a worker-made run directory (both group `runner`, other `r-x`), while the runner
  and runner-cmd uids can;
- a fresh `.tmp`: created by the real entrypoint (split) and by `ensureRunnerTmpdir` as the capless and the
  split worker on an empty `runner-shared`; a symlink or file planted at `.tmp` is refused each time;
- per-uid scratch under a live writer: a leaf owned by a foreign uid (dind, a subuid) with a live writer
  of that uid is refused with contents, owner and mode untouched; expected-owner `0700` leaves with live
  writers are adopted, preserved and announced with "writer exclusion not established";
- old-layout (`TMPDIR=/data/runner`) and new-layout (`TMPDIR=/data/runner/.tmp`) entrypoint boots in
  sequence and overlapping, one workload live while the other layout boots: neither re-owns, wipes or
  follows into the other's tree, and a hostile link between the layouts is refused;
- foreign-owned run-directory content (uid 0, 1000 and 101000: directories holding files, a non-writable
  directory, a sticky directory, loose files in the non-sticky 0777 `out` directory, and a live foreign writer) under the production retention delete
  (`GitCache.deleteRetainedArtifact`) in split mode (the real `setpriv`-to-runner `rm -rf`) and single-uid
  mode (in-process `fs.rm`): with a vetoing writer guard nothing is touched and nothing is counted; with the
  guards open the artifact is retained, the undeletable counter rises by exactly one, the "could not delete
  (not permitted)" diagnostic names the path and errno, and the foreign content that cannot be removed (directories holding files, the non-writable and sticky directories) keeps its owner, mode and inode, and the live foreign writer keeps appending. The loose files in the non-sticky 0777 `out` directory are not asserted either way.

The container gets a 415-second watchdog plus five-second kill grace; each leaf has its own
timeout of at most 115 seconds. Its results are checked against `shared-expected.json` like the
other lanes. These leaves need the root-started capability set, so they cannot run in the worker proof
container (which has dropped to uid 10001) and are not skippable: the container asserts its own uid,
capability set, `no-new-privileges` and tmpfs `/data` first. Not reproduced, because they need
`CAP_SYS_ADMIN`: the dind container's subPath-only mount view (a process cannot replace or traverse above a
bind-mount root) and the kubelet's fsGroup application.

`task test:worker-uid-harness` runs the Docker-free checker, shell wrapper and
required-check regression tests. The wrapper tests stub Node inventory/pattern,
Docker and timeout, use the real checker, and verify status reporting, stderr,
scratch reports and cleanup of exactly the wrapper’s owned resources. Both lanes
have status, skip, missing-report, assertion-failure and watchdog regressions.
The scoped inventory regression is Docker-free:

```sh
node --import ./agent/node_modules/tsx/dist/loader.mjs --test --test-concurrency=1 --test-timeout=120000 e2e/worker-uid/inventory.test.mts
cd agent && npx tsc --noEmit -p ../e2e/worker-uid/tsconfig.json
```

The required task runs the scoped typecheck and inventory regression through
`test:worker-uid-harness`; it retains the existing CI job and aggregate check.

Selection uses escaped, anchored full leaf names from that inventory. Any extra
executed leaf fails; unrelated leaves may only be absent or skipped. The normal
host shards continue to own the other tests in these files. This nine-leaf
selection proves only the selected capture behavior under the worker UID;
acceptance of the full capture suite remains a separate full-capture run.

CI runs this required lane on every main push and on PRs changing agent sources,
this harness, Taskfile or the CI workflow. The existing required `test-agent`
aggregator accepts a skipped lane only when a successful PR path filter explicitly
reports no relevant changes. Image-validation builds retain their existing role.

After the existing worker and command proofs, `task test:agent:worker-uid` requires
both calibration sizes. Local callers must set `WORKER_MEMORY_CALIBRATION_RUN_ID`
and `WORKER_MEMORY_CALIBRATION_ATTEMPT` to explicit local values. CI captures its
own run identity and attempt. Any missing prerequisite, failed leg or report
validation failure fails the same required task and existing CI aggregator.

The M5b calibration runner uses a **provided local image only** with the
locked dependencies and both prebuilt Codex helpers. It never builds, pulls or
installs. An explicit local invocation is:

```sh
python3 -B e2e/worker-uid/calibration.py --image mem2396-offline:complete \
  --mode local --run-id <unique-local-id> --attempt <explicit-attempt>
```

In CI, use `--mode ci`; identity comes from the host's nonempty
`GITHUB_RUN_ID` and `GITHUB_RUN_ATTEMPT`. The script verifies that its root is
the actual Git checkout root before recording the full HEAD SHA. A host manifest
binds unique invocation, fixture, report, leg and tree IDs to the two finite
private cgroups and immutable container IDs before either start.

The two sizes run serially. Each fixture enrolls two actual broker commands,
uses the production reader and monitor, and accepts only the selected command's
actual feedback and confirmed drain. The fixed plan uses 1 MiB chunks, 16/32
rounds and four charge classes, with a 64 MiB overhead allowance and a total
budget no greater than half the cgroup limit. Load is bounded at 30 seconds,
owned cleanup at 30 seconds, and the fixture wall at 90 seconds including host
cleanup. Host capture owns 60 seconds including kill/reap grace, followed by at
most 30 seconds of host cleanup. The checker also bounds the reported Node clock
to that capture window. Sampling at 250 ms allows 268 records: six baseline reads, up to
30 seconds load plus 30 seconds cleanup plus five seconds quiet dwell, and two
boundary ticks. Raw samples are capped at 6,144 bytes; reports at 8 MiB. Evidence
is refused on overflow, never truncated into acceptance.

Version 1 explicitly records `units`: memory bytes, monotonic timing milliseconds,
growth bytes_per_millisecond, PSI averages percent and totals microseconds, events
count, and process start identity as opaque kernel start ticks (not clock time).
The existing clock source and time origin remain separate. `plannedChargeClasses`
names touched anonymous memory, dirty disk, fsynced/read file cache, and tmpfs shmem.
Each class has two producers, 1 MiB chunks, at most 16/32 rounds per producer,
and a combined logical budget of 32/64 MiB; the four budgets sum to
`plan.allocationBytes`. These are planned worst-case logical budgets, not measured
per-class charges or extra headroom from shared cache. They remain null before
a validated plan is available.

`oomEvents` names the first/last raw sample IDs and their before/after
`oom`, `oom_kill`, and `oom_group_kill` counters, plus literal after-minus-before
deltas, including the final raw read. Known zero stays zero; missing endpoints or
counters stay null, with null deltas. Partial evidence and negative observed deltas
remain in failed reports; reader unavailability does not erase known event evidence.
No raw records are discarded. Host-aborted unknown metadata is null.
The positive checker independently verifies these fields from raw evidence and
still requires stable OOM counters across every raw record.

The version-1 derivation attributes every adjacent periodic sample pair by real
sample IDs, signed raw byte delta, completion interval, positive byte/ms rate and
read gap. Peak rate and maximum gap select the earliest pair on ties; baseline
jitter names the earliest minimum/maximum baseline samples and their byte values.
`samplingSchedule` separately records the 250 ms requested anchor/ordinal/deadline,
actual dispatch, reader start/completion and signed lateness (early ticks are valid),
plus maximum positive lateness and maximum absolute jitter in milliseconds.
Manual prerequisite/baseline/final probes are excluded. Failed legs retain captured
scheduling facts with null derived metrics; a host-aborted unknown schedule is null.
Scheduling jitter in ms and read gaps are evidence only: the strict reserve check
remains growth rate times drain-minus-bracket-lower plus baseline jitter in bytes,
without adding either timing quantity again.

The host retains each original producer stdout and exit-status sidecar in its
printed scratch evidence directory. After ownership-verified container
cleanup, it emits exactly one final version-1 JSON report per size between the
standalone calibration markers. Each final report carries its host-observed
container cleanup proof; the matching trusted `host-cleanup.json` sidecar is
also required by `calibration_check.py HOST_MANIFEST REPORTS HOST_CLEANUP`.
A missing or malformed producer report becomes an explicit aborted failure with
unknown evidence set to null. No next size starts after incomplete cleanup or
an active-leg failure. A demonstrated unavailable reader before any command
launch permits collecting the other size's failed prerequisite evidence.

Earlier local prerequisite probes lacked `memory.pressure` at both sizes.
On any such host this fixture fails before load; it has no PSI substitute (only the Node diagnostic guarded leg below does) or
privileged fallback. CI acceptance requires both complete successful bound
reports; local prerequisite failures and synthetic regressions provide no
measurement acceptance. Docker-free regression data lives in
`calibration_synthetic.py`. Focused regressions are:

```sh
python3 -B -m unittest discover -s e2e/worker-uid -p 'calibration*_test.py'
node --import ./agent/node_modules/tsx/dist/loader.mjs --test --test-concurrency=1 --test-timeout=120000 e2e/worker-uid/calibration-producer.test.mts
cd agent && npx tsc --noEmit -p ../e2e/worker-uid/tsconfig.json
```

### Node assert diagnostic calibration (issue #2471)

After the two reports, `calibration.py` runs a measurement-only step of eight serial
containers (both sizes, same flags as the fixtures, `--user 10003`, capless, no network).
Per size: the generic Node 24 workload `many-mismatch` (`assert.deepStrictEqual` on two
200,000-pair arrays, every second pair differing) and its two controls, `equivalent`
(equal arrays) and `bounded-diagnostic` (boolean compare plus a fixed short message), each
as a plain child so a kernel OOM kill is observable; and the **guarded leg**
`guarded-many-mismatch`, the same workload run as a guarded Claude command. The guarded
leg drives the real `ClaudeCommandLane` (`lane.hook` then `lane.handle`, as the SDK does),
the real `launchRunnerCommandRoot` with the image supervisor at the container's own uid,
a real `WorkerMemoryCommands` context, the real `WorkerMemoryReader` and the real
`WorkerMemoryMonitor`; only the reservation ack service is a loopback fixture
(`calibration-claude-guard.mts`). Each result is one JSON line between the
`UZI_NODE_DIAGNOSTIC_BEGIN/END` markers and is kept in `node-diagnostic.json`.

The guarded measurement reports the node version; the sampled series
(`[sampleId, ms, currentBytes]`, bounded to 200) and peak growth; the threshold-crossing
sample, reservation request/grant, cancel and confirmed-drain times with the derived
crossing-to-reserve, cancel-to-drain and crossing-to-drain latencies; the exact
`worker_memory_pressure` tool result the lane returned for the attributed tool use (or the
recorded `oom_before_guard`, `deadline` or `exited_before_guard` outcome; the last requires the workload's own report line in the output as evidence it started); and
cleanup evidence (lane live count and quiesce, empty registry, the command's pids gone, no
leftover process, `oom_kill` delta, `memory.peak`). The guard's high-water mark (half the
cgroup limit), the 250 ms sampling period and the 5 s response budget are measurement
parameters picked to give the guard room to act, never hosted production thresholds. A guard
that did not act in time or an OOM kill is a recorded result; a lane or launcher construction
failure, a launch failure or lane refusal, a supervision or enrollment failure, an admission closed
without the guard's result, a workload that never started (exit 127, an import error), unparseable
output or unconfirmed cleanup fails like any host error, after the owned
container is removed. A kernel kill of the container's main process before it reports (exit
137) is recorded as `container_killed_no_report`. The guarded container's host bound is 90 s (the
plain workloads keep 30 s), above the leg's ~59 s in-container worst case, so a slow `deadline`
outcome is recorded rather than cut by the host.

A host kernel without PSI (no `memory.pressure`) makes the production reader report
`absent`. Only in this guarded leg's harness the reader gets a `readBytes` that is the real
bounded read except that an absent `memory.pressure` is replaced by an all-zero PSI
document; every other file (current, max, stat, events) stays real. Any such run sets
`psiSynthesized: true` in the measurement, so a report never claims real PSI. The two
Codex fixtures above have no such substitute and still fail on that host.

[Issue #2397: partial investigation](issue-2397-investigation.md) records the
available evidence and remaining diagnosis work.
