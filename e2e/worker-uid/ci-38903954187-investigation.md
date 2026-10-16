# CI 38903954187 — worker UID investigation

## Observed failure and scope

The published pipeline tested `104faf8060190ba19e5a74ee83b5753b703cba9b`.
Its production image build passed; `Require worker UID leaves` failed with task
exit 201 / underlying exit 1. Both agent shards passed. The hosted job's detailed
log could not be acquired without authentication, so its first error is unknown.

Default `6e6ebe3b7b918062fe13ad947dbe27b1575be89c` was merged without rewriting
published history in `5c1bb710c538e19bebbe02ba7e1bc73075e3140a`. The load fixture
and calibration producer are unchanged between the published revision and that
merge. The merge tree was reviewed and matches the automatic merge preview.

## Local evidence

- The published canonical task stopped at the cold production image build's
  existing 60-second watchdog. This is a prerequisite failure, not reproduction
  of the hosted failure, whose image build succeeded.
- Reusing the public rc.18 image, the focused `run.sh` passed 38 worker leaves and
  two command leaves with successful owned cleanup. Its older baked supervisor
  does not establish calibration compatibility.
- A scratch image reused that runtime and rebuilt the branch's three supervisor
  helpers from vendored source, offline. Its ID was
  `sha256:6c33153981f5ae2d7347c1bc6106aa1ef39236b3441cd237f15a550320261f47`.
  Dockerfile, toolchain definitions and JS dependency manifests matched rc.18.
- Real calibration at both 1 GiB and 2 GiB stopped before enrollment because
  `/sys/fs/cgroup/memory.pressure` was absent. This remains an environment blocker;
  it is not evidence that the production reader should accept absent PSI.
- A scratch diagnostic substituted zero PSI bytes only, labelled reports
  `synthetic`, retained the other real cgroup counters and original bounds, and
  used the original host's ownership checks and cleanup. Both command producers
  then exited during their first step with `ERR_WORKER_OUT_OF_MEMORY`. No cgroup
  OOM counters increased. The 2 GiB fixture was not started after this failure.
  Owned container removal was confirmed. This diagnostic is not real calibration
  acceptance and does not establish the hosted job's first failure.

## Isolated loader inheritance defect

Environment: Node `v24.20.0`, locked tsx `4.23.15`.

`calibration-producer.mts` launches under `node --import .../tsx/dist/loader.mjs`.
Its 16 MiB worker runs the CommonJS `agent/test/fixtures/worker-memory-load.cjs`.
That fixture creates up to four sequential churn workers with an 8 MiB old heap,
2 MiB young heap and 1 MiB stack. Without an explicit `execArgv`, each worker
inherits the parent's preload arguments. The CommonJS churn payload does not
need the TypeScript loader.

A focused reproducer launched this unmodified fixture with the same worker data
and heap limits under the locked loader. Each case had a five-second watchdog
and awaited worker termination before deleting its owned temporary directories:

| Case | Result |
| --- | --- |
| Churn payload, 8 MiB old heap, inherited loader | `ERR_WORKER_OUT_OF_MEMORY` |
| Churn payload, 16 MiB old heap, inherited loader | `done` reply |
| Churn payload, 8 MiB old heap, `execArgv: []` | `done` reply |
| Full load payload, 16 MiB old heap, inherited loader | First step fails with `ERR_WORKER_OUT_OF_MEMORY` |
| Full load payload with only nested churn `execArgv: []` in a scratch copy | First step replies `step` |

The final control retains the outer worker's inherited loader and every original
heap, load and iteration bound. It isolates the nested worker's unnecessary
preload as a demonstrated fixture defect, independently of PSI availability.
Both the command producer and the older `calibrationThreadPort` scaffolding use
this same CommonJS fixture. Correcting the nested worker therefore covers both
callers without changing their contracts.

The next repair should explicitly clear the nested CommonJS worker's `execArgv`
and add a real first-step regression under the tsx preload. This is the best
supported repair candidate for the failing lane, not confirmation that it is the
only hosted failure. No production memory policy or calibration proof may be
weakened to accommodate the local PSI limitation.

## Verification limits and retained evidence

The merged tree's `task gate:repo` exited 0 under Task 3.53.1, with actionlint
explicitly skipped. Focused changelog checks also passed using the checksum-
verified CI Task 3.54.0 binary. Full CI-pin validation remains a delivery check.

Scratch evidence is available only while this runner is retained; it is not a
published or durable artifact:

- `gate-log.w7o8i0`: published canonical task's image-build prerequisite failure.
- `gate-log.4ntK0N`: focused ownership/command proofs and cleanup, exit 0.
- `gate-log.azlb4K`: real calibration's absent PSI prerequisite, exit 1.
- `psi-diagnostic-20261016-MzIYXm/`: diagnostic source substitutions, synthetic
  reports, producer error replies and host cleanup receipts.
- `gate-log.HRkEKW`: focused inherited-loader cases, exit 1 for failed cases.
- `gate-log.RSqNYg`: nested-worker-only control, first-step ACK, exit 0.
- `gate-log.vPsPAE`: repository gate, exit 0 with the stated tool limitations.
- `gate-log.A7uIrR`: focused checks under CI's verified Task pin, exit 0.

The hosted root cause, real PSI measurements, final composed-lane success and a
subsequent green hosted pipeline remain unproven.
