# ADR-1769: the Codex runner clone is dissociated from the worker bare, not granted read access to it

**Status**: Partially implemented (seed landed; finalize import in progress, issue #1769)
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
alternates path point outside the sandbox's grant. Every Codex run doing any
non-trivial git operation (log, blame, a diff against an older commit) could
hit this once its working set touched an object the initial checkout had not
already copied.

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

### At finalize: stream the fresh default tip in as a copy, not a borrow (in progress)

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
the consumer registers as a `boundary_action` root, the same class of root
the seed-time materialization used).

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
  the producer under them.
- **Unchanged.** The worker's fetch-back of the agent branch (`file://`+pack,
  the CVE-2022-39253-class boundary already documented in
  `docs/proc-hardening.md`) and the command git-trust restrictions are not
  touched by this issue.

## Verification pointers

`task test:codex-git-trust` (opt-in, Docker) now includes a `--shared`-clone
case exercising this path; see that recipe for what it currently covers.
Unit coverage for the seed-time path lives in
`agent/test/git-materialize.test.ts` and
`agent/test/runner-clone-self-contained.test.ts`.
