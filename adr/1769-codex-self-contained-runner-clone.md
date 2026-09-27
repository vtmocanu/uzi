# ADR-1769: the Codex runner clone is dissociated from the worker bare, not granted read access to it

**Status**: Implemented; acceptance evidence pending (issue #1769)
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
`CODEX_GIT_TRUST_TIMEOUT` plus the kill grace. A process-group signal (Ctrl-C
in a terminal) also reaches `docker run` directly, so it returns promptly. A removal that cannot be verified turns a pass
or a skip into a failure (exit 3), because a backgrounded container that
outlived an earlier run left root-owned directories in a clone. The image
build opts in to `--network host` only when
`CODEX_GIT_TRUST_BUILD_NETWORK=host` (bridge egress hangs on some hosts).
Acceptance runs of `task test:codex-git-trust` pass it on every full
invocation because that recipe rebuilds the image. The fixture container
itself always runs with `--network none`.
