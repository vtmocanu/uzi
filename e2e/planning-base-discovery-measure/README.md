# planning-base-discovery-measure: issue #3543 base-discovery measurement

**Measurement only.** This harness changes no production code, bound, ADR or doc. It measures
the plan cross-check capture's existing base discovery so the owner can choose between the two
resolutions in #3543. No Task target, gate or CI job runs it.

## What runs

`run.sh` drives `agent/test/planning-base-discovery.measure.ts` inside the worker image. The image
is `MEASURE_IMAGE`, by default the locally built `uzi-agent-home-split:base`. Build an equivalent
with `docker build -f agent/templates/base/Dockerfile -t <tag> .`. The harness never edits the
helper (except in the validation-only deadline lift, below): it calls the production `GitCache.capturePlanningDiff` path with the production envelope
of `Runner.captureCheckedPlanningDiff` (`agent/src/runner.ts`):

- a 60 s scope timer and a combined abort signal;
- a `TickSpawner` with its default 2000 ms kill grace;
- `scope.abort()`, then `settled()` and `survivors()`;
- the default `PLANNING_LIMITS`.

The helper is the unchanged `PLANNING_CAPTURE_HELPER`, with all of its bounds: 2 MiB per
commit/tree object, depth 64, a 28 s helper deadline inside the 30 s outer timeout, 5 s per git
command, and yields about every 20 ms.

- **One container per memory configuration**, created once. Creating one from the 10 GB image
  took about 2 minutes on the measuring host. Each job is one `docker start`, so the cgroup's
  `memory.peak` starts fresh for every job.
- **Container settings:** `--memory` equal to `--memory-swap` (no swap), `--cpus`,
  `--network none`, and the host uid. `tini` is PID 1, as in the production image.
  With `node` as PID 1, killed `git` children stay as zombies. The process group then never
  empties, and every cancellation reads as a surviving group: measured, 4 s settle,
  `cleanup_failed`.
- **Default configurations:**
  - 1536 MiB / 1 CPU, the compose per-slot budget (`docker-compose.yml`), with modes `full`,
    `control` and `cancel`;
  - 4 GiB / 2 CPU, with mode `full`. This is the compose whole-worker default
    (`AGENT_MEM_LIMIT:-4g`, `AGENT_CPUS:-2`) and the chart's namespace LimitRange default. Hosted
    workers use size presets instead.
- Each container first runs a no-op warm-up start. A fresh container's first start carries a
  node/tsx startup transient (about 140 MiB at start, against about 60 MiB later, observed once),
  so no measurement is a first start. The warm-up logs its cgroup figures to `logs/warmup__N.log`.

```sh
OUT=/abs/out REPO_SHA=<commit> ./e2e/planning-base-discovery-measure/run.sh   # full matrix
OUT=/abs/out FIXTURES="fanout2-d8 chain-d64" MEM_CONFIGS="1536m:1:full" ./e2e/planning-base-discovery-measure/run.sh
```

`OUT` is reused: fixtures already generated there are kept, and each job deletes its own
earlier records before it starts. The report renders every result present in `results/`, so use a
fresh `OUT` (or keep its fixtures and clear `results/`) for one invocation's table. The table is
`$OUT/results/results.md`, and the raw per-job JSON sits beside it.

## Fixtures (deterministic generator)

The generator uses pinned author/committer identities and dates, and plumbing-built objects.
Each fixture writes a `manifest.json` with its commit, its tree, and the analytic tree, leaf,
entry and path-byte counts (decimal strings; see the charge model below).

| spec | shape |
|---|---|
| `repo` | `git archive REPO_SHA`, recommitted. Generation fails unless the tree id equals `REPO_SHA^{tree}` |
| `synthetic-N` | N unique files, 100 per directory; file *i* is `(i*2654435761) mod 4096` bytes |
| `chain-dD` | linear chain, deepest tree at depth D (root is depth 0). `chain-d64` is the deepest accepted; `chain-d65` must refuse `directory_cap` |
| `fanout2-dD` | binary DAG of D+1 distinct trees whose traversal visits 2^(D+1)-1 tree and 2^D leaf occurrences |
| `wide-FxW` | **extra, beyond the issue's spec**: one W-entry leaf tree referenced F times |
| `longpath-dDnNwWfF` | **extra**: long-name chain, see below |
| `distinct-FxWnL` | **extra**: F directories, each its own W-file leaf tree, L-byte names, all F*W blobs distinct (maximal sizing batch), see below |
| `fatchain-dD` | **extra**: D nested directories, every tree near 2 MiB (peak-memory worst case), see below |

`repo` and `synthetic-*` clones are checked out. The adversarial bases are cloned with
`--no-checkout`, because their exploded working tree cannot exist. Discovery reads only `.git`
and the bare's objects, so the discovery numbers are unaffected. The post-discovery capture
does differ: every base path becomes a deletion. In production, a checkout of such a base
would have to happen before any capture.

### `longpath-d<depth>n<namelen>w<width>f<fan>`

The root holds `fan` (>= 1) directory entries that all reference one shared chain of trees. The
chain's deepest tree holds `width` regular files (`w0`: the deepest tree is the empty tree, made
with `git mktree` on empty input). Every directory and file name is exactly `namelen` bytes: a
letter (`d` fan entry, `c` chain level, `f` file), the zero-padded index, then `x` fill. Names
within a tree are therefore distinct and already in canonical order.

**Depth convention.** The root tree is helper depth 0, and the helper refuses depth > 64 with
`directory_cap`. The deepest tree sits at helper depth `depth`, so `1 <= depth <= 64`. Any path
runs through `depth` directories (the fan name, then `depth - 1` chain names), and the deepest
file path has `depth + 1` components (the helper accepts up to 65). The generator and the model
refuse a spec outside the grammar, a `namelen` too short for its index digits, and any tree
object over the helper's 2 MiB per-object cap (checked analytically, then again with
`git cat-file -s`). Occurrences: `1 + fan*depth` trees, `fan*width` files. Example:
`longpath-d60n200w100f2`.

### `distinct-<F>x<W>n<L>`

The root holds F directories (`d000000`..., fixed width, so F <= 1,000,000). Each references its
OWN leaf tree of W regular files named exactly L bytes (`f`, the zero-padded index, then `x`
fill; `L >= 1 + digits(W-1)`, `L <= 255`). Every blob is distinct across the whole fixture
(content is the global index and a newline), so there are F*W distinct blobs and the helper's
sizing batch (`cat-file --batch-check` over all distinct blob ids) is maximal after discovery
completes. Generated with one `git fast-import` stream (inline blobs, fast-import builds the
trees; the root tree is the commit's tree), so F*W near 1.2 million is practical. The generator
and model refuse any tree object over the helper's 2 MiB per-object cap (checked analytically,
then with `git cat-file -s` on the root and the first and last leaf tree). The manifest adds
`distinctBlobs` and `maxTreeObjectBytes`. Occurrences: `1 + F` trees, `F*W` files. Example:
`distinct-1170x1024n75` (1,198,080 files, 1,199,250 entries, just under the 1,200,000 entry cap,
so the sizing batch runs if discovery completes in time; 1171 would refuse at the entry cap first).

### `fatchain-d<depth>`

A chain of `depth` (1..64) nested directories; the root is helper depth 0. Every tree holds
60,000 filler regular files (one shared blob, names `f` + 5 digits) and, except the deepest, a
directory entry `0` that sorts first in canonical order (`0/` before `f...`; the generator asserts
it from `git ls-tree`). Each tree object is 2,040,028 bytes (1.95 MiB; the deepest, without `0`,
2,040,000), under the 2 MiB cap. The
helper descends at the first entry, so every ancestor's tree buffer is live at the deepest point
while only `depth` entries are charged: up to `depth + 1` near-2-MiB buffers at once. The deepest
tree's filler is charged at the bottom, with every buffer still live; the ancestors' filler is
charged while unwinding. Which cap then refuses depends on depth: d19-d48
the entry cap; d49-d64 the path-byte cap (d64: E = 830,983, P = 100,663,348); below d19 neither
(filler plus directory entries stay under 1,200,000). Ask the `model` job for a
given depth. The manifest adds `maxTreeObjectBytes`.

## Charge model and basis labels

The harness carries an analytic model of what the planned base-discovery caps charge. It never
runs the helper. It walks the tree in the helper's order (root entries in canonical git order,
descending into a directory the moment its entry is met) and charges entry `i`:

- **entry occurrences** `Em(i) = i`: every tree entry, leaf or directory, the root's included,
  once per walk occurrence (a shared subtree is charged each time it is reached);
- **path bytes** `Pm(i)`: the sum over entries `1..i` of the entry's full relative path bytes
  (its ancestors' `name/` prefixes plus its own name).

Shared-DAG specs use closed-form subtree totals, so huge fixtures (`fanout2-d64`,
`wide-16384x16384`) need no enumeration. `repo` and `synthetic-N` read `git ls-tree -r -t` of
the generated bare. The manifest records `expectedEntryOccurrences`, `expectedPathBytes` and
`maxChargedPathBytes` (the longest single entry path), all decimal strings.

Print the model for a spec without docker or the opt-in environment (`repo`/`synthetic-N` also
need `"fixture":"<generated fixture dir>"`; `entryCap`, `pathByteCap` and `at` are optional):

```sh
cd agent && node --import tsx test/planning-base-discovery.measure.ts \
  '{"kind":"model","spec":"longpath-d60n200w100f2","entryCap":100,"pathByteCap":50000}'
```

`at` may be one index or an array (`"at":[3000,2999]`). One index prints `prefixAt`; an array
prints `prefixAts` (`em` = i and `pm` = Pm(i) per index). Both also print `probes`: for each index
`i`, the 7f probe pair as ready-to-paste `limits` objects and `limitsJson` strings.
`(entryCap=i, pathByteCap=Pm(i)-1)` must refuse with `base_discovery_path_byte_cap`, and
`(entryCap=i, pathByteCap=Pm(i))` with `base_discovery_entry_cap`. A pair is `null` when the walk
is too short, the cap would be 0, or it exceeds `run.sh`'s 15-digit bound.

Every charge in a record or table carries one basis label:

| label | meaning |
|---|---|
| `analytic` | from the model. For a completed discovery the meter's fetched tree-entry total must equal it: `meterMatch` is `true`, or the record carries a `FAIL METER_MISMATCH` flag, the row shows it, and the job exits non-zero |
| `derived-from-refusal (unverified until 7f probe)` | for a `base_discovery_entry_cap` refusal: E = entryCap + 1, P = Pm(entryCap). For `base_discovery_path_byte_cap`: k = first i with Pm(i) > pathByteCap, E = k, P = Pm(k), and k must not exceed the entry cap. A contradiction (or caps unknown for the job) gives `derived.charge: null` with a reason; a contradiction also sets the `FAIL` flag |
| `treeEntriesFetched` | the meter's count of entries in the trees the helper fetched. It is exact only when discovery completed. Otherwise it is an upper bound on what was charged and is never shown as a charge (`killed` rows show `fetched ≥N (not charged)`) |
| `validation` | the row ran with injected limits (below), never on production defaults |

The caps come from the limits the helper actually receives. `GitCache` keeps its defaults in a
private field (`PLANNING_LIMITS` is not exported), which the harness reads, so a production row
uses whatever caps the helper ships with and records them under `limits`. A cap refusal whose
cap value the harness cannot read is recorded as `derived.charge: null` with a reason and the
`CAP_UNKNOWN` failure flag.

## Validation jobs (injected limits)

A measure job may carry `"limits": {...}`: positive safe integers merged over the helper's
defaults and injected with `GitCache.setPlanningLimitsForTest`. Keys must be existing planning
limits (the two base-discovery caps are among them). The record sets
`validation: true` and the report labels the row `validation`. In `run.sh`:

```sh
LIMITS_JSON='{"baseDiscoveryEntryCap":1000}' OUT=/abs/out FIXTURES="fanout2-d8" ./e2e/planning-base-discovery-measure/run.sh
```

`LIMITS_JSON` must be a one-line JSON object of identifier keys to positive integers of at most
15 digits; `run.sh` refuses anything else before starting a container, and the label gets a
`__validation` suffix. Without it every row runs on the production defaults. It cannot be
combined with `SMOKE_INJECT`, and one invocation's rows are all validation or all production.

Rows of two `LIMITS_JSON` invocations into the same `OUT` overwrite each other unless their
labels differ. Set `LIMITS_TAG` (`[a-z0-9-]{1,40}`, requires `LIMITS_JSON` or `DEADLINE_SCALE`)
to append it to the label as `__validation-<tag>`, e.g. `LIMITS_TAG=probe3000-path`.

### Deadline scale (validation only: the deadline is stretched)

A measure job may carry `"deadlineScale": N` (integer 2..8; absent means production). It lets a
near-cap base finish under the production caps when this container is slower than a production
worker. It changes only the helper's clock: the metering preamble replaces `Date.now` in the
helper process with `pbdT0 + floor((real - pbdT0) / N)`, so the helper's 28 s deadline lasts
28 s x N of real time. The outer 30 s capture timeout (the `timeoutMs` argument of
`readBoundedPlanningOutput`, which `GitCache` also hands to the spawner), the harness's own 60 s
scope timer and `run.sh`'s per-job wait are multiplied by the same N. Every cap and memory bound
stays at its production value; the preamble's own timing keeps the real clock. Every other
`Date.now` reader in the helper runs on the slowed clock: each git command's timer is 5 s of real
time but is clamped to the deadline's remaining helper milliseconds, so a command started in the
last 5 helper-seconds (the last 5 x N real seconds) gets 1/N of the stretched time left, and the
helper's 20 ms event-loop yield cadence becomes 20 x N real ms. The job is rejected when combined with
`injectDeadlineAfterSizing`, and for any mode other than `full`: `control` runs without the
preamble, so its helper would keep the production deadline under a stretched label, and `cancel`
measures cancellation, not memory at the caps.

The record sets `validation: true` and `deadlineScale: N` (null on production rows); `captureMs`
and `discovery.ms` stay real elapsed milliseconds, and `helper.clockScale` /
`helper.helperClockElapsedMs` give the helper's slowed clock for comparison with
`helper.snapshotMs` (real). The report shows `validation (deadline ×N; real <ms> ms)`. Such rows
measure memory and charges at the caps, not production timing, and must be reported as
deadline-stretched.

```sh
DEADLINE_SCALE=4 OUT=/abs/out FIXTURES="distinct-60x19998n75" ./e2e/planning-base-discovery-measure/run.sh
```

`DEADLINE_SCALE` (`^[2-8]$`) works with or without `LIMITS_JSON`; the label gets
`__validation-dl<N>` (with `LIMITS_TAG`: `__validation-<tag>-dl<N>`). It cannot be combined with
`SMOKE_INJECT`.

### Deadline lift (validation only: the wall-clock bounds are lifted)

`DEADLINE_LIFT=1` (the only accepted value) lets a base just under both production caps run its
whole capture, sizing batch included, on a host too slow for the 28 s deadline. It is
harness-only: no production flag or hook exists and `agent/src` is untouched. The harness
rewrites the helper source text it hands to its spawner
(`agent/test/planning-deadline-lift.ts`) and nothing else:

- the helper deadline `Date.now() + 28000` becomes 1,800,000 ms;
- the per-git-command timer ceiling `Math.min(5000, ...)` becomes 1,800,000 ms. It is lifted too
  because the helper's deadline clamps it, and an earlier ×4 row stopped on that timer at under
  half the sizing batch;
- the outer 30 s capture timeout becomes 1,830 s, the harness scope timer 1,860 s and `run.sh`'s
  per-job wait 1,900 s.

The rewrite fails closed: each marker must occur exactly once, the source must carry the
`base_discovery_entry_cap` token, and undoing the two replacements must reproduce the input byte
for byte; otherwise the job errors. Every cap and memory bound keeps its production value.

- **Labels:** `__validation-lift`; the record carries `validation: true` and a `deadlineLift`
  object with the four lifted values. Name these rows "validation, deadline lifted, unmetered"
  (`control`) or "validation, deadline lifted, metered" (`full`); they are never an unchanged
  production-path control, and they measure memory and charges at the caps, not production timing.
- **Allowed modes:** `full` and `control` only. `cancel` is refused.
- **Exclusions:** refused with `DEADLINE_SCALE`, `SMOKE_INJECT`, `LIMITS_JSON` and `LIMITS_TAG`
  (exit 2), and in a measure job with `injectDeadlineAfterSizing`.

```sh
DEADLINE_LIFT=1 OUT=/abs/out FIXTURES="distinct-1170x1024n76" MEM_CONFIGS="1536m:1:control 4g:2:full" ./e2e/planning-base-discovery-measure/run.sh
```

`control` rows carry no preamble, so they have no helper-side timing. A parent-side phase
observer fills that in (next section).

### Phase observer and phase evidence (lift rows)

The 10 ms sampler of a lift row reads `/proc` for the helper and its descendants. It records, in
ms from the job start, in `phases` (null when never seen):

- `sizingSeenMs` / `sizingGoneMs`: the sizing `git cat-file --batch-check` was running / had ended;
- `postSizingSpawnSeenMs`: a later git spawn other than `cat-file`;
- `indexOpenSeenMs`: the clone index was opened (best-effort; a short open can be missed);
- `sizingPid`.

`phaseEvidence` is `completed` when the capture completed. It is `post_sizing_observed` when the
capture was refused after the sizing batch was seen and gone, a later spawn or index open was seen
at or after that time, and every time is no later than the outcome. Anything else is null: the
row proves nothing about the post-sizing window. On a `full` row the metering preamble can only
contradict the observer (`PHASE_OBSERVER_MISMATCH`, a failure flag); a phase the observer missed
is never a mismatch. The helper itself is not instrumented by the observer.

### Row verification (`verify` job)

`verify` re-reads each row's `<label>.json` and `<label>.term.json` under the results directory,
writes `verify.json` there and exits non-zero unless every row is valid. It needs no docker and
the harness does not require the opt-in variable for it, so it runs on the host:

```sh
cd agent && UZI_PLANNING_DISCOVERY_MEASURE=1 node --import tsx test/planning-base-discovery.measure.ts job <jobfile>
```

with a job file of

```json
{"kind":"verify","out":"<results dir>","rows":[{"label":"...","expect":"completed|phase-evidence|<token>"}]}
```

A row is valid only with container exit code 0, no OOM, no harness kill, matching labels, no
failures, an outcome matching `expect` and, on a lift row, the exact lifted values.
`expect` is `completed`, `phase-evidence` (a recorded `phaseEvidence` consistent with the outcome)
or a refusal token such as `base_discovery_entry_cap` (which also needs a derived charge). Cite a
lift row only after it verifies.

## Disk preflight and readability gate

**Disk preflight.** Before any work, before each fixture generation and before each container
creation, the host filesystems of `/data` and of `$OUT` and the Docker store must each have at
least `MIN_FREE_GIB` GiB free. The Docker store is read with a zero-write probe container
`pbd-dfprobe-<pid>` (created once; `df -Pk /` of the image). A store below its floor, or one that
cannot be read, prints `BLOCKED: disk preflight (<what>): <store> has <n> KiB free, need >= <floor>`
and exits 3 without creating anything further.

- `MIN_FREE_GIB`: integer >= 5 (default 5); lower or non-numeric exits 2.
- `MIN_FREE_GIB_DOCKER`: floor for the Docker store only (default `MIN_FREE_GIB`); an integer not
  below `MIN_FREE_GIB`, else exit 2. It can only raise that one floor.

**Preserving readability gate.** A container is removed (`docker rm -f -v`) only after every
result it produced is confirmed readable: the label's result `.json`, its term record and a
non-empty log must each exist and be non-empty, and the JSON files must parse. There is no
exception: a missing result is unreadable whatever the term record says. Otherwise the container
is not removed: it is stopped (`docker kill`, `docker wait`; never `rm`, never `-v`), its id, name
and the failing files are appended to `$OUT/logs/PRESERVED.txt`, `BLOCKED: unreadable results
preserved in container <id>` is printed and the script exits 4. The exit trap applies the same
check to a container still standing.

An OOM-killed, non-zero-exit or harness-killed job normally has no readable result, so it also
preserves its container and stops the invocation. That is intended. The caller recovers the
results from the preserved container, then removes it by its exact name (from `PRESERVED.txt`);
never by pattern.

**Gate self-test.** `GATE_SELFTEST=1` proves the gate and nothing else: it creates the first
`MEM_CONFIGS` container, runs the warm-up and one no-op under the label `selftest__gate`,
truncates that result file to empty and runs the gate. The gate must preserve the container, and
the script then prints `GATE_SELFTEST passed: ...` and exits 6, distinct from a real block (exit 4)
and from a failed self-test (exit 5: the gate accepted the empty file). The caller removes the
container by the printed name and id. It cannot be combined with any other mode variable, and no
fixture is exported or generated.

## Modes and metrics

- **`full`** prepends a metering preamble to the helper's `-e` program, using the technique of
  `git-planning-diff.test.ts`'s meter. It counts:
  - tree occurrences (`cat-file tree` spawns), tree entries, and commit/tree/check/sizing bytes
    during discovery;
  - total git spawns, and bytes from `fs.readSync`, which covers worktree, index and ignore reads.
- **`control`** runs without the preamble. It records wall time, the outcome, and the
  parent-side memory figures only. Compare it with `full` to see the instrumentation overhead on
  time and on parent-observed memory. The preamble's effect on the helper's own maxRSS is not
  separately quantified.
- **`cancel`** aborts the owner signal on a metrics condition, recorded as the trigger: after 50
  tree occurrences mid-discovery, or at the first post-discovery spawn. `cleanup` is `ok` only
  when all three hold:
  1. abort → `settled()` ≤ 2000 ms;
  2. `survivors()` is empty;
  3. no `uzi-planning-*` remains in the helper's actual TMPDIR, the one recorded from the spawn
     request. That TMPDIR must also equal the job's own temp dir.

  A cancel cannot arm once the capture's outcome is known, and only an abort that landed before
  the outcome relabels a refusal `cancelled`. The `after outcome` marker is a defensive check.

  Helper exit 143 only means the SIGTERM handler reached `process.exit`. That handler is
  `try { cleanup(); } finally { process.exit(143); }`, so 143 is not proof of cleanup.

**Discovery boundary.** Discovery counts as completed only when the helper opens the clone index.
That open is the first operation after `discovering = false` (`agent/src/git.ts`, after the
sizing batch, its validation, the credit loops and limit initialisation). `discovery ms` is
helper start → that open. A run that stops earlier reports `no`. The time of its last
discovery git close is kept in the JSON as a lower bound, not as completion. A smoke-only fault,
`SMOKE_INJECT=1`, expires the helper deadline right after the sizing batch closes. It must
report `no`.

**Memory columns:**
- **helper maxRSS:** `process.resourceUsage().maxRSS` at the boundary and at exit. The
  boundary value is taken synchronously inside the helper, so it is exact for discovery.
- **Parent-observed discovery columns are polled, not synchronous.** The parent's tree-RSS
  discovery peak and its cgroup `memory.peak` at the boundary are taken when the parent first
  reads the helper's boundary snapshot. The lag is up to one 10 ms poll plus the parent's
  scheduling delay, which shares the CPU with the helper at 1 CPU. They can therefore include
  early post-discovery work, such as the first capture spawn. The cgroup `memory.peak` value is
  monotonic, so it is an upper bound for discovery. The discovery tree-RSS peak is approximate: polling
  lag can push it above the true discovery peak, and the 10 ms sampling can miss a short peak and leave
  it below.
- **tree RSS:** the sum of VmRSS over the helper and its descendants, sampled every 10 ms. It
  can miss short peaks, and it double-counts shared pages.
- **cgroup peak:** `memory.peak` at the boundary and at the end. It is the authoritative
  container figure, and it includes the harness parent (about 60 MiB at capture start, recorded
  per job as `cgroup.currentAtStart`) and page cache.

**Termination classes:**
- `oom` only with affirmative evidence: Docker `OOMKilled`, or an increase in `memory.events`
  `oom_kill`, which also catches a helper killed while the parent survives.
- `killed_by_harness` when `run.sh` itself issued `docker kill` after `START_TIMEOUT`. This is
  recorded before the kill.
- `killed_unknown` for any other exit 137.
- `helper_killed_unknown` when the helper died by a signal (TickSpawner code 128) that the
  envelope did not send, with no OOM evidence and no cancel.
- `harness_error` for other failures without a result: the harness process itself, or a failed
  `docker wait` (`wait_failed_rc_N`, after which `run.sh` stops the container).

A job whose result, term record or log is missing, empty or unparseable (these classes, an OOM
kill and a harness kill included) fails the readability gate: the container is preserved and the
whole invocation stops with exit 4 (see "Disk preflight and readability gate"). The caller recovers
the results, removes the container by its exact name and reruns the remaining configurations.

For killed runs, values from the last persisted snapshot (every 100 ms) are shown as `≥` lower
bounds. Unavailable values are `n/a`.

## Deviations from production

- There is no worker/runner UID split and no command sandbox. The helper runs as the host uid,
  the same identity as the harness.
- The image and node/git are the production worker's, but the host, CPU quota and storage are
  the measuring host's. Absolute times are host-specific.
- The production run lane holds the boundary through a run-owned spawner. Here the
  `TickSpawner` test hooks `onSpawn`/`signalGroup` only observe: they return `undefined`, so
  signal delivery is real.
