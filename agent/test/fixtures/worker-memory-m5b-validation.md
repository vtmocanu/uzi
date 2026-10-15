# M5b — calibration implementation and incomplete acceptance

Implementation is committed and reviewed at `f837a257bb7da13c303ed540671ce18d450b019f`.
M5b acceptance remains **INCOMPLETE**: real 1 GiB/2 GiB calibration failed its
actual-reader memory.pressure prerequisite before commands or allocation. No successful
reserve inequality, cancellation latency or hosted sizing claim is made. The
[calibration procedure](worker-memory-calibration.md) defines the remaining acceptance.

## Delivered behavior and review

The required worker-UID task additionally requires host-bound, complete version-1
calibration reports for both private finite sizes. The fixture uses the real reader,
monitor and Codex command adapter, two registered model command trees, bounded logical
allocation classes and expected-cancellation handling. The checker independently
reconstructs raw sample-pair growth, the threshold bracket, response spans, byte
jitter, scheduling lateness, OOM deltas, bindings and exit/cleanup proof. Missing,
malformed, failed, duplicate, mismatched or incomplete reports fail the existing task.
Task direct I/O preserves exact standalone stdout markers. Contributor run.sh build
behavior and workflows are unchanged; this run used the provided offline image.

This is test-harness state, not a production privilege, persisted production store
or default configuration. Real reader unavailability fails before commands/load.
Synthetic regression reports remain explicitly nonmeasurement. The sampled hard
crossing bracket runs from the last below-threshold sample's start to the first
at-or-above sample's completion. The end-to-end bound is
`drain.endMs - hardBracket.lowerMs`, never an exact kernel crossing measurement;
the sampler gap is already included and must not be added again.

| Committed review unit | Scope |
| --- | --- |
| `c5a76961` | Core reports/driver |
| `94e43ca0` | Failure and exact-feedback repair |
| `190879f1` | Host/fixture/checker composition |
| `954d33e6` | Atomic ACK, quiet dwell, timing, binding, typed evidence and owned cleanup repair |
| `0746d843` | Exact expected SIGKILL exit and producer-fault repair |
| `8814c888` | Report pair, scheduling, units, classes, OOM completeness and Task framing |
| `f837a257` | Partial raw counter preservation |

Reviewer, tester and auditor reviewed each immutable unit and mandatory repair range;
demonstrated mandatory findings were closed through `f837a257`. Earlier canceled coder
dispatches supply no checks or sign-off.

At `8814c888`, tester passed 85 focused tests (34 helper/driver, 24 checker, 12 host,
12 producer, 3 framing) plus scoped TSC. The compiled earliest-tie mutant failed its
intended assertion. The reviewer partial-counter finding was mandatory despite its
nonblocking label. At `f837a257`, tester independently passed 14 producer cases,
scoped TSC, lint/deadcode and 3 Task framing cases on pinned 3.54.0. Old capture
behavior compiled, then failed the raw-text regression: actual null versus two known
counters, 13 pass/1 fail. Missing counters remain unknown and refuse completion;
malformed raw records remain refused. Full gates are recorded separately below.

## Actual composed lane

Executed command:

```sh
WORKER_UID_IMAGE=mem2396-offline:complete WORKER_UID_SKIP_BUILD=1 WORKER_MEMORY_CALIBRATION_RUN_ID=local-f837a257-final WORKER_MEMORY_CALIBRATION_ATTEMPT=1 .uzi/scratch/task-bin/task test:agent:worker-uid
```

Source was `f837a257`, on local Linux with Node 24.20.0, Python 3.14.7 and Task
3.54.0. The cached offline fixture image was
`sha256:22d5a16b260c8bb2359b392c4c86a923e22bf262bfb7e470aba4a95c5eff5478`.
No build, pull or install ran inside this measurement. The image includes prebuilt
helpers; it is a fixture image, not production-image or worker-to-command UID-transition
proof. Supervisor inputs were unchanged versus `164ef8`.

From 2026-10-15T21:44:53Z to 21:47:58Z (185 seconds local),
`gate-log.DKSwGC` recorded EXIT=201, with inner calibration exit 1. Harness regressions,
the scoped compiler, 38 required worker leaves and 1 real detached command-tree proof
passed. Calibration produced exactly two compact, complete standalone **FAILED**
reports with the correct host SHA, local identity and finite sizes:

- Both observed UID 10003, zero capabilities, NNP and `0::/`.
- Both raw memory.pressure values were null: the actual default reader's PSI input
  was absent. Known OOM counters and deltas were 0.
- Neither report contained plan, tree, allocation, ACK, signal or span measurements.
  Both immutable containers had confirmed owned host cleanup.

Raw reports, the trusted manifest and cleanup sidecars remain under
`.uzi/scratch/memory-calibration-8a61377b82532ea382eb909e2a5f9204`.
Independent log parsing confirmed two failure blocks, no command handles and matching
removed IDs. Bounded read-only metadata checks also confirmed all four exact
worker-UID journal handles absent (owned dependency volume and three containers);
each Docker call used a five-second GNU timeout with five-second kill grace.
Scratch references in this record are run-local evidence, not durable
published artifacts.

Earlier no-load probes `gate-log.9jOTII` (EXIT=1) and `gate-log.1WljG0` (diagnostic EXIT=0) established
the same missing PSI prerequisite. No PSI substitute, reader change, privileged
workaround, shared-worker load or unchanged retry was attempted. A PSI-enabled
fixture host remains required before M5b can complete.

## Previously executed gates and scans

These are recorded results on the implementation, not successful calibration.

| Check on `f837a257` | Recorded result | Local interval on 2026-10-15 | Scratch log |
| --- | --- | --- | --- |
| Repository gate | EXIT=0 | 21:48:17Z–22:04:47Z; 990 seconds | `gate-log.j0YFp9` |
| Agent gate | EXIT=0 | 22:05:27Z–22:58:38Z; 3191 seconds | `gate-log.Udg9Wy` |

Agent units reported 15,181 tests: 15,166 pass, 15 skip, 0 fail, 0 cancel;
real-procfs skip accounting reported 0. Recorded skips cover packaged-profile,
UID/root/platform, missing ssh-agent and live-provider prerequisites; required
worker-UID leaves executed separately above. Codex M4 reported 183 pass/0 skip and completeness
exit 0. The helper progress counter is not the authoritative test total.
API/web/controller/deploy/docs inputs were unchanged versus `0371db71`; their prior
completed component gates and approved M1 sweep-equivalent coverage remain reused
evidence, not reruns (see [M1](worker-memory-m1-reconciliation.md) and
[M4](worker-memory-m4-validation.md)). No affected Compose phase ran here.
Dependency lockfiles were unchanged; no M5b install ran.

At `8814c888`, auditor secret canaries were DETECTED with zero tracked findings
(`auditor-1-secrets.kBF6Uw`); the Semgrep canary was detected with zero applicable
tree findings (`auditor-1-semgrep.xr8KhD`). Full agent/web dependency audits reported
zero vulnerabilities (`auditor-1-npm.1I6XpF`, `auditor-1-web-audit.4bNZ7w`). At
`f837a257`, fresh secret canaries and the agent audit passed. govulncheck was
unavailable; it was neither downloaded nor passed. Synthetic or no-load success
is not real-cgroup acceptance.

## Timing, scope and remaining acceptance

Earlier equivalent local workersvc durations were default `4103` 707.393 seconds
and candidate 882.581 seconds: +175.188 seconds (+24.77%). They used the same
20-minute flags and fresh DB, but inventories, cache and order differed; this is not
causal or CI evidence. The approved M1 record retains the original 15-minute timeout,
complete fresh 20-minute continuation and skip/required-case accounting. No unchanged
single green sweep is claimed.

The earlier worker-UID default `4103` baseline was 50 seconds and
[M5a](worker-memory-m5a-validation.md) was 150 seconds. The current 185-second failed
composition is 135 seconds longer than the baseline, but a successful equivalent
final comparison remains missing. Image setup was outside measured lanes. No CI
queue, cache or critical-path timing was measured; post-publication comparison
remains lander-owned under approved M5 timing.

The branch merges `4103fad73896f103bf7cebd743f009b0b6bde671` under explicit human
deviation and lacks #3039; the lander must integrate current main before landing.
That change's three workflow/harness files remain unchanged. Production remains off
by default; Claude is deferred to #2471. Sizing remains literally
`max(1Gi, 10-15% of memory.max)` and uncalibrated. Bursts, stalls, node pressure and
separate dind remain limitations.

The literal tracked inventory refreshed at `8814c888` found 960 matches for the
requested memory episode/count/policy/pressure/hold/capturedMemory/bind/reconcile
alternatives. This covers literal matches only; indirect control flow and behavior
are not claimed exhaustive. M1 inventory and subsequent contract checks remain
linked evidence.

Deferred milestone notes, retained as review dispositions:

| Revision/report | Deferred note |
| --- | --- |
| Tester `c5a76961` | Coverage/pre-commit: 2 categories |
| Tester `94e43ca0` | Formatter/coverage/pre-commit: 3 categories |
| Reviewer `94e43ca0` / `190879f1` | Hosted CI timing accepted as lander-owned |
| Auditor `190879f1` | 1 below-bar uncapped ACK-read note: fixed trusted producer and bounded protocol, no demonstrated adversarial amplification |
| Tester `8814c888` | 1 optional coverage suggestion |
| Tester `f837a257` | Formatter/coverage: 2 categories |

No graded security or demonstrated mandatory correctness finding remained. Prior
milestone deferred notes remain in their records.

M1/M4/M5a are complete; **M5b is incomplete**. No completion or publication signal
was issued. Successful real 1 GiB/2 GiB measurements and final successful composed
timing await a suitable PSI host. Small-fixture success, when obtained, will still
not establish production reserve settings.
