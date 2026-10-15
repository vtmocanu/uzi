# Worker UID test lane

`task test:agent:worker-uid` builds the base worker image and runs the ownership and cleanup
leaves from five test files through its root entrypoint, which drops to the worker UID with runner
group membership and the production ambient capabilities. It runs offline, with
disposable data and Nix mounts, and removes only its own named container.

To reuse an image, set `WORKER_UID_SKIP_BUILD=1` and `WORKER_UID_IMAGE=<image>`.
`WORKER_UID_TIMEOUT` controls the outer watchdog (default 600 seconds).
The separate command proof has a 115-second watchdog plus five-second kill grace
(120 seconds total). All Docker operations use GNU timeout. Image metadata, volume
creation and ownership inspection each get five seconds plus five-second kill grace.
The optional local build gets 60 seconds plus five-second grace; dependency
provisioning gets 20 seconds plus five-second grace (120 seconds total including
metadata and the optional build). Cleanup has a shared 30-second budget including
kill grace and reaping; each metadata/removal request gets at most 10 seconds,
including at most five-second grace. It tries each recorded resource once, continues
after failures while budget remains, and propagates failure or exhausted budget.

Every invocation uses an unpredictable 128-bit token in its names and ownership
labels. Creation requests are journaled before sending, so an ambiguous Docker
failure or lost acknowledgement still triggers ownership inspection. The volume
is explicitly created and its label checked before mounting it. Cleanup inspects
only attempted resources, verifies the invocation label, and removes containers
by their returned immutable IDs and the volume by its verified name. Missing,
malformed or mismatching ownership metadata fails closed: planned names alone
never authorize removal. Metadata/build failure before creation performs no
resource cleanup. Containers remain until the trap.
`WORKER_UID_SKIP_BUILD=1` requires the hosted image to exist locally; the harness
never pulls an image or installs runtime dependencies.

The host needs Docker, Node, Python, GNU timeout and the locked agent development
dependencies. Test inputs are mounted into `/app`, preserving their relative paths
and resolving runtime packages from the image.

A second, serial container starts directly as command UID/GID 10003, with all
capabilities dropped and no-new-privileges. It mounts this harness, agent source,
agent tsconfig/package metadata and vendored supervisor source read-only under `/proof`.
The package metadata preserves the source’s ESM boundary. A unique
Docker volume is seeded from the image’s locked `/app/node_modules` and mounted
read-only at `/proof/agent/node_modules`; scratch, Go cache and Go temporary files
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
without changing the agent project. The default inventory retains the five old
files; `node e2e/worker-uid/inventory.mjs command` emits the separate command
inventory. `expected.json` / `results.xml` and `command-expected.json` /
`command-results.xml` are checked separately. The JUnit
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
printed scratch evidence directory. After ownership-verified container and volume
cleanup, it emits exactly one final version-1 JSON report per size between the
standalone calibration markers. Each final report carries its host-observed
container cleanup proof; the matching trusted `host-cleanup.json` sidecar is
also required by `calibration_check.py HOST_MANIFEST REPORTS HOST_CLEANUP`.
A missing or malformed producer report becomes an explicit aborted failure with
unknown evidence set to null. No next size starts after incomplete cleanup or
an active-leg failure. A demonstrated unavailable reader before any command
launch permits collecting the other size's failed prerequisite evidence.

The current host's real prerequisite probes found `memory.pressure` absent at
both sizes. Real calibration acceptance remains unverified pending a suitable
host; this fixture fails before load on that prerequisite. There is no PSI
substitute or privileged fallback. Docker-free regression data in
`calibration_synthetic.py` is explicitly synthetic and never accepted as a
measurement. Focused regressions are:

```sh
python3 -B -m unittest discover -s e2e/worker-uid -p 'calibration*_test.py'
node --import ./agent/node_modules/tsx/dist/loader.mjs --test --test-concurrency=1 --test-timeout=120000 e2e/worker-uid/calibration-producer.test.mts
cd agent && npx tsc --noEmit -p ../e2e/worker-uid/tsconfig.json
```

[Issue #2397: partial investigation](issue-2397-investigation.md) records the
available evidence and remaining diagnosis work.
