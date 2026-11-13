# Builder-cache prune leg proof (#4291)

From the repository root:

```sh
task test:dind-builder-prune
```

This is a disposable integration proof for the pressure-only BuildKit cache prune leg
(`agent/src/dind-builder-prune.ts`), whose frozen command is

```
docker builder prune -f --filter type=regular --filter until=24h --max-used-space 4GiB
```

It is separate from the component gates (`task gate`) and from CI. It starts owned throwaway
DinD daemons. Measured on the gate run: about 137 s per posture and about 275 s in total (rootful
137 s, rootless 137 s, measured after the event-driven hold; the red A run takes about 60 s (57 s measured)), under a 1500-second overall deadline.

If a run is killed, reap its containers by hand: `docker ps -a --filter label=dbp-test.owner`, then
`docker rm -fv <exact name>` after checking each container's `dbp-test.owner` label is yours.

## Requirements

Bash, GNU `timeout`, `jq`, `curl`, `node` with `agent/node_modules` installed (the worker installs it at
run start; the harness fails, never installs), and Docker access that may start privileged
containers. The worker's own client `/usr/bin/docker` and
`/usr/libexec/docker/cli-plugins/docker-buildx` are copied into the nested containers, the
same pair the real controller runs (as measured 2026-11-13: docker 29.9.2, buildx v0.38.1; the
harness prints the actual versions at run time). The daemon images are the chart's pins:
`docker:29-dind` and the rootless `docker:29-dind-rootless`; the fixture `alpine:3.24` pin matches
`e2e/dind-recycle/run.sh` (all by digest). Missing docker, client or
plugin, a setup error, or a failed assertion fails the run; there are no skips. A posture that cannot
start prints `BLOCKED: <posture>: <evidence>` and the run exits non-zero.

## Design

For each posture, rootful then rootless, one owned nested daemon (`dbp-test-<hex>-<posture>`) runs with
`--debug`, an unauthenticated TCP listener (`--tls=false`, port 2375 inside the container's own network
namespace, not published), and its data root on a size-limited `tmpfs` (320 MB) so a real `df` of that
filesystem can cross the 85% watermark. Every version is printed: client, buildx, server
Version/APIVersion/MinAPIVersion/commit and BuildKit.

Phases (`DBP_PHASES`, default `points real pressure`; `DBP_POSTURES`, default `rootful rootless`).
The effective selection is printed on an `EFFECTIVE SELECTION:` line at the start of every run, so an
inherited `DBP_*` value in the environment is visible and never silent.

### Verdicts and exit codes

- `RESULT: PASS` (exit 0) needs BOTH postures and EVERY required phase (`points`, `real`, `pressure`)
  to have run with zero failed assertions and no blocked posture.
- Any narrowed run (`DBP_POSTURES` or `DBP_PHASES` leaving out a posture or phase) ends with
  `RESULT: PARTIAL (diagnostic only, not the required proof): ran <posture/phase list> ...` and exits 2,
  even if everything it ran passed. It is for debugging, not proof.
- An unknown posture or phase name, or an empty value (`DBP_PHASES=`), fails before any container is
  created (exit 1, names the valid values).
- `RESULT: FAIL` / `RESULT: BLOCKED` exit 1. `DBP_EXPECT_RED=switch-off` has its own verdict: exit 0 only
  with `RESULT: RED A CONFIRMED` (it needs `rootful/pressure` to have run, otherwise PARTIAL, exit 2);
  it never prints PASS.

- **points 1-6**, each with a positive control deleted in the same prune; record IDs, CacheType, InUse,
  Shared and LastUsedAt are printed before and after, and every exit status is printed.
  1. an old unused regular record is deleted;
  2. a record held by a concurrent build survives while the unheld same-age control is deleted, the
     held build completes rc 0, and the formerly held record is deleted by the same prune once the
     build has finished and aged past the floor. The lease is proven, not assumed (see below);
  3. an `exec.cachemount` record survives while a regular control is deleted;
  4. a record used within the floor survives while an older control is deleted;
  5. a tagged image's (shared) records survive and `docker run --rm` of it works;
  6. the combined `type` + `until` + `--max-used-space` filters are accepted and select with AND
     semantics: a cap above usage deletes nothing, an old non-regular record and a young regular
     record both survive a cap below usage, and only the record that meets every condition goes.

  The exact production argv also runs three times (fresh cache, data present, final), each rc 0 and
  each deleting nothing, because every record is far younger than 24 hours.
- **missing/broken plugin**, isolated: a separate owned alpine container with only the copied `docker`
  client (none of `/usr/local/lib`, `/usr/local/libexec`, `/usr/lib`, `/usr/libexec` `docker/cli-plugins`
  exists, checked) and a fresh empty `DOCKER_CONFIG`, pointed at the nested daemon by TCP, with
  `DOCKER_BUILDKIT=1`. Cases: plugin absent; a garbage non-executable `docker-buildx`; a garbage
  executable `docker-buildx`. Each runs the frozen argv and a labelled scaled argv, must exit non-zero
  with the product's refused match string (read from `BUILDX_MISSING_TEXT`), leave a deletable control
  record in place (the scaled argv would delete it if a prune ran), and leave no `/build/prune`,
  `/grpc` or `/session` request in the nested daemon's debug log during the case (the detector
  fires on any one of those three). The detector is validated first: a real prune must trip it. The same absent case
  without the pin is recorded as `CONTRAST`, not asserted.
- **real path**: `drive-leg.mts real` runs the production argv through the real controller exec path
  (default spawn, `/usr/bin/docker`, the default plugin path, `dockerExecEnvironment` plus
  `DOCKER_BUILDKIT=1`) against `tcp://<nested IP>:2375` and requires outcome `completed`; then aborts
  a just-started prune client and requires a group kill, confirmed settlement inside the 10 s bound, no `-unsettled` log
  and no surviving client; then a fresh controller completes again. The sample is synthetic there (only
  the exec path is under test). If the nested IP is unreachable from the worker the phase reports that
  as a limitation and fails; it is never faked.
- **pressure relief**: the data root is filled with unshared regular cache (cacheonly builds of 24 MB
  random-data layers) to at least 86%, then aged past the scaled floor. The real controller from
  `createDindBuilderPrune` (both switches on) runs with `readSample` returning the REAL
  `stat -f` of that filesystem and a TEST-ONLY `exec` shim that first asserts it received exactly
  `DIND_BUILDER_PRUNE_ARGV`, compared against a literal copy in `drive-leg.mts`, and then runs the
  scaled variant inside the nested daemon. PASS needs outcome `completed`, a post sample below 75%
  (the harness's own df and the controller's logged after-sample), and the held, cachemount, recent
  and tagged-image fixtures still present (the held build completing rc 0, the image still running).

### Lease-proof sequencing (point 2 and the pressure held record)

The hold build is event-driven. Its last `RUN` step waits, bounded by 900 s, for a sentinel file in a
shared BuildKit cache mount, and the harness writes the sentinel with a second tiny build only after the
prune it must survive. Before P1 the harness (1) waits until the held record's `LastUsedAt` is older than
the scaled floor plus a margin, (2) bumps the `recent` record, then (3) takes a fresh `system df -v`
snapshot and asserts the hold build process is still running and the held record is `InUse=true` with
`LastUsedAt` older than the floor, so age exclusion cannot be what protects it (the line is
`LEASE-PROOF[<posture>/before-P1] <time>: ...`). Right after P1 a second fresh snapshot asserts the hold
build is still running and the held record is present and still `InUse=true`
(`LEASE-PROOF[<posture>/after-P1]`). Only then is the sentinel written, the hold build must finish rc 0,
and the later P1b prune must delete the formerly held record. The pressure phase uses the same hold and
the same before/after checks around the controller's prune.

### Raw byte evidence

`docker system df -v --format json`, `docker buildx du --format json` and `docker buildx du --verbose`
(client 29.9.2, buildx v0.38.1) expose only formatted size strings (for example `3.146MB`, `4.1kB`), no
raw bytes. The Engine API `GET /system/df` of the owned nested daemon does (`BuildCache[].Size` is an
int64), and the harness reads it with the nested container's busybox `wget` over the daemon's own TCP
listener (127.0.0.1:2375 inside the container). Every `records [...]` listing prints `bytes=<n>` next to
the ID and formatted size, followed by a `raw bytes total` line, for the fresh, built, during-hold,
before-P1, after-P1, before-P1b, after-P1b and pressure snapshots. The raw API snapshots are kept as
`api-df-<label>.json` in the output directory. If that read ever fails the harness prints
a `FAIL` line and `bytes=unavailable`, so the run cannot PASS; it does not fabricate values. Record ages are computed only from `+0000 UTC` LastUsedAt values; any other zone aborts the run.

The fill uses `RUN dd` layers on the alpine base rather than `FROM scratch` + `COPY`: a COPY puts the
blob in the build context, and BuildKit keeps context data as `source.local` records that
`type=regular` deliberately cannot reclaim, so the fixture would not be relievable by the frozen
command.

## Red demonstrations

- `DBP_EXPECT_RED=switch-off task test:dind-builder-prune` runs only the rootful pressure fixture with
  `dindBuilderPruneEnabled: false` passed straight to `createDindBuilderPrune` (it returns undefined,
  no leg). This does not exercise environment parsing of `UZI_DIND_BUILDER_PRUNE_ENABLED`; the agent
  unit tests cover that. It
  asserts that the driver exits 3 (no relief, pressure unchanged, no prune request in the daemon log)
  and prints `RED A CONFIRMED`. `DBP_SWITCH=off` without the expectation makes the same run a plain
  FAIL.
- Widening the frozen argv in `agent/src/dind-builder-prune.ts` fails the driver at import (the
  module's independent guard literal); if the guard literal is widened too, the driver's own literal
  rejects the shim's argv.

## TEST-ONLY scaled variants

The real floor is 24 hours and the real cap is 4 GiB, which a disposable fixture cannot cross. Wherever
the harness deviates, the printed line is labelled `TEST-ONLY scaled argv:` and uses
`until=<DBP_FLOOR_S>s` (default 25) and a small `--max-used-space` (1MB for the points, 64MB for the
pressure fixture). The exact production argv is only ever run unscaled in the three "exact" prunes and
in the real-path phase.

## What is NOT proven

- The real 24-hour floor (only a scaled floor is observed) and the real 4 GiB cap.
- Behaviour at real volume sizes, ordering among thousands of records, or a multi-hour build.
- The Kubernetes lifecycle: the dind-meter sidecar and its sample file, maintenance fencing, custody,
  restarts, or the worker's ticks every 30 seconds (this driver ticks every 1.1 s with distinct epochs).
- Any particular storage driver: the harness prints the nested daemon's storage driver but does not
  assert it, so the legacy graphdriver versus the containerd snapshotter is not pinned.
- The rootless posture on a real node's storage; here its data root is a tmpfs.
- Concurrent prune races beyond one held build.

## Safety

The worker's own Docker daemon is shared. The harness only pulls images and creates, starts, execs,
copies into, inspects and reads logs of its own containers (`dbp-test-<random hex>-*`, never `uzi-*`),
each labelled `dbp-test.owner=<token>`. No prune, build, system or volume command is ever pointed at
the worker's daemon: they run through `docker exec` into an owned daemon or against that daemon's own
TCP address. Cleanup runs in a trap, verifies the ownership label, removes only the exact names with
`docker rm -fv`, and preserves an earlier failure status. No clock is altered. Output and the daemon
JSON snapshots land in `.uzi/scratch/dbp-<token>/` (override with `DBP_OUT`).

`plan-evidence/` holds the owner-accepted plan-time spike that first established points 1-6; it is an
archive, checksummed in its own README, and not executed by any target.
