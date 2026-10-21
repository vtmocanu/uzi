# ADR-3153: Docker-lane `/data/runner` lives on a `runner-shared` subPath of the data PVC

**Status**: Accepted (issue #3153)
**Date**: 2026-10-20
**Deciders**: maintainer ("Go with guards" on the rootless question) and the agent team (issue #3153 milestones and review waves)
**Amends**: Decision 3 of `prds/done/83-docker-capable-worker.md` (dind mounts none of token, data or nix), now "dind mounts only the runner subPath of data"
**Related**: [ADR-837](0837-worker-disk-lifecycle.md), [ADR-1759](1759-dind-data-metering-and-prune.md), [ADR-1786](1786-stopped-source-retention.md), [ADR-1597](1597-midturn-checkpoint-durability.md), [ADR-1598](1598-codex-command-storage.md)

## Decision (summary)

On Docker-lane hosted workers, the run's working clones and `$TMPDIR` (`/data/runner/.tmp`) live on the `runner-shared` subPath of the worker's `data` PVC, mounted at `/data/runner` in both the worker and the dind sidecar. The `run-workdir` emptyDir is gone, so a pod replacement no longer destroys uncommitted edits or unpublished local commits.

## Context

- On 2026-10-20 a long-running Docker-lane run lost uncommitted work after a kubelet eviction. The run's clone lived on the `run-workdir` emptyDir at `/data/runner`, which is destroyed with the pod; only what had reached the worker's bare tracking ref or a checkpoint survived.
- The plain (non-Docker) lane already keeps the clone on the data PVC and was unaffected.
- The emptyDir existed so the worker and the dind daemon see the same bind-mount sources at the same path (`prds/done/83-docker-capable-worker.md`). The fix must keep that shared view.

## Decisions

### D1. Name the subPath `runner-shared`, not `runner`

- Existing data PVCs already carry a root-owned `0755` `runner` directory, the old emptyDir mountpoint that kubelet created. kubelet will not re-own it, so reusing the name would inherit a directory the worker cannot write.
- A new name gives a clean, worker-owned entry on every PVC. The worker sees it as `/data/runner-shared`; both containers see the subPath as `/data/runner` (`render_dind.go` `dindWorkdirSubPath`, `runnerWorkdirMount`).
- The `run-workdir` emptyDir is removed from the Docker-lane pod. The `codex-cmd-cache` emptyDir stays worker-only and unchanged (`adr/1598-codex-command-storage.md`).

### D2. `runner-init` prepares the subPath before dind

- A new init container (`render.go` `runnerInitScript`; golden `controller/internal/kube/testdata/runner-shared-init.sh`) runs before dind.
- **Image gate.** `/etc/uzi-runner-layout` must equal `runner-shared-v1` (baked in `agent/templates/{base,jvm}/Dockerfile`). Otherwise it refuses before any write of its own: "worker image lacks runner-shared layout support ... upgrade the worker image or roll back the controller". The gate covers `runner-init` and the containers after it only: the existing `seed-nix` init container runs earlier and, when it reseeds, already writes the nix volume and clears its provisioning state on the data volume.
- **Real-dir checks.** It refuses a symlink or non-directory at `runner-shared` or `.tmp`.
- **Ownership.** It creates `runner-shared` (`3775`, `worker:runner` under uid-split) and `.tmp` (`worker:worker` `0755` under uid-split; in single-uid mode both are owned by the worker uid, mode `3775`).

### D3. `TMPDIR=/data/runner/.tmp`; scratch is excluded from durability

- Docker-lane `$TMPDIR` moves onto the same volume so a bind source under it resolves in dind.
- Scratch under `.tmp` carries no durability guarantee.

### D4. Scratch adopt-or-refuse; `.retire-*` inspection; no new cleanup authority

- **Docker lane.** A per-uid scratch directory is adopted (expected-owner `0700` leaf; the entrypoint prints "writer exclusion not established") or refused, never wiped. The non-Docker wipe is unchanged.
- **`.retire-*` leftovers** are inspected at startup and always retained, empty ones included (an old mtime and emptiness prove neither retirement nor writer exclusion: a surviving daemon container may still hold one as a bind source or cwd). One counted warning reports them. The EXDEV fallback is kept.
- Nothing here adds a deletion path that bypasses custody or writer exclusion.

### D5. Retention cleanup retains what it cannot remove

- Daemon-written (root- or subuid-owned) content can make a removal fail with a permission error. The agent keeps the item and counts it: "runner clone retention: could not delete (not permitted); keeping it" (counter `retainedUndeletable`), "runner scratch removal failed; keeping it", "runner retire scratch retained".
- Automatic retention deletion stays off (#1858); see `docs/run-recovery.md`.

### D6. Decision 3 of PRD #83, amended (mount view only)

- dind mounts none of the token or `/nix`, and only the `runner-shared` subPath of `data` (pinned by `TestDindContainersMountOnlyTheRunnerSubPathOfTheDataVolume`).
- This is a mount-view property. It does not bound a rootful daemon's device-level reach (`prds/done/89-optional-nonrootless-dind.md`), which is unchanged.

### D7. Migration guard

- **One predicate**, `migrationHeld` (`controller/internal/kube/layout_migration.go`): Docker lane, `custody_held`, and the observed pod layout is old or unknown (unknown and mixed count as old).
- **Held paths.** Template drift (including `ForceRoll` and the paused restore), paced and run-bound roll admission, and `/nix` size-drift recycle. A held roll is never a canary and takes no roll slot; an incomplete own reservation is withdrawn.
- **Narrow exception to `ForceRoll`.** The operator's emergency `DrainPolicy.ForceRoll` is consent to requeue a busy run, not to discard custody-held state, so the hold applies even under it.
- **Already guarded and pinned elsewhere.** DinD maintenance stop, disk-pressure recycle (custody) and teardown (non-desired only; the api keeps custody-held workers desired).
- **Surfacing.** Per-tick log `layout-migration-held-custody count=N workers=...` and fleet-report pacing state `held`, reason `layout_migration_custody` (health warning "Worker roll held at <image>: layout_migration_custody"). Run-bound workers surface only when they have an observed roll-health row.
- **Unguardable.** Kubelet eviction, node drain and manual pod delete are outside controller control.
- **Conservative delay.** A leftover `Failed` or terminating old-layout pod still counts as old and can delay migration of a custody-held worker.

### D8. Capacity

- `DataVolumeGuard` (`agent/src/disk-full.ts`) samples `config.dataDir`. On the Docker lane runner bytes are now on that volume and count (pinned by `agent/test/data-volume-docker-layout.test.ts`).
- The Docker lane uses the existing data-volume thresholds. No Docker-specific values; measured thresholds are deferred to live acceptance.
- Admission fails open on an unknown sample and reopens after the bounded wait `UZI_DISK_ADMISSION_MAX_WAIT` (default 15m, `agent/src/config.ts`). **No admission headroom is claimed.**
- `workerDefaultDockerEphemeralRequest` stays 5Gi (not remeasured). Data PVC sizes are unchanged (presets 5/25/50Gi, run-bound 40Gi) pending live measurement.
- A data-PVC recycle of a non-custody worker (legacy `{nix,data}` pressure recycle, ephemeral-worker removal) deletes runner contents, which is plain-lane parity. A DinD-only maintenance recycle keeps `/data/runner` and loses only DinD state.

### D9. Availability residual: daemon-owned bytes on a custody-held worker (maintainer wording)

- Daemon-owned content is an extra source of unreclaimable bytes on the data PVC. This is **NOT plain-lane parity**.
- The counted retention diagnostics (D5) plus the existing volume-pressure reporting are sufficient. There is no privileged reclaim and no per-run byte accounting.
- A custody-held worker can exhaust its data PVC, which impairs recovery bookkeeping and needs an operator.
- Pressure or urgency never authorizes bypassing custody or writer exclusion.
- Never claim admission headroom (`UZI_DISK_ADMISSION_MAX_WAIT` reopens admission; it does not create space).

**Operator procedure** (existing tooling only; no new command):

1. Identify the held runs: `uzi run recovery` (owner and per-run views). (`uzi run held <run-id>` is not a custody-hold view: it lists the `refs/uzi-held` publications of failed runs, which are published as custody is released (an open hold normally has none, or one not yet acknowledged), so use it to find already-published held work.)
2. Preserve first: `uzi run export <run-id> --output FILE` for available archives. For retained clones with no open hold, run the worker-local inspect described in `docs/run-recovery.md`, "Retained sources with no open hold (worker-local inspect and discard)".
3. When safe reclaim is unavailable (inspect reports `refused` or a source it cannot prove covered, or removal fails with the "not permitted" diagnostic), stop there: preserve, do not delete by hand, and do not remove the PVC or pod to free space. Resolve the custody holds through the paths below, or enlarge the PVC where the StorageClass allows it.
4. Discard only through the supported paths: `uzi run discard <run-id> --hold <hold-id> --yes` for an open hold (`owner_hold_discard`), or the worker-local discard for `local_discard` / `local_protected_discard` dispositions, each with its own checks. `docs/run-recovery.md`, "Reviewing and resolving held work", covers the hold lifecycle.

### D10. Image skew and rollback ordering

- **Upgrade.** A worker image carrying the marker must be available before or with the controller. The chart's controller follows `appVersion`; the worker image is pinned by `workers.image.tag`, which `scripts/worker-tag-autobump.sh` bumps at release cut when `agent/templates` or `agent/src` changed.
- **New controller, old image.** `runner-init` refuses; the pod sits in `Init:CrashLoopBackOff` and roll health reports the blocking init container. `runner-init`, dind and the worker write nothing; `seed-nix`, which runs first, behaves as it did before #3153 (it may reseed `/nix` and clear its provisioning state on the data volume). Nothing touches `/data/runner` or `runner-shared`.
- **Old controller, new image.** Works (reverse-skew path: the entrypoint keeps its old root handling and never wipes; test "reverse skew").
- **Controller rollback after migration.** The old controller mounts an emptyDir at `/data/runner` again. The PVC keeps `runner-shared/` untouched (the worker sees it as `/data/runner-shared`). Recovery journals and ledgers in `/data/repos` record clone paths under `/data/runner/...`, which now resolve into the fresh emptyDir and read as missing until roll-forward restores them. The old controller has no migration guard.
- **Not the same as resuming a run across the layout change.** Custody-held work stays on its old-layout pod until custody releases; it is not migrated. A non-custody run whose clone was on an old-layout emptyDir finds that clone missing when it is resumed after migration, because the journal paths resolve into the new `runner-shared` (consistent with `docs/run-recovery.md`, "Retained sources with no open hold").
- **Advice:** drain custody before a controller rollback.

### D11. fsGroup re-walk widening residual

- The pod uses `fsGroupChangePolicy: OnRootMismatch` with fsGroup 10001 (`render.go`). If kubelet re-walks the volume (root mismatch, fresh mount), it applies group 10001 and `g+rw` recursively across the whole PVC, including `runner-shared` clones and `.tmp`.
- The entrypoint refuses a worker-owned scratch leaf with group or other write, failing startup closed with contents preserved. `align_pvc_root` restores the root to avoid repeat walks.

### D12. #3031 classification check

- `source_missing` is a pure path-absence check with no lane or emptyDir assumption, so the move needs no classification change.

## Rootless merge condition

The issue made "rootless isolation is preserved" a merge condition. The maintainer answered "Go with guards". The record:

- **Mount view.** As rendered, dind's only `data` mount is the `runner-shared` subPath at `/data/runner` (`render_dind.go` `runnerWorkdirMount`, pinned by `TestDindContainersMountOnlyTheRunnerSubPathOfTheDataVolume`). The bare cache, agent home, recovery journals, `/nix` and the token Secret mount are not in dind's mount namespace, and `..` at a bind root does not reach the PVC. This is a mount-view property only. It does not bound a rootful daemon's device-level reach, which `prds/done/89-optional-nonrootless-dind.md` already accepts.
- **Identities.** Under uid-split, every runner and command spawn uses `setpriv ... --init-groups` (`agent/src/runner-uid.ts`), which drops the pod's fsGroup 10001. The image groups give neither runner (10002) nor runner-cmd (10003) gid 10001, and `/data` is `worker:10001 2775`, so neither can create, rename or unlink `/data/runner-shared`. The rootless daemon (uid 1000) sees only the subPath, never its parent.
- **Permissions.** `runner-init` makes `runner-shared` a sticky, worker-owned `3775` directory and `.tmp` a worker-owned directory inside it (uid-split: `worker:runner` `3775` and `worker:worker` `0755`; single-uid: both `10001:10001` `3775`, `render.go` `runnerInitScript`). Sticky means no container can delete or replace the worker's top-level entries. The cost is a functional limit on a rootless dind container, recorded below.
  - **Owner decision 2026-10-21:** accepted as a documented single-uid regression; modes unchanged.
  - **Writer.** A rootless dind container runs as host uid 1000 (typically a 100000+ subuid range for a non-root container user; the range depends on the image and is not pinned in this repo). By inference (runc sets only the mapped gids), it does not hold the pod fsGroup 10001, and container-root capabilities do not apply to inodes whose owner is unmapped, so it is judged on the "other" bits. The rootless daemon (`render_dind.go` `dindUID` 1000) does hold 10001.
  - **What predates #3153 (unchanged).** Writing inside a run's checkout fails in both uid modes at base `dea3ba68` and now. Checkout creation (`agent/src/git.ts`) is identical. Single-uid checkouts are `0755`/`0644` `worker:10001` under the default umask 022 (`agent/src/main.ts` sets umask 002 only under uid-split); uid-split checkouts are cloned as the runner (`agent/src/git.ts` `runnerCloneForBranch` through `runGitAsRunner`), so a checkout is `runner:runner` `2775` under a worker-made `worker:runner` repo directory, other `r-x`. Under uid-split nothing changed anywhere: the top of `/data/runner` was `worker:runner` `3775` at base (the `agent/templates/entrypoint.sh` carve-out) and still is, and `TMPDIR` leaves were `0700` then and now. The worker-uid lane measures `EACCES` for uid 1000 (+10001) under uid-split (`e2e/worker-uid/runner-shared-real.test.mts`), a more privileged identity (it holds 10001), so a denial for it is the stronger test.
  - **What is new, single-uid only.** At base `/data/runner` was the run-workdir emptyDir root, `2777` (kubelet emptyDir `0777` plus fsGroup; base `render.go` "writable by fsGroup") with no sticky bit, and `TMPDIR` was that same root. A rootless container could create, rename and delete entries at the top of `/data/runner` and in the `TMPDIR` root, and checkouts live at `<runnerRoot>/<repoDir>/<key>`, where the single-uid repo directory is worker-owned `0755`, so the container could rename the top-level `<repoDir>` entry in the non-sticky `2777` root and plant its own tree. Now `runner-shared` and `.tmp` are `10001:10001` `3775` sticky, other `r-x`, so a rootless container can no longer create entries there. An fsGroup re-walk applies the kubelet mask (owner and group bits, `0660`, `0110` on directories, setgid) and never adds "other" access, so it does not change this for the container (it widens the daemon's reach, already residual D11). The daemon (uid 1000 plus 10001) can still create at the top and in `.tmp` in single-uid mode, so daemon-auto-created bind sources still work.

    | Mode | Target (rootless container) | Base `dea3ba68` | Now |
    |---|---|---|---|
    | uid-split | create at top of `/data/runner` | denied (`3775`, other `r-x`) | denied, unchanged |
    | uid-split | `TMPDIR` root | leaves `0700` | `.tmp` `0755`, leaves `0700`; unchanged for the container |
    | uid-split | inside a run checkout | denied (`2775`, other `r-x`) | denied, unchanged |
    | single-uid | create at top of `/data/runner` | allowed (`2777`) | **denied** (new) |
    | single-uid | create in the `TMPDIR` root | allowed (same `2777` root) | **denied** (new) |
    | single-uid | inside a run checkout | denied (`0755`, other `r-x`) | denied, unchanged |
    | either | rootful dind container running as root, anywhere in the bind mount | allowed | allowed (only persistence changed, accepted delta (a)) |
    | either | rootful dind container running a non-root `USER` | judged on mode bits | judged on mode bits, as the rootless rows |

  - **Broken pattern.** In single-uid mode, a rootless container that bind-mounts the `/data/runner` root or the `TMPDIR` root (for example `-v "$TMPDIR":/x`) and writes into it now fails with a permission error; it worked before #3153. A bind of a `mkdtemp` subdirectory (`0700`) was never writable to the container. This is not claimed to be rare.
  - **Evidence level.** Auditor code analysis only (base vs HEAD via `git show dea3ba68:<path>`). No real rootless container was run. The uid-split leaf measures uid 1000 (+10001) only. The real rootless-container/UID functional check remains an open validation item (see Not verified).
- **Accepted delta (a).** Daemon-written content (root-owned under rootful dind, subuid-owned under rootless) now persists into later runs on that worker, as runner-written content already does on the plain lane.
- **Residual (b), single-uid subPath re-resolution.** Kubelet prepares subPath mounts again on every container start, native sidecar restarts included: it resolves the entry with `EvalSymlinks` and accepts any result inside the volume (kubelet v1.31 `pkg/volume/util/subpath/subpath_linux.go`, read from source, not run on a live kubelet). `runner-init` is an ordinary init container and does not rerun when dind restarts.
  - **Scope.** The extra daemon-mount exposure is confined to **single-uid mode**. There the worker uid owns `/data`, so agent code could replace `runner-shared` with a symlink to `.` (or rename a real directory such as `repos` to `runner-shared`), and the next dind restart would mount that instead. Agent code in that mode already reads and writes those data-PVC bytes and drives the daemon, so no new principal gains access to them.
  - **Recommendation.** Run Docker workers in **uid-split mode**. Its protection depends on two verified invariants: runner (10002) and runner-cmd (10003) are not members of gid 10001, and every runner and command spawn uses `--init-groups`, which drops the pod's fsGroup supplementary group. If either invariant changes, uid-split loses this protection.
  - **Maintainer decision (2026-10-20):** accept (b) as a documented single-uid residual. No new mechanism.
- **Availability residuals of (b).**
  - **Missing entry.** If `runner-shared` is missing when dind restarts, kubelet creates an empty root-owned directory at that path and binds it.
  - **Divergent mount.** If the entry was swapped or renamed after `runner-init` ran, the directory a restarted dind mounts is not the one `runner-init` validated, and dind and the worker see different trees.
  - **Recovery.** Recreating the pod alone does not repair either case: a kubelet-created root-owned directory can fail the single-uid `runner-init` ownership checks. The operator procedure is:
    1. Locate and preserve the original workspace (the renamed or replaced tree, on the same PVC).
    2. Restore a valid `runner-shared` entry and its ownership, under the existing writer-exclusion checks (no custody-held run writing to it).
    3. Recreate the pod.

    Never delete the replacement directory on the assumption that it is empty: a daemon container may have written into it.

## Consequences and residuals

- Pod replacement (eviction, reschedule, roll, delete) no longer loses clones or uncommitted edits on migrated workers.
- A data-PVC recycle or delete still removes runner contents (D8).
- Single-uid subPath re-resolution (b) and its availability residuals are accepted as recorded above; uid-split is recommended.
- Daemon-owned content can fill a custody-held worker's data PVC (D9); the remedy is an operator procedure, not automation.
- Kubelet eviction, drain and manual pod delete cannot be guarded against the layout migration (D7).
- Rootless dind containers cannot write into a run's checkout through `docker run -v` bind sources by default; that predates #3153 in both modes. Single-uid additionally loses rootless-container writes at the `/data/runner` root and `TMPDIR` root, accepted as a documented regression by owner decision 2026-10-21 (Permissions).
- Retained clones persist; automatic retention deletion stays off (#1858).

## Not verified (live acceptance, maintainer-owned)

- StorageClass cross-node reattach.
- Pod kill on the same node and across nodes.
- `subPath` init on real PVCs.
- Ephemeral-storage usage before and after.
- Eviction ranking.
- Docker-lane disk thresholds.
- Kubelet `subPath` re-resolution behavior (read from source, not run on a live kubelet).
- Rootless-container/UID functional check: whether a real rootless dind container is denied (or allowed) at the `runner-shared` top, the `TMPDIR` root and inside checkouts, in each uid mode. The Permissions analysis is code reading only.
