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
`CODEX_GIT_TRUST_TIMEOUT` plus the kill grace. The run uses
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

Recorded on the uzi worker host (kernel 6.12, Landlock available: the
image's `uzi-codex-command-sandbox --probe` exited 0, which the sandbox only
returns for Landlock ABI >= 1; ABI < 1 or a probe error exits 11). Every run
set `CODEX_GIT_TRUST_REQUIRE_LANDLOCK=1`, so a missing Landlock would have
been a FAIL, not a skip. No line in any run below was a SKIP except the one
noted for the base tree.

- **Tree and image.** Fixture tree at `74122b7c66b1c476d22a633378e168280c48da17`
  (the runs use its `e2e/codex-git-trust/`, bind-mounted). Image
  `cgt-1769:68deb3e6d41a` (image ID `sha256:a4e68a07cbb3…`, manifest list
  `sha256:dcadaccdfc5b…`), built by
  `CODEX_GIT_TRUST_IMAGE=cgt-1769:68deb3e6d41a CODEX_GIT_TRUST_BUILD_NETWORK=host task test:codex-git-trust:build`
  at `68deb3e6`, exit 0. `git diff 68deb3e6..74122b7c` touches only
  `e2e/codex-git-trust/fixture.ts`, so the image's `agent/src`, binaries and
  entrypoint are those of the final tree.
- **How the gate ran.** `task test:codex-git-trust` is `build` then
  `fixture`; they were run as those two subtasks, not as one invocation.
  The one-shot form was attempted at the final SHA and could not finish on
  this host: a rebuild under a new `UZI_SRC_SHA` re-runs the image's
  `chmod -R a+rX /nix` layer (about 260 s) plus a 3–4 minute export, and the
  second copy of that 8.75 GB image then failed with `no space left on
  device` on the shared daemon's volume. `--network host` for the build is a
  host workaround (bridge egress hangs here); the fixture container ran
  `--network none` throughout.
- **After (fixed source, the image's own `agent/src`).**
  `CODEX_GIT_TRUST_REQUIRE_LANDLOCK=1 CODEX_GIT_TRUST_IMAGE=cgt-1769:68deb3e6d41a task test:codex-git-trust:fixture`
  exit 0: `LANDLOCK: probe=0`, `SHARED-CLONE MODE: FIXED`,
  `SHARED-CLONE RESULT: FIXED PASS`, `RESULT: FIXED PASS`, 64 PASS, 0 FAIL,
  0 SKIP. Among them, in best-effort and required modes:
  `SHARED-CLONE FIXED required: git status`, `… git diff HEAD`,
  `… git log --oneline`, `… write + git add + git commit`, the anchored
  `cat-file -e` of the base, checkpoint marker and advanced default tip, and
  the controls `… ls bare objects/pack is denied` and
  `… ls sibling clone .git is denied`.
- **Before (base `87cb446b`, mounted via `CODEX_GIT_TRUST_SRC_DIR`).** Same
  command, exit 1 (task 201): `SHARED-CLONE MODE: UNFIXED`,
  `SHARED-CLONE RESULT: UNFIXED FAIL`. `git status`, `git diff HEAD`,
  `git log --oneline` and `write + git add + git commit` FAIL in both modes
  with `error: unable to open object pack directory: /data/repos/…/objects/pack:
  Permission denied` (the issue's evidence). The bare and sibling denial
  controls PASS on both trees. The finalize-import part SKIPs on this tree
  (it has no import API), so its red evidence comes from the controls below.
- **Finalize import, through the real boundary machinery.** On the fixed
  tree each case PASSes its named lines: calibration
  (`FINALIZE-IMPORT: process scan sees a sandboxed command-identity process`),
  (a) success (`sandboxed cat-file -e newTip FAILS before import`,
  `alignBranchWithDefault returns "aligned"`,
  `sandboxed cat-file -e newTip succeeds after import`), (b) producer failure
  (`a bare-absent tip throws RunnerCloneImportError`), (c) consumer failure
  (`an unwritable clone objects/pack throws RunnerCloneImportError`), and for
  each of (a), (b), (c): `producer and consumer reaped`,
  `no live command-kind root`, `a subsequent worker_pat boundary action succeeds`.
- **Negative controls** (each a copy of the fixed `agent/src` with one change,
  mounted via `CODEX_GIT_TRUST_SRC_DIR`; a SKIP, missing line or crash never
  counts as red):
  - `mut-noimport` (`ensureRunnerCloneObjects` returns before streaming a
    pack): red on (a) `alignBranchWithDefault returns "aligned"` and
    `sandboxed cat-file -e newTip succeeds after import`, (b)
    `a bare-absent tip throws RunnerCloneImportError`, (c)
    `an unwritable clone objects/pack throws RunnerCloneImportError`.
  - `mut-stall` (no peer teardown, and the consumer's stdin never ended): red
    on (b) `a bare-absent tip throws RunnerCloneImportError` (the boundary
    did not settle within the case bound), (b) and (c)
    `producer and consumer reaped` (a leftover `uzi-codex-supervisor … index-pack`
    process, uid 10003, named in the detail), and (a) and (b)
    `a subsequent worker_pat boundary action succeeds`.
  - Recorded as NOT discriminated at this layer (green): reverting
    `1c3be1a3`'s teardown/attribution hunk, removing the peer teardown
    alone, and reverting `callerOwnedStdout`. In these three real cases git
    ends on EOF/EPIPE by itself, and the flushStdio race did not reproduce;
    those contracts are pinned by stalled-process and deterministic
    red/green tests in `agent/test/git-import.test.ts` instead.
- **What the acceptance runs also fixed in the fixture.** Before these runs
  the fixture exited 0 without reaching the shared-clone mode checks or any
  `RESULT` line (it awaited a reaped command root's promise that never
  settles, and node exits 0 when the loop drains). It now fails with
  `RESULT: FAIL — the fixture ended before main() completed` in that case,
  bounds that wait, runs the finalize-import part after the mode checks it
  would otherwise disturb, and bounds each finalize boundary so a stall is a
  named FAIL rather than a hang.
- **Hygiene.** All bind sources were staged outside the runner clone; every
  docker step ran in the foreground under `timeout`; containers were named
  `codex-git-trust-<pid>` / `cgt-1769-*` and checked absent by exact name.
  One probe container that timed out before starting was found later in the
  `Created` state (no binds) and removed by exact name.

