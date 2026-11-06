# planning-base-discovery-measure: issue #3543 base-discovery measurement

**Measurement only.** This harness changes no production code, bound, ADR or doc. It measures
the plan cross-check capture's existing base discovery so the owner can choose between the two
resolutions in #3543. No Task target, gate or CI job runs it.

## What runs

`run.sh` drives `agent/test/planning-base-discovery.measure.ts` inside the worker image. The image
is `MEASURE_IMAGE`, by default the locally built `uzi-agent-home-split:base`. Build an equivalent
with `docker build -f agent/templates/base/Dockerfile -t <tag> .`. The harness never edits the
helper: it calls the production `GitCache.capturePlanningDiff` path with the production envelope
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
Each fixture writes a `manifest.json` with its commit, its tree, and the analytic tree and leaf
occurrence counts (decimal strings).

| spec | shape |
|---|---|
| `repo` | `git archive REPO_SHA`, recommitted. Generation fails unless the tree id equals `REPO_SHA^{tree}` |
| `synthetic-N` | N unique files, 100 per directory; file *i* is `(i*2654435761) mod 4096` bytes |
| `chain-dD` | linear chain, deepest tree at depth D (root is depth 0). `chain-d64` is the deepest accepted; `chain-d65` must refuse `directory_cap` |
| `fanout2-dD` | binary DAG of D+1 distinct trees whose traversal visits 2^(D+1)-1 tree and 2^D leaf occurrences |
| `wide-FxW` | **extra, beyond the issue's spec**: one W-entry leaf tree referenced F times |

`repo` and `synthetic-*` clones are checked out. The adversarial bases are cloned with
`--no-checkout`, because their exploded working tree cannot exist. Discovery reads only `.git`
and the bare's objects, so the discovery numbers are unaffected. The post-discovery capture
does differ: every base path becomes a deletion. In production, a checkout of such a base
would have to happen before any capture.

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
- **helper maxRSS:** `process.resourceUsage().maxRSS` at the boundary and at exit.
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
