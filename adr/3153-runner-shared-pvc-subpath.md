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

- **Docker lane.** A per-uid scratch directory is adopted or refused, never wiped. On adoption the entrypoint prints "writer exclusion not established". The non-Docker wipe is unchanged.
  - **Runner leaf.** A runner-owned `uzi-runner` is adopted at any mode. The runner can chmod its own leaf, so its mode is no evidence of tampering, and refusing on it would hand the runner a durable outage it could trigger at will.
  - **Worker leaf.** A worker-owned `uzi-worker` is adopted only without group/other write bits (`mode & 022 == 0`). A group- or other-writable worker leaf, such as one left by an fsGroup re-walk to `2770`, fails startup closed.
  - **Refusals.** A wrong owner, a symlink or a non-directory fails startup closed with the contents untouched. An absent leaf is created fresh.
  - **Normalisation is leaf-only.** An adopted leaf not already at `0700` is set to `0700`; nothing beneath it is re-owned or re-moded.
  - **Race safety.** In the reverse-skew layout (an old controller with `TMPDIR=/data/runner`), `uzi-runner` sits directly in the sticky, group-writable runner root, so the runner can rename it and plant a symlink between validation and a path-following `chmod`. Validation therefore records each leaf's identity (owner, device and inode). Every normalisation enters the leaf (`cd -P`), re-checks that identity on `.`, and changes ownership and mode of `.` only, never the path. After reclaiming the leaf to root it re-checks that the leaf name still names that directory. Already-`0700` leaves get the same identity check without any change. A leaf absent at validation that exists later is refused, and a freshly created leaf must be root-owned before it is normalised.
  - **Residuals.** A swap detected after the reclaim leaves the original leaf briefly root-owned, then hands it back with its mode unchanged before refusing. The worker leaf may already be `0700` when a later runner-leaf refusal fires, which is the state a successful boot leaves. A runner swapping its own leaf after boot is out of scope for a boot-time check.
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
- **Migration drain is not admission.** After ordinary admission planning, persistent drifted held workers may receive an existing `image_roll` cordon. No replacement admission, new reservation, canary, roll slot or pod-template patch is authorized under custody; run-bound workers are excluded from drain selection. A distinct live eligible uncordoned same-owner/lane sibling must individually cover the full frozen capabilities (busy is okay; free slots are not required). Full-poll held drains of any reason, even unrenderable ones, and selected/possibly committed drains occupy the migration budget.
- **Accepted availability limits.** Later ordinary rolls may wait behind retained drains; covering sibling loss can remove all intake coverage while the retained drain stays. Reconciliation is level-triggered after registration/restart/explicit undrained polls, with no fixed time bound; registration and poll/write failures permit claims in the gap. Failed writes retain process-local uncertainty until evidence resolves it; restart loses it. A timed-out cordon can commit after an undrained poll, so multiple workers can physically drain. No server-side fence is added: controller-only scope excludes schema, protocol and API product changes. Custody-clear ordinary release can admit fresh work before later re-drain. Worker-pin rollback keeps custody refusal before ordinary release; the existing `image_roll` reason makes this exception deployment-order independent.
- **Operator boundary.** `blocking_reason` and health surface missing coverage, pending/retained drains and multiple-drain intake risk. Unknown-target multiple-drain outcomes keep image/version empty and grant no mutation authority; the existing rolling fallback may stamp the first `upgrading_since`. Idle terminal holds require an operator custody decision (#3031 automatic path is not added); no automatic discard. Chat continue drops the draining-worker pin and may cold-resume. See [operational limits](../docs/hosted-worker-roll-verification.md#migration-drain-policy-and-operational-limits).
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
- Since #3852, automatic pressure recycle preserves Docker `/data` regardless of custody or runner layout: explicit Nix pressure selects Nix only; data pressure without Nix defers. This protection requires the fixed controller; old controllers can still delete data. Ephemeral teardown and manual data-PVC deletion still physically remove runner contents. A DinD-only maintenance recycle keeps `/data/runner` and loses only DinD state.
- Full Docker data now degrades capacity. Existing run park-and-trim and supported manual settled-tree cleanup remain the reclaim paths; D9's custody procedure remains binding for protected sources. PRD #3431 M1b settlement/deletion is implemented behind an off-by-default gate (amendment 2026-11-06 below), so production still grants it no authority unless an operator selects one canary worker (#4134), and #3329's genuine-idle reclaim roll remains deferred. No new reclaim authority is granted; see [ADR-837 D9](0837-worker-disk-lifecycle.md#d9--recycle-defaults-on-over-recorded-reviewer-dissent).

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
- **Go scratch (#3870).** A Claude run's run HOME and Go's internal `$WORK`, now under the run HOME at `.uzi-go-work/g<N>`, are not visible to dind. A container that needs a build artifact must have it staged in the worktree or the shared `TMPDIR`. The new process-ended-park removal of that scratch is not new Docker-data or `runner-shared` reclaim authority; general `TMPDIR` and `DOCKER_HOST` are unchanged. See the [ADR-1809 amendment](1809-per-run-cache-bounds.md#amendment-3870-park-only-go-scratch).

## Not verified (live acceptance, maintainer-owned)

- StorageClass cross-node reattach.
- Pod kill on the same node and across nodes.
- `subPath` init on real PVCs.
- Ephemeral-storage usage before and after.
- Eviction ranking.
- Docker-lane disk thresholds.
- Kubelet `subPath` re-resolution behavior (read from source, not run on a live kubelet).
- Rootless-container/UID functional check: whether a real rootless dind container is denied (or allowed) at the `runner-shared` top, the `TMPDIR` root and inside checkouts, in each uid mode. The Permissions analysis is code reading only.

## Amendment 2026-11-06 — PRD #3431 M1b: gated epoch deletion of `runner-shared` sources

Refs [PRD #3431](../prds/3431-pod-epoch-reclaim.md) M1b. The first-process pod-epoch pass
can now settle and delete a retained attempt clone under `/data/runner` (the `runner-shared`
subPath), but only behind the startup opt-in `UZI_EPOCH_RECLAIM_DELETION` (#4134
replaced the compile-time constant), which the chart leaves off. The D5, D8 and
Consequences statements that automatic retention deletion stays off remain true in
production except on the one hosted Docker worker an operator selects with
`workers.epochReclaim.deletionCanaryWorkerId`. The mechanism is recorded in the
[ADR-1783 amendment of the same date](1783-run-quiescence-and-attempt-clone-paths.md).

- **Scope.** Plain attempt sources in the admitted journal shapes only; protected sources,
  canonical clones and unrecoverable shapes are kept. D9's custody procedure stays binding:
  deletion needs a terminal run, an empty hold list and a run-wide `run_open_holds` count of
  exactly 0, and the first roll onto an image advertising that capability keeps everything.
- **Bounded proof work.** R9 limits (owner answer seq 1162): metadata 16 MiB, worktree
  content 512 MiB, object store 4 GiB per snapshot (logical work, not a memory ceiling),
  records 131,072, directory entries 16,384, `maxBares` 1,024 and `maxSeenPaths` 4,096
  unchanged, delta base cache 96 MiB. A content-stage overflow keeps the source; an
  indirection overflow is UNKNOWN.
- **Durability.** Process crash only. No storage or power-loss durability is claimed: the
  tree removal's parent directory under `runner-shared` is not fsynced.
- **Not verified.** No hosted run of the deletion phase on a real `runner-shared` PVC; it is
  maintainer-owned live acceptance, like the list above.

## Amendment 2026-11-10 — issue #4043: gated runner-`TMPDIR` scratch reclaim

Historical activation decision: the constant-off gate and pre-activation owner
decision below were superseded by #4182 on 2026-11-11. The mechanism protections
and residuals remain; hosted acceptance is still pending.

Issue #4043 adds a scratch-only reclaim phase to the [PRD #3431](../prds/3431-pod-epoch-reclaim.md)
pod-epoch pass. It narrows D4 ("no new cleanup authority") and D5 ("automatic retention deletion
stays off") for the runner-`TMPDIR` scratch root only; clones, `.retire-*` leftovers, retained
sources and every other path under `runner-shared` are unchanged, and D9's custody procedure stays
binding. The amendment of 2026-11-06 above is unaffected.

- **Gate.** `EPOCH_SCRATCH_DELETION_ENABLED` (`agent/src/pod-epoch-reclaim.ts`) is `false` and
  independent of the attempt-deletion activation (`UZI_EPOCH_RECLAIM_DELETION`, the #4134 one-worker
  canary): neither gate opens the other's phase. While it
  is off, startup runs no census, starts no new child and reads no new path. Activation needs a
  separate owner decision recorded on issue #4043; until then the D4, D5, D8 and Consequences
  statements remain true in production.
- **Authority.** Only inside the existing pass, under held EX, at a fresh shadow verdict
  `eligible` with authority `attested`, with the pass intact (not invalidated or aborted). The
  phase runs before the attempt-deletion phase and shares its reclaim scope. Process scans, daemon
  listings, mount tables and ctimes are exclusion proofs only: each can keep a leaf, none can admit
  one.
- **Refusals (whole phase, fixed reason, attempt phase continues).** Anything but the exact
  layouts (uid-split root `/data/runner/.tmp/uzi-runner`, runner-owned `0700`; single-uid root
  `/data/runner/.tmp`); a root that cannot be pinned component by component or has the wrong
  owner, mode or identity; an unexpected mount above or inside the root, or mount state that cannot be read or
  reconciled; a filesystem other than
  ext4 or XFS; an unreadable Docker bind-source inventory, any bind source outside `/data/runner` or
  through a symlink, or a bind source at or above the root; unreadable or malformed protected recovery records; an invalid pod-entry record; a
  failed exclusive-idle proof, an exempt process not running as the worker uid/gid, or unreadable
  process descriptors; a census that fails, is malformed, reports another root identity or would
  overrun the pass deadline. Later in the loop, a failed mount, census, idle-proof or
  process-holder re-read stops the phase the same way. The exceptions end the whole pass, so the
  attempt phase does not run: a daemon-inventory re-read that changes or cannot be read, a
  pod-entry change, a pass abort or stop, and a refused pre-launch observation or launch.
- **Never removed.** The root itself; `uzi-worker` and `uzi-runner`; dot-names and other refused
  names; non-directories; leaves owned by another uid or containing foreign-owned entries; mounted
  or other-filesystem leaves; leaves a Docker bind source or a protected recovery record
  references; leaves a visible process holds open or uses as its cwd; `uzi-quiesce-*` helper leaves; and
  any leaf whose newest subtree ctime is at or after the pod-entry fence (record ctime minus 2 s);
  and any leaf whose census was incomplete (a cap, the deadline or an unreadable entry). At most 512 leaves are admitted per pass, in name order.
- **Deletion.** Top-level leaves are censused read-only as the runner uid, then deleted one at a
  time. Before each delete the mounts, daemon inventory, leaf census, idle proof and process
  holders are re-read; a changed leaf is kept and a failed re-read stops the phase or ends the
  pass as listed above. The strict pinned helper
  stays on one filesystem, re-pins every descendant and prints a removal receipt only after its own
  final `rmdir` succeeded and the retained leaf pin reports `nlink` 0.
- **Settlement.** Each leaf settles as removed or residue before the next starts. A residue whose
  leaf changed or whose helper failed stops the phase at once; otherwise three retained leaves stop
  it. A delete that cannot be settled (or a pending child or stop latch) is
  fatal: EX is kept, nothing is admitted and the process exits non-zero, as for M1b.
- **Bytes.** Reported figures are allocated-block estimates from the census (hard-linked bytes
  counted separately), not proof of freed storage.
- **Costs (bounded work, separate from the byte estimates).** The census reads at most 4096
  top-level entries, 200,000 entries per leaf and 2,000,000 in total, to depth 256, within the pass
  budget minus the 5 s settlement reserve and a 10 s margin; each per-leaf re-census is capped at 30 s; each delete helper at
  2,000,000 entries, depth 256 and 300 s; the whole phase stays inside the existing 600 s pass
  with its 5 s settlement reserve. With the gate off none of this runs. A bounded synthetic
  measurement on one development host (2026-11-10, ext-family filesystem, single uid, warm cache;
  wall time, not reclaimed bytes) took about 0.1 s to census 1,050 entries, 0.35 s for 20,250 and
  1.1 s for 100,500 (100 leaves), and under 0.1 s for the strict helper to remove one 1,005-entry
  leaf with its receipt. Hosted volumes will differ; these are not acceptance figures. The real-UID image lane's
  runtime change from the new split-mode leaf is unmeasured (that lane could not run in-run).
- **Residuals, not accepted until activation.** A historical-pod writer (the #3431 residual) can
  still write inside the revalidation windows between the last re-check and the helper's pin; a
  transient uid switch between scans is not observed; a clock step weakens only the ctime fence
  (the other exclusions stand); the byte figure is an estimate. Activation requires the owner to
  accept these.
- **Not verified.** No hosted before/after-byte acceptance on a real `runner-shared` PVC; that is
  owner-owned, like the list above.

## Amendment 2026-11-11 — issue #4182: default-on scratch startup request

Replace the historical constant-off scratch gate with worker startup
`UZI_EPOCH_SCRATCH_DELETION`: unset, empty or `0` is off; `1` requires
`UZI_EPOCH_LOCK=1` and Docker wiring; other values refuse boot. It is read once
before pod-epoch creation. Chart `workers.epochReclaim.scratchDeletion` is a
strict boolean, default `true`, rendered as quoted controller env
`UZI_WORKER_EPOCH_SCRATCH_DELETION="true"` or `"false"` with false preserved.
The controller accepts unset/empty/`false` as off, only `true` as on, and refuses
other values; it emits worker literal `1` only when enabled, `withDinD(w)` and
not ephemeral, omitting it otherwise. Attempt deletion remains independent,
selector-gated and off by default.

This changes activation policy, not exclusion authority, mechanism protections
or residuals above. Runtime declaration stays empty by default; an undeclared
or unattested pass keeps scratch. A startup request is not deletion evidence:
`pod_epoch_reclaim_activation` logs boolean `deletion_requested` and
`scratch_requested`. Older controllers or absent env leave scratch off; older
workers ignore the env. The image must include #4171 (`7cd46526`, merged).

Disable via false, controller reconciliation, desired Deployment template env
omission and safe paced replacement with busy/custody guards, keeping ForceRoll
off. Confirm the new pod/image/epoch, `scratch_requested: false` and actual
scratch result. Old-template restarts/evictions can still carry `1`; this cannot
stop an active pass or restore bytes. See the [operational procedure](../docs/run-recovery.md#runner-tmpdir-scratch-reclaim-startup-disable-and-observation).
Post-release owner observation, hosted acceptance and the keep-default decision
remain pending on #4043, without a pre-enable canary/approval gate. Record image,
pod, epoch, requested state and actual deleted/kept reasons or fatal result;
keep allocated-byte estimates, unattributed statfs delta and independent physical
reclaim separate. No hosted evidence is claimed by this amendment.

## Amendment 2026-11-13 — issue #4143: `.retire-*` scratch outcome records (observation only)

Worker startup now observes every `.retire-*` retirement scratch parent the
bounded root listing reaches under `<dataDir>/runner` (4096-entry budget;
`enumeration_incomplete` marks a truncated listing) and emits bounded
per-parent `runner retire scratch outcome` rows (at most 10, sorted, parents
only) and exactly one `runner retire scratch total` line per startup (one per
pod epoch on epoch workers) whenever records are on and the runner root exists
and was listed, including with zero parents. A missing, non-directory or
symlink runner root logs none; if the step throws,
`runner retire scratch outcomes failed` replaces the total. With zero parents
`retained_source_read` is `not_attempted` (no journal read or walk) and
`bytes_state` is `measured` unless the listing was incomplete (`unknown`).
Every parent is kept (`reclaimed` is always 0); nothing is deleted, chowned or
moved, and the existing `runner retire scratch retained` warning is unchanged.
A parent is `protected_by_retained_source` when a decoded recovery or restore
journal's `clonePath` (predecessor, successor or tombstone) or
`retainedSources[].clonePath` names it or a path under it; otherwise
`no_disposition_evidence`. Bytes are allocated bytes from a
read-only descriptor-relative walk (hard links once, no symlink following);
`bytes_state` is `measured` only when every parent was measured completely and
the listing was complete. One 30 s startup budget covers the journal read and
the walk; an uninterruptible syscall has no hard real-time bound. Synthetic
local-disk cost: median 970 ms (max 1639) for 10 parents of 1000 small files
plus 3 bares, against 13 ms off; not a production measurement.

Switches: worker `UZI_RETIRE_SCRATCH_OUTCOMES` (unset, empty or `1` on; `0`
off; other values refuse startup), controller
`UZI_WORKER_RETIRE_SCRATCH_OUTCOMES` (empty or `true` on; `false` off; other
values a boot error) and strict-boolean chart
`workers.epochReclaim.retireScratchOutcomes` (default `true`; `false` renders
worker `UZI_RETIRE_SCRATCH_OUTCOMES=0` on every worker). Default renders are
unchanged. Off restores the count-only inspector exactly.

No reclaim, deliberately: no ordinary durable disposition binding proves the
moved footprint (rollback repair can persist a protected moved-clone pointer);
the rename into the scratch parent precedes the journal clear and the
retired-ledger write, and the ledger names only the canonical clone path; a
retired ledger, emptiness, age or the parent name grant nothing. The legacy
stderr-proof repair (#4141, landed via #4152) does not certify remnants created
by older code retroactively, and #4137 is a distinct residual. Reclaim remains
open on #4143 pending a durable parent-binding design (superseded in part by the
2026-11-17 amendment below).

## Amendment 2026-11-17 — issue #4263: durable `.retire-*` parent bindings and verified-epoch reclaim

This amendment narrows D4 and D5 ("no new cleanup authority") and the 2026-11-13
statement that reclaim stays open: a gated, default-on exception now lets a
verified pod epoch remove a **bound** `.retire-*` parent. Unbound (legacy,
attempt-less) parents stay kept exactly as before.

**Owner amendment of the requirement.** Issue #4263 asked for the binding
"inside an existing record (the recovery journal or the attempt ledger)". The
owner amended that to **"a scoped sibling record (`uzi-retire-scratch.<uuid>.binding`)
in the existing bare-config store"**. That is the same store, not the same
record: neither the journal nor the ledger gained a field.

### The binding

- **Key and value.** Key `uzi-retire-scratch.<uuid>.binding` in the retiring
  attempt's bare config; `<uuid>` is the parent's UUID. Value is canonical JSON
  in this field order: `v` (always 1), `class`, `attemptId`, `runId`, `branch`,
  `clonePath`, `phase`, `parentIno`, `cloneIno`, `pod`. Inodes are canonical
  decimals; no device number is persisted (device equality is checked only
  in-pass, against the pinned runner root). The value is never empty or valueless.
- **Phases.** `intent` (written before the clone moves; grants nothing),
  `settled` (every proof held; the parent is owed to a verified epoch) and
  `removing` (written durably before the first byte of the parent is deleted).
- **Strict codec.** A reader accepts only a value that re-serializes to itself
  (known class and phase, valid attempt id and run id, absolute normalized
  `clonePath`, key uuid valid, canonical inodes, bounded length, no control or
  bidi characters). Anything else, a duplicated key or a valueless key is
  `malformed_binding` and keeps the parent.
- **Cap.** At most 64 binding keys per bare, counting every entry under the
  prefix whether valid or not. At the cap a new retire writes no binding: its
  parent is unbound and kept, exactly today's behaviour, and the retire logs it.
  A read that finds more than 64 is `unknown`, never a truncated list, and keeps
  everything. The cap also bounds the added records against the epoch metadata
  caps and the ACK reader's 1024-entry config cap.

### Who binds, and which classes can settle

Every EXDEV retire that carries an `attemptId` and a disposition class binds its
new parent (`retireRunnerClone` takes the class from its caller in
`agent/src/runner.ts`). Only two classes can ever become `settled`:

| Class | Binds | Eligible | Reason |
|---|---|---|---|
| `owner_terminal` | yes | yes | The run's own terminal disposition, which today's code already carries out in-process. |
| `verified_successor` | yes | yes | A server-acknowledged covering FINAL, which today's code already carries out in-process. |
| `owner_terminal_covered` | yes | never | `preserveRecoveryClone` was lifted by the stopped-source covered verdict, whose proof (`worktreeCleanHardened`) can still miss ignored paths and runner-unreadable directories that Git silently omits (#4141 fixed only the stderr case; #4137 stays open). Reads `class_ineligible`. |
| `completion` | yes | never | Completion keeps the outer bare lock and raw-removes in-process; its binding writes `removing` before that removal and never settles. Reads `class_ineligible`. |
| `other` (foreign-orphan) | yes | never | No owner-terminal or acknowledged-FINAL authority backs it. Reads `class_ineligible`. |
| attempt-less retire | no | never | The owner's cross-check against the attempt-ledger entry (attemptId, runId, clonePath) cannot be satisfied without an attemptId; the parent stays unbound and kept. |

On a uid-split pod a bound `owner_terminal` or `verified_successor` retire
settles only when every proof holds: the rename was verified (inode on the
parent's device) and its directories synced, the journal clear and `retired`
ledger append were synced, the re-read journal no longer names the clone,
physical terminal protection is false (re-read last and again right before the
write), no quarantine or process revocation arrived, and any process proof still
holds. Otherwise the binding stays `intent`. This adds no new disposition
authority: the binding makes the same authority recoverable after a failed or
crashed attempt and adds the epoch exclusion.

### Write and sync ordering, failure model

- Each write is git's atomic `config.lock` rename, followed by an fsync of the
  config file and of the bare directory, and a required byte-identical read-back,
  before the next dependent effect. Bindings are durable across process crash and
  across storage loss on ext4 and XFS.
- Order: create and pin the parent, sync the runner root, record the clone inode,
  write `intent`, rename, verify the moved clone through the parent pin, sync the
  source directory and the parent, clear the journal and append `retired`, sync,
  then `settled`. A directory that holds an effect is synced before the binding
  transition that depends on it; the runner root is synced after the parent is
  removed and before the binding is unset.
- Effects that never gate a transition carry no durability claim: the `mkdir`
  before `intent`, and **partial removal under `removing`**. Losing them only
  makes an identity check fail, which keeps.
- Failure before the move (an intent write or sync failure) removes the empty
  parent and fails the retire closed, leaving the clone and journal untouched.
  This is a stated behaviour change. A failure in a later metadata step leaves the
  binding where it was (harmless `intent`).
- **Interrupted removal is kept permanently** (owner decision). A binding at
  `removing` is never retried, resumed or retired automatically; it reads
  `interrupted_removal`.
- **Split pod versus single uid.** On a split pod the worker no longer removes a
  bound `owner_terminal` or `verified_successor` parent in-process; it leaves the
  parent for the verified epoch. On a single-uid pod nothing changes: the walker
  and the writer share the uid, so the binding goes straight to `removing` and the
  existing in-process `rm` runs, then the binding is unset after the runner root
  is synced. Unbound parents keep today's path-based removal. **#2349 site 2
  (holding/scratch disposal in `retireRunnerClone`) therefore stays open**; see
  ADR-1809.

### The verified-epoch phase

The retire-scratch phase runs **last** in the verified pod-epoch pass, after the
scratch phase and the attempt-deletion phase, under the same shadow gate (an
`eligible` verdict, `attested` authority and an intact pass) plus its own switch.
The binding is never activation authority; the phase does not alter the shadow
verdict or reason and reports only a `retire_scratch` block and
`retire_scratch_phase_ms` on the `pod_epoch_reclaim` line.

- **Whole-phase refusals** keep every parent and record one fixed token:
  `split_inactive` (single-uid pods never run it), the pod-entry fence, the
  runner-root pin or posture (`root_posture`), a mount at or under the runner root
  (`mount_unexpected`) or an unreadable mount table (`mount_unknown`), a
  filesystem other than ext4 or XFS (`fs_unsupported`), an unreadable or
  at-or-above bind inventory (`bind_unknown`), a failed idle proof (`not_idle`),
  unreadable evidence (`evidence_unknown`, `protected_unknown`), an incomplete
  root listing over 4096 entries (`list_incomplete`), an unreadable process scan,
  and a budget too short to start (`budget_deferred`). The same conditions
  re-checked mid-phase halt it with that token.
- **Budget.** The phase needs 30 s of admission headroom, is capped at 120 s and
  leaves a 10 s margin before the pass settlement reserve. No new candidate starts
  with under 20 s left; at most 64 candidates and 2,000,000 helper entries per
  pass. A deferral (`budget_deferred`, `epoch_not_eligible`) never changes the
  verdict, is counted separately from `kept` and leaves the binding untouched.
  The helper gets what the phase window holds, never more than 300 s.
- **Per-candidate exclusion.** Each candidate is re-proven under the existing
  A, S, B order: the authority transaction (A), the run's source reservation (S),
  and the parent bare's lock (B). Inside B the binding, ledger, journal, restore
  holder, every other bare's protected paths, custody, quarantine and canonical
  footprint are read again; the source-use check is read-only (a held lease from
  another sink, or an active slot, keeps with `source_use_held`; nothing is
  admitted or taken over); the parent must be a worker-owned 0700 directory with
  the bound inode on the root's device, holding only `clone`, older than the
  predecessor's pod-entry fence; mounts, binds, the daemon inventory, the idle
  proof and a fresh process scan are re-read for that parent. The run must be
  terminal with zero server-side holds.
- **Alias-aware protection.** Retained-source protection treats
  `/data/runner-shared/...` as `/data/runner/...` at the runner root, so a
  journal or retained source written through either spelling protects the parent.
- **Removal route.** The existing strict pinned helper script gains one argument
  shape, `empty-fd`. The worker pins `<parent>/clone` (`O_PATH`, no symlink
  follow, directory) and passes it as child fd 3; the pass's exclusive lock is fd
  4. The helper runs **as the runner uid** through the one `runnerCommand`
  wrapper, never the command-root wrapper and never a worker, retry or chmod
  step, and it refuses to launch unless the uid split is active. It re-checks
  fd 3's identity, empties it under the same strict rules as `remove` (one
  filesystem, descendant re-pin, symlinks unlinked and never followed, entry,
  depth and time caps) and never removes the directory itself. **No chmod, no
  chown, no privilege increase and no raw fallback.** Its receipt is not
  emptiness proof; the worker's own `rmdir` of `clone`, then of the parent (whose
  pin must then report zero links), is. Root posture (worker-owned and sticky) and
  the root's mount id are checked on the parent and clone pins before launch.
  Helper stderr is never logged; only its bounded length is. Any non-receipt,
  `rmdir` failure, identity change or sync failure is residue.
- **`removing` order.** The `removing` phase is written by the last pre-effect
  callback, after every identity, posture and budget refusal. A refusal before it
  keeps `settled`; a failure after it keeps `removing`. After a verified removal
  the runner root is synced and the binding is unset (`unset_failed` counts a
  failed unset; a later pass recovers it).
- **Recovery route (parent gone).** A binding whose parent is already absent is
  resolved by metadata only, under the same A, S, B: case 1, the ledger's last
  canonical value is the matching `retired` entry, the canonical clone is absent
  with no footprint and nothing names it; case 2, an `intent` binding whose
  canonical clone is still present at the bound inode and is still named by its
  journal (the clone never moved). The binding is unset after the relevant
  directory syncs. Anything else is `metadata_unresolved` and the binding stays.

### Switch

Worker `UZI_RETIRE_SCRATCH_RECLAIM` (unset, empty or `1` on; `0` off; other
values refuse startup), controller `UZI_WORKER_RETIRE_SCRATCH_RECLAIM` (empty or
`true` on; `false` off; other values a boot error) and strict-boolean chart
`workers.epochReclaim.retireScratchReclaim` (default `true`; `false` renders
worker `UZI_RETIRE_SCRATCH_RECLAIM=0` on every worker). Default renders are
unchanged. Off means the pass reads no binding, removes nothing and unsets
nothing (`retire_scratch.reason` is `disabled`); retire-path binding writes
continue, as metadata only. The switch permits the phase and proves nothing.

### Outcome records

The startup `.retire-*` inspector now runs **after** the verified pass (still
before the claim loop) and reports what the pass did. Rows carry `reclaimed` as
an outcome for parents the pass removed and a binding-aware `reason` for kept
ones: the pass's own token for a parent it examined, otherwise `bound_intent`,
`interrupted_removal`, `malformed_binding`, `disabled`, `epoch_not_eligible` or
`budget_deferred` for a bound parent, and the #4143 reasons for an unbound one.
The total line gains `reclaimed`, `reclaimed_bytes`, `kept_by_reason` and
`removing_bindings`.

### Readers of the bare config (rollback safety)

Old code has no reader for the key and never deletes `.retire-*`, so it cannot
create authority; authority needs a binding written by new code, the ledger
cross-check and every in-pass check. Reader-by-reader decisions (abbreviated;
`agent/test/retire-binding-old-readers.test.ts` pins the unchanged ones against raw config keys):

| Reader | Decision |
|---|---|
| `collectEpochScratchProtectedPaths` | No change. Absolute paths in the JSON value join the protected set (outside the TMPDIR scratch root, harmless); a valueless `uzi-*` key reads `unknown`, so the binding is never valueless. |
| Epoch census, maintenance and recovery families, FINAL inventory, terminal attribution, restore census and restore-route | No change. They match exact key families and ignore the prefix; none lists the runner root. |
| ACK reader and runGit config accounting | Entries count toward their caps (1024 entries); the 64-per-bare cap bounds the addition. |
| Startup inspector | Changed: reads bindings and reports binding-aware reasons. |
| Attempt-ledger compaction | Changed: an attempt named by a binding keeps its `retired` entry; an unreadable or undecodable binding set keeps every entry. |
| Hibernation census (filesystem walk of the runner root) | No change. A bound parent reports `unknown`, which blocks volume retirement conservatively (a residual below). |
| `backup-runs.sh` config reads | No change. They match exact keys; a retired, discarded clone is not backup material. |

Rollback to an older worker image: old compaction dropping a `retired` entry
reads `ledger_missing`, a re-seeded attempt id `ledger_mismatch`, a journal or
retained source naming the parent `protected_by_retained_source`, a moved clone
or parent `identity_changed`; an old-image retire writes no binding, so its
parent is unbound and kept.

### Writers that participate

| Writer | Participation |
|---|---|
| EXDEV retire in `retireRunnerClone` (owner terminal, verified successor, completion, foreign-orphan) | Writes `intent`, then `settled` (two eligible classes, split pod only) or `removing`; unsets after a confirmed removal. |
| Completion disposal | Writes `removing` before its raw `rm`; never settles; unsets only after a successful full disposal. |
| Single-uid in-process removal | Writes `removing` before the `rm`; unsets after the runner root is synced; failure keeps `removing`. |
| Verified-epoch phase | Writes `removing` (once, under B) and unsets; also unsets in the recovery route. |
| Attempt-ledger compaction | Reads bindings to keep named entries. |

### D1 outcome (design review before the removal route)

An architect, auditor and reviewer examined two judgments and found no stop.

- **Judgment A.** The runner unlinking runner-cmd-owned files through
  group-writable directories is not "runner-cmd disposal" in the sense of #4137 and
  #4143: the helper runs only as the runner uid and never chmods, and the runner
  already had that reach (the worker had it before this change, and the canonical
  clone path is already freed by deleting its content as the runner uid).
- **Judgment B.** Yes, the two eligible classes' disposition can rest on
  runner-uid git status and capture proofs (the restore-point transfer, the
  credential-free clean-head check and the FINAL capture all run as the runner
  uid), but only under a **visibility invariant**: content that #4137's residual
  hides lives in directories the runner cannot open; `empty-fd` runs with the same
  uid and groups as the capture git, so it cannot list or unlink that content
  either; the worker's step is only `rmdir`, which fails on a non-empty directory
  and leaves residue (the binding stays `removing`, the bytes survive). The
  invariant holds only while there is a single runner pass, no chmod by helper or
  worker, identity and groups equal to the capture git identity, and no runner-cmd
  process alive (the process scan refuses non-worker uids). Any second identity or
  a chmod makes both classes ineligible.
- **Narrowings adopted.** The launch wrapper list is exactly `runnerCommand`
  (mutation-pinned); a per-candidate mount, bind and filesystem gate re-read at
  each of A, S and B; residual fixtures with runner-cmd content inside tracked
  directories; helper stderr never logged; empty-fd refused when the split is
  inactive; the receipt is never emptiness proof; deferral applies only to
  bound non-completion discard parents. Rejected alternative: opening the parent
  group write and search for a strict `remove`, which would let the agent uid
  plant entries in a worker-private parent.

### Residuals

- **Historical-pod writer.** A worker image without this change writes no
  binding; its parents stay unbound and kept.
- **Clone substitution before `retire()` starts.** The clone directory is pinned
  at the start of `retire()`; a substitution of `clonePath` before that point is
  runner-controlled content in the runner-writable repo directory. It gains no
  privilege and the deletion scope equals today's.
- **Hibernation census** reports a bound parent as `unknown`, which blocks volume
  retirement conservatively.
- **Availability cost of `removing`.** A `removing` binding is never reclaimed,
  and the total line counts them (`removing_bindings`). This includes the case
  where liveness or the reservation is lost right after the `removing` write
  while the parent is still intact: it stays at `removing` and kept, although
  nothing was deleted. One stray binding entry blocks attempt-ledger compaction
  for that bare (an unreadable or undecodable binding set keeps every entry).
- **Partial removal** carries no durability claim, and the parent and its
  remaining bytes stay.
- **FINAL capture proofs run as the runner uid**; see Judgment B.
- Legacy parents, attempt-less retires, the `owner_terminal_covered` class and
  resume of an interrupted removal are out of scope.

### Not verified

- Hosted rollout observation of the first default-on epochs and the owner's
  keep-default decision are not claimed here and stay on #4143.
- Real worker-UID lane results (ownership, symlink, partial-removal and
  parent-death cases that need a root-owned runner root or a real second uid) come
  from exact-head CI `test-agent-worker-uid` unless reported otherwise; this
  amendment does not claim them.
