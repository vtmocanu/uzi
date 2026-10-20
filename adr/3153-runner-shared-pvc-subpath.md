# ADR-3153: Docker-lane `/data/runner` lives on a `runner-shared` subPath of the data PVC

**Status**: Draft (issue #3153; the rootless record below is complete, the remaining sections land with the issue's docs milestone)
**Date**: 2026-10-20
**Deciders**: maintainer ("Go with guards" on the rootless question) and the agent team (issue #3153 milestones and review waves)
**Amends**: Decision 3 of `prds/done/83-docker-capable-worker.md` (dind mounts none of token, data or nix), now "dind mounts only the runner subPath of data"

## Decision (summary)

On Docker-lane hosted workers, the run's working clones and `$TMPDIR` (`/data/runner/.tmp`) live on the `runner-shared` subPath of the worker's `data` PVC, mounted at `/data/runner` in both the worker and the dind sidecar. The `run-workdir` emptyDir is gone, so a pod replacement no longer destroys uncommitted edits or unpublished local commits.

## Rootless merge condition

The issue made "rootless isolation is preserved" a merge condition. The maintainer answered "Go with guards". The record:

- **Mount view.** As rendered, dind's only `data` mount is the `runner-shared` subPath at `/data/runner` (`render_dind.go` `runnerWorkdirMount`, pinned by `TestDindContainersMountOnlyTheRunnerSubPathOfTheDataVolume`). The bare cache, agent home, recovery journals, `/nix` and the token Secret mount are not in dind's mount namespace, and `..` at a bind root does not reach the PVC. This is a mount-view property only. It does not bound a rootful daemon's device-level reach, which `prds/done/89-optional-nonrootless-dind.md` already accepts.
- **Identities.** Under uid-split, every runner and command spawn uses `setpriv ... --init-groups` (`agent/src/runner-uid.ts`), which drops the pod's fsGroup 10001. The image groups give neither runner (10002) nor runner-cmd (10003) gid 10001, and `/data` is `worker:10001 2775`, so neither can create, rename or unlink `/data/runner-shared`. The rootless daemon (uid 1000) sees only the subPath, never its parent.
- **Permissions.** `runner-init` makes `runner-shared` a sticky, worker-owned `3775` directory and `.tmp` a worker-owned directory inside it. That is tighter than the old world-writable emptyDir root, so no container can delete or replace the worker's top-level entries. Unverified functional side effect: a container process that loses the fsGroup supplementary group may no longer create entries at the top of `/data/runner`.
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
