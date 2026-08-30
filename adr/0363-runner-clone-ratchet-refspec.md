# ADR-0363: The worker runner-clone lint-ratchet base is made durable by removing the tracking-fetch refspec

**Status**: Accepted
**Date**: 2026-08-18
**Origin**: vtmocanu/uzi#363, the durability gap in the worker lint-ratchet base fix that landed across #262 (advance the runner clone's `origin/main` to the fresh default head) and #313 (clamp the ratchet base to the true branch base). Both were applied once at clone setup and were not durable.

## Decision (summary)

In `agent/src/git.ts` `runnerCloneForBranch`, after the existing #262/#313 clamp (`update-ref refs/remotes/origin/<default>`), unset the runner clone's `remote.origin.fetch` refspec (as the runner uid, matching the surrounding runner-owned config writes).

That single config removal makes the clamp durable by construction. The only mechanism that moved `refs/remotes/origin/<default>` off the clamped base was the configured refspec `+refs/heads/*:refs/remotes/origin/*` that `git clone` writes into the runner clone: it maps a fetch of the bare's frozen `refs/heads/<default>` back onto the runner clone's `refs/remotes/origin/<default>`. With no configured refspec, an agent-initiated `git fetch origin main`, `git fetch origin`, `git pull origin main`, or `git remote update` updates only `FETCH_HEAD` and never touches any remote-tracking ref, so the ratchet base the linter reads stays exactly where git.ts planted it.

The runner clone's `origin` is the local, deliberately frozen worker bare (`git clone --shared` from the bare). Its remote-tracking refs into that frozen bare are stale by design and are never authoritative; the fresh values live in the bare's own `refs/remotes/origin/*`, and git.ts hand-plants the one ref the ratchet needs. Auto-syncing tracking refs from a frozen source was the defect; removing the refspec states that.

## Context

`.golangci.yml`'s ratchet gates on findings the branch introduces by computing `merge-base(origin/main, HEAD)`. `origin/main` is a movable remote-tracking ref, chosen deliberately so local and CI read the same base (PRD #103 Success Criterion 1); a bespoke ref name is not an option, because it would not resolve in CI's detached-HEAD checkout. So the fix must keep `refs/remotes/origin/<default>` itself correct in the runner clone; it may not repoint the ratchet.

The worker bare is a frozen mirror: `cloneBare` rewrites the bare's fetch refspec to `+refs/heads/*:refs/remotes/origin/*`, so the bare's own `refs/heads/*` freeze at first clone while `refs/remotes/origin/*` track fresh. This frozen layout is load-bearing for `RunnerClone.baseCommit` / `defaultBranchCommit` resolution and the resume/checkpoint base machinery. The bare's frozen refs must not be touched.

The gap: nothing re-applied the clamp. A later `git fetch origin main` inside the runner clone (the exact action root `CLAUDE.md` prescribes to local contributors when the ratchet reports a large backlog) fetched the bare's frozen `refs/heads/main` and forced `refs/remotes/origin/main` backward, so `merge-base(origin/main, HEAD)` regressed and `whole-files: true` re-reported the pre-existing backlog as branch-introduced. The judge re-raised this in 11 runs; it recurred twice on 2026-08-16, papered over by agents hand-repointing the ref and persisting per-run memory.

The cross-context conflict is resolved rather than forbidden: `CLAUDE.md`'s "run `git fetch origin main`" advice is correct on a dev machine and corrupting in a worker. This decision makes that same command harmless in the worker (it updates `FETCH_HEAD`, the ratchet base is untouched), so the local advice needs no carve-out.

### Evidence (measured, git 2.55.0, frozen-mirror topology reproduced)

`FORK` = fresh branch base (clamped value); `C0` = frozen bare `refs/heads/main`, an ancestor of `FORK`.

| Runner-clone `remote.origin.fetch` | `git fetch origin main` | ratchet base after |
|---|---|---|
| `+refs/heads/*:refs/remotes/origin/*` (today) | rc=0, silent `origin/main` FORK to C0 | corrupted to C0 |
| (unset), the decision | rc=0, updates `FETCH_HEAD` only | stays FORK |
| `refs/heads/*:...` (drop the `+`) | rc=1, rejected non-fast-forward | stays FORK |
| `+...` plus `^refs/heads/main` (negative) | rc=0, forced update | corrupted to C0 |

## Rejected alternatives

- **Drop the `+` (non-forced refspec).** Preserves the clamp, but `git fetch origin main` then exits rc=1 with a rejected non-fast-forward message; a reasonable agent reads that as a failed command. Rejected on agent experience.
- **Negative refspec `^refs/heads/main`.** Refuted by measurement: an explicit `git fetch origin main` still applied a forced update and corrupted the ref. Does not work.
- **Re-apply the clamp after every fetch via a hook.** Git has no client-side post-fetch hook; a `reference-transaction` hook contends with the worker's `core.hooksPath` neutralization guardrail. Rejected: never add a mechanism that fights a guardrail layer when a config change suffices.
- **Guardrail-rewrite/deny the fetch.** Wrong layer: the guardrail exists for the primary directive (no push, no credential/proc read), not ratchet correctness. It would need a fragile enumeration of fetch/pull/remote-update in a safety-critical screener and would contradict `CLAUDE.md`'s own advice. Rejected.
- **Freshen (unfreeze) the mirror.** Would break the mirror-layout fallback that base/resume/checkpoint resolution depend on and would not fully close the race. Rejected as invasive and off-target.

## What this ADR does NOT decide

- It does not defend against a deliberately-forced explicit command-line refspec (`git fetch --force origin main`, or an explicit `+refs/heads/main:refs/remotes/origin/main`). Those override any config; defending them needs a `reference-transaction` hook, which contends with hook neutralization, and they are not a realistic agent action. Accepted residual.
- It does not change that the runner clone cannot see fresh `main` (its `origin` is the frozen bare). Pre-existing design, out of scope.
- It does not change `.golangci.yml`, the bare's refspec, `CLAUDE.md`'s local advice, or any guardrail layer.

## Consequences

- The clamp becomes durable for the run's lifetime regardless of how many times the agent fetches.
- The runner clone no longer opportunistically updates any remote-tracking ref from a fetch. This is benign: those refs pointed at the frozen bare and were stale-and-unused; the worker's push path runs in the bare and does not read the runner clone's `refs/remotes/origin/*`, and `git merge`/`git rebase`/`git pull` continue to work off `FETCH_HEAD`.
- Invariant a future change would break: the runner clone's `refs/remotes/origin/<default>` is authoritative only as git.ts plants it; nothing may re-introduce a `remote.origin.fetch` mapping that lets a fetch of the frozen bare overwrite it. Any future need to track additional refs must use an explicit, curated refspec, never the `+refs/heads/*` wildcard.
