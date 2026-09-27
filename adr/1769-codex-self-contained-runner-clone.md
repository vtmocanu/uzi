# ADR-1769: the Codex runner clone is dissociated from the worker bare, not granted read access to it

**Status**: Accepted and implemented; Landlock acceptance recorded below (issue #1769)
**Date**: 2026-09-26
**Deciders**: architect (design), coder (implementation), reviewer.
**Related**: issue #1769, PRD #51 (uid split), adr/1598-codex-command-storage.md.

## Context: the evidence

Every run's runner clone is seeded with `git clone --shared` from the
worker's bare repository: the clone borrows the bare's objects via
`objects/info/alternates` instead of copying them, which is cheap and, for
Claude runs, safe (see `docs/proc-hardening.md`, "Shared-git
write→worker-execute").

Codex runs are different: model-authorized commands run inside a per-command
Landlock sandbox (`agent/codex/supervisor/cmdsandbox/main.go`, `addRules`)
whose allowlist grants system directories, `/dev`, and the run root (the
clone) — never the bare the clone was seeded from. A `git` invocation inside
that sandbox that needs an object the clone only *borrows* fails with
`unable to open object pack directory … Permission denied`, because the
alternates path point outside the sandbox's grant. A checkout never copies
borrowed objects into the clone: a `--shared --no-checkout` clone followed by
a checkout has zero local objects, so every git command in the sandbox that
reads history or the object store (status, diff, log, commit) fails this way,
not only an operation that reaches an older commit.

## Decision

### At seed: materialize the clone before Codex ever starts

For executors that run model-authorized commands inside that sandbox
(`Executor.sandboxesCommands`, set by `CodexExecutor`; `executor.safety` is
only assigned inside `CodexExecutor.run()` and so is always undefined at seed
time, which is why the flag is a construction-time property, not read off
`safety`), `GitCache.runnerCloneForBranch` performs the `--shared` clone as
before and then, still inside the bare's lock and after every ref/checkpoint
setup step has picked its SHAs:

1. **Anchors every SHA the seed used** under temporary `refs/uzi-materialize/*`
   refs: HEAD, base, the marker parent, the cherry-picked checkpoint marker,
   the default-branch commit, the ratchet base, and the bare's own
   `origin/<branch>`. This is what makes the following repack safe: nothing
   the seed touched can be garbage-collected mid-operation.
2. **Repacks as a copy**, `git repack -a -d`, run as the runner uid — never
   hardlinked, so the clone's objects are genuinely its own from this point.
3. **Verifies with alternates disabled**: parks `objects/info/alternates`
   aside first, then runs `git fsck --connectivity-only` plus a `cat-file -e`
   per anchored SHA, in an environment that strips
   `GIT_ALTERNATE_OBJECT_DIRECTORIES`/`GIT_OBJECT_DIRECTORY` so the check
   cannot silently succeed by still reading the bare.
4. **Only then deletes the alternates file.** Any failure in steps 2 or 3
   restores the alternates file unchanged and fails the run before Codex
   starts, typed `RunnerCloneMaterializationError`, rather than leaving a
   half-dissociated clone that might or might not work depending on which
   object a later command happens to need. The probe that decides whether
   alternates is even present treats only `ENOENT` as "absent"; any other
   probe error fails closed into the same materialization path rather than
   silently skipping it.

Claude runs are unchanged: still a plain `--shared` clone, since their
commands do not run inside the Codex command sandbox.

### At finalize: stream the fresh default tip in as a copy, not a borrow

Finalize's base-align step needs the bare's current default-branch tip,
which can arrive in the bare after the seed already ran and already dropped
alternates. `GitCache.ensureRunnerCloneObjects` closes that gap the same way:
a non-thin, credential-free `git pack-objects --revs --stdout` produced by
the worker (reading its own bare) is streamed into `git index-pack --stdin`
run inside the clone, by the command identity, inside the Codex finalize
boundary — so the new objects land as a copy in the clone the same way the
seed's repack did, and the producer side never needs sandbox access to
anything. The producer (`worker_pat`) is spawned first; the existing guards
are unchanged under this ordering (worker-PAT admission in
`codex/safety.ts` already refuses while a model `command` root is live, and
the consumer registers as a `boundary_action` root of the finalize
boundary). The two sides are not the same kind of operation: the seed-time
materialization (`materializeRunnerClone`) refuses to run inside a
permit-held boundary at all and runs as plain runner-uid git
(`execScoped`), registering no boundary root, while the finalize import runs
inside the boundary under its permit.

An import failure fails the run with `fail_origin:
finalize_base_align_conflict`, with a reason that names the import as what
failed (never "merge" or "rebase" — those are a different failure shape on
the same typed origin). The diff is preserved when the failing state still
scans clean for secrets, and withheld otherwise, matching how every other
`finalize_base_align_conflict` path already behaves.

### Reconciliation with #1804

This branch merged origin/main at `e078ba7916c809ebac7618f157ff5a9906eec04b`,
which brought #1804's `exitGatedStream` into `agent/src/git.ts`. Both changes
wrapped `spawnGit`'s stdout in a `PassThrough` for overlapping reasons, so
the branch's `callerOwnedStdout` was deleted and both `spawnGit` paths (the
supervised `boundary.spawn` and the plain `spawn`) now return
`exitGatedStream`'s stream. That one mechanism serves the checkpoint pack
upload and the finalize import alike: it pipes the child's stdout into the
returned stream at once (Node's child_process `flushStdio` resumes an unread
stdout when the child exits, so a reader that starts later would see only
EOF), ends it only after a clean exit, destroys it with an error carrying
git's stderr on a nonzero exit (and with the error itself on a spawn error or
a rejected completion), and tears the producer down when a consumer abandons
it (outside a boundary it also kills a still-live child). It also errors the
returned stream when the source closes without `end` or `error` (a premature
close), the guarantee `callerOwnedStdout` had from `stream.pipeline`: without
it a clean exit would wait for an `end` that never comes and the import's
pipe would never settle. The boundary tests "inside a boundary: a source that
closes before its end errors the returned stream" and "inside a boundary: a
source that closes before its end fails the import instead of hanging" pin
it.

Two parts of the branch's design stay. `spawnGit` still returns an `exited`
promise that never rejects: inside a boundary it settles only after the
supervisor root has reaped, and outside one on `close` (or -1 on a spawn
error). And `ensureRunnerCloneObjects` still waits for the pipe, the
producer's `exited` and the consumer's `exited` before it returns, and still
tears the peer down when one side fails. Stream completion is not evidence
that a process, or its supervisor root, is gone, and the finalize boundary
must not close with a root of the import still live.

Tests that pin it on both paths are in `agent/test/git-import.test.ts`:
a late reader receives every pack byte (a producer that exits before the
consumer starts, and a multi-MiB pack whose consumer starts only once the
producer blocked), a late reader still sees a nonzero exit, and the
stalled-peer teardown cases. Main's
`agent/test/git-spawn-stream-failure.test.ts` pins the checkpoint upload's
side of the same contract, unchanged. Mutations run against the merged tree:
piping lazily turns the late-reader byte cases red on both paths; ending
the stream cleanly on a failed exit turns the late-reader failure cases red
on both paths, along with main's stream-failure tests; removing
`ensureRunnerCloneObjects`' teardown turns the "consumer cannot start" cases
red on both paths, plus the stalled-before-first-write and SIGKILL cases.
The "consumer exits at once" cases go red only when `exitGatedStream`'s
abandon teardown is removed as well, because a broken pipe also reaches the
producer through that teardown. The boundary case where a producer fails
fast and the consumer is stalled on stdin stays green under both, because
`pipeline` destroying the consumer's stdin does the same thing the abort
does inside a boundary.

## Rejected alternative: grant the bare read access instead of dissociating

Adding a read-only Landlock rule for the bare's objects directory, so the
clone could keep borrowing rather than copying, was considered and rejected:

- **It would expose every object in that store**, not just the ones this
  run's history reaches — the bare accumulates objects from every run that
  has ever cloned from it, so a Codex command in run A could read blobs from
  run B's unrelated work sitting in the same bare.
- **The path would have to come from the runner-editable alternates file
  itself**, which is not a safe source for a sandbox grant: the file lives
  inside the clone the runner controls, so a rule derived from its contents
  is a rule the thing being sandboxed can influence.

Dissociation avoids both problems by construction: once materialized, the
clone has no reference to the bare at all, so there is nothing left for a
sandbox rule to leak.

## Consequences

- **Disk cost.** Each Codex clone becomes a full object copy on disk instead
  of a cheap `--shared` borrow. The copy includes everything reachable from
  the clone's refs, which includes the bare's frozen mirror of every branch
  copied at first clone — so a large blob living on a completely unrelated
  branch lands in every Codex clone that bare seeds, not just the branch the
  run is working on. See `docs/configuration.md`'s parked-run disk-sizing
  section, which now calls this out explicitly.
- **Lock contention.** The repack runs under the bare's lock, so it briefly
  serializes any sibling claim trying to seed from, or write to, the same
  bare while a Codex materialization is in flight. This is bounded by the
  repack's own duration, not open-ended.
- **Fail-closed, not fail-open.** A materialization failure — a repack error,
  a failed fsck, an unreadable anchored SHA, or a probe error deciding
  whether alternates is present — always fails the run before Codex starts.
  There is no degraded mode where Codex runs anyway against a
  possibly-borrowing clone.
- **Process ordering under unchanged guards.** The finalize-time import
  relies on the worker-PAT admission guard and the `boundary_action` root
  registration already in place for other finalize operations; this issue
  does not change either guard, only adds a new consumer that must run after
  the producer under them. The seed-time materialization is outside both: it
  runs before any boundary, as the runner uid, and registers no root.
- **Unchanged.** The worker's fetch-back of the agent branch (`file://`+pack,
  the CVE-2022-39253-class boundary already documented in
  `docs/proc-hardening.md`) and the command git-trust restrictions are not
  touched by this issue.

## Verification pointers

`task test:codex-git-trust` (opt-in, Docker) now includes a `--shared`-clone
case exercising this path; see that recipe for what it currently covers.
Unit coverage for the seed-time path lives in
`agent/test/git-materialize.test.ts` and
`agent/test/runner-clone-self-contained.test.ts` (including the fail-closed
case: a materialization failure fails the run before the executor runs);
the finalize import, and the caller-owned producer stream it reads, are
covered on both spawn paths (plain and boundary) in
`agent/test/git-import.test.ts`.

Fixture hygiene. `e2e/codex-git-trust/run.sh` binds every host path with a
read-only `--mount type=bind`, never `-v`: dockerd silently creates a
missing `-v` source on the host, as root. Each bind source is checked to
exist first, and a missing one is a named usage error (exit 2), never a
skip (77). The fixture container is removed by a time-bounded
`docker rm -f` on every exit path (normal, INT, TERM), and its absence is
then verified by exact name. An INT or TERM sent only to the script's PID
does not interrupt the foreground `timeout … docker run`: bash runs the trap
after that command returns, so cleanup can wait up to
`CODEX_GIT_TRUST_TIMEOUT` (default 420s, sized so two stalled
finalize-import cases still end in named FAIL lines) plus the kill grace.
The run uses
`timeout --foreground`, which keeps `docker run` in the script's process
group, so a process-group signal (Ctrl-C in a terminal) reaches it directly
and it returns promptly. A removal that cannot be verified turns a pass
or a skip into a failure (exit 3), because a backgrounded container that
outlived an earlier run left root-owned directories in a clone. The image
build opts in to `--network host` only when
`CODEX_GIT_TRUST_BUILD_NETWORK=host` (bridge egress hangs on some hosts).
Acceptance runs of `task test:codex-git-trust` pass it on every full
invocation because that recipe rebuilds the image. The fixture container
itself always runs with `--network none`. The image build also runs under
`timeout --foreground`, and the hermetic `e2e/codex-git-trust/run.test.sh`
(`task test:codex-git-trust-wrapper`, in `gate:repo`, Linux-only) pins this
wrapper contract with a stub `docker`: exit 2 before any docker call for a
missing bind source, read-only `--mount` binds only, the 0/77-to-3 cleanup
mapping, the `host`-only build network, and a process-group SIGINT that
reaches `docker run` and `docker build` and exits 130 promptly.

## Acceptance evidence (2026-09-27)

Recorded on the uzi worker host (kernel 6.12) at the merged HEAD
`37683d7250d80e81f05c74ac22d17fa433b68e92` (the tree that also carries
"Reconciliation with #1804" above). Landlock is available there: every
fixture run's `uzi-codex-command-sandbox --probe` printed `LANDLOCK:
probe=0`. Each run set `CODEX_GIT_TRUST_REQUIRE_LANDLOCK=1`, which turns any
probe other than 0 into a FAIL rather than a skip.

- **Tree and image.** One-shot
  `CODEX_GIT_TRUST_REQUIRE_LANDLOCK=1 CODEX_GIT_TRUST_IMAGE=cgt-1769:37683d7250d8 CODEX_GIT_TRUST_BUILD_NETWORK=host CODEX_GIT_TRUST_BUILD_TIMEOUT=1800 task test:codex-git-trust`
  at HEAD `37683d7250d80e81f05c74ac22d17fa433b68e92`: exit 0. Image
  `cgt-1769:37683d7250d8`, image ID
  `sha256:cb92e728dc7f2528c00ec0ad5c33ef8e698163f60617db7b6bee8ca0404ba9ff`
  (from `docker image inspect --format '{{.Id}}'`; the daemon uses the
  containerd image store, where the ID is the manifest-list digest — the
  previous section said this too), created 2026-09-27T14:04:36Z. Built with
  `CODEX_GIT_TRUST_BUILD_NETWORK=host` (a host workaround: bridge egress
  hangs on this worker) and `CODEX_GIT_TRUST_BUILD_TIMEOUT=1800`; the fixture
  container itself always runs `--network none`.
- **How the gate ran.** `task test:codex-git-trust` is `build` then
  `fixture`, run here as one recipe. The build completed cleanly under the
  1800s timeout (the image's final export-and-unpack step alone took
  132.3s); no build timeout or disk-space retry was needed on this run.
- **After (fixed, the image's own `agent/src`).** The one-shot run's fixture
  half: exit 0, `SHARED-CLONE MODE: FIXED`, `SHARED-CLONE RESULT: FIXED
  PASS`, `RESULT: FIXED PASS`, 64 PASS, 0 FAIL, 0 SKIP, `LANDLOCK: probe=0`.
  In both best-effort and required modes this includes `SHARED-CLONE FIXED
  required: git status`, `… git diff HEAD`, `… git log --oneline`, `… write
  + git add + git commit`, the anchored `cat-file -e` checks, and the
  confinement controls `… ls bare objects/pack is denied` and `… ls sibling
  clone .git is denied`. The merged tree also runs #1765 scratch
  provisioning inside `runnerCloneForBranch`, which the fixture drives; it
  passed in this run.
- **Before (base `e078ba7916c809ebac7618f157ff5a9906eec04b`, via
  `CODEX_GIT_TRUST_SRC_DIR`).** The unfixed source was staged with
  `git archive e078ba79 agent/src` at
  `/data/runner/uzi-runner/cgt-1769-stage/base/agent/src`, outside the
  clone, and run against the same image: `EXIT=201` (the task's code for a
  failed target; the fixture itself exited 1). 40 PASS, 9 FAIL, 1 SKIP.
  Eight FAILs are `git status`, `git diff HEAD`, `git log --oneline` and
  `write + git add + git commit` in both modes, each with `error: unable to
  open object pack directory: /data/repos/…/objects/pack: Permission
  denied`; the ninth is the required-mode `cherry-picked WIP.txt was
  committed (by the prior best-effort mode)` (`code=128 stdout=""`), which
  follows from the best-effort commit having failed. The bare and sibling
  denial controls PASS on both trees, and the anchored `cat-file -e` checks
  PASS on the base tree too, so they are a fixed-side positive check, not
  the discriminator. The one SKIP is `SKIP: FINALIZE-IMPORT part;
  importFixed=false probe.status=0` (the base has no import API); acceptable
  only alongside the executed controls below.
- **Finalize import, through the real boundary machinery (fixed tree).** All
  named lines PASS: the calibration line
  (`FINALIZE-IMPORT: process scan sees a sandboxed command-identity process (calibration)`),
  (a) success (`sandboxed cat-file -e newTip FAILS before import`,
  `alignBranchWithDefault returns "aligned"`,
  `sandboxed cat-file -e newTip succeeds after import`), (b) producer failure
  (`a bare-absent tip throws RunnerCloneImportError`), (c) consumer failure
  (`an unwritable clone objects/pack throws RunnerCloneImportError`), and for
  each of (a), (b) and (c): `producer and consumer reaped`,
  `no live command-kind root`, `a subsequent worker_pat boundary action succeeds`.
- **Negative controls, staged the same way** (a copy of HEAD's `agent/src`
  with one change, mounted via `CODEX_GIT_TRUST_SRC_DIR`, same image). Each
  named case executed and FAILED:
  - `mut-noimport` (`return;` inserted right after `if (await
    cloneHasTip()) return;` in `ensureRunnerCloneObjects`, so no pack is
    streamed): `EXIT=201`, 60 PASS, 4 FAIL:
    `FINALIZE-IMPORT (a): alignBranchWithDefault returns "aligned"` (threw
    `git update-ref … trying to write ref 'refs/uzi-align/target' with
    nonexistent object …`), `FINALIZE-IMPORT (a): sandboxed cat-file -e
    newTip succeeds after import` (`code=1`), `FINALIZE-IMPORT (b): a
    bare-absent tip throws RunnerCloneImportError` (`threw=undefined:
    undefined`), and `FINALIZE-IMPORT (c): an unwritable clone objects/pack
    throws RunnerCloneImportError` (`threw=undefined: undefined`).
  - `mut-stall` (`stopProducer`/`stopConsumer` do no teardown, the
    pipe-error handler does no teardown, and `producer.stdout` is piped
    into `consumer.stdin` with `end:false` so the consumer's stdin is never
    ended): `EXIT=201`, 59 PASS, 5 FAIL:
    `FINALIZE-IMPORT (b): a bare-absent tip throws RunnerCloneImportError`
    (threw `CodexBoundaryError: codex boundary failed at action`),
    `FINALIZE-IMPORT (b): producer and consumer reaped` (leftover
    `uzi-codex-supervisor` and `git … index-pack --stdin` processes, uid
    10003, named in the failure), `FINALIZE-IMPORT (b): a subsequent
    worker_pat boundary action succeeds` (`stdout="threw codex boundary
    failed at quiesce"`), `FINALIZE-IMPORT (c): an unwritable clone
    objects/pack throws RunnerCloneImportError` (threw `CodexBoundaryError:
    codex boundary failed at action`), and `FINALIZE-IMPORT (c): a
    subsequent worker_pat boundary action succeeds` (`stdout="threw codex
    boundary failed at quiesce"`).
- **Controls that did not discriminate at this layer, run before the merge
  (not re-run now).** At fixture `f9e9c551`, three earlier non-discriminating
  controls were run — reverting all of `1c3be1a3`'s `agent/src/git.ts`
  change, removing the peer teardown alone, and reverting
  `callerOwnedStdout` (since replaced by `exitGatedStream`, see
  "Reconciliation with #1804" above) — each 64 PASS, 0 FAIL. Those contracts
  are pinned by the stalled-process and deterministic red/green tests in
  `agent/test/git-import.test.ts`; they were not re-run here.
- **What these runs fixed in the fixture (history).** Before this fix, the
  fixture exited 0 without reaching the shared-clone mode checks or any
  `RESULT` line: it awaited a reaped command root's spawn promise that never
  settles, and node exits 0 when the event loop drains. An exit guard turns
  that into `RESULT: FAIL — the fixture ended before main() completed …`
  with exit 1 (observed once, before the wait was bounded). The finalize
  part runs after the mode checks its merge would otherwise disturb, an
  exception in a case is that case's FAIL, and each boundary is bounded so a
  stall is a named FAIL within the outer budget. That fixture code is still
  in place, and this acceptance run exercised it.
- **Gates at the same HEAD.** `agent/src` at `37683d72` is identical to
  `4927099d` except comments; `gate:agent` ran at `4927099d`. `task
  gate:agent`: exit 0 (`test:agent` 5210 tests, 5208 pass, 0 fail, 2
  skipped; `test:codex-m4` 165 pass). `task gate:repo` stopped at
  `test:uzi-lander` (`cr-rate-limit.sh: line 168: File: unbound variable`),
  reproduced identically on `e078ba79` in a detached worktree, so
  environmental; the 13 targets after it each ran individually to `EXIT=0`:
  `check:spec-numbering`, `check:migration-additive`,
  `check:migration-numbering`, `check:e2e-registry-doc`,
  `check:no-binary-text`, `check:mktemp-portability`, `check:skill-size`,
  `check:token-literals`, `test:migration-renumber`,
  `test:agent-runtime-key`, `test:codex-supervisor`, `scan:secrets`,
  `sast:semgrep`.
- **Hygiene.** Bind sources were staged under the runner's private tmp,
  outside the clone; `run.sh` uses read-only `--mount` binds; every docker
  step ran in the foreground (the lead's shell ran the chain as a
  background shell job, but no loop, and each `docker run` ran under
  `run.sh`'s own timeout); containers were named `codex-git-trust-<pid>` by
  `run.sh`, removed and verified absent by exact name by `run.sh`.
  Afterwards `docker ps -a` showed no `codex-git-trust` or `cgt-1769`
  container. Any file changed after this image build (`37683d72`) is
  limited to this ADR.
