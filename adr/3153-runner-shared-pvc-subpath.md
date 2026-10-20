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
- **Residual (b), single-uid subPath re-resolution.** Kubelet prepares subPath mounts again on every container start, native sidecar restarts included: it resolves the entry with `EvalSymlinks` and accepts any result inside the volume (kubelet v1.31 `pkg/volume/util/subpath/subpath_linux.go`, read from source, not run on a live kubelet). `runner-init` is an ordinary init container and does not rerun when dind restarts. In single-uid mode the worker uid owns `/data`, so agent code could replace `runner-shared` with a symlink to `.` (or rename a real directory such as `repos` to `runner-shared`), and the next dind restart would mount that instead. No new principal gains access: the same uid already reads and writes all of `/data` and drives the daemon. Under uid-split no untrusted identity can make the swap. The maintainer's answer on this residual is recorded below.
- **Availability residual of (b).** If the entry is missing when dind restarts, kubelet creates a fresh root-owned directory, so dind and the worker see different directories until the pod is recreated, when `runner-init` refuses or repairs.

Maintainer decision on residual (b): pending at the time of writing.
